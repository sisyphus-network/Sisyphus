package api

import (
	"context"
	"errors"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/access"
	"github.com/sisyphus-network/Sisyphus/packages/nodedb"
	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
)

// localNode is a node as its desktop client sees it.
type localNode struct {
	t      *testing.T
	client nodepb.NodeServiceClient
	list   *access.List

	mu    sync.Mutex
	peers []*nodepb.Peer
}

func (n *localNode) setPeers(peers ...*nodepb.Peer) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.peers = peers
}

// startLocal serves the local API for a node. With coordinating set the node
// runs a pool whose membership the API can change.
func startLocal(t *testing.T, coordinating bool) *localNode {
	t.Helper()
	n := &localNode{t: t}
	cfg := LocalConfig{
		NodeID: "12D3KooWnode", Version: "v1.2.3", Token: "the-token", Poll: 5 * time.Millisecond,
		Listen: func() []string { return []string{"/ip4/10.0.0.5/tcp/7700"} },
		Peers: func() []*nodepb.Peer {
			n.mu.Lock()
			defer n.mu.Unlock()
			return n.peers
		},
	}
	if coordinating {
		n.list = newList(t)
		cfg.Pool = NewPoolAdmin(Config{Identity: newIdentity(t), Access: n.list, Coordinator: newCoordinator(t)})
	}
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
	n.client = nodepb.NewNodeServiceClient(conn)
	return n
}

// withToken is a context that presents the node's token.
func withToken(token string) context.Context {
	return metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+token)
}

func peerEntry(id string, connected, trusted bool) *nodepb.Peer {
	state := nodepb.PeerConnectionState_PEER_CONNECTION_STATE_DISCONNECTED
	if connected {
		state = nodepb.PeerConnectionState_PEER_CONNECTION_STATE_CONNECTED
	}
	return &nodepb.Peer{PeerId: id, ConnectionState: state, TrustedForCompute: trusted}
}

