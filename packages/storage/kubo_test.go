package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multihash"

	"github.com/excho0/Sisyphus/packages/identity"
	"github.com/excho0/Sisyphus/packages/kubo"
)

// startKubo runs a real Kubo daemon on repo for the length of the test,
// skipping the test if ipfs is not installed, or failing it if
// SISYPHUS_REQUIRE_KUBO says it must be.
func startKubo(t *testing.T, repo string) *kubo.Daemon {
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
	d, err := kubo.Start(ctx, kubo.Config{Repo: repo, Identity: ident})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Stop)
	return d
}

func openKubo(t *testing.T, api KuboAPI, dir string) *Store {
	t.Helper()
	s, err := OpenKubo(api, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// ipfs runs the ipfs command against a repository whose daemon is running.
func ipfs(t *testing.T, repo string, stdin []byte, args ...string) []byte {
	t.Helper()
	cmd := exec.Command("ipfs", args...)
	cmd.Env = append(os.Environ(), "IPFS_PATH="+repo)
	cmd.Stdin = bytes.NewReader(stdin)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("ipfs %v: %v", args, err)
	}
	return out
}

func TestWhatTheStorePutsInKuboIsAnOrdinaryIPFSFile(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "ipfs")
	s := openKubo(t, startKubo(t, repo), t.TempDir())

	for _, v := range kuboVectors[:5] { // all but the 50 MB one
		c, err := s.Put(ctx, bytes.NewReader(v.data))
		if err != nil {
			t.Fatalf("%s: %v", v.name, err)
		}
		if c.String() != v.cid {
			t.Errorf("%s: stored as %s, want %s", v.name, c, v.cid)
		}
		if got := ipfs(t, repo, nil, "cat", c.String()); !bytes.Equal(got, v.data) {
			t.Errorf("%s: ipfs cat returned %d bytes that differ from the %d stored", v.name, len(got), len(v.data))
		}
		if !intact(t, s, c, v.data) {
			t.Errorf("%s: the store does not read back what it stored", v.name)
		}
		if err := s.Verify(ctx, c); err != nil {
			t.Errorf("%s: Verify: %v", v.name, err)
		}
	}
}

func TestTheStoreReadsWhatTheIPFSCommandAdded(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "ipfs")
	s := openKubo(t, startKubo(t, repo), t.TempDir())
	data := pattern(700_000)

	added := strings.TrimSpace(string(ipfs(t, repo, data, "add", "--quieter", "--cid-version=1", "--pin=false")))
	c, err := cid.Decode(added)
	if err != nil {
		t.Fatal(err)
	}
	if !intact(t, s, c, data) {
		t.Error("the store cannot read a file added with the ipfs command")
	}
	blob, err := s.Open(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	defer blob.Close()
	if blob.Size() != uint64(len(data)) {
		t.Errorf("size %d, want %d", blob.Size(), len(data))
	}
	if _, err := blob.Seek(600_000, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	tail, _ := io.ReadAll(blob)
	if !bytes.Equal(tail, data[600_000:]) {
		t.Error("a read after a seek returned the wrong bytes")
	}
}

func TestPinsAndCollectionWorkOverKubo(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "ipfs")
	daemon := startKubo(t, repo)
	dir := t.TempDir()
	s := openKubo(t, daemon, dir)
	empty, err := s.Size(ctx)
	if err != nil {
		t.Fatal(err)
	}

	kept, dropped := put(t, s, blobA), put(t, s, blobB)
	if err := s.Pin(ctx, "user", time.Time{}, kept); err != nil {
		t.Fatal(err)
	}
	absent, _ := CID(ctx, strings.NewReader("never stored"))
	if err := s.Pin(ctx, "user", time.Time{}, absent); !errors.Is(err, ErrNotFound) {
		t.Errorf("pinning a blob Kubo lacks: %v, want ErrNotFound", err)
	}
	if _, err := s.Open(ctx, absent); !errors.Is(err, ErrNotFound) {
		t.Errorf("opening a blob Kubo lacks: %v, want ErrNotFound", err)
	}
	if full, _ := s.Size(ctx); full < empty+uint64(len(blobA)+len(blobB)) {
		t.Errorf("size went from %d to %d after storing %d bytes", empty, full, len(blobA)+len(blobB))
	}

	done := gc(t, s, afterGrace())
	if done.Bytes < uint64(len(blobB)) {
		t.Errorf("collection freed %d bytes, the unpinned blob is %d", done.Bytes, len(blobB))
	}
	if !intact(t, s, kept, blobA) {
		t.Error("the pinned blob was damaged")
	}
	if has, _ := s.Has(ctx, dropped); has {
		t.Error("the unpinned blob survived")
	}
	// Kubo itself no longer has it either.
	if _, err := daemon.BlockSize(ctx, dropped.String()); !errors.Is(err, kubo.ErrNotFound) {
		t.Errorf("Kubo still holds the collected blob's root: %v", err)
	}

	// Pins outlive the store being closed and reopened.
	s.Close()
	reopened := openKubo(t, daemon, dir)
	gc(t, reopened, afterGrace())
	if !intact(t, reopened, kept, blobA) {
		t.Error("a blob pinned before reopening was collected after it")
	}
}

