package replication

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ipfs/go-cid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/sisyphus-network/Sisyphus/packages/identity"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// scripted is a coordinator that tells its follower whatever a test has
// it say, and serves blobs from a store.
type scripted struct {
	pb.UnimplementedBlobServiceServer
	store *storage.Store
	ident *identity.Identity

	mu sync.Mutex
	// hold and name are what it answers with, and err, if set, what it
	// fails with instead.
	hold []string
	name string
	err  error
	// list, if set, is the signed list it answers with, and asks is whether
	// it asks a follower that shows no list for the one it has.
	list *pb.KeepList
	asks bool
	// questions, if set, are put to a follower that asks without answers.
	questions []*pb.Challenge
	// asked holds every request it has had.
	asked []*pb.ReplicateRequest
}

func (s *scripted) Replicate(_ context.Context, req *pb.ReplicateRequest) (*pb.ReplicateResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, req)
	if s.err != nil {
		return nil, s.err
	}
	var questions []*pb.Challenge
	if len(req.GetAnswers()) == 0 {
		questions = s.questions
	}
	return &pb.ReplicateResponse{Hold: s.hold, Store: s.name, List: s.list, ShowList: s.asks && req.GetList() == nil, Challenges: questions}, nil
}

func (s *scripted) Replication(stream grpc.BidiStreamingServer[pb.ReplicationHello, pb.ReplicationFrame]) error {
	request, err := ReadReplicationHello(stream.Recv)
	if err != nil {
		return err
	}
	response, err := s.Replicate(stream.Context(), request)
	if err != nil {
		return err
	}
	frames, err := ReplicationFrames(response)
	if err != nil {
		return err
	}
	for _, frame := range frames {
		if err := stream.Send(frame); err != nil {
			return err
		}
	}
	return nil
}

// ask has the coordinator put these questions from now on.
func (s *scripted) ask(questions ...*pb.Challenge) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.questions = questions
}

func (s *scripted) Get(req *pb.GetBlobRequest, stream grpc.ServerStreamingServer[pb.GetBlobResponse]) error {
	blob, err := s.store.Open(stream.Context(), cid.MustParse(req.GetCid()))
	if err != nil {
		return status.Errorf(codes.NotFound, "%v", err)
	}
	defer blob.Close()
	data, err := io.ReadAll(blob)
	if err != nil {
		return err
	}
	return stream.Send(&pb.GetBlobResponse{Data: data})
}

// say sets what the coordinator answers from now on.
func (s *scripted) say(name string, err error, hold ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.name, s.err, s.hold, s.list = name, err, hold, nil
}

// sign has the coordinator answer from now on with a list of the given
// blobs, as one that signs what it says does.
func (s *scripted) sign(name string, hold ...string) *pb.KeepList {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.name, s.err, s.hold, s.list = name, nil, nil, &pb.KeepList{Store: name, Sequence: uint64(len(s.asked)) + 1}
	for _, id := range hold {
		s.list.Blobs = append(s.list.Blobs, &pb.KeptBlob{Cid: id})
	}
	s.list.Signature = s.ident.Sign(signedBytes(s.list))
	return s.list
}

// requests returns how many times the coordinator has been asked, and the
// last request.
func (s *scripted) requests() (int, *pb.ReplicateRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.asked) == 0 {
		return 0, nil
	}
	return len(s.asked), s.asked[len(s.asked)-1]
}

// following returns a scripted coordinator and a follower of it with an
// empty store.
func following(t *testing.T) (*scripted, *Follower, *storage.Store, *logged) {
	t.Helper()
	ident := newIdentity(t)
	coordinator := &scripted{store: storage.NewMemory(), name: "node/first", ident: ident}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	pb.RegisterBlobServiceServer(srv, coordinator)
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	copies, logs := storage.NewMemory(), new(logged)
	return coordinator, &Follower{Store: copies, ListFile: filepath.Join(t.TempDir(), "kept.list"), Coordinator: pb.NewBlobServiceClient(conn), CoordinatorID: ident.ID(), Log: logs.logger()}, copies, logs
}

type legacyCoordinator struct {
	pb.UnimplementedBlobServiceServer
	response *pb.ReplicateResponse
	requests chan *pb.ReplicateRequest
}

func (s legacyCoordinator) Replicate(_ context.Context, request *pb.ReplicateRequest) (*pb.ReplicateResponse, error) {
	s.requests <- request
	return s.response, nil
}

