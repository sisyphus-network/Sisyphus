package replication

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/ipfs/go-cid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/blobclient"
	"github.com/sisyphus-network/Sisyphus/packages/identity"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// ownerPrefix begins the name a follower's pins are held under. The rest is
// the store they are copies of, as the coordinator named it, which is how a
// follower remembers that name for as long as it holds anything.
const ownerPrefix = "pool:"

// FollowerStore is the part of a storage.Store that a follower keeps its
// copies in.
type FollowerStore interface {
	Pins() []storage.Pin
	Pin(ctx context.Context, owner string, expires time.Time, cids ...cid.Cid) error
	Unpin(owner string, cids ...cid.Cid) error
	GC(ctx context.Context, now time.Time) (storage.Collected, error)
	Open(ctx context.Context, c cid.Cid) (storage.Blob, error)
	blobclient.Putter
}

type pinChangeSource interface {
	OnPinChange(func())
}

// Follower is a storage follower's side of replication: it holds, in a
// store set aside for the purpose, the blobs its coordinator tells it to.
type Follower struct {
	// Store is where the copies are kept. It should be durable, and used
	// for nothing else: whatever is in it that the coordinator has not
	// listed is deleted.
	Store FollowerStore
	// ListFile is where the last list the coordinator signed is kept. It
	// should be beside the copies and as durable: it is what a coordinator
	// that has lost its store takes them back on the strength of.
	ListFile string
	// Coordinator is the blob service of the node followed.
	Coordinator pb.BlobServiceClient
	// CoordinatorID is the identity whose signature the follower expects on
	// replication lists.
	CoordinatorID string
	Log           *slog.Logger

	// stream and streamCancel are the persistent initial-baseline stream.
	// Run consumes pushed assignments from it and reconnects after it ends.
	stream       pb.BlobService_ReplicationClient
	streamCancel context.CancelFunc
	lastSequence uint64
}

