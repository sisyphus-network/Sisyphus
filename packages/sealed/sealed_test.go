package sealed

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// pattern returns n deterministic, non-repeating bytes.
func pattern(n int) []byte {
	b := make([]byte, n)
	state := uint64(1)
	for i := range b {
		state = state*6364136223846793005 + 1442695040888963407
		b[i] = byte(state >> 56)
	}
	return b
}

func seal(t *testing.T, key Key, plain []byte) []byte {
	t.Helper()
	sealed, err := io.ReadAll(Encrypt(key, bytes.NewReader(plain)))
	if err != nil {
		t.Fatal(err)
	}
	return sealed
}

func open(key Key, sealed []byte) ([]byte, error) {
	r, err := Open(key, bytes.NewReader(sealed), uint64(len(sealed)))
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

// sizes covers every shape: nothing, one byte, either side of a chunk
// boundary, exactly two chunks, and several chunks with a part left over.
var sizes = []int{0, 1, chunkSize - 1, chunkSize, chunkSize + 1, 2 * chunkSize, 3*chunkSize + 5}

func TestSealedBlobsOpenToWhatWasSealed(t *testing.T) {
	key := NewKey()
	for _, size := range sizes {
		plain := pattern(size)
		sealed := seal(t, key, plain)
		if !IsSealed(sealed) {
			t.Errorf("%d bytes: the sealed form is not recognised as sealed", size)
		}
		if size > 64 && bytes.Contains(sealed, plain[:64]) {
			t.Errorf("%d bytes: the sealed form contains the plaintext", size)
		}
		r, err := Open(key, bytes.NewReader(sealed), uint64(len(sealed)))
		if err != nil {
			t.Fatalf("%d bytes: %v", size, err)
		}
		if r.Size() != uint64(size) {
			t.Errorf("%d bytes: Size reports %d", size, r.Size())
		}
		got, err := io.ReadAll(r)
		if err != nil || !bytes.Equal(got, plain) {
			t.Errorf("%d bytes: opened to %d bytes, %v", size, len(got), err)
		}
	}
}

// The same key and plaintext must give the same sealed blob every time, or a
// job's result would get a different CID on every run.
func TestSealingIsDeterministicPerKey(t *testing.T) {
	key, other := NewKey(), NewKey()
	plain := pattern(3*chunkSize + 5)
	first := seal(t, key, plain)
	if !bytes.Equal(first, seal(t, key, plain)) {
		t.Error("sealing the same bytes with the same key twice gave different results")
	}
	if bytes.Equal(first, seal(t, other, plain)) {
		t.Error("two keys sealed the same bytes identically")
	}
	// However the source hands the bytes over.
	trickle := io.MultiReader(bytes.NewReader(plain[:7]), iotestOneByte{bytes.NewReader(plain[7:1000])}, bytes.NewReader(plain[1000:]))
	trickled, err := io.ReadAll(Encrypt(key, trickle))
	if err != nil || !bytes.Equal(trickled, first) {
		t.Errorf("sealing from a source that delivers in dribbles gave a different result (%v)", err)
	}
}

// iotestOneByte delivers a reader's bytes one at a time.
type iotestOneByte struct{ r io.Reader }

func (o iotestOneByte) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return o.r.Read(p[:1])
}

func TestTheWrongKeyOpensNothing(t *testing.T) {
	for _, size := range sizes {
		sealed := seal(t, NewKey(), pattern(size))
		if got, err := open(NewKey(), sealed); !errors.Is(err, ErrCorrupt) || len(got) != 0 {
			t.Errorf("%d bytes: the wrong key gave %d bytes and %v, want ErrCorrupt", size, len(got), err)
		}
	}
}