func TestLocalAPIDescribesTheNode(t *testing.T) {
	n := startLocal(t, false)
	ctx := context.Background()

	info, err := n.client.GetNodeInfo(ctx, &nodepb.GetNodeInfoRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if info.GetPeerId() != "12D3KooWnode" || info.GetDaemonVersion() != "v1.2.3" || len(info.GetListenAddresses()) != 1 || info.GetListenAddresses()[0] != "/ip4/10.0.0.5/tcp/7700" {
		t.Errorf("node info: %v", info)
	}
	// The node does not look up where in the world it is.
	if info.GetCountryCode() != "" {
		t.Errorf("country code %q", info.GetCountryCode())
	}

	n.setPeers(peerEntry("12D3KooWa", true, true), peerEntry("12D3KooWb", false, false))
	listed, err := n.client.ListPeers(ctx, &nodepb.ListPeersRequest{})
	if err != nil || len(listed.GetPeers()) != 2 || listed.GetPeers()[0].GetPeerId() != "12D3KooWa" || !listed.GetPeers()[0].GetTrustedForCompute() {
		t.Errorf("ListPeers = %v, %v", listed, err)
	}
	if book, err := n.client.GetBootstrapPeers(ctx, &nodepb.GetBootstrapPeersRequest{}); err != nil || len(book.GetPeers()) != 0 {
		t.Errorf("GetBootstrapPeers = %v, %v; want an empty address book", book, err)
	}
}

func TestWatchPeersSendsAFullListWheneverItChanges(t *testing.T) {
	n := startLocal(t, false)
	n.setPeers(peerEntry("12D3KooWa", false, true))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := n.client.WatchPeers(ctx, &nodepb.WatchPeersRequest{})
	if err != nil {
		t.Fatal(err)
	}
	next := func() *nodepb.ListPeersResponse {
		t.Helper()
		update, err := stream.Recv()
		if err != nil {
			t.Fatal(err)
		}
		return update
	}

	first := next()
	if first.GetRevision() != 1 || len(first.GetPeers()) != 1 || first.GetPeers()[0].GetConnectionState() != nodepb.PeerConnectionState_PEER_CONNECTION_STATE_DISCONNECTED {
		t.Fatalf("first update: %v", first)
	}
	// Several polls pass with nothing new, and nothing is sent. Then the
	// peer connects.
	time.Sleep(30 * time.Millisecond)
	n.setPeers(peerEntry("12D3KooWa", true, true))
	second := next()
	if second.GetRevision() != 2 || second.GetPeers()[0].GetConnectionState() != nodepb.PeerConnectionState_PEER_CONNECTION_STATE_CONNECTED {
		t.Errorf("after the peer connected: %v", second)
	}
	n.setPeers(peerEntry("12D3KooWa", true, true), peerEntry("12D3KooWb", true, true))
	if third := next(); third.GetRevision() != 3 || len(third.GetPeers()) != 2 {
		t.Errorf("after a second peer appeared: %v", third)
	}

	// A watcher that goes away ends the watch.
	cancel()
	if _, err := stream.Recv(); status.Code(err) != codes.Canceled {
		t.Errorf("after cancelling: %v", err)
	}
}

func TestChangingTrustAdmitsAndRemovesWorkers(t *testing.T) {
	n := startLocal(t, true)
	const peer = "12D3KooWAMv5mPojCf7tz1PH9F3VCPzqCyc8t2onRa6ihxoPh7TK"
	ctx := withToken("the-token")

	got, err := n.client.SetPeerComputeTrust(ctx, &nodepb.SetPeerComputeTrustRequest{PeerId: peer, Trusted: true})
	if err != nil || !got.GetTrusted() {
		t.Fatalf("trusting a peer: %v, %v", got, err)
	}
	if role, ok := n.list.Role(peer); !ok || role != access.Worker {
		t.Errorf("a trusted peer has role %q (%v), want worker", role, ok)
	}

	got, err = n.client.SetPeerComputeTrust(ctx, &nodepb.SetPeerComputeTrustRequest{PeerId: peer, Trusted: false})
	if err != nil || got.GetTrusted() {
		t.Fatalf("ending trust in a peer: %v, %v", got, err)
	}
	if _, ok := n.list.Role(peer); ok {
		t.Error("a peer no longer trusted is still a member")
	}
	// Ending trust that is not there leaves things as they should be.
	if _, err := n.client.SetPeerComputeTrust(ctx, &nodepb.SetPeerComputeTrustRequest{PeerId: peer, Trusted: false}); err != nil {
		t.Errorf("ending trust in a stranger: %v", err)
	}
	if _, err := n.client.SetPeerComputeTrust(ctx, &nodepb.SetPeerComputeTrustRequest{PeerId: "not-a-node-id", Trusted: true}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("trusting something that is not a node ID: %v, want InvalidArgument", err)
	}
}

func TestChangesNeedTheNodesToken(t *testing.T) {
	n := startLocal(t, true)
	const peer = "12D3KooWAMv5mPojCf7tz1PH9F3VCPzqCyc8t2onRa6ihxoPh7TK"
	request := &nodepb.SetPeerComputeTrustRequest{PeerId: peer, Trusted: true}

	for name, ctx := range map[string]context.Context{
		"no token":              context.Background(),
		"another token":         withToken("a-guess"),
		"the token, unlabelled": metadata.AppendToOutgoingContext(context.Background(), "authorization", "the-token"),
	} {
		_, err := n.client.SetPeerComputeTrust(ctx, request)
		if status.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), "api.token") {
			t.Errorf("%s: %v, want a refusal that says where the token is", name, err)
		}
	}
	if _, ok := n.list.Role(peer); ok {
		t.Error("a refused change admitted the peer")
	}
	// Reading needs none.
	if _, err := n.client.ListPeers(context.Background(), &nodepb.ListPeersRequest{}); err != nil {
		t.Errorf("reading without a token: %v", err)
	}
}

func TestANodeThatCoordinatesNoPoolCannotChangeTrust(t *testing.T) {
	n := startLocal(t, false)
	_, err := n.client.SetPeerComputeTrust(withToken("the-token"), &nodepb.SetPeerComputeTrustRequest{PeerId: "12D3KooWAMv5mPojCf7tz1PH9F3VCPzqCyc8t2onRa6ihxoPh7TK", Trusted: true})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("error %v, want FailedPrecondition", err)
	}
}

// book is an address book kept in memory, which can be made to fail.
type book struct {
	entries []nodedb.BootstrapPeer
	err     error
}

func (b *book) BootstrapPeers() ([]nodedb.BootstrapPeer, error) { return b.entries, b.err }

func (b *book) SetBootstrapPeers(entries []nodedb.BootstrapPeer) error {
	if b.err == nil {
		b.entries = entries
	}
	return b.err
}

func (b *book) AddBootstrapPeer(entry nodedb.BootstrapPeer) error {
	b.entries = append(b.entries, entry)
	return nil
}

