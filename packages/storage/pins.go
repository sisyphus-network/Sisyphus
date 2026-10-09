package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/ipfs/boxo/ipld/merkledag"
	"github.com/ipfs/go-cid"
	ipld "github.com/ipfs/go-ipld-format"
)

// A blob is kept for as long as something pins it. Garbage collection
// removes every block that no live pin reaches.

// GracePeriod is how long a blob is kept after Put if nothing pins it,
// giving whoever stored it time to pin it properly.
const GracePeriod = time.Hour

// graceOwner owns the pin Put places on every new blob.
const graceOwner = "recent"

// Pin keeps one blob on behalf of one owner. Several owners may pin the same
// blob; it is kept until every pin on it has been released or has expired.
type Pin struct {
	CID   cid.Cid
	Owner string
	// Expires is when the pin lapses. The zero time means it holds until
	// released.
	Expires time.Time
}

type pinKey struct {
	cid   cid.Cid
	owner string
}

// Pin pins blobs the store holds for owner until expires, or until released
// if expires is the zero time. Pinning again replaces the earlier expiry.
func (s *Store) Pin(ctx context.Context, owner string, expires time.Time, cids ...cid.Cid) error {
	if len(cids) == 0 {
		return nil
	}
	// Holding off collection makes "present" and "pinned" one step: a blob
	// cannot be swept between the check and the pin.
	s.gcMu.RLock()
	defer s.gcMu.RUnlock()
	for _, c := range cids {
		has, err := s.blocks.Has(ctx, c)
		if err != nil {
			return err
		}
		if !has {
			return fmt.Errorf("pin %s: %w", c, ErrNotFound)
		}
	}
	s.pinMu.Lock()
	defer s.pinMu.Unlock()
	for _, c := range cids {
		s.pins[pinKey{c, owner}] = expires
	}
	return s.savePinsLocked()
}

// Unpin releases owner's pins on the given blobs. Blobs it has not pinned
// are ignored.
func (s *Store) Unpin(owner string, cids ...cid.Cid) error {
	if len(cids) == 0 {
		return nil
	}
	s.pinMu.Lock()
	defer s.pinMu.Unlock()
	for _, c := range cids {
		delete(s.pins, pinKey{c, owner})
	}
	return s.savePinsLocked()
}

// ExpireOpenPins gives an expiry of at to every pin without one whose owner
// is orphaned. It is for pins whose owners are gone and can no longer
// release them.
func (s *Store) ExpireOpenPins(orphaned func(owner string) bool, at time.Time) error {
	s.pinMu.Lock()
	defer s.pinMu.Unlock()
	for key, expires := range s.pins {
		if expires.IsZero() && orphaned(key.owner) {
			s.pins[key] = at
		}
	}
	return s.savePinsLocked()
}

// Pins lists every pin, ordered by CID and then owner.
func (s *Store) Pins() []Pin {
	s.pinMu.Lock()
	defer s.pinMu.Unlock()
	pins := make([]Pin, 0, len(s.pins))
	for key, expires := range s.pins {
		pins = append(pins, Pin{CID: key.cid, Owner: key.owner, Expires: expires})
	}
	sort.Slice(pins, func(i, j int) bool {
		if pins[i].CID != pins[j].CID {
			return pins[i].CID.String() < pins[j].CID.String()
		}
		return pins[i].Owner < pins[j].Owner
	})
	return pins
}

// Size returns how many bytes the store occupies on disk. It is zero for a
// store that is not on disk.
func (s *Store) Size(ctx context.Context) (uint64, error) {
	return s.size(ctx)
}

// Collected reports what a garbage collection removed.
type Collected struct {
	// ExpiredPins is how many pins had lapsed and were dropped.
	ExpiredPins int
	// Blocks and Bytes are what was deleted.
	Blocks int
	Bytes  uint64
}

