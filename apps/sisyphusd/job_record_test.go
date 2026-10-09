package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multihash"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/access"
	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/coordinator"
	jobmodel "github.com/sisyphus-network/Sisyphus/packages/job-model"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
	"github.com/sisyphus-network/Sisyphus/packages/sealed"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// recordOf returns a finished job's record as the node gives it, checked
// against the job.
func (p *pool) recordOf(jobID string) *pb.JobRecord {
	p.t.Helper()
	record, err := p.client.GetJobRecord(p.ctx, &pb.GetJobRecordRequest{JobId: jobID, Verify: true})
	if err != nil {
		p.t.Fatalf("the record of job %s: %v", jobID, err)
	}
	return record
}

// recordNodes returns a record's nodes as parsed DAG-JSON, by CID, having
// checked that each node's CID is the hash of its bytes.
func recordNodes(t *testing.T, record *pb.JobRecord) map[string]map[string]any {
	t.Helper()
	nodes := make(map[string]map[string]any)
	for _, node := range record.GetNodes() {
		if sum, _ := (cid.V1Builder{Codec: cid.DagCBOR, MhType: multihash.SHA2_256}).Sum(node.GetData()); sum.String() != node.GetCid() {
			t.Errorf("node %s holds bytes that hash to %s", node.GetCid(), sum)
		}
		var fields map[string]any
		if err := json.Unmarshal([]byte(node.GetJson()), &fields); err != nil {
			t.Fatalf("node %s as JSON: %v\n%s", node.GetCid(), err, node.GetJson())
		}
		nodes[node.GetCid()] = fields
	}
	return nodes
}

// linkOf returns the CID a DAG-JSON link names.
func linkOf(v any) string {
	link, _ := v.(map[string]any)["/"].(string)
	return link
}

// bytesOf returns the bytes a DAG-JSON value holds.
func bytesOf(t *testing.T, v any) []byte {
	t.Helper()
	held, _ := v.(map[string]any)["/"].(map[string]any)
	text, _ := held["bytes"].(string)
	data, err := base64.RawStdEncoding.DecodeString(text)
	if err != nil {
		t.Fatalf("bytes in a record: %v", err)
	}
	return data
}

// jobRecordPins returns the pins a store holds on a job's record.
func jobRecordPins(store *storage.Store, jobID string) []storage.Pin {
	var pins []storage.Pin
	for _, pin := range store.Pins() {
		if pin.Owner == "record:"+jobID {
			pins = append(pins, pin)
		}
	}
	return pins
}