// Run keeps the store as the coordinator would have it until ctx ends.
func (f *Follower) Run(ctx context.Context) {
	interval := Interval
	pinChanges := make(chan struct{}, 1)
	if source, ok := f.Store.(pinChangeSource); ok {
		source.OnPinChange(func() {
			select {
			case pinChanges <- struct{}{}:
			default:
			}
		})
	}
	initial := true
	// failing is whether the last attempt failed. Only the first failure of
	// a run of them is worth a warning: a coordinator that is away stays
	// away for a while.
	failing := false
	for {
		changed, err := f.syncWith(ctx, initial)
		if err != nil && !failing && ctx.Err() == nil {
			f.Log.Warn("could not bring this node's copies of its pool's data up to date; it keeps what it has and tries again", "every", interval.String(), "error", err)
		}
		failing = err != nil
		if ctx.Err() != nil {
			f.stopStream()
			return
		}
		initial = false

		stream, cancel := f.takeStream()
		if err == nil && stream != nil {
			updates := make(chan *pb.ReplicateResponse, 1)
			requests := make(chan *pb.ReplicateRequest, 8)
			streamEnded := make(chan error, 1)
			leaseEnded := make(chan error, 1)
			streamReadyEvents := make(chan struct{}, 1)
			leaseCtx, stopLeases := context.WithCancel(ctx)
			leaseResult := leaseEnded
			go f.receiveAssignments(ctx, stream, updates, streamReadyEvents, streamEnded)
			go func() { leaseResult <- f.sendLeases(leaseCtx, stream, requests) }()
			timer := time.NewTimer(time.Hour)
			timer.Stop()
			timerC := (<-chan time.Time)(nil)
			streamReady := false
			needsReport := changed
			var pendingAnswers []*pb.ChallengeAnswer
			connected := true
			for connected {
				select {
				case <-streamReadyEvents:
					streamReady = true
					if needsReport {
						connected = enqueueReport(ctx, requests, f.reportRequest(pendingAnswers), func() {
							needsReport = false
							pendingAnswers = nil
							drainPinChanges(pinChanges)
						})
					}
				case response := <-updates:
					changed = false
					if response.GetList() != nil {
						changed, err = f.applyPush(ctx, response)
						if err != nil {
							if !failing {
								f.Log.Warn("could not apply a pushed storage assignment; it keeps what it has and reconnects", "error", err)
							}
							failing = true
							connected = false
							initial = true
							break
						}
					}
					failing = false
					answers, forgot := f.answerChallenges(ctx, response.GetChallenges(), f.heldCopies())
					if changed || forgot || len(response.GetChallenges()) > 0 {
						needsReport = true
						pendingAnswers = append(pendingAnswers, answers...)
						if streamReady {
							connected = enqueueReport(ctx, requests, f.reportRequest(pendingAnswers), func() {
								needsReport = false
								pendingAnswers = nil
								drainPinChanges(pinChanges)
							})
						}
					}
				case err := <-streamEnded:
					if errors.Is(err, io.EOF) {
						// Older coordinators answer the initial baseline and then
						// close the stream cleanly. Keep anti-entropy on the unary
						// RPC rather than reopening that unsupported stream each tick.
						f.Log.Info("the coordinator does not push storage assignments; continuing periodic reconciliation")
						cancel()
						stopLeases()
						updates, streamEnded, leaseEnded = nil, nil, nil
						resetTimer(timer, legacySyncWait(interval, needsReport))
						timerC = timer.C
						continue
					}
					if ctx.Err() == nil && !failing {
						f.Log.Warn("the storage assignment stream ended; reconnecting", "error", err)
					}
					failing = ctx.Err() == nil
					connected = false
					initial = true
				case err := <-leaseEnded:
					if errors.Is(err, io.EOF) {
						// A coordinator may have finished its send side after the
						// baseline. Let Recv decide whether it supports pushes or
						// whether the stream really failed.
						leaseEnded = nil
						resetTimer(timer, legacySyncWait(interval, needsReport))
						timerC = timer.C
						continue
					}
					if ctx.Err() == nil {
						if !failing {
							f.Log.Warn("the storage assignment lease could not be renewed; reconnecting", "error", err)
						}
						failing = true
						connected = false
						initial = true
					}
				case <-pinChanges:
					needsReport = true
					if streamReady {
						connected = enqueueReport(ctx, requests, f.reportRequest(pendingAnswers), func() {
							needsReport = false
							pendingAnswers = nil
							drainPinChanges(pinChanges)
						})
					} else {
						timerC = expediteLegacySync(timer, timerC)
					}
				case <-timerC:
					needsReport = false
					changed, err = f.sync(ctx)
					if err != nil && !failing && ctx.Err() == nil {
						f.Log.Warn("could not reconcile this node's copies with its pool; it keeps what it has and tries again", "every", interval.String(), "error", err)
					}
					failing = err != nil
					if err != nil && updates == nil {
						// The coordinator may have restarted since the stream
						// ended cleanly and now support pushes. Reopen it after a
						// failed unary reconciliation.
						connected = false
						initial = true
						break
					}
					wait := interval
					if changed && err == nil {
						wait = 0
					}
					timer.Reset(wait)
				case <-ctx.Done():
					connected = false
				}
			}
			timer.Stop()
			cancel()
			stopLeases()
			if ctx.Err() != nil {
				return
			}
			// A failed stream needs an initial exchange when it reconnects, but
			// still waits the normal interval before trying again.
			changed = false
		}

		wait := interval
		if changed && err == nil {
			// The coordinator is told at once what is now held.
			wait = 0
		}
		if !sleepForPinChange(ctx, pinChanges, wait) {
			return
		}
	}
}

func sleepForPinChange(ctx context.Context, changed <-chan struct{}, wait time.Duration) bool {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-changed:
		return true
	case <-ctx.Done():
		return false
	}
}

func drainPinChanges(changed <-chan struct{}) {
	for {
		select {
		case <-changed:
		default:
			return
		}
	}
}

func resetTimer(timer *time.Timer, wait time.Duration) {
	// Go 1.23+ timer channels cannot retain a stale tick after Stop returns
	// false, so Reset needs no channel drain.
	timer.Stop()
	timer.Reset(wait)
}

func (f *Follower) takeStream() (pb.BlobService_ReplicationClient, context.CancelFunc) {
	stream, cancel := f.stream, f.streamCancel
	f.stream, f.streamCancel = nil, nil
	return stream, cancel
}

func (f *Follower) stopStream() {
	_, cancel := f.takeStream()
	if cancel != nil {
		cancel()
	}
}

