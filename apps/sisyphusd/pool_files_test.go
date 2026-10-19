package main

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ipfs/go-cid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/api"
	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// A node that has joined another's pool as a client stores its user's files
// in that pool through its own API: the coordinator holds each for it as a
// client, sealed if it is private, a job there reads one by its CID, and
// removing it lets the coordinator go. The names stay on the node. A node
// that is only a worker, and a chat attachment, are refused (#500).
func TestAJoinedClientNodeKeepsItsFilesInThePool(t *testing.T) {
	addr, _ := startNode(t, "--name", "rig")
	workerDir, apiAddr := t.TempDir(), freeAddr(t)
	startDaemon(t, "--role", "worker", "--coordinator", addr, "--join", invite(t, addr, "worker"), "--data-dir", workerDir, "--name", "hand", "--slots", "1", "--api-listen", apiAddr)
	client := desktop(t, apiAddr)
	poolAsSeenBy(t, client)
	ctx, cancel := context.WithTimeout(tokenOf(t, workerDir), 2*time.Minute)
	defer cancel()
	refused := func(attachment bool) error {
		t.Helper()
		upload, err := client.StoreFile(ctx)
		if err != nil {
			t.Fatal(err)
		}
		upload.Send(&nodepb.StoreFileRequest{Name: "notes.txt", Private: attachment, ChatAttachment: attachment, Data: []byte("words")})
		_, err = upload.CloseAndRecv()
		return err
	}

	// As a worker alone it has no files in the pool.
	waitFor(t, func() bool {
		pool, err := client.GetJoinedPool(ctx, &nodepb.GetJoinedPoolRequest{})
		return err == nil && pool.GetState() == nodepb.JoinedPoolState_JOINED_POOL_STATE_CONNECTED
	})
	if err := refused(false); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "not a client") {
		t.Fatalf("a worker alone storing a file: %v", err)
	}

	if _, err := as(t, workerDir, "pool", "join", "--addr", addr, invite(t, addr, "client")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		pool, err := client.GetJoinedPool(ctx, &nodepb.GetJoinedPoolRequest{})
		return err == nil && slices.Contains(pool.GetRoles(), nodepb.PoolRole_POOL_ROLE_CLIENT)
	})

	// A plain file: in the pool, read back, and counted by a job there.
	text := "the quick brown fox jumps over the lazy dog the end"
	plain := storeThroughDesktop(t, ctx, client, "fox.txt", text, false)
	if got, err := fetchThroughDesktop(ctx, client, plain.GetCid()); err != nil || got != text {
		t.Errorf("reading the file back: %q, %v", got, err)
	}
	if listed, err := client.ListFiles(ctx, &nodepb.ListFilesRequest{}); err != nil || len(listed.GetFiles()) != 1 || listed.GetFiles()[0].GetName() != "fox.txt" {
		t.Errorf("ListFiles = %v, %v", listed, err)
	}
	// Kept for the client until it is released.
	if kept := clientPins(t, addr, plain.GetCid()); len(kept) != 1 || !strings.HasSuffix(kept[0], "released") {
		t.Errorf("the coordinator keeps the file for the client as %q", kept)
	}
	submitted, err := client.SubmitJob(ctx, &nodepb.SubmitJobRequest{Workload: "wordcount", Params: []byte(`{"input":"` + plain.GetCid() + `"}`), Mode: nodepb.JobMode_JOB_MODE_DISTRIBUTED})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		job, err := client.GetJob(ctx, &nodepb.GetJobRequest{JobId: submitted.GetJob().GetJobId()})
		return err == nil && job.GetJob().GetState() == nodepb.JobState_JOB_STATE_SUCCEEDED && strings.Contains(string(job.GetJob().GetResult()), `"words":11`)
	})

	// A private file is sealed before the pool holds it, and read back
	// unsealed only here.
	secret := "a note only this machine can read"
	private := storeThroughDesktop(t, ctx, client, "secret.txt", secret, true)
	if got, err := fetchThroughDesktop(ctx, client, private.GetCid()); err != nil || got != secret {
		t.Errorf("reading the private file back: %q, %v", got, err)
	}
	if held := mustCLI(t, "blob", "get", "--addr", addr, private.GetCid()); strings.Contains(held, secret) {
		t.Error("the coordinator holds the private file in the clear")
	}

	// A chat attachment is not yet kept in a joined pool.
	if err := refused(true); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "attachments") {
		t.Errorf("a chat attachment on a joined node: %v", err)
	}

	// Removed, the coordinator no longer keeps it for this client. Like
	// any upload, it may still be held for a while after, by the
	// coordinator's own grace pins.
	if _, err := client.RemoveFile(ctx, &nodepb.RemoveFileRequest{Cid: plain.GetCid()}); err != nil {
		t.Fatal(err)
	}
	if kept := clientPins(t, addr, plain.GetCid()); len(kept) != 0 {
		t.Errorf("after removing the file, the client's pins on it are %q", kept)
	}
}

