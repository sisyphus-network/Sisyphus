package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/blobclient"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
	"github.com/sisyphus-network/Sisyphus/packages/sealed"
)

// Sealing gives the same blob for the same key and data, so workers that
// did the same work for a private job stored blobs with the same CIDs, and
// their results compare as those of any job do.
func TestAPrivateJobIsVerifiedByWorkersThatSealTheSameWorkAlike(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	p.startWorker("a", 3)
	p.startWorker("b", 3)
	p.waitForWorkers(2)
	key, text := sealed.NewKey(), sampleText()
	input, err := blobclient.Upload(p.ctx, p.blobs, sealed.Encrypt(key, strings.NewReader(text)))
	if err != nil {
		t.Fatal(err)
	}
	spec := func(verify uint32, share float64) *pb.JobSpec {
		return &pb.JobSpec{Workload: "wordcount", Params: []byte(`{"input":"` + input.String() + `"}`), MaxTasks: 3, Key: key[:], Verify: verify, VerifyShare: share}
	}

	done := p.wait(p.submit(spec(2, 0)).GetJobId())
	if done.GetState() != pb.JobState_JOB_STATE_SUCCEEDED || !done.GetPrivate() || done.GetSpec().GetVerify() != 2 || len(done.GetTasks()) != 3 {
		t.Fatalf("the job %v, private %v, in %d tasks: %s", done.GetState(), done.GetPrivate(), len(done.GetTasks()), done.GetError())
	}
	// Each task stored a sealed blob, on both workers, and they agreed.
	events := p.events(done.GetJobId(), 0)
	if count(events, "task-disagreed") != 0 || count(events, "task-result") != 6 || count(events, "task-succeeded") != 3 {
		t.Errorf("the job's events:\n%s", story(events))
	}
	// It gives what the same job gives unverified.
	if plain := p.wait(p.submit(spec(0, 0)).GetJobId()); p.unsealedTable(plain, key) != p.unsealedTable(done, key) {
		t.Errorf("verified it gave %s, and unverified %s", done.GetResult(), plain.GetResult())
	}

	// Its record says who agreed, and of what each returned only what the
	// job's key can check: a digest would tell someone who guessed a
	// task's output that they were right.
	record := p.recordText(done.GetJobId())
	if strings.Count(record, `"agreed":true`) != 6 || strings.Count(record, `"digest_commitment"`) != 6 || strings.Contains(record, `"digest":`) {
		t.Errorf("the job's record: %s", record)
	}
	checked, matched, _ := p.commitmentChecks(done.GetJobId(), key[:])
	digests := 0
	for _, name := range matched {
		if strings.HasPrefix(name, "receipts/") && strings.HasSuffix(name, "/digest_commitment") {
			digests++
		}
	}
	// The parameters, the result and the error; and for each task its
	// output, an error for each of two attempts, and two digests.
	if checked != 3+3*5 || len(matched) != checked || digests != 6 {
		t.Errorf("with the job's key %d of %d commitments match, %d of them to digests: %v", len(matched), checked, digests, matched)
	}
	other := sealed.NewKey()
	if _, matched, _ := p.commitmentChecks(done.GetJobId(), other[:]); len(matched) != 0 {
		t.Errorf("another key gave %v", matched)
	}

	// A share of its tasks can be verified, as of any job's.
	spot := p.wait(p.submit(spec(2, 0.3)).GetJobId())
	if spot.GetState() != pb.JobState_JOB_STATE_SUCCEEDED || p.unsealedTable(spot, key) != p.unsealedTable(done, key) {
		t.Fatalf("with a share verified, the job %v: %s, %s", spot.GetState(), spot.GetResult(), spot.GetError())
	}
	if events := p.events(spot.GetJobId(), 0); count(events, "task-result") != 4 {
		t.Errorf("one task of three was to be verified:\n%s", story(events))
	}
}

func TestAWorkerThatReturnsAWrongResultForAPrivateJobIsOutvoted(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	crew := p.crew("liar", "beta", "gamma")
	liar, beta, gamma := crew[0], crew[1], crew[2]
	key := sealed.NewKey()
	spec := verifiedPrimes(1, 2)
	spec.Key = key[:]
	id := p.submit(spec).GetJobId()

	// Every worker asked is given the job's own key, and not the sealing
	// key it was derived from. What each says it stored is compared by
	// name: a blob sealed from other data has another.
	own := key.ForJob(id)
	given := func(a *pb.TaskAssignment) bool {
		return len(a.GetKeys()) == 1 && bytes.Equal(a.GetKeys()[0].GetKey(), own.Key[:]) && bytes.Equal(a.GetKeys()[0].GetId(), own.ID[:]) && a.GetKey() == nil
	}
	first, second := liar.assignment(), beta.assignment()
	if !given(first) || !given(second) {
		t.Fatal("a worker of a private job that is verified was not given the job's key and that alone")
	}
	same, other := p.upload("sealed from the data"), p.upload("sealed from other data")
	liar.returns(first, `{"count":7}`, other)
	beta.returns(second, `{"count":7}`, same)
	third := gamma.assignment()
	if !given(third) {
		t.Fatal("the worker asked to settle it was not given the key")
	}
	gamma.returns(third, `{"count":7}`, same)

	done := p.wait(id)
	if done.GetState() != pb.JobState_JOB_STATE_SUCCEEDED || string(done.GetResult()) != `{"count":7}` {
		t.Fatalf("the job %v: %s, %s", done.GetState(), done.GetResult(), done.GetError())
	}
	if told := story(p.events(id, 0)); !strings.Contains(told, "task-succeeded: beta, gamma returned the same result; liar returned another") {
		t.Errorf("the job's events:\n%s", told)
	}
	record := p.recordText(id)
	if strings.Count(record, `"agreed":true`) != 2 || strings.Count(record, `"agreed":false`) != 1 || strings.Contains(record, `"digest":`) {
		t.Errorf("the job's record: %s", record)
	}
	if checked, matched, _ := p.commitmentChecks(id, key[:]); checked != len(matched) || checked != 3+1+3+3 {
		t.Errorf("with the job's key %d of %d commitments match: %v", len(matched), checked, matched)
	}
}
