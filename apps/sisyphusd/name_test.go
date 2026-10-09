package main

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ipfs/go-cid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/access"
	"github.com/sisyphus-network/Sisyphus/packages/identity"
	"github.com/sisyphus-network/Sisyphus/packages/names"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
)

// nameShelf keeps the records of names in memory, and fails on request.
type nameShelf struct {
	mu                 sync.Mutex
	records            map[string][]byte
	failLoad, failSave bool
}

func (s *nameShelf) NameRecord(name string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failLoad {
		return nil, errShelf
	}
	return s.records[name], nil
}

func (s *nameShelf) SetNameRecord(name string, record []byte, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failSave {
		return errShelf
	}
	s.records[name] = record
	return nil
}

func (s *nameShelf) fail(load, save bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failLoad, s.failSave = load, save
}

// fetchURL gets a path from a node's gateway.
func fetchURL(t *testing.T, at, path string, headers ...string) (int, string, http.Header) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://"+at+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(body), res.Header
}

// namesAt returns the name service of the node at addr, which is the node
// with the ID expected, as called by ident.
func namesAt(t *testing.T, addr string, ident *identity.Identity, expected string) pb.NameServiceClient {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(ident.ClientTLS(expected))))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return pb.NewNameServiceClient(conn)
}

// mustCID reads a content ID a command printed.
func mustCID(t *testing.T, printed string) cid.Cid {
	t.Helper()
	c, err := cid.Decode(strings.TrimSpace(printed))
	if err != nil {
		t.Fatalf("%q is not a content ID: %v", printed, err)
	}
	return c
}

