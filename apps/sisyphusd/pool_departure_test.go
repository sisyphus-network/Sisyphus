package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sisyphus-network/Sisyphus/packages/nodedb"
	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
)

// A joined node that leaves its pool has the coordinator let go of its
// files, forgets the pool, and stops; started again for that pool, it is
// told it has not joined it.
func TestAJoinedNodeLeavesItsPoolAndStops(t *testing.T) {
	coordinator, _ := startNode(t, "--name", "rig")
	workerDir, apiAddr := t.TempDir(), freeAddr(t)
	args := []string{"run", "--role", "worker", "--coordinator", coordinator, "--join", invite(t, coordinator, "worker"),
		"--data-dir", workerDir, "--name", "hand", "--slots", "1", "--api-listen", apiAddr, "--discovery", "off"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan error, 1)
	go func() { stopped <- run(ctx, args) }()
	waitForSecretFile(t, filepath.Join(workerDir, "api.token"))
	client := desktop(t, apiAddr)
	authed := tokenOf(t, workerDir)
	waitFor(t, func() bool {
		pool, err := client.GetJoinedPool(authed, &nodepb.GetJoinedPoolRequest{})
		return err == nil && pool.GetState() == nodepb.JoinedPoolState_JOINED_POOL_STATE_CONNECTED
	})
	if _, err := as(t, workerDir, "pool", "join", "--addr", coordinator, invite(t, coordinator, "client")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		pool, err := client.GetJoinedPool(authed, &nodepb.GetJoinedPoolRequest{})
		return err == nil && slices.Contains(pool.GetRoles(), nodepb.PoolRole_POOL_ROLE_CLIENT)
	})
	stored := storeThroughDesktop(t, authed, client, "notes.txt", "kept in the pool until the node leaves", false)
	coordinatorID := nodeID(t, dataDirOfNode(coordinator))
	if kept := clientPins(t, coordinator, stored.GetCid()); len(kept) == 0 {
		t.Fatal("the coordinator keeps nothing for the node before it leaves")
	}

	if _, err := client.LeavePool(authed, &nodepb.LeavePoolRequest{}); err != nil {
		t.Fatalf("LeavePool: %v", err)
	}
	select {
	case err := <-stopped:
		if err != nil {
			t.Errorf("after leaving, the node stopped with: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the node did not stop after leaving its pool")
	}

	if kept := clientPins(t, coordinator, stored.GetCid()); len(kept) != 0 {
		t.Errorf("after the node left, the coordinator keeps its file as %q", kept)
	}
	if members := mustCLI(t, "pool", "members", "--addr", coordinator); strings.Contains(members, nodeID(t, workerDir)) {
		t.Errorf("after the node left, the members are:\n%s", members)
	}
	if known, err := loadKnown(workerDir); err != nil || known[coordinator] != "" {
		t.Errorf("after leaving, the node knows %v, %v", known, err)
	}
	db, err := nodedb.Open(filepath.Join(workerDir, "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	files, err := db.FilesInPool(coordinatorID)
	db.Close()
	if err != nil || len(files) != 0 {
		t.Errorf("after leaving, the node lists %v, %v", files, err)
	}
	again := slices.Clone(args)
	again = slices.Delete(again, slices.Index(again, "--join"), slices.Index(again, "--join")+2)
	if err := run(context.Background(), again); err == nil || !strings.Contains(err.Error(), "has not joined a coordinator at "+coordinator) {
		t.Errorf("started again for the pool it left: %v", err)
	}
}

// leavingPool is a coordinator to leave, answering as told.
type leavingPool struct {
	pb.PoolServiceClient
	errs  []error
	asked int
}

func (p *leavingPool) Leave(ctx context.Context, _ *pb.LeaveRequest, _ ...grpc.CallOption) (*pb.LeaveResponse, error) {
	p.asked++
	if _, bounded := ctx.Deadline(); !bounded {
		return nil, errors.New("asked without a deadline")
	}
	var err error
	if len(p.errs) > 0 {
		err, p.errs = p.errs[0], p.errs[1:]
	}
	return &pb.LeaveResponse{}, err
}

func TestALeaveThatCannotBeCarriedOutIsAskedAgain(t *testing.T) {
	dataDir := t.TempDir()
	if err := saveKnown(dataDir, map[string]string{"rig:7700": "rig-id", "other:7700": "other-id"}); err != nil {
		t.Fatal(err)
	}
	pool := &leavingPool{errs: []error{status.Error(codes.Unavailable, "coordinator away")}}
	forgot, forgetErr := 0, errors.New("disk full")
	l := newPoolDeparture(pool, func(id string) error {
		if id != "rig-id" {
			t.Errorf("forgot the pool of %q", id)
		}
		forgot++
		return forgetErr
	}, "rig-id", "rig:7700", dataDir, slog.New(slog.DiscardHandler))
	turnedAway := status.Error(codes.PermissionDenied, "not a member")

	// The coordinator cannot be asked: nothing is forgotten, and a worker
	// turned away is the failure it was.
	if err := l.leave(context.Background()); status.Code(err) != codes.Unavailable || !strings.Contains(err.Error(), "has not left the pool; ask again: coordinator away") || forgot != 0 {
		t.Errorf("leaving a coordinator that is away: %v, forgot %d times", err, forgot)
	}
	if err := l.workerStopped(context.Background(), turnedAway); err != turnedAway {
		t.Errorf("the worker stopped before the node left: %v", err)
	}

	// The coordinator lets go, but the node cannot forget: it is told to
	// ask again, and meanwhile does not stop.
	if err := l.leave(context.Background()); status.Code(err) != codes.Internal || !strings.Contains(err.Error(), "has left the pool, but could not forget it here; ask again: disk full") {
		t.Errorf("leaving while the database fails: %v", err)
	}
	waiting, cancel := context.WithCancel(context.Background())
	ended := make(chan error, 1)
	go func() { ended <- l.workerStopped(waiting, turnedAway) }()
	select {
	case err := <-ended:
		t.Fatalf("the node stopped before it forgot the pool: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	// Nor can it read where the pool is remembered.
	forgetErr = nil
	knownPath := filepath.Join(dataDir, "known.json")
	backupPath := filepath.Join(dataDir, "known.saved")
	if err := os.Rename(knownPath, backupPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(knownPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := l.leave(context.Background()); status.Code(err) != codes.Internal || !strings.Contains(err.Error(), "read known nodes") {
		t.Errorf("leaving while known.json cannot be read: %v", err)
	}
	if err := os.Remove(knownPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backupPath, knownPath); err != nil {
		t.Fatal(err)
	}

	// Asked again, it finishes, and the node stops.
	if err := l.leave(context.Background()); err != nil {
		t.Fatalf("leaving again: %v", err)
	}
	if err := <-ended; err != nil {
		t.Errorf("after leaving, the node stopped with: %v", err)
	}
	cancel()
	if known, err := loadKnown(dataDir); err != nil || len(known) != 1 || known["other:7700"] != "other-id" {
		t.Errorf("after leaving, the node knows %v, %v", known, err)
	}
	// Once it has left, it has left: the coordinator is not asked again.
	if err := l.leave(context.Background()); err != nil || pool.asked != 4 || forgot != 3 {
		t.Errorf("leaving once more: %v, coordinator asked %d times, forgot %d times", err, pool.asked, forgot)
	}

	// Stopped while it waits to forget the pool, the node stops.
	l = newPoolDeparture(&leavingPool{}, func(string) error { return errors.New("disk full") }, "rig-id", "rig:7700", dataDir, slog.New(slog.DiscardHandler))
	if err := l.leave(context.Background()); status.Code(err) != codes.Internal {
		t.Fatalf("leaving while the database fails: %v", err)
	}
	gone, stop := context.WithCancel(context.Background())
	stop()
	if err := l.workerStopped(gone, turnedAway); err != nil {
		t.Errorf("stopped while waiting to forget the pool: %v", err)
	}
	// A leave asked with a deadline of its own keeps it.
	bounded, done := context.WithTimeout(context.Background(), time.Minute)
	defer done()
	if err := newPoolDeparture(&leavingPool{}, func(string) error { return nil }, "rig-id", "rig:7700", dataDir, slog.New(slog.DiscardHandler)).leave(bounded); err != nil {
		t.Errorf("leaving with a deadline: %v", err)
	}
}

// loadKnown on a damaged known.json fails, and so does forgetting from it.
func TestForgettingAPoolWhoseKnownNodesCannotBeRead(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, "known.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := forgetKnown(dataDir, "rig:7700"); err == nil || !strings.Contains(err.Error(), "known nodes in") {
		t.Errorf("forgetting from a damaged known.json: %v", err)
	}
}

func TestSavingKnownReplacesItWithACompletePrivateFile(t *testing.T) {
	dataDir := t.TempDir()
	if err := saveKnown(dataDir, map[string]string{"rig:7700": "rig-id"}); err != nil {
		t.Fatal(err)
	}
	if err := saveKnown(dataDir, map[string]string{"other:7700": "other-id"}); err != nil {
		t.Fatal(err)
	}
	got, err := loadKnown(dataDir)
	if err != nil || len(got) != 1 || got["other:7700"] != "other-id" {
		t.Errorf("known nodes after replacement: %v, %v", got, err)
	}
	info, err := os.Stat(filepath.Join(dataDir, "known.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("known.json permissions: %v, %v", info, err)
	}
	temps, err := filepath.Glob(filepath.Join(dataDir, ".known-*"))
	if err != nil || len(temps) != 0 {
		t.Errorf("temporary known files left behind: %v, %v", temps, err)
	}
}

type knownTempFileFailure struct {
	*os.File
	writeErr error
	syncErr  error
	closeErr error
}

func (f *knownTempFileFailure) Write(data []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return f.File.Write(data)
}

func (f *knownTempFileFailure) Sync() error {
	if f.syncErr != nil {
		return f.syncErr
	}
	return f.File.Sync()
}

func (f *knownTempFileFailure) Close() error {
	return errors.Join(f.closeErr, f.File.Close())
}

func TestFailedKnownReplacementKeepsThePreviousFile(t *testing.T) {
	for _, fail := range []string{"create", "write", "sync", "close", "rename"} {
		t.Run(fail, func(t *testing.T) {
			dataDir := t.TempDir()
			original := map[string]string{"rig:7700": "rig-id"}
			if err := saveKnown(dataDir, original); err != nil {
				t.Fatal(err)
			}
			ops := knownFileOps{
				create: func(dir string) (knownTempFile, error) { return os.CreateTemp(dir, ".known-*") },
				remove: os.Remove,
				rename: os.Rename,
			}
			if fail == "create" {
				ops.create = func(string) (knownTempFile, error) { return nil, errors.New("create failed") }
			} else if fail == "rename" {
				ops.rename = func(string, string) error { return errors.New("rename failed") }
			} else {
				ops.create = func(dir string) (knownTempFile, error) {
					file, err := os.CreateTemp(dir, ".known-*")
					if err != nil {
						return nil, err
					}
					f := &knownTempFileFailure{File: file}
					switch fail {
					case "write":
						f.writeErr = errors.New("write failed")
					case "sync":
						f.syncErr = errors.New("sync failed")
					case "close":
						f.closeErr = errors.New("close failed")
					}
					return f, nil
				}
			}

			if err := saveKnownBytes(dataDir, []byte(`{"other:7700":"other-id"}`), ops); err == nil {
				t.Fatal("saving known nodes succeeded despite a simulated file error")
			}
			got, err := loadKnown(dataDir)
			if err != nil || len(got) != 1 || got["rig:7700"] != "rig-id" {
				t.Errorf("previous known nodes after failed replacement: %v, %v", got, err)
			}
			temps, err := filepath.Glob(filepath.Join(dataDir, ".known-*"))
			if err != nil || len(temps) != 0 {
				t.Errorf("temporary known files after failed replacement: %v, %v", temps, err)
			}
		})
	}
}
