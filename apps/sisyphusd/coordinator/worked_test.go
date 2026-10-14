package coordinator

import (
	"testing"

	jobmodel "github.com/sisyphus-network/Sisyphus/packages/job-model"
)

// A finished job's line in the coordinator's log says which machines did
// its tasks, those that did most first.
func TestAJobSaysWhichWorkersDidItsTasks(t *testing.T) {
	job := &jobmodel.Job{}
	for _, name := range []string{"r620", "laptop", "r620", "", "rig", "laptop", "r620", "box"} {
		job.Tasks = append(job.Tasks, &jobmodel.Task{NodeName: name})
	}
	if got, want := workedBy(job), "r620×3 laptop×2 box×1 rig×1"; got != want {
		t.Errorf("workedBy = %q, want %q", got, want)
	}
	if got := workedBy(&jobmodel.Job{}); got != "" {
		t.Errorf("a job no worker touched was worked by %q", got)
	}
}