func TestANodesNameStandsForWhatTheNodeLastPublished(t *testing.T) {
	dataDir := t.TempDir()
	addr, gateway := freeAddr(t), freeAddr(t)
	flags := []string{"--data-dir", dataDir, "--listen", addr, "--name", "rig", "--gateway-listen", gateway}
	stop := startDaemon(t, flags...)
	waitForOutput(t, "rig", "nodes", "--addr", addr)
	id := nodeID(t, dataDir)
	tokenFile, _ := os.ReadFile(filepath.Join(dataDir, "api.token"))
	token := "?token=" + strings.TrimSpace(string(tokenFile))
	first := strings.TrimSpace(mustCLI(t, "blob", "put", "--addr", addr, writeFile(t, "the boulder rolls up")))
	second := strings.TrimSpace(mustCLI(t, "blob", "put", "--addr", addr, writeFile(t, "the boulder rolls down")))

	// A name nothing has been published under stands for nothing.
	if _, err := cli(t, "name", "resolve", "--addr", addr, id); err == nil || !strings.Contains(err.Error(), "holds no record for that name") {
		t.Errorf("resolving before publishing: %v", err)
	}
	if code, body, _ := fetchURL(t, gateway, "/ipns/"+id+token); code != http.StatusNotFound || !strings.Contains(body, "holds no record") {
		t.Errorf("the gateway before publishing: %d %q", code, body)
	}

	// Published, it resolves, on the command line and through the gateway.
	if out := mustCLI(t, "name", "publish", "--addr", addr, first); out != id+"\n" {
		t.Errorf("name publish printed %q, want the name, %s", out, id)
	}
	if out := mustCLI(t, "name", "resolve", "--addr", addr, id); out != first+"\n" {
		t.Errorf("name resolve printed %q, want %s", out, first)
	}
	if code, _, _ := fetchURL(t, gateway, "/ipns/"+id); code != http.StatusUnauthorized {
		t.Errorf("the gateway without the token: %d", code)
	}
	if code, body, _ := fetchURL(t, gateway, "/ipns/"+id+token); code != http.StatusOK || body != "the boulder rolls up" {
		t.Errorf("the gateway: %d %q", code, body)
	}

	// Published again, the newer record replaces the older.
	mustCLI(t, "name", "publish", "--addr", addr, "--lifetime", "1h", "--ttl", "30s", second)
	if out := mustCLI(t, "name", "resolve", "--addr", addr, id); out != second+"\n" {
		t.Errorf("name resolve after publishing again printed %q, want %s", out, second)
	}
	code, body, header := fetchURL(t, gateway, "/ipns/"+id+token)
	if code != http.StatusOK || body != "the boulder rolls down" || strings.Contains(header.Get("Cache-Control"), "immutable") || !strings.Contains(header.Get("Cache-Control"), "max-age=") {
		t.Errorf("the gateway after publishing again: %d %q %v", code, body, header)
	}

	// The gateway gives the signed record itself to whoever asks for it,
	// and it checks out with nothing but the name.
	code, raw, header := fetchURL(t, gateway, "/ipns/"+id+token, "Accept", "application/vnd.ipfs.ipns-record")
	if code != http.StatusOK || header.Get("Content-Type") != "application/vnd.ipfs.ipns-record" {
		t.Fatalf("the record from the gateway: %d %v", code, header)
	}
	record, err := names.Check(id, []byte(raw), time.Now())
	if err != nil || record.Value.String() != second || record.Sequence != 1 || record.TTL != 30*time.Second || time.Until(record.Expires) > time.Hour {
		t.Errorf("the record from the gateway: %+v, %v", record, err)
	}

	// An older record, sound as it is, does not replace the newer.
	ident := mustIdentity(t, dataDir)
	direct := namesAt(t, addr, ident, id)
	older := names.Make(ident, mustCID(t, first), 0, 48*time.Hour, time.Minute, time.Now())
	if _, err := direct.Publish(context.Background(), &pb.PublishNameRequest{Record: older.Bytes()}); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "as new or newer") {
		t.Errorf("publishing an older record: %v", err)
	}
	if out := mustCLI(t, "name", "resolve", "--addr", addr, id); out != second+"\n" {
		t.Errorf("after an older record was offered, name resolve printed %q, want %s", out, second)
	}

	// The record is kept across a restart.
	stop()
	startDaemon(t, flags...)
	if out := waitForOutput(t, second, "name", "resolve", "--addr", addr, id); out != second+"\n" {
		t.Errorf("name resolve after a restart printed %q, want %s", out, second)
	}
	if code, body, _ := fetchURL(t, gateway, "/ipns/"+id+token); code != http.StatusOK || body != "the boulder rolls down" {
		t.Errorf("the gateway after a restart: %d %q", code, body)
	}

	// A name may point at a sealed file, and says so to whoever resolves
	// it, but the gateway gives nobody the file.
	secret := strings.TrimSpace(mustCLI(t, "blob", "put", "--addr", addr, "--key-file", newKeyFile(t), writeFile(t, "for the owner's eyes")))
	mustCLI(t, "name", "publish", "--addr", addr, secret)
	if out := mustCLI(t, "name", "resolve", "--addr", addr, id); out != secret+"\n" {
		t.Errorf("name resolve of a name pointing at a sealed file printed %q, want %s", out, secret)
	}
	if code, body, _ := fetchURL(t, gateway, "/ipns/"+id+token); code != http.StatusForbidden || !strings.Contains(body, "sealed") {
		t.Errorf("the gateway asked for a name pointing at a sealed file: %d %q", code, body)
	}

	// A record that has run out is no longer given by the gateway, and is
	// not believed by whoever resolves it.
	mustCLI(t, "name", "publish", "--addr", addr, "--lifetime", "1s", first)
	waitFor(t, func() bool {
		code, _, _ := fetchURL(t, gateway, "/ipns/"+id+token)
		return code == http.StatusNotFound
	})
	if _, err := cli(t, "name", "resolve", "--addr", addr, id); err == nil || !strings.Contains(err.Error(), "cannot be relied on") || !strings.Contains(err.Error(), "expired") {
		t.Errorf("resolving a name whose record has expired: %v", err)
	}
	// Publishing again carries on from where the expired record left off.
	mustCLI(t, "name", "publish", "--addr", addr, second)
	_, raw, _ = fetchURL(t, gateway, "/ipns/"+id+token+"&format=ipns-record")
	if record, err := names.Check(id, []byte(raw), time.Now()); err != nil || record.Sequence != 4 {
		t.Errorf("the record published after one expired: %+v, %v", record, err)
	}
}

