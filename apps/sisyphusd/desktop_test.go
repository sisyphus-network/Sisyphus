package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/access"
	"github.com/sisyphus-network/Sisyphus/packages/identity"
	"github.com/sisyphus-network/Sisyphus/packages/nodedb"
	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
)

// storeThroughDesktop stores a file through a node's local API.
func storeThroughDesktop(t *testing.T, ctx context.Context, client nodepb.NodeServiceClient, name, content string, private bool) *nodepb.File {
	t.Helper()
	upload, err := client.StoreFile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := upload.Send(&nodepb.StoreFileRequest{Name: name, Private: private, Data: []byte(content[:len(content)/2])}); err != nil {
		t.Fatal(err)
	}
	if err := upload.Send(&nodepb.StoreFileRequest{Data: []byte(content[len(content)/2:])}); err != nil {
		t.Fatal(err)
	}
	file, err := upload.CloseAndRecv()
	if err != nil {
		t.Fatal(err)
	}
	return file
}

// fetchThroughDesktop reads what a node's local API returns for a CID.
func fetchThroughDesktop(ctx context.Context, client nodepb.NodeServiceClient, id string) (string, error) {
	stream, err := client.FetchFile(ctx, &nodepb.FetchFileRequest{Cid: id})
	if err != nil {
		return "", err
	}
	var got bytes.Buffer
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return got.String(), nil
		}
		if err != nil {
			return "", err
		}
		got.Write(msg.GetData())
	}
}

// A private job from the desktop: the file is sealed before the pool holds
// it, the job's output is sealed too, and the desktop reads both as if
// they were not.
func TestTheDesktopRunsAPrivateJob(t *testing.T) {
	dataDir := t.TempDir()
	addr, apiAddr := freeAddr(t), freeAddr(t)
	startDaemon(t, "--data-dir", dataDir, "--listen", addr, "--name", "rig", "--api-listen", apiAddr)
	client := desktop(t, apiAddr)
	poolAsSeenBy(t, client)
	ctx := tokenOf(t, dataDir)
	const text = "the quick brown fox jumps over the lazy dog and the fox sleeps"

	file := storeThroughDesktop(t, ctx, client, "secret.txt", text, true)
	if !file.GetPrivate() || file.GetSizeBytes() != uint64(len(text)) {
		t.Fatalf("the private file: %v", file)
	}
	// What the pool holds, and gives any member that asks, is not the text.
	held := mustCLI(t, "blob", "get", "--addr", addr, file.GetCid())
	if strings.Contains(held, "fox") || len(held) <= len(text) {
		t.Errorf("the pool holds the file unsealed: %q", held)
	}
	if got, err := fetchThroughDesktop(ctx, client, file.GetCid()); err != nil || got != text {
		t.Errorf("the private file fetched: %q, %v", got, err)
	}
	listed, err := client.ListFiles(ctx, &nodepb.ListFilesRequest{})
	if err != nil || len(listed.GetFiles()) != 1 || !listed.GetFiles()[0].GetPrivate() {
		t.Errorf("the files listed: %v, %v", listed, err)
	}

	submitted, err := client.SubmitJob(ctx, &nodepb.SubmitJobRequest{Workload: "wordcount", Params: []byte(`{"input":"` + file.GetCid() + `"}`), Private: true})
	if err != nil || !submitted.GetJob().GetPrivate() {
		t.Fatalf("submitting a private job: %v, %v", submitted, err)
	}
	var job *nodepb.Job
	waitFor(t, func() bool {
		got, err := client.GetJob(ctx, &nodepb.GetJobRequest{JobId: submitted.GetJob().GetJobId()})
		job = got.GetJob()
		return err == nil && job.GetState() == nodepb.JobState_JOB_STATE_SUCCEEDED
	})
	if !job.GetPrivate() || len(job.GetOutputBlobs()) != 1 {
		t.Fatalf("the finished job: %v", job)
	}
	output := job.GetOutputBlobs()[0]
	if held := mustCLI(t, "blob", "get", "--addr", addr, output); strings.Contains(held, "fox") {
		t.Errorf("the pool holds the job's output unsealed: %q", held)
	}
	if counts, err := fetchThroughDesktop(ctx, client, output); err != nil || !strings.HasPrefix(counts, "3\tthe\n2\tfox\n") {
		t.Errorf("the private job's output: %q, %v", counts, err)
	}

	// The key is an ordinary one, in the data directory, and the command
	// line reads with it what the desktop stored.
	key := filepath.Join(dataDir, "private.key")
	if out := mustCLI(t, "blob", "get", "--addr", addr, "--key-file", key, file.GetCid()); out != text {
		t.Errorf("the file read with the node's key from the command line: %q", out)
	}
	// A job that is not private is given no key, and counts only the words
	// it can see in what is sealed, which are not the file's.
	open, err := client.SubmitJob(ctx, &nodepb.SubmitJobRequest{Workload: "wordcount", Params: []byte(`{"input":"` + file.GetCid() + `"}`)})
	if err != nil || open.GetJob().GetPrivate() {
		t.Fatalf("submitting a job that is not private: %v, %v", open, err)
	}
	waitFor(t, func() bool {
		got, err := client.GetJob(ctx, &nodepb.GetJobRequest{JobId: open.GetJob().GetJobId()})
		job = got.GetJob()
		return err == nil && job.GetFinishedAtMs() != 0
	})
	for _, output := range job.GetOutputBlobs() {
		if out, _ := fetchThroughDesktop(ctx, client, output); strings.Contains(out, "\tfox\n") {
			t.Errorf("a job that is not private read the private file: %q", out)
		}
	}

	// Sealed by somebody else, a blob is not this node's to open.
	other := filepath.Join(t.TempDir(), "other.key")
	mustCLI(t, "key", "new", other)
	source := filepath.Join(t.TempDir(), "theirs")
	if err := os.WriteFile(source, []byte("not yours"), 0o600); err != nil {
		t.Fatal(err)
	}
	theirs := strings.TrimSpace(mustCLI(t, "blob", "put", "--addr", addr, "--key-file", other, source))
	if _, err := fetchThroughDesktop(ctx, client, theirs); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "another key") {
		t.Errorf("fetching what another key sealed: %v", err)
	}
}