func TestTheAddressBookIsReadAndReplaced(t *testing.T) {
	const (
		id      = "12D3KooWAMv5mPojCf7tz1PH9F3VCPzqCyc8t2onRa6ihxoPh7TK"
		other   = "12D3KooWGMC9eNSqjbuxQN5gg7emLLsXsBxYAanwqcmCUALXtquU"
		address = "/ip4/10.0.0.9/tcp/7700/p2p/" + id
	)
	kept := &book{}
	started := make(chan []string, 1)
	service := &localService{cfg: LocalConfig{Token: "the-token", Book: kept, Bootstrap: func(_ context.Context, addresses []string) { started <- addresses }}}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer the-token"))
	entry := &nodepb.BootstrapPeer{PeerId: id, Address: address}

	set, err := service.SetBootstrapPeers(ctx, &nodepb.SetBootstrapPeersRequest{Peers: []*nodepb.BootstrapPeer{entry}})
	if err != nil || len(set.GetPeers()) != 1 || set.GetPeers()[0].GetAddress() != address {
		t.Fatalf("SetBootstrapPeers = %v, %v", set, err)
	}
	// The node goes looking for what it was just given.
	if dialled := <-started; len(dialled) != 1 || dialled[0] != address {
		t.Errorf("connected to %v", dialled)
	}
	got, err := service.GetBootstrapPeers(context.Background(), &nodepb.GetBootstrapPeersRequest{})
	if err != nil || len(got.GetPeers()) != 1 || got.GetPeers()[0].GetPeerId() != id {
		t.Errorf("GetBootstrapPeers = %v, %v", got, err)
	}

	for name, bad := range map[string]*nodepb.BootstrapPeer{
		"an address that is none":    {PeerId: id, Address: "10.0.0.9:7700"},
		"an address with no node ID": {PeerId: id, Address: "/ip4/10.0.0.9/tcp/7700"},
		"an address of another node": {PeerId: other, Address: address},
	} {
		_, err := service.SetBootstrapPeers(ctx, &nodepb.SetBootstrapPeersRequest{Peers: []*nodepb.BootstrapPeer{entry, bad}})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v, want InvalidArgument", name, err)
		}
	}
	if len(kept.entries) != 1 {
		t.Errorf("a refused replacement changed the book: %v", kept.entries)
	}
	// Replacing it changes the node, so it needs the token. Reading does not.
	if _, err := service.SetBootstrapPeers(context.Background(), &nodepb.SetBootstrapPeersRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("without the token: %v", err)
	}

	kept.err = errors.New("the disk is full")
	if _, err := service.SetBootstrapPeers(ctx, &nodepb.SetBootstrapPeersRequest{}); status.Code(err) != codes.Internal {
		t.Errorf("a book that cannot be saved: %v", err)
	}
	if _, err := service.GetBootstrapPeers(ctx, &nodepb.GetBootstrapPeersRequest{}); status.Code(err) != codes.Internal {
		t.Errorf("a book that cannot be read: %v", err)
	}
}

func TestANodeWithNoAddressBook(t *testing.T) {
	n := startLocal(t, true)
	got, err := n.client.GetBootstrapPeers(context.Background(), &nodepb.GetBootstrapPeersRequest{})
	if err != nil || len(got.GetPeers()) != 0 {
		t.Errorf("GetBootstrapPeers = %v, %v", got, err)
	}
	if _, err := n.client.SetBootstrapPeers(withToken("the-token"), &nodepb.SetBootstrapPeersRequest{}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("SetBootstrapPeers: %v, want FailedPrecondition", err)
	}
}

func TestANodeSaysWhichCountryItIsInOnlyIfItKnows(t *testing.T) {
	plain := &localService{cfg: LocalConfig{Listen: func() []string { return nil }, Country: func() string { return "NL" }}}
	if info, _ := plain.GetNodeInfo(context.Background(), &nodepb.GetNodeInfoRequest{}); info.GetCountryCode() != "NL" {
		t.Errorf("country code %q", info.GetCountryCode())
	}
}

