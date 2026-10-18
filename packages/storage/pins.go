package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
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

// GraceOwner owns the pin Put places on every new blob.
const GraceOwner = "recent"

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
	return s.pin(ctx, owner, expires, 0, cids)
}

// OverShareError is returned by PinWithin for pins that would have the
// owner's group keep more than it may.
type OverShareError struct {
	Group string
	// Keeps is what the group would have kept with the pins, and May what
	// it may keep.
	Keeps, May uint64
}

func (e *OverShareError) Error() string {
	return fmt.Sprintf("%s would keep %d bytes, and may keep %d", e.Group, e.Keeps, e.May)
}

// PinWithin pins as Pin does, unless that would have the group owner's pins
// count towards keep more than limit bytes, as Usage counts it, and more
// than it kept before; then it pins nothing and returns an
// *OverShareError. Pins that add nothing, such as one placed again, are
// never refused. The pins are checked and
// placed in one step, so pins placed together cannot together go past the
// limit. A limit of zero, or an owner whose pins count towards no group,
// is held to none; so is every owner until GroupPins has named the groups.
func (s *Store) PinWithin(ctx context.Context, owner string, expires time.Time, limit uint64, cids ...cid.Cid) error {
	return s.pin(ctx, owner, expires, limit, cids)
}

func (s *Store) pin(ctx context.Context, owner string, expires time.Time, limit uint64, cids []cid.Cid) error {
	if len(cids) == 0 {
		return nil
	}
	// Holding off collection makes "present" and "pinned" one step: a blob
	// cannot be swept between the check and the pin.
	s.gcMu.RLock()
	defer s.gcMu.RUnlock()
	return s.pinHeld(ctx, owner, expires, limit, cids)
}

// pinHeld is pin with collection already held off.
func (s *Store) pinHeld(ctx context.Context, owner string, expires time.Time, limit uint64, cids []cid.Cid) error {
	for _, c := range cids {
		has, err := s.blocks.Has(ctx, c)
		if err != nil {
			return err
		}
		if !has {
			return fmt.Errorf("pin %s: %w", c, ErrNotFound)
		}
	}
	// What a counted pin keeps is found before the pins are locked, since
	// that reads the blocks.
	reached := make(map[cid.Cid]*reach)
	for {
		s.pinMu.Lock()
		var unknown []cid.Cid
		for _, c := range cids {
			if s.index != nil && reached[c] == nil && !s.index.knowsLocked(owner, c) {
				unknown = append(unknown, c)
			}
		}
		if len(unknown) == 0 {
			break // with the pins locked
		}
		s.pinMu.Unlock()
		for _, c := range unknown {
			r, err := s.reachOf(ctx, c)
			if err != nil {
				return fmt.Errorf("pin %s: %w", c, err)
			}
			reached[c] = r
		}
	}
	defer s.pinMu.Unlock()
	// What the owner's group keeps before these pins, if it is held to a
	// limit: pins that add nothing to it, such as one placed again, are
	// never refused, even for a group already past its limit.
	var group string
	var keptBefore uint64
	if limit > 0 && s.index != nil {
		if group = s.index.group(owner); group != "" {
			s.index.lapseLocked(time.Now())
			keptBefore = s.index.kept(group)
		}
	}
	type before struct {
		expires time.Time
		held    bool
	}
	was := make(map[pinKey]before, len(cids))
	for _, c := range cids {
		key := pinKey{c, owner}
		if _, seen := was[key]; !seen {
			expires, held := s.pins[key]
			was[key] = before{expires, held}
		}
		s.pins[key] = expires
		if s.index != nil {
			s.index.chargeLocked(key, expires, reached[c])
		}
	}
	if group != "" {
		s.index.lapseLocked(time.Now())
		if keeps := s.index.kept(group); keeps > limit && keeps > keptBefore {
			// Put back what the pins were before.
			for key, b := range was {
				if b.held {
					s.pins[key] = b.expires
					s.index.retimeLocked(key, b.expires)
				} else {
					delete(s.pins, key)
					s.index.unchargeLocked(key)
				}
			}
			return &OverShareError{Group: group, Keeps: keeps, May: limit}
		}
	}
	s.tellPinsChangedLocked()
	s.tellGCChangedLocked(false)
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
	changed := false
	for _, c := range cids {
		key := pinKey{c, owner}
		if _, held := s.pins[key]; held {
			delete(s.pins, key)
			changed = true
			if s.index != nil {
				s.index.unchargeLocked(key)
			}
		}
	}
	s.tellPinsChangedLocked()
	if changed {
		s.tellGCChangedLocked(true)
	}
	return s.savePinsLocked()
}