func TestACoordinatorAnswersForTheNamesOfItsPoolsMembers(t *testing.T) {
	addr, gateway := freeAddr(t), freeAddr(t)
	coordinatorDir, workerDir, laptop := t.TempDir(), t.TempDir(), t.TempDir()
	startDaemon(t, "--role", "coordinator", "--listen", addr, "--data-dir", coordinatorDir, "--gateway-listen", gateway, "--gateway-open")
	startDaemon(t, "--role", "worker", "--coordinator", addr, "--data-dir", workerDir, "--name", "hand")
	waitForOutput(t, "hand", "nodes", "--addr", addr)
	if _, err := as(t, laptop, "pool", "join", "--addr", addr, invite(t, addr, "client")); err != nil {
		t.Fatal(err)
	}
	coordinator, hand, client := nodeID(t, coordinatorDir), nodeID(t, workerDir), nodeID(t, laptop)
	report := strings.TrimSpace(mustCLI(t, "blob", "put", "--addr", addr, writeFile(t, "what the worker can do")))
	notes := strings.TrimSpace(mustCLI(t, "blob", "put", "--addr", addr, writeFile(t, "the client's notes")))

	// A worker hands its record to its coordinator, and so does a client:
	// each under its own name.
	if out, err := as(t, workerDir, "name", "publish", "--addr", addr, report); err != nil || out != hand+"\n" {
		t.Fatalf("a worker publishing: %q, %v", out, err)
	}
	if out, err := as(t, laptop, "name", "publish", "--addr", addr, notes); err != nil || out != client+"\n" {
		t.Fatalf("a client publishing: %q, %v", out, err)
	}
	// The coordinator then answers for both, to its members and at its
	// gateway.
	if out, err := as(t, laptop, "name", "resolve", "--addr", addr, hand); err != nil || out != report+"\n" {
		t.Errorf("a client resolving a worker's name: %q, %v", out, err)
	}
	if out, err := as(t, workerDir, "name", "resolve", "--addr", addr, client); err != nil || out != notes+"\n" {
		t.Errorf("a worker resolving a client's name: %q, %v", out, err)
	}
	if status, body, _ := fetchURL(t, gateway, "/ipns/"+hand); status != http.StatusOK || body != "what the worker can do" {
		t.Errorf("the gateway asked for the worker's name: %d %q", status, body)
	}
	if status, body, _ := fetchURL(t, gateway, "/ipns/"+client); status != http.StatusOK || body != "the client's notes" {
		t.Errorf("the gateway asked for the client's name: %d %q", status, body)
	}

	// A member may not publish under another node's name: neither a record
	// it signs itself, nor one the other node really signed.
	ctx := context.Background()
	asClient := namesAt(t, addr, mustIdentity(t, laptop), coordinator)
	forged := names.Make(mustIdentity(t, laptop), mustCID(t, notes), 9, time.Hour, time.Minute, time.Now())
	genuine := names.Make(mustIdentity(t, workerDir), mustCID(t, notes), 9, time.Hour, time.Minute, time.Now())
	if _, err := asClient.Publish(ctx, &pb.PublishNameRequest{Record: genuine.Bytes()}); status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "under its own name only") {
		t.Errorf("a client publishing a record the worker signed: %v", err)
	}
	asWorker := namesAt(t, addr, mustIdentity(t, workerDir), coordinator)
	if _, err := asWorker.Publish(ctx, &pb.PublishNameRequest{Record: forged.Bytes()}); status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "not one signed by node "+hand) {
		t.Errorf("a worker publishing a record the client signed: %v", err)
	}
	if out := mustCLI(t, "name", "resolve", "--addr", addr, hand); out != report+"\n" {
		t.Errorf("the worker's name after another node tried to publish under it: %q, want %s", out, report)
	}
	// A node that has not been admitted may do neither.
	stranger := namesAt(t, addr, newIdentity(t), coordinator)
	if _, err := stranger.Resolve(ctx, &pb.ResolveNameRequest{Name: hand}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("a stranger resolving: %v", err)
	}
	if _, err := stranger.Publish(ctx, &pb.PublishNameRequest{Record: forged.Bytes()}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("a stranger publishing: %v", err)
	}

	// The coordinator answers for its members only: a node removed from
	// the pool is no longer answered for.
	mustCLI(t, "pool", "remove", "--addr", addr, client)
	if _, err := cli(t, "name", "resolve", "--addr", addr, client); err == nil || !strings.Contains(err.Error(), "holds no record for that name") {
		t.Errorf("resolving the name of a node removed from the pool: %v", err)
	}
	if status, _, _ := fetchURL(t, gateway, "/ipns/"+client); status != http.StatusNotFound {
		t.Errorf("the gateway asked for the name of a node removed from the pool: %d", status)
	}

	// A node that coordinates nothing holds no names, whatever is asked of
	// its gateway.
	workerGateway := freeAddr(t)
	startDaemon(t, "--role", "worker", "--coordinator", addr, "--name", "other", "--gateway-listen", workerGateway, "--gateway-open")
	waitForOutput(t, "other", "nodes", "--addr", addr)
	if status, body, _ := fetchURL(t, workerGateway, "/ipns/"+hand); status != http.StatusNotFound || !strings.Contains(body, "holds no record") {
		t.Errorf("a worker's gateway asked for a name: %d %q", status, body)
	}
}

