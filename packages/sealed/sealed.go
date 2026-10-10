// Package sealed encrypts blobs so that only holders of a key can read them.
//
// A pool's storage is shared: every member can fetch any blob it can name.
// Sealing a job's data with a key given only to the nodes that work on the
// job keeps it from the rest.
//
// Nothing is sealed with a sealing key itself. Each blob is sealed with a
// key derived from it, and says in its header which: one key for each job,
// for what the job stores, and one for each stored file. So whoever is to
// work on a job can be given the keys to that job's data, and with them
// opens nothing else the sealing key seals. The holder of the sealing key
// derives any of them and opens everything.
//
// A sealed blob is a short header followed by the plaintext in 64 KiB
// chunks, each encrypted and authenticated on its own with AES-256-GCM, so
// that any part can be read without decrypting what comes before it. Each
// chunk is bound to its position and to whether it is the last, so chunks
// cannot be reordered, dropped from the end, or moved between positions
// without detection.
//
// Encryption is deterministic: the same derived key and plaintext always
// give the same sealed blob, and so the same CID. That is what lets the
// workers of one job be compared by the CIDs of what they stored, and lets
// storage hold one copy of a file sealed twice. The price is that someone
// who can see two blobs sealed with the same derived key can tell whether
// they, or chunks at the same position in them, are identical. Blobs of
// different jobs have different keys and show nothing of each other. A
// stored file's key is derived from its first chunk, so that a file can be
// sealed as it arrives: two files that begin with the same 64 KiB share a
// key, and the key given for one opens the other. Sizes are not hidden.
//
// This is a construction assembled from standard parts (HKDF, HMAC-SHA256
// as a synthetic nonce, AES-256-GCM), not a reviewed standard. It should be
// examined by someone qualified before anything valuable depends on it.
package sealed

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	// magic begins every sealed blob, and is followed by the ID of the key
	// it was sealed with. old began those sealed with a sealing key itself,
	// before keys were derived, which are still opened.
	magic = "SISYENC2"
	old   = "SISYENC1"
	// chunkSize is how much plaintext each encrypted chunk holds; the last
	// may hold less.
	chunkSize = 64 << 10
	nonceSize = 12
	tagSize   = 16
	// overhead is what sealing adds to each chunk: its nonce and its tag.
	overhead   = nonceSize + tagSize
	recordSize = chunkSize + overhead
)

// ErrNotSealed reports that a blob is not a sealed one.
var ErrNotSealed = errors.New("blob is not sealed")

// ErrCorrupt reports that a sealed blob has been altered or cut short, or
// that the key is not the one it was sealed with. The two cannot be told
// apart.
var ErrCorrupt = errors.New("sealed blob does not open: it was sealed with another key, or has been altered")

// KeySize is the length of a key in bytes.
const KeySize = 32

// Key seals and opens blobs.
type Key [KeySize]byte

// NewKey generates a key.
func NewKey() Key {
	var k Key
	rand.Read(k[:]) // never fails; see crypto/rand
	return k
}

// ParseKey reads a key from the text String gives.
func ParseKey(text string) (Key, error) {
	var k Key
	raw, err := base64.RawURLEncoding.DecodeString(text)
	if err != nil || len(raw) != len(k) {
		return k, errors.New("not a sealing key: expected 43 characters of URL-safe base64")
	}
	copy(k[:], raw)
	return k, nil
}

// String returns the key as text. It is the secret itself: do not log it.
func (k Key) String() string {
	return base64.RawURLEncoding.EncodeToString(k[:])
}

// KeyID says which of the keys derived from a sealing key a blob was sealed
// with. It is no secret: it is written at the head of the blob.
type KeyID [16]byte

// Grant is one of the keys derived from a sealing key, with its ID: what is
// given to whoever is to open or seal the blobs it is for, and nothing more.
type Grant struct {
	ID  KeyID
	Key Key
}

