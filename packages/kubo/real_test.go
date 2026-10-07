package kubo

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// These tests run the real ipfs program.

// ipfs runs an ipfs command against a repository, as a person at a terminal
// would, and returns what it printed.
func ipfs(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("ipfs", args...)
	cmd.Env = append(os.Environ(), "IPFS_PATH="+repo)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("ipfs %v: %v", args, err)
	}
	return string(out)
}

func TestKuboRunsAsTheNodeItServes(t *testing.T) {
	ident := newIdentity(t)
	d := startKubo(t, filepath.Join(t.TempDir(), "ipfs"), ident)

	id, err := d.ID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if id != ident.ID() {
		t.Errorf("Kubo is peer %s, the node is %s", id, ident.ID())
	}
}

func TestBlocksGoInAndComeOutOfKubo(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "ipfs")
	d := startKubo(t, repo, newIdentity(t))
	data := []byte("a block of data")

	id, err := d.BlockPut(ctx, "raw", data)
	if err != nil {
		t.Fatal(err)
	}
	// The CID that hashing the same bytes with the ipfs command gives.
	if want := strings.TrimSpace(ipfsStdin(t, repo, data, "add", "--only-hash", "--quieter", "--cid-version=1")); id != want {
		t.Errorf("stored as %s, ipfs names the same bytes %s", id, want)
	}

	got, err := d.BlockGet(ctx, id)
	if err != nil || !bytes.Equal(got, data) {
		t.Errorf("BlockGet = %q, %v", got, err)
	}
	if size, err := d.BlockSize(ctx, id); err != nil || size != len(data) {
		t.Errorf("BlockSize = %d, %v; want %d", size, err, len(data))
	}
	// The ipfs command sees it as an ordinary object.
	if out := ipfs(t, repo, "cat", id); out != string(data) {
		t.Errorf("ipfs cat printed %q", out)
	}

	var listed []string
	if err := d.LocalBlocks(ctx, func(id string) bool { listed = append(listed, id); return true }); err != nil {
		t.Fatal(err)
	}
	sort.Strings(listed)
	if i := sort.SearchStrings(listed, id); i == len(listed) || listed[i] != id {
		t.Errorf("the block is not among the %d listed", len(listed))
	}

	if err := d.BlockRemove(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := d.BlockGet(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Errorf("BlockGet of a removed block: %v, want ErrNotFound", err)
	}
	if _, err := d.BlockSize(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Errorf("BlockSize of a removed block: %v, want ErrNotFound", err)
	}
	if err := d.BlockRemove(ctx, id); err != nil {
		t.Errorf("removing a block that is not there: %v", err)
	}
}

func ipfsStdin(t *testing.T, repo string, stdin []byte, args ...string) string {
	t.Helper()
	cmd := exec.Command("ipfs", args...)
	cmd.Env = append(os.Environ(), "IPFS_PATH="+repo)
	cmd.Stdin = bytes.NewReader(stdin)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("ipfs %v: %v", args, err)
	}
	return string(out)
}

func TestKuboKeepsItsBlocksAndIdentityAcrossRestarts(t *testing.T) {
	requireKubo(t)
	repo := filepath.Join(t.TempDir(), "ipfs")
	ident := newIdentity(t)

	first, err := Start(ctx, Config{Repo: repo, Identity: ident})
	if err != nil {
		t.Fatal(err)
	}
	id, err := first.BlockPut(ctx, "raw", []byte("kept"))
	if err != nil {
		t.Fatal(err)
	}
	before, err := first.RepoSize(ctx)
	if err != nil || before == 0 {
		t.Errorf("RepoSize = %d, %v", before, err)
	}
	first.Stop()
	if _, err := first.ID(ctx); err == nil {
		t.Fatal("the daemon still answers after Stop")
	}

	second := startKubo(t, repo, ident)
	if got, err := second.BlockGet(ctx, id); err != nil || string(got) != "kept" {
		t.Errorf("after a restart BlockGet = %q, %v", got, err)
	}
	if peer, _ := second.ID(ctx); peer != ident.ID() {
		t.Errorf("after a restart Kubo is peer %s, want %s", peer, ident.ID())
	}
}

func TestPinnedListsWhatKuboHasPinned(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "ipfs")
	d := startKubo(t, repo, newIdentity(t))
	added := strings.TrimSpace(ipfsStdin(t, repo, []byte("pinned with the ipfs command"), "add", "--quieter", "--cid-version=1"))
	loose, err := d.BlockPut(ctx, "raw", []byte("stored but not pinned"))
	if err != nil {
		t.Fatal(err)
	}

	pinned := make(map[string]bool)
	if err := d.Pinned(ctx, func(id string) { pinned[id] = true }); err != nil {
		t.Fatal(err)
	}
	if !pinned[added] {
		t.Errorf("%s was added with a pin but is not listed: %v", added, pinned)
	}
	if pinned[loose] {
		t.Errorf("%s was stored without a pin but is listed", loose)
	}
}

func TestKuboIsKeptOffThePublicNetwork(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "ipfs")
	startKubo(t, repo, newIdentity(t))

	if peers := strings.TrimSpace(ipfs(t, repo, "bootstrap", "list")); peers != "" {
		t.Errorf("the daemon would bootstrap from:\n%s", peers)
	}
	config, err := os.ReadFile(filepath.Join(repo, "config"))
	if err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(filepath.Join(repo, "config")); info.Mode().Perm() != 0o600 {
		t.Errorf("the config, which holds the node's key, has mode %o", info.Mode().Perm())
	}
	for _, want := range []string{`"Gateway": []`, `"Swarm": []`, `"API": "/ip4/127.0.0.1/tcp/0"`} {
		if !strings.Contains(string(config), want) {
			t.Errorf("config lacks %s", want)
		}
	}
}
