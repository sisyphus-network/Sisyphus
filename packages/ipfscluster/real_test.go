package ipfscluster

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sisyphus-network/Sisyphus/packages/identity"
	"github.com/sisyphus-network/Sisyphus/packages/kubo"
)

// These tests run the real ipfs-cluster-service and ipfs programs.

var ctx = context.Background()

// requireCluster skips a test that needs the real programs when they are
// not installed. With SISYPHUS_REQUIRE_CLUSTER or SISYPHUS_REQUIRE_KUBO set,
// as in CI, it fails instead, so that these tests cannot go unrun unnoticed.
func requireCluster(t *testing.T) {
	t.Helper()
	for program, required := range map[string]string{"ipfs-cluster-service": "SISYPHUS_REQUIRE_CLUSTER", "ipfs": "SISYPHUS_REQUIRE_KUBO"} {
		if _, err := exec.LookPath(program); err != nil {
			if os.Getenv(required) != "" {
				t.Fatalf("%s is not installed, and %s is set", program, required)
			}
			t.Skipf("%s is not installed", program)
		}
	}
}

func newIdentity(t *testing.T) *identity.Identity {
	t.Helper()
	ident, _, err := identity.LoadOrCreate(filepath.Join(t.TempDir(), "node.key"))
	if err != nil {
		t.Fatal(err)
	}
	return ident
}

// testSecret is the secret of the cluster these tests form.
const testSecret = "5a1f6c0d9e8b7a6f5e4d3c2b1a0f9e8d7c6b5a4f3e2d1c0b9a8f7e6d5c4b3a2f"

// member is one node of a test pool: a Kubo daemon on the pool's private
// network and a cluster peer that pins on it.
type member struct {
	id   string
	repo string
	kubo *kubo.Daemon
	*Daemon
}

// pool is a cluster under test: a first member, the only one trusted to say
// what is pinned, and the followers that have joined it.
type pool struct {
	t        *testing.T
	swarmKey string
	members  []*member
}

// join starts a member and connects it to the first, if it is not the first.
func (p *pool) join(name string) *member {
	p.t.Helper()
	ident := newIdentity(p.t)
	dir := p.t.TempDir()
	m := &member{id: ident.ID(), repo: filepath.Join(dir, "ipfs")}
	swarm := &kubo.Swarm{Key: p.swarmKey, Loopback: true, PeerTimeout: 5 * time.Second}
	cfg := Config{
		Dir: filepath.Join(dir, "cluster"), Identity: ident, Name: name, Secret: testSecret,
		Trusted: []string{ident.ID()}, Heartbeat: time.Second,
	}
	if len(p.members) > 0 {
		first := p.members[0]
		swarm.Peers = addressesOf(p.t, first.kubo.Addresses)
		self, err := first.ID(ctx)
		if err != nil {
			p.t.Fatal(err)
		}
		cfg.Trusted, cfg.Follower, cfg.Peers = []string{first.id}, true, self.Addresses
	}
	var err error
	if m.kubo, err = kubo.Start(ctx, kubo.Config{Repo: m.repo, Identity: ident, Swarm: swarm}); err != nil {
		p.t.Fatal(err)
	}
	p.t.Cleanup(m.kubo.Stop)
	cfg.KuboAPI = m.kubo.Address()
	if m.Daemon, err = Start(ctx, cfg); err != nil {
		p.t.Fatal(err)
	}
	p.t.Cleanup(m.Stop)
	p.members = append(p.members, m)
	return m
}

func addressesOf(t *testing.T, ask func(context.Context) ([]string, error)) []string {
	t.Helper()
	addresses, err := ask(ctx)
	if err != nil || len(addresses) == 0 {
		t.Fatalf("addresses: %v, %v", addresses, err)
	}
	return addresses
}

