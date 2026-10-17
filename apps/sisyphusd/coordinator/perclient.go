package coordinator

import (
	"context"
	"time"
)

// Stored says how many bytes each client keeps in this node's store: what
// the pins of its jobs keep, and what pins of its own keep, own naming the
// client an owner other than a job stands for, or nothing for one that
// stands for none. A blob a client keeps several ways counts once; one
// that several clients keep counts in full for each, whatever it costs on
// disk. The node's own jobs, which have no submitter, count for nobody.
func (c *Coordinator) Stored(ctx context.Context, own func(owner string) string) (map[string]uint64, error) {
	// The submitters are read first: the store calls back with its pins
	// locked, and the coordinator pins with its own lock held.
	c.mu.Lock()
	submitters := make(map[string]string)
	for _, job := range c.jobs {
		if job.Submitter != "" {
			submitters[owner(job)] = job.Submitter
		}
	}
	c.mu.Unlock()
	return c.store.Usage(ctx, time.Now(), func(pinOwner string) string {
		if submitter, of := submitters[pinOwner]; of {
			return submitter
		}
		return own(pinOwner)
	})
}