func TestConnectPeerConnectsTheNodesHost(t *testing.T) {
	const address = "/ip4/10.0.0.9/tcp/7700/p2p/12D3KooWAMv5mPojCf7tz1PH9F3VCPzqCyc8t2onRa6ihxoPh7TK"
	var dialled []string
	var failing error
	service := &localService{cfg: LocalConfig{Token: "the-token", Connect: func(_ context.Context, address string) error {
		dialled = append(dialled, address)
		return failing
	}}}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer the-token"))

	got, err := service.ConnectPeer(ctx, &nodepb.ConnectPeerRequest{Address: address})
	if err != nil || got.GetPeerId() != "12D3KooWAMv5mPojCf7tz1PH9F3VCPzqCyc8t2onRa6ihxoPh7TK" || len(dialled) != 1 || dialled[0] != address {
		t.Fatalf("ConnectPeer = %v, %v, having dialled %v", got, err, dialled)
	}
	// And with an address book, the connection is noted in it.
	kept := &book{}
	service.cfg.Book = kept
	if _, err := service.ConnectPeer(ctx, &nodepb.ConnectPeerRequest{Address: address}); err != nil || len(kept.entries) != 1 || kept.entries[0].Address != address {
		t.Fatalf("connecting with an address book: %v, book %v", err, kept.entries)
	}
	failing = errors.New("nobody answers there")
	if _, err := service.ConnectPeer(ctx, &nodepb.ConnectPeerRequest{Address: address}); status.Code(err) != codes.Unavailable || !strings.Contains(err.Error(), "nobody answers") {
		t.Errorf("a connection that fails: %v", err)
	}
	for _, bad := range []string{"", "10.0.0.9:7700", "/ip4/10.0.0.9/tcp/7700"} {
		if _, err := service.ConnectPeer(ctx, &nodepb.ConnectPeerRequest{Address: bad}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("connecting to %q: %v, want InvalidArgument", bad, err)
		}
	}
	// It changes what the node is connected to, so it needs the token.
	if _, err := service.ConnectPeer(context.Background(), &nodepb.ConnectPeerRequest{Address: address}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("without the token: %v", err)
	}
	hostless := &localService{cfg: LocalConfig{Token: "the-token"}}
	if _, err := hostless.ConnectPeer(ctx, &nodepb.ConnectPeerRequest{Address: address}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("a node with no host: %v", err)
	}
}

func TestTrustChangeReportsAListThatCannotBeSaved(t *testing.T) {
	stuck := &localService{cfg: LocalConfig{Token: "the-token", Pool: &PoolAdmin{service: &poolService{access: stuckAdmit{newList(t)}}}}}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer the-token"))
	_, err := stuck.SetPeerComputeTrust(ctx, &nodepb.SetPeerComputeTrustRequest{PeerId: "12D3KooWAMv5mPojCf7tz1PH9F3VCPzqCyc8t2onRa6ihxoPh7TK", Trusted: true})
	if status.Code(err) != codes.Internal {
		t.Errorf("error %v, want Internal", err)
	}
}

// stuckAdmit is an access list that cannot record an admission.
type stuckAdmit struct{ accessList }

func (stuckAdmit) Admit(string, access.Role, time.Time) error { return errList }

// brokenWatch is a watcher's stream that cannot be sent to.
type brokenWatch struct {
	grpc.ServerStream
	ctx    context.Context
	failOn int
	sent   int
	// first, if set, is closed once the first update has been sent.
	first chan struct{}
}

func (b *brokenWatch) Context() context.Context { return b.ctx }

func (b *brokenWatch) Send(*nodepb.ListPeersResponse) error {
	if b.sent++; b.sent >= b.failOn {
		return status.Error(codes.Unavailable, "watcher went away")
	}
	close(b.first)
	return nil
}

func TestWatchPeersEndsWhenTheWatcherCannotBeSentTo(t *testing.T) {
	n := &localNode{}
	service := &localService{cfg: LocalConfig{Poll: time.Millisecond, Peers: func() []*nodepb.Peer {
		n.mu.Lock()
		defer n.mu.Unlock()
		return n.peers
	}}}
	// The first send fails.
	if err := service.WatchPeers(&nodepb.WatchPeersRequest{}, &brokenWatch{ctx: context.Background(), failOn: 1}); status.Code(err) != codes.Unavailable {
		t.Errorf("error %v, want the send failure", err)
	}
	// The first succeeds and the one after a change fails.
	watch := &brokenWatch{ctx: context.Background(), failOn: 2, first: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- service.WatchPeers(&nodepb.WatchPeersRequest{}, watch) }()
	<-watch.first
	n.setPeers(peerEntry("12D3KooWa", true, true))
	if err := <-done; status.Code(err) != codes.Unavailable {
		t.Errorf("error %v, want the send failure", err)
	}
}

