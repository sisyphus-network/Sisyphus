package nodedb

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	jobmodel "github.com/sisyphus-network/Sisyphus/packages/job-model"
)

func open(t *testing.T, file string) *DB {
	t.Helper()
	db, err := Open(file)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func newDB(t *testing.T) (*DB, string) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "node.db")
	return open(t, file), file
}

// reopen closes a database and opens its file again, as a restarted node
// would.
func reopen(t *testing.T, db *DB, file string) *DB {
	t.Helper()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return open(t, file)
}

var submitted = time.Date(2026, 10, 6, 12, 0, 0, 123456789, time.UTC)

func save(t *testing.T, db *DB, job *jobmodel.Job) {
	t.Helper()
	if err := db.SaveJob(job); err != nil {
		t.Fatal(err)
	}
}

func load(t *testing.T, db *DB) []*jobmodel.Job {
	t.Helper()
	jobs, err := db.LoadJobs()
	if err != nil {
		t.Fatal(err)
	}
	return jobs
}

// same reports whether two jobs are the same in everything a coordinator
// goes by.
func same(t *testing.T, got, want *jobmodel.Job) {
	t.Helper()
	if !reflect.DeepEqual(got.ToProto(), want.ToProto()) {
		t.Errorf("job as loaded:\n%v\nas saved:\n%v", got.ToProto(), want.ToProto())
	}
	if !reflect.DeepEqual(got.Outputs(), want.Outputs()) || !reflect.DeepEqual(got.IntermediateBlobs(), want.IntermediateBlobs()) {
		t.Errorf("loaded outputs %q and intermediate blobs %v, saved %q and %v", got.Outputs(), got.IntermediateBlobs(), want.Outputs(), want.IntermediateBlobs())
	}
	if string(got.Key) != string(want.Key) {
		t.Errorf("loaded key %x, saved %x", got.Key, want.Key)
	}
	for i, task := range want.Tasks {
		if string(got.Tasks[i].Payload) != string(task.Payload) || got.Tasks[i].Failures != task.Failures {
			t.Errorf("task %d: loaded payload %q and %d failures, saved %q and %d", i, got.Tasks[i].Payload, got.Tasks[i].Failures, task.Payload, task.Failures)
		}
	}
}

func TestAJobIsLoadedAsItWasSavedAtEveryStep(t *testing.T) {
	db, file := newDB(t)
	job := jobmodel.New("j1", "wordcount", []byte(`{"input":"x"}`), jobmodel.Distributed, 2, [][]byte{[]byte("first"), []byte("second")}, submitted)
	job.NoteRead("input-cid")

	check := func(step string) {
		t.Helper()
		save(t, db, job)
		db = reopen(t, db, file)
		loaded := load(t, db)
		if len(loaded) != 1 {
			t.Fatalf("%s: %d jobs loaded, want 1", step, len(loaded))
		}
		same(t, loaded[0], job)
		if unsaved := loaded[0].Unsaved(); unsaved.New || len(unsaved.Tasks) != 0 || len(unsaved.Read) != 0 {
			t.Errorf("%s: a job just loaded has unsaved changes: %+v", step, unsaved)
		}
	}
	check("submitted")

	job.Start(job.Tasks[0], "node-a", "alpha", time.Now())
	check("first task started")
	job.Start(job.Tasks[1], "node-b", "beta", time.Now())
	job.Fail(job.Tasks[1], "out of memory", 3, submitted)
	check("second task failed once")
	job.Start(job.Tasks[1], "node-a", "alpha", time.Now())
	job.NoteRead("input-cid", "shard-cid")
	job.NoteTaskOutput("counts-0")
	job.Succeed(job.Tasks[0], []byte("counts-0"))
	check("first task done")
	job.NoteTaskOutput("counts-1")
	job.Succeed(job.Tasks[1], []byte("counts-1"))
	check("every task done")
	job.NoteRead("counts-0", "counts-1")
	job.NoteResult("result-cid")
	job.Finish([]byte("result-cid"), nil, submitted.Add(time.Minute))
	check("finished")

	loaded := load(t, db)[0]
	if got := loaded.InputBlobs(); !reflect.DeepEqual(got, []string{"input-cid", "shard-cid"}) {
		t.Errorf("inputs of the loaded job: %v", got)
	}
	if !loaded.CreatedAt.Equal(submitted) || !loaded.FinishedAt.Equal(submitted.Add(time.Minute)) {
		t.Errorf("loaded times %v and %v", loaded.CreatedAt, loaded.FinishedAt)
	}
}

