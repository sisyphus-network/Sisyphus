package names

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ipfs/boxo/ipns"
	"github.com/ipfs/boxo/path"
	"github.com/ipfs/go-cid"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/multiformats/go-multihash"

	"github.com/sisyphus-network/Sisyphus/packages/identity"
)

func newNode(t *testing.T) *identity.Identity {
	t.Helper()
	ident, _, err := identity.LoadOrCreate(filepath.Join(t.TempDir(), "node.key"))
	if err != nil {
		t.Fatal(err)
	}
	return ident
}

// contentID returns the ID some text would have as a stored file.
func contentID(text string) cid.Cid {
	hash, _ := multihash.Sum([]byte(text), multihash.SHA2_256, -1)
	return cid.NewCidV1(cid.Raw, hash)
}

// signedBy returns a record a node signed for a value of any kind, which
// Make would not produce.
func signedBy(t *testing.T, ident *identity.Identity, value string) []byte {
	t.Helper()
	key, err := crypto.UnmarshalPrivateKey(ident.Libp2pKey())
	if err != nil {
		t.Fatal(err)
	}
	pointed, err := path.NewPath(value)
	if err != nil {
		t.Fatal(err)
	}
	record, err := ipns.NewRecord(key, pointed, 1, time.Now().Add(time.Hour), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := ipns.MarshalRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestARecordIsReadBackAsItWasMadeByAnyoneWhoKnowsTheName(t *testing.T) {
	node, value, now := newNode(t), contentID("the boulder rolls"), time.Now()
	made := Make(node, value, 7, time.Hour, time.Minute, now)
	if made.Name != node.ID() || made.Value != value || made.Sequence != 7 || made.TTL != time.Minute || !made.Expires.Equal(now.Add(time.Hour)) {
		t.Errorf("made %+v", made)
	}

	// The name may be spelled as the node's ID or as IPFS programs print it.
	spelled, err := ipns.NameFromString(node.ID())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(spelled.String(), "k51") {
		t.Fatalf("the name as IPFS programs print it is %s", spelled)
	}
	for _, name := range []string{node.ID(), spelled.String(), "/ipns/" + spelled.String()} {
		read, err := Check(name, made.Bytes(), now)
		if err != nil {
			t.Fatalf("checked as %s: %v", name, err)
		}
		if read.Name != node.ID() || read.Value != value || read.Sequence != 7 || read.TTL != time.Minute || !read.Expires.Equal(made.Expires) || !bytes.Equal(read.Bytes(), made.Bytes()) {
			t.Errorf("read as %s: %+v, made %+v", name, read, made)
		}
		if id, err := ID(name); err != nil || id != node.ID() {
			t.Errorf("the node named by %s: %s, %v", name, id, err)
		}
	}

	// Kubo's own check accepts it too.
	theirs, err := ipns.UnmarshalRecord(made.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if err := ipns.ValidateWithName(theirs, spelled); err != nil {
		t.Errorf("as IPFS checks it: %v", err)
	}
}

func TestARecordThatIsNotTheNamedNodesIsRefused(t *testing.T) {
	node, other, now := newNode(t), newNode(t), time.Now()
	first, second := contentID("one"), contentID("two")
	made := Make(node, first, 1, time.Hour, time.Minute, now)

	// The value is there twice, once as it was first signed and once as it
	// is signed now. Changed in both, the signature no longer fits.
	tampered := bytes.ReplaceAll(made.Bytes(), []byte(first.String()), []byte(second.String()))
	if bytes.Equal(tampered, made.Bytes()) {
		t.Fatal("the record does not carry its value as text")
	}

	for _, tt := range []struct {
		about, name string
		raw         []byte
		want        string
	}{
		{"signed with another node's key", other.ID(), made.Bytes(), "not one signed by node " + other.ID()},
		{"with its value changed", node.ID(), tampered, "not one signed by node " + node.ID()},
		{"that is not a record", node.ID(), []byte("\xff\xff a record"), "not a record for a name"},
		{"under something that is not a name", "rig", made.Bytes(), `"rig" is not a name`},
		{"naming another name", node.ID(), signedBy(t, node, "/ipns/"+other.ID()), "does not name a file by its content ID"},
		{"naming a place inside a file", node.ID(), signedBy(t, node, "/ipfs/"+first.String()+"/inside"), "does not name a file by its content ID"},
	} {
		if _, err := Check(tt.name, tt.raw, now); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("a record %s: %v, want %q", tt.about, err, tt.want)
		}
	}
	if _, err := ID("rig"); err == nil || !strings.Contains(err.Error(), `"rig" is not a name`) {
		t.Errorf("the node named by something that is not a name: %v", err)
	}
}

func TestAnExpiredRecordIsRefusedButCanStillBeRead(t *testing.T) {
	node, now := newNode(t), time.Now()
	made := Make(node, contentID("one"), 3, time.Hour, time.Minute, now)
	if _, err := Check(node.ID(), made.Bytes(), now.Add(59*time.Minute)); err != nil {
		t.Errorf("before it expires: %v", err)
	}
	if _, err := Check(node.ID(), made.Bytes(), now.Add(61*time.Minute)); !errors.Is(err, ErrExpired) {
		t.Errorf("after it expires: %v", err)
	}

	// One that expired before it arrived is as sound as it ever was, which
	// is how the sequence number it used is known.
	old := Make(node, contentID("one"), 3, time.Hour, time.Minute, now.Add(-2*time.Hour))
	if _, err := Check(node.ID(), old.Bytes(), now); !errors.Is(err, ErrExpired) {
		t.Errorf("one that expired an hour ago: %v", err)
	}
	read, err := Parse(node.ID(), old.Bytes())
	if err != nil || read.Sequence != 3 || !read.Expired(now) {
		t.Errorf("read without regard to time: %+v, %v", read, err)
	}
	// But not if anything else is wrong with it.
	if _, err := Parse(newNode(t).ID(), old.Bytes()); err == nil {
		t.Error("an expired record was read under another node's name")
	}
}

func TestANewerRecordReplacesAnOlder(t *testing.T) {
	node, now := newNode(t), time.Now()
	record := func(sequence uint64, lifetime time.Duration) *Record {
		return Make(node, contentID("one"), sequence, lifetime, time.Minute, now)
	}
	held := record(5, time.Hour)
	for _, tt := range []struct {
		about    string
		offered  *Record
		replaces bool
	}{
		{"a higher sequence number", record(6, time.Minute), true},
		{"a lower sequence number, good for longer", record(4, 48*time.Hour), false},
		{"the same sequence number, good for longer", record(5, 2*time.Hour), true},
		{"the same sequence number, good for less long", record(5, time.Minute), false},
		{"the same record again", record(5, time.Hour), false},
	} {
		if got := tt.offered.Replaces(held); got != tt.replaces {
			t.Errorf("a record with %s replaces the one held: %v, want %v", tt.about, got, tt.replaces)
		}
	}
}

func TestARecordMayBeUsedForItsTTLOrWhatIsLeftOfItsLife(t *testing.T) {
	node, now := newNode(t), time.Now()
	made := Make(node, contentID("one"), 1, time.Hour, 10*time.Minute, now)
	for _, tt := range []struct {
		after time.Duration
		want  time.Duration
	}{
		{0, 10 * time.Minute},
		{55 * time.Minute, 5 * time.Minute},
		{2 * time.Hour, 0},
	} {
		if got := made.Fresh(now.Add(tt.after)); got != tt.want {
			t.Errorf("%s into its life, a record may be used for %s, want %s", tt.after, got, tt.want)
		}
	}
}
