package kubo

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests need no ipfs program: a stand-in HTTP server answers as a
// misbehaving Kubo would, and shell scripts stand in for an ipfs binary that
// fails to start.

// misbehaving returns a client for a server that answers every call with
// the given status and body.
func misbehaving(t *testing.T, status int, body string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return NewClient(srv.Listener.Addr().String())
}

// everyCall makes each kind of call on a client and returns the errors.
func everyCall(c *Client) map[string]error {
	errs := make(map[string]error)
	_, errs["ID"] = c.ID(ctx)
	_, errs["BlockPut"] = c.BlockPut(ctx, "raw", []byte("data"))
	_, errs["BlockGet"] = c.BlockGet(ctx, "bafkexample")
	_, errs["BlockSize"] = c.BlockSize(ctx, "bafkexample")
	errs["BlockRemove"] = c.BlockRemove(ctx, "bafkexample")
	errs["LocalBlocks"] = c.LocalBlocks(ctx, func(string) bool { return true })
	_, errs["RepoSize"] = c.RepoSize(ctx)
	errs["Pinned"] = c.Pinned(ctx, func(string) {})
	_, errs["Addresses"] = c.Addresses(ctx)
	errs["Connect"] = c.Connect(ctx, "/ip4/10.0.0.1/tcp/4101/p2p/12D3KooWexample")
	_, errs["Peers"] = c.Peers(ctx)
	_, errs["PeerAddresses"] = c.PeerAddresses(ctx)
	return errs
}

func TestEveryCallReportsKubosOwnErrors(t *testing.T) {
	failing := misbehaving(t, http.StatusInternalServerError, `{"Message":"repo is locked","Code":0,"Type":"error"}`)
	for call, err := range everyCall(failing) {
		if err == nil || !strings.Contains(err.Error(), "repo is locked (HTTP 500)") {
			t.Errorf("%s: %v, want Kubo's message", call, err)
		}
	}

	// An error that is not Kubo's JSON, as a proxy in front of it might send.
	plain := misbehaving(t, http.StatusBadGateway, "upstream is down\n")
	if _, err := plain.ID(ctx); err == nil || !strings.Contains(err.Error(), "upstream is down (HTTP 502)") {
		t.Errorf("a plain-text error: %v", err)
	}
}

func TestKubosNotFoundIsRecognised(t *testing.T) {
	for _, message := range []string{
		"block was not found locally (offline): ipld: could not find bafkexample",
		"ipld: could not find bafkexample",
		"blockservice: key not found",
		"context deadline exceeded", // the time allowed for asking peers ran out
	} {
		c := misbehaving(t, http.StatusInternalServerError, `{"Message":"`+message+`"}`)
		if _, err := c.BlockGet(ctx, "bafkexample"); !errors.Is(err, ErrNotFound) {
			t.Errorf("%q: %v, want ErrNotFound", message, err)
		}
	}
}

func TestRepliesThatMakeNoSenseAreErrors(t *testing.T) {
	garbled := misbehaving(t, http.StatusOK, "this is not JSON")
	errs := everyCall(garbled)
	for _, call := range []string{"ID", "BlockPut", "BlockSize", "LocalBlocks", "RepoSize", "Pinned", "Addresses", "Peers", "PeerAddresses"} {
		if errs[call] == nil {
			t.Errorf("%s accepted a reply that is not JSON", call)
		}
	}
}

func TestCallsFailWhenKuboCannotBeReached(t *testing.T) {
	// Nothing is listening here.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := lis.Addr().String()
	lis.Close()
	for call, err := range everyCall(NewClient(addr)) {
		if err == nil {
			t.Errorf("%s succeeded with no daemon", call)
		}
	}
	// An address no request can even be made for.
	if _, err := NewClient("bad\x00address").ID(ctx); err == nil {
		t.Error("a call to a malformed address succeeded")
	}
}

func TestAClientSaysWhereItsKuboIs(t *testing.T) {
	if got := NewClient("127.0.0.1:5001").Address(); got != "127.0.0.1:5001" {
		t.Errorf("Address = %q, want the address the client was made for", got)
	}
}

func TestBlockRemoveReportsAFailureKuboPutsInTheBody(t *testing.T) {
	c := misbehaving(t, http.StatusOK, `{"Hash":"bafkexample","Error":"pinned: recursive"}`)
	if err := c.BlockRemove(ctx, "bafkexample"); err == nil || !strings.Contains(err.Error(), "pinned: recursive") {
		t.Errorf("error %v, want the reason from the body", err)
	}
}

