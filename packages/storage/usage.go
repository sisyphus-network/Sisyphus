package storage

import (
	"cmp"
	"context"
	"fmt"
	"time"

	"github.com/ipfs/boxo/ipld/merkledag"
	"github.com/ipfs/go-cid"
)

// Usage says how many bytes the pins of each group of owners keep, as of
// now. group names the group an owner's pins count towards, or nothing
// for an owner whose pins count towards none. Within a group a block is
// counted once, however many of its pins reach it; a block kept by
// several groups is counted in full for each. That is what each has asked
// to be kept, which does not change when another keeps the same; what it
// costs on disk, where a block is stored once, is what Size says.
//
// Pins that lapsed before now are not counted, though they are dropped
// only by the next collection. Collection waits while Usage runs.
func (s *Store) Usage(ctx context.Context, now time.Time, group func(owner string) string) (map[string]uint64, error) {
	s.gcMu.RLock()
	defer s.gcMu.RUnlock()

	s.pinMu.Lock()
	roots := make(map[string][]cid.Cid)
	for key, expires := range s.pins {
		if !expires.IsZero() && expires.Before(now) {
			continue
		}
		if name := group(key.owner); name != "" {
			roots[name] = append(roots[name], key.cid)
		}
	}
	s.pinMu.Unlock()

	used := make(map[string]uint64, len(roots))
	for name, pinned := range roots {
		// Blocks are stored by hash alone, so that is what is compared.
		counted := make(map[string]struct{})
		var bytes uint64
		var failed error
		visit := func(c cid.Cid) bool {
			if _, seen := counted[string(c.Hash())]; seen {
				return false
			}
			counted[string(c.Hash())] = struct{}{}
			size, err := s.blocks.GetSize(ctx, c)
			if err != nil {
				failed = err
				return false
			}
			bytes += uint64(size)
			return true
		}
		for _, root := range pinned {
			// A block that cannot be read, or measured, is one a pin
			// says is kept and is not: the count would be wrong.
			if err := cmp.Or(merkledag.Walk(ctx, s.links, root, visit), failed); err != nil {
				return nil, fmt.Errorf("usage of %s: pinned blob %s: %w", name, root, err)
			}
		}
		used[name] = bytes
	}
	return used, nil
}
