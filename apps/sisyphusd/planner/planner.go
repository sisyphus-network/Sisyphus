// Package planner is the loop at the heart of Sisyphus: a language model is
// given a problem, decides what to compute, has the pool compute it, reads
// what came back, and decides what to do next, until it has an answer.
//
// The model does the reasoning. Everything it does to the world it does by
// asking for one of a few tools to be run, and it is this package that runs
// them: it never gives the model more reach than those tools have.
package planner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sisyphus-network/Sisyphus/packages/ai"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/sealed"
)

// Pool is the pool the planner has jobs run on. A coordinator is one.
type Pool interface {
	Submit(ctx context.Context, spec *pb.JobSpec) (*pb.Job, error)
	Watch(ctx context.Context, jobID string, fn func(*pb.Job) error) error
	Get(jobID string) (*pb.Job, error)
}

// Workload is a kind of computation the pool can do, as the model is told
// of it.
type Workload struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Planner answers questions by reasoning with a model and computing on a
// pool.
type Planner struct {
	Model     ai.Provider
	ModelName string
	Pool      Pool
	Workloads []Workload
	// SealingKey supplies this node's key for private jobs. It is never
	// passed to the model or accepted from tool arguments.
	SealingKey     func() (sealed.Key, error)
	requirePrivate bool
	// MaxSteps is how many times the model may be asked before the planner
	// gives up on a question. Zero means eight.
	MaxSteps int
}

// The kinds of thing a planner reports while it works.
const (
	// Text is a piece of what the model is saying.
	Text = "text"
	// Calling is the model asking for a tool to be run.
	Calling = "call"
	// Job is a job the planner has given the pool, named in JobID.
	Job = "job"
	// Result is what a tool returned.
	Result = "result"
)

// Event is something a planner reports while it works on a question.
type Event struct {
	Kind string
	Text string
	// JobID is set on a Job event, and Tool on Calling and Result events.
	JobID string
	Tool  string
}

const instructions = `You are the planner of Sisyphus, a network of computers that people pool to run computations.

When a question needs computing, do not guess and do not work it out in your head: have the pool compute it, by calling run_job, and answer from the result. Split the work into as many tasks as is sensible; the pool runs them at once on different machines. If one job's result shows that more is needed, run another. When you have what you need, answer plainly and say which jobs you ran.

If a question needs no computation, just answer it. If the pool cannot do what is asked, say so rather than pretend.

For encrypted input files, run a private job. Private jobs give the decryption key to their workers, so use only trusted workers. Never ask the user for the key or put it in parameters.

The pool can run these workloads and no others:
`

// tools is what the model may ask to have run.
var tools = []ai.Tool{
	{
		Name:        "run_job",
		Description: "Runs a job on the pool, waits for it to finish, and returns its result.",
		Parameters: json.RawMessage(`{"type":"object","properties":{` +
			`"workload":{"type":"string","description":"The name of the workload to run."},` +
			`"params":{"type":"object","description":"The workload's parameters, as its description gives them."},` +
			`"tasks":{"type":"integer","description":"How many tasks to split the job into. Leave out to use one for each free machine slot."},` +
			`"private":{"type":"boolean","description":"Use this node's encryption key for sealed input files and private output. Workers running the job receive the key."}` +
			`},"required":["workload","params"]}`),
	},
	{
		Name:        "get_job",
		Description: "Returns the state and result of a job run earlier.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"job_id":{"type":"string"}},"required":["job_id"]}`),
	},
}

// ErrTooManySteps reports a question the planner gave up on.
var ErrTooManySteps = errors.New("the planner gave up: the model kept asking for more without reaching an answer")

