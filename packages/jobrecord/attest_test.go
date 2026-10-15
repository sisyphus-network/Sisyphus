package jobrecord

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ipfs/go-cid"
	"github.com/ipld/go-ipld-prime/fluent"
	cidlink "github.com/ipld/go-ipld-prime/linking/cid"
	"github.com/ipld/go-ipld-prime/node/basicnode"

	"github.com/sisyphus-network/Sisyphus/packages/identity"
)

func newIdentity(t *testing.T) *identity.Identity {
	t.Helper()
	ident, _, err := identity.LoadOrCreate(filepath.Join(t.TempDir(), "node.key"))
	if err != nil {
		t.Fatal(err)
	}
	return ident
}

func someRoot(t *testing.T) cid.Cid {
	t.Helper()
	c, err := nodeBuilder.Sum([]byte("a record's root"))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A coordinator's attestation of a record checks out against nothing but
// its node ID, and is the same however often it is made.
func TestAnAttestationIsCheckedAgainstItsSignersID(t *testing.T) {
	ident, root := newIdentity(t), someRoot(t)
	node := Attest(root, ident.ID(), ident.Sign)
	if again := Attest(root, ident.ID(), ident.Sign); !again.CID.Equals(node.CID) || !bytes.Equal(again.Data, node.Data) {
		t.Error("attesting the same record twice gave two attestations")
	}
	if node.CID.Type() != cid.DagCBOR {
		t.Errorf("an attestation's CID is of codec %x", node.CID.Type())
	}
	got, err := CheckAttestation(node.Data)
	if err != nil || !got.Record.Equals(root) || got.Signer != ident.ID() || len(got.Signature) != 64 {
		t.Fatalf("CheckAttestation = %+v, %v", got, err)
	}
	if !bytes.HasPrefix(Statement(root), []byte("sisyphus-job-record-signature/v1\x00")) || !bytes.HasSuffix(Statement(root), root.Bytes()) {
		t.Errorf("the statement signed: %q", Statement(root))
	}
}

func TestAnAttestationThatDoesNotHoldIsRefused(t *testing.T) {
	ident, other, root := newIdentity(t), newIdentity(t), someRoot(t)
	build := func(edit func(m fluent.MapAssembler)) []byte {
		return encode(fluent.MustBuildMap(basicnode.Prototype.Map, -1, edit))
	}
	signature := ident.Sign(Statement(root))
	whole := func(kind string, version int64, signer string, sig []byte) func(m fluent.MapAssembler) {
		return func(m fluent.MapAssembler) {
			m.AssembleEntry("kind").AssignString(kind)
			m.AssembleEntry("version").AssignInt(version)
			m.AssembleEntry("record").AssignLink(cidlink.Link{Cid: root})
			m.AssembleEntry("signer").AssignString(signer)
			m.AssembleEntry("signature").AssignBytes(sig)
		}
	}
	// A bare signature on the CID, without the statement's prefix, is a
	// signature on something else.
	bare := ident.Sign(root.Bytes())
	for _, tt := range []struct {
		name string
		data []byte
		want string
	}{
		{"not DAG-CBOR", []byte("plain text"), "not DAG-CBOR"},
		{"not a map", encode(basicnode.NewString("hello")), "not a version 1"},
		{"another kind", build(whole("something-else", 1, ident.ID(), signature)), "not a version 1"},
		{"a later version", build(whole(AttestationKind, 2, ident.ID(), signature)), "not a version 1"},
		{"no signature", build(func(m fluent.MapAssembler) {
			m.AssembleEntry("kind").AssignString(AttestationKind)
			m.AssembleEntry("version").AssignInt(1)
			m.AssembleEntry("record").AssignLink(cidlink.Link{Cid: root})
			m.AssembleEntry("signer").AssignString(ident.ID())
		}), "not a version 1"},
		{"another node's name on it", build(whole(AttestationKind, 1, other.ID(), signature)), "is not " + other.ID() + "'s"},
		{"a signature on the bare CID", build(whole(AttestationKind, 1, ident.ID(), bare)), "is not"},
		{"a signer that is no node ID", build(whole(AttestationKind, 1, "nobody", signature)), "node ID"},
	} {
		if _, err := CheckAttestation(tt.data); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v, want %q", tt.name, err, tt.want)
		}
	}
}
