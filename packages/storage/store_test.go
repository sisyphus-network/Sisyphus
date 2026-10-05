package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/ipfs/boxo/ipld/merkledag"
	blocks "github.com/ipfs/go-block-format"
)

var ctx = context.Background()

// pattern returns n deterministic, non-repeating bytes, so that no two chunks
// of a blob are alike and a misordered or dropped chunk changes its CID.
func pattern(n int) []byte {
	b := make([]byte, n)
	state := uint64(1)
	for i := range b {
		state = state*6364136223846793005 + 1442695040888963407
		b[i] = byte(state >> 56)
	}
	return b
}

// kuboVectors are the CIDs that Kubo 0.43.1 reports for these inputs with
// `ipfs add --only-hash --cid-version=1`, covering every shape of DAG: no
// data, one raw block, a root with a few leaves, and a root with more leaves
// than fit under one node.
var kuboVectors = []struct {
	name string
	data []byte
	cid  string
}{
	{"empty", nil, "bafkreihdwdcefgh4dqkjv67uzcmw7ojee6xedzdetojuzjevtenxquvyku"},
	{"hello", []byte("hello world\n"), "bafkreifjjcie6lypi6ny7amxnfftagclbuxndqonfipmb64f2km2devei4"},
	{"exactly one chunk", pattern(262144), "bafkreiha25bfy6hjxjlak3z5gyfld3ledvizgmbc66evawrm4lynhav6ei"},
	{"one chunk plus a byte", pattern(262145), "bafybeic7wvg5ivbiztaw5wht4zlc2tryzjzwaoynlhiykbmubosjnjmthu"},
	{"a megabyte", pattern(1_000_000), "bafybeiczwqoyribvfkrxcuhcjnwva43kuvcrl2h57ergkeszwguynobh5a"},
	{"two-level tree", pattern(50_000_000), "bafybeiesku5rwyxrssjox7hjykfrpwphbh3eusdpc4xayiz3ovzp6pah3a"},
}

func TestPutGivesTheSameCIDAsKubo(t *testing.T) {
	store := NewMemory()
	for _, v := range kuboVectors {
		got, err := store.Put(ctx, bytes.NewReader(v.data))
		if err != nil {
			t.Fatalf("%s: %v", v.name, err)
		}
		if got.String() != v.cid {
			t.Errorf("%s: CID %s, Kubo gives %s", v.name, got, v.cid)
		}
	}
}

func TestCIDMatchesPutWithoutStoring(t *testing.T) {
	for _, v := range kuboVectors {
		got, err := CID(ctx, bytes.NewReader(v.data))
		if err != nil {
			t.Fatalf("%s: %v", v.name, err)
		}
		if got.String() != v.cid {
			t.Errorf("%s: CID %s, want %s", v.name, got, v.cid)
		}
	}
}

func TestBlobsReadBackIntact(t *testing.T) {
	store := NewMemory()
	for _, v := range kuboVectors {
		c, err := store.Put(ctx, bytes.NewReader(v.data))
		if err != nil {
			t.Fatal(err)
		}
		blob, err := store.Open(ctx, c)
		if err != nil {
			t.Fatalf("%s: %v", v.name, err)
		}
		if blob.Size() != uint64(len(v.data)) {
			t.Errorf("%s: size %d, want %d", v.name, blob.Size(), len(v.data))
		}
		got, err := io.ReadAll(blob)
		if err != nil {
			t.Fatalf("%s: %v", v.name, err)
		}
		if !bytes.Equal(got, v.data) {
			t.Errorf("%s: read back %d bytes that differ from the %d stored", v.name, len(got), len(v.data))
		}
		blob.Close()
	}
}

