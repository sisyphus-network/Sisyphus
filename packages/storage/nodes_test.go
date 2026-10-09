package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ipfs/boxo/blockstore"
	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	"github.com/ipld/go-ipld-prime/codec/dagcbor"
	"github.com/ipld/go-ipld-prime/fluent"
	cidlink "github.com/ipld/go-ipld-prime/linking/cid"
	"github.com/ipld/go-ipld-prime/node/basicnode"
)

// node returns the DAG-CBOR encoding of a node with the given label that
// links to the given CIDs, and the CID it will be stored under.
func node(label string, links ...cid.Cid) ([]byte, cid.Cid) {
	built := fluent.MustBuildMap(basicnode.Prototype.Map, -1, func(m fluent.MapAssembler) {
		m.AssembleEntry("label").AssignString(label)
		m.AssembleEntry("links").CreateList(-1, func(l fluent.ListAssembler) {
			for _, c := range links {
				l.AssembleValue().AssignLink(cidlink.Link{Cid: c})
			}
		})
	})
	var data bytes.Buffer
	dagcbor.Encode(built, &data)
	c, _ := nodeBuilder.Sum(data.Bytes())
	return data.Bytes(), c
}

func hasBlock(t *testing.T, s *Store, c cid.Cid) bool {
	t.Helper()
	has, err := s.blocks.Has(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	return has
}

func TestNodesAreStoredUnderTheHashOfTheirBytesAndReadBack(t *testing.T) {
	s := NewMemory()
	leaf, leafCID := node("leaf")
	root, rootCID := node("root", leafCID)

	stored, err := s.PutNodes(ctx, root, leaf)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.Equals(rootCID) || stored.Type() != cid.DagCBOR {
		t.Errorf("the root was stored as %s, want %s, a DAG-CBOR CID", stored, rootCID)
	}
	if again, err := s.PutNodes(ctx, root, leaf); err != nil || !again.Equals(stored) {
		t.Errorf("storing the same bytes again gave %s, %v", again, err)
	}
	for c, want := range map[cid.Cid][]byte{rootCID: root, leafCID: leaf} {
		if got, err := s.GetNode(ctx, c); err != nil || !bytes.Equal(got, want) {
			t.Errorf("node %s read back as %x, %v", c, got, err)
		}
	}
	// Only the root is held for the grace period: it holds the rest.
	if pins := s.Pins(); len(pins) != 1 || !pins[0].CID.Equals(rootCID) || pins[0].Owner != GraceOwner {
		t.Errorf("pins after storing two nodes: %v", pins)
	}
}

func TestGetNodeReturnsOnlyNodesThatAreThereAndIntact(t *testing.T) {
	s := NewMemory()
	blob := put(t, s, blobA)
	if _, err := s.GetNode(ctx, blob); err == nil || !strings.Contains(err.Error(), "not a node of linked data") {
		t.Errorf("reading a blob as a node: %v", err)
	}
	data, absent := node("never stored")
	if _, err := s.GetNode(ctx, absent); !errors.Is(err, ErrNotFound) {
		t.Errorf("reading a node the store does not hold: %v, want ErrNotFound", err)
	}

	// Other bytes under the node's CID, as a damaged disk might return.
	damaged, _ := blocks.NewBlockWithCid(append([]byte{0}, data...), absent)
	if err := s.blocks.Put(ctx, damaged); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetNode(ctx, absent); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Errorf("reading a node whose bytes do not match its CID: %v", err)
	}

	s.blocks = failingBlocks{Blockstore: s.blocks, failGet: true}
	if _, err := s.GetNode(ctx, absent); !errors.Is(err, errBadDisk) {
		t.Errorf("GetNode: %v, want the block store's error", err)
	}
}

// readOnly is a block store that takes nothing in.
type readOnly struct{ blockstore.Blockstore }

func (readOnly) Put(context.Context, blocks.Block) error { return errBadDisk }

func TestPutNodesStoresOnlyDAGCBORAndReportsAStoreThatFails(t *testing.T) {
	s := NewMemory()
	leaf, _ := node("leaf")
	if _, err := s.PutNodes(ctx, []byte("\xff not a node")); err == nil || !strings.Contains(err.Error(), "not DAG-CBOR") {
		t.Errorf("storing bytes that are not DAG-CBOR: %v", err)
	}
	if _, err := s.PutNodes(ctx, leaf, []byte("\xff nor is this")); err == nil || !strings.Contains(err.Error(), "not DAG-CBOR") {
		t.Errorf("storing a linked node that is not DAG-CBOR: %v", err)
	}
	if len(s.Pins()) != 0 {
		t.Errorf("a store that took nothing in has pins: %v", s.Pins())
	}
	s.blocks = readOnly{s.blocks}
	if _, err := s.PutNodes(ctx, leaf); !errors.Is(err, errBadDisk) {
		t.Errorf("PutNodes: %v, want the block store's error", err)
	}
}

