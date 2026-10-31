package main

import (
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"golang.org/x/crypto/sha3"

	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
)

// vanityKey is a requester's key pair: the private key, which stays here,
// and the public key in hex, which goes in the job.
func vanityKey(t *testing.T) (*secp256k1.PrivateKey, string) {
	t.Helper()
	a, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	return a, hex.EncodeToString(a.PubKey().SerializeCompressed())
}

// ethAddressOf is the EIP-55 address of a private key, worked out here as
// a wallet would, apart from the workload's own code.
func ethAddressOf(key *secp256k1.PrivateKey) string {
	keccak := func(b []byte) []byte {
		k := sha3.NewLegacyKeccak256()
		k.Write(b)
		return k.Sum(nil)
	}
	digits := []byte(hex.EncodeToString(keccak(key.PubKey().SerializeUncompressed()[1:])[12:]))
	checksum := keccak(digits)
	for i, c := range digits {
		if c >= 'a' && checksum[i/2]>>(4*(1-i%2))&0xf >= 8 {
			digits[i] = c - 'a' + 'A'
		}
	}
	return "0x" + string(digits)
}

// The pool finds b; only the requester, who kept a, can make the key of the
// address found, a + b; and that key's address is the one found.
func TestAVanityJobRunsAcrossThePool(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	p.startWorker("a", 2)
	p.startWorker("b", 2)
	p.waitForWorkers(2)
	a, public := vanityKey(t)
	params := `{"public_key":"` + public + `","kind":"eth","prefix":"0xab","tries":40000}`
	job := p.wait(p.submit(&pb.JobSpec{Workload: "vanity", Params: []byte(params), Mode: pb.ScheduleMode_SCHEDULE_MODE_DISTRIBUTED, MaxTasks: 4}).GetJobId())
	if job.GetState() != pb.JobState_JOB_STATE_SUCCEEDED {
		t.Fatalf("job %v: %s", job.GetState(), job.GetError())
	}
	var result runtime.VanityResult
	if err := json.Unmarshal(job.GetResult(), &result); err != nil || !strings.HasPrefix(strings.ToLower(result.Address), "0xab") {
		t.Fatalf("result %s, %v", job.GetResult(), err)
	}
	var b secp256k1.ModNScalar
	raw, _ := hex.DecodeString(result.Offset)
	b.SetByteSlice(raw)
	if got := ethAddressOf(secp256k1.NewPrivateKey(new(secp256k1.ModNScalar).Add2(&a.Key, &b))); got != result.Address {
		t.Errorf("a + b has address %s, the job found %s", got, result.Address)
	}
}

// A job asking for a pattern harder than the coordinator takes, or naming a
// private key, is refused when submitted.
func TestAVanityJobBeyondTheCapOrWithAPrivateKeyIsRefused(t *testing.T) {
	p := startPool(t, runtime.Builtin().With(runtime.Vanity{MaxTries: 1000}))
	a, public := vanityKey(t)
	for _, tt := range []struct{ params, want string }{
		{`{"public_key":"` + public + `","kind":"eth","prefix":"0xabc"}`, "--vanity-max-tries"},
		{`{"public_key":"` + hex.EncodeToString(a.Serialize()) + `"}`, "Never send a private key"},
	} {
		_, err := p.client.SubmitJob(p.ctx, &pb.SubmitJobRequest{Spec: &pb.JobSpec{Workload: "vanity", Params: []byte(tt.params)}})
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v", tt.params, err)
		}
	}
}

// A worker that returns a b whose address does not match is caught as its
// result comes in, whether or not the job asked to be verified: the attempt
// fails, and the task is tried again.
func TestALyingVanityWorkerIsCaught(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	w := p.connectRaw(hello("liar", 1, "vanity"))
	w.welcome()
	_, public := vanityKey(t)
	id := p.submit(&pb.JobSpec{Workload: "vanity", Params: []byte(`{"public_key":"` + public + `","kind":"eth","prefix":"0xa","tries":2000}`), Mode: pb.ScheduleMode_SCHEDULE_MODE_FULL_WORKER}).GetJobId()
	task := w.assignment()
	var payload struct {
		Start string `json:"start"`
	}
	if err := json.Unmarshal(task.GetPayload(), &payload); err != nil {
		t.Fatal(err)
	}
	lie := `{"found":true,"offset":"` + payload.Start + `","address":"0xa000000000000000000000000000000000000000","tried":1}`
	w.reply(&pb.TaskResult{TaskId: task.GetTaskId(), Attempt: task.GetAttempt(), Outcome: &pb.TaskResult_Output{Output: []byte(lie)}})
	again := w.assignment()
	honest, err := (runtime.Vanity{}).Execute(t.Context(), nil, again.GetPayload())
	if err != nil {
		t.Fatal(err)
	}
	w.reply(&pb.TaskResult{TaskId: again.GetTaskId(), Attempt: again.GetAttempt(), Outcome: &pb.TaskResult_Output{Output: honest}})
	done := p.wait(id)
	if done.GetState() != pb.JobState_JOB_STATE_SUCCEEDED {
		t.Fatalf("the job: %v %s", done.GetState(), done.GetError())
	}
	caught := false
	for _, e := range p.events(id, 0) {
		caught = caught || e.GetKind() == "task-failed" && strings.HasPrefix(e.GetText(), "the coordinator checked the result and refused it: vanity: the address of A + b·G for offset "+payload.Start)
	}
	if !caught || !strings.Contains(p.logs.String(), "does not stand up to checking") {
		t.Errorf("the lie was not caught; the log:\n%s", p.logs.String())
	}
}