func (f *Follower) receiveAssignments(ctx context.Context, stream pb.BlobService_ReplicationClient, updates chan *pb.ReplicateResponse, ready chan<- struct{}, ended chan<- error) {
	for {
		response, err := ReadReplicationResponse(stream.Recv)
		if err != nil {
			select {
			case ended <- err:
			case <-ctx.Done():
			}
			return
		}
		if response.GetList() == nil && len(response.GetChallenges()) == 0 {
			if response.GetStore() == "" && len(response.GetHold()) == 0 && !response.GetShowList() {
				// Capability acknowledgement is separate from snapshots: a slow
				// snapshot application must not leave the stream in discovery mode.
				select {
				case ready <- struct{}{}:
				default:
				}
				continue
			}
			select {
			case ended <- fmt.Errorf("replication stream sent a response without a signed list"):
			case <-ctx.Done():
			}
			return
		}
		if len(response.GetChallenges()) > 0 {
			// Challenge replies are correlated with this exact nonce and must not
			// be coalesced away with a newer assignment snapshot.
			select {
			case updates <- response:
			case <-ctx.Done():
				return
			}
			continue
		}
		select {
		case updates <- response:
		default:
			// Lists are complete snapshots. If application falls behind, keep
			// only the newest one rather than queueing every intermediate state.
			select {
			case <-updates:
			default:
			}
			select {
			case updates <- response:
			case <-ctx.Done():
				return
			}
		}
	}
}

// forget lets go of a blob the store turned out not to have, as held is
// told, so that it is fetched afresh.
func (f *Follower) forget(held map[cid.Cid][]string, id string) {
	c, err := cid.Decode(id)
	if err != nil {
		return
	}
	for _, owner := range held[c] {
		f.Store.Unpin(owner, c) // a pin that will not go is fetched over all the same
	}
	delete(held, c)
}

// sync asks the coordinator what to hold, fetches what is missing and drops
// the rest. It reports whether what the store holds changed.
func (f *Follower) sync(ctx context.Context) (changed bool, err error) {
	return f.syncWith(ctx, false)
}

func (f *Follower) heldCopies() map[cid.Cid][]string {
	held := make(map[cid.Cid][]string)
	for _, pin := range f.Store.Pins() {
		if _, ours := strings.CutPrefix(pin.Owner, ownerPrefix); ours {
			held[pin.CID] = append(held[pin.CID], pin.Owner)
		}
	}
	return held
}

func (f *Follower) reportRequest(answers []*pb.ChallengeAnswer) *pb.ReplicateRequest {
	_, holding, store := f.currentHoldings()
	return &pb.ReplicateRequest{
		Holding: holding, Store: store, KeepsLists: true, AnswersChallenges: true, Answers: answers,
	}
}

func enqueueReport(ctx context.Context, requests chan<- *pb.ReplicateRequest, request *pb.ReplicateRequest, queued func()) bool {
	select {
	case requests <- request:
		queued()
		return true
	case <-ctx.Done():
		return false
	}
}

func legacySyncWait(interval time.Duration, needsReport bool) time.Duration {
	if needsReport {
		return 0
	}
	return interval
}

func expediteLegacySync(timer *time.Timer, timerC <-chan time.Time) <-chan time.Time {
	if timerC == nil {
		return nil
	}
	resetTimer(timer, 0)
	return timer.C
}

func (f *Follower) currentHoldings() (map[cid.Cid][]string, []string, string) {
	held := make(map[cid.Cid][]string)
	var holding []string
	var store string
	for _, pin := range f.Store.Pins() {
		name, ours := strings.CutPrefix(pin.Owner, ownerPrefix)
		if !ours {
			continue
		}
		if held[pin.CID] == nil {
			holding = append(holding, pin.CID.String())
		}
		held[pin.CID] = append(held[pin.CID], pin.Owner)
		store = name
	}
	return held, holding, store
}

func (f *Follower) answerChallenges(ctx context.Context, questions []*pb.Challenge, held map[cid.Cid][]string) (answers []*pb.ChallengeAnswer, changed bool) {
	for _, c := range questions {
		answer := &pb.ChallengeAnswer{Cid: c.GetCid(), Nonce: c.GetNonce()}
		named, unread := read(ctx, f.Store, c.GetCid(), c.GetOffset(), c.GetLength())
		if unread != nil {
			f.Log.Warn("this node cannot read a blob it holds for its pool, and lets go of it to fetch it again", "cid", c.GetCid(), "error", unread)
			f.forget(held, c.GetCid())
			changed = true
		} else {
			answer.Digest = Answer(c.GetNonce(), named)
		}
		answers = append(answers, answer)
	}
	return answers, changed
}

