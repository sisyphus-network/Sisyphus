package jobrecord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multihash"

	jobmodel "github.com/sisyphus-network/Sisyphus/packages/job-model"
)

var (
	ctx       = context.Background()
	submitted = time.Date(2026, 10, 9, 12, 0, 0, 123456789, time.FixedZone("CEST", 2*3600))
)

// named is the CID a test gives a long value: the hash of its bytes, as a
// raw block.
func named(data []byte) cid.Cid {
	c, _ := cid.V1Builder{Codec: cid.Raw, MhType: multihash.SHA2_256}.Sum(data)
	return c
}

func blobCID(text string) string { return named([]byte(text)).String() }

// finished returns a job of two tasks that has run its course: the first
// task failed once on one worker and then succeeded on another.
func finished() *jobmodel.Job {
	job := jobmodel.New("j1", "wordcount", []byte(`{"input":"x"}`), jobmodel.Distributed, 2, [][]byte{[]byte("a"), []byte("b")}, submitted)
	job.TaskTimeout, job.MinMemory, job.MinGPUs, job.Submitter = 90*time.Second, 8<<30, 1, "12D3KooWsubmitter"
	job.NoteRead(blobCID("input"), "not-a-cid")
	job.Start(job.Tasks[0], "node-a", "alpha", submitted)
	job.Fail(job.Tasks[0], "disk full", 3, submitted)
	job.Start(job.Tasks[0], "node-b", "beta", submitted)
	job.Start(job.Tasks[1], "node-a", "alpha", submitted)
	job.NoteTaskOutput(blobCID("part"))
	job.Succeed(job.Tasks[0], []byte("first half"))
	job.Succeed(job.Tasks[1], []byte("second half"))
	job.NoteRead(blobCID("part"))
	job.NoteResult(blobCID("output"))
	job.Finish([]byte(`{"words":7}`), nil, submitted.Add(3*time.Second))
	return job
}

// memory is a place records are read from, kept in a map.
type memory map[cid.Cid][]byte

func (m memory) GetNode(_ context.Context, c cid.Cid) ([]byte, error) {
	data, ok := m[c]
	if !ok {
		return nil, errors.New("no such node")
	}
	return data, nil
}

func keep(record *Record) memory {
	m := make(memory)
	for _, node := range record.Nodes {
		m[node.CID] = node.Data
	}
	return m
}

// parsed returns a record's nodes as parsed DAG-JSON, by the name of the
// root's field that links each, with the root itself under "root" and
// receipts under "receipt 0" and so on.
func parsed(t *testing.T, record *Record) map[string]map[string]any {
	t.Helper()
	byCID := make(map[string]map[string]any)
	for _, node := range record.Nodes {
		var fields map[string]any
		if err := json.Unmarshal([]byte(node.JSON()), &fields); err != nil {
			t.Fatalf("node %s as JSON: %v\n%s", node.CID, err, node.JSON())
		}
		byCID[node.CID.String()] = fields
	}
	link := func(v any) string { return v.(map[string]any)["/"].(string) }
	root := byCID[record.Root.String()]
	out := map[string]map[string]any{"root": root}
	for _, field := range []string{"manifest", "result", "graph"} {
		if v, ok := root[field]; ok {
			out[field] = byCID[link(v)]
		}
	}
	for i, v := range root["receipts"].([]any) {
		out["receipt "+string(rune('0'+i))] = byCID[link(v)]
	}
	return out
}

