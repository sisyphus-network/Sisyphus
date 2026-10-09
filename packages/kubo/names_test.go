package kubo

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ipfs/go-cid"

	"github.com/sisyphus-network/Sisyphus/packages/identity"
	"github.com/sisyphus-network/Sisyphus/packages/names"
)

// namedMember starts a real Kubo daemon on a private network, as a node
// whose key the test holds.
func namedMember(t *testing.T, key string, peers ...string) (*Daemon, *identity.Identity, string) {
	t.Helper()
	requireKubo(t)
	ident, repo := newIdentity(t), filepath.Join(t.TempDir(), "ipfs")
	d, err := Start(ctx, Config{Repo: repo, Identity: ident, Swarm: &Swarm{Key: key, Peers: peers, PeerTimeout: 3 * time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Stop)
	return d, ident, repo
}

func TestARecordHandedToOneMemberIsResolvedByAnother(t *testing.T) {
	key := NewSwarmKey()
	first, ident, firstRepo := namedMember(t, key)
	second, _, secondRepo := namedMember(t, key, addressesOf(t, first)...)
	connected(t, second, first)

	id, err := first.BlockPut(ctx, "raw", []byte("what the name stands for"))
	if err != nil {
		t.Fatal(err)
	}
	value, _ := cid.Decode(id)
	record := names.Make(ident, value, 1, time.Hour, time.Minute, time.Now())
	if err := first.PutName(ctx, ident.ID(), record.Bytes()); err != nil {
		t.Fatal(err)
	}
	for _, repo := range []string{firstRepo, secondRepo} {
		if got := strings.TrimSpace(ipfs(t, repo, "name", "resolve", "/ipns/"+ident.ID())); got != "/ipfs/"+id {
			t.Errorf("ipfs name resolve: %q, want /ipfs/%s", got, id)
		}
	}
}
