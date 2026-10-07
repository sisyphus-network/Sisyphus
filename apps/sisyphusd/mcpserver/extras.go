package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc/status"

	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
	"github.com/sisyphus-network/Sisyphus/skills"
)

// teller is how a tool says how far it has got while it waits.
type teller func(done, total float64, message string)

type tellerKey struct{}

// telling returns ctx carrying a way to say how far a tool has got, if
// whoever called the tool asked to be told.
func telling(ctx context.Context, req *mcp.CallToolRequest) context.Context {
	if req == nil || req.Params.GetProgressToken() == nil {
		return ctx
	}
	token, session := req.Params.GetProgressToken(), req.Session
	return context.WithValue(ctx, tellerKey{}, teller(func(done, total float64, message string) {
		// One that cannot be sent is one nobody is waiting for.
		session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{ProgressToken: token, Progress: done, Total: total, Message: message})
	}))
}

// follow reads a job's events until it is over or the wait is, saying how
// far it has got to whoever asked to be told, and returns the job as it
// then stands. The job's events end when the job does, so reading them to
// their end is waiting for it; if the wait runs out first the job runs on.
func (s *server) follow(ctx context.Context, id string, wait time.Duration, tasks int) (any, error) {
	tell, _ := ctx.Value(tellerKey{}).(teller)
	waiting, done := context.WithTimeout(ctx, wait)
	defer done()
	if events, err := s.Node.WatchJobEvents(waiting, &nodepb.WatchJobEventsRequest{JobId: id}); err == nil {
		finished := 0
		for {
			e, err := events.Recv()
			if err != nil {
				break
			}
			if e.GetKind() == "task-succeeded" {
				finished++
			}
			if tell != nil && e.GetKind() != "log" {
				tell(float64(finished), float64(tasks), strings.TrimSpace(e.GetKind()+" "+e.GetText()))
			}
		}
	}
	got, err := s.Node.GetJob(ctx, &nodepb.GetJobRequest{JobId: id})
	if err != nil {
		return nil, fmt.Errorf("job %s was started and could not be followed to its end: %s", id, status.Convert(err).Message())
	}
	return jobView(got.GetJob(), true), nil
}

// waited is how long to wait, given how long was asked for.
func waited(seconds uint32) time.Duration {
	if seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	return usualWait
}

type waitArgs struct {
	JobID       string `json:"job_id" jsonschema:"the job's ID"`
	WaitSeconds uint32 `json:"wait_seconds,omitempty" jsonschema:"how long to wait for it to finish before returning it as it stands; 300 if left out"`
}

func (s *server) waitForJob(ctx context.Context, req *mcp.CallToolRequest, args waitArgs) (*mcp.CallToolResult, any, error) {
	return shown(s.follow(telling(s.as(ctx), req), args.JobID, waited(args.WaitSeconds), 0))
}

type outputsArgs struct {
	JobID     string `json:"job_id" jsonschema:"the job whose stored outputs to fetch"`
	Dir       string `json:"dir" jsonschema:"the directory on this machine to write them into, made if it is not there"`
	Overwrite bool   `json:"overwrite,omitempty" jsonschema:"replace files already there"`
}

// contentID is what a content ID looks like, of either of the kinds the
// store gives.
var contentID = regexp.MustCompile(`^(baf[a-z2-7]{40,}|Qm[1-9A-HJ-NP-Za-km-z]{44})$`)

// named finds the content IDs in a job's result and gives each a name from
// where in the result it is: tasks-0-files-part, output. The names come in
// the result's own order, lists by position and maps by key.
func named(result any, at string, found map[string]string, order *[]string) {
	switch v := result.(type) {
	case string:
		if _, seen := found[v]; contentID.MatchString(v) && !seen {
			found[v] = strings.TrimPrefix(at, "-")
			*order = append(*order, v)
		}
	case []any:
		for i, item := range v {
			named(item, fmt.Sprintf("%s-%d", at, i), found, order)
		}
	case map[string]any:
		for _, key := range slices.Sorted(maps.Keys(v)) {
			named(v[key], at+"-"+strings.Map(func(r rune) rune {
				if r == '/' || r == '\\' || r == 0 {
					return '_'
				}
				return r
			}, key), found, order)
		}
	}
}