func TestJobsLoadInTheOrderTheyWereSubmitted(t *testing.T) {
	db, _ := newDB(t)
	// IDs that sort the other way, saved again later in another order.
	var jobs []*jobmodel.Job
	for _, id := range []string{"zz", "mm", "aa"} {
		job := jobmodel.New(id, "primes", nil, jobmodel.FullWorker, 0, [][]byte{nil}, submitted)
		jobs = append(jobs, job)
		save(t, db, job)
	}
	jobs[2].Start(jobs[2].Tasks[0], "node-a", "alpha", time.Now())
	save(t, db, jobs[2])
	jobs[0].Finish(nil, os.ErrDeadlineExceeded, submitted)
	save(t, db, jobs[0])

	loaded := load(t, db)
	var order []string
	for _, job := range loaded {
		order = append(order, job.ID)
	}
	if !reflect.DeepEqual(order, []string{"zz", "mm", "aa"}) {
		t.Fatalf("loaded in order %v", order)
	}
	same(t, loaded[0], jobs[0])
	if loaded[0].State != jobmodel.Failed || loaded[0].Mode != jobmodel.FullWorker || loaded[1].Tasks[0].ID != "mm/0" {
		t.Errorf("loaded %+v and task %+v", loaded[0], loaded[1].Tasks[0])
	}
	// A job with no finish time has none when loaded, rather than 1970.
	if !loaded[1].FinishedAt.IsZero() {
		t.Errorf("an unfinished job was loaded with finish time %v", loaded[1].FinishedAt)
	}
}

func TestAPrivateJobsKeyIsKeptOnlyUntilItFinishes(t *testing.T) {
	db, file := newDB(t)
	key := []byte("0123456789abcdef0123456789abcdef")
	job := jobmodel.New("private", "wordcount", nil, jobmodel.Distributed, 1, [][]byte{[]byte("p")}, submitted)
	job.Key = key
	save(t, db, job)
	job.Start(job.Tasks[0], "node-a", "alpha", time.Now())
	save(t, db, job)
	db = reopen(t, db, file)
	if loaded := load(t, db)[0]; string(loaded.Key) != string(key) {
		t.Fatalf("an unfinished job was loaded with key %x", loaded.Key)
	}

	job.Succeed(job.Tasks[0], []byte("out"))
	job.Finish([]byte("result"), nil, submitted)
	save(t, db, job)
	if loaded := load(t, db)[0]; loaded.Key != nil {
		t.Errorf("a finished job was loaded with key %x", loaded.Key)
	}
	// Nor is it left lying in the database's files.
	db.Close()
	for _, name := range []string{file, file + "-wal"} {
		if content, _ := os.ReadFile(name); strings.Contains(string(content), string(key)) {
			t.Errorf("%s still holds the key of a finished job", filepath.Base(name))
		}
	}
}

func TestEveryAttemptAtATaskIsRecorded(t *testing.T) {
	db, _ := newDB(t)
	job := jobmodel.New("j1", "primes", nil, jobmodel.Distributed, 2, [][]byte{nil, nil}, submitted)
	steps := []func(){
		func() {},
		func() { job.Start(job.Tasks[0], "node-a", "alpha", time.Now()) },
		func() { job.Fail(job.Tasks[0], "disk full", 3, submitted) },
		func() { job.Start(job.Tasks[0], "node-b", "beta", time.Now()) },
		func() { job.Requeue(job.Tasks[0], "coordinator restarted") },
		func() { job.Start(job.Tasks[0], "node-b", "beta", time.Now()) },
		func() { job.Succeed(job.Tasks[0], []byte("done")) },
		func() { job.Start(job.Tasks[1], "node-a", "alpha", time.Now()) },
	}
	for _, step := range steps {
		step()
		save(t, db, job)
	}
	got, err := db.Attempts("j1")
	if err != nil {
		t.Fatal(err)
	}
	want := []Attempt{
		{TaskIndex: 0, Number: 1, NodeID: "node-a", NodeName: "alpha", State: jobmodel.Pending, Err: "disk full"},
		{TaskIndex: 0, Number: 2, NodeID: "node-b", NodeName: "beta", State: jobmodel.Pending, Err: "coordinator restarted"},
		{TaskIndex: 0, Number: 3, NodeID: "node-b", NodeName: "beta", State: jobmodel.Succeeded},
		{TaskIndex: 1, Number: 1, NodeID: "node-a", NodeName: "alpha", State: jobmodel.Running},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("attempts:\n%+v\nwant:\n%+v", got, want)
	}
	if none, err := db.Attempts("no-such-job"); err != nil || len(none) != 0 {
		t.Errorf("attempts of an unknown job: %v, %v", none, err)
	}
}