// taking is a list of nodes to take work from, kept in memory, which can be
// made to fail.
type taking struct {
	ids []string
	err error
}

func (w *taking) List() []string { return w.ids }

func (w *taking) Set(id string, willing bool) error {
	if w.err != nil {
		return w.err
	}
	w.ids = slices.DeleteFunc(w.ids, func(other string) bool { return other == id })
	if willing {
		w.ids = append(w.ids, id)
	}
	return nil
}

func TestTheTwoSidesOfTrustAreSetSeparately(t *testing.T) {
	const peerID = "12D3KooWAMv5mPojCf7tz1PH9F3VCPzqCyc8t2onRa6ihxoPh7TK"
	list := newList(t)
	takes := &taking{}
	service := &localService{cfg: LocalConfig{
		Token: "the-token", WorkFor: takes,
		Pool: NewPoolAdmin(Config{Identity: newIdentity(t), Access: list, Coordinator: newCoordinator(t)}),
	}}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer the-token"))
	state := func() (gives, willing bool) {
		role, _ := list.Role(peerID)
		return role == access.Worker, slices.Contains(takes.ids, peerID)
	}
	set := func(gives, willing bool) {
		t.Helper()
		got, err := service.SetPeerComputePermissions(ctx, &nodepb.SetPeerComputePermissionsRequest{PeerId: peerID, GivesWork: gives, TakesWork: willing})
		if err != nil || got.GetGivesWork() != gives || got.GetTakesWork() != willing {
			t.Fatalf("SetPeerComputePermissions(%v, %v) = %v, %v", gives, willing, got, err)
		}
		if g, w := state(); g != gives || w != willing {
			t.Fatalf("after asking for gives=%v takes=%v the node gives=%v takes=%v", gives, willing, g, w)
		}
	}
	// Each side by itself, both, neither, and set again as it already is.
	set(true, false)
	set(false, true)
	set(true, true)
	set(true, true)
	set(false, false)
	set(false, false)

	// The one switch for both sets both, and clears both.
	if _, err := service.SetPeerComputeTrust(ctx, &nodepb.SetPeerComputeTrustRequest{PeerId: peerID, Trusted: true}); err != nil {
		t.Fatal(err)
	}
	if g, w := state(); !g || !w {
		t.Errorf("after trusting, gives=%v takes=%v", g, w)
	}
	if _, err := service.SetPeerComputeTrust(ctx, &nodepb.SetPeerComputeTrustRequest{PeerId: peerID, Trusted: false}); err != nil {
		t.Fatal(err)
	}
	if g, w := state(); g || w {
		t.Errorf("after ending trust, gives=%v takes=%v", g, w)
	}

	// A node admitted as a client is not thrown out for being given no work.
	const client = "12D3KooWGMC9eNSqjbuxQN5gg7emLLsXsBxYAanwqcmCUALXtquU"
	list.Admit(client, access.Client, time.Now())
	if _, err := service.SetPeerComputePermissions(ctx, &nodepb.SetPeerComputePermissionsRequest{PeerId: client}); err != nil {
		t.Fatal(err)
	}
	if role, _ := list.Role(client); role != access.Client {
		t.Errorf("a client given no work now has role %q", role)
	}

	for name, tt := range map[string]struct {
		ctx  context.Context
		req  *nodepb.SetPeerComputePermissionsRequest
		want codes.Code
	}{
		"without the token":         {context.Background(), &nodepb.SetPeerComputePermissionsRequest{PeerId: peerID, TakesWork: true}, codes.PermissionDenied},
		"something that is no node": {ctx, &nodepb.SetPeerComputePermissionsRequest{PeerId: "nope", TakesWork: true}, codes.InvalidArgument},
	} {
		if _, err := service.SetPeerComputePermissions(tt.ctx, tt.req); status.Code(err) != tt.want {
			t.Errorf("%s: %v, want %v", name, err, tt.want)
		}
	}
	takes.err = errors.New("the disk is full")
	if _, err := service.SetPeerComputePermissions(ctx, &nodepb.SetPeerComputePermissionsRequest{PeerId: peerID, TakesWork: true}); status.Code(err) != codes.Internal {
		t.Errorf("a list that cannot be saved: %v", err)
	}
}