func TestOpeningAKuboStoreFails(t *testing.T) {
	api := &fakeKubo{}
	file := filepath.Join(t.TempDir(), "a-file")
	os.WriteFile(file, nil, 0o600)
	if _, err := OpenKubo(api, filepath.Join(file, "store")); err == nil {
		t.Error("opened a store beneath a regular file")
	}

	lockIsDir := t.TempDir()
	os.Mkdir(filepath.Join(lockIsDir, "LOCK"), 0o700)
	if _, err := OpenKubo(api, lockIsDir); err == nil {
		t.Error("opened a store whose lock file cannot be created")
	}

	dir := t.TempDir()
	first := openKubo(t, api, dir)
	if second, err := OpenKubo(api, dir); err == nil {
		second.Close()
		t.Error("opened a store directory that is already open")
	}
	first.Close()

	badPins := t.TempDir()
	os.WriteFile(filepath.Join(badPins, "pins.json"), []byte("not json"), 0o600)
	if _, err := OpenKubo(api, badPins); err == nil {
		t.Error("opened a store whose pins cannot be read")
	}
	// The failed open released the directory.
	os.Remove(filepath.Join(badPins, "pins.json"))
	openKubo(t, api, badPins)
}

// fakeKubo is a Kubo that can be made to fail or to answer wrongly.
type fakeKubo struct {
	blocks map[string][]byte
	// fail, if set, is returned by every call.
	fail error
	// putAs, if set, is the CID every stored block is reported under.
	putAs string
	// extraRefs are listed alongside the blocks actually held.
	extraRefs []string
	// pinned are the CIDs Kubo reports as pinned by itself, and failPinned
	// makes that report fail.
	pinned     []string
	failPinned error
}

func (f *fakeKubo) Pinned(_ context.Context, visit func(string)) error {
	for _, id := range f.pinned {
		visit(id)
	}
	return f.failPinned
}

func (f *fakeKubo) BlockPut(_ context.Context, codec string, data []byte) (string, error) {
	if f.fail != nil {
		return "", f.fail
	}
	if f.putAs != "" {
		return f.putAs, nil
	}
	kind := map[string]uint64{"raw": cid.Raw, "dag-pb": cid.DagProtobuf}[codec]
	c, _ := cid.V1Builder{Codec: kind, MhType: multihash.SHA2_256}.Sum(data)
	if f.blocks == nil {
		f.blocks = make(map[string][]byte)
	}
	f.blocks[c.String()] = data
	return c.String(), nil
}

func (f *fakeKubo) BlockGet(_ context.Context, id string) ([]byte, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	data, ok := f.blocks[id]
	if !ok {
		return nil, kubo.ErrNotFound
	}
	return data, nil
}