// Run works on the conversation in history, whose last message is the
// question, and returns what was said in the course of answering it: the
// model's messages and what the tools returned, in order, ending with the
// answer. What it returns, added to history, is the history for the next
// question. It reports what it is doing as it goes.
//
// If it fails part way, it returns what was said up to then with the error.
func (p *Planner) Run(ctx context.Context, history []ai.Message, report func(Event)) ([]ai.Message, error) {
	// Keep this policy local to one conversation/run, even if a caller
	// reuses the Planner for an unrelated conversation.
	run := *p
	run.requirePrivate = hasPrivateAttachments(history)
	p = &run
	system := instructions
	for _, w := range p.Workloads {
		system += fmt.Sprintf("- %s: %s\n", w.Name, w.Description)
	}
	steps := p.MaxSteps
	if steps == 0 {
		steps = 8
	}
	var said []ai.Message
	for range steps {
		messages := append(append([]ai.Message{{Role: ai.System, Content: system}}, history...), said...)
		// Text that may yet turn out to be a call written out is held back
		// until it is known which it is, so that a call is not shown to
		// the person as a line of JSON first.
		var held strings.Builder
		passing := false
		pass := func() {
			if passing = true; held.Len() > 0 {
				report(Event{Kind: Text, Text: held.String()})
				held.Reset()
			}
		}
		reply, err := p.Model.Chat(ctx, ai.Request{Model: p.ModelName, Messages: messages, Tools: tools}, func(text string) {
			if held.WriteString(text); passing || !mayBeWrittenOut(held.String()) {
				pass()
			}
		})
		if err != nil {
			pass()
			return said, err
		}
		if call, written := writtenOut(reply.Content); written && len(reply.Calls) == 0 {
			// A small model sometimes writes its call out as text rather
			// than making it. What it meant is plain, so it is taken as
			// the call it is: otherwise its question goes unanswered.
			call.ID = fmt.Sprintf("written-%d", len(said))
			reply.Content, reply.Calls, reply.Raw = "", []ai.Call{call}, nil
			held.Reset()
		}
		pass()
		said = append(said, reply)
		if len(reply.Calls) == 0 {
			return said, nil
		}
		for _, call := range reply.Calls {
			report(Event{Kind: Calling, Tool: call.Name, Text: string(call.Arguments)})
			result := p.call(ctx, call, report)
			report(Event{Kind: Result, Tool: call.Name, Text: result})
			said = append(said, ai.Message{Role: ai.ToolRole, CallID: call.ID, Name: call.Name, Content: result})
		}
	}
	return said, ErrTooManySteps
}

// writtenOut is the tool call a reply's text is, if the whole of it is one:
// a JSON object naming one of the planner's tools and giving its arguments
// as "arguments" or "parameters", bare or in a code fence. A reply that
// says anything else besides is an answer, and is left as it is.
func writtenOut(text string) (ai.Call, bool) {
	text = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), "<|python_tag|>"))
	if fenced, is := strings.CutPrefix(text, "```"); is {
		fenced, closed := strings.CutSuffix(fenced, "```")
		if !closed {
			return ai.Call{}, false
		}
		text = strings.TrimSpace(strings.TrimPrefix(fenced, "json"))
	}
	if !strings.HasPrefix(text, "{") {
		return ai.Call{}, false
	}
	var written struct {
		Name       string          `json:"name"`
		Arguments  json.RawMessage `json:"arguments"`
		Parameters json.RawMessage `json:"parameters"`
	}
	if json.Unmarshal([]byte(text), &written) != nil && json.Unmarshal([]byte(asJSON(text)), &written) != nil {
		return ai.Call{}, false
	}
	arguments := written.Arguments
	if arguments == nil {
		arguments = written.Parameters
	}
	if !strings.HasPrefix(string(arguments), "{") {
		return ai.Call{}, false
	}
	for _, tool := range tools {
		if tool.Name == written.Name {
			return ai.Call{Name: written.Name, Arguments: arguments}, true
		}
	}
	return ai.Call{}, false
}

// heldAtMost is how much text is held back while it may be a call written
// out. A call is short; an answer that opens like one and runs on is shown.
const heldAtMost = 4096

// mayBeWrittenOut reports whether text, which is the beginning of a reply,
// could be the beginning of a call written out: it opens as one does, or
// has not yet said enough to tell.
func mayBeWrittenOut(text string) bool {
	text = strings.TrimSpace(text)
	if len(text) > heldAtMost {
		return false
	}
	for _, opens := range []string{"{", "```", "<|python_tag|>"} {
		if strings.HasPrefix(text, opens) || strings.HasPrefix(opens, text) {
			return true
		}
	}
	return false
}

// asJSON is text with Python's True, False and None, where they stand as
// values and not inside a string, written as JSON writes them: models
// trained on Python write a call that way.
func asJSON(text string) string {
	var out strings.Builder
	quoted := false
	for i := 0; i < len(text); i++ {
		switch c := text[i]; {
		case quoted && c == '\\' && i+1 < len(text):
			out.WriteByte(c)
			i++
			out.WriteByte(text[i])
			continue
		case c == '"':
			quoted = !quoted
		case !quoted:
			word := false
			for python, json := range map[string]string{"True": "true", "False": "false", "None": "null"} {
				if strings.HasPrefix(text[i:], python) {
					out.WriteString(json)
					i += len(python) - 1
					word = true
				}
			}
			if word {
				continue
			}
		}
		out.WriteByte(text[i])
	}
	return out.String()
}