func TestTheNodesSealingKeyIsMadeOnceAndKept(t *testing.T) {
	file := filepath.Join(t.TempDir(), "private.key")
	first, err := sealingKey(file)
	if err != nil {
		t.Fatal(err)
	}
	again, err := sealingKey(file)
	if err != nil || again != first {
		t.Errorf("the key the second time: %v, %v", again, err)
	}
	if info, err := os.Stat(file); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("the key file: %v, %v", info, err)
	}
	if err := os.WriteFile(file, []byte("not a key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := sealingKey(file); err == nil {
		t.Error("a file that is not a key was taken for one")
	}
	if _, err := sealingKey(filepath.Join(t.TempDir(), "no", "such", "dir", "private.key")); err == nil || !strings.Contains(err.Error(), "create key file") {
		t.Errorf("a key that cannot be saved: %v", err)
	}
}

// A machine with a node already running joins a friend's pool from its
// desktop client, and without being restarted is working for it.
func TestTheDesktopJoinsAnotherNodesPool(t *testing.T) {
	theirs, theirAPI := freeAddr(t), freeAddr(t)
	theirDir := t.TempDir()
	startDaemon(t, "--data-dir", theirDir, "--listen", theirs, "--name", "theirs", "--api-listen", theirAPI)
	friend := desktop(t, theirAPI)
	poolAsSeenBy(t, friend)

	mine, myAPI := freeAddr(t), freeAddr(t)
	myDir := t.TempDir()
	startDaemon(t, "--data-dir", myDir, "--listen", mine, "--name", "mine", "--api-listen", myAPI)
	client := desktop(t, myAPI)
	poolAsSeenBy(t, client)
	ctx := tokenOf(t, myDir)

	invited, err := friend.CreateInvitation(tokenOf(t, theirDir), &nodepb.CreateInvitationRequest{Role: nodepb.PoolRole_POOL_ROLE_WORKER})
	if err != nil {
		t.Fatal(err)
	}
	joined, err := client.JoinPool(ctx, &nodepb.JoinPoolRequest{Address: theirs, Invitation: invited.GetInvitation()})
	if err != nil {
		t.Fatal(err)
	}
	if joined.GetPeerId() != nodeID(t, theirDir) || joined.GetRole() != nodepb.PoolRole_POOL_ROLE_WORKER {
		t.Errorf("joined: %v", joined)
	}
	// Their pool gains a worker: this node, under its own name.
	waitFor(t, func() bool {
		workers, err := friend.ListWorkers(context.Background(), &nodepb.ListWorkersRequest{})
		if err != nil {
			return false
		}
		for _, w := range workers.GetWorkers() {
			if w.GetPeerId() == nodeID(t, myDir) {
				return true
			}
		}
		return false
	})
	// And this node shows them as a peer it takes work from and is working for.
	waitFor(t, func() bool {
		peer := peerSeenBy(t, client, nodeID(t, theirDir))
		return peer.GetTakesWork() && peer.GetThisNodeWorksFor()
	})
	// The address is in the book, to be found again after a restart.
	book, err := client.GetBootstrapPeers(ctx, &nodepb.GetBootstrapPeersRequest{})
	if err != nil || len(book.GetPeers()) != 1 || book.GetPeers()[0].GetPeerId() != nodeID(t, theirDir) {
		t.Errorf("the address book: %v, %v", book, err)
	}

	// An invitation works once.
	if _, err := client.JoinPool(ctx, &nodepb.JoinPoolRequest{Address: theirs, Invitation: invited.GetInvitation()}); status.Code(err) != codes.PermissionDenied && status.Code(err) != codes.InvalidArgument {
		t.Errorf("joining twice with one invitation: %v", err)
	}
	if _, err := client.JoinPool(ctx, &nodepb.JoinPoolRequest{Address: theirs}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("joining with no invitation: %v, want InvalidArgument", err)
	}
	if _, err := client.JoinPool(context.Background(), &nodepb.JoinPoolRequest{Address: theirs, Invitation: "x"}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("joining without the token: %v, want PermissionDenied", err)
	}
}

