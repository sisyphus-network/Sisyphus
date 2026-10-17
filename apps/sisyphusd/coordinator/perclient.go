package coordinator

import (
	"time"

	jobmodel "github.com/sisyphus-network/Sisyphus/packages/job-model"
)

// Stored says how many bytes each client keeps in this node's store: what
// the pins of its jobs keep, and what pins of its own keep, as ClientOf
// names them. A blob a client keeps several ways counts once; one that
// several clients keep counts in full for each, whatever it costs on disk.
// The node's own jobs, which have no submitter, count for nobody. The
// store keeps count from the first time it is asked, so it is not walked
// again.
func (c *Coordinator) Stored() map[string]uint64 {
	c.grouping.Do(func() {
		if err := c.store.GroupPins(c.ctx, c.groupOf); err != nil {
			c.log.Warn("some pins cannot be counted towards what their clients keep", "error", err)
		}
	})
	return c.store.Usage(time.Now())
}

// groupOf names the client a pin owner holds pins for: a client job's
// submitter, or the client ClientOf names.
func (c *Coordinator) groupOf(pinOwner string) string {
	if submitter, of := c.submitters.Load(pinOwner); of {
		return submitter.(string)
	}
	if c.clientOf == nil {
		return ""
	}
	return c.clientOf(pinOwner)
}

// noteSubmitter has a client job's pins counted towards its submitter.
func (c *Coordinator) noteSubmitter(job *jobmodel.Job) {
	if job.Submitter != "" {
		c.submitters.Store(owner(job), job.Submitter)
	}
}
