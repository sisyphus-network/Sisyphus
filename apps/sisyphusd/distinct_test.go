package main

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/access"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
)

// apartPool starts a pool that keeps a verified task's copies apart, and
// returns a way to connect a worker at an address a test names. Each has a
// slot fewer than the one before, so that a task several of them could be
// given goes to them in the order they connected.
func apartPool(t *testing.T) (*pool, func(name, address string) *rawWorker) {
	t.Helper()
	at := map[string]string{}
	workersAt = func(ctx context.Context) string { return at[access.Caller(ctx)] }
	t.Cleanup(func() { workersAt = nil })
	p := startPool(t, runtime.Builtin())
	slots := uint32(8)
	return p, func(name, address string) *rawWorker {
		ident, creds := p.admit(access.Worker)
		at[ident.ID()] = address
		slots--
		w := p.connectRawAs(creds, hello(name, slots, "primes"))
		w.id = ident.ID()
		p.rawIdents[w] = ident
		w.welcome()
		return w
	}
}

// Asked to, a coordinator hands a verified task's copies only to workers at
// different addresses: two on one machine cannot vouch for each other.
func TestAVerifiedTasksCopiesGoToWorkersAtDifferentAddresses(t *testing.T) {
	p, at := apartPool(t)
	here := at("here", "203.0.113.5")
	at("beside", "203.0.113.5")

	// Two workers at one address are one, for this.
	_, err := p.client.SubmitJob(p.ctx, &pb.SubmitJobRequest{Spec: verifiedPrimes(1, 2)})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "counting those at one address as one") {
		t.Fatalf("a job to be verified by two workers at one address: %v", err)
	}
	// A job that is not verified is handed out as before.
	plain := p.submit(primesJob(pb.ScheduleMode_SCHEDULE_MODE_DISTRIBUTED, 1)).GetJobId()
	here.returns(here.assignment(), `{"count":25}`)
	p.wait(plain)

	// With a worker at another address, one copy goes to the first worker
	// and the other to the newcomer, though the worker beside the first has
	// more room. The two differ, and nobody is at a third address to ask.
	elsewhere := at("elsewhere", "203.0.113.9")
	id := p.submit(verifiedPrimes(1, 2)).GetJobId()
	first, second := here.assignment(), elsewhere.assignment()
	here.returns(first, `{"count":7}`)
	elsewhere.returns(second, `{"count":25}`)
	failed := p.wait(id)
	if failed.GetState() != pb.JobState_JOB_STATE_FAILED {
		t.Fatalf("the job: %v %s", failed.GetState(), failed.GetError())
	}
	if told := story(p.events(id, 0)); strings.Contains(told, "beside") {
		t.Errorf("a worker at the address of one already asked was asked too:\n%s", told)
	}

	// A worker at a third address settles the next one.
	third := at("third", "203.0.113.77")
	id = p.submit(verifiedPrimes(1, 2)).GetJobId()
	first, second = here.assignment(), elsewhere.assignment()
	here.returns(first, `{"count":7}`)
	elsewhere.returns(second, `{"count":25}`)
	third.returns(third.assignment(), `{"count":25}`)
	if settled := p.wait(id); settled.GetState() != pb.JobState_JOB_STATE_SUCCEEDED || string(settled.GetResult()) != `{"count":25}` {
		t.Errorf("with a worker at a third address the job: %v %s %s", settled.GetState(), settled.GetResult(), settled.GetError())
	}
}

// A worker whose address is not known is beside nobody: it is not held
// apart from workers it cannot be shown to be with.
func TestWorkersWhoseAddressIsNotKnownAreNotHeldApart(t *testing.T) {
	p, at := apartPool(t)
	one, other := at("one", ""), at("other", "")
	id := p.submit(verifiedPrimes(1, 2)).GetJobId()
	first, second := one.assignment(), other.assignment()
	one.returns(first, `{"count":25}`)
	other.returns(second, `{"count":25}`)
	if done := p.wait(id); done.GetState() != pb.JobState_JOB_STATE_SUCCEEDED {
		t.Errorf("the job: %v %s", done.GetState(), done.GetError())
	}
}

// From the command line: workers on this machine all connect from one
// address, so a coordinator told to keep copies apart has too few.
func TestANodeToldToKeepCopiesApartCountsWorkersOnOneMachineAsOne(t *testing.T) {
	addr := freeAddr(t)
	startDaemon(t, "--role", "coordinator", "--listen", addr, "--verify-distinct-addresses")
	for _, name := range []string{"north", "south"} {
		startDaemon(t, "--role", "worker", "--coordinator", addr, "--name", name, "--slots", "1")
	}
	waitForOutput(t, "south", "nodes", "--addr", addr)
	waitForOutput(t, "north", "nodes", "--addr", addr)
	if _, err := cli(t, "job", "submit", "--addr", addr, "--verify", "2", "--params", `{"from":0,"to":100}`); err == nil || !strings.Contains(err.Error(), "counting those at one address as one") {
		t.Errorf("a job to be verified by two workers on one machine: %v", err)
	}
	if out := mustCLI(t, "job", "submit", "--addr", addr, "--params", `{"from":0,"to":100}`); !strings.Contains(out, `{"count":25}`) {
		t.Errorf("a job that is not verified:\n%s", out)
	}
}
