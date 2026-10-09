// Package names makes and checks the signed records behind names.
//
// A content ID names bytes that never change. A name is a node's ID, and
// stands for whatever that node last published under it: a record that
// carries a content ID, a sequence number that only goes up, and a time
// after which it is no longer good, all signed with the node's key. The ID
// alone is enough to check the signature, so a record can be believed no
// matter who hands it over.
//
// The records are IPNS records, so other IPFS programs can read and check
// them. Nothing here touches a network.
package names

import (
	"errors"
	"fmt"
	"time"

	"github.com/ipfs/boxo/ipns"
	"github.com/ipfs/boxo/path"
	"github.com/ipfs/go-cid"
	"github.com/libp2p/go-libp2p/core/crypto"

	"github.com/sisyphus-network/Sisyphus/packages/identity"
)

const (
	// DefaultLifetime is how long a record is good for unless its publisher
	// says otherwise, and DefaultTTL how long whoever reads one may go on
	// using it before asking again. They are what Kubo uses.
	DefaultLifetime = ipns.DefaultRecordLifetime
	DefaultTTL      = ipns.DefaultRecordTTL
)

// ContentType is the media type of a record as it is signed and passed on.
const ContentType = "application/vnd.ipfs.ipns-record"

// ErrExpired reports a record that was good once and no longer is.
var ErrExpired = errors.New("the record has expired")

// Record is what a node has published under its name.
type Record struct {
	// Name is the ID of the node that signed the record.
	Name string
	// Value is the content the name stands for.
	Value cid.Cid
	// Sequence orders a name's records: a higher one is newer.
	Sequence uint64
	// Expires is when the record stops being good.
	Expires time.Time
	// TTL is how long a reader may use the record before asking again.
	TTL time.Duration

	raw []byte
}

// Bytes returns the record as it was signed, which is the form to store and
// to pass on.
func (r *Record) Bytes() []byte {
	return r.raw
}

// Expired reports whether the record is no longer good at the given time.
func (r *Record) Expired(now time.Time) bool {
	return now.After(r.Expires)
}

// Replaces reports whether the record should take the place of an older one
// for the same name: it does if its sequence number is higher, or the same
// and it is good for longer. The second is how a record is renewed without
// its content changing.
func (r *Record) Replaces(old *Record) bool {
	if r.Sequence != old.Sequence {
		return r.Sequence > old.Sequence
	}
	return r.Expires.After(old.Expires)
}

// Fresh returns how long from now a reader may keep using the record: its
// TTL, or what is left of its life if that is less.
func (r *Record) Fresh(now time.Time) time.Duration {
	return max(0, min(r.TTL, r.Expires.Sub(now)))
}

// Make signs a record saying that the node's name stands for value, good
// for lifetime from now.
func Make(ident *identity.Identity, value cid.Cid, sequence uint64, lifetime, ttl time.Duration, now time.Time) *Record {
	// None of these steps fails for a node's key, which is Ed25519, and a
	// content ID.
	key, _ := crypto.UnmarshalPrivateKey(ident.Libp2pKey())
	// The time is kept to the precision the record states it in.
	expires := now.Add(lifetime).UTC()
	signed, _ := ipns.NewRecord(key, path.FromCid(value), sequence, expires, ttl)
	raw, _ := ipns.MarshalRecord(signed)
	return &Record{Name: ident.ID(), Value: value, Sequence: sequence, Expires: expires, TTL: ttl, raw: raw}
}

// ID returns the node ID a name stands for. A name may be spelled as the ID
// itself, which starts "12D3KooW", or as IPFS programs print it, which
// starts "k51", and with or without /ipns/ in front.
func ID(name string) (string, error) {
	parsed, err := ipns.NameFromString(name)
	if err != nil {
		return "", fmt.Errorf("%q is not a name: a name is a node's ID", name)
	}
	return parsed.Peer().String(), nil
}

// Parse reads a record handed over as the one for name and checks that the
// node of that name signed it. It does not look at whether the record has
// expired: Check does both.
func Parse(name string, raw []byte) (*Record, error) {
	parsed, err := ipns.NameFromString(name)
	if err != nil {
		return nil, fmt.Errorf("%q is not a name: a name is a node's ID", name)
	}
	signed, err := ipns.UnmarshalRecord(raw)
	if err != nil {
		return nil, fmt.Errorf("that is not a record for a name: %w", err)
	}
	// Expiry is the last thing looked at there, so a record refused for
	// that alone is sound in every other way.
	if err := ipns.ValidateWithName(signed, parsed); err != nil && !errors.Is(err, ipns.ErrExpiredRecord) {
		return nil, fmt.Errorf("the record is not one signed by node %s: %w", parsed.Peer(), err)
	}
	value, _ := signed.Value() // nil if it is no path at all
	if value == nil || value.Namespace() != path.IPFSNamespace || len(value.Segments()) != 2 {
		return nil, fmt.Errorf("the record does not name a file by its content ID, as /ipfs/<cid>")
	}
	c, _ := cid.Decode(value.Segments()[1]) // a path under /ipfs/ starts with a content ID
	// A record that passed has an expiry. One with no sequence number or
	// TTL counts as having zero.
	sequence, _ := signed.Sequence()
	expires, _ := signed.Validity()
	ttl, _ := signed.TTL()
	return &Record{Name: parsed.Peer().String(), Value: c, Sequence: sequence, Expires: expires, TTL: ttl, raw: raw}, nil
}

// Check reads a record as Parse does and also refuses one that has expired
// by the given time, with ErrExpired.
func Check(name string, raw []byte, now time.Time) (*Record, error) {
	record, err := Parse(name, raw)
	if err != nil {
		return nil, err
	}
	if record.Expired(now) {
		return nil, fmt.Errorf("%w: it was good until %s", ErrExpired, record.Expires.UTC().Format(time.RFC3339))
	}
	return record, nil
}