func TestBlockGetReportsAReplyCutShort(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.Write([]byte("only this much"))
	}))
	t.Cleanup(srv.Close)
	if _, err := NewClient(srv.Listener.Addr().String()).BlockGet(ctx, "bafkexample"); err == nil {
		t.Error("BlockGet returned a block that arrived incomplete")
	}
}

func TestLocalBlocks(t *testing.T) {
	listing := misbehaving(t, http.StatusOK, `{"Ref":"a","Err":""}`+"\n"+`{"Ref":"b","Err":""}`+"\n"+`{"Ref":"c","Err":""}`+"\n")
	var seen []string
	if err := listing.LocalBlocks(ctx, func(id string) bool { seen = append(seen, id); return true }); err != nil || strings.Join(seen, "") != "abc" {
		t.Errorf("listed %v, %v", seen, err)
	}
	// The visitor can stop early.
	seen = nil
	if err := listing.LocalBlocks(ctx, func(id string) bool { seen = append(seen, id); return id != "b" }); err != nil || strings.Join(seen, "") != "ab" {
		t.Errorf("after asking to stop at b: listed %v, %v", seen, err)
	}

	failed := misbehaving(t, http.StatusOK, `{"Ref":"a","Err":""}`+"\n"+`{"Ref":"","Err":"datastore closed"}`+"\n")
	if err := failed.LocalBlocks(ctx, func(string) bool { return true }); err == nil || !strings.Contains(err.Error(), "datastore closed") {
		t.Errorf("a listing that fails part-way: %v", err)
	}
	// A line longer than any real entry.
	endless := misbehaving(t, http.StatusOK, `{"Ref":"`+strings.Repeat("a", 100_000)+`"}`)
	if err := endless.LocalBlocks(ctx, func(string) bool { return true }); err == nil {
		t.Error("a listing with an absurd line was accepted")
	}
}

