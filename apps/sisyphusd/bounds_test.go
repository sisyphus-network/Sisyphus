package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ipfs/go-cid"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/coordinator"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// logging sends, as the worker running a task, lines the task is to have
// logged.
func (w *rawWorker) logging(a *pb.TaskAssignment, lines ...string) {
	w.t.Helper()
	w.send(&pb.WorkerMessage{Kind: &pb.WorkerMessage_TaskUpdate{TaskUpdate: &pb.TaskUpdate{TaskId: a.GetTaskId(), Attempt: a.GetAttempt(), Log: lines}}})
}

// A coordinator keeps what a job's tasks log, so a worker can have only so
// much of it kept: so many lines, of such a length.
func TestOnlySoMuchOfWhatAJobsTasksLogIsKept(t *testing.T) {
	const kept = 20000
	p := startPool(t, runtime.Builtin())
	w := p.connectRaw(hello("chatty", 1, "primes"))
	w.welcome()
	id := p.submit(primesJob(pb.ScheduleMode_SCHEDULE_MODE_FULL_WORKER, 0)).GetJobId()
	task := w.assignment()

	w.logging(task, "line 0")
	lines := make([]string, 0, 5000)
	for sent := 1; sent < kept+500; sent += len(lines) {
		lines = lines[:0]
		for i := range 5000 {
			lines = append(lines, fmt.Sprintf("line %d", sent+i))
		}
		w.logging(task, lines...)
	}
	w.reply(&pb.TaskResult{TaskId: task.GetTaskId(), Attempt: task.GetAttempt(), Outcome: &pb.TaskResult_Output{Output: []byte(`{"count":5}`)}})
	if done := p.wait(id); done.GetState() != pb.JobState_JOB_STATE_SUCCEEDED {
		t.Fatalf("the job: %v %s", done.GetState(), done.GetError())
	}

	var logged []string
	for _, e := range p.events(id, 0) {
		if e.GetKind() == "log" {
			logged = append(logged, e.GetText())
		}
	}
	// Of the lines kept, the last 5000 events are to hand: the last lines
	// logged within the limit, and then the word that no more are kept.
	last := len(logged) - 1
	if last < 1 || logged[last] != fmt.Sprintf("this job's tasks have logged %d lines, and what they log from here on is not kept", kept) || logged[last-1] != fmt.Sprintf("line %d", kept-1) {
		t.Errorf("the log ends %q", logged[max(last-2, 0):])
	}
}

// A line longer than a worker would send is cut to what one would send,
// between characters and not within one.
func TestALoggedLineLongerThanAWorkerWouldSendIsCutShort(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	w := p.connectRaw(hello("wordy", 1, "primes"))
	w.welcome()
	cut := p.cutLine(w, strings.Repeat("é", 4096))
	if !strings.HasSuffix(cut, " [cut short]") || len(cut) > 4<<10+2*len(" [cut short]") || strings.ContainsRune(cut, '\uFFFD') || !strings.HasPrefix(cut, "ééé") {
		t.Errorf("a long line is kept as %d bytes ending %q", len(cut), cut[max(len(cut)-20, 0):])
	}
	if short := p.cutLine(w, "a line of no great length"); short != "a line of no great length" {
		t.Errorf("a short line is kept as %q", short)
	}
}

// cutLine has a task of a new job, run by w, log one line, and returns it
// as kept.
func (p *pool) cutLine(w *rawWorker, line string) string {
	p.t.Helper()
	id := p.submit(primesJob(pb.ScheduleMode_SCHEDULE_MODE_FULL_WORKER, 0)).GetJobId()
	task := w.assignment()
	w.logging(task, line)
	w.reply(&pb.TaskResult{TaskId: task.GetTaskId(), Attempt: task.GetAttempt(), Outcome: &pb.TaskResult_Output{Output: []byte(`{"count":5}`)}})
	p.wait(id)
	for _, e := range p.events(id, 0) {
		if e.GetKind() == "log" {
			return e.GetText()
		}
	}
	p.t.Fatal("the line was not kept")
	return ""
}

