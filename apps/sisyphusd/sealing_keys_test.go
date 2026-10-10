package main

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/ipfs/go-cid"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/blobclient"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
	"github.com/sisyphus-network/Sisyphus/packages/sealed"
	"github.com/sisyphus-network/Sisyphus/packages/sealed/sealedtest"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// sealedFile stores a file sealed with a sealing key, as a private file is
// stored, and returns its CID.
func (p *pool) sealedFile(key sealed.Key, text string) cid.Cid {
	p.t.Helper()
	c, err := blobclient.Upload(p.ctx, p.blobs, sealed.Encrypt(key, strings.NewReader(text)))
	if err != nil {
		p.t.Fatal(err)
	}
	return c
}

// ringOf makes of the keys a task came with what a worker would.
func ringOf(a *pb.TaskAssignment) *sealed.Ring {
	var grants []sealed.Grant
	for _, given := range a.GetKeys() {
		grants = append(grants, sealed.Grant{ID: sealed.KeyID(given.GetId()), Key: sealed.Key(given.GetKey())})
	}
	return sealed.NewRing(grants...)
}

// opens reports what comes of opening one of the pool's blobs with a ring.
func (p *pool) opens(ring *sealed.Ring, c cid.Cid) error {
	data := stored(p.t, p.store, c)
	_, err := sealed.Open(ring, bytes.NewReader(data), uint64(len(data)))
	return err
}

// A private job's workers are given the job's own key and the key of each
// sealed blob its parameters name, however deep. They are not given the
// sealing key those were derived from, so what they hold opens nothing
// else that key seals.
func TestAWorkerIsGivenTheKeysToItsJobsDataAndNoOther(t *testing.T) {
	g := gated{started: make(chan struct{}, 16), release: make(chan struct{})}
	p := startPool(t, runtime.NewRegistry(g, runtime.Primes{}))
	w := p.connectRaw(hello("rig", 2, "gated"))
	w.welcome()

	key := sealed.NewKey()
	input, second, other := p.sealedFile(key, "what the job is to read"), p.sealedFile(key, "and this, named deeper"), p.sealedFile(key, "another file altogether")
	plain := p.upload("not secret")
	absent, err := storage.CID(p.ctx, strings.NewReader("never stored"))
	if err != nil {
		t.Fatal(err)
	}
	params := `{"input":"` + input.String() + `","more":[{"deep":"` + second.String() + `"},"` + input.String() + `",7],"open":"` + plain + `","gone":"` + absent.String() + `","note":"no CID at all"}`
	id := p.submit(&pb.JobSpec{Workload: "gated", Params: []byte(params), MaxTasks: 2, Key: key[:]}).GetJobId()

	first, again := w.assignment(), w.assignment()
	own := key.ForJob(id)
	if len(first.GetKeys()) != 3 || !bytes.Equal(first.GetKeys()[0].GetKey(), own.Key[:]) || !bytes.Equal(first.GetKeys()[0].GetId(), own.ID[:]) {
		t.Fatalf("the task came with %d keys, the first of them not the job's own", len(first.GetKeys()))
	}
	if first.GetKey() != nil {
		t.Error("the task came with a sealing key whole")
	}
	for _, given := range first.GetKeys() {
		if bytes.Equal(given.GetKey(), key[:]) {
			t.Error("the sealing key itself is among the keys the task came with")
		}
	}
	// Each task of the job comes with the same keys.
	if len(again.GetKeys()) != 3 || !bytes.Equal(again.GetKeys()[2].GetKey(), first.GetKeys()[2].GetKey()) {
		t.Errorf("the job's second task came with %d keys", len(again.GetKeys()))
	}

	ring := ringOf(first)
	for name, c := range map[string]cid.Cid{"its input": input, "the input named deeper": second} {
		if err := p.opens(ring, c); err != nil {
			t.Errorf("with the keys it was given a worker opens %s: %v", name, err)
		}
	}
	if err := p.opens(ring, other); !errors.Is(err, sealed.ErrNoKey) {
		t.Errorf("a file the job does not name, sealed with the same sealing key: %v, want ErrNoKey", err)
	}
	// Nor does it open what another job stored, though that was sealed
	// with a key derived from the same one.
	elsewhere, err := p.store.Put(p.ctx, sealed.EncryptWith(key.ForJob("another-job"), strings.NewReader("another job's output")))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.opens(ring, elsewhere); !errors.Is(err, sealed.ErrNoKey) {
		t.Errorf("what another job stored: %v, want ErrNoKey", err)
	}
	for _, a := range []*pb.TaskAssignment{first, again} {
		w.reply(&pb.TaskResult{TaskId: a.GetTaskId(), Attempt: a.GetAttempt(), Outcome: &pb.TaskResult_Output{Output: []byte("ok")}})
	}
	if done := p.wait(id); done.GetState() != pb.JobState_JOB_STATE_SUCCEEDED {
		t.Errorf("the job: %v %s", done.GetState(), done.GetError())
	}
}

// oldFile stores the blob that was sealed before keys were derived, and
// returns its CID and the sealing key it was sealed with.
func (p *pool) oldFile() (cid.Cid, sealed.Key) {
	p.t.Helper()
	key, err := sealed.ParseKey(sealedtest.OldKey)
	if err != nil {
		p.t.Fatal(err)
	}
	blob, err := base64.StdEncoding.DecodeString(sealedtest.OldBlob)
	if err != nil {
		p.t.Fatal(err)
	}
	c, err := blobclient.Upload(p.ctx, p.blobs, bytes.NewReader(blob))
	if err != nil {
		p.t.Fatal(err)
	}
	return c, key
}