func TestBlobSeeksAcrossChunks(t *testing.T) {
	store := NewMemory()
	data := pattern(1_000_000)
	c, err := store.Put(ctx, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	blob, err := store.Open(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	defer blob.Close()

	const offset = 700_000 // inside the third chunk
	if _, err := blob.Seek(offset, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 1000)
	if _, err := io.ReadFull(blob, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data[offset:offset+1000]) {
		t.Error("bytes read after a seek do not match the stored data")
	}
}

func TestMissingBlob(t *testing.T) {
	store := NewMemory()
	absent, err := CID(ctx, bytes.NewReader([]byte("never stored")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open(ctx, absent); !errors.Is(err, ErrNotFound) {
		t.Errorf("Open of a missing blob: %v, want ErrNotFound", err)
	}
	if has, err := store.Has(ctx, absent); err != nil || has {
		t.Errorf("Has of a missing blob = %v, %v", has, err)
	}
}

func TestLocalStoreSurvivesReopening(t *testing.T) {
	dir := t.TempDir()
	data := pattern(600_000)

	store, err := OpenLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	c, err := store.Put(ctx, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = OpenLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if has, err := store.Has(ctx, c); err != nil || !has {
		t.Fatalf("Has after reopening = %v, %v", has, err)
	}
	blob, err := store.Open(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	defer blob.Close()
	got, err := io.ReadAll(blob)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Error("blob changed across a reopen")
	}
}

func TestPutStopsWhenCancelled(t *testing.T) {
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	store, err := OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Put(cancelled, bytes.NewReader(pattern(600_000))); err == nil {
		t.Error("Put with a cancelled context succeeded")
	}
}

func TestPutReportsAReadError(t *testing.T) {
	broken := io.MultiReader(bytes.NewReader(pattern(300_000)), errReader{})
	if _, err := NewMemory().Put(ctx, broken); err == nil {
		t.Error("Put succeeded although its input failed part-way")
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("input broke") }

func TestHasherMatchesPut(t *testing.T) {
	for _, v := range kuboVectors {
		h := NewHasher(ctx)
		// Write in pieces that do not line up with chunk boundaries.
		for rest := v.data; len(rest) > 0; {
			n := min(len(rest), 100_003)
			if _, err := h.Write(rest[:n]); err != nil {
				t.Fatalf("%s: %v", v.name, err)
			}
			rest = rest[n:]
		}
		got, err := h.Sum()
		if err != nil {
			t.Fatalf("%s: %v", v.name, err)
		}
		if got.String() != v.cid {
			t.Errorf("%s: CID %s, want %s", v.name, got, v.cid)
		}
	}
}

func TestHasherFailsWritesOnceCancelled(t *testing.T) {
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	h := NewHasher(cancelled)
	var err error
	for i := 0; i < 10 && err == nil; i++ {
		_, err = h.Write(pattern(300_000))
	}
	if err == nil {
		t.Error("writes to a cancelled Hasher kept succeeding")
	}
	if _, err := h.Sum(); err == nil {
		t.Error("Sum of a cancelled Hasher succeeded")
	}
}

func TestLocalStoreIsExclusive(t *testing.T) {
	dir := t.TempDir()
	first, err := OpenLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := OpenLocal(dir); err == nil {
		second.Close()
		t.Fatal("opened a store directory that is already open")
	}

	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenLocal(dir)
	if err != nil {
		t.Fatalf("reopening a closed store: %v", err)
	}
	reopened.Close()
}

func openCache(t *testing.T, dir string, syncWrites bool) *Store {
	t.Helper()
	cache, err := OpenCache(dir, syncWrites)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cache.Close() })
	return cache
}

func TestCacheKeepsBlobsAcrossReopening(t *testing.T) {
	for _, syncWrites := range []bool{false, true} {
		dir := t.TempDir()
		data := pattern(600_000)
		cache, err := OpenCache(dir, syncWrites)
		if err != nil {
			t.Fatal(err)
		}
		c, err := cache.Put(ctx, bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		if err := cache.Close(); err != nil {
			t.Fatal(err)
		}

		cache = openCache(t, dir, syncWrites)
		if err := cache.Verify(ctx, c); err != nil {
			t.Fatalf("sync=%v: blob does not verify after reopening: %v", syncWrites, err)
		}
		blob, err := cache.Open(ctx, c)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(blob)
		blob.Close()
		if !bytes.Equal(got, data) {
			t.Errorf("sync=%v: blob read back after reopening differs from what was stored", syncWrites)
		}
	}
}

// blockFiles returns the files holding a disk store's blocks, largest first.
func blockFiles(t *testing.T, dir string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "blocks", "*", "*.data"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no block files under %s (%v)", dir, err)
	}
	sort.Slice(files, func(i, j int) bool {
		a, _ := os.Stat(files[i])
		b, _ := os.Stat(files[j])
		return a.Size() > b.Size()
	})
	return files
}

// A power cut can leave a block file short or missing. Verify must notice,
// and storing the blob again must put it right.
func TestCacheDetectsAndRepairsDamagedBlocks(t *testing.T) {
	damage := map[string]func(file string) error{
		"cut short": func(file string) error { return os.Truncate(file, 100) },
		"emptied":   func(file string) error { return os.Truncate(file, 0) },
		"missing":   os.Remove,
		"altered": func(file string) error {
			data, err := os.ReadFile(file)
			if err != nil {
				return err
			}
			data[len(data)/2] ^= 0xff
			return os.WriteFile(file, data, 0o600)
		},
	}
	for name, spoil := range damage {
		dir := t.TempDir()
		data := pattern(600_000)
		cache := openCache(t, dir, false)
		c, err := cache.Put(ctx, bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		if err := cache.Verify(ctx, c); err != nil {
			t.Fatalf("%s: a freshly stored blob does not verify: %v", name, err)
		}

		if err := spoil(blockFiles(t, dir)[0]); err != nil {
			t.Fatal(err)
		}
		if err := cache.Verify(ctx, c); err == nil {
			t.Fatalf("%s: Verify passed a blob with a damaged block", name)
		}

		if _, err := cache.Put(ctx, bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
		if err := cache.Verify(ctx, c); err != nil {
			t.Errorf("%s: storing the blob again did not repair it: %v", name, err)
		}
	}
}

func TestVerifyChecksEveryBlock(t *testing.T) {
	s := NewMemory()
	data := pattern(50_000_000) // a root, two inner nodes and many leaves
	c, err := s.Put(ctx, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Verify(ctx, c); err != nil {
		t.Fatalf("an intact blob does not verify: %v", err)
	}

	absent, err := CID(ctx, strings.NewReader("never stored"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Verify(ctx, absent); err == nil {
		t.Error("Verify passed a blob the store does not hold")
	}

	// Remove the very last leaf, which only a full walk reaches.
	last, err := CID(ctx, bytes.NewReader(data[len(data)-len(data)%chunkSize:]))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.blocks.DeleteBlock(ctx, last); err != nil {
		t.Fatal(err)
	}
	if err := s.Verify(ctx, c); err == nil {
		t.Error("Verify passed a blob missing its last block")
	}
}

func TestVerifyRejectsATreeNodeThatCannotBeDecoded(t *testing.T) {
	s := NewMemory()
	garbage := []byte("\xff\xff not a node")
	mangled, err := cidBuilder.Sum(garbage)
	if err != nil {
		t.Fatal(err)
	}
	block, err := blocks.NewBlockWithCid(garbage, mangled)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.blocks.Put(ctx, block); err != nil {
		t.Fatal(err)
	}
	// Its bytes match its CID, so only decoding shows it is not a node.
	if err := s.Verify(ctx, mangled); err == nil || strings.Contains(err.Error(), "corrupt") {
		t.Errorf("error %v, want a decoding failure", err)
	}
}

func TestOpeningAStoreFails(t *testing.T) {
	file := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenLocal(filepath.Join(file, "store")); err == nil {
		t.Error("opened a store beneath a regular file")
	}

	// The lock file's name is taken by a directory.
	lockIsDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(lockIsDir, "LOCK"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenLocal(lockIsDir); err == nil {
		t.Error("opened a store whose lock file cannot be created")
	}

	// The block directory's name is taken by a file.
	blocksIsFile := t.TempDir()
	if err := os.WriteFile(filepath.Join(blocksIsFile, "blocks"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenLocal(blocksIsFile); err == nil {
		t.Error("opened a store whose block directory cannot be created")
	}
	// A failed open must release the lock, or the directory stays unusable.
	if err := os.Remove(filepath.Join(blocksIsFile, "blocks")); err != nil {
		t.Fatal(err)
	}
	store, err := OpenLocal(blocksIsFile)
	if err != nil {
		t.Fatalf("opening after a failed open: %v", err)
	}
	store.Close()
}

func TestOpenReportsBlocksThatAreNotABlob(t *testing.T) {
	store := NewMemory()

	// A block stored under a file-tree CID whose bytes are not a tree node.
	garbage := []byte("\xff\xff not a node")
	mangled, err := cidBuilder.Sum(garbage)
	if err != nil {
		t.Fatal(err)
	}
	block, err := blocks.NewBlockWithCid(garbage, mangled)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.blocks.Put(ctx, block); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open(ctx, mangled); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("Open of an undecodable block: %v, want a decoding error", err)
	}

	// A well-formed tree node that does not describe a file.
	notAFile := merkledag.NodeWithData([]byte("no file metadata here"))
	if err := store.dag.Add(ctx, notAFile); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open(ctx, notAFile.Cid()); err == nil || !strings.Contains(err.Error(), "is not a blob") {
		t.Errorf("Open of a node that is not a file: %v", err)
	}
}
