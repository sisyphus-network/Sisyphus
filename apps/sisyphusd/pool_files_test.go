package main

import (
	"context"
	"errors"
	"fmt"
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
// removing it lets the coordinator go. The names stay on the node. A chat's
// attachments are kept there as long as the chat or their draft wants them.
// A node that is only a worker is refused (#500).
func TestAJoinedClientNodeKeepsItsFilesInThePool(t *testing.T) {
	addr, _ := startNode(t, "--name", "rig")
	workerDir, apiAddr := t.TempDir(), freeAddr(t)
	startDaemon(t, "--role", "worker", "--coordinator", addr, "--join", invite(t, addr, "worker"), "--data-dir", workerDir, "--name", "hand", "--slots", "1", "--api-listen", apiAddr)
	client := desktop(t, apiAddr)
	poolAsSeenBy(t, client)
	ctx, cancel := context.WithTimeout(tokenOf(t, workerDir), 2*time.Minute)
	defer cancel()
	refused := func() error {
		t.Helper()
		upload, err := client.StoreFile(ctx)
		if err != nil {
			t.Fatal(err)
		}
		upload.Send(&nodepb.StoreFileRequest{Name: "notes.txt", Data: []byte("words")})
		_, err = upload.CloseAndRecv()
		return err
	}

	// As a worker alone it has no files in the pool.
	waitFor(t, func() bool {
		pool, err := client.GetJoinedPool(ctx, &nodepb.GetJoinedPoolRequest{})
		return err == nil && pool.GetState() == nodepb.JoinedPoolState_JOINED_POOL_STATE_CONNECTED
	})
	if err := refused(); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "not a client") {
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

	// A chat attachment is kept in the pool too: for a while as a draft,
	// for good once a chat has it, and for a while again once that chat is
	// gone, since the draft still holds it.
	attached := storeThroughDesktop(t, ctx, client, "attached.txt", text, true, true)
	if kept := clientPins(t, addr, attached.GetCid()); len(kept) != 1 || strings.HasSuffix(kept[0], "released") {
		t.Errorf("the coordinator keeps a draft attachment as %q", kept)
	}
	arguments := fmt.Sprintf(`{"workload":"wordcount","params":{"input":%q}}`, attached.GetCid())
	served := newModel(t, wants("run_job", arguments), says("counted"))
	if _, err := client.SetModelConfig(ctx, &nodepb.SetModelConfigRequest{Provider: "ollama", BaseUrl: served.URL, Model: "test-model"}); err != nil {
		t.Fatal(err)
	}
	asked, err := client.Ask(ctx, &nodepb.AskRequest{Text: fmt.Sprintf("Count these words.\n\n[Sisyphus attachments]\n- \"attached.txt\" | cid:%s | image:false | private:true", attached.GetCid()), AttachmentCids: []string{attached.GetCid()}})
	if err != nil {
		t.Fatal(err)
	}
	chatID, counted := "", false
	for {
		event, err := asked.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		chatID = event.GetChatId()
		counted = counted || event.GetKind() == "result" && strings.Contains(event.GetText(), `"state":"succeeded"`)
	}
	if !counted {
		t.Error("the attachment was not counted in the pool")
	}
	waitFor(t, func() bool {
		kept := clientPins(t, addr, attached.GetCid())
		return len(kept) == 1 && strings.HasSuffix(kept[0], "released")
	})
	if _, err := client.DeleteChat(ctx, &nodepb.DeleteChatRequest{ChatId: chatID}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		kept := clientPins(t, addr, attached.GetCid())
		return len(kept) == 1 && !strings.HasSuffix(kept[0], "released")
	})

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

// pinningPool records what a joined node asks its coordinator to keep, and
// fails as told.
type pinningPool struct {
	pb.BlobServiceClient
	pinned   []*pb.PinBlobRequest
	unpinned []string
	err      error
}

func (p *pinningPool) Pin(_ context.Context, req *pb.PinBlobRequest, _ ...grpc.CallOption) (*pb.PinBlobResponse, error) {
	if p.err == nil {
		p.pinned = append(p.pinned, req)
	}
	return &pb.PinBlobResponse{}, p.err
}

func (p *pinningPool) Unpin(_ context.Context, req *pb.UnpinBlobRequest, _ ...grpc.CallOption) (*pb.UnpinBlobResponse, error) {
	if p.err == nil {
		p.unpinned = append(p.unpinned, req.GetCid())
	}
	return &pb.UnpinBlobResponse{}, p.err
}

// memHolds keeps a joined node's holds on files in memory, failing as told.
type memHolds struct {
	held       map[[3]string]time.Time
	load, save error
}

func (h *memHolds) PoolFileHolds(coordinator, c string, _ time.Time) (map[string]time.Time, error) {
	holds := make(map[string]time.Time)
	for key, expires := range h.held {
		if key[0] == coordinator && key[1] == c {
			holds[key[2]] = expires
		}
	}
	return holds, h.load
}

func (h *memHolds) HoldPoolFile(coordinator, c, owner string, expires, _ time.Time) error {
	if h.save == nil {
		h.held[[3]string{coordinator, c, owner}] = expires
	}
	return h.save
}

func (h *memHolds) ReleasePoolFile(coordinator, c, owner string) error {
	if h.save == nil {
		delete(h.held, [3]string{coordinator, c, owner})
	}
	return h.save
}

// What a joined node's file store asks of its coordinator as the owners on
// the node hold and let go of a file, and when the pool, its coordinator or
// the node's own record fails it.
func TestAJoinedNodesFilesAreKeptAsLongAsTheLongestHold(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c, _ := cid.Decode("bafkreigh2akiscaildcqabsyg3dfr6chu3fgpregiymsck7e7aqa4s52zy")
	other, _ := cid.Decode("bafkreie5cvv4h45feadgeuwhbcutmh6t2ceseocckahdoe6uat64zmz454")
	away := errors.New("away")
	pool, holds := &pinningPool{}, &memHolds{held: make(map[[3]string]time.Time)}
	files := &poolFiles{blobs: fakeBlobs{}, client: pool, holds: holds, coordinator: func() string { return "c" }, available: func() error { return nil }}
	ttl := func() uint64 { return pool.pinned[len(pool.pinned)-1].GetTtlSeconds() }
	about := func(got uint64, want time.Duration) bool {
		return got > uint64((want-10*time.Second)/time.Second) && got <= uint64(want/time.Second)
	}

	// A draft's hold, for an hour; then a chat's, for good; then the
	// user's, for two hours, which changes nothing while the chat holds it.
	if err := files.Pin(ctx, "chat-draft", time.Now().Add(time.Hour), c); err != nil || !about(ttl(), time.Hour) {
		t.Errorf("held by a draft: %v, pinned for %ds", err, ttl())
	}
	if err := files.Pin(ctx, "chat:1", time.Time{}, c); err != nil || ttl() != 0 {
		t.Errorf("held by a chat: %v, pinned for %ds", err, ttl())
	}
	if err := files.Pin(ctx, api.UserOwner, time.Now().Add(2*time.Hour), c); err != nil || ttl() != 0 {
		t.Errorf("held by the user as well: %v, pinned for %ds", err, ttl())
	}
	// The chat lets go: the longest left is the user's.
	if err := files.Unpin("chat:1", c); err != nil || !about(ttl(), 2*time.Hour) {
		t.Errorf("after the chat let go: %v, pinned for %ds", err, ttl())
	}
	// A hold already over keeps a file a second, until the next change.
	if err := files.Pin(ctx, "chat-draft", time.Now().Add(-time.Minute), other); err != nil || ttl() != 1 {
		t.Errorf("held until a moment past: %v, pinned for %ds", err, ttl())
	}
	// The last to let go has the coordinator let go too.
	if err := files.Unpin(api.UserOwner, c); err != nil || len(pool.unpinned) != 0 {
		t.Errorf("after the user let go: %v, released %v", err, pool.unpinned)
	}
	if err := files.Unpin("chat-draft", c); err != nil || !slices.Equal(pool.unpinned, []string{c.String()}) {
		t.Errorf("after the last let go: %v, released %v", err, pool.unpinned)
	}
	if size, err := files.Size(ctx); size != 0 || err != nil {
		t.Errorf("Size = %d, %v", size, err)
	}

	// The coordinator no longer has a file, or fails: nothing is noted.
	pool.err = status.Error(codes.NotFound, "blob not found")
	if err := files.Pin(ctx, "chat:2", time.Time{}, c); !errors.Is(err, storage.ErrNotFound) || len(holds.held) != 1 {
		t.Errorf("pinning what the pool does not have: %v, holds %v", err, holds.held)
	}
	pool.err = away
	if err := files.Unpin("chat-draft", other); !errors.Is(err, away) || len(holds.held) != 1 {
		t.Errorf("releasing with the coordinator failing: %v, holds %v", err, holds.held)
	}
	pool.err = nil
	// The node's record fails.
	holds.save = away
	if err := files.Pin(ctx, "chat:2", time.Time{}, c); !errors.Is(err, away) {
		t.Errorf("noting a hold failing: %v", err)
	}
	holds.save, holds.load = nil, away
	if err := files.Unpin("chat-draft", other); !errors.Is(err, away) {
		t.Errorf("reading the holds failing: %v", err)
	}

	// Away from the pool, nothing is stored, read, held or let go there.
	gone := &poolFiles{blobs: fakeBlobs{put: away}, client: pool, available: func() error { return errPoolAway }}
	if _, err := gone.Put(ctx, strings.NewReader("x")); !errors.Is(err, errPoolAway) {
		t.Errorf("storing while away: %v", err)
	}
	if _, err := gone.Open(ctx, c); !errors.Is(err, errPoolAway) {
		t.Errorf("reading while away: %v", err)
	}
	if err := gone.Available(); !errors.Is(err, errPoolAway) {
		t.Errorf("Available while away: %v", err)
	}
	if err := gone.Unpin(api.UserOwner, c); !errors.Is(err, errPoolAway) {
		t.Errorf("letting go while away: %v", err)
	}
	here := &poolFiles{blobs: fakeBlobs{put: away}, client: pool, available: func() error { return nil }}
	if _, err := here.Put(ctx, strings.NewReader("x")); !errors.Is(err, away) {
		t.Errorf("storing with the pool failing: %v", err)
	}
	if _, err := here.Open(ctx, c); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("reading what the pool does not have: %v", err)
	}
}
