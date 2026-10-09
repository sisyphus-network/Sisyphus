package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ipfs/go-cid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/access"
	"github.com/sisyphus-network/Sisyphus/packages/nodedb"
	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
	"github.com/sisyphus-network/Sisyphus/packages/sealed"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// serveLocal serves the local API of a node described by cfg, whose token
// is "the-token".
func serveLocal(t *testing.T, cfg LocalConfig) nodepb.NodeServiceClient {
	t.Helper()
	cfg.NodeID, cfg.Token = "12D3KooWnode", "the-token"
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := NewLocalServer(cfg)
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return nodepb.NewNodeServiceClient(conn)
}

func newFileList(t *testing.T) *nodedb.DB {
	t.Helper()
	db, err := nodedb.Open(filepath.Join(t.TempDir(), "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// storeFile sends a file to the node in the given pieces.
func storeFile(ctx context.Context, client nodepb.NodeServiceClient, name string, pieces ...string) (*nodepb.File, error) {
	stream, err := client.StoreFile(ctx)
	if err != nil {
		return nil, err
	}
	for i, piece := range pieces {
		msg := &nodepb.StoreFileRequest{Data: []byte(piece)}
		if i == 0 {
			msg.Name = name
		}
		if err := stream.Send(msg); err != nil {
			break // the answer says why
		}
	}
	return stream.CloseAndRecv()
}

// notStored is the CID of something no store in these tests holds.
func notStored(t *testing.T) string {
	t.Helper()
	_, id := storeWithBlob(t)
	return id
}

// fetchFile reads everything stored under a CID.
func fetchFile(ctx context.Context, client nodepb.NodeServiceClient, id string) ([]byte, error) {
	stream, err := client.FetchFile(ctx, &nodepb.FetchFileRequest{Cid: id})
	if err != nil {
		return nil, err
	}
	var got bytes.Buffer
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return got.Bytes(), nil
		}
		if err != nil {
			return nil, err
		}
		got.Write(msg.GetData())
	}
}

func TestFilesAreStoredListedFetchedAndRemoved(t *testing.T) {
	store := storage.NewMemory()
	client := serveLocal(t, LocalConfig{Store: store, Files: newFileList(t)})
	ctx := withToken("the-token")

	before := time.Now()
	notes, err := storeFile(ctx, client, "notes.txt", "to be or ", "not to be")
	if err != nil {
		t.Fatal(err)
	}
	if notes.GetName() != "notes.txt" || notes.GetSizeBytes() != 18 || notes.GetCid() == "" || notes.GetStoredAtMs() < before.UnixMilli() {
		t.Errorf("the stored file: %v", notes)
	}
	// It is held for the user until removed, not for the short while
	// anything newly stored is.
	held := false
	for _, pin := range store.Pins() {
		held = held || (pin.CID.String() == notes.GetCid() && pin.Owner == userOwner && pin.Expires.IsZero())
	}
	if !held {
		t.Errorf("the file is not held for the user: %v", store.Pins())
	}
	// A file may be empty, and need not be named.
	empty, err := storeFile(ctx, client, "")
	if err != nil || empty.GetSizeBytes() != 0 || empty.GetName() != "" {
		t.Fatalf("an empty file: %v, %v", empty, err)
	}

	// Listing needs no token; the newest is first.
	listed, err := client.ListFiles(context.Background(), &nodepb.ListFilesRequest{})
	if err != nil || len(listed.GetFiles()) != 2 || listed.GetFiles()[1].GetCid() != notes.GetCid() || listed.GetFiles()[1].GetName() != "notes.txt" {
		t.Fatalf("the files listed: %v, %v", listed, err)
	}

	got, err := fetchFile(ctx, client, notes.GetCid())
	if err != nil || string(got) != "to be or not to be" {
		t.Errorf("the file fetched: %q, %v", got, err)
	}
	if _, err := fetchFile(ctx, client, notStored(t)); status.Code(err) != codes.NotFound {
		t.Errorf("fetching what is not stored: %v, want NotFound", err)
	}

	if _, err := client.RemoveFile(ctx, &nodepb.RemoveFileRequest{Cid: notes.GetCid()}); err != nil {
		t.Fatal(err)
	}
	listed, err = client.ListFiles(ctx, &nodepb.ListFilesRequest{})
	if err != nil || len(listed.GetFiles()) != 1 || listed.GetFiles()[0].GetCid() != empty.GetCid() {
		t.Errorf("the files after removing one: %v, %v", listed, err)
	}
	for _, pin := range store.Pins() {
		if pin.CID.String() == notes.GetCid() && pin.Owner == userOwner {
			t.Error("a removed file is still held for the user")
		}
	}
	if _, err := client.RemoveFile(ctx, &nodepb.RemoveFileRequest{Cid: "not a CID"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("removing something that is not a CID: %v, want InvalidArgument", err)
	}
}

func TestReferencedFilesStayPinnedUntilEveryOwnerReleasesThem(t *testing.T) {
	store := storage.NewMemory()
	db := newFileList(t)
	client := serveLocal(t, LocalConfig{Store: store, Files: db})
	ctx := withToken("the-token")
	file, err := storeFile(ctx, client, "shared.txt", "shared attachment")
	if err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{"chat:first", "chat:second"} {
		if err := db.AddFileReference(file.GetCid(), owner); err != nil {
			t.Fatal(err)
		}
	}
	assertRetained := func() {
		t.Helper()
		if _, err := client.RemoveFile(ctx, &nodepb.RemoveFileRequest{Cid: file.GetCid()}); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("removing a referenced file: %v, want rejection", err)
		}
		listed, err := client.ListFiles(ctx, &nodepb.ListFilesRequest{})
		if err != nil || len(listed.GetFiles()) != 1 || listed.GetFiles()[0].GetCid() != file.GetCid() {
			t.Fatalf("referenced file lost from catalogue: %v, %v", listed, err)
		}
		held := false
		for _, pin := range store.Pins() {
			held = held || (pin.CID.String() == file.GetCid() && pin.Owner == userOwner && pin.Expires.IsZero())
		}
		if !held {
			t.Fatal("rejected removal released the storage pin")
		}
		got, err := fetchFile(ctx, client, file.GetCid())
		if err != nil || string(got) != "shared attachment" {
			t.Fatalf("referenced file cannot be fetched: %q, %v", got, err)
		}
	}
	assertRetained()
	if err := db.RemoveFileReference(file.GetCid(), "chat:first"); err != nil {
		t.Fatal(err)
	}
	assertRetained()
	if err := db.RemoveFileReference(file.GetCid(), "chat:second"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.RemoveFile(ctx, &nodepb.RemoveFileRequest{Cid: file.GetCid()}); err != nil {
		t.Fatal(err)
	}
	for _, pin := range store.Pins() {
		if pin.CID.String() == file.GetCid() && pin.Owner == userOwner {
			t.Fatal("unreferenced file remains pinned after explicit removal")
		}
	}
}

func TestFilesNeedTheTokenAndAStore(t *testing.T) {
	calls := func(ctx context.Context, client nodepb.NodeServiceClient) map[string]error {
		_, stored := storeFile(ctx, client, "a", "data")
		_, fetched := fetchFile(ctx, client, notStored(t))
		_, removed := client.RemoveFile(ctx, &nodepb.RemoveFileRequest{Cid: notStored(t)})
		return map[string]error{"StoreFile": stored, "FetchFile": fetched, "RemoveFile": removed}
	}
	withStore := serveLocal(t, LocalConfig{Store: storage.NewMemory(), Files: newFileList(t)})
	for name, err := range calls(context.Background(), withStore) {
		if status.Code(err) != codes.PermissionDenied {
			t.Errorf("%s without the token: %v, want PermissionDenied", name, err)
		}
	}
	without := serveLocal(t, LocalConfig{})
	for name, err := range calls(withToken("the-token"), without) {
		if status.Code(err) != codes.FailedPrecondition {
			t.Errorf("%s on a node with no store: %v, want FailedPrecondition", name, err)
		}
	}
	if _, err := without.ListFiles(context.Background(), &nodepb.ListFilesRequest{}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("ListFiles on a node with no store: %v, want FailedPrecondition", err)
	}
}

// brokenFiles is a list of files that cannot be read or written.
type brokenFiles struct{}

func (brokenFiles) Files() ([]nodedb.File, error) { return nil, errList }
func (brokenFiles) AddFile(nodedb.File) error     { return errList }
func (brokenFiles) RemoveFile(string) error       { return errList }

func TestFileFailuresAreReported(t *testing.T) {
	ctx := withToken("the-token")
	stored := notStored(t)

	full := serveLocal(t, LocalConfig{Store: brokenStore{Store: storage.NewMemory(), used: 100}, MaxStoreBytes: 100, Files: newFileList(t)})
	if _, err := storeFile(ctx, full, "a", "data"); status.Code(err) != codes.ResourceExhausted {
		t.Errorf("storing in a full store: %v, want ResourceExhausted", err)
	}

	unpinnable := serveLocal(t, LocalConfig{Store: brokenStore{Store: storage.NewMemory(), failPin: true}, Files: newFileList(t)})
	if _, err := storeFile(ctx, unpinnable, "a", "data"); status.Code(err) != codes.Internal || !strings.Contains(err.Error(), "keep file") {
		t.Errorf("storing what cannot be kept: %v", err)
	}
	if _, err := unpinnable.RemoveFile(ctx, &nodepb.RemoveFileRequest{Cid: stored}); status.Code(err) != codes.Internal || !strings.Contains(err.Error(), "stop keeping file") {
		t.Errorf("removing what cannot be released: %v", err)
	}

	unlisted := serveLocal(t, LocalConfig{Store: storage.NewMemory(), Files: brokenFiles{}})
	if _, err := storeFile(ctx, unlisted, "a", "data"); status.Code(err) != codes.Internal || !strings.Contains(err.Error(), errList.Error()) {
		t.Errorf("storing what cannot be listed: %v", err)
	}
	if _, err := unlisted.ListFiles(ctx, &nodepb.ListFilesRequest{}); status.Code(err) != codes.Internal {
		t.Errorf("listing from a broken list: %v", err)
	}
	if _, err := unlisted.RemoveFile(ctx, &nodepb.RemoveFileRequest{Cid: stored}); status.Code(err) != codes.Internal || !strings.Contains(err.Error(), errList.Error()) {
		t.Errorf("removing from a broken list: %v", err)
	}
}

func TestInvitationsAndMembersThroughTheLocalAPI(t *testing.T) {
	list := newList(t)
	ident := newIdentity(t)
	client := serveLocal(t, LocalConfig{Pool: NewPoolAdmin(Config{Identity: ident, Access: list, Coordinator: newCoordinator(t)})})
	ctx := withToken("the-token")
	const worker = "12D3KooWAMv5mPojCf7tz1PH9F3VCPzqCyc8t2onRa6ihxoPh7TK"

	before := time.Now()
	invited, err := client.CreateInvitation(ctx, &nodepb.CreateInvitationRequest{Role: nodepb.PoolRole_POOL_ROLE_WORKER})
	if err != nil {
		t.Fatal(err)
	}
	// It is this node's ID and a token, good for an hour unless said otherwise.
	id, token, _ := strings.Cut(invited.GetInvitation(), ":")
	if id != ident.ID() || token == "" {
		t.Fatalf("the invitation: %q", invited.GetInvitation())
	}
	if lasts := time.UnixMilli(invited.GetExpiresAtMs()).Sub(before); lasts < 59*time.Minute || lasts > 61*time.Minute {
		t.Errorf("the invitation lasts %v, want an hour", lasts)
	}
	if role, err := list.Redeem(token, worker, time.Now()); err != nil || role != access.Worker {
		t.Fatalf("using the invitation: %q, %v", role, err)
	}
	brief, err := client.CreateInvitation(ctx, &nodepb.CreateInvitationRequest{Role: nodepb.PoolRole_POOL_ROLE_CLIENT, TtlSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	if lasts := time.UnixMilli(brief.GetExpiresAtMs()).Sub(before); lasts > 2*time.Minute {
		t.Errorf("an invitation for a minute lasts %v", lasts)
	}
	if _, err := client.CreateInvitation(ctx, &nodepb.CreateInvitationRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("an invitation to no role: %v, want InvalidArgument", err)
	}

	// Listing needs no token.
	members, err := client.ListMembers(context.Background(), &nodepb.ListMembersRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var found *nodepb.PoolMember
	for _, m := range members.GetMembers() {
		if m.GetPeerId() == worker {
			found = m
		}
	}
	if found == nil || found.GetRole() != nodepb.PoolRole_POOL_ROLE_WORKER || found.GetJoinedAtMs() < before.UnixMilli() {
		t.Errorf("the members: %v", members)
	}

	if _, err := client.RemoveMember(ctx, &nodepb.RemoveMemberRequest{PeerId: worker}); err != nil {
		t.Fatal(err)
	}
	if _, member := list.Role(worker); member {
		t.Error("a removed node is still a member")
	}
	if _, err := client.RemoveMember(ctx, &nodepb.RemoveMemberRequest{PeerId: worker}); status.Code(err) != codes.NotFound {
		t.Errorf("removing a node that is not a member: %v, want NotFound", err)
	}
	if _, err := client.RemoveMember(ctx, &nodepb.RemoveMemberRequest{PeerId: "not-a-node-id"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("removing something that is not a node ID: %v, want InvalidArgument", err)
	}
}

func TestMembershipNeedsTheTokenAndAPool(t *testing.T) {
	calls := func(ctx context.Context, client nodepb.NodeServiceClient) map[string]error {
		_, invited := client.CreateInvitation(ctx, &nodepb.CreateInvitationRequest{Role: nodepb.PoolRole_POOL_ROLE_WORKER})
		_, removed := client.RemoveMember(ctx, &nodepb.RemoveMemberRequest{PeerId: "12D3KooWAMv5mPojCf7tz1PH9F3VCPzqCyc8t2onRa6ihxoPh7TK"})
		return map[string]error{"CreateInvitation": invited, "RemoveMember": removed}
	}
	pool := serveLocal(t, LocalConfig{Pool: NewPoolAdmin(Config{Identity: newIdentity(t), Access: newList(t), Coordinator: newCoordinator(t)})})
	for name, err := range calls(context.Background(), pool) {
		if status.Code(err) != codes.PermissionDenied {
			t.Errorf("%s without the token: %v, want PermissionDenied", name, err)
		}
	}
	without := serveLocal(t, LocalConfig{})
	for name, err := range calls(withToken("the-token"), without) {
		if status.Code(err) != codes.FailedPrecondition {
			t.Errorf("%s on a node with no pool: %v, want FailedPrecondition", name, err)
		}
	}
	if _, err := without.ListMembers(context.Background(), &nodepb.ListMembersRequest{}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("ListMembers on a node with no pool: %v, want FailedPrecondition", err)
	}
}

// fromDesktop is the context of a call that arrived with the node's token.
func fromDesktop() context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer the-token"))
}

// brokenUpload is an upload that fails before anything arrives.
type brokenUpload struct{ grpc.ServerStream }

func (brokenUpload) Context() context.Context                { return fromDesktop() }
func (brokenUpload) Recv() (*nodepb.StoreFileRequest, error) { return nil, errDisk }
func (brokenUpload) SendAndClose(*nodepb.File) error         { return nil }

// fetched is where a FetchFile sends, for a call made directly.
type fetched struct{ grpc.ServerStream }

func (fetched) Context() context.Context             { return fromDesktop() }
func (fetched) Send(*nodepb.FetchFileResponse) error { return nil }

// unseekable is a store whose blobs cannot be read from the start again.
type unseekable struct{ *storage.Store }

func (u unseekable) Open(ctx context.Context, c cid.Cid) (storage.Blob, error) {
	blob, err := u.Store.Open(ctx, c)
	return stuckBlob{blob}, err
}

type stuckBlob struct{ storage.Blob }

func (stuckBlob) Seek(int64, int) (int64, error) { return 0, errDisk }

func TestPrivateFilesNeedAKeyThatCanBeHad(t *testing.T) {
	ctx := withToken("the-token")
	store := storage.NewMemory()

	// A node with no key to seal with stores what is not private, and
	// refuses what is.
	keyless := serveLocal(t, LocalConfig{Store: store, Files: newFileList(t), Jobs: &office{}})
	private := func(client nodepb.NodeServiceClient) error {
		stream, err := client.StoreFile(ctx)
		if err != nil {
			return err
		}
		stream.Send(&nodepb.StoreFileRequest{Name: "a", Private: true, Data: []byte("data")})
		_, err = stream.CloseAndRecv()
		return err
	}
	if err := private(keyless); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "keeps no key") {
		t.Errorf("a private file on a node with no key: %v", err)
	}
	if _, err := keyless.SubmitJob(ctx, &nodepb.SubmitJobRequest{Workload: "primes", Private: true}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("a private job on a node with no key: %v, want FailedPrecondition", err)
	}

	// One whose key cannot be read says so.
	lost := serveLocal(t, LocalConfig{Store: store, Files: newFileList(t), SealingKey: func() (sealed.Key, error) { return sealed.Key{}, errDisk }})
	if err := private(lost); status.Code(err) != codes.Internal || !strings.Contains(err.Error(), "sealing key") {
		t.Errorf("a private file when the key cannot be read: %v", err)
	}

	// Something sealed is not opened without a key either, and something
	// that only begins like a sealed blob is not opened at all.
	key := sealed.NewKey()
	c, err := store.Put(context.Background(), sealed.Encrypt(key, strings.NewReader("a secret")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fetchFile(ctx, keyless, c.String()); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "keeps no key") {
		t.Errorf("fetching a sealed blob on a node with no key: %v", err)
	}
	keyed := serveLocal(t, LocalConfig{Store: store, Files: newFileList(t), SealingKey: func() (sealed.Key, error) { return key, nil }})
	if got, err := fetchFile(ctx, keyed, c.String()); err != nil || string(got) != "a secret" {
		t.Errorf("fetching a sealed blob with its key: %q, %v", got, err)
	}
	if err := private(keyed); err != nil {
		t.Errorf("a private file on a node with a key: %v", err)
	}
	listed, err := keyed.ListFiles(ctx, &nodepb.ListFilesRequest{})
	if err != nil || len(listed.GetFiles()) != 1 || !listed.GetFiles()[0].GetPrivate() || listed.GetFiles()[0].GetSizeBytes() != 4 {
		t.Fatalf("the private file listed: %v, %v", listed, err)
	}
	if got, err := fetchFile(ctx, keyed, listed.GetFiles()[0].GetCid()); err != nil || string(got) != "data" {
		t.Errorf("the private file fetched: %q, %v", got, err)
	}
	stub, err := store.Put(context.Background(), strings.NewReader("SISYENC1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fetchFile(ctx, keyed, stub.String()); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "not that of any sealed blob") {
		t.Errorf("fetching the mere beginning of a sealed blob: %v", err)
	}
}

func TestFileStreamsThatBreakAreReported(t *testing.T) {
	store, id := storeWithBlob(t)
	service := &localService{cfg: LocalConfig{Token: "the-token", Store: store, Files: newFileList(t)}}
	if err := service.StoreFile(brokenUpload{}); !errors.Is(err, errDisk) {
		t.Errorf("an upload that fails at once: %v", err)
	}
	service.cfg.Store = unseekable{store}
	if err := service.FetchFile(&nodepb.FetchFileRequest{Cid: id}, fetched{}); status.Code(err) != codes.Internal || !strings.Contains(err.Error(), "disk on fire") {
		t.Errorf("fetching a blob that cannot be read from its start: %v", err)
	}
}

func TestJoiningAPoolThroughTheLocalAPI(t *testing.T) {
	ctx := withToken("the-token")
	req := &nodepb.JoinPoolRequest{Address: "rig.example.net:7700", Invitation: "12D3KooWinviter:token"}

	if _, err := serveLocal(t, LocalConfig{}).JoinPool(ctx, req); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "runs no worker") {
		t.Errorf("joining from a node that runs no worker: %v", err)
	}

	var asked []string
	joins := serveLocal(t, LocalConfig{Join: func(_ context.Context, address, invitation string) (string, access.Role, error) {
		asked = append(asked, address, invitation)
		if invitation == "spent" {
			return "", "", status.Error(codes.PermissionDenied, "that invitation has been used")
		}
		if invitation == "unlucky" {
			return "", "", errDisk
		}
		return "12D3KooWinviter", access.Client, nil
	}})
	joined, err := joins.JoinPool(ctx, req)
	if err != nil || joined.GetPeerId() != "12D3KooWinviter" || joined.GetRole() != nodepb.PoolRole_POOL_ROLE_CLIENT || len(asked) != 2 || asked[0] != req.GetAddress() || asked[1] != req.GetInvitation() {
		t.Errorf("joining: %v, %v, having asked %v", joined, err, asked)
	}
	// What the other node said is passed on as it said it.
	if _, err := joins.JoinPool(ctx, &nodepb.JoinPoolRequest{Address: "a:1", Invitation: "spent"}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("joining with a used invitation: %v, want PermissionDenied", err)
	}
	if _, err := joins.JoinPool(ctx, &nodepb.JoinPoolRequest{Address: "a:1", Invitation: "unlucky"}); status.Code(err) != codes.Internal {
		t.Errorf("joining when it fails here: %v, want Internal", err)
	}
	for _, incomplete := range []*nodepb.JoinPoolRequest{{Address: "a:1"}, {Invitation: "x"}} {
		if _, err := joins.JoinPool(ctx, incomplete); status.Code(err) != codes.InvalidArgument {
			t.Errorf("joining with %v: %v, want InvalidArgument", incomplete, err)
		}
	}
	if _, err := joins.JoinPool(context.Background(), req); status.Code(err) != codes.PermissionDenied {
		t.Errorf("joining without the token: %v, want PermissionDenied", err)
	}
}