func TestInitialReplicationFallsBackToAnOlderCoordinator(t *testing.T) {
	ident := newIdentity(t)
	store := ident.ID() + "/store"
	responseList := &pb.KeepList{Store: store, Sequence: 1}
	responseList.Signature = ident.Sign(signedBytes(responseList))
	legacy := legacyCoordinator{
		response: &pb.ReplicateResponse{Store: store, List: responseList},
		requests: make(chan *pb.ReplicateRequest, 1),
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	pb.RegisterBlobServiceServer(server, legacy)
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	listFile := filepath.Join(t.TempDir(), "kept.list")
	previousList := &pb.KeepList{Store: store, Sequence: 2}
	for range 60_000 {
		previousList.Blobs = append(previousList.Blobs, &pb.KeptBlob{Cid: strings.Repeat("b", 75)})
	}
	previousList.Signature = ident.Sign(signedBytes(previousList))
	if err := saveList(listFile, previousList); err != nil {
		t.Fatal(err)
	}
	request := &pb.ReplicateRequest{Store: store, List: previousList}
	frames, err := ReplicationHellos(request)
	if err != nil || len(frames) < 2 {
		t.Fatalf("large recovery hello uses %d frame(s), error %v; want multiple frames", len(frames), err)
	}
	follower := &Follower{
		Store: storage.NewMemory(), ListFile: listFile,
		Coordinator: pb.NewBlobServiceClient(conn), CoordinatorID: ident.ID(), Log: new(logged).logger(),
	}
	if _, err := follower.syncWith(context.Background(), true); err != nil {
		t.Fatalf("initial sync with legacy coordinator: %v", err)
	}
	select {
	case unary := <-legacy.requests:
		if unary.GetList() != nil {
			t.Fatal("legacy unary fallback resent the oversized signed list")
		}
		if size := proto.Size(unary); size >= 4<<20 {
			t.Fatalf("legacy unary fallback is %d bytes; want below gRPC's 4 MiB limit", size)
		}
	default:
		t.Fatal("large initial hello did not fall back to unary Replicate")
	}
}

func TestInitialReplicationWarnsAndContinuesWithoutUnreadableLastList(t *testing.T) {
	_, follower, _, logs := following(t)
	if err := os.WriteFile(follower.ListFile, []byte("not a signed list"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := follower.syncWith(context.Background(), true); err != nil {
		t.Fatalf("initial sync without a readable previous list: %v", err)
	}
	if got := logs.count("could not read the list this node's coordinator signed"); got != 1 {
		t.Errorf("unreadable last-list warnings = %d, want 1", got)
	}
}

func TestFollowerRejectsAnInvalidReplicationSignature(t *testing.T) {
	_, follower, _, _ := following(t)
	foreign := newIdentity(t)
	list := &pb.KeepList{Store: "foreign/store", Sequence: 1}
	list.Signature = foreign.Sign(signedBytes(list))
	if err := follower.verifyList(list); err == nil {
		t.Fatal("accepted a replication list signed by another identity")
	}
}

func TestFollowerIgnoresAnAlreadyAppliedPushedAssignment(t *testing.T) {
	coordinator, follower, copies, _ := following(t)
	blob := put(t, coordinator.store, "a stale assignment must not change the follower")
	list := &pb.KeepList{Store: "node/first", Sequence: 7, Blobs: []*pb.KeptBlob{{Cid: blob}}}
	list.Signature = coordinator.ident.Sign(signedBytes(list))
	follower.lastSequence = list.GetSequence()

	changed, err := follower.applyPush(ctx, &pb.ReplicateResponse{Store: "node/first", List: list})
	if err != nil || changed {
		t.Fatalf("applying a stale assignment: changed %v, error %v; want no change", changed, err)
	}
	if holds(t, copies, blob) {
		t.Error("the follower acted on an assignment it had already applied")
	}
}

func TestFollowerRejectsMalformedPushedAssignments(t *testing.T) {
	_, follower, _, _ := following(t)
	if _, err := follower.applyPush(ctx, &pb.ReplicateResponse{}); err == nil {
		t.Fatal("pushed assignment without a signed list was accepted")
	}

	list := &pb.KeepList{Store: "node/first", Sequence: 1}
	list.Signature = newIdentity(t).Sign(signedBytes(list))
	if _, err := follower.applyPush(ctx, &pb.ReplicateResponse{Store: "node/first", List: list}); err == nil {
		t.Fatal("pushed assignment signed by another identity was accepted")
	}
}

type replicationFrames struct {
	grpc.ClientStream
	frames []*pb.ReplicationFrame
	index  int
}

type recordedLeaseFrames struct {
	*replicationFrames
	sent chan *pb.ReplicationHello
}

type duplexReplicationFrames struct {
	*replicationFrames
	ctx      context.Context
	incoming chan *pb.ReplicationFrame
	sent     chan *pb.ReplicationHello
}

func (s *duplexReplicationFrames) Recv() (*pb.ReplicationFrame, error) {
	if s.index < len(s.frames) {
		return s.replicationFrames.Recv()
	}
	select {
	case frame := <-s.incoming:
		return frame, nil
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	}
}

func (s *duplexReplicationFrames) Send(frame *pb.ReplicationHello) error {
	select {
	case s.sent <- frame:
		return nil
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
}

type failLeaseReplicationFrames struct {
	*blockingReplicationFrames
	err        error
	failAfter  int
	leaseSends int
}

func (s *failLeaseReplicationFrames) Send(frame *pb.ReplicationHello) error {
	if frame.GetLease() {
		s.leaseSends++
		if s.leaseSends > s.failAfter {
			return s.err
		}
	}
	return nil
}

func (s *recordedLeaseFrames) Send(frame *pb.ReplicationHello) error {
	s.sent <- frame
	return nil
}

func (s *replicationFrames) Recv() (*pb.ReplicationFrame, error) {
	if s.index == len(s.frames) {
		return nil, io.EOF
	}
	frame := s.frames[s.index]
	s.index++
	return frame, nil
}

func (*replicationFrames) Send(*pb.ReplicationHello) error { return nil }

func (*replicationFrames) CloseSend() error { return nil }

func TestFollowerRenewsItsReplicationLeaseOnTheOpenStream(t *testing.T) {
	usual := LeaseInterval
	LeaseInterval = 5 * time.Millisecond
	t.Cleanup(func() { LeaseInterval = usual })
	stream := &recordedLeaseFrames{replicationFrames: &replicationFrames{}, sent: make(chan *pb.ReplicationHello, 2)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- (&Follower{}).sendLeases(ctx, stream, nil) }()
	for range 2 {
		select {
		case frame := <-stream.sent:
			if !frame.GetLease() || frame.GetSequence() != 0 || frame.GetIndex() != 0 || frame.GetCount() != 0 || len(frame.GetPayload()) != 0 {
				t.Fatalf("follower sent malformed lease frame: %v", frame)
			}
		case <-time.After(time.Second):
			t.Fatal("follower did not renew its stream lease")
		}
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("lease sender returned %v, want context canceled", err)
	}
}

func TestRunReportsAppliedPushOnTheOpenStream(t *testing.T) {
	ident := newIdentity(t)
	copies := storage.NewMemory()
	c, err := copies.Put(ctx, strings.NewReader("a follower copy to release"))
	if err != nil {
		t.Fatal(err)
	}
	if err := copies.Pin(ctx, ownerPrefix+"node/first", time.Time{}, c); err != nil {
		t.Fatal(err)
	}
	baseline := &pb.KeepList{Store: "node/first", Sequence: 1, Blobs: []*pb.KeptBlob{{Cid: c.String()}}}
	baseline.Signature = ident.Sign(signedBytes(baseline))
	assigned := &pb.KeepList{Store: "node/first", Sequence: 2}
	assigned.Signature = ident.Sign(signedBytes(assigned))
	initialFrames, err := ReplicationFrames(&pb.ReplicateResponse{Store: baseline.GetStore(), List: baseline})
	if err != nil {
		t.Fatal(err)
	}
	pushFrames, err := ReplicationFrames(&pb.ReplicateResponse{Store: assigned.GetStore(), List: assigned})
	if err != nil {
		t.Fatal(err)
	}
	var stream *duplexReplicationFrames
	created := make(chan struct{})
	client := &replicationClientStub{
		calls: make(chan struct{}, 1),
		replicate: func(context.Context, *pb.ReplicateRequest) (*pb.ReplicateResponse, error) {
			return nil, errors.New("healthy stream must report follower state without unary Replicate")
		},
		stream: func(streamCtx context.Context) (pb.BlobService_ReplicationClient, error) {
			stream = &duplexReplicationFrames{
				replicationFrames: &replicationFrames{frames: initialFrames}, ctx: streamCtx,
				incoming: make(chan *pb.ReplicationFrame, 4), sent: make(chan *pb.ReplicationHello, 16),
			}
			close(created)
			return stream, nil
		},
	}
	follower := &Follower{
		Store: copies, ListFile: filepath.Join(t.TempDir(), "kept.list"),
		Coordinator: client, CoordinatorID: ident.ID(), Log: new(logged).logger(),
	}
	stop := run(t, follower, time.Hour)
	defer stop()
	select {
	case <-created:
	case <-time.After(time.Second):
		t.Fatal("follower did not open its replication stream")
	}
	// The initial bounded hello and the first lease are the only outbound
	// messages until the coordinator pushes a new signed assignment.
	deadline := time.After(time.Second)
	for {
		select {
		case frame := <-stream.sent:
			if frame.GetLease() {
				goto leased
			}
		case <-deadline:
			t.Fatal("follower did not establish its stream lease")
		}
	}

leased:
	ack, err := ReplicationFrames(&pb.ReplicateResponse{})
	if err != nil {
		t.Fatal(err)
	}
	for _, frame := range ack {
		stream.incoming <- frame
	}
	for _, frame := range pushFrames {
		stream.incoming <- frame
	}
	var first *pb.ReplicationHello
	for first == nil {
		select {
		case frame := <-stream.sent:
			if !frame.GetLease() {
				first = frame
			}
		case <-time.After(time.Second):
			t.Fatal("follower did not report the applied push on its open stream")
		}
	}
	request, err := ReadReplicationRequestAfter(first, func() (*pb.ReplicationHello, error) {
		return <-stream.sent, nil
	})
	if err != nil {
		t.Fatalf("read live follower state report: %v", err)
	}
	if len(request.GetHolding()) != 0 || !request.GetAnswersChallenges() {
		t.Errorf("live report has holding=%v answersChallenges=%v, want no copies and challenge support", request.GetHolding(), request.GetAnswersChallenges())
	}
	if holds(t, copies, c.String()) {
		t.Error("follower kept a copy removed by the pushed assignment")
	}

	// A local pin change is reported on the same stream without waiting for
	// another coordinator assignment or the reconciliation interval.
	local, err := copies.Put(ctx, strings.NewReader("a local, non-pool pin"))
	if err != nil {
		t.Fatal(err)
	}
	if err := copies.Pin(ctx, "local", time.Time{}, local); err != nil {
		t.Fatal(err)
	}
	var changedFirst *pb.ReplicationHello
	for changedFirst == nil {
		select {
		case frame := <-stream.sent:
			if !frame.GetLease() {
				changedFirst = frame
			}
		case <-time.After(time.Second):
			t.Fatal("follower did not report a local pin change on its open stream")
		}
	}
	changedRequest, err := ReadReplicationRequestAfter(changedFirst, func() (*pb.ReplicationHello, error) {
		return <-stream.sent, nil
	})
	if err != nil {
		t.Fatalf("read local pin-change report: %v", err)
	}
	if len(changedRequest.GetHolding()) != 0 || !changedRequest.GetKeepsLists() {
		t.Errorf("local pin-change report has holding=%v keepsLists=%v, want no pool copies and signed-list support", changedRequest.GetHolding(), changedRequest.GetKeepsLists())
	}
}

func TestFollowerLeaseSenderReturnsRenewalError(t *testing.T) {
	usual := LeaseInterval
	LeaseInterval = time.Millisecond
	t.Cleanup(func() { LeaseInterval = usual })
	want := errors.New("lease send failed")
	stream := &failLeaseReplicationFrames{
		blockingReplicationFrames: &blockingReplicationFrames{replicationFrames: &replicationFrames{}},
		err:                       want, failAfter: 1,
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := (&Follower{}).sendLeases(ctx, stream, nil)
	if !errors.Is(err, want) {
		t.Errorf("lease sender returned %v, want %v", err, want)
	}
}

func TestFollowerLeaseSenderRejectsAnUnframeableStateReport(t *testing.T) {
	requests := make(chan *pb.ReplicateRequest, 1)
	requests <- nil
	stream := &recordedLeaseFrames{replicationFrames: &replicationFrames{}, sent: make(chan *pb.ReplicationHello, 1)}
	if err := (&Follower{}).sendLeases(context.Background(), stream, requests); err == nil {
		t.Fatal("lease sender accepted an unframeable nil state report")
	}
}

type blockingReplicationFrames struct {
	*replicationFrames
	ctx context.Context
}

func (s *blockingReplicationFrames) Recv() (*pb.ReplicationFrame, error) {
	frame, err := s.replicationFrames.Recv()
	if !errors.Is(err, io.EOF) {
		return frame, err
	}
	<-s.ctx.Done()
	return nil, s.ctx.Err()
}

type eofOnLeaseReplicationFrames struct{ *blockingReplicationFrames }

func (s *eofOnLeaseReplicationFrames) Send(frame *pb.ReplicationHello) error {
	if frame.GetLease() {
		return io.EOF
	}
	return nil
}

type failingReplicationFrames struct {
	*replicationFrames
	err error
}

func (s *failingReplicationFrames) Recv() (*pb.ReplicationFrame, error) {
	frame, err := s.replicationFrames.Recv()
	if !errors.Is(err, io.EOF) {
		return frame, err
	}
	return nil, s.err
}

type replicationClientStub struct {
	pb.BlobServiceClient
	replicate func(context.Context, *pb.ReplicateRequest) (*pb.ReplicateResponse, error)
	stream    func(context.Context) (pb.BlobService_ReplicationClient, error)
	calls     chan struct{}
	opened    chan struct{}
}

func (s *replicationClientStub) Replication(ctx context.Context, _ ...grpc.CallOption) (pb.BlobService_ReplicationClient, error) {
	if s.stream != nil {
		if s.opened != nil {
			select {
			case s.opened <- struct{}{}:
			default:
			}
		}
		return s.stream(ctx)
	}
	return nil, status.Error(codes.Unimplemented, "replication stream is not supported")
}

func (s *replicationClientStub) Replicate(ctx context.Context, request *pb.ReplicateRequest, _ ...grpc.CallOption) (*pb.ReplicateResponse, error) {
	select {
	case s.calls <- struct{}{}:
	default:
	}
	return s.replicate(ctx, request)
}

func frameResponse(t *testing.T, response *pb.ReplicateResponse) *pb.ReplicationFrame {
	t.Helper()
	frames, err := ReplicationFrames(response)
	if err != nil {
		t.Fatal(err)
	}
	return frames[0]
}

func TestReceiveAssignmentsRejectsMissingListsAndCoalescesSnapshots(t *testing.T) {
	ctx := context.Background()
	updates := make(chan *pb.ReplicateResponse, 1)
	ready := make(chan struct{}, 1)
	ended := make(chan error, 1)
	missing := &replicationFrames{frames: []*pb.ReplicationFrame{frameResponse(t, &pb.ReplicateResponse{Store: "missing"})}}
	(&Follower{}).receiveAssignments(ctx, missing, updates, ready, ended)
	if err := <-ended; err == nil || !strings.Contains(err.Error(), "without a signed list") {
		t.Fatalf("stream without a signed list ended with %v", err)
	}

	first := frameResponse(t, &pb.ReplicateResponse{List: &pb.KeepList{Sequence: 1}})
	latest := frameResponse(t, &pb.ReplicateResponse{List: &pb.KeepList{Sequence: 2}})
	ended = make(chan error, 1)
	(&Follower{}).receiveAssignments(ctx, &replicationFrames{frames: []*pb.ReplicationFrame{first, latest}}, updates, ready, ended)
	if got := (<-updates).GetList().GetSequence(); got != 2 {
		t.Errorf("coalesced assignment sequence = %d, want latest sequence 2", got)
	}
	if err := <-ended; !errors.Is(err, io.EOF) {
		t.Errorf("completed stream ended with %v, want EOF", err)
	}
}

func TestReceiveAssignmentsKeepsHighestSequenceWhenResponsesArriveOutOfOrder(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	updates := make(chan *pb.ReplicateResponse, 1)
	stream := &replicationFrames{frames: []*pb.ReplicationFrame{
		frameResponse(t, &pb.ReplicateResponse{List: &pb.KeepList{Sequence: 9}}),
		frameResponse(t, &pb.ReplicateResponse{List: &pb.KeepList{Sequence: 8}}),
	}}
	(&Follower{}).receiveAssignments(ctx, stream, updates, make(chan struct{}, 1), make(chan error, 1))
	if got := (<-updates).GetList().GetSequence(); got != 9 {
		t.Fatalf("coalesced assignment sequence = %d, want highest sequence 9", got)
	}
}

func TestReceiveAssignmentsKeepsChallengesWhenCoalescingSnapshots(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	updates := make(chan *pb.ReplicateResponse, 1)
	stream := &replicationFrames{frames: []*pb.ReplicationFrame{
		frameResponse(t, &pb.ReplicateResponse{List: &pb.KeepList{Sequence: 1}, Challenges: []*pb.Challenge{{Cid: "blob-cid", Nonce: []byte("nonce")}}}),
		frameResponse(t, &pb.ReplicateResponse{List: &pb.KeepList{Sequence: 2}}),
	}}
	(&Follower{}).receiveAssignments(ctx, stream, updates, make(chan struct{}, 1), make(chan error, 1))
	response := <-updates
	if got := response.GetList().GetSequence(); got != 2 {
		t.Fatalf("coalesced assignment sequence = %d, want latest sequence 2", got)
	}
	if len(response.GetChallenges()) != 1 || string(response.GetChallenges()[0].GetNonce()) != "nonce" {
		t.Fatalf("challenge was not preserved while coalescing: %v", response)
	}
}

func TestReceiveAssignmentsDeliversChallengeOnlyPushAfterCapabilityAck(t *testing.T) {
	ctx := context.Background()
	ready := make(chan struct{}, 2)
	updates := make(chan *pb.ReplicateResponse, 1)
	ended := make(chan error, 1)
	stream := &replicationFrames{frames: []*pb.ReplicationFrame{
		frameResponse(t, &pb.ReplicateResponse{}),
		frameResponse(t, &pb.ReplicateResponse{Challenges: []*pb.Challenge{{Cid: "blob-cid", Nonce: []byte("challenge-nonce")}}}),
	}}

	(&Follower{}).receiveAssignments(ctx, stream, updates, ready, ended)
	if len(ready) != 1 {
		t.Fatalf("capability acknowledgements = %d, want 1", len(ready))
	}
	response := <-updates
	if len(response.GetChallenges()) != 1 || string(response.GetChallenges()[0].GetNonce()) != "challenge-nonce" {
		t.Fatalf("challenge-only push was not delivered intact: %v", response)
	}
	if err := <-ended; !errors.Is(err, io.EOF) {
		t.Errorf("completed stream ended with %v, want EOF", err)
	}
}

func TestReceiveAssignmentsDropsAdditionalCapabilityAcksWhenReadyBufferIsFull(t *testing.T) {
	updates := make(chan *pb.ReplicateResponse, 1)
	ready := make(chan struct{}, 1)
	stream := &replicationFrames{frames: []*pb.ReplicationFrame{
		frameResponse(t, &pb.ReplicateResponse{}),
		frameResponse(t, &pb.ReplicateResponse{}),
	}}
	(&Follower{}).receiveAssignments(context.Background(), stream, updates, ready, make(chan error, 1))
	if len(ready) != 1 {
		t.Errorf("buffered capability acknowledgements = %d, want one coalesced acknowledgement", len(ready))
	}
}

func TestReceiveAssignmentsStopsWhenCanceledWhileWaitingToQueue(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	updates := make(chan *pb.ReplicateResponse)
	ended := make(chan error, 1)
	stream := &replicationFrames{frames: []*pb.ReplicationFrame{
		frameResponse(t, &pb.ReplicateResponse{List: &pb.KeepList{Sequence: 1}}),
	}}
	(&Follower{}).receiveAssignments(ctx, stream, updates, make(chan struct{}, 1), ended)
	if stream.index != 1 {
		t.Errorf("received %d frames after cancellation, want one", stream.index)
	}
}

func TestReceiveAssignmentsStopsWhenCanceledWhileDeliveringChallenge(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	updates := make(chan *pb.ReplicateResponse)
	ended := make(chan error, 1)
	stream := &replicationFrames{frames: []*pb.ReplicationFrame{
		frameResponse(t, &pb.ReplicateResponse{Challenges: []*pb.Challenge{{Cid: "blob-cid", Nonce: []byte("nonce")}}}),
	}}
	(&Follower{}).receiveAssignments(ctx, stream, updates, make(chan struct{}, 1), ended)
	if stream.index != 1 {
		t.Errorf("received %d frames after cancellation, want one", stream.index)
	}
}

func TestReceiveAssignmentsCanReportMalformedFrameAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stream := &replicationFrames{frames: []*pb.ReplicationFrame{
		frameResponse(t, &pb.ReplicateResponse{Store: "missing"}),
	}}
	(&Follower{}).receiveAssignments(ctx, stream, make(chan *pb.ReplicateResponse), make(chan struct{}), make(chan error))
}

func TestEnqueueReportChoosesBetweenSendingAndCancellation(t *testing.T) {
	request := &pb.ReplicateRequest{KeepsLists: true}
	requests := make(chan *pb.ReplicateRequest, 1)
	queued := false
	if !enqueueReport(context.Background(), requests, request, func() { queued = true }) {
		t.Fatal("report was not queued on an open context")
	}
	if !queued {
		t.Fatal("successful report enqueue did not run its callback")
	}
	if got := <-requests; got != request {
		t.Fatalf("queued report = %p, want %p", got, request)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if enqueueReport(ctx, make(chan *pb.ReplicateRequest), request, func() {
		t.Error("canceled report enqueue ran its callback")
	}) {
		t.Fatal("report was queued after its context was canceled")
	}
}

func TestLegacySyncWaitAndPinChangeTimer(t *testing.T) {
	interval := 5 * time.Second
	if got := legacySyncWait(interval, false); got != interval {
		t.Errorf("legacy wait without a pending report = %s, want %s", got, interval)
	}
	if got := legacySyncWait(interval, true); got != 0 {
		t.Errorf("legacy wait with a pending report = %s, want immediate", got)
	}

	if got := expediteLegacySync(time.NewTimer(time.Hour), nil); got != nil {
		t.Errorf("pin change without an active legacy timer returned %v, want nil", got)
	}
	timer := time.NewTimer(time.Hour)
	timerC := timer.C
	got := expediteLegacySync(timer, timerC)
	if got != timer.C {
		t.Fatal("pin change did not return the expedited timer channel")
	}
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("pin change did not expedite legacy reconciliation")
	}
}

func TestReceiveAssignmentsPrefersCancellationWithoutAnErrorReceiver(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stream := &replicationFrames{frames: []*pb.ReplicationFrame{frameResponse(t, &pb.ReplicateResponse{})}}
	(&Follower{}).receiveAssignments(ctx, stream, make(chan *pb.ReplicateResponse), make(chan struct{}, 1), make(chan error))
}

func TestResetTimerStopsOrDrainsBeforeResetting(t *testing.T) {
	active := time.NewTimer(time.Hour)
	resetTimer(active, time.Hour)
	active.Stop()

	expired := time.NewTimer(time.Millisecond)
	time.Sleep(5 * time.Millisecond)
	resetTimer(expired, time.Hour)
	expired.Stop()

	consumed := time.NewTimer(time.Millisecond)
	time.Sleep(5 * time.Millisecond)
	<-consumed.C
	resetTimer(consumed, time.Hour)
	consumed.Stop()
}

func TestRunImmediatelyReportsChangedLegacyBaseline(t *testing.T) {
	ident := newIdentity(t)
	copies := storage.NewMemory()
	c, err := copies.Put(ctx, strings.NewReader("a previously held follower copy"))
	if err != nil {
		t.Fatal(err)
	}
	if err := copies.Pin(ctx, ownerPrefix+"node/old", time.Time{}, c); err != nil {
		t.Fatal(err)
	}
	list := &pb.KeepList{Store: "node/first", Sequence: 1}
	list.Signature = ident.Sign(signedBytes(list))
	client := &replicationClientStub{
		calls: make(chan struct{}, 4),
		replicate: func(context.Context, *pb.ReplicateRequest) (*pb.ReplicateResponse, error) {
			return &pb.ReplicateResponse{Store: list.GetStore(), List: list}, nil
		},
	}
	follower := &Follower{
		Store: copies, ListFile: filepath.Join(t.TempDir(), "kept.list"),
		Coordinator: client, CoordinatorID: ident.ID(), Log: new(logged).logger(),
	}
	stop := run(t, follower, time.Hour)
	select {
	case <-client.calls:
	case <-time.After(time.Second):
		t.Fatal("legacy baseline did not make its initial unary request")
	}
	select {
	case <-client.calls:
	case <-time.After(time.Second):
		t.Fatal("changed legacy baseline was not reported immediately")
	}
	stop()
	if holds(t, copies, c.String()) {
		t.Error("the follower kept a blob absent from its new signed assignment")
	}
}

func TestRunKeepsUnaryReconciliationWhenCoordinatorClosesStreamCleanly(t *testing.T) {
	ident := newIdentity(t)
	baseline := &pb.KeepList{Store: "node/first", Sequence: 1}
	baseline.Signature = ident.Sign(signedBytes(baseline))
	logs := new(logged)
	client := &replicationClientStub{
		calls: make(chan struct{}, 64), opened: make(chan struct{}, 64),
		replicate: func(context.Context, *pb.ReplicateRequest) (*pb.ReplicateResponse, error) {
			return &pb.ReplicateResponse{Store: baseline.GetStore(), List: baseline}, nil
		},
		stream: func(context.Context) (pb.BlobService_ReplicationClient, error) {
			return &replicationFrames{frames: []*pb.ReplicationFrame{
				frameResponse(t, &pb.ReplicateResponse{Store: baseline.GetStore(), List: baseline}),
			}}, nil
		},
	}
	follower := &Follower{
		Store: storage.NewMemory(), ListFile: filepath.Join(t.TempDir(), "kept.list"),
		Coordinator: client, CoordinatorID: ident.ID(), Log: logs.logger(),
	}
	stop := run(t, follower, 5*time.Millisecond)
	for range 2 {
		select {
		case <-client.calls:
		case <-time.After(5 * time.Second):
			stop()
			t.Fatal("the follower stopped reconciling after the stream ended")
		}
	}
	stop()

	if got := len(client.opened); got != 1 {
		t.Errorf("opened assignment stream %d times, want one", got)
	}
	if got := logs.count("the coordinator does not push storage assignments"); got != 1 {
		t.Errorf("logged unsupported-stream info %d times, want one", got)
	}
	if got := logs.count("the storage assignment stream ended; reconnecting"); got != 0 {
		t.Errorf("logged %d stream reconnect warnings for a clean EOF, want none", got)
	}
}

func TestRunWaitsForRecvAfterLeaseSendReturnsEOF(t *testing.T) {
	ident := newIdentity(t)
	baseline := &pb.KeepList{Store: "node/first", Sequence: 1}
	baseline.Signature = ident.Sign(signedBytes(baseline))
	client := &replicationClientStub{
		calls: make(chan struct{}, 4), opened: make(chan struct{}, 4),
		replicate: func(context.Context, *pb.ReplicateRequest) (*pb.ReplicateResponse, error) {
			return &pb.ReplicateResponse{Store: baseline.GetStore(), List: baseline}, nil
		},
		stream: func(ctx context.Context) (pb.BlobService_ReplicationClient, error) {
			return &eofOnLeaseReplicationFrames{blockingReplicationFrames: &blockingReplicationFrames{
				replicationFrames: &replicationFrames{frames: []*pb.ReplicationFrame{
					frameResponse(t, &pb.ReplicateResponse{Store: baseline.GetStore(), List: baseline}),
				}},
				ctx: ctx,
			}}, nil
		},
	}
	follower := &Follower{
		Store: storage.NewMemory(), ListFile: filepath.Join(t.TempDir(), "kept.list"),
		Coordinator: client, CoordinatorID: ident.ID(), Log: new(logged).logger(),
	}
	stop := run(t, follower, 5*time.Millisecond)
	select {
	case <-client.calls:
	case <-time.After(time.Second):
		stop()
		t.Fatal("unary anti-entropy did not continue after lease Send returned EOF")
	}
	stop()
	if got := len(client.opened); got != 1 {
		t.Errorf("opened assignment stream %d times, want one when Recv is still open", got)
	}
}

func TestRunReconnectsWhenLeaseRenewalFails(t *testing.T) {
	ident := newIdentity(t)
	baseline := &pb.KeepList{Store: "node/first", Sequence: 1}
	baseline.Signature = ident.Sign(signedBytes(baseline))
	want := errors.New("lease renewal failed")
	client := &replicationClientStub{
		calls: make(chan struct{}, 4), opened: make(chan struct{}, 4),
		replicate: func(context.Context, *pb.ReplicateRequest) (*pb.ReplicateResponse, error) {
			return &pb.ReplicateResponse{Store: baseline.GetStore(), List: baseline}, nil
		},
		stream: func(ctx context.Context) (pb.BlobService_ReplicationClient, error) {
			return &failLeaseReplicationFrames{
				blockingReplicationFrames: &blockingReplicationFrames{
					replicationFrames: &replicationFrames{frames: []*pb.ReplicationFrame{
						frameResponse(t, &pb.ReplicateResponse{Store: baseline.GetStore(), List: baseline}),
					}},
					ctx: ctx,
				},
				err: want,
			}, nil
		},
	}
	logs := new(logged)
	follower := &Follower{
		Store: storage.NewMemory(), ListFile: filepath.Join(t.TempDir(), "kept.list"),
		Coordinator: client, CoordinatorID: ident.ID(), Log: logs.logger(),
	}
	stop := run(t, follower, time.Second)
	defer stop()
	deadline := time.After(time.Second)
	for logs.count("the storage assignment lease could not be renewed; reconnecting") == 0 {
		select {
		case <-deadline:
			t.Fatal("failed lease renewal was not logged")
		case <-time.After(time.Millisecond):
		}
	}
	if got := len(client.opened); got < 1 {
		t.Fatal("follower did not open its assignment stream")
	}
}

func TestRunReopensAssignmentStreamAfterUnaryFailureFollowingCleanEOF(t *testing.T) {
	ident := newIdentity(t)
	baseline := &pb.KeepList{Store: "node/first", Sequence: 1}
	baseline.Signature = ident.Sign(signedBytes(baseline))
	var calls int
	var mu sync.Mutex
	var streams int
	client := &replicationClientStub{
		calls: make(chan struct{}, 8), opened: make(chan struct{}, 4),
		replicate: func(context.Context, *pb.ReplicateRequest) (*pb.ReplicateResponse, error) {
			mu.Lock()
			defer mu.Unlock()
			calls++
			if calls == 1 {
				return nil, status.Error(codes.Unavailable, "coordinator restarted")
			}
			return &pb.ReplicateResponse{Store: baseline.GetStore(), List: baseline}, nil
		},
		stream: func(ctx context.Context) (pb.BlobService_ReplicationClient, error) {
			streams++
			if streams == 2 {
				return &blockingReplicationFrames{
					replicationFrames: &replicationFrames{frames: []*pb.ReplicationFrame{
						frameResponse(t, &pb.ReplicateResponse{Store: baseline.GetStore(), List: baseline}),
					}},
					ctx: ctx,
				}, nil
			}
			return &replicationFrames{frames: []*pb.ReplicationFrame{
				frameResponse(t, &pb.ReplicateResponse{Store: baseline.GetStore(), List: baseline}),
			}}, nil
		},
	}
	follower := &Follower{
		Store: storage.NewMemory(), ListFile: filepath.Join(t.TempDir(), "kept.list"),
		Coordinator: client, CoordinatorID: ident.ID(), Log: new(logged).logger(),
	}
	stop := run(t, follower, 5*time.Millisecond)
	for range 2 {
		select {
		case <-client.opened:
		case <-time.After(5 * time.Second):
			stop()
			t.Fatal("the follower did not reopen its stream after unary reconciliation failed")
		}
	}
	stop()
}

func TestRunReconnectsAfterReplicationStreamError(t *testing.T) {
	ident := newIdentity(t)
	baseline := &pb.KeepList{Store: "node/first", Sequence: 1}
	baseline.Signature = ident.Sign(signedBytes(baseline))
	logs := new(logged)
	var streams int
	client := &replicationClientStub{
		calls: make(chan struct{}, 8), opened: make(chan struct{}, 4),
		replicate: func(context.Context, *pb.ReplicateRequest) (*pb.ReplicateResponse, error) {
			return &pb.ReplicateResponse{Store: baseline.GetStore(), List: baseline}, nil
		},
		stream: func(ctx context.Context) (pb.BlobService_ReplicationClient, error) {
			streams++
			if streams == 1 {
				return &failingReplicationFrames{
					replicationFrames: &replicationFrames{frames: []*pb.ReplicationFrame{
						frameResponse(t, &pb.ReplicateResponse{Store: baseline.GetStore(), List: baseline}),
					}},
					err: status.Error(codes.Unavailable, "stream interrupted"),
				}, nil
			}
			return &blockingReplicationFrames{
				replicationFrames: &replicationFrames{frames: []*pb.ReplicationFrame{
					frameResponse(t, &pb.ReplicateResponse{Store: baseline.GetStore(), List: baseline}),
				}},
				ctx: ctx,
			}, nil
		},
	}
	follower := &Follower{
		Store: storage.NewMemory(), ListFile: filepath.Join(t.TempDir(), "kept.list"),
		Coordinator: client, CoordinatorID: ident.ID(), Log: logs.logger(),
	}
	stop := run(t, follower, 5*time.Millisecond)
	for range 2 {
		select {
		case <-client.opened:
		case <-time.After(5 * time.Second):
			stop()
			t.Fatal("the follower did not reconnect after a replication stream error")
		}
	}
	stop()
	if got := logs.count("the storage assignment stream ended; reconnecting"); got != 1 {
		t.Errorf("logged stream reconnect warning %d times, want one", got)
	}
}

func TestRunReconnectsAfterRejectingAnInvalidPushedAssignment(t *testing.T) {
	ident := newIdentity(t)
	baseline := &pb.KeepList{Store: "node/first", Sequence: 1}
	baseline.Signature = ident.Sign(signedBytes(baseline))
	foreign := &pb.KeepList{Store: "node/first", Sequence: 2}
	foreign.Signature = newIdentity(t).Sign(signedBytes(foreign))
	client := &replicationClientStub{
		calls: make(chan struct{}, 4), opened: make(chan struct{}, 4),
		replicate: func(context.Context, *pb.ReplicateRequest) (*pb.ReplicateResponse, error) {
			return &pb.ReplicateResponse{Store: baseline.GetStore(), List: baseline}, nil
		},
		stream: func(ctx context.Context) (pb.BlobService_ReplicationClient, error) {
			return &blockingReplicationFrames{
				replicationFrames: &replicationFrames{frames: []*pb.ReplicationFrame{
					frameResponse(t, &pb.ReplicateResponse{Store: baseline.GetStore(), List: baseline}),
					frameResponse(t, &pb.ReplicateResponse{Store: foreign.GetStore(), List: foreign}),
				}},
				ctx: ctx,
			}, nil
		},
	}
	follower := &Follower{Store: storage.NewMemory(), ListFile: filepath.Join(t.TempDir(), "kept.list"), Coordinator: client, CoordinatorID: ident.ID(), Log: new(logged).logger()}
	stop := run(t, follower, 5*time.Millisecond)
	for range 2 {
		select {
		case <-client.opened:
		case <-time.After(time.Second):
			stop()
			t.Fatal("the follower did not reconnect after rejecting the foreign assignment")
		}
	}
	stop()
}

func TestRunDoesNotPollWhileAssignmentStreamIsHealthy(t *testing.T) {
	ident := newIdentity(t)
	baseline := &pb.KeepList{Store: "node/first", Sequence: 1}
	baseline.Signature = ident.Sign(signedBytes(baseline))
	client := &replicationClientStub{
		calls: make(chan struct{}, 4), opened: make(chan struct{}, 2),
		replicate: func(context.Context, *pb.ReplicateRequest) (*pb.ReplicateResponse, error) {
			return nil, errors.New("healthy assignment stream must not use unary anti-entropy")
		},
		stream: func(ctx context.Context) (pb.BlobService_ReplicationClient, error) {
			return &blockingReplicationFrames{
				replicationFrames: &replicationFrames{frames: []*pb.ReplicationFrame{
					frameResponse(t, &pb.ReplicateResponse{Store: baseline.GetStore(), List: baseline}),
					frameResponse(t, &pb.ReplicateResponse{}),
				}},
				ctx: ctx,
			}, nil
		},
	}
	follower := &Follower{Store: storage.NewMemory(), ListFile: filepath.Join(t.TempDir(), "kept.list"), Coordinator: client, CoordinatorID: ident.ID(), Log: new(logged).logger()}
	stop := run(t, follower, 5*time.Millisecond)
	time.Sleep(30 * time.Millisecond)
	stop()
	if got := len(client.calls); got != 0 {
		t.Fatalf("healthy assignment stream made %d unary requests, want none", got)
	}
}

func TestRunStopsItsStreamWhenCanceledBeforeTheInitialExchange(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	streamCtx, streamCancel := context.WithCancel(context.Background())
	client := &replicationClientStub{
		calls: make(chan struct{}, 1),
		replicate: func(ctx context.Context, _ *pb.ReplicateRequest) (*pb.ReplicateResponse, error) {
			return nil, ctx.Err()
		},
	}
	follower := &Follower{Store: storage.NewMemory(), Coordinator: client, Log: new(logged).logger(), streamCancel: streamCancel}
	follower.Run(ctx)
	if streamCtx.Err() == nil {
		t.Error("the follower left its stream context running after cancellation")
	}
}

func TestInitialSyncCancelsItsStreamWhenSignedListRecoveryFails(t *testing.T) {
	ident := newIdentity(t)
	list := &pb.KeepList{Store: "node/first", Sequence: 1}
	list.Signature = ident.Sign(signedBytes(list))
	listFile := filepath.Join(t.TempDir(), "kept.list")
	if err := saveList(listFile, list); err != nil {
		t.Fatal(err)
	}
	var streamCtx context.Context
	client := &replicationClientStub{
		calls: make(chan struct{}, 1),
		replicate: func(context.Context, *pb.ReplicateRequest) (*pb.ReplicateResponse, error) {
			return nil, status.Error(codes.Unavailable, "recovery request failed")
		},
		stream: func(ctx context.Context) (pb.BlobService_ReplicationClient, error) {
			streamCtx = ctx
			return &blockingReplicationFrames{
				replicationFrames: &replicationFrames{frames: []*pb.ReplicationFrame{
					frameResponse(t, &pb.ReplicateResponse{Store: list.GetStore(), List: list, ShowList: true}),
				}},
				ctx: ctx,
			}, nil
		},
	}
	follower := &Follower{
		Store: storage.NewMemory(), ListFile: listFile, Coordinator: client,
		CoordinatorID: ident.ID(), Log: new(logged).logger(),
	}
	if _, err := follower.syncWith(ctx, true); status.Code(err) != codes.Unavailable {
		t.Fatalf("initial signed-list recovery error = %v, want Unavailable", err)
	}
	if streamCtx == nil || streamCtx.Err() == nil {
		t.Error("the follower left its stream open after signed-list recovery failed")
	}
}

// run runs a follower, asking every interval, until the test ends or the
// returned function is called.
func run(t *testing.T, f *Follower, interval time.Duration) (stop func()) {
	t.Helper()
	usual := Interval
	Interval = interval
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.Run(ctx)
	}()
	stop = sync.OnceFunc(func() {
		cancel()
		<-done
		Interval = usual
	})
	t.Cleanup(stop)
	return stop
}

func eventually(t *testing.T, what string, condition func() bool) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); !condition(); time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("never happened: %s", what)
		}
	}
}

