package api

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
)

var errNoJoinedPool = status.Error(codes.FailedPrecondition, "this node coordinates its own pool and has joined no other")

// GetJoinedPool says which pool this node works for, and how it stands with
// it.
func (s *localService) GetJoinedPool(context.Context, *nodepb.GetJoinedPoolRequest) (*nodepb.JoinedPool, error) {
	if s.cfg.JoinedPool == nil {
		return nil, errNoJoinedPool
	}
	return s.cfg.JoinedPool(), nil
}

// WatchJoinedPool sends the joined pool as it stands, and again, whole,
// each time it changes. Word of a change that leaves it as it was sends
// nothing.
func (s *localService) WatchJoinedPool(_ *nodepb.GetJoinedPoolRequest, stream grpc.ServerStreamingServer[nodepb.JoinedPool]) error {
	if s.cfg.JoinedPool == nil {
		return errNoJoinedPool
	}
	var changes <-chan struct{}
	if s.cfg.JoinedPoolChanges != nil {
		var unsubscribe func()
		changes, unsubscribe = s.cfg.JoinedPoolChanges.subscribe()
		defer unsubscribe()
	}
	var last *nodepb.JoinedPool
	for {
		if now := s.cfg.JoinedPool(); !proto.Equal(now, last) {
			if err := stream.Send(now); err != nil {
				return err
			}
			last = now
		}
		select {
		case <-changes:
		case <-stream.Context().Done():
			return stream.Context().Err()
		}
	}
}
