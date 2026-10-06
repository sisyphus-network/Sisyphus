package nodedb

import (
	"reflect"
	"strings"
	"testing"
	"time"

	jobmodel "github.com/sisyphus-network/Sisyphus/packages/job-model"
)

func TestMembersAreKeptAcrossReopening(t *testing.T) {
	db, file := newDB(t)
	joined := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	for _, m := range []Member{{"12D3KooWb", "worker", joined}, {"12D3KooWa", "client", joined.Add(time.Hour)}, {"12D3KooWc", "worker", joined}} {
		if err := db.SaveMember(m); err != nil {
			t.Fatal(err)
		}
	}
	// One changes role and one leaves. Removing a stranger is no error.
	if err := db.SaveMember(Member{"12D3KooWb", "client", joined.Add(2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"12D3KooWc", "12D3KooWnobody"} {
		if err := db.DeleteMember(id); err != nil {
			t.Fatal(err)
		}
	}

	db = reopen(t, db, file)
	got, err := db.Members()
	if err != nil {
		t.Fatal(err)
	}
	want := []Member{{"12D3KooWa", "client", joined.Add(time.Hour)}, {"12D3KooWb", "client", joined.Add(2 * time.Hour)}}
	if len(got) != 2 || got[0].ID != want[0].ID || got[1].Role != "client" || !got[0].Joined.Equal(want[0].Joined) || !got[1].Joined.Equal(want[1].Joined) {
		t.Errorf("members after reopening: %+v, want %+v", got, want)
	}
	// Only the roles there are can be recorded.
	if err := db.SaveMember(Member{"12D3KooWd", "owner", joined}); err == nil || !strings.Contains(err.Error(), "save member") {
		t.Errorf("saving a member as owner: %v", err)
	}
}

func TestAnInvitationCanBeTakenOnceAndSurvivesReopening(t *testing.T) {
	db, file := newDB(t)
	now := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	if err := db.SaveInvitation("hash-1", "worker", now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	db = reopen(t, db, file)

	role, expires, found, err := db.TakeInvitation("hash-1")
	if err != nil || !found || role != "worker" || !expires.Equal(now.Add(time.Hour)) {
		t.Fatalf("taking an invitation: %q, %v, %v, %v", role, expires, found, err)
	}
	if _, _, found, err := db.TakeInvitation("hash-1"); err != nil || found {
		t.Errorf("taking it a second time: found %v, %v", found, err)
	}
	if _, _, found, err := db.TakeInvitation("never-issued"); err != nil || found {
		t.Errorf("taking one never issued: found %v, %v", found, err)
	}
}

func TestExpiredInvitationsAreForgottenWhenANewOneIsIssued(t *testing.T) {
	db, _ := newDB(t)
	now := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	if err := db.SaveInvitation("old", "worker", now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveInvitation("current", "client", now.Add(3*time.Hour), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	// Two hours on, the first has lapsed and the next issue clears it out.
	if err := db.SaveInvitation("new", "worker", now.Add(4*time.Hour), now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	var left []string
	rows, err := db.sql.Query(`SELECT token_hash FROM invitations ORDER BY token_hash`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var hash string
		rows.Scan(&hash)
		left = append(left, hash)
	}
	if !reflect.DeepEqual(left, []string{"current", "new"}) {
		t.Errorf("invitations on record: %v, want the expired one gone", left)
	}
}

func TestDeletingJobsTakesEverythingOfTheirsAndNothingElse(t *testing.T) {
	db, _ := newDB(t)
	for _, id := range []string{"old-1", "old-2", "kept"} {
		job := jobmodel.New(id, "primes", nil, jobmodel.Distributed, 1, [][]byte{nil}, submitted)
		job.NoteRead("input-of-" + id)
		job.Start(job.Tasks[0], "node-a", "alpha")
		save(t, db, job)
	}
	if err := db.DeleteJobs([]string{"old-1", "old-2", "never-existed"}); err != nil {
		t.Fatal(err)
	}
	jobs := load(t, db)
	if len(jobs) != 1 || jobs[0].ID != "kept" || len(jobs[0].Tasks) != 1 || !reflect.DeepEqual(jobs[0].InputBlobs(), []string{"input-of-kept"}) {
		t.Errorf("jobs left: %+v", jobs)
	}
	for table, want := range map[string]int{"jobs": 1, "tasks": 1, "task_attempts": 1, "job_blobs": 1} {
		var count int
		db.sql.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count)
		if count != want {
			t.Errorf("%d rows left in %s, want %d", count, table, want)
		}
	}
}

func TestMemberCallsOnAClosedDatabaseReportThemselves(t *testing.T) {
	db, _ := newDB(t)
	loosen(t, db, "members", "node_id, role, joined_at_ns")
	if _, err := db.Members(); err == nil || !strings.Contains(err.Error(), "load members") {
		t.Errorf("Members with a damaged table: %v", err)
	}
	db.Close()
	now := time.Now()
	for what, err := range map[string]error{
		"save member":     db.SaveMember(Member{"12D3KooWa", "worker", now}),
		"delete member":   db.DeleteMember("12D3KooWa"),
		"save invitation": db.SaveInvitation("hash", "worker", now, now),
		"delete jobs":     db.DeleteJobs([]string{"j1"}),
		"take invitation": func() error { _, _, _, err := db.TakeInvitation("hash"); return err }(),
	} {
		if err == nil || !strings.Contains(err.Error(), what) {
			t.Errorf("%s on a closed database: %v", what, err)
		}
	}
}
