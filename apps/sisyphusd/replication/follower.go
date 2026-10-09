package replication

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/ipfs/go-cid"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/blobclient"
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
	blobclient.Putter
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
	Log         *slog.Logger
}

// Run keeps the store as the coordinator would have it until ctx ends.
func (f *Follower) Run(ctx context.Context) {
	interval := Interval
	// failing is whether the last attempt failed. Only the first failure of
	// a run of them is worth a warning: a coordinator that is away stays
	// away for a while.
	failing := false
	for {
		changed, err := f.sync(ctx)
		if err != nil && !failing && ctx.Err() == nil {
			f.Log.Warn("could not bring this node's copies of its pool's data up to date; it keeps what it has and tries again", "every", interval.String(), "error", err)
		}
		failing = err != nil
		wait := interval
		if changed && err == nil {
			// The coordinator is told at once what is now held.
			wait = 0
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return
		}
	}
}

// sync asks the coordinator what to hold, fetches what is missing and drops
// the rest. It reports whether what the store holds changed.
func (f *Follower) sync(ctx context.Context) (changed bool, err error) {
	// held maps each blob held to the names it is pinned under: one, unless
	// an earlier change of name was cut short.
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
	asked := &pb.ReplicateRequest{Holding: holding, Store: store, KeepsLists: true}
	told, err := f.Coordinator.Replicate(ctx, asked)
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
		}
	}
	if err != nil {
		return false, err
	}

	hold := told.GetHold()
	if list := told.GetList(); list != nil {
		// The list is kept before it is acted on, so that the one on disk
		// never names less than what is held.
		if err := saveList(f.ListFile, list); err != nil {
			return false, fmt.Errorf("keep the list the coordinator signed: %w", err)
		}
		for _, blob := range list.GetBlobs() {
			hold = append(hold, blob.GetCid())
		}
	}
	owner := ownerPrefix + told.GetStore()
	wanted := make(map[cid.Cid]bool)
	var pin []cid.Cid
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
		if !slices.Contains(held[c], owner) {
			pin = append(pin, c)
		}
	}
	err = f.Store.Pin(ctx, owner, time.Time{}, pin...)

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