// Grant returns the key derived from k that has the given ID.
func (k Key) Grant(id KeyID) Grant {
	// Cannot fail for these sizes.
	derived, _ := hkdf.Key(sha256.New, k[:], id[:], "sisyphus sealed blob: derived key", KeySize)
	return Grant{ID: id, Key: Key(derived)}
}

// ForJob returns the key that what a job stores is sealed with.
func (k Key) ForJob(jobID string) Grant {
	sum := sha256.Sum256([]byte("sisyphus sealed blob: job " + jobID))
	return k.Grant(KeyID(sum[:len(KeyID{})]))
}

// forFile returns the key a stored file is sealed with, given how the file
// begins: a file is sealed as it arrives, before the rest of it is known.
func (k Key) forFile(first []byte) Grant {
	idKey, _ := hkdf.Key(sha256.New, k[:], nil, "sisyphus sealed blob: file key IDs", 32)
	mac := hmac.New(sha256.New, idKey)
	mac.Write(first)
	return k.Grant(KeyID(mac.Sum(nil)[:len(KeyID{})]))
}

// Keys is what opens sealed blobs: a sealing key, which opens whatever was
// sealed with any key derived from it, or a Ring of such keys, which opens
// what was sealed with those.
type Keys interface {
	// keyTo returns the key to a blob sealed with the derived key that has
	// the given ID, or, with whole set, with a sealing key itself.
	keyTo(id KeyID, whole bool) (Key, error)
}

func (k Key) keyTo(id KeyID, whole bool) (Key, error) {
	if whole {
		return k, nil
	}
	return k.Grant(id).Key, nil
}

// ErrNoKey reports that a blob was sealed with a key that was not given.
var ErrNoKey = errors.New("sealed blob does not open: the key it was sealed with is not among those given")

// Ring is the keys given to whoever works on a job: those of the blobs the
// job is to read and the one it seals what it stores with.
type Ring struct {
	grants map[KeyID]Key
	// Whole, if set, is a sealing key itself, for a blob sealed before
	// keys were derived, which nothing less opens.
	Whole *Key
}

// NewRing returns a ring of the given keys.
func NewRing(grants ...Grant) *Ring {
	r := &Ring{grants: make(map[KeyID]Key, len(grants))}
	for _, g := range grants {
		r.grants[g.ID] = g.Key
	}
	return r
}

func (r *Ring) keyTo(id KeyID, whole bool) (Key, error) {
	if whole {
		if r.Whole == nil {
			return Key{}, ErrNoKey
		}
		return *r.Whole, nil
	}
	key, held := r.grants[id]
	if !held {
		return Key{}, ErrNoKey
	}
	return key, nil
}

// sealer does the encryption for one key.
type sealer struct {
	aead   cipher.AEAD
	macKey []byte
}

func (k Key) sealer() *sealer {
	// One key for encrypting and one for choosing nonces, both derived from
	// the sealing key. None of these steps can fail for these sizes.
	encKey, _ := hkdf.Key(sha256.New, k[:], nil, "sisyphus sealed blob: encryption", 32)
	macKey, _ := hkdf.Key(sha256.New, k[:], nil, "sisyphus sealed blob: nonces", 32)
	block, _ := aes.NewCipher(encKey)
	aead, _ := cipher.NewGCM(block)
	return &sealer{aead: aead, macKey: macKey}
}

// position is what a chunk is bound to: where it is and whether it is last.
func position(index uint64, final bool) []byte {
	var p [9]byte
	binary.BigEndian.PutUint64(p[:8], index)
	if final {
		p[8] = 1
	}
	return p[:]
}

// seal encrypts one chunk, returning its nonce, ciphertext and tag. The
// nonce is computed from the key, the position and the plaintext, which is
// what makes sealing deterministic without ever using a nonce for two
// different messages.
func (s *sealer) seal(index uint64, final bool, plain []byte) []byte {
	pos := position(index, final)
	mac := hmac.New(sha256.New, s.macKey)
	mac.Write(pos)
	mac.Write(plain)
	nonce := mac.Sum(nil)[:nonceSize]
	return s.aead.Seal(nonce, nonce, plain, pos)
}