// What a worker says its task stored is taken as stored only if the pool
// holds it, and what it says the task read only if it could be a blob.
func TestWhatAWorkerSaysItsTaskStoredIsTakenOnlyIfThePoolHoldsIt(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	w := p.connectRaw(hello("careless", 1, "primes"))
	w.welcome()
	absent, err := storage.CID(p.ctx, strings.NewReader("never uploaded"))
	if err != nil {
		t.Fatal(err)
	}
	stored, opened := p.upload("what the task did store"), p.upload("what it read")
	job := p.submit(primesJob(pb.ScheduleMode_SCHEDULE_MODE_DISTRIBUTED, 1))
	task := w.assignment()
	w.reply(&pb.TaskResult{
		TaskId: task.GetTaskId(), Attempt: task.GetAttempt(),
		Outcome:      &pb.TaskResult_Output{Output: []byte(`{"count":5}`)},
		ReadBlobs:    []string{"nor is this", opened},
		WrittenBlobs: []string{"not a CID at all", absent.String(), stored},
	})
	if done := p.wait(job.GetJobId()); string(done.GetResult()) != `{"count":5}` {
		t.Errorf("result %s (%s)", done.GetResult(), done.GetError())
	}
	for _, named := range []string{"not a CID at all", absent.String()} {
		if !strings.Contains(p.logs.String(), `msg="a worker named a blob as stored by its task that the pool does not hold"`) || !strings.Contains(p.logs.String(), `blob="`+named+`"`) && !strings.Contains(p.logs.String(), "blob="+named) {
			t.Errorf("nothing was said of %q:\n%s", named, p.logs.String())
		}
	}
	// What it did store was kept for as long as the job ran, though it was
	// named beside what was not there.
	if !strings.Contains(p.logs.String(), "job finished") || strings.Contains(p.logs.String(), "could not pin a job's blobs") {
		t.Errorf("the log:\n%s", p.logs.String())
	}
	if read := p.job(job.GetJobId()).GetInputBlobs(); len(read) != 1 || read[0] != opened {
		t.Errorf("the job is said to have read %q", read)
	}
}

// pinFails is a store that cannot pin.
type pinFails struct{ *storage.Store }

func (pinFails) Pin(context.Context, string, time.Time, ...cid.Cid) error {
	return errors.New("pin file is read-only")
}

// A store that cannot pin is not the worker's doing: what it stored is
// still taken as stored.
func TestJobSucceedsEvenIfWhatItsTasksStoredCannotBePinned(t *testing.T) {
	p := startPoolOver(t, runtime.Builtin(), func(store *storage.Store) coordinator.Store { return pinFails{store} })
	w := p.connectRaw(hello("honest", 1, "primes"))
	w.welcome()
	stored := p.upload("what the task stored")
	job := p.submit(primesJob(pb.ScheduleMode_SCHEDULE_MODE_DISTRIBUTED, 1))
	task := w.assignment()
	w.reply(&pb.TaskResult{TaskId: task.GetTaskId(), Attempt: task.GetAttempt(), Outcome: &pb.TaskResult_Output{Output: []byte(`{"count":5}`)}, WrittenBlobs: []string{stored}})
	if done := p.wait(job.GetJobId()); string(done.GetResult()) != `{"count":5}` {
		t.Errorf("result %s (%s)", done.GetResult(), done.GetError())
	}
	if !strings.Contains(p.logs.String(), "could not pin a job's blobs") || strings.Contains(p.logs.String(), "does not hold") {
		t.Errorf("the log:\n%s", p.logs.String())
	}
}

// A task may name only so many blobs: each is noted with the job and
// pinned for it.
func TestATaskThatNamesTooManyBlobsFails(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	w := p.connectRaw(hello("greedy", 1, "primes"))
	w.welcome()
	id := p.submit(primesJob(pb.ScheduleMode_SCHEDULE_MODE_FULL_WORKER, 0)).GetJobId()
	task := w.assignment()
	stored := p.upload("named again and again")
	w.reply(&pb.TaskResult{
		TaskId: task.GetTaskId(), Attempt: task.GetAttempt(), Outcome: &pb.TaskResult_Output{Output: []byte(`{"count":5}`)},
		ReadBlobs: slicesRepeat(stored, 4000), WrittenBlobs: slicesRepeat(stored, 97),
	})
	// It is tried again, as any task that failed is.
	again := w.assignment()
	w.reply(&pb.TaskResult{TaskId: again.GetTaskId(), Attempt: again.GetAttempt(), Outcome: &pb.TaskResult_Output{Output: []byte(`{"count":5}`)}})
	if done := p.wait(id); done.GetState() != pb.JobState_JOB_STATE_SUCCEEDED {
		t.Fatalf("the job: %v %s", done.GetState(), done.GetError())
	}
	failed := false
	for _, e := range p.events(id, 0) {
		failed = failed || e.GetKind() == "task-failed" && e.GetText() == "the worker named 4097 blobs as read and stored by the task, and a task may have 4096"
	}
	if !failed || len(p.jobPins(id)) != 0 {
		t.Errorf("the attempt failed: %v; the job's pins: %v", failed, p.jobPins(id))
	}
}

func slicesRepeat(s string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = s
	}
	return out
}