func TestAPinOnANodeKeepsTheNodesItLinksToAndNotTheBlobs(t *testing.T) {
	s := NewMemory()
	blob := put(t, s, blobA)
	inner, innerCID := node("inner", blob)
	middle, middleCID := node("middle", innerCID, blob)
	root, rootCID := node("root", middleCID, blob)
	if _, err := s.PutNodes(ctx, root, middle, inner); err != nil {
		t.Fatal(err)
	}
	stray, strayCID := node("stray")
	if _, err := s.PutNodes(ctx, stray); err != nil {
		t.Fatal(err)
	}
	if err := s.Pin(ctx, "record:j", time.Time{}, rootCID); err != nil {
		t.Fatal(err)
	}

	gc(t, s, afterGrace())
	for name, c := range map[string]cid.Cid{"root": rootCID, "middle": middleCID, "inner": innerCID} {
		if !hasBlock(t, s, c) {
			t.Errorf("the %s node was collected from under a pin on the root", name)
		}
	}
	if hasBlock(t, s, blob) {
		t.Error("a blob that only nodes link to outlived its own pins")
	}
	if hasBlock(t, s, strayCID) {
		t.Error("a node nothing pins or links to was kept")
	}

	if err := s.Unpin("record:j", rootCID); err != nil {
		t.Fatal(err)
	}
	if done := gc(t, s, afterGrace()); done.Blocks != 3 {
		t.Errorf("collected %d blocks once the root was released, want its 3 nodes", done.Blocks)
	}
}

func TestGCStopsBeforeDeletingIfAPinnedNodeCannotBeRead(t *testing.T) {
	s := NewMemory()
	other := put(t, s, blobB)
	garbage := []byte("\xff not a node")
	mangled, _ := nodeBuilder.Sum(garbage)
	block, _ := blocks.NewBlockWithCid(garbage, mangled)
	if err := s.blocks.Put(ctx, block); err != nil {
		t.Fatal(err)
	}
	if err := s.Pin(ctx, "user", time.Time{}, mangled); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GC(ctx, afterGrace()); err == nil || !strings.Contains(err.Error(), "not DAG-CBOR") {
		t.Errorf("collecting with a pinned node that does not decode: %v", err)
	}
	if err := s.blocks.DeleteBlock(ctx, mangled); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GC(ctx, afterGrace()); err == nil || !strings.Contains(err.Error(), "pinned blob "+mangled.String()) {
		t.Errorf("collecting with a pinned node that is gone: %v", err)
	}
	if !hasBlock(t, s, other) {
		t.Error("a failed collection still deleted an unpinned blob")
	}
}

func TestWhatTheStorePutsInKuboIsAnOrdinaryIPLDNode(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "ipfs")
	dir := t.TempDir()
	s := openKubo(t, startKubo(t, repo), dir)
	blob := put(t, s, blobA)
	leaf, leafCID := node("leaf", blob)
	root, rootCID := node("root", leafCID)
	if _, err := s.PutNodes(ctx, root, leaf); err != nil {
		t.Fatal(err)
	}

	// Kubo itself reads the node, and follows its link to the next.
	var got struct{ Label string }
	if err := json.Unmarshal(ipfs(t, repo, nil, "dag", "get", rootCID.String()+"/links/0"), &got); err != nil || got.Label != "leaf" {
		t.Errorf("ipfs dag get of the node the root links to: %+v, %v", got, err)
	}
	if data, err := s.GetNode(ctx, rootCID); err != nil || !bytes.Equal(data, root) {
		t.Errorf("the root read back from Kubo: %x, %v", data, err)
	}
	// A pin on the root keeps both nodes through a collection.
	if err := s.Pin(ctx, "record:j", time.Time{}, rootCID); err != nil {
		t.Fatal(err)
	}
	gc(t, s, afterGrace())
	if !hasBlock(t, s, rootCID) || !hasBlock(t, s, leafCID) || hasBlock(t, s, blob) {
		t.Error("after a collection Kubo does not hold exactly the pinned root and the node it links to")
	}
}