func TestTheNameServiceRefusesWhatItCannotCheckOrKeep(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	ctx := context.Background()
	service := pb.NewNameServiceClient(p.conn)
	value := mustCID(t, p.upload("what the name stands for"))
	now := time.Now()

	for about, tt := range map[string]struct {
		record []byte
		want   string
	}{
		"that has expired":     {names.Make(p.ident, value, 1, time.Hour, time.Minute, now.Add(-2*time.Hour)).Bytes(), "the record has expired"},
		"that is not a record": {[]byte("\xff not a record"), "not a record for a name"},
		"that is empty":        {nil, "not a record for a name"},
	} {
		if _, err := service.Publish(ctx, &pb.PublishNameRequest{Record: tt.record}); status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("publishing a record %s: %v, want %q", about, err, tt.want)
		}
	}
	if _, err := service.Resolve(ctx, &pb.ResolveNameRequest{Name: "rig"}); status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), `"rig" is not a name`) {
		t.Errorf("resolving what is not a name: %v", err)
	}

	// A member's record is kept under the member's name.
	member, creds := p.admit(access.Worker)
	conn, err := grpc.NewClient(p.addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	good := names.Make(member, value, 1, time.Hour, time.Minute, now)
	if _, err := pb.NewNameServiceClient(conn).Publish(ctx, &pb.PublishNameRequest{Record: good.Bytes()}); err != nil {
		t.Fatal(err)
	}
	if held, err := service.Resolve(ctx, &pb.ResolveNameRequest{Name: member.ID()}); err != nil || string(held.GetRecord()) != string(good.Bytes()) {
		t.Errorf("the member's record as held: %v", err)
	}

	p.names.fail(false, true)
	own := names.Make(p.ident, value, 1, time.Hour, time.Minute, now)
	if _, err := service.Publish(ctx, &pb.PublishNameRequest{Record: own.Bytes()}); status.Code(err) != codes.Internal || !strings.Contains(err.Error(), "keep the record") {
		t.Errorf("publishing when the record cannot be saved: %v", err)
	}
	p.names.fail(true, false)
	if _, err := service.Resolve(ctx, &pb.ResolveNameRequest{Name: member.ID()}); status.Code(err) != codes.Internal || !strings.Contains(err.Error(), "read the record") {
		t.Errorf("resolving when the record cannot be loaded: %v", err)
	}
}

// scriptedNames is a node whose name service answers as it is told to.
type scriptedNames struct {
	pb.UnimplementedNameServiceServer
	held       []byte
	resolveErr error
	publishErr error
}

func (s *scriptedNames) Resolve(context.Context, *pb.ResolveNameRequest) (*pb.ResolveNameResponse, error) {
	return &pb.ResolveNameResponse{Record: s.held}, s.resolveErr
}