// A file sealed before each had a key of its own is opened by the sealing
// key and nothing less. A job that names one still runs: its workers are
// given that key whole, as every private job's once were, and the job says
// so.
func TestAJobThatNamesAFileSealedInTheOldFormStillRuns(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	p.startWorker("a", 1)
	p.waitForWorkers(1)
	old, key := p.oldFile()

	job := p.wait(p.submit(&pb.JobSpec{Workload: "wordcount", Params: []byte(`{"input":"` + old.String() + `"}`), MaxTasks: 1, Key: key[:]}).GetJobId())
	if job.GetState() != pb.JobState_JOB_STATE_SUCCEEDED {
		t.Fatalf("the job: %v %s", job.GetState(), job.GetError())
	}
	if _, want := wordCountLocally(t, sealedtest.OldText); p.unsealedTable(job, key) != want {
		t.Error("the words of a file sealed in the old form were counted wrong")
	}
	told := story(p.events(job.GetJobId(), 0))
	if strings.Count(told, "was sealed before each file had a key of its own, so this job's workers are given the whole sealing key") != 1 {
		t.Errorf("the job's events:\n%s", told)
	}
	if !strings.Contains(p.logs.String(), "its workers are given that key whole") {
		t.Errorf("the coordinator logged:\n%s", p.logs.String())
	}

	// What the worker is sent: the job's own key, and the sealing key
	// whole, said once however many times the file is named.
	g := gated{started: make(chan struct{}, 16), release: make(chan struct{})}
	q := startPool(t, runtime.NewRegistry(g))
	w := q.connectRaw(hello("rig", 1, "gated"))
	w.welcome()
	old, _ = q.oldFile()
	id := q.submit(&pb.JobSpec{Workload: "gated", Params: []byte(`["` + old.String() + `","` + old.String() + `"]`), MaxTasks: 1, Key: key[:]}).GetJobId()
	a := w.assignment()
	if len(a.GetKeys()) != 1 || !bytes.Equal(a.GetKey(), key[:]) {
		t.Errorf("the task came with %d keys and a whole key of %d bytes", len(a.GetKeys()), len(a.GetKey()))
	}
	w.reply(&pb.TaskResult{TaskId: a.GetTaskId(), Attempt: a.GetAttempt(), Outcome: &pb.TaskResult_Output{Output: []byte("ok")}})
	q.wait(id)
	if said := strings.Count(story(q.events(id, 0)), "was sealed before each file had a key of its own"); said != 1 {
		t.Errorf("that the whole key was given was said %d times", said)
	}
}

// A worker from before keys were derived would store unsealed what a
// private job's task was to seal. It says nothing of taking keys as they
// are now given, and so is given no private job's tasks.
func TestAPrivateJobWaitsForAWorkerThatTakesItsKeys(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	from := func(name string, labels ...string) *rawWorker {
		w := p.connectRaw(&pb.WorkerMessage{Kind: &pb.WorkerMessage_Hello{Hello: &pb.Hello{
			Name: name, Capabilities: &pb.NodeCapabilities{TaskSlots: 1, Workloads: []string{"primes"}, Labels: labels},
		}}})
		w.welcome()
		return w
	}
	before := from("before")
	key := sealed.NewKey()
	private := p.submit(&pb.JobSpec{Workload: "primes", Params: []byte(`{"from":0,"to":10}`), MaxTasks: 1, Key: key[:]}).GetJobId()
	open := p.submit(&pb.JobSpec{Workload: "primes", Params: []byte(`{"from":0,"to":10}`), MaxTasks: 1}).GetJobId()

	// The job that is not private goes to it; the private one waits.
	a := before.assignment()
	if a.GetJobId() != open || len(a.GetKeys()) != 0 || a.GetKey() != nil {
		t.Fatalf("a worker from before was handed a task of job %s with %d keys", a.GetJobId(), len(a.GetKeys()))
	}
	before.reply(&pb.TaskResult{TaskId: a.GetTaskId(), Attempt: a.GetAttempt(), Outcome: &pb.TaskResult_Output{Output: []byte(`{"count":4}`)}})
	p.wait(open)
	if waiting := p.job(private); waiting.GetState() != pb.JobState_JOB_STATE_PENDING {
		t.Fatalf("with no worker that takes its keys the private job is %v", waiting.GetState())
	}

	now := from("now", runtime.SealingLabel)
	a = now.assignment()
	if a.GetJobId() != private || len(a.GetKeys()) != 1 {
		t.Fatalf("a worker that takes keys was handed a task of job %s with %d keys", a.GetJobId(), len(a.GetKeys()))
	}
	now.reply(&pb.TaskResult{TaskId: a.GetTaskId(), Attempt: a.GetAttempt(), Outcome: &pb.TaskResult_Output{Output: []byte(`{"count":4}`)}})
	if done := p.wait(private); done.GetState() != pb.JobState_JOB_STATE_SUCCEEDED {
		t.Errorf("the private job: %v %s", done.GetState(), done.GetError())
	}
}