func TestAnyAlterationIsDetected(t *testing.T) {
	key := NewKey()
	plain := pattern(3*chunkSize + 5)
	sealed := seal(t, key, plain)
	record := func(i int) []byte { return sealed[len(magic)+i*recordSize : len(magic)+(i+1)*recordSize] }

	alter := map[string]func([]byte) []byte{
		"a flipped bit in the first chunk": func(b []byte) []byte { b[len(magic)+nonceSize+10] ^= 1; return b },
		"a flipped bit in a nonce":         func(b []byte) []byte { b[len(magic)+recordSize+3] ^= 1; return b },
		"a flipped bit in the last byte":   func(b []byte) []byte { b[len(b)-1] ^= 1; return b },
		"two chunks swapped": func(b []byte) []byte {
			first, second := bytes.Clone(record(0)), bytes.Clone(record(1))
			copy(b[len(magic):], second)
			copy(b[len(magic)+recordSize:], first)
			return b
		},
		"the last chunk dropped":       func(b []byte) []byte { return b[:len(magic)+3*recordSize] },
		"cut short inside a chunk":     func(b []byte) []byte { return b[:len(magic)+2*recordSize+100] },
		"cut down to its first chunk":  func(b []byte) []byte { return b[:len(magic)+recordSize] },
		"a chunk repeated at the end":  func(b []byte) []byte { return append(b, record(0)...) },
		"a few bytes added at the end": func(b []byte) []byte { return append(b, 1, 2, 3) },
	}
	for name, change := range alter {
		altered := change(bytes.Clone(sealed))
		got, err := open(key, altered)
		if err == nil {
			t.Errorf("%s: opened without complaint to %d bytes", name, len(got))
		}
		// Whatever was returned before the damage was reached is genuine.
		if !bytes.HasPrefix(plain, got) {
			t.Errorf("%s: returned bytes that are not from the original", name)
		}
	}

	// An empty blob cut down to just its header is not an empty blob.
	empty := seal(t, key, nil)
	if _, err := open(key, empty[:len(magic)]); err == nil {
		t.Error("a sealed empty blob with its only chunk removed opened")
	}
}

func TestThingsThatAreNotSealedBlobs(t *testing.T) {
	key := NewKey()
	for name, data := range map[string][]byte{
		"nothing":                 nil,
		"plain text":              []byte("just some ordinary text, longer than the header"),
		"shorter than the header": []byte("SISY"),
	} {
		if _, err := Open(key, bytes.NewReader(data), uint64(len(data))); !errors.Is(err, ErrNotSealed) {
			t.Errorf("%s: %v, want ErrNotSealed", name, err)
		}
		if IsSealed(data) {
			t.Errorf("%s is recognised as sealed", name)
		}
	}
}