func (f *Follower) syncWith(ctx context.Context, initial bool) (changed bool, err error) {
	if initial {
		f.stopStream()
	}
	// held maps each blob held to the names it is pinned under: one, unless
	// an earlier change of name was cut short.
	held, holding, store := f.currentHoldings()
	asked := &pb.ReplicateRequest{Holding: holding, Store: store, KeepsLists: true, AnswersChallenges: true}
	var told *pb.ReplicateResponse
	var activeStream pb.BlobService_ReplicationClient
	var activeCancel context.CancelFunc
	defer func() {
		if activeCancel != nil {
			activeCancel()
		}
	}()
	if initial {
		var list *pb.KeepList
		if f.ListFile != "" {
			var unread error
			list, unread = loadList(f.ListFile)
			if unread != nil {
				f.Log.Warn("could not read the list this node's coordinator signed of what to hold; without it the coordinator takes nothing back by itself", "error", unread)
			} else if list != nil {
				f.lastSequence = max(f.lastSequence, list.GetSequence())
			}
		}
		streamCtx, cancel := context.WithCancel(ctx)
		activeCancel = cancel
		stream, streamErr := f.Coordinator.Replication(streamCtx)
		if streamErr == nil {
			asked.List = list
			var frames []*pb.ReplicationHello
			frames, streamErr = ReplicationHellos(asked)
			if streamErr == nil {
				streamErr = sendReplicationHellos(stream.Send, frames)
			}
			if streamErr == nil {
				told, streamErr = ReadReplicationResponse(stream.Recv)
			}
			if streamErr == nil {
				activeStream, activeCancel = stream, cancel
			}
		}
		if stream != nil && errors.Is(streamErr, io.EOF) {
			// A server may have already ended an unsupported stream. grpc-go
			// reports EOF from Send/CloseSend; Recv carries the real status.
			_, streamErr = stream.Recv()
		}
		if status.Code(streamErr) == codes.Unimplemented {
			// Older coordinators keep the unary recovery path. The signed list
			// will be offered only if that coordinator asks for it.
			asked.List = nil
			told, err = f.Coordinator.Replicate(ctx, asked)
			activeCancel()
			activeCancel = nil
		} else {
			err = streamErr
		}
	} else {
		told, err = f.Coordinator.Replicate(ctx, asked)
	}
	if err == nil {
		err = f.verifyList(told.GetList())
	}
	if err == nil && told.GetShowList() {
		// The coordinator has another store than the one these copies are
		// of. Shown the list it signed for that one, it can take them back.
		list, unread := loadList(f.ListFile)
		if unread != nil {
			f.Log.Warn("could not read the list this node's coordinator signed of what to hold; without it the coordinator takes nothing back by itself", "error", unread)
		}
		if list != nil {
			asked.List = list
			told, err = f.Coordinator.Replicate(ctx, asked)
			if err == nil {
				err = f.verifyList(told.GetList())
			}
		}
	}
	if err == nil && len(told.GetChallenges()) > 0 {
		// Asked to show what it holds, answer at once. Unreadable copies are
		// released before the resulting state report is sent.
		answers, forgot := f.answerChallenges(ctx, told.GetChallenges(), held)
		asked.Answers = answers
		changed = changed || forgot
		told, err = f.Coordinator.Replicate(ctx, asked)
		if err == nil {
			err = f.verifyList(told.GetList())
		}
	}
	if err != nil {
		return false, err
	}
	changed, err = f.applyResponse(ctx, told, held)
	if err != nil {
		return false, err
	}
	if activeStream != nil {
		f.stream, f.streamCancel = activeStream, activeCancel
		activeCancel = nil
	}
	return changed, nil
}

