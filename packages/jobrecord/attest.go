package jobrecord

import (
	"errors"
	"fmt"

	"github.com/ipfs/go-cid"
	"github.com/ipld/go-ipld-prime/datamodel"
	"github.com/ipld/go-ipld-prime/fluent"
	cidlink "github.com/ipld/go-ipld-prime/linking/cid"
	"github.com/ipld/go-ipld-prime/node/basicnode"

	"github.com/sisyphus-network/Sisyphus/packages/identity"
)

// An attestation is a coordinator's signature on a job's record, kept in a
// node of its own beside the record, so that the record and its CID are
// the same with or without one. Whoever holds the attestation needs nothing
// but the signer's node ID to check it: the ID carries the public key.

// AttestationKind names an attestation node, and AttestationVersion its
// layout.
const (
	AttestationKind    = "sisyphus-job-record-attestation"
	AttestationVersion = 1
)

// statementPrefix begins what is signed, so that a signature on a record
// cannot be taken for a signature on anything else a node signs, such as a
// name. It names its version, so that a later layout signs something else.
const statementPrefix = "sisyphus-job-record-signature/v1\x00"

// Statement is what a coordinator signs to attest a record: the prefix,
// then the record root's CID in its binary form.
func Statement(root cid.Cid) []byte {
	return append([]byte(statementPrefix), root.Bytes()...)
}

// Attestation is a signed statement that a node vouches for a record.
type Attestation struct {
	Record    cid.Cid
	Signer    string
	Signature []byte
}

// Attest returns the attestation node, by the node with ID signer, of the
// record whose root is root. An Ed25519 signature depends on nothing but
// the key and what is signed, so a record has one attestation per signer.
func Attest(root cid.Cid, signer string, sign func([]byte) []byte) Node {
	data := encode(fluent.MustBuildMap(basicnode.Prototype.Map, -1, func(m fluent.MapAssembler) {
		m.AssembleEntry("kind").AssignString(AttestationKind)
		m.AssembleEntry("version").AssignInt(AttestationVersion)
		m.AssembleEntry("record").AssignLink(cidlink.Link{Cid: root})
		m.AssembleEntry("signer").AssignString(signer)
		m.AssembleEntry("signature").AssignBytes(sign(Statement(root)))
	}))
	c, _ := nodeBuilder.Sum(data) // hashing bytes in memory cannot fail
	return Node{CID: c, Data: data}
}

// CheckAttestation reads an attestation node and checks its signature
// against its signer's node ID. It returns the attestation only if the
// signature holds.
func CheckAttestation(data []byte) (Attestation, error) {
	node, err := decode(data)
	if err != nil {
		return Attestation{}, fmt.Errorf("an attestation that is not DAG-CBOR: %w", err)
	}
	var a Attestation
	kind, kindErr := field(node, "kind").AsString()
	version, versionErr := field(node, "version").AsInt()
	link, linkErr := field(node, "record").AsLink()
	a.Signer, err = field(node, "signer").AsString()
	signature, signatureErr := field(node, "signature").AsBytes()
	if err := errors.Join(kindErr, versionErr, linkErr, err, signatureErr); err != nil || kind != AttestationKind || version != AttestationVersion {
		return Attestation{}, fmt.Errorf("not a version %d %s", AttestationVersion, AttestationKind)
	}
	a.Record, a.Signature = link.(cidlink.Link).Cid, signature
	valid, err := identity.Verify(a.Signer, Statement(a.Record), a.Signature)
	if err != nil {
		return Attestation{}, err
	}
	if !valid {
		return Attestation{}, fmt.Errorf("the signature is not %s's on record %s", a.Signer, a.Record)
	}
	return a, nil
}

// field is a map's entry under key, or an absent node, whose As methods
// fail, if there is none.
func field(node datamodel.Node, key string) datamodel.Node {
	if value, err := node.LookupByString(key); err == nil {
		return value
	}
	return datamodel.Absent
}
