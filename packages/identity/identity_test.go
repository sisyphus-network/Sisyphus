package identity

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func create(t *testing.T) (*Identity, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "node.key")
	id, created, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("a key generated for an empty path was reported as already there")
	}
	return id, path
}

func TestANewNodeGetsAKeyOnlyItsOwnerCanRead(t *testing.T) {
	id, path := create(t)
	if !strings.HasPrefix(id.ID(), "12D3KooW") {
		t.Errorf("node ID %q is not an Ed25519 libp2p peer ID", id.ID())
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("node key is stored with mode %o, want 600", mode)
	}
}

func TestANodeKeepsItsIDAcrossRestarts(t *testing.T) {
	first, path := create(t)
	second, created, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Error("an existing key was reported as new")
	}
	if second.ID() != first.ID() {
		t.Errorf("node ID changed from %s to %s on reload", first.ID(), second.ID())
	}
}

func TestEveryNodeGetsItsOwnID(t *testing.T) {
	a, _ := create(t)
	b, _ := create(t)
	if a.ID() == b.ID() {
		t.Errorf("two nodes were both given ID %s", a.ID())
	}
}

func TestSignaturesAreCheckedAgainstTheSignersID(t *testing.T) {
	signer, _ := create(t)
	other, _ := create(t)
	message := []byte("node 12D3KooW... offers 8 cores")
	signature := signer.Sign(message)

	tests := []struct {
		name      string
		id        string
		message   []byte
		signature []byte
		want      bool
	}{
		{"the signer's own message", signer.ID(), message, signature, true},
		{"a changed message", signer.ID(), []byte("node 12D3KooW... offers 9 cores"), signature, false},
		{"another node's ID", other.ID(), message, signature, false},
		{"a changed signature", signer.ID(), message, append([]byte{signature[0] ^ 1}, signature[1:]...), false},
		{"no signature", signer.ID(), message, nil, false},
	}
	for _, tt := range tests {
		got, err := Verify(tt.id, tt.message, tt.signature)
		if err != nil || got != tt.want {
			t.Errorf("%s: Verify = %v, %v; want %v", tt.name, got, err, tt.want)
		}
	}
}

func TestVerifyRejectsIDsItCannotCheckAgainst(t *testing.T) {
	signer, _ := create(t)
	signature := signer.Sign([]byte("message"))
	for name, id := range map[string]string{
		"not an ID at all": "alpha",
		// A peer ID that is only the hash of a key, as RSA keys have.
		"an ID without the key in it": "QmYyQSo1c1Ym7orWxLYvCrM2EmxFTANf8wXmmE7DWjhx5N",
	} {
		if valid, err := Verify(id, []byte("message"), signature); err == nil || valid {
			t.Errorf("%s: Verify = %v, %v; want an error", name, valid, err)
		}
	}
}

func TestLoadingAKeyFails(t *testing.T) {
	ecdsaKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecdsaDER, err := x509.MarshalPKCS8PrivateKey(ecdsaKey)
	if err != nil {
		t.Fatal(err)
	}
	contents := map[string][]byte{
		"not PEM":               []byte("not a key"),
		"PEM that is no key":    pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("junk")}),
		"a key of another kind": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ecdsaDER}),
	}
	for name, content := range contents {
		path := filepath.Join(t.TempDir(), "node.key")
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := LoadOrCreate(path); err == nil {
			t.Errorf("%s: loaded", name)
		}
		// A bad key file is never replaced with a new identity.
		if after, _ := os.ReadFile(path); string(after) != string(content) {
			t.Errorf("%s: the file was overwritten", name)
		}
	}

	dir := t.TempDir()
	if _, _, err := LoadOrCreate(dir); err == nil {
		t.Error("loaded a key from a directory")
	}
	if _, _, err := LoadOrCreate(filepath.Join(dir, "missing", "node.key")); err == nil {
		t.Error("created a key in a directory that does not exist")
	}
}
