package coordinator

import (
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/sisyphus-network/Sisyphus/packages/accounting"
	jobmodel "github.com/sisyphus-network/Sisyphus/packages/job-model"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
)

// Accounting totals what each machine did and what was done of each
// workload, over the jobs it holds that ended in a range; see
// packages/accounting. With a submitter it counts only that node's jobs.
// It holds jobs only for as long as --keep-jobs says, so a range that goes
// back further finds no more than that.
func (c *Coordinator) Accounting(r accounting.Range, submitter string) *pb.Accounting {
	c.mu.Lock()
	jobs := make([]*jobmodel.Job, 0, len(c.jobs))
	for _, job := range c.jobs {
		if submitter == "" || job.Submitter == submitter {
			jobs = append(jobs, job)
		}
	}
	summary := accounting.Summarize(accounting.Input{Jobs: jobs, Range: r, Composite: func(job *jobmodel.Job) bool {
		_, is := c.composite(job)
		return is
	}})
	c.mu.Unlock()

	out := &pb.Accounting{From: moment(r.From), To: moment(r.To), Jobs: uint32(summary.Jobs)}
	for _, m := range summary.Machines {
		out.Machines = append(out.Machines, &pb.MachineTotals{
			NodeId: m.ID, Name: m.Name, Jobs: uint32(m.Jobs), Attempts: uint32(m.Attempts), FailedAttempts: uint32(m.FailedAttempts),
			Accepted: acceptedTasks(m.Counts), Outvoted: uint32(m.Outvoted),
		})
	}
	for _, w := range summary.Workloads {
		out.Workloads = append(out.Workloads, &pb.WorkloadTotals{
			Workload: w.Name, Jobs: uint32(w.Jobs), Succeeded: uint32(w.Succeeded), Failed: uint32(w.Failed), Cancelled: uint32(w.Cancelled),
			Tasks: uint32(w.Tasks), Attempts: uint32(w.Attempts), FailedAttempts: uint32(w.FailedAttempts),
			Accepted: acceptedTasks(w.Counts), Machines: uint32(w.Machines), WallSeconds: uint64(w.WallTime / time.Second),
		})
	}
	return out
}

func acceptedTasks(c accounting.Counts) *pb.AcceptedTasks {
	return &pb.AcceptedTasks{Agreement: uint32(c.Agreement), Audit: uint32(c.Audit), Unchecked: uint32(c.Unchecked), Caught: uint32(c.Caught)}
}

// moment is a time as a message, or none for no time.
func moment(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}
