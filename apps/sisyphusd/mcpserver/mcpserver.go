// Package mcpserver lets an AI agent use a Sisyphus node. It speaks the
// Model Context Protocol, which agents such as Claude Code, Claude Desktop
// and many others speak, and offers the node's pool as a handful of tools:
// see what the pool is, run a job on it, follow the job, move files in and
// out.
//
// It is a client of the node's local API and nothing more. Whatever an
// agent does through it, the node's owner could do from the command line
// with the same token, and it can do nothing else.
package mcpserver

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
)

// Config is what a server is made from.
type Config struct {
	// Node is the node's local API, and Token what it wants shown.
	Node  nodepb.NodeServiceClient
	Token string
	// Workloads is where the description of each workload is looked up.
	Workloads *runtime.Registry
	// Version is the daemon's, which the server gives as its own.
	Version string

	// FilesUnder is the directory the server may read files from and
	// write them to, and nowhere else. Empty is anywhere its user may.
	FilesUnder string
	// ReadOnly has the server offer only the tools that change nothing.
	ReadOnly bool
	// Admin has it also offer the tools that change the node itself: who
	// is in its pool, which nodes it trusts, what it plans with.
	Admin bool
	// Images, if not empty, are the only container images it will run.
	Images []string
}

type server struct{ Config }

const instructions = `Sisyphus is a pool of computers that run jobs. A job is one workload run over an input, split into tasks that the pool's machines run at once.

Start with pool_status to see which machines there are, and list_workloads to see what they can run and what parameters each workload takes. Run something with run_job, which waits for the result. Inputs and outputs that are files are named by content ID: store_file puts a local file in the pool and returns its ID, and fetch_file brings one back.

Workers may serve language models: ask_model has one answer a prompt, and ask_planner hands a whole question to the node's own planner. Which tools there are depends on how the server was started: it may be read-only, may keep to one directory for files, and offers the tools that change the node itself only if its owner allowed them.`

// New returns a server offering cfg's node to an agent.
func New(cfg Config) *mcp.Server {
	s := &server{cfg}
	out := mcp.NewServer(&mcp.Implementation{Name: "sisyphus", Title: "Sisyphus", Version: cfg.Version}, &mcp.ServerOptions{Instructions: instructions})
	seen := &mcp.ToolAnnotations{ReadOnlyHint: true}
	add(s, out, reads, &mcp.Tool{Name: "pool_status", Annotations: seen,
		Description: "Says what the pool is now: this node, and each worker with its cores, memory, graphics cards, the language models it serves, how many tasks it can run at once and how many it is running."}, s.poolStatus)
	add(s, out, reads, &mcp.Tool{Name: "list_workloads", Annotations: seen,
		Description: "Lists the workloads the pool can run, each with what it does and the parameters it takes. Read this before run_job."}, s.listWorkloads)
	add(s, out, uses, &mcp.Tool{Name: "run_job",
		Description: "Runs a job on the pool and waits for it to finish, returning its result. A job still running when the wait is over is returned as it stands, to be followed with get_job."}, s.runJob)
	add(s, out, reads, &mcp.Tool{Name: "get_job", Annotations: seen,
		Description: "Returns the state of a job and, if it has finished, its result."}, s.getJob)
	add(s, out, reads, &mcp.Tool{Name: "list_jobs", Annotations: seen,
		Description: "Lists the pool's jobs, newest first, without their results."}, s.listJobs)
	add(s, out, uses, &mcp.Tool{Name: "cancel_job",
		Description: "Stops a job that has not finished."}, s.cancelJob)
	add(s, out, reads, &mcp.Tool{Name: "job_logs", Annotations: seen,
		Description: "Returns what has happened to a job: the steps of its life and the lines its tasks have logged."}, s.jobLogs)
	add(s, out, uses, &mcp.Tool{Name: "store_file",
		Description: "Puts a file from this machine in the pool's store and returns its content ID, which is what a job takes as input."}, s.storeFile)
	add(s, out, uses, &mcp.Tool{Name: "fetch_file",
		Description: "Fetches a stored file by content ID, such as one of a job's stored outputs. It is written to the path given, or returned as text if it is short text and no path is given."}, s.fetchFile)
	add(s, out, reads, &mcp.Tool{Name: "list_files", Annotations: seen,
		Description: "Lists the files kept in the pool's store, with their content IDs."}, s.listFiles)
	s.more(out)
	s.extras(out)
	return out
}

