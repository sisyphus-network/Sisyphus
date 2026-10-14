package worker

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
)

// asking is a workload whose tasks need a model, as a conversation's do,
// and that does what its payload says: answers, fails, or waits to be
// stopped.
type asking struct{}

func (asking) Name() string { return "asking" }
func (asking) Split(context.Context, runtime.Blobs, []byte, int) ([][]byte, error) {
	return nil, nil
}
func (asking) Aggregate(context.Context, runtime.Blobs, [][]byte) ([]byte, error) { return nil, nil }
func (asking) Needs([]byte) []string                                              { return []string{"model:llama3.1:8b"} }
func (asking) Execute(ctx context.Context, _ runtime.Blobs, payload []byte) ([]byte, error) {
	switch string(payload) {
	case "fail":
		return nil, errors.New("out of memory")
	case "wait":
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return []byte("an answer"), nil
}

// A worker's own log says what it is doing for the pool: each task as it
// starts, with the model it asks for, and how it ended.
func TestAWorkerLogsEachTaskItIsGivenAndHowItEnded(t *testing.T) {
	log := new(lines)
	w := &Worker{Name: "laptop", Workloads: runtime.NewRegistry(asking{}, runtime.Primes{}), Log: slog.New(slog.NewTextHandler(log, nil))}
	w.execute(context.Background(), &pb.TaskAssignment{JobId: "j1", TaskId: "j1/0", Workload: "asking", Payload: []byte("answer")})
	w.execute(context.Background(), &pb.TaskAssignment{JobId: "j1", TaskId: "j1/1", Workload: "asking", Payload: []byte("fail")})
	stopped, stop := context.WithCancel(context.Background())
	stop()
	w.execute(stopped, &pb.TaskAssignment{JobId: "j1", TaskId: "j1/2", Workload: "asking", Payload: []byte("wait")})
	w.execute(context.Background(), &pb.TaskAssignment{JobId: "j2", TaskId: "j2/0", Workload: "primes", Payload: []byte(`{"from":1,"to":10}`)})
	// A private job's task names no model, even to its worker's log.
	w.execute(context.Background(), &pb.TaskAssignment{JobId: "j3", TaskId: "j3/0", Workload: "asking", Payload: []byte("answer"), Keys: []*pb.SealingKey{{}}})
	w.execute(context.Background(), &pb.TaskAssignment{JobId: "j4", TaskId: "j4/0", Workload: "nothing known"})

	said := strings.Join(log.said, "")
	for _, want := range []string{
		`msg="task started" job=j1 task=j1/0 workload=asking attempt=0 model=llama3.1:8b`,
		`msg="task done" job=j1 task=j1/0 took=`,
		`output_bytes=9`,
		`level=WARN msg="task failed" job=j1 task=j1/1`,
		`error="out of memory"`,
		`msg="task stopped" job=j1 task=j1/2`,
		`msg="task started" job=j2 task=j2/0 workload=primes attempt=0` + "\n",
		`msg="task started" job=j3 task=j3/0 workload=asking attempt=0` + "\n",
		`msg="task failed" job=j4 task=j4/0`,
	} {
		if !strings.Contains(said, want) {
			t.Errorf("the log has no %q:\n%s", want, said)
		}
	}
	if strings.Contains(said, "task=j4/0 workload") {
		t.Errorf("a task of a workload the worker does not have was said to start:\n%s", said)
	}
}
