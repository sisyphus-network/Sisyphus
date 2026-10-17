package storage

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ipfs/boxo/ipld/merkledag"
	"github.com/ipfs/go-cid"
)

// usageIndex keeps, for each group of owners, how many bytes the group's
// pins reach, as pins are placed, released and lapse, so that what a group
// keeps is known without walking what it has pinned. Within a group a
// block counts once, however many of its pins reach it; each block it
// reaches is counted by reference, so that releasing one pin frees only
// what no other pin of the group still reaches. Its maps are guarded by the
// store's pinMu.
type usageIndex struct {
	group func(owner string) string
	// roots holds the blocks each pinned blob reaches, found once, since
	// what is under a CID never changes, and kept while a charged pin
	// still holds it.
	roots   map[cid.Cid]*reach
	groups  map[string]*groupUse
	charged map[pinKey]charge
	lapses  lapseHeap
}

// reach is the blocks a blob reaches, each once, and how many charged pins
// hold it.
type reach struct {
	blocks []block
	pins   int
}

type block struct {
	hash string
	size uint64
}

// groupUse is what one group keeps: how many of its pins reach each block,
// and the bytes of those it reaches at all.
type groupUse struct {
	refs  map[string]int
	bytes uint64
}

// charge is the group a pin is counted towards, and when it lapses.
type charge struct {
	group   string
	expires time.Time
}

func newUsageIndex(group func(owner string) string) *usageIndex {
	return &usageIndex{
		group:   group,
		roots:   make(map[cid.Cid]*reach),
		groups:  make(map[string]*groupUse),
		charged: make(map[pinKey]charge),
	}
}

// chargeLocked counts a pin towards its owner's group, or, for one already
// counted, takes note of its new expiry. r is what the pinned blob reaches,
// needed only when the index has not got it already.
func (ix *usageIndex) chargeLocked(key pinKey, expires time.Time, r *reach) {
	if ix.retimeLocked(key, expires) {
		return
	}
	name := ix.group(key.owner)
	if name == "" {
		return
	}
	root := ix.roots[key.cid]
	if root == nil {
		root = r
		ix.roots[key.cid] = root
	}
	root.pins++
	use := ix.groups[name]
	if use == nil {
		use = &groupUse{refs: make(map[string]int)}
		ix.groups[name] = use
	}
	for _, b := range root.blocks {
		if use.refs[b.hash] == 0 {
			use.bytes += b.size
		}
		use.refs[b.hash]++
	}
	ix.charged[key] = charge{group: name, expires: expires}
	ix.lapseAt(key, expires)
}

// retimeLocked takes note of a counted pin's new expiry, and reports
// whether the pin is counted.
func (ix *usageIndex) retimeLocked(key pinKey, expires time.Time) bool {
	c, counted := ix.charged[key]
	if counted {
		c.expires = expires
		ix.charged[key] = c
		ix.lapseAt(key, expires)
	}
	return counted
}

// unchargeLocked stops counting a pin, if it was counted.
func (ix *usageIndex) unchargeLocked(key pinKey) {
	c, counted := ix.charged[key]
	if !counted {
		return
	}
	delete(ix.charged, key)
	root, use := ix.roots[key.cid], ix.groups[c.group]
	for _, b := range root.blocks {
		if use.refs[b.hash]--; use.refs[b.hash] == 0 {
			delete(use.refs, b.hash)
			use.bytes -= b.size
		}
	}
	if len(use.refs) == 0 {
		delete(ix.groups, c.group)
	}
	if root.pins--; root.pins == 0 {
		delete(ix.roots, key.cid)
	}
}

// knowsLocked reports whether the index has what c reaches, or whether a
// pin of owner's on it would not be counted at all, so that nothing need
// be found out before it is pinned.
func (ix *usageIndex) knowsLocked(owner string, c cid.Cid) bool {
	return ix.roots[c] != nil || ix.group(owner) == ""
}

// lapseAt has a pin stop counting when it expires, if it does.
func (ix *usageIndex) lapseAt(key pinKey, expires time.Time) {
	if !expires.IsZero() {
		heap.Push(&ix.lapses, lapse{at: expires, key: key})
	}
}

// lapseLocked stops counting every pin that expired before now. A pin
// whose expiry has since changed is not stopped by its old one.
func (ix *usageIndex) lapseLocked(now time.Time) {
	for len(ix.lapses) > 0 && ix.lapses[0].at.Before(now) {
		l := heap.Pop(&ix.lapses).(lapse)
		if c, counted := ix.charged[l.key]; counted && c.expires.Equal(l.at) {
			ix.unchargeLocked(l.key)
		}
	}
}