// call runs one tool and returns what it has to say, as JSON. A tool that
// fails says so in what it returns: the model is told, and may try again
// some other way.
func (p *Planner) call(ctx context.Context, call ai.Call, report func(Event)) string {
	var out any
	var err error
	switch call.Name {
	case "run_job":
		out, err = p.runJob(ctx, call.Arguments, report)
	case "get_job":
		out, err = p.getJob(call.Arguments)
	default:
		err = fmt.Errorf("there is no tool called %q", call.Name)
	}
	if err != nil {
		out = map[string]string{"error": err.Error()}
	}
	encoded, _ := json.Marshal(out) // maps of strings and numbers always encode
	return string(encoded)
}

func (p *Planner) runJob(ctx context.Context, arguments json.RawMessage, report func(Event)) (any, error) {
	var args struct {
		Workload string          `json:"workload"`
		Params   json.RawMessage `json:"params"`
		Tasks    uint32          `json:"tasks"`
		Private  bool            `json:"private"`
	}
	if err := json.Unmarshal(arguments, &args); err != nil {
		return nil, fmt.Errorf("the arguments are not what run_job takes: %w", err)
	}
	spec := &pb.JobSpec{Workload: args.Workload, Params: args.Params, MaxTasks: args.Tasks}
	if args.Private || p.requirePrivate {
		if p.SealingKey == nil {
			return nil, errors.New("private jobs are unavailable: this node has no sealing key provider")
		}
		key, err := p.SealingKey()
		if err != nil {
			return nil, fmt.Errorf("private job key unavailable: %w", err)
		}
		spec.Key = key[:]
	}
	submitted, err := p.Pool.Submit(ctx, spec)
	if err != nil {
		return nil, err
	}
	report(Event{Kind: Job, JobID: submitted.GetJobId()})
	var finished *pb.Job
	err = p.Pool.Watch(ctx, submitted.GetJobId(), func(job *pb.Job) error {
		finished = job
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("job %s was started and could not be followed to its end: %w", submitted.GetJobId(), err)
	}
	return describe(finished), nil
}

// Attachment metadata is carried by the desktop's existing text envelope.
// A private attachment makes all jobs in this conversation private, even
// if the model omits the flag. Older public attachments remain compatible.
func hasPrivateAttachments(history []ai.Message) bool {
	for _, message := range history {
		if message.Role != ai.User {
			continue
		}
		_, attachments, found := strings.Cut(message.Content, "[Sisyphus attachments]\n")
		if !found {
			continue
		}
		for _, line := range strings.Split(attachments, "\n") {
			if strings.HasPrefix(line, "- ") && strings.Contains(line, " | cid:") && strings.HasSuffix(line, " | private:true") {
				return true
			}
		}
	}
	return false
}

func (p *Planner) getJob(arguments json.RawMessage) (any, error) {
	var args struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(arguments, &args); err != nil {
		return nil, fmt.Errorf("the arguments are not what get_job takes: %w", err)
	}
	job, err := p.Pool.Get(args.JobID)
	if err != nil {
		return nil, err
	}
	return describe(job), nil
}

// maxResult is how much of a job's result the model is shown.
const maxResult = 8 << 10

// describe tells the model what became of a job.
func describe(job *pb.Job) map[string]any {
	out := map[string]any{
		"job_id": job.GetJobId(),
		"state":  strings.ToLower(strings.TrimPrefix(job.GetState().String(), "JOB_STATE_")),
		"tasks":  len(job.GetTasks()),
	}
	if job.GetFinishedAt() != nil {
		out["seconds"] = job.GetFinishedAt().AsTime().Sub(job.GetCreatedAt().AsTime()).Round(time.Millisecond).Seconds()
	}
	if job.GetError() != "" {
		out["error"] = job.GetError()
	}
	// A result that is JSON is passed on as JSON, and anything else as text.
	switch result := job.GetResult(); {
	case len(result) > maxResult:
		out["result"] = string(result[:maxResult]) + " [cut short]"
	case json.Valid(result):
		out["result"] = json.RawMessage(result)
	case len(result) > 0:
		out["result"] = string(result)
	}
	if len(job.GetOutputBlobs()) > 0 {
		out["stored_outputs"] = job.GetOutputBlobs()
	}
	return out
}
