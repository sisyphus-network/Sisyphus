package coordinator

import (
	"testing"

	jobmodel "github.com/sisyphus-network/Sisyphus/packages/job-model"
)

func TestACoordinatorWithNoJournalKeepsNothingAndRecallsNothing(t *testing.T) {
	var none noJournal
	if err := none.SaveJob(nil); err != nil {
		t.Error(err)
	}
	if err := none.SaveEvent("j", jobmodel.Event{}); err != nil {
		t.Error(err)
	}
	if err := none.DeleteJobs([]string{"j"}); err != nil {
		t.Error(err)
	}
	if jobs, err := none.LoadJobs(); err != nil || len(jobs) != 0 {
		t.Errorf("LoadJobs = %v, %v", jobs, err)
	}
	if events, err := none.LoadEvents("j"); err != nil || len(events) != 0 {
		t.Errorf("LoadEvents = %v, %v", events, err)
	}
}