// UnpinPrefix releases every pin whose owner's name begins with prefix,
// whatever blobs they are on: for owners whose pins are not all known by
// name, such as some left by a process that stopped before noting them.
func (s *Store) UnpinPrefix(prefix string) error {
	s.pinMu.Lock()
	defer s.pinMu.Unlock()
	changed := false
	for key := range s.pins {
		if strings.HasPrefix(key.owner, prefix) {
			delete(s.pins, key)
			changed = true
			if s.index != nil {
				s.index.unchargeLocked(key)
			}
		}
	}
	s.tellPinsChangedLocked()
	if changed {
		s.tellGCChangedLocked(true)
	}
	return s.savePinsLocked()
}

// ExpireOpenPins gives an expiry of at to every pin without one whose owner
// is orphaned. It is for pins whose owners are gone and can no longer
// release them.
func (s *Store) ExpireOpenPins(orphaned func(owner string) bool, at time.Time) error {
	s.pinMu.Lock()
	defer s.pinMu.Unlock()
	changed := false
	for key, expires := range s.pins {
		if expires.IsZero() && orphaned(key.owner) {
			s.pins[key] = at
			changed = true
			if s.index != nil {
				s.index.retimeLocked(key, at)
			}
		}
	}
	if changed {
		s.tellGCChangedLocked(false)
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

// Kept lists the blobs that something has pinned, each once, in order of
// CID. A blob that only the grace period after Put is keeping is not among
// them: nobody has asked for it to be kept. Nor is anything pinned that is
// not a blob, such as the record of a job: that is linked data, whose pin
// keeps it and not the blobs it names, which a pin made of it elsewhere
// would not know to leave out.
func (s *Store) Kept() []cid.Cid {
	s.pinMu.Lock()
	defer s.pinMu.Unlock()
	seen := make(map[cid.Cid]struct{})
	var kept []cid.Cid
	for key := range s.pins {
		if kind := key.cid.Type(); kind != cid.Raw && kind != cid.DagProtobuf {
			continue
		}
		if _, dup := seen[key.cid]; key.owner == GraceOwner || dup {
			continue
		}
		seen[key.cid] = struct{}{}
		kept = append(kept, key.cid)
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].String() < kept[j].String() })
	return kept
}

// OnPinChange has f called after every pin that is placed, released or
// dropped as expired, so that whatever mirrors the store's pins elsewhere
// can look again at Kept. f is called with the pins locked: it must return
// at once and must not call the store.
func (s *Store) OnPinChange(f func()) {
	s.pinMu.Lock()
	defer s.pinMu.Unlock()
	s.pinsChanged = f
}

// OnGCChange has f called after every change to pin roots or expiry. The
// mayCollect argument says whether a pin was removed; the listener must
// re-read the pins either way. f is called with pins locked: it must return
// at once and must not call the store.
func (s *Store) OnGCChange(f func(mayCollect bool)) {
	s.pinMu.Lock()
	defer s.pinMu.Unlock()
	s.gcChanged = f
}

func (s *Store) tellPinsChangedLocked() {
	if s.pinsChanged != nil {
		s.pinsChanged()
	}
}

func (s *Store) tellGCChangedLocked(mayCollect bool) {
	if s.gcChanged != nil {
		s.gcChanged(mayCollect)
	}
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
			if s.index != nil {
				s.index.unchargeLocked(key)
			}
			done.ExpiredPins++
			continue
		}
		roots = append(roots, key.cid)
	}
	if done.ExpiredPins > 0 {
		s.tellPinsChangedLocked()
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
