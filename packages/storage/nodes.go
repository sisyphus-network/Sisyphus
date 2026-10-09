package storage

import (
	"bytes"
	"context"
	"fmt"
	"time"

	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	ipld "github.com/ipfs/go-ipld-format"
	"github.com/ipld/go-ipld-prime/codec/dagcbor"
	cidlink "github.com/ipld/go-ipld-prime/linking/cid"
	"github.com/ipld/go-ipld-prime/node/basicnode"
	"github.com/ipld/go-ipld-prime/traversal"
	"github.com/multiformats/go-multihash"
)

// Beside blobs, a store holds nodes of linked data: small structured
// records, encoded as DAG-CBOR, that name each other and blobs by CID. A
// job's record is made of them.
//
// A pin on a node keeps it and every node it links to, directly or through
// other nodes. It does not keep the blobs they link to: a record outlives
// the data it describes, and names it still.

var nodeBuilder = cid.V1Builder{Codec: cid.DagCBOR, MhType: multihash.SHA2_256}

// PutNodes stores a node and the nodes it links to, each given in its
// DAG-CBOR encoding, and returns the CID of the first. The same bytes always
// give the same CID. The node is kept for GracePeriod, and through it the
// others; pin it to keep them longer.
func (s *Store) PutNodes(ctx context.Context, root []byte, linked ...[]byte) (cid.Cid, error) {
	s.gcMu.RLock()
	defer s.gcMu.RUnlock()
	// The root goes in last, so that a root that is present means the
	// nodes it links to are.
	var name cid.Cid
	for i := len(linked); i >= 0; i-- {
		data := root
		if i > 0 {
			data = linked[i-1]
		}
		if _, err := nodeLinks(data); err != nil {
			return cid.Undef, fmt.Errorf("store a node: %w", err)
		}
		name, _ = nodeBuilder.Sum(data) // hashing bytes in memory cannot fail
		block, _ := blocks.NewBlockWithCid(data, name)
		if err := s.blocks.Put(ctx, block); err != nil {
			return cid.Undef, err
		}
	}
	s.pinMu.Lock()
	s.pins[pinKey{name, graceOwner}] = time.Now().Add(GracePeriod)
	s.pinMu.Unlock()
	return name, nil
}

// GetNode returns the DAG-CBOR encoding of the node with the given CID,
// checked against that CID, or ErrNotFound.
func (s *Store) GetNode(ctx context.Context, c cid.Cid) ([]byte, error) {
	if c.Type() != cid.DagCBOR {
		return nil, fmt.Errorf("%s is not a node of linked data", c)
	}
	block, err := s.blocks.Get(ctx, c)
	if err != nil {
		if ipld.IsNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if sum, _ := c.Prefix().Sum(block.RawData()); !sum.Equals(c) {
		return nil, fmt.Errorf("block %s is corrupt", c)
	}
	return block.RawData(), nil
}

// nodeLinks returns every CID a node links to, in the order they appear in
// its encoding.
func nodeLinks(data []byte) ([]cid.Cid, error) {
	builder := basicnode.Prototype.Any.NewBuilder()
	if err := dagcbor.Decode(builder, bytes.NewReader(data)); err != nil {
		return nil, fmt.Errorf("not DAG-CBOR: %w", err)
	}
	found, _ := traversal.SelectLinks(builder.Build()) // a decoded node always lists its links
	links := make([]cid.Cid, 0, len(found))
	for _, link := range found {
		links = append(links, link.(cidlink.Link).Cid) // the decoder makes no other kind
	}
	return links, nil
}

// nodesLinked returns the nodes that node c links to, for a collection to
// keep along with it. The blobs it links to are left out.
func (s *Store) nodesLinked(ctx context.Context, c cid.Cid) ([]*ipld.Link, error) {
	data, err := s.GetNode(ctx, c)
	if err != nil {
		return nil, err
	}
	all, err := nodeLinks(data)
	if err != nil {
		return nil, fmt.Errorf("block %s: %w", c, err)
	}
	var nodes []*ipld.Link
	for _, linked := range all {
		if linked.Type() == cid.DagCBOR {
			nodes = append(nodes, &ipld.Link{Cid: linked})
		}
	}
	return nodes, nil
}
