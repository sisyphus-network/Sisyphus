package worker

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
)

// A worker runs a task only for what it has: the coordinator may be
// anyone's, and one that gives it a task its job needs something else for
// is refused.
func TestAWorkerRefusesATaskItDoesNotHaveWhatItNeedsFor(t *testing.T) {
	w := &Worker{Name: "laptop", Workloads: runtime.NewRegistry(runtime.Primes{}), Log: slog.New(slog.DiscardHandler)}
	primes := func(needs ...string) *pb.TaskResult {
		return w.execute(context.Background(), &pb.TaskAssignment{JobId: "j", TaskId: "j/0", Workload: "primes", Payload: []byte(`{"from":1,"to":10}`), Needs: needs})
	}
	// Before it has told a coordinator anything, it goes by what it has.
	if got := primes(runtime.SealingLabel, runtime.WorkerLabel+"someone-else's-name"); got.GetError() != "" {
		t.Fatalf("a task needing what the worker has: %v", got.GetError())
	}
	if got := primes(runtime.NetworkLabel); !strings.Contains(got.GetError(), "this worker does not have container-network") {
		t.Fatalf("a task needing what the worker has not: %v", got)
	}
	// And afterwards by what it told the coordinator it has.
	offered := []string{runtime.SealingLabel, runtime.NetworkLabel}
	w.offered.Store(&offered)
	if got := primes(runtime.NetworkLabel); got.GetError() != "" {
		t.Fatalf("a task needing what the worker said it has: %v", got.GetError())
	}
	if got := primes("model:llama3.1:8b"); got.GetError() == "" {
		t.Fatal("a task needing what the worker did not say it has was run")
	}
}
