package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ipfs/go-cid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/blobclient"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
	"github.com/sisyphus-network/Sisyphus/packages/sealed"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// stored returns the bytes a store holds under a CID, as any member of the
// pool could fetch them.
func stored(t *testing.T, store *storage.Store, c cid.Cid) []byte {
	t.Helper()
	blob, err := store.Open(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	defer blob.Close()
	data, err := io.ReadAll(blob)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// privateWordcount uploads text sealed with key and counts its words in a
// job that has the key.
func (p *pool) privateWordcount(text string, key sealed.Key, tasks uint32) *pb.Job {
	p.t.Helper()
	input, err := blobclient.Upload(p.ctx, p.blobs, sealed.Encrypt(key, strings.NewReader(text)))
	if err != nil {
		p.t.Fatal(err)
	}
	return p.wait(p.submit(&pb.JobSpec{
		Workload: "wordcount", Params: []byte(`{"input":"` + input.String() + `"}`), MaxTasks: tasks, Key: key[:],
	}).GetJobId())
}

func TestPrivateJobLeavesOnlySealedDataInThePool(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	p.startWorker("a", 2)
	p.startWorker("b", 2)
	p.waitForWorkers(2)
	key := sealed.NewKey()
	text := sampleText()

	job := p.privateWordcount(text, key, 4)
	if job.GetState() != pb.JobState_JOB_STATE_SUCCEEDED {
		t.Fatalf("job %v: %s", job.GetState(), job.GetError())
	}
	// The job's public state does not carry its key.
	if len(job.GetSpec().GetKey()) != 0 {
		t.Error("the job, as reported, includes its key")
	}

	// Everything the job put in any store of the pool is sealed: its input,
	// what its tasks passed on, and its result. None of it contains a word
	// of the text.
	stores := map[string]*storage.Store{"coordinator": p.store, "worker a": p.workerStores["a"], "worker b": p.workerStores["b"]}
	checked := 0
	for name, store := range stores {
		for _, pin := range store.Pins() {
			if pin.CID.Type() == cid.DagCBOR {
				continue // the job's record, which is no blob and has a test of its own
			}
			data := stored(t, store, pin.CID)
			if !sealed.IsSealed(data) {
				t.Errorf("%s holds a blob that is not sealed: %s", name, pin.CID)
			}
			if bytes.Contains(data, []byte("Sisyphus")) || bytes.Contains(data, []byte("boulder")) {
				t.Errorf("%s holds a blob with the text readable in it: %s", name, pin.CID)
			}
			checked++
		}
	}
	if checked < 6 {
		t.Fatalf("only %d blobs were found to check; the job should have left an input, four partial counts and a result", checked)
	}

	// With the key, the result is what an ordinary run of the same job gives.
	var result runtime.WordCountResult
	if err := json.Unmarshal(job.GetResult(), &result); err != nil {
		t.Fatal(err)
	}
	_, wantTable := wordCountLocally(t, text)
	table := stored(t, p.store, cid.MustParse(result.Output))
	opened, err := sealed.Open(key, bytes.NewReader(table), uint64(len(table)))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := io.ReadAll(opened); err != nil || string(got) != wantTable {
		t.Errorf("the unsealed result differs from an ordinary run's (%v)", err)
	}

	// And it is the same sealed blob however the job is split, so two runs
	// can still be compared by CID.
	again := p.privateWordcount(text, key, 2)
	if string(again.GetResult()) != string(job.GetResult()) {
		t.Errorf("the same private job split differently gave %s, then %s", job.GetResult(), again.GetResult())
	}
}

func TestPrivateJobWithTheWrongKeyFailsRatherThanComputeOnNoise(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	p.startWorker("a", 2)
	p.waitForWorkers(1)
	input, err := blobclient.Upload(p.ctx, p.blobs, sealed.Encrypt(sealed.NewKey(), strings.NewReader(sampleText())))
	if err != nil {
		t.Fatal(err)
	}
	wrong := sealed.NewKey()

	job := p.wait(p.submit(&pb.JobSpec{Workload: "wordcount", Params: []byte(`{"input":"` + input.String() + `"}`), MaxTasks: 2, Key: wrong[:]}).GetJobId())
	if job.GetState() != pb.JobState_JOB_STATE_FAILED || !strings.Contains(job.GetError(), "sealed with another key") {
		t.Errorf("job %v: %q, want it failed for the key", job.GetState(), job.GetError())
	}
}

func TestJobKeyMustBeAKey(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	_, err := p.client.SubmitJob(p.ctx, &pb.SubmitJobRequest{Spec: &pb.JobSpec{Workload: "primes", Params: []byte(`{"from":0,"to":10}`), Key: []byte("too short")}})
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "must be 32 bytes") {
		t.Errorf("error %v, want the key refused", err)
	}
}

func newKeyFile(t *testing.T) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "job.key")
	mustCLI(t, "key", "new", file)
	return file
}