// shown is how every tool answers: as JSON, in text.
func shown(v any, err error) (*mcp.CallToolResult, any, error) {
	if err != nil {
		// What the node said, without the wrapping of how it was said.
		return nil, nil, errors.New(status.Convert(err).Message())
	}
	// Written as it is to be read: a description's <angle brackets> are
	// not escaped as they would be for a web page.
	var encoded strings.Builder
	writer := json.NewEncoder(&encoded)
	writer.SetEscapeHTML(false)
	writer.Encode(v) // maps of strings and numbers always encode
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: strings.TrimSpace(encoded.String())}}}, nil, nil
}

// as returns ctx carrying the node's token.
func (s *server) as(ctx context.Context) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+s.Token)
}

type none struct{}

func (s *server) poolStatus(ctx context.Context, _ *mcp.CallToolRequest, _ none) (*mcp.CallToolResult, any, error) {
	return shown(s.status(s.as(ctx)))
}

func (s *server) status(ctx context.Context) (any, error) {
	node, err := s.Node.GetNodeInfo(ctx, &nodepb.GetNodeInfoRequest{})
	if err != nil {
		return nil, err
	}
	listed, err := s.Node.ListWorkers(ctx, &nodepb.ListWorkersRequest{})
	if err != nil {
		return nil, err
	}
	workers := []map[string]any{}
	slots, running := 0, 0
	for _, w := range listed.GetWorkers() {
		worker := map[string]any{
			"name": w.GetName(), "peer_id": w.GetPeerId(), "os": w.GetOs(), "arch": w.GetArch(),
			"cpu_cores": w.GetCpuCores(), "memory_bytes": w.GetMemoryBytes(),
			"task_slots": w.GetTaskSlots(), "running_tasks": w.GetRunningTasks(), "workloads": w.GetWorkloads(),
		}
		var cards []string
		for _, g := range w.GetGpus() {
			cards = append(cards, g.GetName())
		}
		if cards != nil {
			worker["gpus"] = cards
		}
		if models := w.GetModels(); models != nil {
			worker["models"] = models
		}
		workers = append(workers, worker)
		slots += int(w.GetTaskSlots())
		running += int(w.GetRunningTasks())
	}
	return map[string]any{
		"node":    map[string]any{"peer_id": node.GetPeerId(), "version": node.GetDaemonVersion()},
		"workers": workers, "task_slots": slots, "running_tasks": running,
	}, nil
}

func (s *server) listWorkloads(ctx context.Context, _ *mcp.CallToolRequest, _ none) (*mcp.CallToolResult, any, error) {
	return shown(s.workloads(s.as(ctx)))
}

// workloads lists what the node can coordinate, and for each which of the
// workers now connected run it: a workload none runs would wait for one.
func (s *server) workloads(ctx context.Context) (any, error) {
	node, err := s.Node.GetNodeInfo(ctx, &nodepb.GetNodeInfoRequest{})
	if err != nil {
		return nil, err
	}
	listed, err := s.Node.ListWorkers(ctx, &nodepb.ListWorkersRequest{})
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, name := range node.GetWorkloads() {
		runBy := 0
		for _, w := range listed.GetWorkers() {
			for _, runs := range w.GetWorkloads() {
				if runs == name {
					runBy++
				}
			}
		}
		workload := map[string]any{"name": name, "description": s.Workloads.Describe(name), "workers_running_it": runBy}
		if s.Workloads.Composite(name) {
			// No worker runs it and none need: its steps are jobs.
			delete(workload, "workers_running_it")
			workload["run_by"] = "the coordinator, as a job for each step"
		}
		out = append(out, workload)
	}
	return out, nil
}

