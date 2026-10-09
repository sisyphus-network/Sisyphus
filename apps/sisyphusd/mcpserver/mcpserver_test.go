package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
)

// node is a node that answers as it is set to, and fails at whatever call
// is named in failing.
type node struct {
	nodepb.NodeServiceClient
	failing string
	workers []*nodepb.Worker
	job     *nodepb.Job
	jobs    []*nodepb.Job
	events  []*nodepb.JobEvent
	// content is what a fetched file holds, in the pieces it comes in, and
	// broken has a stream fail part way.
	content []string
	broken  bool
	// watched is the context a job's events were last asked for with.
	watched context.Context
	stored  []*nodepb.StoreFileRequest
	token   string
	// submitted is the job the node was last asked to run.
	submitted *nodepb.SubmitJobRequest
}

var errDown = status.Error(codes.Unavailable, "the node is down")

func (n *node) fail(call string) error {
	if n.failing == call {
		return errDown
	}
	return nil
}

func (n *node) GetNodeInfo(ctx context.Context, _ *nodepb.GetNodeInfoRequest, _ ...grpc.CallOption) (*nodepb.GetNodeInfoResponse, error) {
	if md, ok := metadata.FromOutgoingContext(ctx); ok {
		n.token = strings.Join(md.Get("authorization"), "")
	}
	return &nodepb.GetNodeInfoResponse{PeerId: "node-1", Workloads: []string{"primes"}}, n.fail("GetNodeInfo")
}

func (n *node) ListWorkers(context.Context, *nodepb.ListWorkersRequest, ...grpc.CallOption) (*nodepb.ListWorkersResponse, error) {
	return &nodepb.ListWorkersResponse{Workers: n.workers}, n.fail("ListWorkers")
}

func (n *node) SubmitJob(_ context.Context, req *nodepb.SubmitJobRequest, _ ...grpc.CallOption) (*nodepb.SubmitJobResponse, error) {
	n.submitted = req
	return &nodepb.SubmitJobResponse{Job: &nodepb.Job{JobId: "job-1", Verify: req.GetVerify()}}, n.fail("SubmitJob")
}

func (n *node) GetJob(context.Context, *nodepb.GetJobRequest, ...grpc.CallOption) (*nodepb.GetJobResponse, error) {
	return &nodepb.GetJobResponse{Job: n.job}, n.fail("GetJob")
}

func (n *node) ListJobs(context.Context, *nodepb.ListJobsRequest, ...grpc.CallOption) (*nodepb.ListJobsResponse, error) {
	return &nodepb.ListJobsResponse{Jobs: n.jobs}, n.fail("ListJobs")
}

func (n *node) ListFiles(context.Context, *nodepb.ListFilesRequest, ...grpc.CallOption) (*nodepb.ListFilesResponse, error) {
	return nil, n.fail("ListFiles")
}

// eventStream gives a job's events and then ends, or breaks.
type eventStream struct {
	grpc.ClientStream
	left   []*nodepb.JobEvent
	broken bool
}

func (e *eventStream) Recv() (*nodepb.JobEvent, error) {
	if len(e.left) == 0 {
		if e.broken {
			return nil, errDown
		}
		return nil, io.EOF
	}
	next := e.left[0]
	e.left = e.left[1:]
	return next, nil
}

func (n *node) WatchJobEvents(ctx context.Context, _ *nodepb.WatchJobEventsRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[nodepb.JobEvent], error) {
	n.watched = ctx
	if err := n.fail("WatchJobEvents"); err != nil {
		return nil, err
	}
	return &eventStream{left: n.events, broken: n.broken}, nil
}

type fileStream struct {
	grpc.ClientStream
	left   []string
	broken bool
}

func (f *fileStream) Recv() (*nodepb.FetchFileResponse, error) {
	if len(f.left) == 0 {
		if f.broken {
			return nil, errDown
		}
		return nil, io.EOF
	}
	next := f.left[0]
	f.left = f.left[1:]
	return &nodepb.FetchFileResponse{Data: []byte(next)}, nil
}

func (n *node) FetchFile(context.Context, *nodepb.FetchFileRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[nodepb.FetchFileResponse], error) {
	return &fileStream{left: n.content, broken: n.broken}, n.fail("FetchFile")
}

type upload struct {
	grpc.ClientStream
	node *node
}