func TestARecordHoldsWhatWasAskedWhoDidWhatAndWhatCameOfIt(t *testing.T) {
	record := Build(finished(), nil, named)
	if record.Root.Type() != cid.DagCBOR || !record.Nodes[0].CID.Equals(record.Root) || len(record.Nodes) != 5 || len(record.Blobs) != 0 {
		t.Fatalf("root %s, %d nodes and %d blobs; want a DAG-CBOR root first of 5 nodes", record.Root, len(record.Nodes), len(record.Blobs))
	}
	nodes := parsed(t, record)

	root := nodes["root"]
	if root["kind"] != Kind || root["version"] != float64(Version) || root["job"] != "j1" || root["private"] != false {
		t.Errorf("the root: %v", root)
	}
	if _, has := root["parent"]; has {
		t.Errorf("a job that is no step names a parent: %v", root)
	}
	if _, has := root["graph"]; has {
		t.Errorf("a job with no steps has a graph: %v", root)
	}

	manifest := nodes["manifest"]
	wantManifest := `{"created_at":"2026-10-09T10:00:00.123456789Z","inputs":[{"/":"` + blobCID("input") + `"}],"mode":"distributed",` +
		`"params":{"/":{"bytes":"eyJpbnB1dCI6IngifQ"}},"requirements":{"max_tasks":2,"min_gpus":1,"min_memory_bytes":8589934592,"task_timeout_seconds":90},` +
		`"submitter":"12D3KooWsubmitter","workload":"wordcount"}`
	if got, _ := json.Marshal(manifest); string(got) != wantManifest {
		t.Errorf("the manifest:\n%s\nwant:\n%s", got, wantManifest)
	}

	wantReceipt := `{"attempts":[{"attempt":1,"error":"disk full","name":"alpha","node":"node-a","state":"pending"},` +
		`{"attempt":2,"name":"beta","node":"node-b","state":"succeeded"}],"failures":1,"output":{"/":{"bytes":"Zmlyc3QgaGFsZg"}},"state":"succeeded","task":0}`
	if got, _ := json.Marshal(nodes["receipt 0"]); string(got) != wantReceipt {
		t.Errorf("the first task's receipt:\n%s\nwant:\n%s", got, wantReceipt)
	}
	if nodes["receipt 1"]["task"] != float64(1) {
		t.Errorf("the second receipt: %v", nodes["receipt 1"])
	}

	wantResult := `{"finished_at":"2026-10-09T10:00:03.123456789Z","intermediate":[{"/":"` + blobCID("part") + `"}],` +
		`"outputs":[{"/":"` + blobCID("output") + `"}],"result":{"/":{"bytes":"eyJ3b3JkcyI6N30"}},"state":"succeeded"}`
	if got, _ := json.Marshal(nodes["result"]); string(got) != wantResult {
		t.Errorf("the result:\n%s\nwant:\n%s", got, wantResult)
	}
}