type runJobArgs struct {
	Workload    string         `json:"workload" jsonschema:"the name of the workload to run, from list_workloads"`
	Params      map[string]any `json:"params,omitempty" jsonschema:"the workload's parameters, as its description gives them"`
	Tasks       uint32         `json:"tasks,omitempty" jsonschema:"how many tasks to split the job into; leave out for one per free worker slot"`
	Private     bool           `json:"private,omitempty" jsonschema:"seal everything the job stores with this node's key, so that only it and the workers running the job can read it"`
	MinMemoryMB uint32         `json:"min_memory_mb,omitempty" jsonschema:"give its tasks only to workers with at least this much memory, in mebibytes"`
	MinGPUs     uint32         `json:"min_gpus,omitempty" jsonschema:"give its tasks only to workers with at least this many graphics cards"`
	TaskTimeout uint32         `json:"task_timeout_seconds,omitempty" jsonschema:"stop and retry any attempt at a task that runs longer than this"`
	Verify      uint32         `json:"verify,omitempty" jsonschema:"have each task run by this many different workers and take its result only once that many have returned the same one; only for work that gives the same result every time it is run, not with private, and it multiplies the work by that many"`
	WaitSeconds uint32         `json:"wait_seconds,omitempty" jsonschema:"how long to wait for the job to finish before returning it as it stands; 300 if left out, 0 with detach"`
	Detach      bool           `json:"detach,omitempty" jsonschema:"return at once with the job's ID instead of waiting"`
}

// usualWait is how long run_job waits if not told.
const usualWait = 300 * time.Second

func (s *server) runJob(ctx context.Context, req *mcp.CallToolRequest, args runJobArgs) (*mcp.CallToolResult, any, error) {
	return shown(s.run(telling(s.as(ctx), req), args))
}

func (s *server) run(ctx context.Context, args runJobArgs) (any, error) {
	if err := s.permitted(args); err != nil {
		return nil, err
	}
	params, _ := json.Marshal(args.Params) // decoded from JSON a moment ago
	if args.Params == nil {
		params = nil
	}
	submitted, err := s.Node.SubmitJob(ctx, &nodepb.SubmitJobRequest{
		Workload: args.Workload, Params: params, MaxTasks: args.Tasks, Private: args.Private,
		MinGpus: args.MinGPUs, MinMemoryBytes: uint64(args.MinMemoryMB) << 20, TaskTimeoutSeconds: args.TaskTimeout,
		Verify: args.Verify,
	})
	if err != nil {
		return nil, err
	}
	id := submitted.GetJob().GetJobId()
	if args.Detach {
		return jobView(submitted.GetJob(), true), nil
	}
	return s.follow(ctx, id, waited(args.WaitSeconds), len(submitted.GetJob().GetTasks()))
}

// maxResult is the longest result an agent is shown, and resultBegins how
// much it is shown of a longer one.
const (
	maxResult    = 32 << 10
	resultBegins = 1 << 10
)

// jobView is a job as an agent is told of it, with or without its result.
func jobView(job *nodepb.Job, result bool) map[string]any {
	state := strings.ToLower(strings.TrimPrefix(job.GetState().String(), "JOB_STATE_"))
	out := map[string]any{"job_id": job.GetJobId(), "workload": job.GetWorkload(), "state": state, "tasks": len(job.GetTasks())}
	if job.GetFinishedAtMs() > 0 {
		out["seconds"] = float64(job.GetFinishedAtMs()-job.GetCreatedAtMs()) / 1000
	} else {
		out["progress"] = job.GetProgress()
		if result {
			out["note"] = "the job has not finished: follow it with get_job or job_logs"
		}
	}
	if job.GetError() != "" {
		out["error"] = job.GetError()
	}
	if job.GetVerify() >= 2 {
		// How many different workers had to return the same result for
		// each task.
		out["verified_by"] = job.GetVerify()
	}
	if !result {
		return out
	}
	// A result that is JSON is passed on as JSON, and anything else as text.
	switch r := job.GetResult(); {
	case len(r) > maxResult:
		// Too long to be read here: its beginning, and how to have it all.
		out["result_begins"] = string(r[:resultBegins])
		out["result_bytes"] = len(r)
		out["note"] = "the result is too long to show: write it to a file with save_result"
	case json.Valid(r):
		out["result"] = json.RawMessage(r)
	case len(r) > 0:
		out["result"] = string(r)
	}
	if len(job.GetOutputBlobs()) > 0 {
		out["stored_outputs"] = job.GetOutputBlobs()
	}
	return out
}

