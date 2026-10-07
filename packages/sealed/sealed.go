// Package sealed encrypts blobs so that only holders of a key can read them.
//
// A pool's storage is shared: every member can fetch any blob it can name.
// Sealing a job's data with a key given only to the nodes that work on the
// job keeps it from the rest.
//
// A sealed blob is a short header followed by the plaintext in 64 KiB
// chunks, each encrypted and authenticated on its own with AES-256-GCM, so
// that any part can be read without decrypting what comes before it. Each
// chunk is bound to its position and to whether it is the last, so chunks
// cannot be reordered, dropped from the end, or moved between positions
// without detection.
//
// Encryption is deterministic: the same key and plaintext always give the
// same sealed blob, and so the same CID. That is what lets two runs of a job
// be compared by CID, and lets storage hold one copy of a blob sealed twice.
// The price is that someone who can see two sealed blobs can tell whether
// they, or chunks at the same position in them, are identical under the
// same key. They learn nothing else about the contents. Sizes are not
// hidden.
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
	// magic begins every sealed blob.
	magic = "SISYENC1"
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

// Encrypt returns a reader of the sealed form of everything read from r.
func Encrypt(key Key, r io.Reader) io.Reader {
	return &encrypter{sealer: key.sealer(), src: r, pending: *bytes.NewBufferString(magic)}
}

type encrypter struct {
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
	return bytes.HasPrefix(start, []byte(magic))
}

// Reader reads the plaintext of a sealed blob, decrypting chunks as they
// are needed.
type Reader struct {
	sealer *sealer
	src    io.ReadSeeker
	size   uint64 // of the plaintext
	chunks uint64
	pos    uint64

	// loaded is the index of the decrypted chunk held in plain, if any.
	loaded   uint64
	hasChunk bool
	plain    []byte
}

// Open returns a reader for the sealed blob in src, which is sealedSize
// bytes long. It fails with ErrNotSealed if src is not a sealed blob. A
// wrong key or an altered blob is found when the affected part is read.
func Open(key Key, src io.ReadSeeker, sealedSize uint64) (*Reader, error) {
	header := make([]byte, len(magic))
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	n, err := io.ReadFull(src, header)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, err
	}
	if !IsSealed(header[:n]) {
		return nil, ErrNotSealed
	}
	body := sealedSize - uint64(len(magic))
	full, rest := body/recordSize, body%recordSize
	r := &Reader{sealer: key.sealer(), src: src}
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
	if _, err := r.src.Seek(int64(uint64(len(magic))+index*recordSize), io.SeekStart); err != nil {
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