type lapse struct {
	at  time.Time
	key pinKey
}

// lapseHeap orders lapses soonest first.
type lapseHeap []lapse

func (h lapseHeap) Len() int           { return len(h) }
func (h lapseHeap) Less(i, j int) bool { return h[i].at.Before(h[j].at) }
func (h lapseHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *lapseHeap) Push(x any)        { *h = append(*h, x.(lapse)) }
func (h *lapseHeap) Pop() any {
	old := *h
	last := old[len(old)-1]
	*h = old[:len(old)-1]
	return last
}

// reachOf finds the blocks blob c reaches, each once, and their sizes.
func (s *Store) reachOf(ctx context.Context, c cid.Cid) (*reach, error) {
	r := &reach{}
	seen := make(map[string]struct{})
	var failed error
	visit := func(c cid.Cid) bool {
		// Blocks are stored by hash alone, so that is what is compared.
		if _, done := seen[string(c.Hash())]; done {
			return false
		}
		seen[string(c.Hash())] = struct{}{}
		size, err := s.blocks.GetSize(ctx, c)
		if err != nil {
			failed = err
			return false
		}
		r.blocks = append(r.blocks, block{hash: string(c.Hash()), size: uint64(size)})
		return true
	}
	if err := errors.Join(merkledag.Walk(ctx, s.links, c, visit), failed); err != nil {
		return nil, fmt.Errorf("blob %s: %w", c, err)
	}
	return r, nil
}

// GroupPins has the store count, from now on, how many bytes each group of
// owners keeps, which Usage then says. group names the group an owner's
// pins count towards, or nothing for an owner whose pins count towards
// none; it is called with the pins locked, so it must not call the store,
// and it must name the same group for an owner every time. The pins
// already held are counted at once. Those of them that reach a block the
// store does not have, as no pin should, cannot be counted and are left
// out, and said so in the error; the rest are counted all the same.
func (s *Store) GroupPins(ctx context.Context, group func(owner string) string) error {
	// Collection and pinning wait while the pins already held are walked.
	s.gcMu.Lock()
	defer s.gcMu.Unlock()
	ix := newUsageIndex(group)
	// The pins are walked with them unlocked, and then looked at again
	// with them locked, until none is left that has not been walked, so
	// that the index is published counting every pin there is then, even
	// one that came while the walk went on.
	reached := make(map[cid.Cid]*reach)
	walked := make(map[cid.Cid]bool)
	var failed []error
	for {
		s.pinMu.Lock()
		var unwalked []pinKey
		for key := range s.pins {
			if !walked[key.cid] && group(key.owner) != "" {
				walked[key.cid] = true
				unwalked = append(unwalked, key)
			}
		}
		if len(unwalked) == 0 {
			break // with the pins locked
		}
		s.pinMu.Unlock()
		for _, key := range unwalked {
			r, err := s.reachOf(ctx, key.cid)
			if err != nil {
				failed = append(failed, fmt.Errorf("pin of %s: %w", key.owner, err))
			}
			reached[key.cid] = r
		}
	}
	defer s.pinMu.Unlock()
	// Pins released while the walk went on are not there to count.
	for key, expires := range s.pins {
		if r := reached[key.cid]; r != nil || ix.knowsLocked(key.owner, key.cid) {
			ix.chargeLocked(key, expires, r)
		}
	}
	s.index = ix
	if len(failed) > 0 {
		return fmt.Errorf("group pins: %w", errors.Join(failed...))
	}
	return nil
}

// Usage says how many bytes the pins of each group of owners keep, as of
// now, by the groups GroupPins was given; with none given, it says
// nothing. Within a group a block is counted once, however many of its
// pins reach it; a block kept by several groups is counted in full for
// each. That is what each has asked to be kept, which does not change
// when another keeps the same; what it costs on disk, where a block is
// stored once, is what Size says.
//
// Pins that lapsed before now are not counted, though they are dropped
// only by the next collection. Once a pin has lapsed it is not counted
// again, so now should not go back between calls.
func (s *Store) Usage(now time.Time) map[string]uint64 {
	s.pinMu.Lock()
	defer s.pinMu.Unlock()
	if s.index == nil {
		return nil
	}
	s.index.lapseLocked(now)
	used := make(map[string]uint64, len(s.index.groups))
	for name, use := range s.index.groups {
		used[name] = use.bytes
	}
	return used
}