// fetchOutputs brings back every file a job stored: those its result names,
// each called by where in the result it is, and any others by content ID.
func (s *server) fetchOutputs(ctx context.Context, args outputsArgs) (any, error) {
	got, err := s.Node.GetJob(ctx, &nodepb.GetJobRequest{JobId: args.JobID})
	if err != nil {
		return nil, err
	}
	names, order := map[string]string{}, []string{}
	var result any
	json.Unmarshal(got.GetJob().GetResult(), &result) // a result that is not JSON names nothing
	named(result, "", names, &order)
	for _, cid := range got.GetJob().GetOutputBlobs() {
		if _, seen := names[cid]; !seen {
			names[cid] = cid
			order = append(order, cid)
		}
	}
	fetched := []any{}
	for i, cid := range order {
		file, err := s.fetch(ctx, fetchArgs{CID: cid, Path: filepath.Join(args.Dir, names[cid]), Overwrite: args.Overwrite})
		if err != nil {
			return nil, fmt.Errorf("output %d of %d: %w", i+1, len(order), err)
		}
		fetched = append(fetched, file)
	}
	return map[string]any{"job_id": args.JobID, "state": jobView(got.GetJob(), false)["state"], "files": fetched}, nil
}

// create makes the file at a path the server may write to, with the
// directory it goes in if that is not there. A file already there is left
// alone unless told otherwise.
func (s *server) create(asked string, overwrite bool) (*os.File, error) {
	path, err := s.allowed(asked)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	how := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if overwrite {
		how = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}
	file, err := os.OpenFile(path, how, 0o644)
	if errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("there is a file at %s already: use another path, or pass overwrite to replace it", asked)
	}
	return file, err
}

type saveArgs struct {
	JobID     string `json:"job_id" jsonschema:"the job whose result to save"`
	Path      string `json:"path" jsonschema:"where on this machine to write it"`
	Overwrite bool   `json:"overwrite,omitempty" jsonschema:"replace a file already at the path"`
}

// saveResult writes a job's result to a file, whole: for one too long to
// be shown, or one a program is to read.
func (s *server) saveResult(ctx context.Context, args saveArgs) (any, error) {
	got, err := s.Node.GetJob(ctx, &nodepb.GetJobRequest{JobId: args.JobID})
	if err != nil {
		return nil, err
	}
	file, err := s.create(args.Path, args.Overwrite)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if _, err := file.Write(got.GetJob().GetResult()); err != nil {
		return nil, fmt.Errorf("write %s: %w", args.Path, err)
	}
	return map[string]any{"job_id": args.JobID, "state": jobView(got.GetJob(), false)["state"], "path": args.Path, "size_bytes": len(got.GetJob().GetResult())}, nil
}

type compareArgs struct {
	Model string   `json:"model" jsonschema:"an embedding model a worker serves, as pool_status names it"`
	Texts []string `json:"texts" jsonschema:"the texts to compare, at most 127"`
	Query string   `json:"query,omitempty" jsonschema:"rank the texts by how alike in meaning each is to this; leave out to rank pairs of the texts by how alike they are to each other"`
	Top   int      `json:"top,omitempty" jsonschema:"how many of the most alike to return; 10 if left out"`
}

