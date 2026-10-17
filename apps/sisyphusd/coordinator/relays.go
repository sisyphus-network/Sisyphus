package coordinator

import (
	"context"
	"sync"
)

// WatchRelays sends the current relay addresses, then sends a fresh snapshot
// after workers join or leave. Each watcher has its own coalescing channel,
// so a slow watcher cannot consume another's notification.
func (c *Coordinator) WatchRelays(ctx context.Context, send func([]string) error) error {
	changes, unsubscribe := c.subscribeRelays()
	defer unsubscribe()
	if err := send(c.Relays()); err != nil {
		return err
	}
	for {
		select {
		case <-changes:
			if err := send(c.Relays()); err != nil {
				return err
			}
		case <-c.ctx.Done():
			return nil
		case <-ctx.Done():
			return nil
		}
	}
}

func (c *Coordinator) notifyRelaysChanged() {
	c.relayMu.Lock()
	defer c.relayMu.Unlock()
	for subscriber := range c.relaySubs {
		select {
		case subscriber <- struct{}{}:
		default:
		}
	}
}

func (c *Coordinator) subscribeRelays() (<-chan struct{}, func()) {
	subscriber := make(chan struct{}, 1)
	c.relayMu.Lock()
	c.relaySubs[subscriber] = struct{}{}
	c.relayMu.Unlock()
	var once sync.Once
	return subscriber, func() {
		once.Do(func() {
			c.relayMu.Lock()
			delete(c.relaySubs, subscriber)
			c.relayMu.Unlock()
		})
	}
}