// pinsOf lists a store's pins as "owner cid", leaving out the short pin
// every new blob gets.
func pinsOf(store *storage.Store) []string {
	var pins []string
	for _, pin := range store.Pins() {
		if pin.Owner != storage.GraceOwner {
			pins = append(pins, pin.Owner+" "+pin.CID.String())
		}
	}
	slices.Sort(pins)
	return pins
}

func holds(t *testing.T, store *storage.Store, id string) bool {
	t.Helper()
	has, err := store.Has(ctx, cid.MustParse(id))
	if err != nil {
		t.Fatal(err)
	}
	return has
}

func TestAFollowerFetchesWhatItIsToldToHoldAndDropsTheRest(t *testing.T) {
	coordinator, follower, copies, _ := following(t)
	first, second := put(t, coordinator.store, "the first blob"), put(t, coordinator.store, "the second blob")
	// Something else in the store, which is none of the pool's.
	other := keep(t, copies, "kept here for another reason")
	coordinator.say("node/first", nil, first, second)

	// It asks once a day, and again at once when what it holds has changed.
	run(t, follower, 24*time.Hour)
	eventually(t, "the follower says it holds both blobs", func() bool {
		_, last := coordinator.requests()
		return len(last.GetHolding()) == 2
	})
	asked, last := coordinator.requests()
	want := []string{first, second}
	slices.Sort(want)
	if asked != 2 || !slices.Equal(last.GetHolding(), want) || last.GetStore() != "node/first" {
		t.Errorf("after %d requests the follower says it holds %v for %q, want both blobs for the store named, at the second", asked, last.GetHolding(), last.GetStore())
	}
	wantPins := []string{"pool:node/first " + first, "pool:node/first " + second, "user " + other}
	slices.Sort(wantPins)
	if pins := pinsOf(copies); !slices.Equal(pins, wantPins) {
		t.Errorf("the follower's pins are %v, want %v", pins, wantPins)
	}
	time.Sleep(50 * time.Millisecond)
	if asked, _ := coordinator.requests(); asked != 2 {
		t.Errorf("with nothing changing the follower asked %d times in all, want 2", asked)
	}
}

