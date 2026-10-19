package nodedb

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestTheJobsSubmittedToAPoolAreKeptAcrossReopening(t *testing.T) {
	db, file := newDB(t)
	start := time.Now()
	for i, job := range []struct{ coordinator, id string }{{"c", "first"}, {"other", "elsewhere"}, {"c", "second"}, {"c", "third"}, {"c", "first"}} {
		if err := db.AddPoolJob(job.coordinator, job.id, start.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	// One is forgotten, and forgetting one never recorded is no error.
	for _, id := range []string{"second", "never"} {
		if err := db.DropPoolJob(id); err != nil {
			t.Fatal(err)
		}
	}
	db = reopen(t, db, file)
	// Newest first, each once, and only those submitted to that pool.
	if got, err := db.PoolJobs("c"); err != nil || !reflect.DeepEqual(got, []string{"third", "first"}) {
		t.Errorf("jobs submitted to the pool after reopening: %v, %v", got, err)
	}
	if got, err := db.PoolJobs("nobody"); err != nil || len(got) != 0 {
		t.Errorf("jobs submitted to a pool never joined: %v, %v", got, err)
	}

	loosen(t, db, "pool_jobs", "job_id, coordinator_id, submitted_ns")
	if _, err := db.sql.Exec(`UPDATE pool_jobs SET coordinator_id = 'c'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.PoolJobs("c"); err == nil || !strings.Contains(err.Error(), "load the jobs submitted to the pool") {
		t.Errorf("with a damaged table: %v", err)
	}
	db.Close()
	if err := db.AddPoolJob("c", "late", start); err == nil || !strings.Contains(err.Error(), "save a job submitted to the pool") {
		t.Errorf("on a closed database: %v", err)
	}
	if err := db.DropPoolJob("first"); err == nil || !strings.Contains(err.Error(), "forget a job submitted to the pool") {
		t.Errorf("on a closed database: %v", err)
	}
}
