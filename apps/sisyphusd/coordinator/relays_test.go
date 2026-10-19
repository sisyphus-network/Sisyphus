package coordinator

import (
	"context"
	"errors"
	"testing"
	"time"

	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
)

// eventualWait bounds every wait for something that must happen. Passing tests
// never wait it out, so it is long: under the race detector and a full
// parallel suite a stop or a notification can take more than a second (#466).
// Windows in which something must not happen stay short.
const eventualWait = 30 * time.Second

func TestWatchRelaysSendsInitialListAndMembershipChanges(t *testing.T) {
	c := New(Config{ID: "coordinator"})
	defer c.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	updates := make(chan []string, 2)
	done := make(chan error, 1)
	go func() {
		done <- c.WatchRelays(ctx, func(addresses []string) error {
			updates <- addresses
			return nil
		})
	}()

	select {
	case got := <-updates:
		if len(got) != 0 {
			t.Fatalf("initial relays = %v, want none", got)
		}
	case <-time.After(eventualWait):
		t.Fatal("watcher did not send its initial snapshot")
	}

	const relayAddress = "/ip4/192.0.2.10/tcp/7700/p2p/12D3KooWexample"
	c.mu.Lock()
	c.workers["relay"] = &worker{
		id: "relay", relayAddresses: []string{relayAddress}, capabilities: &pb.NodeCapabilities{},
		connectedAt: time.Now(), lastSeen: time.Now(), running: make(map[string]assignment),
	}
	c.mu.Unlock()
	c.notifyRelaysChanged()

	select {
	case got := <-updates:
		if len(got) != 1 || got[0] != relayAddress {
			t.Fatalf("updated relays = %v, want [%s]", got, relayAddress)
		}
	case <-time.After(eventualWait):
		t.Fatal("watcher did not send a relay membership update")
	}

	c.mu.Lock()
	delete(c.workers, "relay")
	c.mu.Unlock()
	c.notifyRelaysChanged()
	select {
	case got := <-updates:
		if len(got) != 0 {
			t.Fatalf("relays after member left = %v, want none", got)
		}
	case <-time.After(eventualWait):
		t.Fatal("watcher did not send a relay removal update")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WatchRelays: %v", err)
		}
	case <-time.After(eventualWait):
		t.Fatal("watcher did not stop after cancellation")
	}
}

// A relay watcher ends with the error of the first list it cannot send,
// the first or a later one. A change that comes while another is still
// unheard is told to it once.
func TestWatchRelaysStopsAtAListItCannotSend(t *testing.T) {
	c := New(Config{ID: "coordinator"})
	defer c.Close()
	gone := errors.New("watcher went away")
	if err := c.WatchRelays(context.Background(), func([]string) error { return gone }); !errors.Is(err, gone) {
		t.Errorf("first list: %v, want %v", err, gone)
	}

	// The watcher is held in its first send while two changes come, so the
	// second finds the first still unheard.
	sending, release := make(chan struct{}), make(chan struct{})
	lists := 0
	done := make(chan error, 1)
	go func() {
		done <- c.WatchRelays(context.Background(), func([]string) error {
			if lists++; lists > 1 {
				return gone
			}
			close(sending)
			<-release
			return nil
		})
	}()
	<-sending
	c.notifyRelaysChanged()
	c.notifyRelaysChanged()
	close(release)
	if err := <-done; !errors.Is(err, gone) || lists != 2 {
		t.Errorf("later list: %v after %d lists, want %v after two", err, lists, gone)
	}
}