// closedList is a list of nodes worked for that cannot be written to.
type closedList struct{}

func (closedList) Set(string, bool) error { return errors.New("the list is closed") }
func (closedList) List() []string         { return nil }

// taking is a list of nodes worked for, kept in memory.
type taking struct{ ids []string }

func (w *taking) Set(id string, _ bool) error { w.ids = append(w.ids, id); return nil }
func (w *taking) List() []string              { return w.ids }

// unkept is an address book that keeps nothing.
type unkept struct{ added []nodedb.BootstrapPeer }

func (f *unkept) BootstrapPeers() ([]nodedb.BootstrapPeer, error) { return nil, nil }
func (f *unkept) SetBootstrapPeers([]nodedb.BootstrapPeer) error  { return nil }
func (f *unkept) AddBootstrapPeer(p nodedb.BootstrapPeer) error {
	f.added = append(f.added, p)
	return nil
}

func TestJoiningFromTheDesktopWhenPartOfItFails(t *testing.T) {
	addr := freeAddr(t)
	startDaemon(t, "--role", "coordinator", "--listen", addr)
	ctx := context.Background()
	newcomer := func() (string, *identity.Identity) {
		dir := t.TempDir()
		ident, _, err := identity.LoadOrCreate(filepath.Join(dir, "node.key"))
		if err != nil {
			t.Fatal(err)
		}
		return dir, ident
	}
	connects := func(context.Context, string) error { return nil }
	looks := 0
	looked := func() { looks++ }

	// An invitation that is not one.
	dir, ident := newcomer()
	if _, _, err := joinFromDesktop(dir, ident, connects, &unkept{}, closedList{}, looked)(ctx, addr, "not an invitation"); err == nil || !strings.Contains(err.Error(), "an invitation looks like") {
		t.Errorf("a bad invitation: %v", err)
	}

	// Joined, but the other node's host cannot be reached.
	unreachable := func(context.Context, string) error { return errors.New("no route") }
	if _, _, err := joinFromDesktop(dir, ident, unreachable, &unkept{}, &taking{}, looked)(ctx, addr, invite(t, addr, "worker")); err == nil || !strings.Contains(err.Error(), "could not connect to it") {
		t.Errorf("joining a node that cannot be connected to: %v", err)
	}

	// Joined and connected: the node takes its work, and knows where it is.
	dir, ident = newcomer()
	book, list := &unkept{}, &taking{}
	if looks != 0 {
		t.Errorf("the node looked for work %d times before it had joined anything", looks)
	}
	id, role, err := joinFromDesktop(dir, ident, connects, book, list, looked)(ctx, addr, invite(t, addr, "worker"))
	if err != nil || looks != 1 || role != access.Worker || len(list.ids) != 1 || list.ids[0] != id || len(book.added) != 1 || book.added[0].PeerID != id || !strings.HasSuffix(book.added[0].Address, "/p2p/"+id) {
		t.Errorf("joining as a worker: %q, %q, %v; takes work from %v, noted %v", id, role, err, list.ids, book.added)
	}

	// Joined and connected, but the node cannot record whom it works for.
	dir, ident = newcomer()
	if _, _, err := joinFromDesktop(dir, ident, connects, &unkept{}, closedList{}, looked)(ctx, addr, invite(t, addr, "worker")); err == nil || !strings.Contains(err.Error(), "the list is closed") {
		t.Errorf("joining when the list cannot be written: %v", err)
	}

	// Invited as a client, the node joins and takes no work.
	dir, ident = newcomer()
	if id, role, err := joinFromDesktop(dir, ident, connects, &unkept{}, closedList{}, looked)(ctx, addr, invite(t, addr, "client")); err != nil || role != access.Client || id == "" {
		t.Errorf("joining as a client: %q, %q, %v", id, role, err)
	}
}