func (u *upload) Send(req *nodepb.StoreFileRequest) error {
	if u.node.broken {
		return io.EOF
	}
	u.node.stored = append(u.node.stored, &nodepb.StoreFileRequest{Name: req.GetName(), Private: req.GetPrivate(), Data: append([]byte(nil), req.GetData()...)})
	return nil
}

func (u *upload) CloseAndRecv() (*nodepb.File, error) {
	if u.node.broken {
		return nil, errDown
	}
	size := 0
	for _, part := range u.node.stored {
		size += len(part.GetData())
	}
	return &nodepb.File{Cid: "cid-1", Name: u.node.stored[0].GetName(), SizeBytes: uint64(size), Private: u.node.stored[0].GetPrivate()}, nil
}

func (n *node) StoreFile(context.Context, ...grpc.CallOption) (grpc.ClientStreamingClient[nodepb.StoreFileRequest, nodepb.File], error) {
	return &upload{node: n}, n.fail("StoreFile")
}

func serving(n *node) *server {
	return &server{Config{Node: n, Token: "the-token", Workloads: runtime.Builtin()}}
}

func TestEveryToolSaysSoWhenTheNodeFailsIt(t *testing.T) {
	ctx := context.Background()
	for failing, call := range map[string]func(*server) error{
		"GetNodeInfo":    func(s *server) error { _, err := s.status(ctx); return err },
		"ListWorkers":    func(s *server) error { _, err := s.status(ctx); return err },
		"SubmitJob":      func(s *server) error { _, err := s.run(ctx, runJobArgs{Workload: "primes"}); return err },
		"WatchJobEvents": func(s *server) error { _, err := s.logs(ctx, logArgs{JobID: "job-1"}); return err },
		"StoreFile":      func(s *server) error { _, err := s.store(ctx, storeArgs{Path: os.Args[0]}); return err },
		"FetchFile":      func(s *server) error { _, err := s.fetch(ctx, fetchArgs{CID: "cid-1"}); return err },
		"ListFiles":      func(s *server) error { _, _, err := s.listFiles(ctx, nil, none{}); return err },
		"ListJobs":       func(s *server) error { _, _, err := s.listJobs(ctx, nil, none{}); return err },
	} {
		if err := call(serving(&node{failing: failing})); err == nil || !strings.Contains(err.Error(), "the node is down") {
			t.Errorf("with %s failing: %v", failing, err)
		}
	}
	for _, failing := range []string{"GetNodeInfo", "ListWorkers"} {
		if _, err := serving(&node{failing: failing}).workloads(ctx); err == nil {
			t.Errorf("workloads with %s failing: no error", failing)
		}
	}
	// A job started and then lost sight of is named, so it can be found.
	if _, err := serving(&node{failing: "GetJob"}).run(ctx, runJobArgs{Workload: "primes"}); err == nil || !strings.Contains(err.Error(), "job job-1 was started") {
		t.Errorf("a job that could not be followed: %v", err)
	}
	// Events that cannot be watched do not stop a job being reported.
	n := &node{failing: "WatchJobEvents", job: &nodepb.Job{JobId: "job-1", State: nodepb.JobState_JOB_STATE_RUNNING}}
	if got, err := serving(n).run(ctx, runJobArgs{Workload: "primes", WaitSeconds: 1}); err != nil || got.(map[string]any)["state"] != "running" {
		t.Errorf("a job whose events cannot be watched: %v, %v", got, err)
	}
	// What a tool says of a failure is the node's words and no more.
	if _, _, err := shown(nil, errDown); err == nil || err.Error() != "the node is down" {
		t.Errorf("a failure as shown: %v", err)
	}
}

