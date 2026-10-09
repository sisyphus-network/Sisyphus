package api

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ipfs/go-cid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/blobclient"
	"github.com/sisyphus-network/Sisyphus/packages/identity"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

func newIdentity(t *testing.T) *identity.Identity {
	t.Helper()
	ident, _, err := identity.LoadOrCreate(filepath.Join(t.TempDir(), "node.key"))
	if err != nil {
		t.Fatal(err)
	}
	return ident
}

// peerServer is a worker serving its store to fellow members.
type peerServer struct {
	t     *testing.T
	addr  string
	ident *identity.Identity
	store *storage.Store
	// members are the nodes the coordinator would vouch for, and askErr, if
	// set, is what asking the coordinator returns instead.
	members map[string]bool
	askErr  error
}

func startPeerServer(t *testing.T) *peerServer {
	t.Helper()
	p := &peerServer{t: t, ident: newIdentity(t), store: storage.NewMemory(), members: make(map[string]bool)}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := NewPeerServer(p.ident, p.store, func(_ context.Context, nodeID string) (bool, error) {
		return p.members[nodeID], p.askErr
	})
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	p.addr = lis.Addr().String()
	return p
}

func (p *peerServer) dial(creds credentials.TransportCredentials) pb.BlobServiceClient {
	p.t.Helper()
	conn, err := grpc.NewClient(p.addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		p.t.Fatal(err)
	}
	p.t.Cleanup(func() { conn.Close() })
	return pb.NewBlobServiceClient(conn)
}

func (p *peerServer) dialAs(ident *identity.Identity) pb.BlobServiceClient {
	return p.dial(credentials.NewTLS(ident.ClientTLS(p.ident.ID())))
}

func TestAWorkerServesItsBlobsToFellowMembersOnly(t *testing.T) {
	ctx := context.Background()
	p := startPeerServer(t)
	data := bytes.Repeat([]byte("held by this worker. "), 30_000)
	c, err := p.store.Put(ctx, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	member, stranger := newIdentity(t), newIdentity(t)
	p.members[member.ID()] = true

	// A member gets the blob, whole and checked against its CID.
	var got bytes.Buffer
	if err := blobclient.Download(ctx, p.dialAs(member), c, &got); err != nil || !bytes.Equal(got.Bytes(), data) {
		t.Errorf("a member's download: %d bytes, %v", got.Len(), err)
	}
	if stat, err := p.dialAs(member).Stat(ctx, &pb.StatBlobRequest{Cid: c.String()}); err != nil || stat.GetSize() != uint64(len(data)) {
		t.Errorf("a member's Stat = %v, %v", stat, err)
	}

	// Anyone else is refused, on both kinds of call, and told why.
	_, err = p.dialAs(stranger).Stat(ctx, &pb.StatBlobRequest{Cid: c.String()})
	if status.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), "node "+stranger.ID()+" is not a member") {
		t.Errorf("a stranger's Stat: %v", err)
	}
	if err := blobclient.Download(ctx, p.dialAs(stranger), c, io.Discard); status.Code(err) != codes.PermissionDenied {
		t.Errorf("a stranger's download: %v", err)
	}
}

func TestAWorkerOnlyServesReads(t *testing.T) {
	ctx := context.Background()
	p := startPeerServer(t)
	member := newIdentity(t)
	p.members[member.ID()] = true
	client := p.dialAs(member)

	if _, err := blobclient.Upload(ctx, client, strings.NewReader("an upload")); status.Code(err) != codes.Unimplemented {
		t.Errorf("uploading to a worker: %v, want Unimplemented", err)
	}
	if _, err := client.CollectGarbage(ctx, &pb.CollectGarbageRequest{}); status.Code(err) != codes.Unimplemented {
		t.Errorf("collecting a worker's garbage: %v, want Unimplemented", err)
	}
	if _, err := client.Pin(ctx, &pb.PinBlobRequest{}); status.Code(err) != codes.Unimplemented {
		t.Errorf("pinning on a worker: %v, want Unimplemented", err)
	}
}

func TestAWorkerRefusesWhenItCannotCheckMembership(t *testing.T) {
	ctx := context.Background()
	p := startPeerServer(t)
	p.askErr = errors.New("coordinator is unreachable")
	someone := newIdentity(t)

	_, err := p.dialAs(someone).Stat(ctx, &pb.StatBlobRequest{})
	if status.Code(err) != codes.Unavailable || !strings.Contains(err.Error(), "coordinator is unreachable") {
		t.Errorf("error %v, want Unavailable naming the cause", err)
	}
}

func TestAWorkerRefusesACallerItCannotIdentify(t *testing.T) {
	ctx := context.Background()
	p := startPeerServer(t)

	// A client whose certificate holds a key of a kind nodes do not use.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "not a node"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	foreign := credentials.NewTLS(&tls.Config{
		Certificates:       []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		InsecureSkipVerify: true,
	})
	if _, err := p.dial(foreign).Stat(ctx, &pb.StatBlobRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("error %v, want Unauthenticated", err)
	}
}

func TestAFollowerServesItsCopiesAndItsCacheAsOneStore(t *testing.T) {
	ctx := context.Background()
	copies, cache := storage.NewMemory(), storage.NewMemory()
	held, err := copies.Put(ctx, strings.NewReader("held for the pool"))
	if err != nil {
		t.Fatal(err)
	}
	cached, err := cache.Put(ctx, strings.NewReader("used by a task"))
	if err != nil {
		t.Fatal(err)
	}
	absent, err := storage.CID(ctx, strings.NewReader("in neither"))
	if err != nil {
		t.Fatal(err)
	}

	both := Stores(copies, cache)
	for c, want := range map[cid.Cid]string{held: "held for the pool", cached: "used by a task"} {
		blob, err := both.Open(ctx, c)
		if err != nil {
			t.Fatalf("opening %q: %v", want, err)
		}
		got, err := io.ReadAll(blob)
		blob.Close()
		if err != nil || string(got) != want {
			t.Errorf("read %q, error %v, want %q", got, err, want)
		}
	}
	if _, err := both.Open(ctx, absent); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("opening a blob neither store holds: %v", err)
	}
	if _, err := Stores().Open(ctx, held); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("opening a blob from no stores at all: %v", err)
	}
	// A store that fails is not passed over as if it merely lacked the blob.
	if _, err := Stores(brokenStore{Store: copies, failOpen: true}, cache).Open(ctx, cached); !errors.Is(err, errDisk) {
		t.Errorf("opening a blob when the first store fails: %v", err)
	}
}

func TestANodeHoldingABlobTwoWaysIsNamedOnce(t *testing.T) {
	a, b, c := &pb.BlobHolder{NodeId: "a", Address: "a:1"}, &pb.BlobHolder{NodeId: "b", Address: "b:1"}, &pb.BlobHolder{NodeId: "c", Address: "c:1"}
	merged := mergeHolders([]*pb.BlobHolder{a, c}, []*pb.BlobHolder{b, c})
	if len(merged) != 3 || merged[0] != a || merged[1] != b || merged[2].GetNodeId() != "c" {
		t.Errorf("merged into %v, want a, b and c once each, in order", merged)
	}
	if merged := mergeHolders(nil, nil); len(merged) != 0 {
		t.Errorf("two empty lists merged into %v", merged)
	}
}