func TestTheSameJobAlwaysGivesTheSameRecordAndAnyChangeGivesAnother(t *testing.T) {
	first, second := Build(finished(), nil, named), Build(finished(), nil, named)
	if !first.Root.Equals(second.Root) {
		t.Fatalf("the same job built twice gave %s and %s", first.Root, second.Root)
	}
	for i := range first.Nodes {
		if !bytes.Equal(first.Nodes[i].Data, second.Nodes[i].Data) {
			t.Errorf("node %d differs between two builds of the same job", i)
		}
	}
	// Where the clock was read makes no difference: it is the moment
	// that is recorded.
	elsewhere := finished()
	elsewhere.CreatedAt = elsewhere.CreatedAt.In(time.UTC)
	if got := Build(elsewhere, nil, named); !got.Root.Equals(first.Root) {
		t.Error("the same moment in another time zone gave another record")
	}

	changes := map[string]func(*jobmodel.Job){
		"another worker in a receipt":       func(j *jobmodel.Job) { j.Tasks[1].History[0].NodeID = "node-c" },
		"another name in a receipt":         func(j *jobmodel.Job) { j.Tasks[1].History[0].NodeName = "gamma" },
		"another error in an earlier try":   func(j *jobmodel.Job) { j.Tasks[0].History[0].Err = "disk on fire" },
		"an attempt left out":               func(j *jobmodel.Job) { j.Tasks[0].History = j.Tasks[0].History[1:] },
		"another output in a receipt":       func(j *jobmodel.Job) { j.Tasks[1].Output = []byte("second hal") },
		"another state in a receipt":        func(j *jobmodel.Job) { j.Tasks[1].State = jobmodel.Failed },
		"another count of failures":         func(j *jobmodel.Job) { j.Tasks[1].Failures = 1 },
		"another result":                    func(j *jobmodel.Job) { j.Result = []byte(`{"words":8}`) },
		"another outcome":                   func(j *jobmodel.Job) { j.State, j.Err = jobmodel.Failed, "it did not work" },
		"another finish time":               func(j *jobmodel.Job) { j.FinishedAt = j.FinishedAt.Add(time.Nanosecond) },
		"other parameters":                  func(j *jobmodel.Job) { j.Params = []byte(`{"input":"y"}`) },
		"another workload":                  func(j *jobmodel.Job) { j.Workload = "primes" },
		"another mode":                      func(j *jobmodel.Job) { j.Mode = jobmodel.FullWorker },
		"another requirement":               func(j *jobmodel.Job) { j.MinGPUs = 2 },
		"another submitter":                 func(j *jobmodel.Job) { j.Submitter = "" },
		"another input":                     func(j *jobmodel.Job) { j.NoteRead(blobCID("more input")) },
		"another output blob":               func(j *jobmodel.Job) { j.NoteResult(blobCID("more output")) },
		"another ID":                        func(j *jobmodel.Job) { j.ID = "j2" },
		"being a step of another job":       func(j *jobmodel.Job) { j.Parent, j.Step = "j0", "count" },
		"being private":                     func(j *jobmodel.Job) { j.Private = true },
		"a task its job ended without":      func(j *jobmodel.Job) { j.Tasks = j.Tasks[:1] },
		"an attempt whose task was dropped": func(j *jobmodel.Job) { j.Tasks[1].History[0].State = jobmodel.Cancelled },
	}
	seen := map[string]string{first.Root.String(): "the job unchanged"}
	for what, change := range changes {
		job := finished()
		change(job)
		root := Build(job, nil, named).Root.String()
		if other, dup := seen[root]; dup {
			t.Errorf("%s gave the same record as %s", what, other)
		}
		seen[root] = what
	}
}

func TestAPrivateJobsRecordLeavesOutWhatWasNotSealed(t *testing.T) {
	job := finished()
	job.Private = true
	job.Params, job.Result = []byte("SECRET parameters"), []byte("SECRET result")
	job.Tasks[0].Output, job.Tasks[0].History[0].Err = []byte("SECRET output"), "SECRET in an error"
	job.Tasks[1].Output = bytes.Repeat([]byte("SECRET "), InlineLimit)
	job.State, job.Err = jobmodel.Failed, "SECRET in the job's error"

	record := Build(job, nil, named)
	if len(record.Blobs) != 0 {
		t.Errorf("a private job's record stores %d values beside it", len(record.Blobs))
	}
	for _, node := range record.Nodes {
		if bytes.Contains(node.Data, []byte("SECRET")) || strings.Contains(node.JSON(), "U0VDUkVU") {
			t.Errorf("node %s of a private job's record holds plaintext: %s", node.CID, node.JSON())
		}
	}
	nodes := parsed(t, record)
	if nodes["root"]["private"] != true {
		t.Errorf("the root of a private job's record: %v", nodes["root"])
	}
	// What was sealed, or was never secret, is still there.
	if len(nodes["manifest"]["inputs"].([]any)) != 1 || len(nodes["result"]["outputs"].([]any)) != 1 || nodes["result"]["state"] != "failed" {
		t.Errorf("a private job's manifest %v and result %v", nodes["manifest"], nodes["result"])
	}
	if attempts := nodes["receipt 0"]["attempts"].([]any); len(attempts) != 2 || attempts[0].(map[string]any)["node"] != "node-a" {
		t.Errorf("a private job's receipt: %v", nodes["receipt 0"])
	}
}