func TestAFinishedJobCarriesARecordOfWhatWasAskedWhoDidItAndWhatCameOfIt(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	p.startWorker("a", 1)
	p.startWorker("b", 1)
	p.waitForWorkers(2)
	done := p.wait(p.submit(primesJob(pb.ScheduleMode_SCHEDULE_MODE_DISTRIBUTED, 2)).GetJobId())
	if done.GetState() != pb.JobState_JOB_STATE_SUCCEEDED || done.GetRecordCid() == "" || done.GetSubmitterId() != p.ident.ID() {
		t.Fatalf("the job %v, with record %q, submitted by %q", done.GetState(), done.GetRecordCid(), done.GetSubmitterId())
	}

	record := p.recordOf(done.GetJobId())
	if record.GetRootCid() != done.GetRecordCid() || !record.GetMatches() || record.GetRecomputedCid() != done.GetRecordCid() {
		t.Fatalf("the record is %s and works out again as %s; the job names %s", record.GetRootCid(), record.GetRecomputedCid(), done.GetRecordCid())
	}
	if len(record.GetNodes()) != 5 || record.GetNodes()[0].GetCid() != record.GetRootCid() {
		t.Fatalf("the record has %d nodes, want the root first, a manifest, two receipts and a result", len(record.GetNodes()))
	}
	nodes := recordNodes(t, record)
	root := nodes[record.GetRootCid()]
	if root["kind"] != "sisyphus-job-record" || root["version"] != float64(1) || root["job"] != done.GetJobId() || root["private"] != false {
		t.Errorf("the root: %v", root)
	}
	manifest := nodes[linkOf(root["manifest"])]
	if manifest["workload"] != "primes" || manifest["submitter"] != p.ident.ID() || string(bytesOf(t, manifest["params"])) != string(done.GetSpec().GetParams()) {
		t.Errorf("the manifest: %v", manifest)
	}
	workers := map[string]string{p.workerIdents["a"].ID(): "a", p.workerIdents["b"].ID(): "b"}
	for i, link := range root["receipts"].([]any) {
		receipt := nodes[linkOf(link)]
		attempts, _ := receipt["attempts"].([]any)
		if receipt["task"] != float64(i) || receipt["state"] != "succeeded" || len(attempts) != 1 {
			t.Fatalf("receipt %d: %v", i, receipt)
		}
		attempt := attempts[0].(map[string]any)
		if name, known := workers[attempt["node"].(string)]; !known || attempt["name"] != name || attempt["state"] != "succeeded" {
			t.Errorf("receipt %d says the task was run by %v", i, attempt)
		}
	}
	result := nodes[linkOf(root["result"])]
	if result["state"] != "succeeded" || string(bytesOf(t, result["result"])) != string(done.GetResult()) {
		t.Errorf("the result: %v", result)
	}

	// The store holds the record, for as long as the job is on record:
	// long after the job's data has gone.
	rootCID := cid.MustParse(record.GetRootCid())
	waitFor(t, func() bool { return len(jobRecordPins(p.store, done.GetJobId())) == 1 })
	if pin := jobRecordPins(p.store, done.GetJobId())[0]; !pin.CID.Equals(rootCID) || !pin.Expires.IsZero() {
		t.Errorf("the record is pinned as %+v", pin)
	}
	if _, err := p.store.GC(p.ctx, time.Now().Add(testRetain+2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if again := p.recordOf(done.GetJobId()); len(again.GetNodes()) != 5 || !again.GetMatches() {
		t.Errorf("after a collection past the retention period the record has %d nodes", len(again.GetNodes()))
	}
	if _, err := p.store.GetNode(p.ctx, rootCID); err != nil {
		t.Errorf("the store no longer holds the record's root: %v", err)
	}

	// A record is for those who may see the job: a worker may not ask.
	_, creds := p.admit(access.Worker)
	conn, err := grpc.NewClient(p.addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := pb.NewNodeServiceClient(conn).GetJobRecord(p.ctx, &pb.GetJobRecordRequest{JobId: done.GetJobId()}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("a worker asking for a record: %v, want PermissionDenied", err)
	}
}

func TestAJobHasNoRecordUntilItIsOverAndACancelledJobHasOne(t *testing.T) {
	g := gated{started: make(chan struct{}, 16), release: make(chan struct{})}
	p := startPool(t, runtime.NewRegistry(g))
	p.startWorker("rig", 1)
	p.waitForWorkers(1)
	job := p.submit(&pb.JobSpec{Workload: "gated", MaxTasks: 2})
	<-g.started

	if _, err := p.client.GetJobRecord(p.ctx, &pb.GetJobRecordRequest{JobId: job.GetJobId()}); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "is not over") {
		t.Errorf("the record of a running job: %v", err)
	}
	if _, err := p.client.GetJobRecord(p.ctx, &pb.GetJobRecordRequest{JobId: "no-such-job"}); status.Code(err) != codes.NotFound {
		t.Errorf("the record of a job that is not there: %v", err)
	}
	if running, _ := p.coord.Get(job.GetJobId()); running.GetRecordCid() != "" {
		t.Errorf("a running job names a record: %s", running.GetRecordCid())
	}

	cancelled, err := p.client.CancelJob(p.ctx, &pb.CancelJobRequest{JobId: job.GetJobId()})
	if err != nil {
		t.Fatal(err)
	}
	record := p.recordOf(job.GetJobId())
	if record.GetRootCid() != cancelled.GetJob().GetRecordCid() || !record.GetMatches() {
		t.Fatalf("the cancelled job names record %q and has %q", cancelled.GetJob().GetRecordCid(), record.GetRootCid())
	}
	nodes := recordNodes(t, record)
	root := nodes[record.GetRootCid()]
	if result := nodes[linkOf(root["result"])]; result["state"] != "cancelled" || result["error"] != "cancelled" {
		t.Errorf("the result of a cancelled job: %v", result)
	}
	// One task was running and one had never been handed out.
	ran, waited := nodes[linkOf(root["receipts"].([]any)[0])], nodes[linkOf(root["receipts"].([]any)[1])]
	if attempts := ran["attempts"].([]any); len(attempts) != 1 || attempts[0].(map[string]any)["state"] != "cancelled" || attempts[0].(map[string]any)["name"] != "rig" {
		t.Errorf("the receipt of a task that was running: %v", ran)
	}
	if attempts := waited["attempts"].([]any); len(attempts) != 0 || waited["state"] != "cancelled" {
		t.Errorf("the receipt of a task nobody was given: %v", waited)
	}
}

func TestAGraphsRecordLinksTheRecordsOfItsSteps(t *testing.T) {
	p := startPool(t, runtime.Builtin().With(runtime.Graph{}))
	p.startWorker("rig", 4)
	p.waitForWorkers(1)
	done := p.wait(p.submit(graphOf(`
		{"name":"low","workload":"primes","params":{"from":0,"to":1000},"tasks":2},
		{"name":"span","workload":"primes","params":{"from":0,"to":"${low.result.count}"}}`)).GetJobId())
	if done.GetState() != pb.JobState_JOB_STATE_SUCCEEDED {
		t.Fatalf("the graph %v: %s", done.GetState(), done.GetError())
	}
	steps := p.stepsOf(done.GetJobId())

	record := p.recordOf(done.GetJobId())
	if !record.GetMatches() || len(record.GetNodes()) != 6 {
		t.Fatalf("the graph's record matches the graph: %v; it has %d nodes, want the usual four, two receipts and the graph", record.GetMatches(), len(record.GetNodes()))
	}
	nodes := recordNodes(t, record)
	listed := nodes[linkOf(nodes[record.GetRootCid()]["graph"])]["steps"].([]any)
	if len(listed) != 2 {
		t.Fatalf("the graph's record lists %d steps", len(listed))
	}
	for i, name := range []string{"low", "span"} {
		step, entry := steps[name][0], listed[i].(map[string]any)
		if entry["step"] != name || entry["job"] != step.GetJobId() || linkOf(entry["record"]) != step.GetRecordCid() || step.GetRecordCid() == "" {
			t.Errorf("step %s is job %s with record %q; the graph's record says %v", name, step.GetJobId(), step.GetRecordCid(), entry)
		}
		// The step's own record names the graph, which had no record to
		// link when the step ended, and whoever submitted the graph.
		own := p.recordOf(step.GetJobId())
		stepNodes := recordNodes(t, own)
		stepRoot := stepNodes[own.GetRootCid()]
		if parent, _ := stepRoot["parent"].(map[string]any); !own.GetMatches() || parent["job"] != done.GetJobId() || parent["step"] != name {
			t.Errorf("the record of step %s: %v", name, stepRoot)
		}
		if manifest := stepNodes[linkOf(stepRoot["manifest"])]; manifest["submitter"] != p.ident.ID() {
			t.Errorf("step %s was submitted by %v, want the graph's submitter", name, manifest["submitter"])
		}
	}
	// A pin on the graph's record holds its steps' records too.
	waitFor(t, func() bool { return len(jobRecordPins(p.store, done.GetJobId())) == 1 })
	for _, step := range steps {
		waitFor(t, func() bool { return len(jobRecordPins(p.store, step[0].GetJobId())) == 1 })
		if err := p.store.Unpin("record:"+step[0].GetJobId(), cid.MustParse(step[0].GetRecordCid())); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := p.store.GC(p.ctx, time.Now().Add(testRetain+2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := p.store.GetNode(p.ctx, cid.MustParse(steps["low"][0].GetRecordCid())); err != nil {
		t.Errorf("a step's record was collected from under the graph's: %v", err)
	}
}

func TestAPrivateJobsRecordHoldsNoPlaintext(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	p.startWorker("a", 2)
	p.waitForWorkers(1)
	key := sealed.NewKey()
	job := p.privateWordcount(sampleText(), key, 2)
	if job.GetState() != pb.JobState_JOB_STATE_SUCCEEDED || !job.GetPrivate() {
		t.Fatalf("job %v, private %v: %s", job.GetState(), job.GetPrivate(), job.GetError())
	}

	record := p.recordOf(job.GetJobId())
	if !record.GetMatches() {
		t.Error("a private job's record does not match the job")
	}
	// The job's parameters and result are not sealed, and the record goes
	// where any member can fetch it: neither is in it, in any encoding.
	secrets := [][]byte{job.GetSpec().GetParams(), job.GetResult(), []byte("boulder"), []byte("Sisyphus"), key[:]}
	for _, node := range record.GetNodes() {
		for _, secret := range secrets {
			encoded := base64.RawStdEncoding.EncodeToString(secret)
			if bytes.Contains(node.GetData(), secret) || strings.Contains(node.GetJson(), encoded) {
				t.Errorf("node %s of a private job's record holds %q: %s", node.GetCid(), secret, node.GetJson())
			}
		}
	}
	nodes := recordNodes(t, record)
	root := nodes[record.GetRootCid()]
	manifest, result := nodes[linkOf(root["manifest"])], nodes[linkOf(root["result"])]
	if root["private"] != true {
		t.Errorf("the root does not say the job is private: %v", root)
	}
	for field, node := range map[string]map[string]any{"params": manifest, "result": result, "error": result} {
		if _, has := node[field]; has {
			t.Errorf("a private job's record has %s: %v", field, node)
		}
	}
	for _, link := range root["receipts"].([]any) {
		if _, has := nodes[linkOf(link)]["output"]; has {
			t.Errorf("a private job's receipt has the task's output: %v", nodes[linkOf(link)])
		}
	}
	// It still names the sealed data, which says nothing to those without
	// the key.
	inputs, outputs := manifest["inputs"].([]any), result["outputs"].([]any)
	if len(inputs) != 1 || linkOf(inputs[0]) != job.GetInputBlobs()[0] || len(outputs) != 1 || linkOf(outputs[0]) != job.GetOutputBlobs()[0] {
		t.Errorf("the sealed data the record names: inputs %v, outputs %v", inputs, outputs)
	}
	for _, link := range append(inputs, outputs...) {
		if !sealed.IsSealed(stored(t, p.store, cid.MustParse(linkOf(link)))) {
			t.Errorf("the record links %s, which is not sealed", linkOf(link))
		}
	}
}

func TestParametersTooLongForARecordAreLinkedAndStoredBesideIt(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	p.startWorker("rig", 1)
	p.waitForWorkers(1)
	// JSON may be padded as far as anyone likes.
	params := []byte(`{"from":0,"to":1000}` + strings.Repeat(" ", 20_000))
	done := p.wait(p.submit(&pb.JobSpec{Workload: "primes", Params: params, MaxTasks: 1}).GetJobId())
	if done.GetState() != pb.JobState_JOB_STATE_SUCCEEDED {
		t.Fatalf("job %v: %s", done.GetState(), done.GetError())
	}
	record := p.recordOf(done.GetJobId())
	nodes := recordNodes(t, record)
	link := linkOf(nodes[linkOf(nodes[record.GetRootCid()]["manifest"])]["params"])
	want, err := storage.CID(p.ctx, bytes.NewReader(params))
	if err != nil {
		t.Fatal(err)
	}
	if link != want.String() || !record.GetMatches() {
		t.Fatalf("the manifest links its parameters as %q, want the CID they have as a blob, %s", link, want)
	}
	// They are kept as the job's data is, for the retention period.
	waitFor(t, func() bool { return len(jobRecordPins(p.store, done.GetJobId())) == 1 })
	if got := stored(t, p.store, want); !bytes.Equal(got, params) {
		t.Error("the stored parameters are not the ones submitted")
	}
	held := false
	for _, pin := range p.jobPins(done.GetJobId()) {
		held = held || (pin.CID.Equals(want) && !pin.Expires.IsZero())
	}
	if !held {
		t.Errorf("the job does not hold its long parameters for the retention period: %v", p.jobPins(done.GetJobId()))
	}
}

// nodeless is a store that takes in no nodes of linked data.
type nodeless struct{ *storage.Store }

func (nodeless) PutNodes(context.Context, []byte, ...[]byte) (cid.Cid, error) {
	return cid.Undef, errors.New("the disk is full")
}

func TestAJobEndsAsUsualWhenItsRecordCannotBeStored(t *testing.T) {
	p := startPoolOver(t, runtime.Builtin(), func(store *storage.Store) coordinator.Store { return nodeless{store} })
	p.startWorker("rig", 1)
	p.waitForWorkers(1)
	done := p.wait(p.submit(primesJob(pb.ScheduleMode_SCHEDULE_MODE_FULL_WORKER, 0)).GetJobId())
	// The record's CID follows from the job alone, so the job has it.
	if done.GetState() != pb.JobState_JOB_STATE_SUCCEEDED || done.GetRecordCid() == "" {
		t.Fatalf("job %v with record %q: %s", done.GetState(), done.GetRecordCid(), done.GetError())
	}
	waitFor(t, func() bool { return strings.Contains(p.logs.String(), "could not store a job's record") })
	// Asking for it tries again, and says why it cannot be had.
	if _, err := p.client.GetJobRecord(p.ctx, &pb.GetJobRecordRequest{JobId: done.GetJobId()}); status.Code(err) != codes.Internal || !strings.Contains(err.Error(), "the disk is full") {
		t.Errorf("the record of a job whose record could not be stored: %v", err)
	}
}

// rewriting is a journal that changes the jobs it hands back.
type rewriting struct {
	coordinator.Journal
	change func(*jobmodel.Job)
}

func (r rewriting) LoadJobs() ([]*jobmodel.Job, error) {
	jobs, err := r.Journal.LoadJobs()
	for _, job := range jobs {
		r.change(job)
	}
	return jobs, err
}

// restarted returns a coordinator that has taken up the jobs in a journal.
func restarted(t *testing.T, store coordinator.Store, journal coordinator.Journal) *coordinator.Coordinator {
	t.Helper()
	coord := coordinator.New(coordinator.Config{Workloads: runtime.Builtin(), Store: store, Journal: journal, Log: quiet})
	t.Cleanup(coord.Close)
	if _, err := coord.Recover(); err != nil {
		t.Fatal(err)
	}
	return coord
}

func TestCheckingARecordFindsAJobThatHasChangedSinceItWasWritten(t *testing.T) {
	ctx := context.Background()
	journal := journalIn(t, filepath.Join(t.TempDir(), "node.db"))
	store := storage.NewMemory()
	first := restarted(t, store, journal)
	job, err := first.Submit(ctx, &pb.JobSpec{Workload: "primes", Params: []byte(`{"from":0,"to":10}`), MaxTasks: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Cancel(job.GetJobId()); err != nil {
		t.Fatal(err)
	}
	written, err := first.Record(ctx, job.GetJobId(), true, nil)
	if err != nil || !written.GetMatches() {
		t.Fatalf("the record as first written: %v, %v", written, err)
	}
	first.Close()

	// A restart changes nothing: the job comes back from the database and
	// gives the record it gave before, which the store still holds.
	same := restarted(t, store, journal)
	if again, err := same.Record(ctx, job.GetJobId(), true, nil); err != nil || !again.GetMatches() || again.GetRootCid() != written.GetRootCid() {
		t.Errorf("the record after a restart: %v, %v", again, err)
	}
	same.Close()

	// The job's error rewritten behind the coordinator's back.
	tampered := rewriting{journal, func(j *jobmodel.Job) { j.Err = "nothing to see here" }}
	changed := restarted(t, store, tampered)
	checked, err := changed.Record(ctx, job.GetJobId(), true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if checked.GetMatches() || checked.GetRecomputedCid() == written.GetRootCid() || checked.GetRootCid() != written.GetRootCid() {
		t.Errorf("a changed job checked against its record: matches %v, record %s, worked out again as %s", checked.GetMatches(), checked.GetRootCid(), checked.GetRecomputedCid())
	}
	// What comes back is the record as it was written, not as the job
	// would have it now.
	nodes := recordNodes(t, checked)
	if result := nodes[linkOf(nodes[checked.GetRootCid()]["result"])]; result["error"] != "cancelled" {
		t.Errorf("the stored record's result: %v", result)
	}
	// Not asked to check, it does not say.
	if plain, err := changed.Record(ctx, job.GetJobId(), false, nil); err != nil || plain.GetMatches() || plain.GetRecomputedCid() != "" {
		t.Errorf("a record nobody asked to have checked: %v, %v", plain, err)
	}
	changed.Close()

	// A store that has lost the record gets it again from the job, if the
	// job still gives that record.
	empty := storage.NewMemory()
	healed := restarted(t, empty, journal)
	if again, err := healed.Record(ctx, job.GetJobId(), true, nil); err != nil || !again.GetMatches() || len(again.GetNodes()) != len(written.GetNodes()) {
		t.Errorf("the record from a store that had lost it: %v, %v", again, err)
	}
	if _, err := empty.GetNode(ctx, cid.MustParse(written.GetRootCid())); err != nil || len(jobRecordPins(empty, job.GetJobId())) != 1 {
		t.Errorf("the record was not stored and pinned again: %v", err)
	}
	healed.Close()
	// And if the job has changed as well, the record is gone for good.
	lost := restarted(t, storage.NewMemory(), tampered)
	if _, err := lost.Record(ctx, job.GetJobId(), true, nil); status.Code(err) != codes.Internal || !strings.Contains(err.Error(), "not found") {
		t.Errorf("a record that is in no store and that its job no longer gives: %v", err)
	}
	lost.Close()

	// A job that ended before records were kept names none.
	before := restarted(t, store, rewriting{journal, func(j *jobmodel.Job) { j.Record = "" }})
	if _, err := before.Record(ctx, job.GetJobId(), false, nil); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "has no record") {
		t.Errorf("the record of a job from before records were kept: %v", err)
	}
}

func TestARecordIsKeptAsLongAsItsJobAndAStepAsLongAsItsGraph(t *testing.T) {
	ctx := context.Background()
	logs := new(syncBuffer)
	store := storage.NewMemory()
	coord := coordinator.New(coordinator.Config{
		Workloads: runtime.Builtin().With(runtime.Graph{}), Store: unpinFails{store}, KeepJobs: time.Hour,
		Log: slog.New(slog.NewTextHandler(logs, nil)),
	})
	defer coord.Close()
	// A graph of one step, with nobody to run it.
	graph, err := coord.Submit(ctx, graphOf(`{"name":"count","workload":"primes","params":{"from":0,"to":10}}`))
	if err != nil {
		t.Fatal(err)
	}
	var step *pb.Job
	waitFor(t, func() bool {
		for _, job := range coord.Jobs() {
			if job.GetParentJobId() == graph.GetJobId() {
				step = job
			}
		}
		return step != nil
	})
	// The step is stopped, and the graph fails for it a moment later.
	if step, err = coord.Cancel(step.GetJobId()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		graph, _ = coord.Get(graph.GetJobId())
		return graph.GetState() == pb.JobState_JOB_STATE_FAILED
	})
	record, err := coord.Record(ctx, graph.GetJobId(), true, nil)
	if err != nil || !record.GetMatches() {
		t.Fatalf("the failed graph's record: %v, %v", record, err)
	}
	nodes := recordNodes(t, record)
	if listed := nodes[linkOf(nodes[record.GetRootCid()]["graph"])]["steps"].([]any); len(listed) != 1 || linkOf(listed[0].(map[string]any)["record"]) != step.GetRecordCid() {
		t.Errorf("the failed graph's record lists its steps as %v", listed)
	}
	waitFor(t, func() bool {
		return len(jobRecordPins(store, graph.GetJobId())) == 1 && len(jobRecordPins(store, step.GetJobId())) == 1
	})

	// The step ended first. When its own time is up and the graph's is
	// not, it stays: the graph's record links its record.
	stepEnded, graphEnded := step.GetFinishedAt().AsTime(), graph.GetFinishedAt().AsTime()
	between := stepEnded.Add(time.Hour).Add(graphEnded.Sub(stepEnded)/2 + 1)
	if n, err := coord.Prune(between); n != 0 || err != nil {
		t.Fatalf("pruned %d jobs when only the step's time was up, %v", n, err)
	}
	if n, err := coord.Prune(graphEnded.Add(time.Hour + time.Second)); n != 2 || err != nil {
		t.Fatalf("pruned %d jobs once the graph's time was up, %v; want the graph and its step", n, err)
	}
	// This store cannot release a pin, which is logged and no more.
	if !strings.Contains(logs.String(), "could not release a job's record") {
		t.Errorf("the failure to release the records was not logged:\n%s", logs)
	}

	// A store that can lets the record go with the job.
	plain := storage.NewMemory()
	other := coordinator.New(coordinator.Config{Workloads: runtime.Builtin(), Store: plain, KeepJobs: time.Hour, Log: quiet})
	defer other.Close()
	job, err := other.Submit(ctx, &pb.JobSpec{Workload: "primes", Params: []byte(`{"from":0,"to":10}`), MaxTasks: 1})
	if err != nil {
		t.Fatal(err)
	}
	if job, err = other.Cancel(job.GetJobId()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return len(jobRecordPins(plain, job.GetJobId())) == 1 })
	if n, err := other.Prune(time.Now().Add(2 * time.Hour)); n != 1 || err != nil {
		t.Fatalf("pruned %d jobs, %v", n, err)
	}
	if left := jobRecordPins(plain, job.GetJobId()); len(left) != 0 {
		t.Errorf("a forgotten job's record is still pinned: %v", left)
	}
	if _, err := plain.GC(ctx, time.Now().Add(2*storage.GracePeriod)); err != nil {
		t.Fatal(err)
	}
	if _, err := plain.GetNode(ctx, cid.MustParse(job.GetRecordCid())); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("a forgotten job's record after a collection: %v, want it gone", err)
	}
}

// slowSplit is a workload whose jobs take their time being split, so a test
// can act while one is on its way in.
type slowSplit struct {
	broken
	entered, release chan struct{}
}

func (slowSplit) Name() string { return "slow-split" }

func (s slowSplit) Split(context.Context, runtime.Blobs, []byte, int) ([][]byte, error) {
	s.entered <- struct{}{}
	<-s.release
	return [][]byte{nil}, nil
}

func TestAStepIsNotBegunOnceItsGraphIsOver(t *testing.T) {
	ctx := context.Background()
	slow := slowSplit{entered: make(chan struct{}), release: make(chan struct{})}
	coord := coordinator.New(coordinator.Config{Workloads: runtime.NewRegistry(runtime.Graph{}, slow), Store: storage.NewMemory(), Log: quiet})
	defer coord.Close()
	graph, err := coord.Submit(ctx, graphOf(`{"name":"late","workload":"slow-split"}`))
	if err != nil {
		t.Fatal(err)
	}
	// The graph is cancelled while its step is on its way in.
	<-slow.entered
	if _, err := coord.Cancel(graph.GetJobId()); err != nil {
		t.Fatal(err)
	}
	written, err := coord.Record(ctx, graph.GetJobId(), true, nil)
	if err != nil {
		t.Fatal(err)
	}
	close(slow.release)
	// Give the step every chance to arrive after all.
	time.Sleep(50 * time.Millisecond)
	if jobs := coord.Jobs(); len(jobs) != 1 {
		t.Errorf("there are %d jobs, want only the graph: a step was begun for a graph that was over", len(jobs))
	}
	// So the graph's record is still the graph's whole story.
	if again, err := coord.Record(ctx, graph.GetJobId(), true, nil); err != nil || !again.GetMatches() || again.GetRootCid() != written.GetRootCid() {
		t.Errorf("the cancelled graph's record: %v, %v", again, err)
	}
}

// scriptedRecord is a node that gives every job the same record.
type scriptedRecord struct {
	pb.UnimplementedNodeServiceServer
	record *pb.JobRecord
}

func (s scriptedRecord) GetJobRecord(context.Context, *pb.GetJobRecordRequest) (*pb.JobRecord, error) {
	return s.record, nil
}

func TestCLIPrintsAJobsRecordAndItIsThereAfterARestart(t *testing.T) {
	dataDir := t.TempDir()
	addr, stop := startNode(t, "--data-dir", dataDir)
	out := mustCLI(t, "job", "submit", "--addr", addr, "--params", `{"from":0,"to":1000}`)
	found := regexp.MustCompile(`job ([0-9a-f]+) succeeded`).FindStringSubmatch(out)
	if found == nil {
		t.Fatalf("no finished job in:\n%s", out)
	}
	id := found[1]

	printed := mustCLI(t, "job", "record", "--addr", addr, id)
	first, rest, _ := strings.Cut(printed, "\n")
	root, err := cid.Decode(first)
	if err != nil || root.Type() != cid.DagCBOR {
		t.Fatalf("the first line printed is %q, want the record's CID: %v", first, err)
	}
	var nodes map[string]map[string]any
	if err := json.Unmarshal([]byte(rest), &nodes); err != nil {
		t.Fatalf("what follows the CID is not JSON: %v\n%s", err, rest)
	}
	if len(nodes) != 4 || nodes[first]["job"] != id || nodes[linkOf(nodes[first]["manifest"])]["workload"] != "primes" {
		t.Errorf("printed:\n%s", printed)
	}
	if checked := mustCLI(t, "job", "record", "--verify", "--addr", addr, id); !strings.HasPrefix(checked, printed) || !strings.Contains(checked, "verified:") {
		t.Errorf("job record --verify printed:\n%s", checked)
	}
	stop()

	// The record is on disk, in the node's own store, pinned.
	store, err := storage.OpenLocal(filepath.Join(dataDir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	_, missing := store.GetNode(context.Background(), root)
	pins := jobRecordPins(store, id)
	store.Close()
	if missing != nil || len(pins) != 1 {
		t.Fatalf("with the node stopped, its store holds the record: %v, under %d pins", missing, len(pins))
	}

	// And the node, started again, gives the same record for the same job.
	again := freeAddr(t)
	startDaemon(t, "--listen", again, "--slots", "1", "--data-dir", dataDir)
	waitForOutput(t, "primes", "nodes", "--addr", again)
	if after := mustCLI(t, "job", "record", "--verify", "--addr", again, id); !strings.HasPrefix(after, printed) || !strings.Contains(after, "verified:") {
		t.Errorf("after a restart job record --verify printed:\n%s\nbefore it:\n%s", after, printed)
	}
	if got := mustCLI(t, "job", "get", "--addr", again, id); !strings.Contains(got, "succeeded") || !strings.Contains(got, "\n  record "+first+"\n") {
		t.Errorf("job get after a restart:\n%s", got)
	}

	for _, bad := range [][]string{
		{"job", "record", "--addr", again},
		{"job", "record", "--addr", again, "no-such-job"},
		{"job", "record", "--no-such-flag"},
		{"job", "record", "--data-dir", "\x00", id},
	} {
		if _, err := cli(t, bad...); err == nil {
			t.Errorf("%v succeeded", bad)
		}
	}
}

func TestCLIReportsARecordThatDoesNotMatchOrCannotBeRead(t *testing.T) {
	record := &pb.JobRecord{
		JobId: "j", RootCid: "bafyroot", RecomputedCid: "bafyother",
		Nodes: []*pb.RecordNode{{Cid: "bafyroot", Json: `{"job":"j"}`}, {Cid: "bafymanifest", Json: `{"workload":"primes"}`}},
	}
	addr := serveFake(t, func(srv *grpc.Server) { pb.RegisterNodeServiceServer(srv, scriptedRecord{record: record}) })
	// It is printed either way, and checking it says what is wrong.
	out, err := cli(t, "job", "record", "--addr", addr, "j")
	if err != nil || !strings.HasPrefix(out, "bafyroot\n{") || !strings.Contains(out, `"workload": "primes"`) {
		t.Errorf("job record printed %q, %v", out, err)
	}
	out, err = cli(t, "job", "record", "--verify", "--addr", addr, "j")
	if err == nil || !strings.Contains(err.Error(), "job j does not match its record") || !strings.Contains(err.Error(), "bafyother") || !strings.HasPrefix(out, "bafyroot\n") {
		t.Errorf("job record --verify of a record that does not match printed %q, error %v", out, err)
	}

	record.Nodes[1].Json = `{"workload":`
	if _, err := cli(t, "job", "record", "--addr", addr, "j"); err == nil || !strings.Contains(err.Error(), "not JSON") {
		t.Errorf("a record that is not JSON: %v", err)
	}
}

func TestANodeWithKuboKeepsRecordsWhereTheIPFSCommandCanReadThem(t *testing.T) {
	requireKubo(t)
	dataDir := t.TempDir()
	addr, _ := startNode(t, "--kubo", "--data-dir", dataDir)
	out := mustCLI(t, "job", "submit", "--addr", addr, "--params", `{"from":0,"to":1000}`)
	found := regexp.MustCompile(`job ([0-9a-f]+) succeeded`).FindStringSubmatch(out)
	if found == nil {
		t.Fatalf("no finished job in:\n%s", out)
	}
	printed := mustCLI(t, "job", "record", "--verify", "--addr", addr, found[1])
	root, _, _ := strings.Cut(printed, "\n")

	// Kubo reads the root as the node of linked data it is, and follows
	// its links by name.
	var viaKubo map[string]any
	if err := json.Unmarshal([]byte(ipfsIn(t, dataDir, "dag", "get", root)), &viaKubo); err != nil || viaKubo["job"] != found[1] || viaKubo["kind"] != "sisyphus-job-record" {
		t.Errorf("ipfs dag get of the record's root: %v, %v", viaKubo, err)
	}
	if got := strings.TrimSpace(ipfsIn(t, dataDir, "dag", "get", root+"/manifest/workload")); got != `"primes"` {
		t.Errorf("ipfs dag get of the manifest's workload through the root: %s", got)
	}
	if got := strings.TrimSpace(ipfsIn(t, dataDir, "dag", "get", root+"/receipts/0/attempts/0/state")); got != `"succeeded"` {
		t.Errorf("ipfs dag get of the first receipt's first attempt through the root: %s", got)
	}
	if got := strings.TrimSpace(ipfsIn(t, dataDir, "dag", "get", root+"/result/state")); got != `"succeeded"` {
		t.Errorf("ipfs dag get of the result's state through the root: %s", got)
	}
	// A collection leaves it where it is.
	mustCLI(t, "blob", "gc", "--addr", addr)
	if again := mustCLI(t, "job", "record", "--addr", addr, found[1]); !strings.HasPrefix(printed, again) {
		t.Errorf("after a collection the record reads:\n%s\nbefore it:\n%s", again, printed)
	}
}

// commitmentChecks returns what a node says of a job's commitments when
// given a key: how many it checked, and the names of those the key gave.
func (p *pool) commitmentChecks(jobID string, key []byte) (checked int, matched []string, record *pb.JobRecord) {
	p.t.Helper()
	record, err := p.client.GetJobRecord(p.ctx, &pb.GetJobRecordRequest{JobId: jobID, Verify: true, Key: key})
	if err != nil {
		p.t.Fatalf("the record of job %s, checked with a key: %v", jobID, err)
	}
	for _, check := range record.GetCommitments() {
		if check.GetMatches() {
			matched = append(matched, check.GetName())
		}
	}
	return len(record.GetCommitments()), matched, record
}

func TestAPrivateJobsRecordCommitsToWhatItLeavesOutAndIsTheSameAfterARestart(t *testing.T) {
	file := filepath.Join(t.TempDir(), "node.db")
	db := journalIn(t, file)
	before := startPoolWith(t, runtime.Builtin(), sameStore, db)
	stopWorker := before.startWorker("a", 2)
	before.waitForWorkers(1)
	key, other := sealed.NewKey(), sealed.NewKey()
	job := before.privateWordcount(sampleText(), key, 2)
	if job.GetState() != pb.JobState_JOB_STATE_SUCCEEDED || !job.GetPrivate() {
		t.Fatalf("job %v, private %v: %s", job.GetState(), job.GetPrivate(), job.GetError())
	}
	open := before.wait(before.submit(primesJob(pb.ScheduleMode_SCHEDULE_MODE_FULL_WORKER, 0)).GetJobId())

	// In place of each value it leaves out, the record has 32 bytes.
	written := before.recordOf(job.GetJobId())
	if !written.GetMatches() || len(written.GetCommitments()) != 0 {
		t.Fatalf("the record matches the job: %v; asked with no key it checked %d commitments", written.GetMatches(), len(written.GetCommitments()))
	}
	nodes := recordNodes(t, written)
	root := nodes[written.GetRootCid()]
	manifest, result := nodes[linkOf(root["manifest"])], nodes[linkOf(root["result"])]
	held := [][]byte{bytesOf(t, manifest["params_commitment"]), bytesOf(t, result["result_commitment"]), bytesOf(t, result["error_commitment"])}
	for _, link := range root["receipts"].([]any) {
		receipt := nodes[linkOf(link)]
		held = append(held, bytesOf(t, receipt["output_commitment"]))
		for _, attempt := range receipt["attempts"].([]any) {
			held = append(held, bytesOf(t, attempt.(map[string]any)["error_commitment"]))
		}
	}
	seen := make(map[string]bool)
	for _, commitment := range held {
		if len(commitment) != 32 || seen[string(commitment)] {
			t.Errorf("a commitment in the record is %x, want 32 bytes found nowhere else in it", commitment)
		}
		seen[string(commitment)] = true
	}
	// Two tasks, each done at the first attempt.
	if len(held) != 7 {
		t.Errorf("the record holds %d commitments, want 7", len(held))
	}

	// The job's key gives every one of them. Another key gives none, and
	// that says nothing against the record.
	if checked, matched, _ := before.commitmentChecks(job.GetJobId(), key[:]); checked != 7 || len(matched) != 7 {
		t.Errorf("with the job's key, %d of %d commitments match: %v", len(matched), checked, matched)
	}
	if checked, matched, record := before.commitmentChecks(job.GetJobId(), other[:]); checked != 7 || len(matched) != 0 || !record.GetMatches() {
		t.Errorf("with another key, %d of %d commitments match, and the record matches the job: %v", len(matched), checked, record.GetMatches())
	}
	// A job that is not private has none to check.
	if checked, _, record := before.commitmentChecks(open.GetJobId(), key[:]); checked != 0 || !record.GetMatches() {
		t.Errorf("a job that is not private had %d commitments checked", checked)
	}

	before.stopAndForget()
	stopWorker()
	db.Close()

	// The key is gone from the database, and the commitments are there.
	db = journalIn(t, file)
	saved, err := db.LoadJobs()
	if err != nil {
		t.Fatal(err)
	}
	if len(saved) != 2 || saved[0].Key != nil || len(saved[0].Commitments) != 7 || len(saved[1].Commitments) != 0 {
		t.Fatalf("saved: %d jobs, the private one with key %x and %d commitments", len(saved), saved[0].Key, len(saved[0].Commitments))
	}
	// A coordinator started on it, over a store that has never held the
	// record, works the same record out from the job without the key.
	after := startPoolWith(t, runtime.Builtin(), sameStore, db)
	again := after.recordOf(job.GetJobId())
	if !again.GetMatches() || again.GetRootCid() != written.GetRootCid() || after.job(job.GetJobId()).GetRecordCid() != written.GetRootCid() {
		t.Fatalf("after a restart the record is %s and matches the job: %v; it was %s", again.GetRootCid(), again.GetMatches(), written.GetRootCid())
	}
	for i, node := range again.GetNodes() {
		if !bytes.Equal(node.GetData(), written.GetNodes()[i].GetData()) {
			t.Errorf("node %d of the record differs after a restart", i)
		}
	}
	// Whoever still has the key can check it as before.
	if checked, matched, _ := after.commitmentChecks(job.GetJobId(), key[:]); checked != 7 || len(matched) != 7 {
		t.Errorf("after a restart, with the job's key, %d of %d commitments match: %v", len(matched), checked, matched)
	}
	if _, matched, _ := after.commitmentChecks(job.GetJobId(), other[:]); len(matched) != 0 {
		t.Errorf("after a restart, with another key, these match: %v", matched)
	}
	after.stopAndForget()

	// A result rewritten behind the coordinator's back leaves the record
	// as it was, since the record never held the result. The key finds it.
	tampered := restarted(t, storage.NewMemory(), rewriting{db, func(j *jobmodel.Job) { j.Result = []byte(`{"words":1}`) }})
	checked, err := tampered.Record(context.Background(), job.GetJobId(), true, key[:])
	if err != nil {
		t.Fatal(err)
	}
	var missed []string
	for _, check := range checked.GetCommitments() {
		if !check.GetMatches() {
			missed = append(missed, check.GetName())
		}
	}
	if !checked.GetMatches() || len(missed) != 1 || missed[0] != "result/result_commitment" {
		t.Errorf("with the result changed the record matches the job: %v, and these commitments do not match: %v", checked.GetMatches(), missed)
	}
}

func TestCLIChecksAPrivateJobsCommitmentsWithItsKey(t *testing.T) {
	dataDir := t.TempDir()
	addr, stop := startNode(t, "--data-dir", dataDir)
	keyFile, otherKeyFile := newKeyFile(t), newKeyFile(t)
	finishedJob := regexp.MustCompile(`job ([0-9a-f]+) succeeded`)
	found := finishedJob.FindStringSubmatch(mustCLI(t, "job", "submit", "--addr", addr, "--key-file", keyFile, "--params", `{"from":0,"to":1000}`))
	if found == nil {
		t.Fatal("the private job did not finish")
	}
	id := found[1]

	// Without the key the record is printed and checked against the job as
	// any other is.
	printed := mustCLI(t, "job", "record", "--verify", "--addr", addr, id)
	if !strings.Contains(printed, `"params_commitment"`) || !strings.Contains(printed, "verified:") || strings.Contains(printed, "matches") {
		t.Errorf("job record --verify of a private job printed:\n%s", printed)
	}
	// With it, each commitment is checked: those of the parameters, the one
	// task's output and its one attempt's error, the result and the error.
	checkedWithKey := func(addr string) {
		t.Helper()
		out := mustCLI(t, "job", "record", "--key-file", keyFile, "--addr", addr, id)
		if !strings.HasPrefix(out, printed) || !strings.Contains(out, "checked: the key and the values the node holds give all 5 commitments") {
			t.Errorf("job record --key-file printed:\n%s", out)
		}
		for _, name := range []string{"manifest/params_commitment", "receipts/0/output_commitment", "receipts/0/attempts/0/error_commitment", "result/result_commitment", "result/error_commitment"} {
			if !strings.Contains(out, "\n"+name+": matches\n") {
				t.Errorf("job record --key-file does not say that %s matches:\n%s", name, out)
			}
		}
	}
	checkedWithKey(addr)

	// A key that is not the job's is said not to match. The record is still
	// verified: nothing is wrong with it.
	out, err := cli(t, "job", "record", "--key-file", otherKeyFile, "--addr", addr, id)
	if err == nil || !strings.Contains(err.Error(), "5 of the 5 commitments in the record of job "+id+" do not match") || !strings.Contains(err.Error(), otherKeyFile) ||
		strings.Contains(err.Error(), "does not match its record") {
		t.Errorf("job record with another key: %v", err)
	}
	if !strings.HasPrefix(out, printed) || strings.Count(out, ": does not match\n") != 5 || strings.Contains(out, "checked:") {
		t.Errorf("job record with another key printed:\n%s", out)
	}

	// A job that is not private has nothing for a key to check.
	found = finishedJob.FindStringSubmatch(mustCLI(t, "job", "submit", "--addr", addr, "--params", `{"from":0,"to":1000}`))
	if found == nil {
		t.Fatal("the job that is not private did not finish")
	}
	if out := mustCLI(t, "job", "record", "--key-file", keyFile, "--addr", addr, found[1]); !strings.Contains(out, "verified:") || !strings.Contains(out, "nothing to check with a key") {
		t.Errorf("job record --key-file of a job that is not private printed:\n%s", out)
	}
	if _, err := cli(t, "job", "record", "--key-file", filepath.Join(dataDir, "no-such.key"), "--addr", addr, id); err == nil || !strings.Contains(err.Error(), "read key") {
		t.Errorf("job record with a key file that is not there: %v", err)
	}
	stop()

	// The node, started again, no longer has the key. It gives the same
	// record, and the key still checks it.
	again := freeAddr(t)
	startDaemon(t, "--listen", again, "--slots", "1", "--data-dir", dataDir)
	waitForOutput(t, "primes", "nodes", "--addr", again)
	if after := mustCLI(t, "job", "record", "--verify", "--addr", again, id); after != printed {
		t.Errorf("after a restart job record --verify printed:\n%s\nbefore it:\n%s", after, printed)
	}
	checkedWithKey(again)
}