func TestAFollowerDeletesWhatItIsNoLongerToHold(t *testing.T) {
	coordinator, follower, copies, _ := following(t)
	stays, goes := put(t, coordinator.store, "stays"), put(t, coordinator.store, "goes")
	coordinator.say("node/first", nil, stays, goes)
	if changed, err := follower.sync(ctx); err != nil || !changed {
		t.Fatalf("fetching two blobs: changed %v, error %v", changed, err)
	}
	if changed, err := follower.sync(ctx); err != nil || changed {
		t.Fatalf("asking again with nothing to do: changed %v, error %v", changed, err)
	}

	coordinator.say("node/first", nil, stays)
	if changed, err := follower.sync(ctx); err != nil || !changed {
		t.Fatalf("dropping a blob: changed %v, error %v", changed, err)
	}
	if holds(t, copies, goes) || !holds(t, copies, stays) {
		t.Errorf("after one blob was released the follower holds the released one: %v, the other: %v", holds(t, copies, goes), holds(t, copies, stays))
	}
	if pins := pinsOf(copies); !slices.Equal(pins, []string{"pool:node/first " + stays}) {
		t.Errorf("the follower's pins are %v", pins)
	}
}

func TestAFollowerKeepsWhatItHasWhileItsCoordinatorCannotBeAsked(t *testing.T) {
	coordinator, follower, copies, logs := following(t)
	held := put(t, coordinator.store, "held through an outage")
	coordinator.say("node/first", nil, held)
	if _, err := follower.sync(ctx); err != nil {
		t.Fatal(err)
	}

	coordinator.say("", status.Error(codes.Unavailable, "the coordinator is away"))
	stop := run(t, follower, 5*time.Millisecond)
	eventually(t, "the follower asks several times", func() bool {
		asked, _ := coordinator.requests()
		return asked >= 5
	})
	if !holds(t, copies, held) || len(pinsOf(copies)) != 1 {
		t.Errorf("with its coordinator away the follower's pins are %v", pinsOf(copies))
	}
	if n := logs.count("could not bring this node's copies"); n != 1 {
		t.Errorf("an outage was logged %d times, want once for the lot", n)
	}

	// When the coordinator is back the follower carries on, and a second
	// outage is news again.
	coordinator.say("node/first", nil, held)
	before, _ := coordinator.requests()
	eventually(t, "the follower asks again", func() bool {
		asked, _ := coordinator.requests()
		return asked >= before+2
	})
	coordinator.say("", status.Error(codes.Unavailable, "the coordinator is away again"))
	eventually(t, "the second outage is logged", func() bool { return logs.count("could not bring this node's copies") == 2 })
	stop()
	if !holds(t, copies, held) {
		t.Error("the follower dropped a blob while its coordinator was away")
	}
}