func TestSeekingReadsAnyPartWithoutTheRest(t *testing.T) {
	key := NewKey()
	plain := pattern(3*chunkSize + 5)
	sealed := seal(t, key, plain)
	r, err := Open(key, bytes.NewReader(sealed), uint64(len(sealed)))
	if err != nil {
		t.Fatal(err)
	}

	read := func(n int) []byte {
		got := make([]byte, n)
		if _, err := io.ReadFull(r, got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	// Across a chunk boundary, from the start.
	if _, err := r.Seek(chunkSize-10, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if got := read(20); !bytes.Equal(got, plain[chunkSize-10:chunkSize+10]) {
		t.Error("a read across a chunk boundary returned the wrong bytes")
	}
	// Relative to where the last read ended.
	if pos, err := r.Seek(100, io.SeekCurrent); err != nil || pos != chunkSize+110 {
		t.Fatalf("Seek from current = %d, %v", pos, err)
	}
	if got := read(5); !bytes.Equal(got, plain[chunkSize+110:chunkSize+115]) {
		t.Error("a read after a relative seek returned the wrong bytes")
	}
	// From the end, into the short last chunk.
	if _, err := r.Seek(-3, io.SeekEnd); err != nil {
		t.Fatal(err)
	}
	if got, _ := io.ReadAll(r); !bytes.Equal(got, plain[len(plain)-3:]) {
		t.Errorf("the last three bytes read as %v", got)
	}
	// Past the end there is nothing.
	if _, err := r.Seek(10, io.SeekEnd); err != nil {
		t.Fatal(err)
	}
	if n, err := r.Read(make([]byte, 4)); n != 0 || err != io.EOF {
		t.Errorf("a read past the end = %d, %v", n, err)
	}

	if _, err := r.Seek(-1, io.SeekStart); err == nil {
		t.Error("sought to before the start")
	}
	if _, err := r.Seek(0, 7); err == nil {
		t.Error("sought with a meaningless whence")
	}
}

func TestKeysAsText(t *testing.T) {
	key := NewKey()
	text := key.String()
	if len(text) != 43 {
		t.Errorf("a key is %d characters as text, want 43", len(text))
	}
	back, err := ParseKey(text)
	if err != nil || back != key {
		t.Errorf("a key did not survive being written and read: %v", err)
	}
	if NewKey() == key {
		t.Error("two generated keys are the same")
	}
	for _, bad := range []string{"", "too-short", text + "A", strings.Replace(text, text[:1], "!", 1), text[:42] + "="} {
		if _, err := ParseKey(bad); err == nil {
			t.Errorf("ParseKey accepted %q", bad)
		}
	}
}

// failing is a source that works up to a point.
type failing struct {
	data     []byte
	pos      int64
	failRead bool
	failSeek bool
	closed   bool
}

var errSource = errors.New("source failed")

func (f *failing) Read(p []byte) (int, error) {
	if f.failRead {
		return 0, errSource
	}
	if f.pos >= int64(len(f.data)) {
		return 0, io.EOF
	}
	n := copy(p, f.data[f.pos:])
	f.pos += int64(n)
	return n, nil
}

func (f *failing) Seek(offset int64, _ int) (int64, error) {
	if f.failSeek {
		return 0, errSource
	}
	f.pos = offset
	return offset, nil
}

func (f *failing) Close() error {
	f.closed = true
	return nil
}

func TestFailuresOfTheSourceArePassedOn(t *testing.T) {
	key := NewKey()
	// Sealing from a source that fails at once, and one that fails after a
	// full chunk, when the sealer looks to see whether more follows.
	if _, err := io.ReadAll(Encrypt(key, &failing{failRead: true})); !errors.Is(err, errSource) {
		t.Errorf("sealing from a failing source: %v", err)
	}
	later := io.MultiReader(bytes.NewReader(pattern(chunkSize)), &failing{failRead: true})
	if _, err := io.ReadAll(Encrypt(key, later)); !errors.Is(err, errSource) {
		t.Errorf("sealing from a source that fails after a chunk: %v", err)
	}

	sealed := seal(t, key, pattern(2*chunkSize))
	if _, err := Open(key, &failing{data: sealed, failSeek: true}, uint64(len(sealed))); !errors.Is(err, errSource) {
		t.Errorf("opening a blob that cannot be rewound: %v", err)
	}
	// A blob whose first bytes cannot be read is not thereby an unsealed one.
	if _, err := Open(key, &failing{data: sealed, failRead: true}, uint64(len(sealed))); !errors.Is(err, errSource) {
		t.Errorf("opening a blob that cannot be read: %v", err)
	}
	src := &failing{data: sealed}
	r, err := Open(key, src, uint64(len(sealed)))
	if err != nil {
		t.Fatal(err)
	}
	src.failSeek = true
	if _, err := r.Read(make([]byte, 10)); !errors.Is(err, errSource) {
		t.Errorf("reading when the blob cannot be positioned: %v", err)
	}
	src.failSeek, src.failRead = false, true
	if _, err := r.Read(make([]byte, 10)); !errors.Is(err, errSource) {
		t.Errorf("reading when the blob cannot be read: %v", err)
	}

	// Closing closes what is underneath, if it can be closed.
	if err := r.Close(); err != nil || !src.closed {
		t.Errorf("Close = %v, underlying closed = %v", err, src.closed)
	}
	plainSource, _ := Open(key, bytes.NewReader(sealed), uint64(len(sealed)))
	if err := plainSource.Close(); err != nil {
		t.Errorf("closing a reader over something that cannot be closed: %v", err)
	}
}