func (f *Follower) sendLeases(ctx context.Context, stream pb.BlobService_ReplicationClient, requests <-chan *pb.ReplicateRequest) error {
	if err := stream.Send(&pb.ReplicationHello{Lease: true}); err != nil {
		return err
	}
	ticker := time.NewTicker(LeaseInterval)
	defer ticker.Stop()
	for {
		select {
		case request := <-requests:
			frames, err := ReplicationHellos(request)
			if err != nil {
				return err
			}
			if err := sendReplicationHellos(stream.Send, frames); err != nil {
				return err
			}
		case <-ticker.C:
			if err := stream.Send(&pb.ReplicationHello{Lease: true}); err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (f *Follower) applyPush(ctx context.Context, response *pb.ReplicateResponse) (bool, error) {
	list := response.GetList()
	if list == nil {
		return false, fmt.Errorf("pushed storage assignment has no signed list")
	}
	if err := f.verifyList(list); err != nil {
		return false, err
	}
	if list.GetSequence() <= f.lastSequence {
		return false, nil
	}
	held := make(map[cid.Cid][]string)
	for _, pin := range f.Store.Pins() {
		if _, ours := strings.CutPrefix(pin.Owner, ownerPrefix); ours {
			held[pin.CID] = append(held[pin.CID], pin.Owner)
		}
	}
	return f.applyResponse(ctx, response, held)
}

func (f *Follower) applyResponse(ctx context.Context, told *pb.ReplicateResponse, held map[cid.Cid][]string) (changed bool, err error) {
	hold := told.GetHold()
	expiresByCID := make(map[string]time.Time)
	if list := told.GetList(); list != nil {
		// Persist the signed list before acting on it, so the file never
		// names less than what the follower is expected to keep.
		if err := saveList(f.ListFile, list); err != nil {
			return false, fmt.Errorf("keep the list the coordinator signed: %w", err)
		}
		f.lastSequence = max(f.lastSequence, list.GetSequence())
		for _, blob := range list.GetBlobs() {
			hold = append(hold, blob.GetCid())
			if blob.GetKeepUntil() != nil {
				expiresByCID[blob.GetCid()] = blob.GetKeepUntil().AsTime()
			}
		}
	}
	owner := ownerPrefix + told.GetStore()
	wanted := make(map[cid.Cid]bool)
	pin := make(map[time.Time][]cid.Cid)
	currentExpiry := make(map[cid.Cid]time.Time)
	for _, existing := range f.Store.Pins() {
		if existing.Owner == owner {
			currentExpiry[existing.CID] = existing.Expires
		}
	}
	failed := false
	for _, id := range hold {
		c, err := cid.Decode(id)
		if err == nil && held[c] == nil {
			// Whatever arrives is checked against its CID as it is stored.
			if err = blobclient.Fetch(ctx, f.Coordinator, c, f.Store); err == nil {
				changed = true
			}
		}
		if err != nil {
			f.Log.Warn("could not fetch a blob this node is to hold for its pool", "cid", id, "error", err)
			failed = true
			continue
		}
		wanted[c] = true
		expires := expiresByCID[id]
		if !slices.Contains(held[c], owner) || !currentExpiry[c].Equal(expires) {
			pin[expires] = append(pin[expires], c)
		}
	}
	for expires, cids := range pin {
		if err = f.Store.Pin(ctx, owner, expires, cids...); err != nil {
			break
		}
	}

	// Everything else goes, as does any pin under a name no longer used.
	drop := make(map[string][]cid.Cid)
	for c, owners := range held {
		for _, name := range owners {
			if name != owner || !wanted[c] {
				drop[name] = append(drop[name], c)
			}
		}
		changed = changed || !wanted[c]
	}
	changed = changed || len(pin) > 0 || len(drop) > 0
	for name, cids := range drop {
		if err == nil {
			err = f.Store.Unpin(name, cids...)
		}
	}
	if err == nil && (len(drop) > 0 || failed) {
		// Collecting as of the far future ignores the short pin every new
		// blob gets, so what was dropped, and what a failed fetch left
		// behind, is deleted now.
		_, err = f.Store.GC(ctx, time.Now().AddDate(100, 0, 0))
	}
	return changed, err
}

func (f *Follower) verifyList(list *pb.KeepList) error {
	if list == nil {
		return nil
	}
	valid, err := identity.Verify(f.CoordinatorID, signedBytes(list), list.GetSignature())
	if err != nil || !valid {
		return fmt.Errorf("the replication list is not signed by coordinator %s", f.CoordinatorID)
	}
	return nil
}
