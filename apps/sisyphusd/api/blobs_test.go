package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ipfs/go-cid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/replication"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// These tests call the blob service's handlers directly with streams and
// stores that fail, covering what a real connection only does when a disk or
// a client breaks at the wrong moment. The working paths are tested over
// real connections in the daemon's own tests.

var errDisk = errors.New("disk on fire")

// brokenStore is a store whose operations fail on request.
type brokenStore struct {
	*storage.Store
	failPut, failOpen, failRead bool
	failPin, failGC, failSize   bool
	// used, if set, is what the store claims to occupy on disk.
	used uint64
}

func (b brokenStore) Pin(ctx context.Context, owner string, expires time.Time, cids ...cid.Cid) error {
	if b.failPin {
		return errDisk
	}
	return b.Store.Pin(ctx, owner, expires, cids...)
}

func (b brokenStore) PinWithin(ctx context.Context, owner string, expires time.Time, limit uint64, cids ...cid.Cid) error {
	if b.failPin {
		return errDisk
	}
	return b.Store.PinWithin(ctx, owner, expires, limit, cids...)
}

func (b brokenStore) Unpin(owner string, cids ...cid.Cid) error {
	if b.failPin {
		return errDisk
	}
	return b.Store.Unpin(owner, cids...)
}

func (b brokenStore) GC(ctx context.Context, now time.Time) (storage.Collected, error) {
	if b.failGC {
		return storage.Collected{}, errDisk
	}
	return b.Store.GC(ctx, now)
}

func (b brokenStore) Size(context.Context) (uint64, error) {
	if b.failSize {
		return 0, errDisk
	}
	return b.used, nil
}

func (b brokenStore) Put(ctx context.Context, r io.Reader) (cid.Cid, error) {
	if b.failPut {
		return cid.Undef, errDisk
	}
	return b.Store.Put(ctx, r)
}

func (b brokenStore) Open(ctx context.Context, c cid.Cid) (storage.Blob, error) {
	if b.failOpen {
		return nil, errDisk
	}
	blob, err := b.Store.Open(ctx, c)
	if err != nil || !b.failRead {
		return blob, err
	}
	return unreadableBlob{blob}, nil
}

type unreadableBlob struct{ storage.Blob }

func (unreadableBlob) Read([]byte) (int, error) { return 0, errDisk }

// upload is the server's side of a client's upload.
type upload struct {
	grpc.ServerStream
	// first, if set, is the first message, before the chunks.
	first   *pb.PutBlobRequest
	chunks  [][]byte
	recvErr error // returned after the chunks instead of a clean end
}

func (u *upload) Context() context.Context { return context.Background() }

func (u *upload) Recv() (*pb.PutBlobRequest, error) {
	if first := u.first; first != nil {
		u.first = nil
		return first, nil
	}
	if len(u.chunks) > 0 {
		chunk := u.chunks[0]
		u.chunks = u.chunks[1:]
		return &pb.PutBlobRequest{Data: chunk}, nil
	}
	if u.recvErr != nil {
		return nil, u.recvErr
	}
	return nil, io.EOF
}

func (u *upload) SendAndClose(*pb.PutBlobResponse) error { return nil }

// download is the server's side of a client's download.
type download struct {
	grpc.ServerStream
	sendErr error
}

func (d *download) Context() context.Context { return context.Background() }

func (d *download) Send(*pb.GetBlobResponse) error { return d.sendErr }

type replicationExchange struct {
	grpc.ServerStream
	hello   *pb.ReplicationHello
	recvErr error
	sendErr error
	sent    []*pb.ReplicationFrame
}

type halfClosedReplicationExchange struct {
	grpc.ServerStream
	ctx      context.Context
	hello    *pb.ReplicationHello
	second   *pb.ReplicationHello
	received bool
	sent     chan *pb.ReplicationFrame
}

type failingPushReplicationExchange struct {
	*halfClosedReplicationExchange
	sends int
	err   error
}

type blockedLeaseRecvExchange struct {
	*halfClosedReplicationExchange
	release <-chan struct{}
}

type canceledLeaseRecvExchange struct{ *halfClosedReplicationExchange }

func (s *canceledLeaseRecvExchange) Recv() (*pb.ReplicationHello, error) {
	if !s.received {
		s.received = true
		return s.hello, nil
	}
	return nil, context.Canceled
}

func (s *blockedLeaseRecvExchange) Recv() (*pb.ReplicationHello, error) {
	if !s.received {
		s.received = true
		return s.hello, nil
	}
	<-s.release
	return nil, io.EOF
}

