package identity

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
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

// Starting a node and asking for its ID at the same moment must not leave
// the two with different keys.
func TestCreatingAKeyFromSeveralProcessesAtOnceGivesOneKey(t *testing.T) {
	for round := 0; round < 20; round++ {
		path := filepath.Join(t.TempDir(), "node.key")
		const racers = 16
		ids := make([]string, racers)
		created := make([]bool, racers)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range racers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				id, isNew, err := LoadOrCreate(path)
				if err != nil {
					t.Error(err)
					return
				}
				ids[i], created[i] = id.ID(), isNew
			}()
		}
		close(start)
		wg.Wait()

		winners := 0
		for i := range racers {
			if ids[i] != ids[0] {
				t.Fatalf("racers ended up with different keys: %s and %s", ids[0], ids[i])
			}
			if created[i] {
				winners++
			}
		}
		// Racers that arrived after the key was in place just loaded it, so
		// there may be fewer than one "creator" per racer, but never two.
		if winners > 1 {
			t.Fatalf("%d racers each believe they created the key", winners)
		}
		if leftovers, _ := filepath.Glob(path + ".tmp-*"); len(leftovers) != 0 {
			t.Errorf("temporary key files left behind: %v", leftovers)
		}
		stored, _, err := LoadOrCreate(path)
		if err != nil || stored.ID() != ids[0] {
			t.Fatalf("the key on disk is %v (%v), the racers got %s", stored, err, ids[0])
		}
	}
}

func TestKeyIsCreatedOnAFilesystemWithoutHardLinks(t *testing.T) {
	defer func() { link = os.Link }()
	link = func(string, string) error { return errors.New("operation not supported") }

	path := filepath.Join(t.TempDir(), "node.key")
	first, created, err := LoadOrCreate(path)
	if err != nil || !created {
		t.Fatalf("LoadOrCreate = %v, %v, %v; want a new key", first, created, err)
	}
	again, created, err := LoadOrCreate(path)
	if err != nil || created || again.ID() != first.ID() {
		t.Errorf("reloading gave %v, %v, %v; want the same key, not new", again, created, err)
	}
	if leftovers, _ := filepath.Glob(path + ".tmp-*"); len(leftovers) != 0 {
		t.Errorf("temporary key files left behind: %v", leftovers)
	}
}

func TestFailingToPutTheKeyInPlaceIsReported(t *testing.T) {
	defer func() { link = os.Link }()
	// A filesystem that loses the file instead of linking it.
	link = func(tmp, _ string) error {
		os.Remove(tmp)
		return errors.New("input/output error")
	}
	if _, _, err := LoadOrCreate(filepath.Join(t.TempDir(), "node.key")); err == nil {
		t.Error("LoadOrCreate reported success with no key on disk")
	}
}

func TestLibp2pKeyEncodesTheSameIdentity(t *testing.T) {
	id, _ := create(t)
	private, err := crypto.UnmarshalPrivateKey(id.Libp2pKey())
	if err != nil {
		t.Fatal(err)
	}
	derived, err := peer.IDFromPrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	if derived.String() != id.ID() {
		t.Errorf("the libp2p key belongs to %s, the node is %s", derived, id.ID())
	}
}

func TestAdoptingAKeyMadeElsewhere(t *testing.T) {
	elsewhere, _, err := LoadOrCreate(filepath.Join(t.TempDir(), "node.key"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "node.key")
	if err := Adopt(path, elsewhere.Libp2pKey()); err != nil {
		t.Fatal(err)
	}
	adopted, created, err := LoadOrCreate(path)
	if err != nil || created || adopted.ID() != elsewhere.ID() {
		t.Fatalf("the adopted key gives node %v (new: %v, %v), want %s", adopted, created, err, elsewhere.ID())
	}
	// A node that has a key keeps it.
	other, _, _ := LoadOrCreate(filepath.Join(t.TempDir(), "node.key"))
	if err := Adopt(path, other.Libp2pKey()); err != nil {
		t.Fatal(err)
	}
	if still, _, _ := LoadOrCreate(path); still.ID() != elsewhere.ID() {
		t.Errorf("adopting over an existing key changed the node to %s", still.ID())
	}

	// Only a key of the kind a node has can be adopted.
	rsaKey, _, err := crypto.GenerateKeyPair(crypto.RSA, 2048)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := crypto.MarshalPrivateKey(rsaKey)
	fresh := filepath.Join(t.TempDir(), "node.key")
	if err := Adopt(fresh, encoded); err == nil || !strings.Contains(err.Error(), "must be Ed25519") {
		t.Errorf("adopting an RSA key: %v", err)
	}
	if err := Adopt(fresh, []byte("not a key")); err == nil {
		t.Error("adopting what is no key succeeded")
	}
	if _, err := os.Stat(fresh); !os.IsNotExist(err) {
		t.Error("a refused key left a file behind")
	}
}
