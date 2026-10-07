package worker_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/worker"
	"github.com/sisyphus-network/Sisyphus/packages/identity"
	"github.com/sisyphus-network/Sisyphus/packages/kubo"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// swarmMember starts a real Kubo daemon on a private network, skipping the
// test if ipfs is not installed, or failing it if SISYPHUS_REQUIRE_KUBO says
// it must be.
func swarmMember(t *testing.T, key string, peers ...string) *kubo.Daemon {
	t.Helper()
	if _, err := exec.LookPath("ipfs"); err != nil {
		if os.Getenv("SISYPHUS_REQUIRE_KUBO") != "" {
			t.Fatal("ipfs is not installed, and SISYPHUS_REQUIRE_KUBO is set")
		}
		t.Skip("ipfs is not installed")
	}
	ident, _, err := identity.LoadOrCreate(filepath.Join(t.TempDir(), "node.key"))
	if err != nil {
		t.Fatal(err)
	}
	d, err := kubo.Start(ctx, kubo.Config{
		Repo: filepath.Join(t.TempDir(), "ipfs"), Identity: ident,
		Swarm: &kubo.Swarm{Key: key, Peers: peers, PeerTimeout: 3 * time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Stop)
	return d
}

// A worker whose cache is on the pool's private network gets a task's input
// from the network. To be sure that is where it came from, the coordinator's
// own blob service here hands out the wrong bytes for everything.
func TestWorkerOnASwarmGetsItsInputFromTheSwarmNotTheCoordinator(t *testing.T) {
	key := kubo.NewSwarmKey()
	holder := swarmMember(t, key)
	addresses, err := holder.Addresses(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fetcher := swarmMember(t, key, addresses...)

	held, err := storage.OpenKubo(holder, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	c, err := held.Put(ctx, bytes.NewReader(cachedData))
	if err != nil {
		t.Fatal(err)
	}

	srv := newServer()
	pb.RegisterBlobServiceServer(srv, impostor{})
	addr, _ := serve(t, srv)
	blobs, err := worker.DialBlobs(addr, workerCreds, storage.OpenKuboCache(fetcher), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer blobs.Close()

	blob, err := blobs.Open(ctx, c)
	if err != nil {
		t.Fatalf("opening a blob another member of the swarm holds: %v", err)
	}
	if got := read(t, blob); !bytes.Equal(got, cachedData) {
		t.Errorf("read %d bytes that differ from the %d the other member holds", len(got), len(cachedData))
	}
}