// GC drops pins that expired before now, then deletes every block that no
// remaining pin reaches. Stores and pins wait while it runs.
func (s *Store) GC(ctx context.Context, now time.Time) (Collected, error) {
	s.gcMu.Lock()
	defer s.gcMu.Unlock()
	var done Collected

	s.pinMu.Lock()
	var roots []cid.Cid
	for key, expires := range s.pins {
		if !expires.IsZero() && expires.Before(now) {
			delete(s.pins, key)
			done.ExpiredPins++
			continue
		}
		roots = append(roots, key.cid)
	}
	err := s.savePinsLocked()
	s.pinMu.Unlock()
	if err != nil {
		return done, err
	}

	// Mark. Blocks are stored by hash alone, so that is what is compared.
	// Any failure here ends the collection before anything is deleted.
	keep := make(map[string]struct{})
	visit := func(c cid.Cid) bool {
		if _, seen := keep[string(c.Hash())]; seen {
			return false
		}
		keep[string(c.Hash())] = struct{}{}
		return true
	}
	for _, root := range roots {
		if err := merkledag.Walk(ctx, s.links, root, visit); err != nil {
			return done, fmt.Errorf("collect garbage: pinned blob %s: %w", root, err)
		}
	}

	// Sweep.
	blocks, err := s.blocks.AllKeysChan(ctx)
	if err != nil {
		return done, fmt.Errorf("collect garbage: %w", err)
	}
	for c := range blocks {
		if _, kept := keep[string(c.Hash())]; kept {
			continue
		}
		size, _ := s.blocks.GetSize(ctx, c) // only for the report
		if err := s.blocks.DeleteBlock(ctx, c); err != nil {
			return done, fmt.Errorf("collect garbage: %w", err)
		}
		done.Blocks++
		done.Bytes += uint64(size)
	}
	return done, nil
}

// links returns the blocks that block c refers to and a pin on it keeps.
// Leaves refer to none, and are not read to find that out. A node of linked
// data keeps the nodes it links to and not the blobs.
func (s *Store) links(ctx context.Context, c cid.Cid) ([]*ipld.Link, error) {
	switch c.Type() {
	case cid.Raw:
		return nil, nil
	case cid.DagCBOR:
		return s.nodesLinked(ctx, c)
	}
	node, err := s.dag.Get(ctx, c)
	if err != nil {
		return nil, err
	}
	return node.Links(), nil
}

// pinRecord is how a pin is written to disk.
type pinRecord struct {
	CID     string     `json:"cid"`
	Owner   string     `json:"owner"`
	Expires *time.Time `json:"expires,omitempty"`
}

// savePinsLocked writes the pins of a disk store to its pin file. Grace pins
// are included but never cause a write of their own, so storing a blob does
// not wait on the pin file.
func (s *Store) savePinsLocked() error {
	if s.pinFile == "" {
		return nil
	}
	records := make([]pinRecord, 0, len(s.pins))
	for key, expires := range s.pins {
		record := pinRecord{CID: key.cid.String(), Owner: key.owner}
		if !expires.IsZero() {
			record.Expires = &expires
		}
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].CID != records[j].CID {
			return records[i].CID < records[j].CID
		}
		return records[i].Owner < records[j].Owner
	})
	data, _ := json.MarshalIndent(records, "", "\t") // plain strings and times always encode

	// Write beside the file and rename, so a crash leaves the old pins or
	// the new ones and never half of either.
	tmp := s.pinFile + ".tmp"
	if err := writeSynced(tmp, data); err != nil {
		return fmt.Errorf("save pins: %w", err)
	}
	if err := os.Rename(tmp, s.pinFile); err != nil {
		return fmt.Errorf("save pins: %w", err)
	}
	return nil
}

func writeSynced(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	return errors.Join(err, f.Close())
}

// loadPins reads a pin file. A missing file means no pins.
func loadPins(path string) (map[pinKey]time.Time, error) {
	pins := make(map[pinKey]time.Time)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return pins, nil
	}
	if err != nil {
		return nil, err
	}
	var records []pinRecord
	if err := json.Unmarshal(data, &records); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for _, record := range records {
		c, err := cid.Decode(record.CID)
		if err != nil {
			return nil, fmt.Errorf("%s: pin %q: %w", path, record.CID, err)
		}
		var expires time.Time
		if record.Expires != nil {
			expires = *record.Expires
		}
		pins[pinKey{c, record.Owner}] = expires
	}
	return pins, nil
}