func TestKeyNewMakesAKeyFileAndNeverReplacesOne(t *testing.T) {
	file := newKeyFile(t)
	info, err := os.Stat(file)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v, mode %v", err, info.Mode().Perm())
	}
	first, _ := os.ReadFile(file)
	if _, err := sealed.ParseKey(strings.TrimSpace(string(first))); err != nil {
		t.Errorf("the file does not hold a key: %v", err)
	}
	if other, _ := os.ReadFile(newKeyFile(t)); string(other) == string(first) {
		t.Error("two key files hold the same key")
	}

	if _, err := cli(t, "key", "new", file); err == nil || !strings.Contains(err.Error(), "create key file") {
		t.Errorf("overwriting a key file: %v, want a refusal", err)
	}
	if after, _ := os.ReadFile(file); string(after) != string(first) {
		t.Error("the refused attempt changed the key")
	}
	for _, args := range [][]string{{"key"}, {"key", "new"}, {"key", "rotate", file}, {"key", "new", file, "extra"}} {
		if _, err := cli(t, args...); err == nil || !strings.Contains(err.Error(), `expected "key new <file>"`) {
			t.Errorf("%v: error %v", args, err)
		}
	}
}

func TestPrivateJobFromTheCommandLine(t *testing.T) {
	addr := freeAddr(t)
	coordinatorDir := t.TempDir()
	startDaemon(t, "--role", "coordinator", "--listen", addr, "--data-dir", coordinatorDir)
	startDaemon(t, "--role", "worker", "--coordinator", addr, "--name", "hand", "--slots", "2")
	waitForOutput(t, "hand", "nodes", "--addr", addr)
	key := newKeyFile(t)
	text := strings.Repeat("the boulder's whereabouts are confidential.\n", 20_000)

	input := strings.TrimSpace(mustCLI(t, "blob", "put", "--addr", addr, "--key-file", key, writeFile(t, text)))
	out := mustCLI(t, "job", "submit", "--addr", addr, "--workload", "wordcount", "--tasks", "4", "--key-file", key, "--params", `{"input":"`+input+`"}`)
	result := regexp.MustCompile(`"output":"([a-z0-9]+)"`).FindStringSubmatch(out)
	// Six words a line: "boulder's" is two.
	if result == nil || !strings.Contains(out, `"words":120000`) {
		t.Fatalf("job output:\n%s", out)
	}

	// Without the key, what comes back is sealed, input and result alike.
	for name, id := range map[string]string{"input": input, "result": result[1]} {
		raw := mustCLI(t, "blob", "get", "--addr", addr, id)
		if !sealed.IsSealed([]byte(raw)) || strings.Contains(raw, "confidential") {
			t.Errorf("the %s, fetched without the key, is readable", name)
		}
	}
	// With it, the table, to standard output or a file.
	wantTop := "20000\tare\n20000\tboulder\n20000\tconfidential\n"
	if table := mustCLI(t, "blob", "get", "--addr", addr, "--key-file", key, result[1]); !strings.HasPrefix(table, wantTop) {
		t.Errorf("the unsealed result starts:\n%.60s", table)
	}
	saved := filepath.Join(t.TempDir(), "table.txt")
	mustCLI(t, "blob", "get", "--addr", addr, "--key-file", key, "-o", saved, result[1])
	if table, _ := os.ReadFile(saved); !strings.HasPrefix(string(table), wantTop) {
		t.Errorf("the unsealed result saved to a file starts:\n%.60s", table)
	}
	if back := mustCLI(t, "blob", "get", "--addr", addr, "--key-file", key, input); back != text {
		t.Error("the input, fetched with the key, is not what was stored")
	}

	// Another key opens nothing, and leaves nothing behind.
	target := filepath.Join(t.TempDir(), "out")
	if _, err := cli(t, "blob", "get", "--addr", addr, "--key-file", newKeyFile(t), "-o", target, result[1]); err == nil || !strings.Contains(err.Error(), "sealed with another key") {
		t.Errorf("fetching with another key: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("a failed unsealing left a file (stat: %v)", err)
	}
	// A key is refused for a blob that was never sealed.
	plain := strings.TrimSpace(mustCLI(t, "blob", "put", "--addr", addr, writeFile(t, "not a secret")))
	if _, err := cli(t, "blob", "get", "--addr", addr, "--key-file", key, plain); err == nil || !strings.Contains(err.Error(), "is not sealed; fetch it without --key-file") {
		t.Errorf("unsealing an unsealed blob: %v", err)
	}
	// A blob that claims to be sealed but is too short to be is refused too.
	stump := strings.TrimSpace(mustCLI(t, "blob", "put", "--addr", addr, writeFile(t, "SISYENC1 and then not enough")))
	if _, err := cli(t, "blob", "get", "--addr", addr, "--key-file", key, stump); err == nil || !strings.Contains(err.Error(), "its length is not that of any sealed blob") {
		t.Errorf("unsealing a truncated sealed blob: %v", err)
	}
	// Nothing in the coordinator's block files contains the text.
	blocks, _ := filepath.Glob(filepath.Join(coordinatorDir, "blobs", "blocks", "*", "*.data"))
	for _, block := range blocks {
		if data, _ := os.ReadFile(block); bytes.Contains(data, []byte("confidential")) {
			t.Errorf("the coordinator's disk holds the text in the clear, in %s", block)
		}
	}
}

func TestKeyFileProblems(t *testing.T) {
	addr, _ := startNode(t)
	file := writeFile(t, "data")
	notAKey := writeFile(t, "this is not a key")
	missing := filepath.Join(t.TempDir(), "absent.key")
	for _, command := range [][]string{
		{"blob", "put", "--addr", addr, "--key-file", "KEY", file},
		{"blob", "get", "--addr", addr, "--key-file", "KEY", helloCID},
		{"job", "submit", "--addr", addr, "--key-file", "KEY", "--params", `{"from":0,"to":10}`},
	} {
		for keyFile, want := range map[string]string{missing: "read key", notAKey: "not a sealing key"} {
			args := append([]string{}, command...)
			args[5] = keyFile
			if _, err := cli(t, args...); err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("%v with a bad key file: error %v, want one containing %q", command[:2], err, want)
			}
		}
	}

	key := newKeyFile(t)
	// A sealed fetch of a blob the node lacks.
	if _, err := cli(t, "blob", "get", "--addr", addr, "--key-file", key, helloCID); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("unsealing a blob the node lacks: %v", err)
	}
	// Nowhere to put the sealed blob while it is unsealed.
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "no-such-directory"))
	if _, err := cli(t, "blob", "get", "--addr", addr, "--key-file", key, helloCID); err == nil || !strings.Contains(err.Error(), "no such file or directory") {
		t.Errorf("unsealing with no temporary directory: %v", err)
	}
}
