package main

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/worker"
	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
)

// A node that works for another's pool says, on its local API, which pool
// it has joined and how it stands with it: connected, with the roles its
// coordinator gives it, told again when those change, how many workers the
// pool has once it is a client, and, once the coordinator has gone, the pool
// it last knew and that it is no longer in it, with no count (#474).
func TestAWorkerNodeSaysWhichPoolItHasJoinedAndHowItStands(t *testing.T) {
	addr, stopCoordinator := startNode(t, "--name", "rig")
	workerDir, apiAddr := t.TempDir(), freeAddr(t)
	startDaemon(t, "--role", "worker", "--coordinator", addr, "--join", invite(t, addr, "worker"), "--data-dir", workerDir, "--name", "hand", "--slots", "1", "--api-listen", apiAddr)
	client := desktop(t, apiAddr)
	poolAsSeenBy(t, client)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	stream, err := client.WatchJoinedPool(ctx, &nodepb.GetJoinedPoolRequest{})
	if err != nil {
		t.Fatal(err)
	}
	// until reads the stream until the pool stands as wanted, and returns it.
	until := func(what string, want func(*nodepb.JoinedPool) bool) *nodepb.JoinedPool {
		t.Helper()
		for {
			pool, err := stream.Recv()
			if err != nil {
				t.Fatalf("waiting for %s: %v", what, err)
			}
			if want(pool) {
				return pool
			}
		}
	}
	worker := []nodepb.PoolRole{nodepb.PoolRole_POOL_ROLE_WORKER}
	both := []nodepb.PoolRole{nodepb.PoolRole_POOL_ROLE_WORKER, nodepb.PoolRole_POOL_ROLE_CLIENT}

	joined := until("the node to be in the pool", func(p *nodepb.JoinedPool) bool {
		return p.GetState() == nodepb.JoinedPoolState_JOINED_POOL_STATE_CONNECTED
	})
	if joined.GetCoordinatorName() != "rig" || joined.GetCoordinatorId() == "" || joined.GetAddress() != addr || !slices.Equal(joined.GetRoles(), worker) || joined.WorkerCount != nil {
		t.Errorf("the joined pool: %v", joined)
	}

	// Given a client invitation too, it is told it holds both roles.
	if _, err := as(t, workerDir, "pool", "join", "--addr", addr, invite(t, addr, "client")); err != nil {
		t.Fatal(err)
	}
	// As a client, it is also told how many workers the pool has, and told
	// again when another joins.
	counted := until("the node to hold both roles and be told the worker count", func(p *nodepb.JoinedPool) bool {
		return slices.Equal(p.GetRoles(), both) && p.WorkerCount != nil
	})
	startDaemon(t, "--role", "worker", "--coordinator", addr, "--join", invite(t, addr, "worker"), "--data-dir", t.TempDir(), "--name", "other", "--slots", "1")
	until("the count to take in the new worker", func(p *nodepb.JoinedPool) bool {
		return p.GetWorkerCount() == counted.GetWorkerCount()+1
	})
	if got, err := client.GetJoinedPool(ctx, &nodepb.GetJoinedPoolRequest{}); err != nil || !slices.Equal(got.GetRoles(), both) || got.GetState() != nodepb.JoinedPoolState_JOINED_POOL_STATE_CONNECTED {
		t.Errorf("GetJoinedPool = %v, %v", got, err)
	}

	// With its coordinator gone, it says so, and keeps what it knew.
	stopCoordinator()
	gone := until("the node to see its coordinator gone", func(p *nodepb.JoinedPool) bool {
		return p.GetState() == nodepb.JoinedPoolState_JOINED_POOL_STATE_DISCONNECTED
	})
	if gone.GetCoordinatorName() != "rig" || gone.GetCoordinatorId() != joined.GetCoordinatorId() || !slices.Equal(gone.GetRoles(), both) || gone.WorkerCount != nil {
		t.Errorf("after its coordinator went, the joined pool: %v", gone)
	}
}

// What a worker knows of its pool is said as the local API says it:
// connecting until first welcomed, then connected, then, once it has lost
// the connection, disconnected with what it knew kept.
func TestJoinedPoolStates(t *testing.T) {
	for _, tt := range []struct {
		pool worker.JoinedPool
		want nodepb.JoinedPoolState
	}{
		{worker.JoinedPool{}, nodepb.JoinedPoolState_JOINED_POOL_STATE_CONNECTING},
		{worker.JoinedPool{ID: "c", Connected: true, Welcomed: true}, nodepb.JoinedPoolState_JOINED_POOL_STATE_CONNECTED},
		{worker.JoinedPool{ID: "c", Welcomed: true}, nodepb.JoinedPoolState_JOINED_POOL_STATE_DISCONNECTED},
	} {
		if got := joinedPool(tt.pool, "rig:7700"); got.GetState() != tt.want || got.GetAddress() != "rig:7700" || got.GetCoordinatorId() != tt.pool.ID {
			t.Errorf("joinedPool(%+v) = %v, want state %v", tt.pool, got, tt.want)
		}
	}
}