func (s *failingPushReplicationExchange) Send(frame *pb.ReplicationFrame) error {
	s.sends++
	if s.sends > 1 {
		return s.err
	}
	return s.halfClosedReplicationExchange.Send(frame)
}

func (s *halfClosedReplicationExchange) Context() context.Context { return s.ctx }

func (s *halfClosedReplicationExchange) Recv() (*pb.ReplicationHello, error) {
	if !s.received {
		s.received = true
		return s.hello, nil
	}
	if s.second != nil {
		frame := s.second
		s.second = nil
		return frame, nil
	}
	return nil, io.EOF
}

func (s *halfClosedReplicationExchange) Send(frame *pb.ReplicationFrame) error {
	s.sent <- frame
	return nil
}

func (s *replicationExchange) Context() context.Context { return context.Background() }

func (s *replicationExchange) Recv() (*pb.ReplicationHello, error) {
	if s.recvErr != nil {
		return nil, s.recvErr
	}
	return s.hello, nil
}

func (s *replicationExchange) Send(frame *pb.ReplicationFrame) error {
	if s.sendErr != nil {
		return s.sendErr
	}
	s.sent = append(s.sent, frame)
	return nil
}

func TestReplicationStreamRequiresAHelloAndPassesReceiveErrors(t *testing.T) {
	service := &blobService{}
	if err := service.Replication(&replicationExchange{recvErr: io.EOF}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("empty replication stream error = %v, want InvalidArgument", err)
	}
	gone := status.Error(codes.Unavailable, "connection lost")
	if err := service.Replication(&replicationExchange{recvErr: gone}); !errors.Is(err, gone) {
		t.Errorf("replication receive error = %v, want original error", err)
	}
	if err := service.Replication(&replicationExchange{hello: &pb.ReplicationHello{}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("malformed replication hello error = %v, want InvalidArgument", err)
	}
}

func TestReplicationStreamFramesTheResponseAndReportsSendFailure(t *testing.T) {
	service := &blobService{replication: replication.New(replication.Config{
		Store: storage.NewMemory(), StoreID: "test", Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})}
	hellos, err := replication.ReplicationHellos(&pb.ReplicateRequest{})
	if err != nil {
		t.Fatal(err)
	}
	stream := &replicationExchange{hello: hellos[0]}
	if err := service.Replication(stream); err != nil {
		t.Fatalf("replication stream: %v", err)
	}
	if len(stream.sent) != 1 {
		t.Fatalf("sent %d frames, want one", len(stream.sent))
	}
	if _, err := replication.ReadReplicationResponse(func() (*pb.ReplicationFrame, error) {
		return stream.sent[0], nil
	}); err != nil {
		t.Fatalf("read framed response: %v", err)
	}

	gone := status.Error(codes.Unavailable, "follower disconnected")
	stream = &replicationExchange{hello: hellos[0], sendErr: gone}
	if err := service.Replication(stream); !errors.Is(err, gone) {
		t.Errorf("initial replication response send error = %v, want original error", err)
	}
	if err := sendReplicationFrames(&pb.ReplicateResponse{}, func(*pb.ReplicationFrame) error { return gone }); !errors.Is(err, gone) {
		t.Errorf("replication send error = %v, want original error", err)
	}
	if err := sendReplicationFrames(nil, func(*pb.ReplicationFrame) error { return nil }); status.Code(err) != codes.ResourceExhausted {
		t.Errorf("unframeable replication response error = %v, want ResourceExhausted", err)
	}
}

func TestReplicationPushStaysOpenAfterClientHalfClosesSendSide(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := storage.NewMemory()
	manager := replication.New(replication.Config{
		Identity: newIdentity(t), Store: store, StoreID: "test", Replicas: 1,
		Address: func(string) string { return "127.0.0.1:7700" },
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	hellos, err := replication.ReplicationHellos(&pb.ReplicateRequest{KeepsLists: true})
	if err != nil {
		t.Fatal(err)
	}
	stream := &halfClosedReplicationExchange{
		ctx: ctx, hello: hellos[0], sent: make(chan *pb.ReplicationFrame, 4),
	}
	service := &blobService{store: store, replication: manager}
	done := make(chan error, 1)
	go func() { done <- service.Replication(stream) }()

	first, err := replication.ReadReplicationResponse(func() (*pb.ReplicationFrame, error) {
		select {
		case frame := <-stream.sent:
			return frame, nil
		case <-time.After(eventualWait):
			return nil, errors.New("timed out waiting for baseline assignment")
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.GetList().GetBlobs()) != 0 {
		t.Fatalf("initial assignment has %d blobs, want none", len(first.GetList().GetBlobs()))
	}

	blob, err := store.Put(ctx, strings.NewReader("newly pinned after the client half-closed"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Pin(ctx, "test", time.Time{}, blob); err != nil {
		t.Fatal(err)
	}
	manager.PinsChanged()
	push, err := replication.ReadReplicationResponse(func() (*pb.ReplicationFrame, error) {
		select {
		case frame := <-stream.sent:
			return frame, nil
		case <-time.After(eventualWait):
			return nil, errors.New("timed out waiting for pushed assignment after client EOF")
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(push.GetList().GetBlobs()); got != 1 || push.GetList().GetBlobs()[0].GetCid() != blob.String() {
		t.Fatalf("pushed assignment has blobs %v, want only %s", push.GetList().GetBlobs(), blob)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("replication handler after client half-close: %v", err)
		}
	case <-time.After(eventualWait):
		t.Fatal("replication handler did not stop after the client canceled")
	}
}

func TestReplicationReturnsAssignmentPushSendError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := storage.NewMemory()
	manager := replication.New(replication.Config{
		Identity: newIdentity(t), Store: store, StoreID: "test", Replicas: 1,
		Address: func(string) string { return "127.0.0.1:7700" },
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	hellos, err := replication.ReplicationHellos(&pb.ReplicateRequest{KeepsLists: true})
	if err != nil {
		t.Fatal(err)
	}
	want := status.Error(codes.Unavailable, "follower disconnected during push")
	stream := &failingPushReplicationExchange{
		halfClosedReplicationExchange: &halfClosedReplicationExchange{
			ctx: ctx, hello: hellos[0], sent: make(chan *pb.ReplicationFrame, 4),
		},
		err: want,
	}
	service := &blobService{store: store, replication: manager}
	done := make(chan error, 1)
	go func() { done <- service.Replication(stream) }()
	if _, err := replication.ReadReplicationResponse(func() (*pb.ReplicationFrame, error) {
		select {
		case frame := <-stream.sent:
			return frame, nil
		case <-time.After(eventualWait):
			return nil, errors.New("timed out waiting for baseline assignment")
		}
	}); err != nil {
		t.Fatal(err)
	}
	blob, err := store.Put(ctx, strings.NewReader("assignment that triggers a push"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Pin(ctx, "test", time.Time{}, blob); err != nil {
		t.Fatal(err)
	}
	manager.PinsChanged()
	select {
	case err := <-done:
		if !errors.Is(err, want) {
			t.Errorf("replication handler returned %v, want %v", err, want)
		}
	case <-time.After(eventualWait):
		t.Fatal("replication handler did not return after assignment push failed")
	}
}

func TestReplicationReturnsWhenStreamContextEndsBeforeLeaseRecv(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := storage.NewMemory()
	manager := replication.New(replication.Config{
		Identity: newIdentity(t), Store: store, StoreID: "test", Replicas: 1,
		Address: func(string) string { return "127.0.0.1:7700" },
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	hellos, err := replication.ReplicationHellos(&pb.ReplicateRequest{KeepsLists: true})
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	stream := &blockedLeaseRecvExchange{
		halfClosedReplicationExchange: &halfClosedReplicationExchange{
			ctx: ctx, hello: hellos[0], sent: make(chan *pb.ReplicationFrame, 2),
		},
		release: release,
	}
	done := make(chan error, 1)
	go func() { done <- (&blobService{store: store, replication: manager}).Replication(stream) }()
	if _, err := replication.ReadReplicationResponse(func() (*pb.ReplicationFrame, error) {
		select {
		case frame := <-stream.sent:
			return frame, nil
		case <-time.After(eventualWait):
			return nil, errors.New("timed out waiting for baseline assignment")
		}
	}); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("replication handler after stream cancellation: %v", err)
		}
	case <-time.After(eventualWait):
		t.Fatal("replication handler waited for a blocked lease receive after cancellation")
	}
	close(release)
}

func TestReplicationTreatsCanceledLeaseWatcherAsCleanEnd(t *testing.T) {
	store := storage.NewMemory()
	manager := replication.New(replication.Config{
		Identity: newIdentity(t), Store: store, StoreID: "test", Replicas: 1,
		Address: func(string) string { return "127.0.0.1:7700" },
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	hellos, err := replication.ReplicationHellos(&pb.ReplicateRequest{KeepsLists: true})
	if err != nil {
		t.Fatal(err)
	}
	stream := &canceledLeaseRecvExchange{&halfClosedReplicationExchange{
		ctx: context.Background(), hello: hellos[0], sent: make(chan *pb.ReplicationFrame, 2),
	}}
	if err := (&blobService{store: store, replication: manager}).Replication(stream); err != nil {
		t.Errorf("replication handler after canceled lease watcher: %v", err)
	}
}

func TestReplicationStreamRejectsUnexpectedClientFrameAfterHello(t *testing.T) {
	store := storage.NewMemory()
	manager := replication.New(replication.Config{
		Identity: newIdentity(t), Store: store, StoreID: "test", Replicas: 1,
		Address: func(string) string { return "127.0.0.1:7700" },
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	hellos, err := replication.ReplicationHellos(&pb.ReplicateRequest{KeepsLists: true})
	if err != nil {
		t.Fatal(err)
	}
	stream := &halfClosedReplicationExchange{
		ctx: context.Background(), hello: hellos[0], second: &pb.ReplicationHello{},
		sent: make(chan *pb.ReplicationFrame, 4),
	}
	err = (&blobService{store: store, replication: manager}).Replication(stream)
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("replication stream with a non-lease frame after hello: %v, want InvalidArgument", err)
	}
}

func TestWatchReplicationMessagesStopsAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	manager := replication.New(replication.Config{
		Identity: newIdentity(t), Store: storage.NewMemory(), StoreID: "test", Replicas: 1,
		Address: func(string) string { return "127.0.0.1:7700" },
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	err := watchReplicationMessages(ctx, func() (*pb.ReplicationHello, error) {
		return &pb.ReplicationHello{Lease: true}, nil
	}, "follower", 1, manager, func(*pb.ReplicateResponse) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("watchReplicationMessages returned %v, want context canceled", err)
	}
}

func TestWatchReplicationMessagesRejectsMalformedLeaseAndPassesOnSendErrors(t *testing.T) {
	manager := replication.New(replication.Config{
		Identity: newIdentity(t), Store: storage.NewMemory(), StoreID: "test", Replicas: 1,
		Address: func(string) string { return "127.0.0.1:7700" },
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	malformed := watchReplicationMessages(context.Background(), func() (*pb.ReplicationHello, error) {
		return &pb.ReplicationHello{Lease: true, Index: 1}, nil
	}, "follower", 1, manager, func(*pb.ReplicateResponse) error { return nil })
	if status.Code(malformed) != codes.InvalidArgument {
		t.Errorf("malformed lease error = %v, want InvalidArgument", malformed)
	}

	want := status.Error(codes.Unavailable, "follower disconnected")
	ackFailure := watchReplicationMessages(context.Background(), func() (*pb.ReplicationHello, error) {
		return &pb.ReplicationHello{Lease: true}, nil
	}, "follower", 1, manager, func(*pb.ReplicateResponse) error { return want })
	if !errors.Is(ackFailure, want) {
		t.Errorf("capability-ack send error = %v, want original error", ackFailure)
	}

	frames, err := replication.ReplicationHellos(&pb.ReplicateRequest{KeepsLists: true})
	if err != nil {
		t.Fatal(err)
	}
	var received int
	reportFailure := watchReplicationMessages(context.Background(), func() (*pb.ReplicationHello, error) {
		if received < len(frames) {
			frame := frames[received]
			received++
			return frame, nil
		}
		return nil, io.EOF
	}, "follower", 1, manager, func(*pb.ReplicateResponse) error { return want })
	if !errors.Is(reportFailure, want) {
		t.Errorf("state-report response send error = %v, want original error", reportFailure)
	}
}

func TestWatchReplicationMessagesAnswersLiveStateReports(t *testing.T) {
	manager := replication.New(replication.Config{
		Identity: newIdentity(t), Store: storage.NewMemory(), StoreID: "test", Replicas: 1,
		Address: func(string) string { return "127.0.0.1:7700" },
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	frames, err := replication.ReplicationHellos(&pb.ReplicateRequest{KeepsLists: true, AnswersChallenges: true})
	if err != nil {
		t.Fatal(err)
	}
	var received int
	var sent []*pb.ReplicateResponse
	err = watchReplicationMessages(context.Background(), func() (*pb.ReplicationHello, error) {
		if received < len(frames) {
			frame := frames[received]
			received++
			return frame, nil
		}
		return nil, context.Canceled
	}, "follower", 1, manager, func(response *pb.ReplicateResponse) error {
		sent = append(sent, response)
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("watchReplicationMessages returned %v, want context canceled", err)
	}
	if len(sent) != 1 || sent[0].GetList() == nil {
		t.Fatalf("live state report got responses %v, want one signed assignment", sent)
	}
}

func TestFollowerRecoversEventGapOverReplicationStreamWithoutUnaryPolling(t *testing.T) {
	previousInterval := replication.Interval
	replication.Interval = 20 * time.Millisecond
	t.Cleanup(func() { replication.Interval = previousInterval })
	ident := newIdentity(t)
	coordinatorStore := storage.NewMemory()
	first, err := coordinatorStore.Put(context.Background(), strings.NewReader("first assignment"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := coordinatorStore.Put(context.Background(), strings.NewReader("assignment changed while follower was away"))
	if err != nil {
		t.Fatal(err)
	}
	manager := replication.New(replication.Config{
		Identity: ident, Store: coordinatorStore, StoreID: "test", Replicas: 1,
		Address: func(string) string { return "127.0.0.1:7701" },
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	var unaryReplicates atomic.Int64
	server := grpc.NewServer(grpc.UnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if info.FullMethod == "/sisyphus.v1.BlobService/Replicate" {
			unaryReplicates.Add(1)
		}
		return handler(ctx, req)
	}))
	pb.RegisterBlobServiceServer(server, &blobService{store: coordinatorStore, replication: manager})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})
	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	followerStore := storage.NewMemory()
	follower := &replication.Follower{
		Store: followerStore, ListFile: filepath.Join(t.TempDir(), "kept.list"),
		Coordinator: pb.NewBlobServiceClient(conn), CoordinatorID: ident.ID(),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ctx, cancelManager := context.WithCancel(context.Background())
	managerDone := make(chan struct{})
	go func() { manager.Run(ctx); close(managerDone) }()
	startFollower := func() (context.CancelFunc, <-chan struct{}) {
		followerCtx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { follower.Run(followerCtx); close(done) }()
		return cancel, done
	}
	waitFor := func(description string, condition func() bool) {
		t.Helper()
		deadline := time.Now().Add(eventualWait)
		for !condition() && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if !condition() {
			t.Fatalf("timed out waiting for %s", description)
		}
	}
	stopFollower, followerDone := startFollower()
	if err := coordinatorStore.Pin(context.Background(), "user", time.Time{}, first); err != nil {
		t.Fatal(err)
	}
	manager.PinsChanged()
	waitFor("the first pushed assignment to be fetched", func() bool {
		has, err := followerStore.Has(context.Background(), first)
		return err == nil && has
	})
	waitFor("the stream challenge to be answered", func() bool {
		status := manager.Status(first.String())
		return len(status.GetBlobs()) == 1 && len(status.GetBlobs()[0].GetShown()) > 0
	})
	stopFollower()
	waitStopped(t, followerDone, "follower did not stop its first stream")
	waitFor("the coordinator to observe the disconnected follower", func() bool {
		return len(manager.Status("").GetFollowers()) == 0
	})
	oldListBytes, err := os.ReadFile(follower.ListFile)
	if err != nil {
		t.Fatal(err)
	}
	var oldList pb.KeepList
	if err := proto.Unmarshal(oldListBytes, &oldList); err != nil {
		t.Fatal(err)
	}
	if err := coordinatorStore.Unpin("user", first); err != nil {
		t.Fatal(err)
	}
	if err := coordinatorStore.Pin(context.Background(), "user", time.Time{}, second); err != nil {
		t.Fatal(err)
	}
	manager.PinsChanged()
	stopFollower, followerDone = startFollower()
	waitFor("the reconnect baseline to recover the missed assignment", func() bool {
		hasSecond, secondErr := followerStore.Has(context.Background(), second)
		hasFirst, firstErr := followerStore.Has(context.Background(), first)
		return secondErr == nil && firstErr == nil && hasSecond && !hasFirst
	})
	newListBytes, err := os.ReadFile(follower.ListFile)
	if err != nil {
		t.Fatal(err)
	}
	var newList pb.KeepList
	if err := proto.Unmarshal(newListBytes, &newList); err != nil {
		t.Fatal(err)
	}
	if newList.GetSequence() <= oldList.GetSequence() || len(newList.GetBlobs()) != 1 || newList.GetBlobs()[0].GetCid() != second.String() {
		t.Errorf("recovered signed list = %v, want a newer list containing only %s", &newList, second)
	}
	if got := unaryReplicates.Load(); got != 0 {
		t.Errorf("stream-capable coordinator used unary Replicate %d times, want none", got)
	}
	stopFollower()
	waitStopped(t, followerDone, "follower did not stop its restarted stream")
	cancelManager()
	waitStopped(t, managerDone, "manager challenge scheduler did not stop")
}

// eventualWait bounds every wait for something that must happen. Passing tests
// never wait it out, so it is long: under the race detector and a full
// parallel suite a stop can take more than a second (#462, #466). Windows in
// which something must not happen stay short.
const eventualWait = 30 * time.Second

// waitStopped waits for done as long as the test's other waits do. If it never
// comes, every goroutine's stack is shown, so a real hang says where it is
// stuck.
func waitStopped(t *testing.T, done <-chan struct{}, failure string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(eventualWait):
		stacks := make([]byte, 1<<20)
		t.Fatalf("%s; goroutines:\n%s", failure, stacks[:runtime.Stack(stacks, true)])
	}
}

func storeWithBlob(t *testing.T) (*storage.Store, string) {
	t.Helper()
	store := storage.NewMemory()
	c, err := store.Put(context.Background(), strings.NewReader("a stored blob"))
	if err != nil {
		t.Fatal(err)
	}
	return store, c.String()
}

func TestPutReportsAStoreFailureAsInternal(t *testing.T) {
	service := &blobService{store: brokenStore{Store: storage.NewMemory(), failPut: true}}
	err := service.Put(&upload{chunks: [][]byte{[]byte("data")}})
	if status.Code(err) != codes.Internal || !strings.Contains(err.Error(), "disk on fire") {
		t.Errorf("error %v, want Internal naming the cause", err)
	}
}

// A client that goes away mid-upload is its own doing, not a server fault,
// so its status must reach the caller unchanged.
func TestPutPassesOnTheClientsOwnFailure(t *testing.T) {
	service := &blobService{store: storage.NewMemory()}
	gone := status.Error(codes.Canceled, "client went away")
	err := service.Put(&upload{chunks: [][]byte{[]byte("data")}, recvErr: gone})
	if status.Code(err) != codes.Canceled {
		t.Errorf("error %v, want the client's Canceled status", err)
	}
}

func TestGetStopsWhenTheClientCannotBeSentTo(t *testing.T) {
	store, id := storeWithBlob(t)
	service := &blobService{store: store}
	gone := status.Error(codes.Unavailable, "client went away")
	if err := service.Get(&pb.GetBlobRequest{Cid: id}, &download{sendErr: gone}); !errors.Is(err, gone) {
		t.Errorf("error %v, want the send failure", err)
	}
}

func TestGetReportsAReadFailureAsInternal(t *testing.T) {
	store, id := storeWithBlob(t)
	service := &blobService{store: brokenStore{Store: store, failRead: true}}
	err := service.Get(&pb.GetBlobRequest{Cid: id}, &download{})
	if status.Code(err) != codes.Internal || !strings.Contains(err.Error(), "disk on fire") {
		t.Errorf("error %v, want Internal naming the cause", err)
	}
}

func TestOpenFailureOtherThanNotFoundIsInternal(t *testing.T) {
	store, id := storeWithBlob(t)
	service := &blobService{store: brokenStore{Store: store, failOpen: true}}
	_, err := service.Stat(context.Background(), &pb.StatBlobRequest{Cid: id})
	if status.Code(err) != codes.Internal {
		t.Errorf("Stat: error %v, want Internal", err)
	}
	if err := service.Get(&pb.GetBlobRequest{Cid: id}, &download{}); status.Code(err) != codes.Internal {
		t.Errorf("Get: error %v, want Internal", err)
	}
}

func TestPinAndUnpinRejectWhatTheyCannotActOn(t *testing.T) {
	store, id := storeWithBlob(t)
	ctx := context.Background()
	absent, err := storage.CID(ctx, strings.NewReader("never stored"))
	if err != nil {
		t.Fatal(err)
	}
	working := &blobService{store: store}
	failing := &blobService{store: brokenStore{Store: store, failPin: true}}

	if _, err := working.Pin(ctx, &pb.PinBlobRequest{Cid: "nope"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("Pin of a malformed CID: %v", err)
	}
	if _, err := working.Pin(ctx, &pb.PinBlobRequest{Cid: absent.String()}); status.Code(err) != codes.NotFound {
		t.Errorf("Pin of a blob the store lacks: %v", err)
	}
	if _, err := failing.Pin(ctx, &pb.PinBlobRequest{Cid: id}); status.Code(err) != codes.Internal {
		t.Errorf("Pin when the store fails: %v", err)
	}
	if _, err := working.Unpin(ctx, &pb.UnpinBlobRequest{Cid: "nope"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("Unpin of a malformed CID: %v", err)
	}
	if _, err := failing.Unpin(ctx, &pb.UnpinBlobRequest{Cid: id}); status.Code(err) != codes.Internal {
		t.Errorf("Unpin when the store fails: %v", err)
	}
	if _, err := (&blobService{store: brokenStore{Store: store, failGC: true}}).CollectGarbage(ctx, &pb.CollectGarbageRequest{}); status.Code(err) != codes.Internal {
		t.Errorf("CollectGarbage when the store fails: %v", err)
	}
}

func TestPinsAreHeldForTheUserWithTheRequestedLifetime(t *testing.T) {
	store, id := storeWithBlob(t)
	ctx := context.Background()
	service := &blobService{store: store}

	before := time.Now()
	if _, err := service.Pin(ctx, &pb.PinBlobRequest{Cid: id, TtlSeconds: 3600}); err != nil {
		t.Fatal(err)
	}
	userPin := func() *pb.Pin {
		listed, err := service.ListPins(ctx, &pb.ListPinsRequest{})
		if err != nil {
			t.Fatal(err)
		}
		for _, pin := range listed.GetPins() {
			if pin.GetOwner() == "user" {
				return pin
			}
		}
		return nil
	}
	pin := userPin()
	if pin == nil || pin.GetCid() != id {
		t.Fatalf("no pin held for the user on %s: %v", id, pin)
	}
	if expires := pin.GetExpiresAt().AsTime(); expires.Before(before.Add(time.Hour)) || expires.After(time.Now().Add(time.Hour)) {
		t.Errorf("pin expires %v, want an hour from now", expires)
	}

	// Without a lifetime the pin holds until released.
	if _, err := service.Pin(ctx, &pb.PinBlobRequest{Cid: id}); err != nil {
		t.Fatal(err)
	}
	if pin := userPin(); pin.GetExpiresAt() != nil {
		t.Errorf("a pin with no lifetime expires at %v", pin.GetExpiresAt().AsTime())
	}

	if _, err := service.Unpin(ctx, &pb.UnpinBlobRequest{Cid: id}); err != nil {
		t.Fatal(err)
	}
	if pin := userPin(); pin != nil {
		t.Errorf("still pinned for the user after unpinning: %v", pin)
	}
}

func TestCollectGarbageReportsWhatItRemoved(t *testing.T) {
	store, id := storeWithBlob(t)
	ctx := context.Background()
	// Replace the blob's pins with one that has already lapsed.
	c, err := cid.Decode(id)
	if err != nil {
		t.Fatal(err)
	}
	for _, pin := range store.Pins() {
		store.Unpin(pin.Owner, pin.CID)
	}
	if err := store.Pin(ctx, "user", time.Now().Add(-time.Minute), c); err != nil {
		t.Fatal(err)
	}

	done, err := (&blobService{store: store}).CollectGarbage(ctx, &pb.CollectGarbageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if done.GetExpiredPins() != 1 || done.GetBlocksRemoved() != 1 || done.GetBytesFreed() != uint64(len("a stored blob")) {
		t.Errorf("reported %v", done)
	}
}

func TestUploadsAreRefusedOnceTheStoreIsFull(t *testing.T) {
	data := []byte("twenty bytes of data")
	tests := []struct {
		name  string
		store brokenStore
		want  codes.Code
	}{
		{"room to spare", brokenStore{used: 50}, codes.OK},
		{"exactly enough room", brokenStore{used: 80}, codes.OK},
		{"one byte short", brokenStore{used: 81}, codes.ResourceExhausted},
		{"already full", brokenStore{used: 100}, codes.ResourceExhausted},
		{"over the limit already", brokenStore{used: 500}, codes.ResourceExhausted},
		{"its size cannot be measured", brokenStore{failSize: true}, codes.Internal},
	}
	for _, tt := range tests {
		tt.store.Store = storage.NewMemory()
		service := &blobService{store: tt.store, quota: 100}
		err := service.Put(&upload{chunks: [][]byte{data[:12], data[12:]}})
		if status.Code(err) != tt.want {
			t.Errorf("%s: %v, want %v", tt.name, err, tt.want)
		}
	}

	// With no limit set, the store's size is not even consulted.
	unlimited := &blobService{store: brokenStore{Store: storage.NewMemory(), failSize: true}}
	if err := unlimited.Put(&upload{chunks: [][]byte{data}}); err != nil {
		t.Errorf("upload with no limit: %v", err)
	}
}

// asClient has every call in a test come from the client named, which
// the service takes for one of its clients.
func asClient(t *testing.T, s *blobService, client string) {
	t.Helper()
	before := caller
	caller = func(context.Context) string { return client }
	t.Cleanup(func() { caller = before })
	s.clients = func(id string) bool { return id == client }
}

// A client's share that cannot be measured is no share it can be held
// to, so what it asks is refused as the node's own failure, and a pin
// that was taken is given back.
func TestAShareThatCannotBeMeasuredRefusesItsClient(t *testing.T) {
	ctx := context.Background()
	store, held := storeWithBlob(t)
	service := &blobService{store: store, owner: "node", perClient: 100, stored: func(context.Context) (map[string]uint64, error) { return nil, errDisk }}
	asClient(t, service, "alice")

	if err := service.Put(&upload{chunks: [][]byte{[]byte("hi")}}); status.Code(err) != codes.Internal || !strings.Contains(err.Error(), "measure what this client keeps") {
		t.Errorf("an upload: %v", err)
	}
	if _, err := service.Pin(ctx, &pb.PinBlobRequest{Cid: held}); status.Code(err) != codes.Internal || !strings.Contains(err.Error(), "measure what this client keeps") {
		t.Errorf("a pin: %v", err)
	}
	for _, pin := range store.Pins() {
		if pin.Owner == clientOwner+"alice" {
			t.Errorf("a pin that was refused was left in place: %v", pin)
		}
	}
}

// What a client uploads counts towards it from the start; a store that
// will not count it fails the upload rather than let it go uncounted.
func TestAnUploadThatCannotBeCountedFails(t *testing.T) {
	service := &blobService{store: brokenStore{Store: storage.NewMemory(), failPin: true}, owner: "node"}
	asClient(t, service, "alice")
	if err := service.Put(&upload{chunks: [][]byte{[]byte("hi")}}); status.Code(err) != codes.Internal || !strings.Contains(err.Error(), "for its client") {
		t.Errorf("an upload that could not be counted: %v", err)
	}
}

// A client's pin of a blob the store does not have is refused as not
// found, whether or not it is held to a share.
func TestAClientsPinOfAMissingBlobIsNotFound(t *testing.T) {
	store := storage.NewMemory()
	none := func(context.Context) (map[string]uint64, error) { return nil, nil }
	service := &blobService{store: store, owner: "node", perClient: 100, stored: none}
	asClient(t, service, "alice")
	_, missing := storeWithBlob(t) // in another store
	if _, err := service.Pin(context.Background(), &pb.PinBlobRequest{Cid: missing}); status.Code(err) != codes.NotFound {
		t.Errorf("a pin of a missing blob: %v", err)
	}
}

// What a worker uploads for an attempt is let go of again if the attempt
// ended while it was on its way.
func TestWhatAWorkerUploadsIsKeptOnlyWhileItsAttemptLasts(t *testing.T) {
	store, id := storeWithBlob(t)
	c, err := cid.Decode(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Pin(context.Background(), "job:j#0@1.", time.Time{}, c); err != nil {
		t.Fatal(err)
	}
	ended := &blobService{store: store, uploading: func(string, string, uint32) (string, bool) { return "", false }}
	if err := ended.stillHeld(context.Background(), "job:j#0@1.", c, &pb.PutBlobRequest{TaskId: "j/0", Attempt: 1}); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "ended while") {
		t.Errorf("an attempt that ended during the upload: %v", err)
	}
	for _, pin := range store.Pins() {
		if pin.Owner == "job:j#0@1." {
			t.Errorf("left held for an attempt that had ended")
		}
	}
}

// An upload of nothing at all is an empty blob; one whose first message
// cannot be read is not stored.
func TestAnUploadOfNoMessagesIsAnEmptyBlob(t *testing.T) {
	service := &blobService{store: storage.NewMemory(), owner: "node"}
	if err := service.Put(&upload{}); err != nil {
		t.Errorf("an upload with no messages: %v", err)
	}
	if err := service.Put(&upload{recvErr: errDisk}); !errors.Is(err, errDisk) {
		t.Errorf("an upload whose first message fails: %v", err)
	}
}

// A worker's upload for an attempt that ends while it is on its way is
// refused, and nothing is kept for the attempt.
func TestAnUploadForAnAttemptThatEndsMeanwhileIsRefused(t *testing.T) {
	before := caller
	caller = func(context.Context) string { return "worker" }
	t.Cleanup(func() { caller = before })
	store := storage.NewMemory()
	asked := 0
	service := &blobService{store: store, owner: "node", workers: func(string) bool { return true },
		uploading: func(node, task string, attempt uint32) (string, bool) {
			asked++
			return "job:j#0@1.", asked == 1
		}}
	err := service.Put(&upload{first: &pb.PutBlobRequest{TaskId: "j/0", Attempt: 1, Data: []byte("half")}})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "ended while") {
		t.Errorf("an upload for an attempt that ended meanwhile: %v", err)
	}
	for _, pin := range store.Pins() {
		if pin.Owner == "job:j#0@1." {
			t.Error("kept for the attempt")
		}
	}
}
