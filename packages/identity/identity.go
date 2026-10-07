// Package identity gives each node a long-lived key pair and an ID derived
// from it.
//
// The key is Ed25519 and the ID is the libp2p peer ID of its public half, so
// the same identity can later secure TLS between nodes and name the node on
// a libp2p network. Dash and Evrmore sign with secp256k1, which TLS does not
// accept; a node's chain key will be tied to this one by a signed statement
// rather than being the same key.
package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

// Identity is a node's key pair.
type Identity struct {
	key ed25519.PrivateKey
	id  peer.ID
}

// LoadOrCreate reads the node key stored at path, or, if there is none,
// generates one and stores it there readable only by its owner. It reports
// whether the key is new. If several processes do this at once for the same
// path, they all end up with the same key and one of them reports it as new.
func LoadOrCreate(path string) (id *Identity, created bool, err error) {
	id, err = load(path)
	if !errors.Is(err, os.ErrNotExist) {
		return id, false, err
	}

	_, key, _ := ed25519.GenerateKey(rand.Reader) // never fails; see crypto/rand
	return store(path, key)
}

// Adopt stores at path a node key that was made elsewhere and is given in
// libp2p's encoding, as Libp2pKey returns it, so that the node whose key it
// is carries on as itself. It changes nothing if there is a key at path
// already.
func Adopt(path string, libp2pKey []byte) error {
	private, err := crypto.UnmarshalPrivateKey(libp2pKey)
	if err != nil {
		return fmt.Errorf("the key to adopt is not one libp2p can read: %w", err)
	}
	if private.Type() != crypto.Ed25519 {
		return fmt.Errorf("the key to adopt is %s, and a node's key must be Ed25519", private.Type())
	}
	raw, _ := private.Raw() // an Ed25519 key always has a raw form
	_, _, err = store(path, ed25519.PrivateKey(raw))
	return err
}

// store puts key at path unless a key is there already, and returns the key
// that is there afterwards and whether it is this one.
func store(path string, key ed25519.PrivateKey) (id *Identity, created bool, err error) {
	der, _ := x509.MarshalPKCS8PrivateKey(key) // always encodes an Ed25519 key
	var suffix [8]byte
	rand.Read(suffix[:])
	tmp := path + ".tmp-" + hex.EncodeToString(suffix[:])
	if err := os.WriteFile(tmp, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		return nil, false, fmt.Errorf("save node key: %w", err)
	}
	// Linking puts the finished file in place only if nothing is there, so
	// of several processes creating a key at once exactly one succeeds, and
	// nobody ever reads a half-written key. Whoever won, what is at path
	// afterwards is the node's key.
	if link(tmp, path) != nil {
		// Either another process got there first, or this filesystem has no
		// hard links. In the second case nothing is at path yet, and moving
		// the file there is the best that can be done.
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			os.Rename(tmp, path)
		}
	}
	os.Remove(tmp)
	id, err = load(path)
	if err != nil {
		return nil, false, err
	}
	return id, id.key.Equal(key), nil
}

// link is os.Link; tests replace it to stand in for filesystems without
// hard links.
var link = os.Link

// load reads the node key stored at path.
func load(path string) (*Identity, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read node key: %w", err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("node key %s is not PEM", path)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("node key %s: %w", path, err)
	}
	key, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("node key %s is %T, not an Ed25519 key", path, parsed)
	}
	return fromKey(key), nil
}

func fromKey(key ed25519.PrivateKey) *Identity {
	// Neither step fails for an Ed25519 key.
	public, _ := crypto.UnmarshalEd25519PublicKey(key.Public().(ed25519.PublicKey))
	id, _ := peer.IDFromPublicKey(public)
	return &Identity{key: key, id: id}
}

// ID returns the node's ID: the libp2p peer ID of its public key, which
// starts "12D3KooW".
func (i *Identity) ID() string {
	return i.id.String()
}

// Sign signs data with the node's private key.
func (i *Identity) Sign(data []byte) []byte {
	return ed25519.Sign(i.key, data)
}

// Verify reports whether signature is a signature of data by the node with
// the given ID. The ID alone is enough, because it contains the public key.
func Verify(id string, data, signature []byte) (bool, error) {
	decoded, err := peer.Decode(id)
	if err != nil {
		return false, fmt.Errorf("node ID %q: %w", id, err)
	}
	public, err := decoded.ExtractPublicKey()
	if err != nil {
		return false, fmt.Errorf("node ID %q carries no public key: %w", id, err)
	}
	valid, _ := public.Verify(data, signature) // an Ed25519 check has no error of its own
	return valid, nil
}

// Libp2pKey returns the node's private key in libp2p's encoding, which is
// how Kubo and other libp2p programs store a peer's identity. Handle it as
// carefully as the key file itself.
func (i *Identity) Libp2pKey() []byte {
	// Neither step fails for an Ed25519 key.
	private, _ := crypto.UnmarshalEd25519PrivateKey(i.key)
	encoded, _ := crypto.MarshalPrivateKey(private)
	return encoded
}
