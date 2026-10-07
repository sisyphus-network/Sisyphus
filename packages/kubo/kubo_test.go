package kubo

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sisyphus-network/Sisyphus/packages/identity"
)

var ctx = context.Background()

// requireKubo skips a test that needs the real ipfs program when it is not
// installed. With SISYPHUS_REQUIRE_KUBO set, as in CI, it fails instead, so
// that these tests cannot go unrun unnoticed.
func requireKubo(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ipfs"); err != nil {
		if os.Getenv("SISYPHUS_REQUIRE_KUBO") != "" {
			t.Fatal("ipfs is not installed, and SISYPHUS_REQUIRE_KUBO is set")
		}
		t.Skip("ipfs is not installed")
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

// startKubo runs a real Kubo daemon for the length of the test.
func startKubo(t *testing.T, repo string, ident *identity.Identity) *Daemon {
	t.Helper()
	requireKubo(t)
	d, err := Start(ctx, Config{Repo: repo, Identity: ident})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Stop)
	return d
}