// clientPins returns the lines of a coordinator's pins that keep c for a
// client.
func clientPins(t *testing.T, addr, c string) []string {
	t.Helper()
	var kept []string
	for line := range strings.Lines(mustCLI(t, "blob", "pins", "--addr", addr)) {
		if fields := strings.Fields(line); len(fields) >= 2 && fields[0] == c && strings.HasPrefix(fields[1], "user:") {
			kept = append(kept, strings.TrimSpace(line))
		}
	}
	return kept
}

// fakeBlobs is a worker's way to its coordinator's store, failing as told.
type fakeBlobs struct{ put error }

func (fakeBlobs) Open(context.Context, cid.Cid) (storage.Blob, error) {
	return nil, storage.ErrNotFound
}

func (b fakeBlobs) Put(context.Context, io.Reader) (cid.Cid, error) { return cid.Undef, b.put }

// pinningPool records what a joined node asks its coordinator to keep.
type pinningPool struct {
	pb.BlobServiceClient
	pinned   []*pb.PinBlobRequest
	unpinned []string
	err      error
}

func (p *pinningPool) Pin(_ context.Context, req *pb.PinBlobRequest, _ ...grpc.CallOption) (*pb.PinBlobResponse, error) {
	p.pinned = append(p.pinned, req)
	return &pb.PinBlobResponse{}, p.err
}

func (p *pinningPool) Unpin(_ context.Context, req *pb.UnpinBlobRequest, _ ...grpc.CallOption) (*pb.UnpinBlobResponse, error) {
	p.unpinned = append(p.unpinned, req.GetCid())
	return &pb.UnpinBlobResponse{}, p.err
}

// What a joined node's file store does with owners other than the user,
// with time limits, and when the pool or its coordinator fails it.
func TestAJoinedNodesFileStoreKeepsOnlyTheUsersFiles(t *testing.T) {
	ctx := context.Background()
	c, _ := cid.Decode("bafkreigh2akiscaildcqabsyg3dfr6chu3fgpregiymsck7e7aqa4s52zy")
	away := errors.New("away")
	pool := &pinningPool{}
	files := poolFiles{blobs: fakeBlobs{}, client: pool, available: func() error { return nil }}

	if err := files.Pin(ctx, "chat-draft", time.Time{}, c); !errors.Is(err, errOnlyUserFiles) || len(pool.pinned) != 0 {
		t.Errorf("pinning for a chat: %v, asked the pool %v", err, pool.pinned)
	}
	if err := files.Pin(ctx, api.UserOwner, time.Now().Add(time.Hour), c); err != nil || len(pool.pinned) != 1 || pool.pinned[0].GetTtlSeconds() < 3590 || pool.pinned[0].GetTtlSeconds() > 3600 {
		t.Errorf("pinning for an hour: %v, asked the pool %v", err, pool.pinned)
	}
	if err := files.Unpin("chat-draft", c); err != nil || len(pool.unpinned) != 0 {
		t.Errorf("releasing for a chat: %v, asked the pool to release %v", err, pool.unpinned)
	}
	if size, err := files.Size(ctx); size != 0 || err != nil {
		t.Errorf("Size = %d, %v", size, err)
	}

	pool.err = away
	if err := files.Pin(ctx, api.UserOwner, time.Time{}, c); !errors.Is(err, away) {
		t.Errorf("pinning with the coordinator failing: %v", err)
	}
	if err := files.Unpin(api.UserOwner, c, c); !errors.Is(err, away) {
		t.Errorf("releasing with the coordinator failing: %v", err)
	}

	// Away from the pool, nothing is stored or read there.
	gone := poolFiles{blobs: fakeBlobs{put: away}, client: pool, available: func() error { return errPoolAway }}
	if _, err := gone.Put(ctx, strings.NewReader("x")); !errors.Is(err, errPoolAway) {
		t.Errorf("storing while away: %v", err)
	}
	if _, err := gone.Open(ctx, c); !errors.Is(err, errPoolAway) {
		t.Errorf("reading while away: %v", err)
	}
	if err := gone.Available(); !errors.Is(err, errPoolAway) {
		t.Errorf("Available while away: %v", err)
	}
	here := poolFiles{blobs: fakeBlobs{put: away}, client: pool, available: func() error { return nil }}
	if _, err := here.Put(ctx, strings.NewReader("x")); !errors.Is(err, away) {
		t.Errorf("storing with the pool failing: %v", err)
	}
	if _, err := here.Open(ctx, c); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("reading what the pool does not have: %v", err)
	}
}
