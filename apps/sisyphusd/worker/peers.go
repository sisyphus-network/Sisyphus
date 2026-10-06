package worker

import (
	"context"
	"net"
	"sync"
	"time"

	"github.com/ipfs/go-cid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/blobclient"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
)

// Workers of a pool fetch blobs from each other when they can, so that a
// popular input is not served to all of them by the coordinator alone. The
// coordinator says who is likely to hold a blob; each holder checks with the
// coordinator that whoever asks is a member; and whatever arrives is checked
// against its CID like any other download.

// Members answers, for a worker serving blobs, whether a node that asks is a
// member of its pool. Only the coordinator knows, so it is asked, and a yes
// is remembered for a while to spare it a question per download.
type Members struct {
	pool pb.PoolServiceClient
	// remember is how long a node found to be a member is taken to still be
	// one. It is also how long a node just removed can go on fetching.
	remember time.Duration

	mu    sync.Mutex
	known map[string]time.Time // when each known member was last confirmed
}

// NewMembers returns a membership check that asks the pool's coordinator
// through conn.
func NewMembers(conn grpc.ClientConnInterface) *Members {
	return &Members{pool: pb.NewPoolServiceClient(conn), remember: time.Minute, known: make(map[string]time.Time)}
}

// IsMember reports whether the node with the given ID belongs to the pool.
func (m *Members) IsMember(ctx context.Context, nodeID string) (bool, error) {
	m.mu.Lock()
	confirmed, ok := m.known[nodeID]
	m.mu.Unlock()
	if ok && time.Since(confirmed) < m.remember {
		return true, nil
	}
	answer, err := m.pool.IsMember(ctx, &pb.IsMemberRequest{NodeId: nodeID})
	if err != nil {
		return false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if answer.GetMember() {
		m.known[nodeID] = time.Now()
	} else {
		delete(m.known, nodeID)
	}
	return answer.GetMember(), nil
}

// peerProbe is how long a worker waits for another to answer at all before
// moving on to the next.
var peerProbe = 5 * time.Second

// fetchFromPeers tries to get blob c into the local store from another
// worker of the pool, and reports whether it did.
func (b *RemoteBlobs) fetchFromPeers(ctx context.Context, c cid.Cid) bool {
	if b.PeerCredentials == nil {
		return false
	}
	located, err := b.remote.Locate(ctx, &pb.LocateBlobRequest{Cid: c.String()})
	if err != nil {
		return false
	}
	for _, holder := range located.GetHolders() {
		if b.fetchFrom(ctx, holder, c) == nil {
			if b.OnPeerFetch != nil {
				b.OnPeerFetch(c, holder)
			}
			return true
		}
	}
	return false
}

// ViaP2P is what a worker gives as its address when it has none of its own
// to give and is to be reached by its node ID, through PeerDialer.
const ViaP2P = "p2p"

func (b *RemoteBlobs) fetchFrom(ctx context.Context, holder *pb.BlobHolder, c cid.Cid) error {
	target := holder.GetAddress()
	options := []grpc.DialOption{grpc.WithTransportCredentials(b.PeerCredentials(holder.GetNodeId()))}
	if target == ViaP2P {
		// The connection is made by name rather than to an address. The
		// node at the far end is checked as on any other.
		target = "passthrough:///" + holder.GetNodeId()
		options = append(options, grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return b.PeerDialer(ctx, holder.GetNodeId())
		}))
	}
	conn, err := grpc.NewClient(target, options...)
	if err != nil {
		return err
	}
	defer conn.Close()
	peer := pb.NewBlobServiceClient(conn)
	// A peer that is gone, or no longer has the blob, should cost seconds,
	// not the minutes a large download is allowed.
	probe, cancel := context.WithTimeout(ctx, peerProbe)
	_, err = peer.Stat(probe, &pb.StatBlobRequest{Cid: c.String()})
	cancel()
	if err != nil {
		return err
	}
	return blobclient.Fetch(ctx, peer, c, b.local)
}

// PeerCredentialsFunc returns, for a node ID, what to connect to that node
// with: this node's key, and the expectation of finding that node.
type PeerCredentialsFunc func(nodeID string) credentials.TransportCredentials