func (s *sealer) open(index uint64, final bool, record []byte) ([]byte, error) {
	plain, err := s.aead.Open(nil, record[:nonceSize], record[nonceSize:], position(index, final))
	if err != nil {
		return nil, ErrCorrupt
	}
	return plain, nil
}

// Encrypt returns a reader of everything read from r, sealed as a stored
// file is: with a key derived from key and from how the file begins.
func Encrypt(key Key, r io.Reader) io.Reader {
	return &encrypter{from: &key, src: r}
}

// EncryptWith returns a reader of everything read from r, sealed with the
// given key: as what a job stores is sealed with the job's.
func EncryptWith(grant Grant, r io.Reader) io.Reader {
	e := &encrypter{src: r}
	e.begin(grant)
	return e
}

type encrypter struct {
	// from is the sealing key a file's own key is still to be derived
	// from, once its first chunk has been read.
	from   *Key
	sealer *sealer
	src    io.Reader
	// current is the chunk read but not yet sealed. It is held back until it
	// is known whether more follows, since the last chunk is sealed as such.
	current []byte
	started bool
	index   uint64
	done    bool
	pending bytes.Buffer
}

func (e *encrypter) Read(p []byte) (int, error) {
	for e.pending.Len() == 0 {
		if e.done {
			return 0, io.EOF
		}
		if err := e.sealNext(); err != nil {
			return 0, err
		}
	}
	return e.pending.Read(p)
}

// sealNext seals one more chunk into pending.
func (e *encrypter) sealNext() error {
	if !e.started {
		e.started = true
		var err error
		if e.current, err = readChunk(e.src); err != nil {
			return err
		}
		if e.from != nil {
			e.begin(e.from.forFile(e.current))
		}
	}
	// A short chunk is the last. A full one is the last only if nothing
	// follows it.
	var next []byte
	if len(e.current) == chunkSize {
		var err error
		if next, err = readChunk(e.src); err != nil {
			return err
		}
	}
	final := len(next) == 0
	e.pending.Write(e.sealer.seal(e.index, final, e.current))
	e.current, e.done = next, final
	e.index++
	return nil
}

// begin writes the head of a blob sealed with the given key.
func (e *encrypter) begin(grant Grant) {
	e.sealer = grant.Key.sealer()
	e.pending.WriteString(magic)
	e.pending.Write(grant.ID[:])
}

// readChunk reads up to a chunk from r. Fewer bytes than a chunk means r is
// exhausted.
func readChunk(r io.Reader) ([]byte, error) {
	chunk := make([]byte, chunkSize)
	n, err := io.ReadFull(r, chunk)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, err
	}
	return chunk[:n], nil
}

// IsSealed reports whether a blob that begins with the given bytes is a
// sealed one. It needs the first eight.
func IsSealed(start []byte) bool {
	return bytes.HasPrefix(start, []byte(magic)) || bytes.HasPrefix(start, []byte(old))
}

// HeaderSize is how much of a blob Header needs to be given.
const HeaderSize = len(magic) + len(KeyID{})

// Header reads the head of a blob: whether it is sealed, and with which
// derived key, or, if whole is set, that it was sealed with a sealing key
// itself. It needs the first HeaderSize bytes, or all there are.
func Header(start []byte) (id KeyID, whole, isSealed bool) {
	switch {
	case bytes.HasPrefix(start, []byte(old)):
		return id, true, true
	case bytes.HasPrefix(start, []byte(magic)) && len(start) >= HeaderSize:
		return KeyID(start[len(magic):HeaderSize]), false, true
	}
	return id, false, false
}