func TestOnlyWhatChangedIsWritten(t *testing.T) {
	db, _ := newDB(t)
	payloads := make([][]byte, 500)
	job := jobmodel.New("big", "primes", nil, jobmodel.Distributed, 500, payloads, submitted)
	save(t, db, job)

	// Someone else's edit to a task that has not changed since stays.
	if _, err := db.sql.Exec(`UPDATE tasks SET node_name = 'untouched' WHERE job_id = 'big' AND task_index = 7`); err != nil {
		t.Fatal(err)
	}
	job.Start(job.Tasks[3], "node-a", "alpha", time.Now())
	save(t, db, job)
	loaded := load(t, db)[0]
	if loaded.Tasks[7].NodeName != "untouched" {
		t.Error("saving one task's change rewrote another task")
	}
	if loaded.Tasks[3].State != jobmodel.Running || len(loaded.Tasks) != 500 {
		t.Errorf("task 3 is %v among %d tasks", loaded.Tasks[3].State, len(loaded.Tasks))
	}
}

func TestTheDatabaseIsItsOwnersAlone(t *testing.T) {
	db, file := newDB(t)
	save(t, db, jobmodel.New("j1", "primes", nil, jobmodel.Distributed, 1, [][]byte{nil}, submitted))
	matches, _ := filepath.Glob(file + "*")
	if len(matches) < 2 {
		t.Fatalf("database files: %v, want the database and its write-ahead log", matches)
	}
	for _, name := range matches {
		if stat, _ := os.Stat(name); stat.Mode().Perm() != 0o600 {
			t.Errorf("%s has mode %v, want 0600", filepath.Base(name), stat.Mode().Perm())
		}
	}
}

func TestOpeningAppliesEachMigrationOnce(t *testing.T) {
	db, file := newDB(t)
	applied := func() (versions []int) {
		t.Helper()
		rows, err := db.sql.Query(`SELECT version FROM schema_migrations ORDER BY version`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var v int
			rows.Scan(&v)
			versions = append(versions, v)
		}
		return versions
	}
	first := applied()
	if len(first) == 0 || first[0] != 1 {
		t.Fatalf("migrations applied to a new database: %v", first)
	}
	save(t, db, jobmodel.New("kept", "primes", nil, jobmodel.Distributed, 1, [][]byte{nil}, submitted))
	db = reopen(t, db, file)
	if again := applied(); !reflect.DeepEqual(again, first) {
		t.Errorf("migrations after reopening: %v, want %v", again, first)
	}
	if jobs := load(t, db); len(jobs) != 1 || jobs[0].ID != "kept" {
		t.Errorf("jobs after reopening: %v", jobs)
	}
}