// compareTexts has an embedding model the pool serves turn texts into
// vectors and says which mean most alike, so that the vectors themselves,
// which are long, need not pass through whoever asked.
func (s *server) compareTexts(ctx context.Context, args compareArgs) (any, error) {
	if len(args.Texts) == 0 || len(args.Texts) > runtime.MaxEmbedInputs-1 {
		return nil, fmt.Errorf("give between 1 and %d texts", runtime.MaxEmbedInputs-1)
	}
	input := args.Texts
	if args.Query != "" {
		input = append([]string{args.Query}, input...)
	}
	params, _ := json.Marshal(map[string]any{"model": args.Model, "input": input}) // strings always encode
	submitted, err := s.Node.SubmitJob(ctx, &nodepb.SubmitJobRequest{Workload: "embed", Params: params})
	if err != nil {
		return nil, err
	}
	id := submitted.GetJob().GetJobId()
	if _, err := s.follow(ctx, id, usualWait, len(submitted.GetJob().GetTasks())); err != nil {
		return nil, err
	}
	got, _ := s.Node.GetJob(ctx, &nodepb.GetJobRequest{JobId: id}) // it answered a moment ago
	var reply struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if json.Unmarshal(got.GetJob().GetResult(), &reply); len(reply.Data) != len(input) {
		return nil, fmt.Errorf("job %s did not turn the texts into vectors: %s %s", id, jobView(got.GetJob(), false)["state"], got.GetJob().GetError())
	}
	alike := func(a, b []float64) float64 {
		var dot, aa, bb float64
		for i := range min(len(a), len(b)) {
			dot, aa, bb = dot+a[i]*b[i], aa+a[i]*a[i], bb+b[i]*b[i]
		}
		if aa == 0 || bb == 0 {
			return 0
		}
		return math.Round(dot/math.Sqrt(aa*bb)*10000) / 10000
	}
	ranked := []map[string]any{}
	if args.Query != "" {
		for i, text := range args.Texts {
			ranked = append(ranked, map[string]any{"text": text, "index": i, "similarity": alike(reply.Data[0].Embedding, reply.Data[i+1].Embedding)})
		}
	} else {
		for i := range args.Texts {
			for j := i + 1; j < len(args.Texts); j++ {
				ranked = append(ranked, map[string]any{"a": args.Texts[i], "b": args.Texts[j], "a_index": i, "b_index": j, "similarity": alike(reply.Data[i].Embedding, reply.Data[j].Embedding)})
			}
		}
	}
	sort.SliceStable(ranked, func(a, b int) bool { return ranked[a]["similarity"].(float64) > ranked[b]["similarity"].(float64) })
	top := args.Top
	if top <= 0 {
		top = 10
	}
	return map[string]any{"job_id": id, "most_alike": ranked[:min(top, len(ranked))]}, nil
}

func (s *server) deleteChat(ctx context.Context, args chatArgs) (any, error) {
	if _, err := s.Node.DeleteChat(ctx, &nodepb.DeleteChatRequest{ChatId: args.ChatID}); err != nil {
		return nil, err
	}
	return map[string]any{"deleted": args.ChatID}, nil
}

func (s *server) listProviders(ctx context.Context, _ none) (any, error) {
	listed, err := s.Node.ListProviders(ctx, &nodepb.ListProvidersRequest{})
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, p := range listed.GetProviders() {
		out = append(out, map[string]any{"provider": p.GetId(), "name": p.GetName(), "about": p.GetAbout(), "usual_url": p.GetDefaultUrl(), "fetches_models": p.GetFetchesModels()})
	}
	return out, nil
}

func (s *server) removeModel(ctx context.Context, args pullArgs) (any, error) {
	if _, err := s.Node.RemoveModel(ctx, &nodepb.RemoveModelRequest{Model: args.Model}); err != nil {
		return nil, err
	}
	return map[string]any{"removed": args.Model}, nil
}