func TestAFollowerHoldsWhatItCanWhenSomeOfItCannotBeFetched(t *testing.T) {
	coordinator, follower, copies, logs := following(t)
	there := put(t, coordinator.store, "the coordinator has this one")
	missing := put(t, storage.NewMemory(), "and has lost this one")
	coordinator.say("node/first", nil, missing, "not-a-cid", there)

	changed, err := follower.sync(ctx)
	if err != nil || !changed {
		t.Fatalf("fetching one blob of three: changed %v, error %v", changed, err)
	}
	if pins := pinsOf(copies); !slices.Equal(pins, []string{"pool:node/first " + there}) {
		t.Errorf("the follower's pins are %v, want only the blob it could fetch", pins)
	}
	if n := logs.count("could not fetch a blob this node is to hold"); n != 2 {
		t.Errorf("%d failed fetches were logged, want 2", n)
	}
}

func TestAFollowerHoldsItsCopiesForTheStoreItsCoordinatorNames(t *testing.T) {
	coordinator, follower, copies, _ := following(t)
	blob, other := put(t, coordinator.store, "held for one store, then the next"), put(t, coordinator.store, "another")
	coordinator.say("node/first", nil, blob, other)
	if _, err := follower.sync(ctx); err != nil {
		t.Fatal(err)
	}
	// A change of name that was cut short left one blob held under both.
	if err := copies.Pin(ctx, "pool:node/second", time.Time{}, cid.MustParse(blob)); err != nil {
		t.Fatal(err)
	}

	coordinator.say("node/second", nil, blob, other)
	changed, err := follower.sync(ctx)
	if err != nil || !changed {
		t.Fatalf("taking up a new name for the store: changed %v, error %v", changed, err)
	}
	want := []string{"pool:node/second " + blob, "pool:node/second " + other}
	slices.Sort(want)
	if pins := pinsOf(copies); !slices.Equal(pins, want) {
		t.Errorf("the follower's pins are %v, want both blobs held for the second store only", pins)
	}
	if !holds(t, copies, blob) || !holds(t, copies, other) {
		t.Error("a blob was deleted in changing the name it is held under")
	}
	if _, err := follower.sync(ctx); err != nil {
		t.Fatal(err)
	}
	if _, last := coordinator.requests(); last.GetStore() != "node/second" || len(last.GetHolding()) != 2 {
		t.Errorf("the follower now says it holds %v for %q", last.GetHolding(), last.GetStore())
	}
}