// eventually waits for a condition that a cluster reaches in its own time.
func eventually(t *testing.T, what string, met func() bool) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	defer func(started time.Time) { t.Logf("%s: after %v", what, time.Since(started).Round(100*time.Millisecond)) }(time.Now())
	for !met() {
		if time.Now().After(deadline) {
			t.Fatalf("never happened: %s", what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// add stores a file in a member's Kubo without pinning it there, and
// returns its CID.
func (m *member) add(t *testing.T, content string) string {
	t.Helper()
	cmd := exec.Command("ipfs", "add", "--quieter", "--cid-version=1", "--pin=false")
	cmd.Env = append(os.Environ(), "IPFS_PATH="+m.repo)
	cmd.Stdin = strings.NewReader(content)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("ipfs add: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// holds says whether a member's Kubo has all of a file, without asking
// anyone else for it.
func (m *member) holds(content, id string) bool {
	cmd := exec.Command("ipfs", "cat", "--offline", id)
	cmd.Env = append(os.Environ(), "IPFS_PATH="+m.repo)
	out, err := cmd.Output()
	return err == nil && bytes.Equal(out, []byte(content))
}

// holders asks a member which members hold a pin, in order of ID.
func holders(t *testing.T, asked *member, id string) []string {
	t.Helper()
	all, err := asked.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range all {
		if status.CID == id {
			found := status.Holders()
			slices.Sort(found)
			return found
		}
	}
	return nil
}

func TestARestartedPeerKeepsItsIdentityAndItsPins(t *testing.T) {
	requireCluster(t)
	p := &pool{t: t, swarmKey: kubo.NewSwarmKey()}
	only := p.join("only")
	id := only.add(t, "kept across a change of secret")
	if _, err := only.Pin(ctx, id, PinOptions{Min: 1, Max: 1}); err != nil {
		t.Fatal(err)
	}
	before := NewClient(strings.TrimPrefix(only.base, "http://"), only.password)

	const another = "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0"
	if err := only.Restart(ctx, another, only.kubo.Address(), nil); err != nil {
		t.Fatal(err)
	}
	if self, err := only.ID(ctx); err != nil || self.ID != only.id {
		t.Errorf("after a restart the peer is %+v, %v; want ID %s", self, err, only.id)
	}
	if pins, err := only.Pins(ctx); err != nil || len(pins) != 1 || pins[0].CID != id {
		t.Errorf("after a restart the peer pins %+v, %v", pins, err)
	}
	if settings := string(readFile(filepath.Join(only.cfg.Dir, "service.json"))); !strings.Contains(settings, another) || strings.Contains(settings, testSecret) {
		t.Error("the peer's settings do not hold the new secret in place of the old")
	}
	// The API is not where it was, nor opened by the password it had.
	if _, err := before.ID(ctx); err == nil {
		t.Error("the peer still answers where it did before the restart, to the password it had then")
	}
}

func TestAClusterKeepsAsManyCopiesAsAskedAndOnlyItsTrustedMemberSaysWhat(t *testing.T) {
	requireCluster(t)
	p := &pool{t: t, swarmKey: kubo.NewSwarmKey()}
	first := p.join("first")
	second, third := p.join("second"), p.join("third")

	// Each peer is the node it serves, and the three find each other.
	if self, err := second.ID(ctx); err != nil || self.ID != second.id || self.Name != "second" {
		t.Errorf("the second peer is %+v, %v; want ID %s", self, err, second.id)
	}
	eventually(t, "the first member hears from the other two", func() bool {
		peers, err := first.Peers(ctx)
		return err == nil && len(peers) == 3
	})

	content := strings.Repeat("the boulder rolls down, and Sisyphus walks after it.\n", 20_000)
	id := first.add(t, content)
	pinned, err := first.Pin(ctx, id, PinOptions{Name: "boulder", Min: 2, Max: 2, Prefer: []string{first.id}})
	if err != nil {
		t.Fatal(err)
	}
	if pinned.CID != id || pinned.Min != 2 || pinned.Max != 2 || len(pinned.Allocations) != 2 || !slices.Contains(pinned.Allocations, first.id) {
		t.Errorf("pinned as %+v, want two holders, the first member among them", pinned)
	}
	// The other holder is whichever follower the cluster chose.
	holder, spare := second, third
	if slices.Contains(pinned.Allocations, third.id) {
		holder, spare = third, second
	}
	want := []string{first.id, holder.id}
	slices.Sort(want)
	eventually(t, "the two chosen members hold the file", func() bool { return slices.Equal(holders(t, first, id), want) })
	if !holder.holds(content, id) {
		t.Error("the cluster says a follower holds the file, but its Kubo does not have all of it")
	}
	if spare.holds(content, id) {
		t.Error("the member that was not chosen holds the file as well")
	}
	// Any member can be asked what is pinned.
	eventually(t, "the followers learn of the pin", func() bool {
		pins, err := spare.Pins(ctx)
		return err == nil && len(pins) == 1 && pins[0].CID == id && pins[0].Name == "boulder"
	})

	// A follower can change nothing.
	other := first.add(t, "something a follower would like the pool to keep")
	if _, err := holder.Pin(ctx, other, PinOptions{Min: 1, Max: 1}); err == nil || !strings.Contains(err.Error(), "follower") {
		t.Errorf("a follower pinning: %v, want a refusal", err)
	}
	if err := holder.Unpin(ctx, id); err == nil || !strings.Contains(err.Error(), "follower") {
		t.Errorf("a follower unpinning: %v, want a refusal", err)
	}
	if pins, err := first.Pins(ctx); err != nil || len(pins) != 1 || pins[0].CID != id {
		t.Errorf("after a follower's attempts the cluster pins %+v, %v", pins, err)
	}

	// When a holder goes, the member left over takes its place.
	holder.Stop()
	want = []string{first.id, spare.id}
	slices.Sort(want)
	eventually(t, "the remaining follower takes over from the one that left", func() bool { return slices.Equal(holders(t, first, id), want) })
	if !spare.holds(content, id) {
		t.Error("the cluster says the remaining follower holds the file, but its Kubo does not have all of it")
	}

	// Unpinned by the trusted member, it is let go everywhere.
	if err := first.Unpin(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := first.Unpin(ctx, id); err != nil {
		t.Errorf("unpinning what is no longer pinned: %v", err)
	}
	eventually(t, "the follower's Kubo lets go of the file", func() bool {
		still := false
		spare.kubo.Pinned(ctx, func(pinned string) { still = still || pinned == id })
		pins, err := spare.Pins(ctx)
		return err == nil && len(pins) == 0 && !still
	})
}
