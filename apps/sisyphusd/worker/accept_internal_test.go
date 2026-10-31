package worker

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"

	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
)

// optInPrimes is an opt-in workload, as mining is to be (#776), that offers
// a label of its own while the worker takes it.
type optInPrimes struct{ runtime.Primes }

func (optInPrimes) Name() string                        { return "opt-in-primes" }
func (optInPrimes) OptIn() bool                         { return true }
func (optInPrimes) Offers(ctx context.Context) []string { return []string{"opt-in-label"} }

func accepting(t *testing.T, known *runtime.Registry, allow, deny []string) runtime.Acceptance {
	t.Helper()
	a, err := runtime.Accept(known, allow, deny)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// A worker tells its coordinator of only the workloads its owner lets it
// take, and of only what those offer, so that it is given nothing else.
func TestAWorkerOffersOnlyWhatItsOwnerAccepts(t *testing.T) {
	runs := runtime.NewRegistry(runtime.Primes{}, runtime.WordCount{}, optInPrimes{})
	for _, tc := range []struct {
		name        string
		allow, deny []string
		want        []string
		label       bool
	}{
		{name: "saying nothing leaves out the opt-in one", want: []string{"primes", "wordcount"}},
		{name: "a denied workload", deny: []string{"wordcount"}, want: []string{"primes"}},
		{name: "the opt-in one when allowed", allow: []string{runtime.GroupDefault, "opt-in-primes"}, want: []string{"opt-in-primes", "primes", "wordcount"}, label: true},
		{name: "only what is allowed", allow: []string{"opt-in-primes"}, want: []string{"opt-in-primes"}, label: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &Worker{Name: "rig", Workloads: runs, Accepts: accepting(t, runs, tc.allow, tc.deny)}
			c := w.capabilities()
			if !slices.Equal(c.GetWorkloads(), tc.want) {
				t.Errorf("offered workloads %v, want %v", c.GetWorkloads(), tc.want)
			}
			if got := slices.Contains(c.GetLabels(), "opt-in-label"); got != tc.label {
				t.Errorf("labels %v: the opt-in workload's label offered %v, want %v", c.GetLabels(), got, tc.label)
			}
		})
	}
}

// Defence in depth: a coordinator that hands the worker a task of a
// workload its owner refuses, though the worker never offered it, has the
// task refused, not run.
func TestAWorkerRefusesATaskItsOwnerDoesNotAccept(t *testing.T) {
	runs := runtime.NewRegistry(runtime.Primes{}, runtime.WordCount{}, optInPrimes{})
	w := &Worker{Name: "rig", Workloads: runs, Accepts: accepting(t, runs, nil, []string{"primes"}), Log: slog.New(slog.DiscardHandler)}
	task := func(workload string) *pb.TaskResult {
		return w.execute(context.Background(), &pb.TaskAssignment{JobId: "j", TaskId: "j/0", Workload: workload, Payload: []byte(`{"from":1,"to":10}`)})
	}
	for _, workload := range []string{"primes", "opt-in-primes"} {
		if got := task(workload); got.GetOutput() != nil || !strings.Contains(got.GetError(), "this worker's owner does not let it take "+workload+" tasks") {
			t.Errorf("a %s task: %v", workload, got)
		}
	}
	// Allowed, the opt-in one runs.
	w.Accepts = accepting(t, runs, []string{"opt-in-primes"}, nil)
	if got := task("opt-in-primes"); got.GetError() != "" {
		t.Errorf("an allowed opt-in task: %v", got.GetError())
	}
}
