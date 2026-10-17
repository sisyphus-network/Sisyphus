package coordinator

import (
	"context"
	"testing"
	"time"

	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
)

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
	case <-time.After(time.Second):
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
	case <-time.After(time.Second):
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
	case <-time.After(time.Second):
		t.Fatal("watcher did not send a relay removal update")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WatchRelays: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("watcher did not stop after cancellation")
	}
}
