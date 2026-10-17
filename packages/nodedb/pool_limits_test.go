package nodedb

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestPoolLimitsAreUnsetUntilSavedAndThenReplaced(t *testing.T) {
	db, _ := newDB(t)
	got, set, err := db.PoolLimits()
	if err != nil || set || got != (PoolLimits{}) {
		t.Fatalf("unset limits = %+v, set %v, err %v", got, set, err)
	}
	want := PoolLimits{
		MaxJobsPerClient: 3, MaxTasksPerClient: 40, MaxTaskTimeout: 90 * time.Minute,
		MaxStoreBytesPerClient: math.MaxInt64, MaxUploadBytesPerAttempt: 1 << 30,
	}
	if err := db.SetPoolLimits(want); err != nil {
		t.Fatal(err)
	}
	if got, set, err = db.PoolLimits(); err != nil || !set || got != want {
		t.Fatalf("saved limits = %+v, set %v, err %v; want %+v", got, set, err, want)
	}
	// Less than a second of a timeout is not kept.
	if err := db.SetPoolLimits(PoolLimits{MaxJobsPerClient: 1, MaxTaskTimeout: 2500 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	want = PoolLimits{MaxJobsPerClient: 1, MaxTaskTimeout: 2 * time.Second}
	if got, set, err = db.PoolLimits(); err != nil || !set || got != want {
		t.Fatalf("replaced limits = %+v, set %v, err %v; want %+v", got, set, err, want)
	}
}

// SQLite keeps signed integers, so limits it cannot hold are refused, and
// what was saved before is kept.
func TestPoolLimitsThatCannotBeKeptAreRefused(t *testing.T) {
	db, _ := newDB(t)
	kept := PoolLimits{MaxJobsPerClient: 2}
	if err := db.SetPoolLimits(kept); err != nil {
		t.Fatal(err)
	}
	for _, limits := range []PoolLimits{
		{MaxStoreBytesPerClient: math.MaxInt64 + 1},
		{MaxUploadBytesPerAttempt: math.MaxUint64},
		{MaxJobsPerClient: -1},
		{MaxTasksPerClient: -1},
		{MaxTaskTimeout: -time.Second},
	} {
		if err := db.SetPoolLimits(limits); err == nil || !strings.Contains(err.Error(), "save pool limits") {
			t.Errorf("saving %+v: %v", limits, err)
		}
	}
	if got, set, err := db.PoolLimits(); err != nil || !set || got != kept {
		t.Errorf("limits after refusals = %+v, set %v, err %v; want %+v", got, set, err, kept)
	}
}

func TestPoolLimitsReportMissingTable(t *testing.T) {
	db, _ := newDB(t)
	if _, err := db.sql.Exec(`DROP TABLE pool_limits`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.PoolLimits(); err == nil || !strings.Contains(err.Error(), "load pool limits") {
		t.Errorf("load from missing table: %v", err)
	}
	if err := db.SetPoolLimits(PoolLimits{}); err == nil || !strings.Contains(err.Error(), "save pool limits") {
		t.Errorf("save to missing table: %v", err)
	}
}