// extras gives a server the tools that came last, and what an agent can
// read and be prompted with besides tools.
func (s *server) extras(out *mcp.Server) {
	seen := &mcp.ToolAnnotations{ReadOnlyHint: true}
	gone := &mcp.ToolAnnotations{DestructiveHint: ptr(true)}
	add(s, out, reads, &mcp.Tool{Name: "wait_for_job", Annotations: seen,
		Description: "Waits for a job to finish, for as long as told, and returns it as it then stands. For a job started with detach, or one still running when run_job's wait was over."}, s.waitForJob)
	add(s, out, uses, &mcp.Tool{Name: "fetch_outputs",
		Description: "Fetches every file a job stored into a directory on this machine. Each is named by where the job's result names it, such as tasks-0-stdout and tasks-0-files-part for a container job."}, doing(s, s.fetchOutputs))
	add(s, out, uses, &mcp.Tool{Name: "save_result",
		Description: "Writes a job's whole result to a file on this machine: for a result too long to be shown, or one a program is to read."}, doing(s, s.saveResult))
	add(s, out, uses, &mcp.Tool{Name: "compare_texts",
		Description: "Says which texts mean most alike, using an embedding model the pool serves: each text ranked against a query, or every pair of the texts ranked against each other. The workers running it see the texts."}, doing(s, s.compareTexts))
	add(s, out, reads, &mcp.Tool{Name: "list_model_providers", Annotations: seen,
		Description: "Lists the kinds of model service the node's planner can plan with."}, doing(s, s.listProviders))
	add(s, out, uses, &mcp.Tool{Name: "delete_chat", Annotations: gone,
		Description: "Forgets a conversation with the planner."}, doing(s, s.deleteChat))
	add(s, out, admin, &mcp.Tool{Name: "remove_model", Annotations: gone,
		Description: "Deletes a model from the Ollama the planner is set to use, freeing the room it took."}, doing(s, s.removeModel))

	// What an agent may read without calling anything: the pool as it is,
	// and the skill that says how to use it, for one that was not given it.
	read := func(uri, kind string, text func(context.Context) (string, error)) mcp.ResourceHandler {
		return func(ctx context.Context, _ *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			said, err := text(s.as(ctx))
			if err != nil {
				return nil, fmt.Errorf("%s", status.Convert(err).Message())
			}
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: uri, MIMEType: kind, Text: said}}}, nil
		}
	}
	live := func(get func(context.Context) (any, error)) func(context.Context) (string, error) {
		return func(ctx context.Context) (string, error) {
			result, _, err := shown(get(ctx))
			if err != nil {
				return "", err
			}
			return result.Content[0].(*mcp.TextContent).Text, nil
		}
	}
	out.AddResource(&mcp.Resource{URI: "sisyphus://pool", Name: "pool", Title: "The pool now", MIMEType: "application/json",
		Description: "This node and its workers: what each has, serves and is doing."}, read("sisyphus://pool", "application/json", live(s.status)))
	out.AddResource(&mcp.Resource{URI: "sisyphus://workloads", Name: "workloads", Title: "What the pool can run", MIMEType: "application/json",
		Description: "Each workload, the parameters it takes, and how many connected workers run it."}, read("sisyphus://workloads", "application/json", live(s.workloads)))
	for name, title := range map[string]string{"SKILL.md": "How to use a Sisyphus pool", "recipes.md": "Worked shapes for common jobs"} {
		uri := "sisyphus://skill/" + name
		out.AddResource(&mcp.Resource{URI: uri, Name: name, Title: title, MIMEType: "text/markdown", Description: title + ", from the skill that comes with the daemon."},
			read(uri, "text/markdown", func(context.Context) (string, error) {
				text, err := fs.ReadFile(skills.Files, "sisyphus/"+name)
				return string(text), err
			}))
	}

	prompt := func(text string) *mcp.GetPromptResult {
		return &mcp.GetPromptResult{Messages: []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: text}}}}
	}
	out.AddPrompt(&mcp.Prompt{Name: "run-on-pool", Title: "Run something on the pool",
		Description: "Plan and run a piece of work across the pool's machines.",
		Arguments:   []*mcp.PromptArgument{{Name: "task", Description: "What to compute, and on what.", Required: true}}},
		func(_ context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			return prompt("Run this on the Sisyphus pool: " + req.Params.Arguments["task"] + "\n\n" +
				"Read sisyphus://skill/SKILL.md and sisyphus://skill/recipes.md if you have not. Look at pool_status and list_workloads first. " +
				"Say how you will split the work and try it with one or two tasks before running all of it. " +
				"When it is done, report the job's ID, how long it took, on how many tasks, and where the results are."), nil
		})
	out.AddPrompt(&mcp.Prompt{Name: "pool-report", Title: "Report on the pool", Description: "Say what the pool is and what it has been doing."},
		func(context.Context, *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			return prompt("Report on the Sisyphus pool in a few lines: which workers are connected and what each has and serves (pool_status), " +
				"what it can run (list_workloads), and the last few jobs and how they ended (list_jobs). Say plainly if anything looks wrong: no workers, a workload nothing runs, jobs that failed or are stuck."), nil
		})
}
