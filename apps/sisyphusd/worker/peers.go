package worker

import (
	"context"
	"log/slog"
	"net"
	"slices"
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

	mu           sync.Mutex
	known        map[string]time.Time // when each known member was last confirmed
	relays       []string
	relayChanges chan struct{}
}

// NewMembers returns a membership check that asks the pool's coordinator
// through conn.
func NewMembers(conn grpc.ClientConnInterface) *Members {
	return &Members{pool: pb.NewPoolServiceClient(conn), remember: time.Minute, known: make(map[string]time.Time), relayChanges: make(chan struct{}, 1)}
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

// Relays returns the latest relay list received from the coordinator's
// event stream. The worker keeps its explicitly configured relay separately.
func (m *Members) Relays(context.Context) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.relays...)
}

// RelayChanges is signalled when the stream publishes a different list.
// Notifications coalesce while a consumer is busy.
func (m *Members) RelayChanges() <-chan struct{} { return m.relayChanges }

// WatchRelays consumes one coordinator stream until it ends. The server sends
// an initial snapshot, followed only by snapshots after membership changes.
func (m *Members) WatchRelays(ctx context.Context) error {
	stream, err := m.pool.WatchRelays(ctx, &pb.RelaysRequest{})
	if err != nil {
		return err
	}
	for {
		listed, err := stream.Recv()
		if err != nil {
			return err
		}
		next := listed.GetAddresses()
		m.mu.Lock()
		changed := !slices.Equal(m.relays, next)
		if changed {
			m.relays = append(m.relays[:0], next...)
		}
		m.mu.Unlock()
		if changed {
			select {
			case m.relayChanges <- struct{}{}:
			default:
			}
		}
	}
}

// RunRelayWatch reconnects after a broken coordinator stream with bounded
// backoff. The last usable list remains active until a new snapshot arrives.
func (m *Members) RunRelayWatch(ctx context.Context, log *slog.Logger) {
	delay := time.Second
	for {
		err := m.WatchRelays(ctx)
		if ctx.Err() != nil {
			return
		}
		if log != nil {
			log.Warn("relay update stream ended; reconnecting", "error", err, "retry_in", delay)
		}
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return
		}
		delay *= 2
		if delay > 15*time.Second {
			delay = 15 * time.Second
		}
	}
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
	return FetchFromPeer(ctx, holder, c, b.local, b.PeerCredentials, b.PeerDialer)
}

// FetchFromPeer copies blob c from the store a member of the pool serves
// into a local one, failing if what arrives does not hash to c. The member
// is connected to with what credentials gives for it, and through dial if
// it gave no address of its own.
func FetchFromPeer(ctx context.Context, holder *pb.BlobHolder, c cid.Cid, into blobclient.Putter, credentials PeerCredentialsFunc, dial func(ctx context.Context, nodeID string) (net.Conn, error)) error {
	target := holder.GetAddress()
	options := []grpc.DialOption{grpc.WithTransportCredentials(credentials(holder.GetNodeId()))}
	if target == ViaP2P {
		// The connection is made by name rather than to an address. The
		// node at the far end is checked as on any other.
		target = "passthrough:///" + holder.GetNodeId()
		options = append(options, grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return dial(ctx, holder.GetNodeId())
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
	return blobclient.Fetch(ctx, peer, c, into)
}

// PeerCredentialsFunc returns, for a node ID, what to connect to that node
// with: this node's key, and the expectation of finding that node.
type PeerCredentialsFunc func(nodeID string) credentials.TransportCredentials