func (s *scriptedNames) Publish(context.Context, *pb.PublishNameRequest) (*pb.PublishNameResponse, error) {
	return &pb.PublishNameResponse{}, s.publishErr
}

func TestNameCommandsBelieveOnlyWhatChecksOut(t *testing.T) {
	script := new(scriptedNames)
	addr := serveFake(t, func(srv *grpc.Server) { pb.RegisterNameServiceServer(srv, script) })
	dir, _ := dataDirs.Load(addr)
	ident := mustIdentity(t, dir.(string))
	other := newIdentity(t)
	value := mustCID(t, megabyteCID)
	theirs := names.Make(other, value, 3, time.Hour, time.Minute, time.Now()).Bytes()

	// A node that answers with another node's record is not believed.
	script.held = theirs
	if _, err := cli(t, "name", "resolve", "--addr", addr, ident.ID()); err == nil || !strings.Contains(err.Error(), "cannot be relied on") || !strings.Contains(err.Error(), "not one signed by node "+ident.ID()) {
		t.Errorf("resolving from a node that gives another's record: %v", err)
	}
	if _, err := cli(t, "name", "publish", "--addr", addr, megabyteCID); err == nil || !strings.Contains(err.Error(), "as this node's last record, one that is not") {
		t.Errorf("publishing through a node that gives another's record as this node's: %v", err)
	}
	// Given a sound one, resolving prints what it points at, by either
	// spelling of the name.
	if out, err := cli(t, "name", "resolve", "--addr", addr, "/ipns/"+other.ID()); err != nil || out != megabyteCID+"\n" {
		t.Errorf("resolving another node's name: %q, %v", out, err)
	}

	// Failures of the node are reported as they are.
	script.resolveErr = status.Error(codes.Unavailable, "the node is busy")
	for _, command := range [][]string{{"name", "resolve", "--addr", addr, ident.ID()}, {"name", "publish", "--addr", addr, megabyteCID}} {
		if _, err := cli(t, command...); err == nil || !strings.Contains(err.Error(), "the node is busy") {
			t.Errorf("%v when the node cannot resolve: %v", command[:2], err)
		}
	}
	script.resolveErr, script.publishErr = status.Error(codes.NotFound, "none"), status.Error(codes.Internal, "the disk is full")
	if _, err := cli(t, "name", "publish", "--addr", addr, megabyteCID); err == nil || !strings.Contains(err.Error(), "the disk is full") {
		t.Errorf("publishing when the node cannot keep the record: %v", err)
	}
}

func TestNameCommandUsageErrors(t *testing.T) {
	badKey := t.TempDir()
	os.WriteFile(filepath.Join(badKey, "node.key"), []byte("not a key"), 0o600)
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"name"}, "expected name publish or resolve"},
		{[]string{"name", "frobnicate"}, "expected name publish or resolve"},
		{[]string{"name", "publish"}, "expected exactly one CID"},
		{[]string{"name", "publish", megabyteCID, "extra"}, "expected exactly one CID"},
		{[]string{"name", "publish", "not-a-cid"}, `invalid CID "not-a-cid"`},
		{[]string{"name", "publish", "--lifetime", "10ms", megabyteCID}, "--lifetime must be at least a second"},
		{[]string{"name", "publish", "--ttl", "-1s", megabyteCID}, "--ttl cannot be negative"},
		{[]string{"name", "publish", "--no-such-flag"}, "flag provided but not defined"},
		{[]string{"name", "publish", "--data-dir", badKey, megabyteCID}, "is not PEM"},
		{[]string{"name", "publish", "--data-dir", t.TempDir(), "--addr", badAddr, megabyteCID}, "invalid control character"},
		{[]string{"name", "resolve"}, "expected exactly one name"},
		{[]string{"name", "resolve", "rig"}, `"rig" is not a name`},
		{[]string{"name", "resolve", "--no-such-flag"}, "flag provided but not defined"},
		{[]string{"name", "resolve", "--data-dir", t.TempDir(), "--addr", badAddr, newIdentity(t).ID()}, "invalid control character"},
	} {
		if _, err := cli(t, tt.args...); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%v: error %v, want one containing %q", tt.args, err, tt.want)
		}
	}
}