func (f *fakeKubo) BlockSize(ctx context.Context, id string) (int, error) {
	data, err := f.BlockGet(ctx, id)
	return len(data), err
}

func (f *fakeKubo) BlockRemove(_ context.Context, id string) error {
	delete(f.blocks, id)
	return f.fail
}

func (f *fakeKubo) LocalBlocks(_ context.Context, visit func(string) bool) error {
	if f.fail != nil {
		return f.fail
	}
	for id := range f.blocks {
		visit(id)
	}
	for _, id := range f.extraRefs {
		visit(id)
	}
	return nil
}

func (f *fakeKubo) RepoSize(context.Context) (uint64, error) { return 0, f.fail }

var errKubo = errors.New("kubo went away")

func TestKuboStoreReportsADaemonThatFailsOrLies(t *testing.T) {
	api := &fakeKubo{}
	s := openKubo(t, api, t.TempDir())
	c := put(t, s, blobA)

	// Kubo naming a block differently from the store means one of them has
	// the bytes wrong.
	api.putAs = "bafkreigh2akiscaildcqabsyg3dfr6chu3fgpregiymsck7e7aqa4s52zy"
	if _, err := s.Put(ctx, strings.NewReader("more")); err == nil || !strings.Contains(err.Error(), "Kubo stored block") {
		t.Errorf("Put when Kubo reports another CID: %v", err)
	}
	api.putAs = ""

	api.fail = errKubo
	if _, err := s.Put(ctx, strings.NewReader("more")); !errors.Is(err, errKubo) {
		t.Errorf("Put: %v", err)
	}
	if _, err := s.Open(ctx, c); !errors.Is(err, errKubo) {
		t.Errorf("Open: %v", err)
	}
	if _, err := s.Has(ctx, c); !errors.Is(err, errKubo) {
		t.Errorf("Has: %v", err)
	}
	if _, err := s.GC(ctx, afterGrace()); !errors.Is(err, errKubo) {
		t.Errorf("GC: %v", err)
	}
	if _, err := s.Size(ctx); !errors.Is(err, errKubo) {
		t.Errorf("Size: %v", err)
	}
	api.fail = nil

	// An entry in either of Kubo's listings that is not a CID is skipped,
	// not fatal.
	api.extraRefs, api.pinned = []string{"not-a-cid"}, []string{"nor-is-this"}
	if _, err := s.GC(ctx, afterGrace()); err != nil {
		t.Errorf("GC with malformed entries in the listings: %v", err)
	}
	api.failPinned = errKubo
	if _, err := s.GC(ctx, afterGrace()); !errors.Is(err, errKubo) {
		t.Errorf("GC when Kubo cannot say what it has pinned: %v", err)
	}
}

func TestKuboBlocksRejectFormatsTheStoreDoesNotWrite(t *testing.T) {
	adapter := kuboBlocks{&fakeKubo{}}
	c, _ := cid.V1Builder{Codec: cid.DagCBOR, MhType: multihash.SHA2_256}.Sum([]byte("cbor"))
	block, _ := blocks.NewBlockWithCid([]byte("cbor"), c)
	if err := adapter.PutMany(ctx, []blocks.Block{block}); err == nil || !strings.Contains(err.Error(), "neither raw nor dag-pb") {
		t.Errorf("error %v, want the format refused", err)
	}
	raw, _ := cid.V1Builder{Codec: cid.Raw, MhType: multihash.SHA2_256}.Sum([]byte("raw"))
	rawBlock, _ := blocks.NewBlockWithCid([]byte("raw"), raw)
	if err := adapter.PutMany(ctx, []blocks.Block{rawBlock}); err != nil {
		t.Errorf("PutMany of a raw block: %v", err)
	}
	if size, err := adapter.GetSize(ctx, raw); err != nil || size != 3 {
		t.Errorf("GetSize = %d, %v", size, err)
	}
	if err := (kuboBlocks{&fakeKubo{fail: errKubo}}).Put(ctx, rawBlock); !errors.Is(err, errKubo) {
		t.Errorf("Put when Kubo fails: %v", err)
	}
}