// readOnly is a store that cannot record a pin.
type readOnly struct{ *storage.Store }

var errReadOnly = errors.New("the pin file is read-only")

func (readOnly) Pin(context.Context, string, time.Time, ...cid.Cid) error { return errReadOnly }

func TestAFollowerWhoseStoreFailsSaysSoAndDoesNotAskInARush(t *testing.T) {
	coordinator, follower, copies, logs := following(t)
	follower.Store = readOnly{copies}
	blob := put(t, coordinator.store, "fetched, and then not pinned")
	coordinator.say("node/first", nil, blob)

	if _, err := follower.sync(ctx); !errors.Is(err, errReadOnly) {
		t.Fatalf("with a store that cannot pin: %v", err)
	}
	run(t, follower, 24*time.Hour)
	eventually(t, "the failure is logged", func() bool { return logs.count("the pin file is read-only") == 1 })
	time.Sleep(50 * time.Millisecond)
	if asked, _ := coordinator.requests(); asked != 2 {
		t.Errorf("the follower has asked %d times, want once by hand and once more: a fetch that could not be kept is no reason to ask again at once", asked)
	}
}

func TestAFollowerKeepsTheLastListItsCoordinatorSigned(t *testing.T) {
	coordinator, follower, copies, _ := following(t)
	first, second := put(t, coordinator.store, "on the first list"), put(t, coordinator.store, "on both lists")
	if list, err := loadList(follower.ListFile); list != nil || err != nil {
		t.Fatalf("before it has asked the follower keeps %v, error %v", list, err)
	}

	signed := coordinator.sign("node/first", first, second)
	if changed, err := follower.sync(ctx); err != nil || !changed {
		t.Fatalf("fetching what a list names: changed %v, error %v", changed, err)
	}
	want := []string{"pool:node/first " + first, "pool:node/first " + second}
	slices.Sort(want)
	if pins := pinsOf(copies); !slices.Equal(pins, want) {
		t.Errorf("the follower's pins are %v, want the two blobs the list names", pins)
	}
	if kept, err := loadList(follower.ListFile); err != nil || !proto.Equal(kept, signed) {
		t.Errorf("the follower keeps %v, error %v, want the list it was given, signature and all", kept, err)
	}
	if _, last := coordinator.requests(); !last.GetKeepsLists() || last.GetList() != nil {
		t.Errorf("the follower asked with %v, want it to say it keeps lists and to show none unasked", last)
	}

	// A later list takes the place of the first, and what it leaves out goes.
	signed = coordinator.sign("node/first", second)
	if changed, err := follower.sync(ctx); err != nil || !changed {
		t.Fatalf("dropping what a list no longer names: changed %v, error %v", changed, err)
	}
	if kept, err := loadList(follower.ListFile); err != nil || !proto.Equal(kept, signed) {
		t.Errorf("the follower keeps %v, error %v, want the later list", kept, err)
	}
	if holds(t, copies, first) || !holds(t, copies, second) {
		t.Error("the follower does not hold exactly what the later list names")
	}
	// An answer with no list leaves the one it has alone.
	coordinator.say("node/first", nil, second)
	if _, err := follower.sync(ctx); err != nil {
		t.Fatal(err)
	}
	if kept, err := loadList(follower.ListFile); err != nil || !proto.Equal(kept, signed) {
		t.Errorf("after an answer without a list the follower keeps %v, error %v", kept, err)
	}
}

