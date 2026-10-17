package worker

import (
	"context"
	"errors"
	"io"
	"slices"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
)

// roster is a coordinator's answer to "is this node a member?", counting how
// often it is asked.
type roster struct {
	pb.PoolServiceClient
	members map[string]bool
	err     error
	asked   int
	relays  grpc.ServerStreamingClient[pb.RelaysResponse]
}

func (r *roster) IsMember(_ context.Context, req *pb.IsMemberRequest, _ ...grpc.CallOption) (*pb.IsMemberResponse, error) {
	r.asked++
	if r.err != nil {
		return nil, r.err
	}
	return &pb.IsMemberResponse{Member: r.members[req.GetNodeId()]}, nil
}

func (r *roster) WatchRelays(context.Context, *pb.RelaysRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[pb.RelaysResponse], error) {
	return r.relays, nil
}

type relayUpdates struct {
	grpc.ServerStreamingClient[pb.RelaysResponse]
	updates chan *pb.RelaysResponse
}

type relayWatchPool struct {
	pb.PoolServiceClient
	streams chan grpc.ServerStreamingClient[pb.RelaysResponse]
	calls   chan struct{}
}

func (p *relayWatchPool) WatchRelays(context.Context, *pb.RelaysRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[pb.RelaysResponse], error) {
	p.calls <- struct{}{}
	return <-p.streams, nil
}

func (s *relayUpdates) Recv() (*pb.RelaysResponse, error) {
	update, ok := <-s.updates
	if !ok {
		return nil, io.EOF
	}
	return update, nil
}

func TestMembersPublishesChangedRelaySnapshotsOnly(t *testing.T) {
	stream := &relayUpdates{updates: make(chan *pb.RelaysResponse, 3)}
	members := &Members{pool: &roster{relays: stream}, relayChanges: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- members.WatchRelays(ctx) }()

	addresses := []string{"/ip4/192.0.2.1/tcp/7700/p2p/relay"}
	stream.updates <- &pb.RelaysResponse{Addresses: addresses}
	select {
	case <-members.RelayChanges():
	case <-time.After(time.Second):
		t.Fatal("relay snapshot did not notify the consumer")
	}
	if got := members.Relays(ctx); !slices.Equal(got, addresses) {
		t.Fatalf("Relays() = %v, want %v", got, addresses)
	}

	stream.updates <- &pb.RelaysResponse{Addresses: addresses}
	select {
	case <-members.RelayChanges():
		t.Fatal("an unchanged relay snapshot notified the consumer")
	case <-time.After(20 * time.Millisecond):
	}

	stream.updates <- &pb.RelaysResponse{}
	select {
	case <-members.RelayChanges():
	case <-time.After(time.Second):
		t.Fatal("relay removal did not notify the consumer")
	}
	if got := members.Relays(ctx); len(got) != 0 {
		t.Fatalf("Relays() after removal = %v, want none", got)
	}

	cancel()
	close(stream.updates)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("relay watcher did not stop")
	}
}

