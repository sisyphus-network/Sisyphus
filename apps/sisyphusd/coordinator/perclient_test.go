package coordinator

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// One client may have only so many jobs unfinished at once, so that it
// cannot fill the coordinator for everyone else. Others are not held to
// its count, nor is the node itself, nor are the steps of a job.
func TestAClientMayHaveOnlySoManyJobsUnfinished(t *testing.T) {
	c := New(Config{Workloads: runtime.Builtin(), Store: storage.NewMemory(), MaxJobsPerClient: 2, Log: slog.New(slog.DiscardHandler)})
	defer c.Close()
	ctx := context.Background()
	submit := func(parent, submitter string) (*pb.Job, error) {
		return c.submit(ctx, &pb.JobSpec{Workload: "primes", Params: []byte(`{"from":1,"to":10}`)}, parent, "", submitter)
	}
	// With no worker connected, every job stays unfinished.
	first, err := submit("", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := submit("", "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := submit("", "alice"); status.Code(err) != codes.ResourceExhausted || !strings.Contains(err.Error(), "this client has 2 jobs unfinished") {
		t.Fatalf("a third job from one client: %v", err)
	}
	for name, submitter := range map[string]string{"another client": "bob", "the node itself": ""} {
		if _, err := submit("", submitter); err != nil {
			t.Errorf("a job from %s: %v", name, err)
		}
	}
	if _, err := submit("a job of alice's", "alice"); err != nil {
		t.Errorf("a step of one of the client's jobs: %v", err)
	}
	// Once one of its jobs is over, the client may submit another.
	if _, err := c.Cancel(first.GetJobId()); err != nil {
		t.Fatal(err)
	}
	if _, err := submit("", "alice"); err != nil {
		t.Errorf("a job once one of the client's was over: %v", err)
	}
}
