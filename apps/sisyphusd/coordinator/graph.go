package coordinator

import (
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc/status"

	jobmodel "github.com/sisyphus-network/Sisyphus/packages/job-model"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
)

// stepRunner is the name a composite job's tasks are said to be run by:
// the coordinator itself, which carries each out as a job.
const stepRunner = "coordinator"

// composite returns a job's workload as one whose tasks the coordinator
// carries out itself, if it is one.
func (c *Coordinator) composite(job *jobmodel.Job) (runtime.Composite, bool) {
	workload, _ := c.workloads.Get(job.Workload) // a job in hand has a workload that is known
	composite, is := workload.(runtime.Composite)
	return composite, is
}

// stepsLocked begins every task of a composite job that is waiting and has
// nothing left to wait for.
func (c *Coordinator) stepsLocked(job *jobmodel.Job, composite runtime.Composite) {
	for _, task := range job.Assignable() {
		ready := true
		for _, before := range composite.After(task.Payload) {
			ready = ready && job.Tasks[before].State == jobmodel.Succeeded
		}
		if !ready {
			continue
		}
		job.Start(task, c.id, stepRunner, time.Now())
		c.notifyLocked(job)
		c.aggregating.Add(1)
		go c.runStep(job, task, composite)
	}
}

// childLocked returns the ID of the job last submitted to carry out a step
// of a composite job, or nothing if none has been. A coordinator that was
// restarted finds the steps it had begun this way and does not begin them
// again.
func (c *Coordinator) childLocked(parent, step string) string {
	var found *jobmodel.Job
	for _, job := range c.jobs {
		if job.Parent == parent && job.Step == step && (found == nil || job.CreatedAt.After(found.CreatedAt)) {
			found = job
		}
	}
	if found == nil {
		return ""
	}
	return found.ID
}

// runStep carries out one task of a composite job: it submits the job the
// task describes, unless that was done before a restart, waits for it, and
// makes its result the task's output.
func (c *Coordinator) runStep(job *jobmodel.Job, task *jobmodel.Task, composite runtime.Composite) {
	defer c.aggregating.Done()

	c.mu.Lock()
	child, err := composite.Child(task.Payload, job.Outputs())
	id, key := c.childLocked(job.ID, child.Name), job.Key
	c.mu.Unlock()

	if err == nil && id == "" {
		var submitted *pb.Job
		submitted, err = c.submit(c.ctx, &pb.JobSpec{
			Workload: child.Workload, Params: child.Params, MaxTasks: uint32(child.Tasks), Key: key,
			TaskTimeoutSeconds: uint32(child.Timeout / time.Second),
		}, job.ID, child.Name)
		id = submitted.GetJobId()
	}
	var ended *pb.Job
	if err == nil {
		c.mu.Lock()
		c.recordLocked(job, eventTaskStarted, task.Index, stepRunner, fmt.Sprintf("step %s is job %s", child.Name, id))
		c.wakeLocked(job)
		c.mu.Unlock()
		err = c.Watch(c.ctx, id, func(now *pb.Job) error { ended = now; return nil })
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ctx.Err() != nil || job.Terminal() {
		// The coordinator is stopping, and the step is taken up again
		// after a restart; or the job was stopped while the step ran.
		return
	}
	failed := ""
	switch {
	case err != nil:
		failed = fmt.Sprintf("step %s: %s", child.Name, status.Convert(err).Message())
	case ended.GetState() != pb.JobState_JOB_STATE_SUCCEEDED:
		state := strings.ToLower(strings.TrimPrefix(ended.GetState().String(), "JOB_STATE_"))
		failed = strings.TrimSpace(fmt.Sprintf("step %s (job %s) %s: %s", child.Name, id, state, ended.GetError()))
	}
	if failed != "" {
		c.recordLocked(job, eventTaskFailed, task.Index, stepRunner, failed)
		// A step is a whole job, which has had its own attempts.
		job.Fail(task, failed, 1, time.Now())
	} else {
		c.recordLocked(job, eventTaskSucceeded, task.Index, stepRunner, fmt.Sprintf("step %s", child.Name))
		if job.Succeed(task, composite.Output(task.Payload, id, ended.GetResult(), ended.GetOutputBlobs())) {
			c.aggregating.Add(1)
			go c.aggregate(job)
		}
	}
	c.settleLocked(job)
	c.notifyLocked(job)
	c.scheduleLocked()
}

// stopStepsLocked stops the jobs carrying out the steps of a job that has
// ended without them.
func (c *Coordinator) stopStepsLocked(job *jobmodel.Job) {
	for _, child := range c.jobs {
		if child.Parent == job.ID && child.Cancel(time.Now()) {
			c.settleLocked(child)
			c.log.Info("job cancelled with the job it was a step of", "job", child.ID, "parent", job.ID)
			c.notifyLocked(child)
		}
	}
}