// Reader reads the plaintext of a sealed blob, decrypting chunks as they
// are needed.
type Reader struct {
	sealer *sealer
	src    io.ReadSeeker
	head   uint64 // the length of the blob's header
	size   uint64 // of the plaintext
	chunks uint64
	pos    uint64

	// loaded is the index of the decrypted chunk held in plain, if any.
	loaded   uint64
	hasChunk bool
	plain    []byte
}

// Open returns a reader for the sealed blob in src, which is sealedSize
// bytes long. It fails with ErrNotSealed if src is not a sealed blob, and
// with ErrNoKey if keys has no key to it. A wrong key or an altered blob is
// found when the affected part is read.
func Open(keys Keys, src io.ReadSeeker, sealedSize uint64) (*Reader, error) {
	header := make([]byte, HeaderSize)
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	n, err := io.ReadFull(src, header)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, err
	}
	id, whole, is := Header(header[:n])
	if !is {
		if IsSealed(header[:n]) {
			// It says it is sealed and stops before saying with what.
			return nil, fmt.Errorf("%w: its length is not that of any sealed blob", ErrCorrupt)
		}
		return nil, ErrNotSealed
	}
	key, err := keys.keyTo(id, whole)
	if err != nil {
		return nil, err
	}
	head := uint64(HeaderSize)
	if whole {
		head = uint64(len(old))
	}
	if sealedSize < head {
		return nil, fmt.Errorf("%w: its length is not that of any sealed blob", ErrCorrupt)
	}
	body := sealedSize - head
	full, rest := body/recordSize, body%recordSize
	r := &Reader{sealer: key.sealer(), src: src, head: head}
	switch {
	case rest == 0 && full > 0:
		// Every chunk is full, the last included.
		r.size, r.chunks = full*chunkSize, full
	case rest >= overhead:
		r.size, r.chunks = full*chunkSize+rest-overhead, full+1
	default:
		// Too short to hold even an empty last chunk.
		return nil, fmt.Errorf("%w: its length is not that of any sealed blob", ErrCorrupt)
	}
	return r, nil
}

// Size returns the length of the plaintext.
func (r *Reader) Size() uint64 { return r.size }

func (r *Reader) Read(p []byte) (int, error) {
	if r.pos >= r.size {
		// An empty blob still has one chunk, which proves it was sealed
		// empty rather than cut down to nothing.
		if r.size == 0 && !r.hasChunk {
			if err := r.load(0); err != nil {
				return 0, err
			}
		}
		return 0, io.EOF
	}
	index := r.pos / chunkSize
	if !r.hasChunk || r.loaded != index {
		if err := r.load(index); err != nil {
			return 0, err
		}
	}
	n := copy(p, r.plain[r.pos%chunkSize:])
	r.pos += uint64(n)
	return n, nil
}

// load reads and decrypts the chunk at index.
func (r *Reader) load(index uint64) error {
	final := index == r.chunks-1
	length := uint64(recordSize)
	if final {
		length = r.size - index*chunkSize + overhead
	}
	if _, err := r.src.Seek(int64(r.head+index*recordSize), io.SeekStart); err != nil {
		return err
	}
	record := make([]byte, length)
	if _, err := io.ReadFull(r.src, record); err != nil {
		return fmt.Errorf("read sealed blob: %w", err)
	}
	plain, err := r.sealer.open(index, final, record)
	if err != nil {
		return err
	}
	r.plain, r.loaded, r.hasChunk = plain, index, true
	return nil
}

func (r *Reader) Seek(offset int64, whence int) (int64, error) {
	var base int64
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		base = int64(r.pos)
	case io.SeekEnd:
		base = int64(r.size)
	default:
		return 0, fmt.Errorf("seek: invalid whence %d", whence)
	}
	if base+offset < 0 {
		return 0, errors.New("seek: position before the start")
	}
	r.pos = uint64(base + offset)
	return int64(r.pos), nil
}

// Close closes the underlying blob if it can be closed.
func (r *Reader) Close() error {
	if closer, ok := r.src.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}