type jobArgs struct {
	JobID string `json:"job_id" jsonschema:"the job's ID, as run_job or list_jobs gave it"`
}

func (s *server) getJob(ctx context.Context, _ *mcp.CallToolRequest, args jobArgs) (*mcp.CallToolResult, any, error) {
	got, err := s.Node.GetJob(s.as(ctx), &nodepb.GetJobRequest{JobId: args.JobID})
	if err != nil {
		return shown(nil, err)
	}
	return shown(jobView(got.GetJob(), true), nil)
}

func (s *server) cancelJob(ctx context.Context, _ *mcp.CallToolRequest, args jobArgs) (*mcp.CallToolResult, any, error) {
	stopped, err := s.Node.CancelJob(s.as(ctx), &nodepb.CancelJobRequest{JobId: args.JobID})
	if err != nil {
		return shown(nil, err)
	}
	return shown(jobView(stopped.GetJob(), false), nil)
}

// mostJobs is how many jobs list_jobs lists.
const mostJobs = 50

func (s *server) listJobs(ctx context.Context, _ *mcp.CallToolRequest, _ none) (*mcp.CallToolResult, any, error) {
	listed, err := s.Node.ListJobs(s.as(ctx), &nodepb.ListJobsRequest{})
	if err != nil {
		return shown(nil, err)
	}
	// Newest first, whichever way the node gave them.
	newest := slices.SortedStableFunc(slices.Values(listed.GetJobs()), func(a, b *nodepb.Job) int {
		return cmp.Compare(b.GetCreatedAtMs(), a.GetCreatedAtMs())
	})
	out := []map[string]any{}
	for _, job := range newest[:min(len(newest), mostJobs)] {
		out = append(out, jobView(job, false))
	}
	return shown(out, nil)
}

type logArgs struct {
	JobID    string `json:"job_id" jsonschema:"the job's ID"`
	AfterSeq uint64 `json:"after_seq,omitempty" jsonschema:"return only events numbered after this, to carry on from an earlier call"`
}

// How long job_logs listens to a job still running, and how many events it
// returns at most: the last of them.
const (
	listenFor  = 2 * time.Second
	mostEvents = 200
)

func (s *server) jobLogs(ctx context.Context, _ *mcp.CallToolRequest, args logArgs) (*mcp.CallToolResult, any, error) {
	return shown(s.logs(s.as(ctx), args))
}

func (s *server) logs(ctx context.Context, args logArgs) (any, error) {
	// A finished job's events end by themselves; a running job's go on, so
	// they are listened to for a moment and what came is returned.
	// The moment is timed here and the node is not told of it: a node that
	// knows the deadline may be first to end the stream, and its word for
	// why cannot be told from a failure.
	listening, done := context.WithCancel(ctx)
	defer done()
	defer time.AfterFunc(listenFor, done).Stop()
	events, err := s.Node.WatchJobEvents(listening, &nodepb.WatchJobEventsRequest{JobId: args.JobID, AfterSeq: args.AfterSeq})
	if err != nil {
		return nil, err
	}
	lines := []map[string]any{}
	last, ended := args.AfterSeq, false
	for {
		e, err := events.Recv()
		if errors.Is(err, io.EOF) {
			ended = true
			break
		}
		if err != nil {
			if listening.Err() != nil && ctx.Err() == nil {
				break
			}
			return nil, err
		}
		line := map[string]any{"seq": e.GetSeq(), "kind": e.GetKind()}
		if e.GetTaskIndex() >= 0 {
			line["task"] = e.GetTaskIndex()
		}
		if e.GetWorkerName() != "" {
			line["worker"] = e.GetWorkerName()
		}
		if e.GetText() != "" {
			line["text"] = e.GetText()
		}
		lines = append(lines, line)
		last = e.GetSeq()
	}
	if len(lines) > mostEvents {
		lines = lines[len(lines)-mostEvents:]
	}
	return map[string]any{"events": lines, "last_seq": last, "job_finished": ended}, nil
}

