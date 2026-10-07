package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/excho0/Sisyphus/packages/protocol/sisyphus/v1"
)

// roster is a coordinator's answer to "is this node a member?", counting how
// often it is asked.
type roster struct {
	pb.PoolServiceClient
	members map[string]bool
	err     error
	asked   int
}

func (r *roster) IsMember(_ context.Context, req *pb.IsMemberRequest, _ ...grpc.CallOption) (*pb.IsMemberResponse, error) {
	r.asked++
	if r.err != nil {
		return nil, r.err
	}
	return &pb.IsMemberResponse{Member: r.members[req.GetNodeId()]}, nil
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