func TestThePoolIsDescribedWithItsCardsAndTheTokenIsShown(t *testing.T) {
	n := &node{workers: []*nodepb.Worker{
		{Name: "rig", TaskSlots: 4, RunningTasks: 1, Workloads: []string{"primes"}, Gpus: []*nodepb.WorkerGpu{{Name: "RTX 5090"}}, Models: []string{"llama3.1:8b"},
			VerifiedAgreed: 12, VerifiedOutvoted: 1, Probation: 7},
		{Name: "laptop", TaskSlots: 2},
	}}
	got, err := serving(n).status(serving(n).as(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	pool := got.(map[string]any)
	workers := pool["workers"].([]map[string]any)
	if pool["task_slots"] != 6 || pool["running_tasks"] != 1 || workers[0]["gpus"].([]string)[0] != "RTX 5090" || workers[1]["gpus"] != nil ||
		workers[0]["models"].([]string)[0] != "llama3.1:8b" || workers[1]["models"] != nil {
		t.Errorf("pool = %v", pool)
	}
	// A worker is shown with its standing once verified tasks have given
	// it one.
	if shown, _ := json.Marshal(workers[0]["verified"]); string(shown) != `{"agreed":12,"outvoted":1,"probation":7}` || workers[1]["verified"] != nil {
		t.Errorf("the standing of the rig is shown as %s, and of the laptop as %v", shown, workers[1]["verified"])
	}
	if n.token != "Bearer the-token" {
		t.Errorf("the node was shown %q", n.token)
	}
}

func TestAJobIsShownAsItIs(t *testing.T) {
	failed := jobView(&nodepb.Job{JobId: "j", State: nodepb.JobState_JOB_STATE_FAILED, Error: "no worker runs it", Result: []byte("plain words"), CreatedAtMs: 1000, FinishedAtMs: 3500}, true)
	if failed["state"] != "failed" || failed["error"] != "no worker runs it" || failed["result"] != "plain words" || failed["seconds"] != 2.5 {
		t.Errorf("a failed job: %v", failed)
	}
	long := jobView(&nodepb.Job{Result: []byte(strings.Repeat("9", maxResult+1))}, true)
	if got := long["result_begins"].(string); len(got) != resultBegins || long["result"] != nil || long["result_bytes"] != maxResult+1 {
		t.Errorf("a long result is shown as %v", long)
	}
}

func TestAJobCanBeAskedToBeVerifiedAndIsShownToHaveBeen(t *testing.T) {
	n := &node{}
	shown, err := serving(n).run(context.Background(), runJobArgs{Workload: "primes", Verify: 3, Detach: true})
	if err != nil || n.submitted.GetVerify() != 3 {
		t.Fatalf("the node was asked for %v, %v", n.submitted, err)
	}
	if view := shown.(map[string]any); view["verified_by"] != uint32(3) {
		t.Errorf("a job each task of which three workers must agree on: %v", view)
	}
	// One that was not says nothing of it.
	if plain := jobView(&nodepb.Job{JobId: "j", Verify: 1}, true); plain["verified_by"] != nil {
		t.Errorf("a job that is not verified: %v", plain)
	}

	// With a share, the node is asked for that, and the job is shown with
	// the tasks that were held to it.
	if _, err := serving(n).run(context.Background(), runJobArgs{Workload: "primes", Verify: 2, VerifyShare: 0.25, Detach: true}); err != nil || n.submitted.GetVerifyShare() != 0.25 {
		t.Fatalf("the node was asked for %v, %v", n.submitted, err)
	}
	spot := jobView(&nodepb.Job{JobId: "j", Verify: 2, VerifyShare: 0.25, Tasks: []*nodepb.JobTask{{Index: 0, Verify: 1}, {Index: 1, Verify: 2}, {Index: 2, Verify: 1}}}, true)
	if got, _ := json.Marshal(spot["verified_tasks"]); string(got) != "[1]" || spot["verified_by"] != uint32(2) {
		t.Errorf("a job one task of which was verified: %v", spot)
	}
	if graph := jobView(&nodepb.Job{JobId: "j", Verify: 2, VerifyShare: 0.25, Tasks: []*nodepb.JobTask{{Index: 0}}}, true); graph["verified_tasks"] != nil || graph["verify_share"] != 0.25 {
		t.Errorf("a job whose tasks are jobs, a quarter of each to be verified: %v", graph)
	}
}

func TestJobsAreListedNewestFirstAndNotWithoutEnd(t *testing.T) {
	n := &node{}
	for i := range mostJobs + 5 {
		n.jobs = append(n.jobs, &nodepb.Job{JobId: fmt.Sprint(i), CreatedAtMs: int64(i)})
	}
	result, _, err := serving(n).listJobs(context.Background(), nil, none{})
	if err != nil {
		t.Fatal(err)
	}
	text := result.Content[0].(interface{ MarshalJSON() ([]byte, error) })
	encoded, _ := text.MarshalJSON()
	if got := string(encoded); strings.Count(got, "job_id") != mostJobs || strings.Index(got, fmt.Sprintf(`\"job_id\":\"%d\"`, mostJobs+4)) > strings.Index(got, fmt.Sprintf(`\"job_id\":\"%d\"`, mostJobs+3)) {
		t.Errorf("jobs listed: %s", got)
	}
}

func TestOnlyTheLastOfManyEventsAreGivenAndABrokenStreamIsAFailure(t *testing.T) {
	n := &node{}
	for i := range mostEvents + 10 {
		n.events = append(n.events, &nodepb.JobEvent{Seq: uint64(i + 1), Kind: "log", TaskIndex: 0, Text: "line"})
	}
	got, err := serving(n).logs(context.Background(), logArgs{JobID: "job-1"})
	if err != nil {
		t.Fatal(err)
	}
	logs := got.(map[string]any)
	if events := logs["events"].([]map[string]any); len(events) != mostEvents || events[0]["seq"] != uint64(11) || events[0]["task"] != int32(0) || logs["last_seq"] != uint64(mostEvents+10) {
		t.Errorf("%d events, the first %v, the last numbered %v", len(events), events[0], logs["last_seq"])
	}
	n.broken = true
	if _, err := serving(n).logs(context.Background(), logArgs{JobID: "job-1"}); !errors.Is(err, errDown) {
		t.Errorf("events that break off: %v", err)
	}
}

func TestStoringAFileInPiecesAndWhenItCannotBeRead(t *testing.T) {
	big := filepath.Join(t.TempDir(), "big.bin")
	os.WriteFile(big, make([]byte, piece+10), 0o600)
	n := &node{}
	got, err := serving(n).store(context.Background(), storeArgs{Path: big, Name: "renamed", Private: true})
	if err != nil {
		t.Fatal(err)
	}
	if file := got.(map[string]any); file["name"] != "renamed" || file["size_bytes"] != uint64(piece+10) || file["private"] != true || len(n.stored[0].GetData()) != piece || n.stored[1].GetName() != "" {
		t.Errorf("stored %v in %d pieces", file, len(n.stored))
	}
	// A directory opens and cannot be read.
	if _, err := serving(&node{}).store(context.Background(), storeArgs{Path: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "read ") {
		t.Errorf("storing a directory: %v", err)
	}
	if _, err := serving(&node{broken: true}).store(context.Background(), storeArgs{Path: big}); !errors.Is(err, errDown) {
		t.Errorf("storing to a node that hangs up: %v", err)
	}
}

func TestFetchingWhatCannotBeReturnedOrWritten(t *testing.T) {
	ctx := context.Background()
	for name, tt := range map[string]struct {
		node *node
		path string
		want string
	}{
		"too long to return":   {&node{content: []string{strings.Repeat("a", maxInline), "b"}}, "", "give a path"},
		"not text":             {&node{content: []string{"\xff\xfe"}}, "", "not text"},
		"a stream that breaks": {&node{content: []string{"some"}, broken: true}, "", "the node is down"},
		"nowhere to write it":  {&node{content: []string{"some"}}, t.TempDir(), "is a directory"},
		"a full disk":          {&node{content: []string{"some"}}, "/dev/full", "write /dev/full"},
	} {
		if _, err := serving(tt.node).fetch(ctx, fetchArgs{CID: "cid-1", Path: tt.path, Overwrite: true}); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v, want %q", name, err, tt.want)
		}
	}
}

func TestWhatAToolReturnsIsWrittenToBeRead(t *testing.T) {
	result, _, err := shown(map[string]string{"description": `{"image": "<image>"}`}, nil)
	if err != nil || result.Content[0].(*mcp.TextContent).Text != `{"description":"{\"image\": \"<image>\"}"}` {
		t.Fatalf("shown = %v, %v", result.Content[0], err)
	}
}

// A running job is listened to for a moment that is timed on this side
// alone: the node is given no deadline, so it cannot be first to end the
// stream and have that taken for a failure.
func TestLogsOfARunningJobGiveTheNodeNoDeadline(t *testing.T) {
	n := &node{events: []*nodepb.JobEvent{{Seq: 1, Kind: "submitted", TaskIndex: -1}}}
	if _, err := serving(n).logs(context.Background(), logArgs{JobID: "job-1"}); err != nil {
		t.Fatal(err)
	}
	if _, timed := n.watched.Deadline(); timed {
		t.Error("the node was told when the listening would end")
	}
}
