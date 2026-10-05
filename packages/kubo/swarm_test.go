package kubo

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// member starts a real Kubo daemon on a private network.
func member(t *testing.T, key string, peers ...string) *Daemon {
	t.Helper()
	requireKubo(t)
	d, err := Start(ctx, Config{
		Repo: filepath.Join(t.TempDir(), "ipfs"), Identity: newIdentity(t),
		Swarm: &Swarm{Key: key, Peers: peers, PeerTimeout: 3 * time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Stop)
	return d
}

func addressesOf(t *testing.T, d *Daemon) []string {
	t.Helper()
	addresses, err := d.Addresses(ctx)
	if err != nil || len(addresses) == 0 {
		t.Fatalf("Addresses = %v, %v", addresses, err)
	}
	return addresses
}

func idOf(t *testing.T, d *Daemon) string {
	t.Helper()
	id, err := d.ID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// connected waits until a is connected to b.
func connected(t *testing.T, a, b *Daemon) {
	t.Helper()
	want := idOf(t, b)
	deadline := time.Now().Add(20 * time.Second)
	for {
		peers, err := a.Peers(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(peers, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("never connected to %s; peers: %v", want, peers)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestMembersOfASwarmFetchBlocksFromEachOther(t *testing.T) {
	key := NewSwarmKey()
	first := member(t, key)
	second := member(t, key, addressesOf(t, first)...)
	connected(t, second, first)

	// Every address a member reports ends in its own ID.
	for _, address := range addressesOf(t, first) {
		if !strings.HasSuffix(address, "/p2p/"+idOf(t, first)) {
			t.Errorf("address %q does not end in the member's ID", address)
		}
	}

	id, err := first.BlockPut(ctx, "raw", []byte("held by the first member"))
	if err != nil {
		t.Fatal(err)
	}
	// The second member does not hold it...
	if _, err := second.BlockSize(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the second member holds the block before asking for it: %v", err)
	}
	// ...but gets it for the asking, and then does.
	got, err := second.BlockGet(ctx, id)
	if err != nil || string(got) != "held by the first member" {
		t.Fatalf("BlockGet through the swarm = %q, %v", got, err)
	}
	if _, err := second.BlockSize(ctx, id); err != nil {
		t.Errorf("a block fetched from a peer was not kept: %v", err)
	}

	// A block nobody has is given up on after the swarm's timeout.
	started := time.Now()
	if _, err := second.BlockGet(ctx, "bafkreifjjcie6lypi6ny7amxnfftagclbuxndqonfipmb64f2km2devei4"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a block nobody has: %v, want ErrNotFound", err)
	}
	if took := time.Since(started); took < 2*time.Second || took > 15*time.Second {
		t.Errorf("giving up took %v, want about the 3s timeout", took)
	}
}

func TestAJoiningMemberLearnsOfTheOthersFromTheFirst(t *testing.T) {
	key := NewSwarmKey()
	hub := member(t, key)
	early := member(t, key, addressesOf(t, hub)...)
	connected(t, hub, early)

	// What the hub would tell a newcomer: where the members connected to it
	// can be reached.
	others, err := hub.PeerAddresses(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(others) == 0 || !strings.HasSuffix(others[0], "/p2p/"+idOf(t, early)) {
		t.Fatalf("PeerAddresses = %v, want addresses of the connected member", others)
	}

	late := member(t, key, append(addressesOf(t, hub), others...)...)
	connected(t, late, early)

	// So the newcomer can fetch what only the other member holds.
	id, err := early.BlockPut(ctx, "raw", []byte("held only by the early member"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := late.BlockGet(ctx, id); err != nil || string(got) != "held only by the early member" {
		t.Errorf("BlockGet from a fellow member = %q, %v", got, err)
	}
	if _, err := hub.BlockSize(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Errorf("the hub holds the block, so the fetch proves nothing: %v", err)
	}
}

func TestADaemonWithoutTheKeyCannotJoinOrFetch(t *testing.T) {
	insider := member(t, NewSwarmKey())
	outsider := member(t, NewSwarmKey(), addressesOf(t, insider)...)

	id, err := insider.BlockPut(ctx, "raw", []byte("for members only"))
	if err != nil {
		t.Fatal(err)
	}
	if err := outsider.Connect(ctx, addressesOf(t, insider)[0]); err == nil {
		t.Error("a daemon with a different key connected")
	}
	if _, err := outsider.BlockGet(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Errorf("a daemon with a different key fetched a block: %v", err)
	}
	if peers, _ := insider.Peers(ctx); slices.Contains(peers, idOf(t, outsider)) {
		t.Error("the member lists the outsider as a peer")
	}
}

func TestConnect(t *testing.T) {
	key := NewSwarmKey()
	a, b := member(t, key), member(t, key)
	if err := b.Connect(ctx, addressesOf(t, a)[0]); err != nil {
		t.Fatal(err)
	}
	connected(t, a, b)
}

func TestSwarmKeysAreSecretsInKubosFormat(t *testing.T) {
	key := NewSwarmKey()
	lines := strings.Split(key, "\n")
	if len(lines) != 4 || lines[0] != "/key/swarm/psk/1.0.0/" || lines[1] != "/base16/" || len(lines[2]) != 64 || lines[3] != "" {
		t.Errorf("key is %q", key)
	}
	if NewSwarmKey() == key {
		t.Error("two generated keys are the same")
	}
}

func TestConfigureForASwarm(t *testing.T) {
	repo := t.TempDir()
	file := filepath.Join(repo, "config")
	os.WriteFile(file, []byte(`{}`), 0o600)
	ident := newIdentity(t)
	swarm := &Swarm{Key: NewSwarmKey(), Port: 4101, Peers: []string{
		"/ip4/10.0.0.5/tcp/4101/p2p/12D3KooWAAAA",
		"/ip4/192.168.1.5/tcp/4101/p2p/12D3KooWAAAA",
		"/ip4/10.0.0.9/tcp/40000/p2p/12D3KooWBBBB",
	}}
	if err := configure(file, ident, swarm); err != nil {
		t.Fatal(err)
	}
	config, _ := os.ReadFile(file)
	normal := strings.Join(strings.Fields(string(config)), "")
	for _, want := range []string{
		`"Swarm":["/ip4/0.0.0.0/tcp/4101"]`, `"Type":"dht"`, `"QUIC":false`,
		// One entry per member, with all of its addresses.
		`{"Addrs":["/ip4/10.0.0.5/tcp/4101","/ip4/192.168.1.5/tcp/4101"],"ID":"12D3KooWAAAA"}`,
		`{"Addrs":["/ip4/10.0.0.9/tcp/40000"],"ID":"12D3KooWBBBB"}`,
		`"Bootstrap":["/ip4/10.0.0.5/tcp/4101/p2p/12D3KooWAAAA",`,
	} {
		if !strings.Contains(normal, want) {
			t.Errorf("config lacks %s:\n%s", want, config)
		}
	}
	stored, err := os.ReadFile(filepath.Join(repo, "swarm.key"))
	if err != nil || string(stored) != swarm.Key {
		t.Errorf("swarm.key holds %q, %v", stored, err)
	}
	if info, _ := os.Stat(filepath.Join(repo, "swarm.key")); info.Mode().Perm() != 0o600 {
		t.Errorf("swarm.key has mode %o", info.Mode().Perm())
	}

	// Run offline again, the daemon keeps neither the key nor the members.
	if err := configure(file, ident, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo, "swarm.key")); !os.IsNotExist(err) {
		t.Errorf("an offline daemon still has a swarm key (stat: %v)", err)
	}
	config, _ = os.ReadFile(file)
	if strings.Contains(string(config), "12D3KooWAAAA") {
		t.Errorf("an offline daemon still lists swarm members:\n%s", config)
	}

	for _, bad := range []string{"/ip4/10.0.0.5/tcp/4101", "/p2p/12D3KooWAAAA", "/ip4/10.0.0.5/tcp/4101/p2p/"} {
		if err := configure(file, ident, &Swarm{Key: swarm.Key, Peers: []string{bad}}); err == nil {
			t.Errorf("accepted the member address %q", bad)
		}
	}
	// The key cannot be written.
	os.Remove(filepath.Join(repo, "swarm.key"))
	os.Mkdir(filepath.Join(repo, "swarm.key"), 0o700)
	if err := configure(file, ident, swarm); err == nil {
		t.Error("configured a swarm whose key could not be stored")
	}
}
