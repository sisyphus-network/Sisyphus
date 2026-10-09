package nodedb

import (
	"reflect"
	"strings"
	"testing"

	jobmodel "github.com/sisyphus-network/Sisyphus/packages/job-model"
)

func TestTheStandingOfWorkersIsKeptAcrossReopening(t *testing.T) {
	db, file := newDB(t)
	if got, err := db.LoadStandings(); err != nil || len(got) != 0 {
		t.Errorf("standings in a new database: %v, %v", got, err)
	}
	for _, s := range []jobmodel.Standing{
		{NodeID: "node-b", Agreed: 3},
		{NodeID: "node-a", Outvoted: 1, Probation: 10},
		// What is saved for a worker later replaces what was saved before.
		{NodeID: "node-a", Agreed: 4, Outvoted: 1, Probation: 6},
	} {
		if err := db.SaveStanding(s); err != nil {
			t.Fatal(err)
		}
	}
	db = reopen(t, db, file)
	want := []jobmodel.Standing{{NodeID: "node-a", Agreed: 4, Outvoted: 1, Probation: 6}, {NodeID: "node-b", Agreed: 3}}
	if got, err := db.LoadStandings(); err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("standings after reopening: %v, %v, want %v", got, err, want)
	}

	// It is no part of any job, and stays when the jobs go.
	save(t, db, jobmodel.New("j1", "primes", nil, jobmodel.Distributed, 1, [][]byte{nil}, submitted))
	if err := db.DeleteJobs([]string{"j1"}); err != nil {
		t.Fatal(err)
	}
	if got, err := db.LoadStandings(); err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("standings after a job was forgotten: %v, %v", got, err)
	}

	loosen(t, db, "worker_standing", "node_id, agreed, outvoted, probation")
	if _, err := db.LoadStandings(); err == nil || !strings.Contains(err.Error(), "load workers' standing") {
		t.Errorf("with a damaged table: %v", err)
	}
	db.Close()
	if err := db.SaveStanding(want[0]); err == nil || !strings.Contains(err.Error(), "save a worker's standing") {
		t.Errorf("on a closed database: %v", err)
	}
}