func TestOpenRefusesWhatItCannotUse(t *testing.T) {
	dir := t.TempDir()

	if _, err := Open(filepath.Join(dir, "no-such-directory", "node.db")); err == nil || !strings.Contains(err.Error(), "open node database") {
		t.Errorf("a database in a missing directory: %v", err)
	}

	junk := filepath.Join(dir, "junk.db")
	if err := os.WriteFile(junk, []byte(strings.Repeat("this is not a database ", 100)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(junk); err == nil || !strings.Contains(err.Error(), "junk.db") {
		t.Errorf("a file that is not a database: %v", err)
	}

	// A database from a later version may hold what this one would misread.
	newer := filepath.Join(dir, "newer.db")
	db := open(t, newer)
	if _, err := db.sql.Exec(`INSERT INTO schema_migrations (version, name) VALUES (9999, '9999_from_the_future')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := Open(newer); err == nil || !strings.Contains(err.Error(), "newer sisyphusd") {
		t.Errorf("a database from a newer version: %v", err)
	}

	// One that claims no migrations but already has their tables.
	clash := filepath.Join(dir, "clash.db")
	db = open(t, clash)
	if _, err := db.sql.Exec(`DELETE FROM schema_migrations`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := Open(clash); err == nil || !strings.Contains(err.Error(), "migration 0001_jobs.sql") {
		t.Errorf("a database whose tables are already there: %v", err)
	}
}

func TestAClosedDatabaseReportsItself(t *testing.T) {
	db, _ := newDB(t)
	db.Close()
	if err := db.SaveJob(jobmodel.New("j1", "primes", nil, jobmodel.Distributed, 1, [][]byte{nil}, submitted)); err == nil || !strings.Contains(err.Error(), "save job j1") {
		t.Errorf("SaveJob: %v", err)
	}
	if _, err := db.LoadJobs(); err == nil || !strings.Contains(err.Error(), "load jobs") {
		t.Errorf("LoadJobs: %v", err)
	}
	if _, err := db.Attempts("j1"); err == nil || !strings.Contains(err.Error(), "load attempts") {
		t.Errorf("Attempts: %v", err)
	}
}

func TestAJobThatCannotBeSavedKeepsItsChangesForNextTime(t *testing.T) {
	db, _ := newDB(t)
	job := jobmodel.New("j1", "primes", nil, jobmodel.Distributed, 1, [][]byte{nil}, submitted)
	// A blob role the schema does not allow cannot arise, so make saving
	// fail by taking a table away.
	if _, err := db.sql.Exec(`ALTER TABLE job_blobs RENAME TO elsewhere`); err != nil {
		t.Fatal(err)
	}
	job.NoteRead("input-cid")
	if err := db.SaveJob(job); err == nil {
		t.Fatal("saving with a table missing succeeded")
	}
	if jobs := load2(db); len(jobs) != 0 {
		t.Errorf("a failed save left %d jobs behind", len(jobs))
	}
	if unsaved := job.Unsaved(); !unsaved.New || len(unsaved.Tasks) != 1 || len(unsaved.Read) != 1 {
		t.Errorf("after a failed save the job's unsaved changes are %+v", unsaved)
	}
	if _, err := db.sql.Exec(`ALTER TABLE elsewhere RENAME TO job_blobs`); err != nil {
		t.Fatal(err)
	}
	save(t, db, job)
	if jobs := load(t, db); len(jobs) != 1 || !reflect.DeepEqual(jobs[0].InputBlobs(), []string{"input-cid"}) {
		t.Errorf("after the retry: %v", jobs)
	}
}

// load2 counts jobs without going through the tables a test has moved.
func load2(db *DB) (ids []string) {
	rows, _ := db.sql.Query(`SELECT job_id FROM jobs`)
	defer rows.Close()
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	return ids
}

// loosen replaces a table with one that takes anything, and puts a row of
// NULLs in it: what a database damaged or edited by hand might hold.
func loosen(t *testing.T, db *DB, table, columns string) {
	t.Helper()
	for _, statement := range []string{
		`PRAGMA foreign_keys = OFF`,
		`DROP TABLE ` + table,
		`CREATE TABLE ` + table + ` (` + columns + `)`,
		`INSERT INTO ` + table + ` DEFAULT VALUES`,
	} {
		if _, err := db.sql.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

func TestDamagedRowsAreReportedNotGuessedAt(t *testing.T) {
	const (
		jobColumns  = "seq, job_id, workload, params, mode, max_tasks, state, result, error, sealing_key, created_at_ns, finished_at_ns, task_timeout_ns, min_memory_bytes, min_gpus, parent_job_id, step, submitter_id, record_cid, private, verify"
		taskColumns = "job_id DEFAULT 'j1', task_index, payload, state, attempt, failures, node_id, node_name, output, error"
	)
	for _, tt := range []struct {
		table, columns, want string
	}{
		{"jobs", jobColumns, "load jobs"},
		{"tasks", taskColumns, "load tasks"},
		{"job_blobs", "job_id, role, cid", "load job blobs"},
		{"job_commitments", "job_id DEFAULT 'j1', name, commitment", "load job commitments"},
		// A row for the one task there is, with nothing else in it.
		{"task_attempts", "job_id DEFAULT 'j1', task_index DEFAULT 0, attempt, node_id, node_name, state, error", "load attempts"},
		{"task_results", "job_id DEFAULT 'j1', task_index DEFAULT 0, attempt, node_id, node_name, output, blobs", "load task results"},
	} {
		db, _ := newDB(t)
		save(t, db, jobmodel.New("j1", "primes", nil, jobmodel.Distributed, 1, [][]byte{nil}, submitted))
		loosen(t, db, tt.table, tt.columns)
		if _, err := db.LoadJobs(); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("with a damaged %s table: %v, want an error containing %q", tt.table, err, tt.want)
		}
	}

	db, _ := newDB(t)
	loosen(t, db, "task_attempts", "job_id DEFAULT 'j1', task_index, attempt, node_id, node_name, state, error")
	if _, err := db.Attempts("j1"); err == nil || !strings.Contains(err.Error(), "load attempts") {
		t.Errorf("with a damaged attempts table: %v", err)
	}
}

func TestWriteReportsACommitThatFails(t *testing.T) {
	db, _ := newDB(t)
	err := db.write(func(b *batch) {
		b.exec(`CREATE TABLE parents (id INTEGER PRIMARY KEY)`)
		b.exec(`CREATE TABLE children (parent INTEGER REFERENCES parents (id) DEFERRABLE INITIALLY DEFERRED)`)
		// Allowed until the transaction ends, and then it is not.
		b.exec(`INSERT INTO children VALUES (1)`)
	})
	if err == nil || !strings.Contains(err.Error(), "FOREIGN KEY") {
		t.Errorf("error %v, want the commit refused", err)
	}
}

func TestAJobsEventsAndItsLimitAreKept(t *testing.T) {
	db, file := newDB(t)
	job := jobmodel.New("j1", "primes", nil, jobmodel.Distributed, 1, [][]byte{nil}, submitted)
	job.TaskTimeout, job.MinMemory, job.MinGPUs = 90*time.Second, 8<<30, 2
	save(t, db, job)
	events := []jobmodel.Event{
		{Seq: 1, At: submitted, Kind: "submitted", Task: -1, Text: "primes, in 1 tasks"},
		{Seq: 2, At: submitted.Add(time.Second), Kind: "log", Task: 0, Node: "rig", Text: "counting"},
	}
	for _, e := range events {
		if err := db.SaveEvent("j1", e); err != nil {
			t.Fatal(err)
		}
	}
	db = reopen(t, db, file)
	got, err := db.LoadEvents("j1")
	if err != nil || len(got) != 2 || got[0].Kind != "submitted" || got[0].Task != -1 || !got[1].At.Equal(events[1].At) || got[1].Node != "rig" || got[1].Text != "counting" {
		t.Errorf("events after reopening: %+v, %v", got, err)
	}
	if loaded := load(t, db)[0]; loaded.TaskTimeout != 90*time.Second || loaded.MinMemory != 8<<30 || loaded.MinGPUs != 2 {
		t.Errorf("after reopening the job's limit is %v and it asks for %d bytes and %d cards", loaded.TaskTimeout, loaded.MinMemory, loaded.MinGPUs)
	}
	if none, err := db.LoadEvents("no-such-job"); err != nil || len(none) != 0 {
		t.Errorf("the events of a job that is not there: %v, %v", none, err)
	}
	// They go when the job does, and an event of no job is not kept.
	if err := db.DeleteJobs([]string{"j1"}); err != nil {
		t.Fatal(err)
	}
	if left, _ := db.LoadEvents("j1"); len(left) != 0 {
		t.Errorf("events left after their job was deleted: %+v", left)
	}
	if err := db.SaveEvent("j1", events[0]); err == nil || !strings.Contains(err.Error(), "save job event") {
		t.Errorf("saving an event of a job that is gone: %v", err)
	}
	loosen(t, db, "job_events", "job_id DEFAULT 'j1', seq, at_ns, kind, task_index, node_name, text")
	if _, err := db.LoadEvents("j1"); err == nil || !strings.Contains(err.Error(), "load job events") {
		t.Errorf("with a damaged events table: %v", err)
	}
}

func TestAJobsRecordSubmitterPrivacyAndAttemptsAreKept(t *testing.T) {
	db, file := newDB(t)
	job := jobmodel.New("j1", "primes", nil, jobmodel.Distributed, 1, [][]byte{nil}, submitted)
	job.Key, job.Private, job.Submitter = []byte("0123456789abcdef0123456789abcdef"), true, "12D3KooWsubmitter"
	save(t, db, job)
	job.Start(job.Tasks[0], "node-a", "alpha", time.Now())
	job.Fail(job.Tasks[0], "disk full", 3, submitted)
	// Not saved in between: the attempt that failed is written with the next.
	job.Start(job.Tasks[0], "node-b", "beta", time.Now())
	job.Succeed(job.Tasks[0], []byte("done"))
	job.Finish([]byte("result"), nil, submitted.Add(time.Second))
	job.Record = "bafyreib2rxk3rybk3aobmv5cjuql3bm2twh4jo5uxgf5kpqcsgz7soitae"
	save(t, db, job)

	loaded := load(t, reopen(t, db, file))[0]
	// The key went when the job ended; that it was private did not.
	if loaded.Key != nil || !loaded.Private || loaded.Submitter != job.Submitter || loaded.Record != job.Record {
		t.Errorf("loaded with key %x, private %v, submitter %q and record %q", loaded.Key, loaded.Private, loaded.Submitter, loaded.Record)
	}
	if !reflect.DeepEqual(loaded.Tasks[0].History, job.Tasks[0].History) || len(loaded.Tasks[0].History) != 2 {
		t.Errorf("attempts as loaded:\n%+v\nas saved:\n%+v", loaded.Tasks[0].History, job.Tasks[0].History)
	}
}

func TestAnUnfinishedPrivateJobFromBeforePrivacyWasKeptIsKnownForPrivate(t *testing.T) {
	db, file := newDB(t)
	sealedJob := jobmodel.New("sealed", "primes", nil, jobmodel.Distributed, 1, [][]byte{nil}, submitted)
	sealedJob.Key = []byte("0123456789abcdef0123456789abcdef")
	save(t, db, sealedJob)
	save(t, db, jobmodel.New("open", "primes", nil, jobmodel.Distributed, 1, [][]byte{nil}, submitted))
	// Put the database back as it was before the migration that added the
	// column, and so before those that came after it, and open it again.
	for _, statement := range []string{
		`DROP TABLE task_results`,
		`ALTER TABLE jobs DROP COLUMN verify`,
		`DROP TABLE job_commitments`,
		`ALTER TABLE jobs DROP COLUMN record_cid`,
		`ALTER TABLE jobs DROP COLUMN submitter_id`,
		`ALTER TABLE jobs DROP COLUMN private`,
		`DELETE FROM schema_migrations WHERE version >= 13`,
	} {
		if _, err := db.sql.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	jobs := load(t, reopen(t, db, file))
	if !jobs[0].Private || jobs[1].Private {
		t.Errorf("after the migration the job with a key is private: %v, and the one without: %v", jobs[0].Private, jobs[1].Private)
	}
}

func TestAJobsCommitmentsAreKeptWithItAndGoWhenItDoes(t *testing.T) {
	db, file := newDB(t)
	job := jobmodel.New("j1", "primes", nil, jobmodel.Distributed, 1, [][]byte{nil}, submitted)
	job.Key, job.Private = []byte("0123456789abcdef0123456789abcdef"), true
	save(t, db, job)
	save(t, db, jobmodel.New("open", "primes", nil, jobmodel.Distributed, 1, [][]byte{nil}, submitted))
	job.Finish([]byte("result"), nil, submitted.Add(time.Second))
	job.Commitments = map[string][]byte{
		"manifest/params_commitment": []byte("a keyed hash of the parameters"),
		"result/result_commitment":   []byte("a keyed hash of the result"),
	}
	save(t, db, job)
	// Saved again, as a finished job is when it is next touched.
	save(t, db, job)

	db = reopen(t, db, file)
	loaded := load(t, db)
	if loaded[0].Key != nil || !reflect.DeepEqual(loaded[0].Commitments, job.Commitments) {
		t.Errorf("loaded with key %x and commitments %q, saved with %q", loaded[0].Key, loaded[0].Commitments, job.Commitments)
	}
	if loaded[1].Commitments != nil {
		t.Errorf("a job saved with no commitments was loaded with %q", loaded[1].Commitments)
	}

	if err := db.DeleteJobs([]string{"j1"}); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := db.sql.QueryRow(`SELECT count(*) FROM job_commitments`).Scan(&left); err != nil || left != 0 {
		t.Errorf("%d commitments left after their job was deleted, %v", left, err)
	}
}

func TestWhatEachWorkerReturnedForAVerifiedTaskIsKeptWithItAndGoesWhenItDoes(t *testing.T) {
	db, file := newDB(t)
	job := jobmodel.New("j1", "wordcount", nil, jobmodel.Distributed, 2, [][]byte{nil, nil}, submitted)
	job.Verify = 2
	save(t, db, job)
	task := job.Tasks[0]
	first := job.StartCopy(task, "node-a", "alpha", submitted)
	second := job.StartCopy(task, "node-b", "beta", submitted)
	save(t, db, job)
	// The second answers first, with blobs; the first with none, and
	// something else.
	job.ReturnCopy(task, second, []byte("counts"), []string{"cid-1", "cid-2"})
	save(t, db, job)
	job.ReturnCopy(task, first, nil, nil)
	job.FailCopy(task, job.StartCopy(task, "node-c", "gamma", submitted), "disk full", 3, submitted)
	save(t, db, job)
	// Saved again with nothing new: a result is written once.
	job.RequeueCopy(task, job.StartCopy(task, "node-c", "gamma", submitted), "the coordinator stopped")
	save(t, db, job)

	db = reopen(t, db, file)
	loaded := load(t, db)[0]
	got := loaded.Tasks[0]
	if loaded.Verify != 2 || loaded.ToProto().GetSpec().GetVerify() != 2 {
		t.Errorf("loaded as verified by %d", loaded.Verify)
	}
	if len(got.Results) != 2 || got.Results[0].Digest() != task.Results[0].Digest() || got.Results[1].Digest() != task.Results[1].Digest() {
		t.Fatalf("results as loaded:\n%+v\nas saved:\n%+v", got.Results, task.Results)
	}
	if r := got.Results[1]; r.Attempt != 2 || r.NodeID != "node-b" || r.NodeName != "beta" || string(r.Output) != "counts" || !reflect.DeepEqual(r.Blobs, []string{"cid-1", "cid-2"}) {
		t.Errorf("the second attempt's result as loaded: %+v", r)
	}
	if r := got.Results[0]; r.Attempt != 1 || r.NodeName != "alpha" || len(r.Output) != 0 || r.Blobs != nil {
		t.Errorf("the first attempt's result as loaded: %+v", r)
	}
	// It stands where it stood: two results that differ, two attempts lost,
	// one more worker to ask, and neither of the two that answered.
	if !reflect.DeepEqual(got.History, task.History) || got.State != jobmodel.Pending || got.Failures != 1 || loaded.Wanted(got) != 1 ||
		!got.Asked("node-a") || !got.Asked("node-b") || got.Asked("node-c") || len(loaded.Tasks[1].Results) != 0 {
		t.Errorf("the task as loaded: %v after %d failures, wanting %d, attempts\n%+v\nsaved as\n%+v", got.State, got.Failures, loaded.Wanted(got), got.History, task.History)
	}

	if err := db.DeleteJobs([]string{"j1"}); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := db.sql.QueryRow(`SELECT count(*) FROM task_results`).Scan(&left); err != nil || left != 0 {
		t.Errorf("%d results left after their job was deleted, %v", left, err)
	}
}