func TestAValueTooLongForANodeIsLinkedAndHandedBack(t *testing.T) {
	job := finished()
	long := bytes.Repeat([]byte("x"), InlineLimit+1)
	job.Params = long
	job.Tasks[0].Output = long[:InlineLimit] // just short enough to stay
	job.Result = nil                         // and nothing at all

	record := Build(job, nil, named)
	if len(record.Blobs) != 1 || !record.Blobs[0].CID.Equals(named(long)) || !bytes.Equal(record.Blobs[0].Data, long) {
		t.Fatalf("values handed back to be stored: %d, want the one long value under its CID", len(record.Blobs))
	}
	nodes := parsed(t, record)
	if link, _ := nodes["manifest"]["params"].(map[string]any)["/"].(string); link != named(long).String() {
		t.Errorf("long parameters in the manifest: %v", nodes["manifest"]["params"])
	}
	if _, inline := nodes["receipt 0"]["output"].(map[string]any)["/"].(map[string]any); !inline {
		t.Errorf("an output at the limit is not held in its receipt: %v", nodes["receipt 0"]["output"])
	}
	if _, has := nodes["result"]["result"]; has {
		t.Errorf("an empty result is in the record: %v", nodes["result"])
	}
}

func TestAStepNamesItsParentAndAGraphLinksItsStepsRecords(t *testing.T) {
	step := finished()
	step.Parent, step.Step = "g1", "count"
	stepRecord := Build(step, nil, named)
	if parent := parsed(t, stepRecord)["root"]["parent"].(map[string]any); parent["job"] != "g1" || parent["step"] != "count" || len(parent) != 2 {
		t.Errorf("a step's parent: %v", parent)
	}

	graphJob := finished()
	graphJob.ID = "g1"
	steps := []Step{
		{Name: "count", Job: "j1", Record: stepRecord.Root.String()},
		{Name: "stopped", Job: "j2"}, // it has no record
	}
	record := Build(graphJob, steps, named)
	if len(record.Nodes) != 6 {
		t.Fatalf("a graph's record has %d nodes, want the usual five and the graph", len(record.Nodes))
	}
	listed := parsed(t, record)["graph"]["steps"].([]any)
	first, second := listed[0].(map[string]any), listed[1].(map[string]any)
	if first["step"] != "count" || first["job"] != "j1" || first["record"].(map[string]any)["/"] != stepRecord.Root.String() {
		t.Errorf("the first step in the graph: %v", first)
	}
	if _, has := second["record"]; has || second["job"] != "j2" {
		t.Errorf("a step with no record: %v", second)
	}
	// A step's record changing changes the graph's.
	steps[0].Record = Build(finished(), nil, named).Root.String()
	if again := Build(graphJob, steps, named); again.Root.Equals(record.Root) {
		t.Error("another record for a step left the graph's record as it was")
	}
}

func TestReadReturnsTheRootAndTheNodesItLinksAndNoFurther(t *testing.T) {
	step := Build(finished(), nil, named)
	record := Build(finished(), []Step{{Name: "count", Job: "j1", Record: step.Root.String()}}, named)
	// The step's record is not in the store at all.
	store := keep(record)

	nodes, err := Read(ctx, store, record.Root)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != len(record.Nodes) {
		t.Fatalf("read %d nodes, built %d", len(nodes), len(record.Nodes))
	}
	for i := range nodes {
		if !nodes[i].CID.Equals(record.Nodes[i].CID) || !bytes.Equal(nodes[i].Data, record.Nodes[i].Data) {
			t.Errorf("node %d was read as %s, built as %s", i, nodes[i].CID, record.Nodes[i].CID)
		}
	}

	if _, err := Read(ctx, memory{}, record.Root); err == nil {
		t.Error("read a record from a store that does not hold it")
	}
	delete(store, record.Nodes[2].CID)
	if _, err := Read(ctx, store, record.Root); err == nil {
		t.Error("read a record with a node missing")
	}
	store[record.Root] = []byte("\xff not a node")
	if _, err := Read(ctx, store, record.Root); err == nil || !strings.Contains(err.Error(), "not DAG-CBOR") {
		t.Errorf("reading a root that does not decode: %v", err)
	}
	if text := (Node{Data: []byte("\xff not a node")}).JSON(); text != "null" {
		t.Errorf("bytes that are no node, as JSON: %q", text)
	}
}