// Someone using the ipfs command on the same daemon can keep things by
// pinning them there, and the store must not delete them from under Kubo.
func TestCollectionLeavesAloneWhatKuboItselfHasPinned(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "ipfs")
	s := openKubo(t, startKubo(t, repo), t.TempDir())

	theirs := pattern(700_000)
	pinnedInKubo := strings.TrimSpace(string(ipfs(t, repo, theirs, "add", "--quieter", "--cid-version=1")))
	ours := put(t, s, blobB)

	gc(t, s, afterGrace())

	if has, _ := s.Has(ctx, ours); has {
		t.Error("the store's own unpinned blob survived")
	}
	if got := ipfs(t, repo, nil, "cat", pinnedInKubo); !bytes.Equal(got, theirs) {
		t.Error("a file pinned with the ipfs command was damaged by the store's collection")
	}
	// Kubo's own bookkeeping is still sound.
	if out := string(ipfs(t, repo, nil, "pin", "verify")); strings.Contains(out, "broken") {
		t.Errorf("ipfs pin verify reports damage:\n%s", out)
	}
}

func TestKuboCacheKeepsItsPinsInMemory(t *testing.T) {
	api := &fakeKubo{}
	cache := OpenKuboCache(api)
	c := put(t, cache, blobA)
	if err := cache.Pin(ctx, "cache", time.Time{}, c); err != nil {
		t.Fatal(err)
	}
	gc(t, cache, afterGrace())
	if !intact(t, cache, c, blobA) {
		t.Error("a pinned blob was collected from the cache")
	}
	if err := cache.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	// A new cache over the same daemon starts with nothing pinned.
	if pins := OpenKuboCache(api).Pins(); len(pins) != 0 {
		t.Errorf("a new cache has pins: %v", pins)
	}
}

// A worker's cache on one member of a swarm must be able to get a blob that
// only another member holds, just by asking for it.
func TestAKuboCacheFetchesFromOtherMembersOfItsSwarm(t *testing.T) {
	if _, err := exec.LookPath("ipfs"); err != nil {
		if os.Getenv("SISYPHUS_REQUIRE_KUBO") != "" {
			t.Fatal("ipfs is not installed, and SISYPHUS_REQUIRE_KUBO is set")
		}
		t.Skip("ipfs is not installed")
	}
	start := func(key string, peers ...string) *kubo.Daemon {
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
	key := kubo.NewSwarmKey()
	holder := start(key)
	addresses, err := holder.Addresses(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fetcher := start(key, addresses...)

	held := openKubo(t, holder, t.TempDir())
	cache := OpenKuboCache(fetcher)
	c := put(t, held, blobA)
	if has, _ := cache.Has(ctx, c); has {
		t.Fatal("the cache holds the blob before asking for it")
	}

	if err := cache.Verify(ctx, c); err != nil {
		t.Fatalf("verifying a blob another member holds: %v", err)
	}
	if !intact(t, cache, c, blobA) {
		t.Error("the blob fetched through the swarm differs from the original")
	}
	// It is now held locally, and can be pinned.
	if err := cache.Pin(ctx, "cache", time.Time{}, c); err != nil {
		t.Errorf("pinning a blob fetched through the swarm: %v", err)
	}

	// A blob nobody holds is reported missing once the swarm has been asked.
	absent, _ := CID(ctx, strings.NewReader("held by nobody"))
	if err := cache.Verify(ctx, absent); err == nil {
		t.Error("Verify passed a blob no member holds")
	}
	if _, err := cache.Open(ctx, absent); !errors.Is(err, ErrNotFound) {
		t.Errorf("opening a blob no member holds: %v, want ErrNotFound", err)
	}
}
