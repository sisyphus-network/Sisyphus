package nodedb

import (
	"testing"

	jobmodel "github.com/sisyphus-network/Sisyphus/packages/job-model"
)

// What an audit found is kept with its task across a restart.
func TestATasksAuditOutcomeIsKept(t *testing.T) {
	db, file := newDB(t)
	job := jobmodel.New("audited", "primes", nil, jobmodel.Distributed, 2, [][]byte{nil, nil}, submitted)
	job.AuditShare = 1
	save(t, db, job)
	job.NoteAudit(job.Tasks[1], jobmodel.AuditCaught)
	save(t, db, job)
	jobs := load(t, reopen(t, db, file))
	if got := jobs[0].Tasks; got[0].Audit != "" || got[1].Audit != jobmodel.AuditCaught {
		t.Errorf("audits after a restart: %q, %q", got[0].Audit, got[1].Audit)
	}
}

// The tier that settled a task, and the processor each of its results was
// returned from, are kept across a restart, so that the task's results are
// compared in the same tiers again.
func TestATasksTierAndItsResultsProcessorsAreKept(t *testing.T) {
	db, file := newDB(t)
	job := jobmodel.New("tiered", "qchem", nil, jobmodel.Distributed, 1, [][]byte{nil}, submitted)
	job.Check(2, 0, []int{0})
	save(t, db, job)
	task := job.Tasks[0]
	job.ReturnCopyOn(task, job.StartCopy(task, "node-a", "alpha", submitted), "Xeon E5-2670", []byte("1"), nil)
	job.ReturnCopyOn(task, job.StartCopy(task, "node-b", "beta", submitted), "", []byte("1"), nil)
	job.NoteTier(task, "cross-cpu")
	save(t, db, job)
	got := load(t, reopen(t, db, file))[0].Tasks[0]
	if got.Tier != "cross-cpu" || len(got.Results) != 2 || got.Results[0].CPUModel != "Xeon E5-2670" || got.Results[1].CPUModel != "" {
		t.Errorf("after a restart: tier %q, results %+v", got.Tier, got.Results)
	}
}
