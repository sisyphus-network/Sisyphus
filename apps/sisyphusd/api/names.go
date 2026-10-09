package api

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/access"
	"github.com/sisyphus-network/Sisyphus/packages/names"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
)

var (
	// ErrNoRecord reports a name this node holds no record for.
	ErrNoRecord = errors.New("this node holds no record for that name")
	// ErrOlderRecord reports a record that is no newer than the one held.
	ErrOlderRecord = errors.New("this node already holds a record for that name that is as new or newer")
)

// NameStore is where the records of names are kept.
type NameStore interface {
	NameRecord(name string) ([]byte, error)
	SetNameRecord(name string, record []byte, now time.Time) error
}

// Names is the names a node answers for: the latest record of each, as its
// node signed it. A coordinator answers for itself and for the nodes it has
// admitted, who hand it their records.
type Names struct {
	store NameStore
	// answers says whether this node answers for the node with an ID.
	answers func(id string) bool
	// carry, if set, is given every record that is taken in.
	carry func(*names.Record)
	// mu makes comparing with the record held and replacing it one step.
	mu sync.Mutex
}

// NewNames returns the names kept in store. answers says which nodes' names
// are answered for; a record held for any other is not given out.
func NewNames(store NameStore, answers func(id string) bool) *Names {
	return &Names{store: store, answers: answers}
}

// OnPublish has every record that is taken in from now on passed to carry
// as well, which is how it reaches the pool's private IPFS network. It is
// set before the names are in use.
func (n *Names) OnPublish(carry func(*names.Record)) {
	n.carry = carry
}

// Publish keeps a record, which has been checked, as the latest for its
// name. One that would not replace the record held is refused with
// ErrOlderRecord, unless it is that very record, handed over again.
func (n *Names) Publish(record *names.Record, now time.Time) error {
	if err := n.replace(record, now); err != nil {
		return err
	}
	// Passing it on can take a while, and the next record need not wait.
	if n.carry != nil {
		n.carry(record)
	}
	return nil
}

// replace puts a record in the place of the one held for its name, if it
// should have it.
func (n *Names) replace(record *names.Record, now time.Time) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	held, err := n.store.NameRecord(record.Name)
	if err != nil {
		return err
	}
	// A record held that can no longer be read is as good as none.
	if old, err := names.Parse(record.Name, held); err == nil && !record.Replaces(old) && !bytes.Equal(held, record.Bytes()) {
		return ErrOlderRecord
	}
	return n.store.SetNameRecord(record.Name, record.Bytes(), now)
}

// Held returns the latest record held for the node with an ID, or
// ErrNoRecord. The record may have expired: it is what its node last
// published, and for whoever asks to judge.
func (n *Names) Held(id string) (*names.Record, error) {
	if !n.answers(id) {
		return nil, ErrNoRecord
	}
	held, err := n.store.NameRecord(id)
	if err != nil {
		return nil, err
	}
	// None, or one that can no longer be read, which is as good as none.
	record, err := names.Parse(id, held)
	if err != nil {
		return nil, ErrNoRecord
	}
	return record, nil
}

type nameService struct {
	pb.UnimplementedNameServiceServer
	names *Names
}

func (s *nameService) Publish(ctx context.Context, req *pb.PublishNameRequest) (*pb.PublishNameResponse, error) {
	// The name is the caller's, whatever the record says, so a record
	// another node signed does not pass.
	caller, now := access.Caller(ctx), time.Now()
	record, err := names.Check(caller, req.GetRecord(), now)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "a node publishes under its own name only, and this is not a record node %s could publish: %v", caller, err)
	}
	err = s.names.Publish(record, now)
	if errors.Is(err, ErrOlderRecord) {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "keep the record: %v", err)
	}
	return &pb.PublishNameResponse{}, nil
}

func (s *nameService) Resolve(_ context.Context, req *pb.ResolveNameRequest) (*pb.ResolveNameResponse, error) {
	id, err := names.ID(req.GetName())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	record, err := s.names.Held(id)
	if errors.Is(err, ErrNoRecord) {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read the record: %v", err)
	}
	return &pb.ResolveNameResponse{Record: record.Bytes()}, nil
}
