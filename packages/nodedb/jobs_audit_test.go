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