// fakeIPFS writes a shell script to stand in for the ipfs program and
// returns its path. The script is given the repository as $IPFS_PATH and the
// subcommand as $1.
func fakeIPFS(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ipfs")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// initialised returns a repository directory that already has a config, so
// that Start goes straight to running the daemon.
func initialised(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "config"), []byte(`{"Identity":{},"Addresses":"not a section"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return repo
}

func TestStartFailures(t *testing.T) {
	ident := newIdentity(t)
	short := 300 * time.Millisecond

	configIsDir := t.TempDir()
	os.Mkdir(filepath.Join(configIsDir, "config"), 0o700)
	configIsGarbage := t.TempDir()
	os.WriteFile(filepath.Join(configIsGarbage, "config"), []byte("not json"), 0o600)
	logIsDir := initialised(t)
	os.Mkdir(filepath.Join(logIsDir, "daemon.log"), 0o700)
	notAProgram := filepath.Join(t.TempDir(), "ipfs")
	os.WriteFile(notAProgram, []byte("this is not a program"), 0o700)
	// A port with nothing on it, for a daemon that claims to listen there.
	lis, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := lis.Addr().(*net.TCPAddr).Port
	lis.Close()

	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{"there is no such program", Config{Binary: filepath.Join(t.TempDir(), "absent"), Repo: t.TempDir()}, "no such file"},
		{"setting up the repository fails", Config{Binary: fakeIPFS(t, `echo "starting up"; echo "Error: disk is full" >&2; exit 1`), Repo: t.TempDir()}, "Error: disk is full"},
		{"the config cannot be read", Config{Binary: fakeIPFS(t, "exit 0"), Repo: configIsDir}, "is a directory"},
		{"the config is not JSON", Config{Binary: fakeIPFS(t, "exit 0"), Repo: configIsGarbage}, "invalid character"},
		{"the log cannot be written", Config{Binary: fakeIPFS(t, "exit 0"), Repo: logIsDir}, "is a directory"},
		{"the program cannot be run", Config{Binary: notAProgram, Repo: initialised(t)}, "exec format error"},
		{"the daemon exits at once", Config{Binary: fakeIPFS(t, `echo "Error: lock is held by another daemon"; exit 1`), Repo: initialised(t)}, "stopped while starting: Error: lock is held by another daemon"},
		{"the daemon never listens", Config{Binary: fakeIPFS(t, "exec sleep 30"), Repo: initialised(t), ReadyTimeout: short}, "did not answer within 300ms"},
		{"the daemon's address file is garbage", Config{Binary: fakeIPFS(t, `echo nonsense > "$IPFS_PATH/api"; exec sleep 30`), Repo: initialised(t), ReadyTimeout: short}, "did not answer within 300ms"},
		{"the daemon claims an address it is not on", Config{Binary: fakeIPFS(t, fmt.Sprintf(`echo /ip4/127.0.0.1/tcp/%d > "$IPFS_PATH/api"; exec sleep 30`, dead)), Repo: initialised(t), ReadyTimeout: short}, "did not answer within 300ms"},
	}
	for _, tt := range tests {
		tt.cfg.Identity = ident
		d, err := Start(ctx, tt.cfg)
		if err == nil {
			d.Stop()
			t.Errorf("%s: Start succeeded", tt.name)
			continue
		}
		if !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: error %v, want one containing %q", tt.name, err, tt.want)
		}
	}
}

func TestStartFailsIfTheConfigCannotBeRewritten(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can write to a read-only file")
	}
	repo := initialised(t)
	if err := os.Chmod(filepath.Join(repo, "config"), 0o400); err != nil {
		t.Fatal(err)
	}
	if _, err := Start(ctx, Config{Binary: fakeIPFS(t, "exit 0"), Repo: repo, Identity: newIdentity(t)}); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("error %v, want the config reported unwritable", err)
	}
}

func TestStartGivesUpWhenCancelled(t *testing.T) {
	cancelled, cancel := context.WithCancel(ctx)
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	_, err := Start(cancelled, Config{Binary: fakeIPFS(t, "exec sleep 30"), Repo: initialised(t), Identity: newIdentity(t)})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error %v, want context.Canceled", err)
	}
}

func TestStopKillsADaemonThatWillNotShutDown(t *testing.T) {
	defer func(grace time.Duration) { stopGrace = grace }(stopGrace)
	stopGrace = 200 * time.Millisecond

	// A daemon that ignores the request to stop.
	stubborn := fakeIPFS(t, `trap "" INT; while :; do sleep 1; done`)
	started := time.Now()
	_, err := Start(ctx, Config{Binary: stubborn, Repo: initialised(t), Identity: newIdentity(t), ReadyTimeout: 100 * time.Millisecond})
	if err == nil {
		t.Fatal("Start succeeded")
	}
	// Start stops the daemon before reporting that it never answered; had
	// the daemon not been killed, that would have hung.
	if took := time.Since(started); took > 5*time.Second {
		t.Errorf("giving up on a stubborn daemon took %v", took)
	}
}

func TestConfigureKeepsSettingsItDoesNotChange(t *testing.T) {
	file := filepath.Join(t.TempDir(), "config")
	original := `{"Datastore":{"StorageMax":"10GB"},"Addresses":{"API":"/ip4/0.0.0.0/tcp/5001","Announce":["/dns4/example"]},"Bootstrap":["/dnsaddr/bootstrap.libp2p.io"]}`
	if err := os.WriteFile(file, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	ident := newIdentity(t)
	if err := configure(file, ident, nil); err != nil {
		t.Fatal(err)
	}
	edited, _ := os.ReadFile(file)
	for _, want := range []string{
		`"StorageMax": "10GB"`, `"/dns4/example"`, // left alone
		`"API": "/ip4/127.0.0.1/tcp/0"`, `"Bootstrap": []`, `"PeerID": "` + ident.ID() + `"`, // set
	} {
		if !strings.Contains(string(edited), want) {
			t.Errorf("config lacks %s:\n%s", want, edited)
		}
	}
	if strings.Contains(string(edited), "bootstrap.libp2p.io") || strings.Contains(string(edited), "0.0.0.0") {
		t.Errorf("config still reaches outside:\n%s", edited)
	}
}

func TestPeerAddressesFailsIfKuboCannotListAddresses(t *testing.T) {
	// The list of peers arrives; the addresses known for them do not.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "swarm/peers") {
			fmt.Fprint(w, `{"Peers":[{"Peer":"12D3KooWexample"}]}`)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"Message":"peerstore closed"}`)
	}))
	t.Cleanup(srv.Close)
	if _, err := NewClient(srv.Listener.Addr().String()).PeerAddresses(ctx); err == nil || !strings.Contains(err.Error(), "peerstore closed") {
		t.Errorf("error %v, want Kubo's", err)
	}
}

func TestBlockGetTellsKuboHowLongToAskItsPeers(t *testing.T) {
	var asked string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.Query().Get("timeout")
		fmt.Fprint(w, "data")
	}))
	t.Cleanup(srv.Close)
	c := NewClient(srv.Listener.Addr().String())

	c.BlockGet(ctx, "bafkexample")
	if asked != "" {
		t.Errorf("with no timeout set, Kubo was given %q", asked)
	}
	c.PeerTimeout = 45 * time.Second
	c.BlockGet(ctx, "bafkexample")
	if asked != "45s" {
		t.Errorf("Kubo was given a timeout of %q, want 45s", asked)
	}
}