func TestANodeSetsOnlyTheSidesOfTrustItHas(t *testing.T) {
	const peerID = "12D3KooWAMv5mPojCf7tz1PH9F3VCPzqCyc8t2onRa6ihxoPh7TK"
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer the-token"))

	// A node with a worker and no pool can take work and has none to give.
	takes := &taking{}
	workerOnly := &localService{cfg: LocalConfig{Token: "the-token", WorkFor: takes}}
	if _, err := workerOnly.SetPeerComputePermissions(ctx, &nodepb.SetPeerComputePermissionsRequest{PeerId: peerID, GivesWork: true}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("giving work from a node with no pool: %v", err)
	}
	// The one switch does what the node can: here, take.
	if _, err := workerOnly.SetPeerComputeTrust(ctx, &nodepb.SetPeerComputeTrustRequest{PeerId: peerID, Trusted: true}); err != nil || len(takes.ids) != 1 {
		t.Errorf("trusting from a node with no pool: %v, taking from %v", err, takes.ids)
	}

	// A node with a pool and no worker can give work and takes none.
	list := newList(t)
	coordinatorOnly := &localService{cfg: LocalConfig{Token: "the-token", Pool: NewPoolAdmin(Config{Identity: newIdentity(t), Access: list, Coordinator: newCoordinator(t)})}}
	if _, err := coordinatorOnly.SetPeerComputePermissions(ctx, &nodepb.SetPeerComputePermissionsRequest{PeerId: peerID, TakesWork: true}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("taking work on a node with no worker: %v", err)
	}
	if _, err := coordinatorOnly.SetPeerComputeTrust(ctx, &nodepb.SetPeerComputeTrustRequest{PeerId: peerID, Trusted: true}); err != nil {
		t.Errorf("trusting from a node with no worker: %v", err)
	}
	if role, _ := list.Role(peerID); role != access.Worker {
		t.Errorf("the trusted node's role is %q", role)
	}
}

func TestPoolServiceSetsWhomTheNodeWorksFor(t *testing.T) {
	const peerID = "12D3KooWAMv5mPojCf7tz1PH9F3VCPzqCyc8t2onRa6ihxoPh7TK"
	takes := &taking{}
	service := &poolService{access: newList(t), workFor: takes}
	ctx := context.Background()
	if _, err := service.SetWorkFor(ctx, &pb.SetWorkForRequest{NodeId: peerID, Willing: true}); err != nil {
		t.Fatal(err)
	}
	listed, err := service.ListMembers(ctx, &pb.ListMembersRequest{})
	if err != nil || !slices.Equal(listed.GetWorksFor(), []string{peerID}) {
		t.Errorf("ListMembers = %v, %v", listed, err)
	}
	if _, err := service.SetWorkFor(ctx, &pb.SetWorkForRequest{NodeId: peerID}); err != nil || len(takes.ids) != 0 {
		t.Errorf("giving it up: %v, still taking from %v", err, takes.ids)
	}
	if _, err := service.SetWorkFor(ctx, &pb.SetWorkForRequest{NodeId: "nope", Willing: true}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("something that is no node: %v", err)
	}
	takes.err = errors.New("the disk is full")
	if _, err := service.SetWorkFor(ctx, &pb.SetWorkForRequest{NodeId: peerID, Willing: true}); status.Code(err) != codes.Internal {
		t.Errorf("a list that cannot be saved: %v", err)
	}
	// A node that keeps no such list.
	without := &poolService{access: newList(t)}
	if _, err := without.SetWorkFor(ctx, &pb.SetWorkForRequest{NodeId: peerID, Willing: true}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("a node with no worker: %v", err)
	}
	if listed, err := without.ListMembers(ctx, &pb.ListMembersRequest{}); err != nil || len(listed.GetWorksFor()) != 0 {
		t.Errorf("ListMembers on a node with no worker: %v, %v", listed, err)
	}
}

func TestEndingTheGivingOfWorkReportsAListThatCannotBeSaved(t *testing.T) {
	const peerID = "12D3KooWAMv5mPojCf7tz1PH9F3VCPzqCyc8t2onRa6ihxoPh7TK"
	list := newList(t)
	list.Admit(peerID, access.Worker, time.Now())
	service := &localService{cfg: LocalConfig{Token: "the-token", Pool: &PoolAdmin{service: &poolService{access: stuckList{list}}}}}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer the-token"))
	if _, err := service.SetPeerComputePermissions(ctx, &nodepb.SetPeerComputePermissionsRequest{PeerId: peerID}); status.Code(err) != codes.Internal {
		t.Errorf("error %v, want Internal", err)
	}
}