type storeArgs struct {
	Path    string `json:"path" jsonschema:"the file on this machine to store"`
	Name    string `json:"name,omitempty" jsonschema:"what to call it in the pool; the file's own name if left out"`
	Private bool   `json:"private,omitempty" jsonschema:"seal it with this node's key before it leaves the machine; a job must then be private to read it"`
}

// piece is how much of a file goes in one message, as the daemon does it.
const piece = 256 << 10

func (s *server) storeFile(ctx context.Context, _ *mcp.CallToolRequest, args storeArgs) (*mcp.CallToolResult, any, error) {
	return shown(s.store(s.as(ctx), args))
}

func (s *server) store(ctx context.Context, args storeArgs) (any, error) {
	path, err := s.allowed(args.Path)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	name := args.Name
	if name == "" {
		name = file.Name()[strings.LastIndexAny(file.Name(), `/\`)+1:]
	}
	stream, err := s.Node.StoreFile(ctx)
	if err != nil {
		return nil, err
	}
	next := &nodepb.StoreFileRequest{Name: name, Private: args.Private}
	buf := make([]byte, piece)
	for {
		n, readErr := file.Read(buf)
		next.Data = buf[:n]
		// A send that fails is explained by what closing the stream returns.
		if stream.Send(next) != nil || readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, fmt.Errorf("read %s: %w", args.Path, readErr)
		}
		next = &nodepb.StoreFileRequest{}
	}
	stored, err := stream.CloseAndRecv()
	if err != nil {
		return nil, err
	}
	return fileView(stored), nil
}

func fileView(f *nodepb.File) map[string]any {
	return map[string]any{"cid": f.GetCid(), "name": f.GetName(), "size_bytes": f.GetSizeBytes(), "private": f.GetPrivate()}
}

type fetchArgs struct {
	CID       string `json:"cid" jsonschema:"the content ID of the file"`
	Path      string `json:"path,omitempty" jsonschema:"where on this machine to write it; leave out to have a short text file returned instead"`
	Overwrite bool   `json:"overwrite,omitempty" jsonschema:"replace a file already at the path, which is otherwise left alone and the fetch refused"`
}

// maxInline is the longest file fetch_file returns as text.
const maxInline = 64 << 10

func (s *server) fetchFile(ctx context.Context, _ *mcp.CallToolRequest, args fetchArgs) (*mcp.CallToolResult, any, error) {
	return shown(s.fetch(s.as(ctx), args))
}

func (s *server) fetch(ctx context.Context, args fetchArgs) (any, error) {
	var into io.Writer
	var text strings.Builder
	if args.Path == "" {
		into = &text
	} else {
		file, err := s.create(args.Path, args.Overwrite)
		if err != nil {
			return nil, err
		}
		defer file.Close()
		into = file
	}
	stream, err := s.Node.FetchFile(ctx, &nodepb.FetchFileRequest{Cid: args.CID})
	if err != nil {
		return nil, err
	}
	size := 0
	for {
		part, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		size += len(part.GetData())
		if args.Path == "" && size > maxInline {
			return nil, fmt.Errorf("the file is longer than %d bytes: give a path to write it to", maxInline)
		}
		if _, err := into.Write(part.GetData()); err != nil {
			return nil, fmt.Errorf("write %s: %w", args.Path, err)
		}
	}
	if args.Path != "" {
		return map[string]any{"cid": args.CID, "path": args.Path, "size_bytes": size}, nil
	}
	if !utf8.ValidString(text.String()) {
		return nil, errors.New("the file is not text: give a path to write it to")
	}
	return map[string]any{"cid": args.CID, "size_bytes": size, "text": text.String()}, nil
}

func (s *server) listFiles(ctx context.Context, _ *mcp.CallToolRequest, _ none) (*mcp.CallToolResult, any, error) {
	listed, err := s.Node.ListFiles(s.as(ctx), &nodepb.ListFilesRequest{})
	if err != nil {
		return shown(nil, err)
	}
	out := []map[string]any{}
	for _, f := range listed.GetFiles() {
		out = append(out, fileView(f))
	}
	return shown(out, nil)
}
