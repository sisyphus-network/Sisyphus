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
// whether the key is new.
func LoadOrCreate(path string) (id *Identity, created bool, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		_, key, _ := ed25519.GenerateKey(rand.Reader) // never fails; see crypto/rand
		der, _ := x509.MarshalPKCS8PrivateKey(key)    // always encodes an Ed25519 key
		encoded := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
		if err := os.WriteFile(path, encoded, 0o600); err != nil {
			return nil, false, fmt.Errorf("save node key: %w", err)
		}
		return fromKey(key), true, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read node key: %w", err)
	}

	block, _ := pem.Decode(data)
	if block == nil {
		return nil, false, fmt.Errorf("node key %s is not PEM", path)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, false, fmt.Errorf("node key %s: %w", path, err)
	}
	key, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, false, fmt.Errorf("node key %s is %T, not an Ed25519 key", path, parsed)
	}
	return fromKey(key), false, nil
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