func TestAFollowerShowsItsListWhenItsCoordinatorAsks(t *testing.T) {
	coordinator, follower, copies, logs := following(t)
	blob := put(t, coordinator.store, "held for a store the coordinator has lost")
	signed := coordinator.sign("node/first", blob)
	if _, err := follower.sync(ctx); err != nil {
		t.Fatal(err)
	}

	// The coordinator comes back with another store, and asks.
	coordinator.say("node/first", nil, blob)
	coordinator.mu.Lock()
	coordinator.asks = true
	coordinator.mu.Unlock()
	before, _ := coordinator.requests()
	if changed, err := follower.sync(ctx); err != nil || changed {
		t.Fatalf("showing the list: changed %v, error %v", changed, err)
	}
	asked, last := coordinator.requests()
	if asked != before+2 || !proto.Equal(last.GetList(), signed) || !slices.Equal(last.GetHolding(), []string{blob}) || last.GetStore() != "node/first" {
		t.Errorf("asked for its list, the follower made %d requests, the last %v, want a second that shows the list it kept", asked-before, last)
	}

	// A follower whose list cannot be read says so and carries on with what
	// it was told; one that has none just carries on.
	if err := os.WriteFile(follower.ListFile, []byte("not a list"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, unreadable := range []int{1, 1} {
		before, _ = coordinator.requests()
		if _, err := follower.sync(ctx); err != nil {
			t.Fatal(err)
		}
		if asked, _ := coordinator.requests(); asked != before+1 {
			t.Errorf("with no list to show the follower made %d requests, want one", asked-before)
		}
		if n := logs.count("could not read the list this node's coordinator signed"); n != unreadable {
			t.Errorf("a list that cannot be read was logged %d times, want %d", n, unreadable)
		}
		if err := os.Remove(follower.ListFile); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	if !holds(t, copies, blob) {
		t.Error("the follower dropped its copy while its coordinator was asking for a list")
	}
}

func TestAFollowerThatCannotKeepAListDoesNotActOnIt(t *testing.T) {
	coordinator, follower, copies, _ := following(t)
	held, fresh := put(t, coordinator.store, "held already"), put(t, coordinator.store, "named by a list that cannot be kept")
	coordinator.say("node/first", nil, held)
	if _, err := follower.sync(ctx); err != nil {
		t.Fatal(err)
	}

	follower.ListFile = filepath.Join(t.TempDir(), "absent", "kept.list")
	coordinator.sign("node/first", fresh)
	if changed, err := follower.sync(ctx); err == nil || changed {
		t.Fatalf("with nowhere to keep the list: changed %v, error %v", changed, err)
	}
	if !holds(t, copies, held) || holds(t, copies, fresh) {
		t.Error("the follower acted on a list it could not keep")
	}
}