func TestRunRelayWatchResetsBackoffAfterReceivingSnapshot(t *testing.T) {
	stream := func(snapshot bool) *relayUpdates {
		updates := make(chan *pb.RelaysResponse, 1)
		if snapshot {
			updates <- &pb.RelaysResponse{Addresses: []string{"relay"}}
		}
		close(updates)
		return &relayUpdates{updates: updates}
	}
	lastStream := &relayUpdates{updates: make(chan *pb.RelaysResponse)}
	pool := &relayWatchPool{
		streams: make(chan grpc.ServerStreamingClient[pb.RelaysResponse], 3),
		calls:   make(chan struct{}, 3),
	}
	pool.streams <- stream(true)
	pool.streams <- stream(false)
	pool.streams <- lastStream
	members := &Members{pool: pool, relayChanges: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	started := time.Now()
	go members.runRelayWatch(ctx, nil, 150*time.Millisecond, 600*time.Millisecond)
	for range 3 {
		select {
		case <-pool.calls:
		case <-time.After(400 * time.Millisecond):
			t.Fatal("relay stream did not reconnect after a healthy snapshot reset the backoff")
		}
	}
	close(lastStream.updates)
	if elapsed := time.Since(started); elapsed >= 400*time.Millisecond {
		t.Fatalf("third stream started after %v; expected reset backoff", elapsed)
	}
}

func TestMembersRemembersAYesForAWhileAndNeverANo(t *testing.T) {
	ctx := context.Background()
	coordinator := &roster{members: map[string]bool{"member": true}}
	m := &Members{pool: coordinator, remember: time.Hour, known: make(map[string]time.Time)}

	for range 3 {
		if ok, err := m.IsMember(ctx, "member"); err != nil || !ok {
			t.Fatalf("IsMember(member) = %v, %v", ok, err)
		}
	}
	if coordinator.asked != 1 {
		t.Errorf("the coordinator was asked %d times about a known member, want once", coordinator.asked)
	}

	// A no is not remembered, so a node admitted a moment later is served.
	if ok, _ := m.IsMember(ctx, "newcomer"); ok {
		t.Fatal("a node the coordinator does not know was taken for a member")
	}
	coordinator.members["newcomer"] = true
	if ok, _ := m.IsMember(ctx, "newcomer"); !ok {
		t.Error("a node admitted after first being refused is still refused")
	}

	// Once what is remembered has aged, the coordinator is asked again, and a
	// member since removed is found out.
	m.remember = 0
	delete(coordinator.members, "member")
	if ok, _ := m.IsMember(ctx, "member"); ok {
		t.Error("a removed node is still taken for a member after the memory of it expired")
	}
	m.remember = time.Hour
	if ok, _ := m.IsMember(ctx, "member"); ok {
		t.Error("a removed node was remembered as a member")
	}
}

func TestMembersPassesOnAFailureToAsk(t *testing.T) {
	down := errors.New("coordinator is unreachable")
	m := &Members{pool: &roster{err: down}, remember: time.Hour, known: make(map[string]time.Time)}
	if ok, err := m.IsMember(context.Background(), "anyone"); ok || !errors.Is(err, down) {
		t.Errorf("IsMember = %v, %v; want the failure", ok, err)
	}
}

func TestNewMembersAsksOverTheGivenConnection(t *testing.T) {
	// A connection to nowhere: the question fails rather than being skipped.
	conn, err := grpc.NewClient("127.0.0.1:1", grpc.WithTransportCredentials(workerOnlyCreds()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if ok, err := NewMembers(conn).IsMember(ctx, "anyone"); ok || err == nil {
		t.Errorf("IsMember over a dead connection = %v, %v", ok, err)
	}
}

func workerOnlyCreds() credentials.TransportCredentials {
	return insecure.NewCredentials()
}

// Relay lists that change while the consumer is busy wake it once, and it
// then reads the latest.
func TestRelayChangesWhileTheConsumerIsBusyComeToOne(t *testing.T) {
	stream := &relayUpdates{updates: make(chan *pb.RelaysResponse, 2)}
	members := &Members{pool: &roster{relays: stream}, relayChanges: make(chan struct{}, 1)}
	stream.updates <- &pb.RelaysResponse{Addresses: []string{"first"}}
	stream.updates <- &pb.RelaysResponse{Addresses: []string{"second"}}
	close(stream.updates)
	if err := members.WatchRelays(context.Background()); err == nil {
		t.Fatal("the watch outlived its stream")
	}
	if len(members.RelayChanges()) != 1 {
		t.Errorf("%d wakings waiting, want one", len(members.RelayChanges()))
	}
	if got := members.Relays(context.Background()); !slices.Equal(got, []string{"second"}) {
		t.Errorf("Relays() = %v, want the latest", got)
	}
}
