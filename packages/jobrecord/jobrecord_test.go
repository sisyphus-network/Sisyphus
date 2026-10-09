package jobrecord

import (
	"bytes"
	"context"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
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

// jobKey and otherKey are sealing keys.
var (
	jobKey   = []byte("0123456789abcdef0123456789abcdef")
	otherKey = []byte("fedcba9876543210fedcba9876543210")
)

// secret returns a private job that has ended, with a secret in every value
// its record leaves out, and its commitments not yet made.
func secret() *jobmodel.Job {
	job := finished()
	job.Private = true
	job.Params, job.Result = []byte("SECRET parameters"), []byte("SECRET result")
	job.Tasks[0].Output, job.Tasks[0].History[0].Err = []byte("SECRET output"), "SECRET in an error"
	job.Tasks[1].Output = bytes.Repeat([]byte("SECRET "), InlineLimit)
	job.State, job.Err = jobmodel.Failed, "SECRET in the job's error"
	return job
}

// commitmentIn returns the commitment a parsed node holds under a field.
func commitmentIn(t *testing.T, node map[string]any, field string) []byte {
	t.Helper()
	held, _ := node[field].(map[string]any)["/"].(map[string]any)
	text, _ := held["bytes"].(string)
	data, err := base64.RawStdEncoding.DecodeString(text)
	if err != nil || len(data) != sha256.Size {
		t.Fatalf("%s is %v, want the 32 bytes of a commitment: %v", field, node[field], err)
	}
	return data
}

func TestAPrivateJobsRecordLeavesOutWhatWasNotSealed(t *testing.T) {
	// As a private job that ended before commitments were made, and as one
	// that has them.
	without, with := secret(), secret()
	with.Commitments = Commit(with, jobKey)
	for _, job := range []*jobmodel.Job{without, with} {
		record := Build(job, nil, named)
		if len(record.Blobs) != 0 {
			t.Errorf("a private job's record stores %d values beside it", len(record.Blobs))
		}
		for _, node := range record.Nodes {
			if bytes.Contains(node.Data, []byte("SECRET")) || strings.Contains(node.JSON(), "U0VDUkVU") || bytes.Contains(node.Data, jobKey) {
				t.Errorf("node %s of a private job's record holds plaintext: %s", node.CID, node.JSON())
			}
		}
		nodes := parsed(t, record)
		for name, fields := range map[string][]string{"manifest": {"params"}, "result": {"result", "error"}, "receipt 0": {"output"}, "receipt 1": {"output"}} {
			for _, field := range fields {
				if _, has := nodes[name][field]; has {
					t.Errorf("the %s of a private job's record has %s: %v", name, field, nodes[name])
				}
				if _, has := nodes[name][field+"_commitment"]; has != (job == with) {
					t.Errorf("the %s of the record of a private job with %d commitments has %s_commitment: %v", name, len(job.Commitments), field, has)
				}
			}
		}
		for _, attempt := range nodes["receipt 0"]["attempts"].([]any) {
			if _, has := attempt.(map[string]any)["error"]; has {
				t.Errorf("an attempt in a private job's record has its error: %v", attempt)
			}
			if _, has := attempt.(map[string]any)["error_commitment"]; has != (job == with) {
				t.Errorf("an attempt in the record of a private job with %d commitments has error_commitment: %v", len(job.Commitments), has)
			}
		}
	}

	// Each commitment is in the record where its name says.
	committed := parsed(t, Build(with, nil, named))
	attempts := committed["receipt 0"]["attempts"].([]any)
	for name, got := range map[string][]byte{
		"manifest/params_commitment":             commitmentIn(t, committed["manifest"], "params_commitment"),
		"receipts/0/output_commitment":           commitmentIn(t, committed["receipt 0"], "output_commitment"),
		"receipts/0/attempts/0/error_commitment": commitmentIn(t, attempts[0].(map[string]any), "error_commitment"),
		"receipts/0/attempts/1/error_commitment": commitmentIn(t, attempts[1].(map[string]any), "error_commitment"),
		"receipts/1/output_commitment":           commitmentIn(t, committed["receipt 1"], "output_commitment"),
		"result/result_commitment":               commitmentIn(t, committed["result"], "result_commitment"),
		"result/error_commitment":                commitmentIn(t, committed["result"], "error_commitment"),
	} {
		if !bytes.Equal(got, with.Commitments[name]) {
			t.Errorf("the record holds %x where the job's commitment %s is %x", got, name, with.Commitments[name])
		}
	}
	// The record is the job's with its commitments, whether or not the job
	// still has its key, and another with other commitments.
	with.Key = jobKey
	keyed := Build(with, nil, named)
	with.Key = nil
	if again := Build(with, nil, named); !again.Root.Equals(keyed.Root) {
		t.Error("a private job gave another record once its key was gone")
	}
	with.Commitments = Commit(with, otherKey)
	if other := Build(with, nil, named); other.Root.Equals(keyed.Root) {
		t.Error("other commitments left a private job's record as it was")
	}

	record := Build(without, nil, named)
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

func TestCommitmentsAreTheSameEveryTimeAndDifferWithTheKeyTheValueAndTheJob(t *testing.T) {
	first, second := Commit(secret(), jobKey), Commit(secret(), jobKey)
	// Parameters, a result and an error, two outputs, and three attempts.
	if len(first) != 8 {
		t.Fatalf("a job of two tasks and three attempts has %d commitments, want 8: %v", len(first), first)
	}
	for name, commitment := range first {
		if len(commitment) != sha256.Size || !bytes.Equal(commitment, second[name]) {
			t.Errorf("%s was %x and then %x", name, commitment, second[name])
		}
	}

	// A commitment is an HMAC-SHA-256 under a key derived from the job's,
	// of the job's ID, the commitment's name and the value.
	derived, err := hkdf.Key(sha256.New, jobKey, nil, "sisyphus job record: commitments", 32)
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, derived)
	mac.Write([]byte("\x00\x00\x00\x00\x00\x00\x00\x02j1" + "\x00\x00\x00\x00\x00\x00\x00\x18result/result_commitment" + "SECRET result"))
	if want := mac.Sum(nil); !bytes.Equal(first["result/result_commitment"], want) {
		t.Errorf("the commitment to the result is %x, want %x", first["result/result_commitment"], want)
	}
	// Not one under the sealing key itself, nor a plain hash.
	direct := hmac.New(sha256.New, jobKey)
	direct.Write([]byte("SECRET result"))
	plain := sha256.Sum256([]byte("SECRET result"))
	for name, commitment := range first {
		if bytes.Equal(commitment, direct.Sum(nil)) || bytes.Equal(commitment, plain[:]) {
			t.Errorf("%s can be had without deriving a key: %x", name, commitment)
		}
	}

	// Another key changes every commitment, and so does another job, even
	// one with the same values.
	otherJob := secret()
	otherJob.ID = "j2"
	for what, other := range map[string]map[string][]byte{"another key": Commit(secret(), otherKey), "another job": Commit(otherJob, jobKey)} {
		for name, commitment := range first {
			if bytes.Equal(commitment, other[name]) {
				t.Errorf("with %s, %s is still %x", what, name, commitment)
			}
		}
	}
	// Another value changes its own commitment and no other.
	changes := map[string]func(*jobmodel.Job){
		"manifest/params_commitment":             func(j *jobmodel.Job) { j.Params = []byte("SECRET parameterz") },
		"receipts/0/output_commitment":           func(j *jobmodel.Job) { j.Tasks[0].Output = nil },
		"receipts/0/attempts/0/error_commitment": func(j *jobmodel.Job) { j.Tasks[0].History[0].Err = "" },
		"receipts/0/attempts/1/error_commitment": func(j *jobmodel.Job) { j.Tasks[0].History[1].Err = "late" },
		"receipts/1/output_commitment":           func(j *jobmodel.Job) { j.Tasks[1].Output = []byte("SECRET") },
		"receipts/1/attempts/0/error_commitment": func(j *jobmodel.Job) { j.Tasks[1].History[0].Err = "late" },
		"result/result_commitment":               func(j *jobmodel.Job) { j.Result = []byte(`{"count":25}`) },
		"result/error_commitment":                func(j *jobmodel.Job) { j.Err = "" },
	}
	for changed, change := range changes {
		job := secret()
		change(job)
		for name, commitment := range Commit(job, jobKey) {
			if same := bytes.Equal(commitment, first[name]); same == (name == changed) {
				t.Errorf("with another value at %s, the commitment %s is the same: %v", changed, name, same)
			}
		}
	}
	// Equal values in two places of one job do not give equal commitments,
	// and no two commitments of a job are equal.
	twins := secret()
	twins.Tasks[0].Output, twins.Tasks[1].Output = []byte("the same"), []byte("the same")
	twins.Result, twins.Params = []byte("the same"), []byte("the same")
	seen := make(map[string]string)
	for name, commitment := range Commit(twins, jobKey) {
		if other, dup := seen[string(commitment)]; dup {
			t.Errorf("%s and %s have the same commitment", name, other)
		}
		seen[string(commitment)] = name
	}
}

func TestOnlyTheJobsKeyGivesItsCommitments(t *testing.T) {
	job := secret()
	job.Commitments = Commit(job, jobKey)
	names := []string{
		"manifest/params_commitment",
		"receipts/0/output_commitment", "receipts/0/attempts/0/error_commitment", "receipts/0/attempts/1/error_commitment",
		"receipts/1/output_commitment", "receipts/1/attempts/0/error_commitment",
		"result/result_commitment", "result/error_commitment",
	}
	matching := func(checks []Check) (matched []string) {
		t.Helper()
		if len(checks) != len(names) {
			t.Fatalf("%d checks, want one for each of the %d commitments: %v", len(checks), len(names), checks)
		}
		for i, check := range checks {
			if check.Name != names[i] {
				t.Errorf("check %d is of %s, want %s: the order the record has them", i, check.Name, names[i])
			}
			if check.Matches {
				matched = append(matched, check.Name)
			}
		}
		return matched
	}
	if matched := matching(CheckCommitments(job, jobKey)); len(matched) != len(names) {
		t.Errorf("with the job's key only %v match", matched)
	}
	if matched := matching(CheckCommitments(job, otherKey)); len(matched) != 0 {
		t.Errorf("with another key %v match", matched)
	}
	// A value changed since the job ended is found, and it alone.
	job.Result = []byte("SECRET result, improved")
	if matched := matching(CheckCommitments(job, jobKey)); len(matched) != len(names)-1 || slices.Contains(matched, "result/result_commitment") {
		t.Errorf("with the result changed, %v match", matched)
	}
	// So is a commitment that has gone.
	job = secret()
	job.Commitments = Commit(job, jobKey)
	delete(job.Commitments, "receipts/1/output_commitment")
	if matched := matching(CheckCommitments(job, jobKey)); len(matched) != len(names)-1 || slices.Contains(matched, "receipts/1/output_commitment") {
		t.Errorf("with a commitment missing, %v match", matched)
	}

	// A job that is not private carries none, and neither does a private
	// one from before they were made.
	if checks := CheckCommitments(finished(), jobKey); checks != nil {
		t.Errorf("a job that is not private has commitments to check: %v", checks)
	}
	if checks := CheckCommitments(secret(), jobKey); checks != nil {
		t.Errorf("a private job with no commitments has some to check: %v", checks)
	}
}

func TestTheRecordOfAVerifiedJobSaysWhoReturnedWhatAndWhoAgreed(t *testing.T) {
	plain := finished()
	job := jobmodel.New("verified", "primes", []byte(`{"to":10}`), jobmodel.Distributed, 2, [][]byte{nil, nil}, submitted)
	job.Check(2, 0, []int{0, 1})
	settled, disputed := job.Tasks[0], job.Tasks[1]
	for _, node := range []string{"a", "b", "c"} {
		job.StartCopy(settled, "node-"+node, "worker-"+node, submitted)
	}
	job.ReturnCopy(settled, 1, []byte("4"), nil)
	job.ReturnCopy(settled, 2, []byte("5"), nil)
	job.ReturnCopy(settled, 3, []byte("4"), nil)
	job.StartCopy(disputed, "node-a", "worker-a", submitted)
	job.StartCopy(disputed, "node-b", "worker-b", submitted)
	job.ReturnCopy(disputed, 1, []byte("x"), nil)
	job.ReturnCopy(disputed, 2, []byte("y"), nil)
	job.Dispute(disputed, "task 1 could not be verified", submitted.Add(time.Minute))

	record := Build(job, nil, named)
	nodes := parsed(t, record)
	manifest, first, second := nodes["manifest"], nodes["receipt 0"], nodes["receipt 1"]
	if got := manifest["requirements"].(map[string]any)["verify"]; got != float64(2) {
		t.Errorf("the manifest says the job was verified by %v", got)
	}
	// Each result by who returned it, with a digest the same for results
	// that are the same, and whether the task was settled by it.
	results := first["results"].([]any)
	if len(results) != 3 {
		t.Fatalf("the settled task's results: %v", results)
	}
	var digests []string
	for i, want := range []struct {
		node   string
		agreed bool
	}{{"node-a", true}, {"node-b", false}, {"node-c", true}} {
		r := results[i].(map[string]any)
		if r["attempt"] != float64(i+1) || r["node"] != want.node || r["name"] != "worker-"+want.node[5:] || r["agreed"] != want.agreed {
			t.Errorf("result %d: %v", i, r)
		}
		digests = append(digests, r["digest"].(string))
	}
	if digests[0] != digests[2] || digests[0] == digests[1] || digests[0] != settled.Results[0].Digest() {
		t.Errorf("digests of the results: %v", digests)
	}
	// A task nobody agreed on has its results, and none that settled it.
	for _, r := range second["results"].([]any) {
		if r.(map[string]any)["agreed"] != false {
			t.Errorf("a result of the task that was never settled: %v", r)
		}
	}
	if second["state"] != "failed" || len(second["results"].([]any)) != 2 {
		t.Errorf("the receipt of the task that was never settled: %v", second)
	}

	// The same job gives the same record, and a result changed another.
	if again := Build(job, nil, named); !again.Root.Equals(record.Root) {
		t.Error("the same verified job gave two records")
	}
	settled.Results[1].NodeID = "node-z"
	if changed := Build(job, nil, named); changed.Root.Equals(record.Root) {
		t.Error("changing who returned a result did not change the record")
	}

	// A job that was not verified has none of this in its record.
	for _, node := range parsed(t, Build(plain, nil, named)) {
		if node["results"] != nil {
			t.Errorf("a receipt of a job that was not verified has results: %v", node)
		}
		if requirements, is := node["requirements"].(map[string]any); is && requirements["verify"] != nil {
			t.Errorf("the manifest of a job that was not verified: %v", node)
		}
	}
}
