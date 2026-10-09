package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Graph is a job made of other jobs: steps, each a job of some workload,
// where a step may wait for others and use what they produced. A video is
// transcoded and then the words of its transcript counted; three sweeps run
// side by side and a fourth compares them.
//
// Unlike other workloads its tasks are not given to workers. Each task is a
// step, and the coordinator runs a step by submitting the job it describes
// and waiting for it. So a graph needs no worker that knows of graphs, only
// workers for what its steps run.
type Graph struct{}

// Composite is a workload whose tasks the coordinator carries out itself,
// each as a job of its own, instead of handing them to workers.
type Composite interface {
	// After returns the indices of the tasks that must have succeeded
	// before the task with this payload is begun.
	After(payload []byte) []int
	// Child returns the job that carries out the task with this payload,
	// given the outputs of the job's tasks so far, in task order, those
	// not yet done being empty.
	Child(payload []byte, outputs [][]byte) (ChildJob, error)
	// Output returns what the task's output is, its job having succeeded
	// with this result and these stored outputs.
	Output(payload []byte, jobID string, result []byte, stored []string) []byte
}

// ChildJob is a job that carries out one task of a composite job.
type ChildJob struct {
	// Name is what the task calls itself, for whoever follows the job.
	Name     string
	Workload string
	Params   []byte
	// Tasks is how many tasks to split it into; zero leaves it to the pool.
	Tasks int
	// Timeout, if not zero, is how long one attempt at one of its tasks may run.
	Timeout time.Duration
}

// GraphParams is a graph: its steps, in the order they are written.
type GraphParams struct {
	Steps []GraphStep `json:"steps"`
}

// GraphStep is one step of a graph.
type GraphStep struct {
	// Name is what other steps call it by.
	Name     string `json:"name"`
	Workload string `json:"workload"`
	// Params are the workload's parameters. Anywhere in them, a string may
	// refer to what an earlier step produced: ${name.result}, or a field
	// of it as ${name.result.output}, ${name.outputs.0} for the first file
	// it stored, ${name.job_id}. A string that is nothing but one
	// reference becomes the value itself, whatever it is.
	Params json.RawMessage `json:"params,omitempty"`
	// After names steps to wait for besides those the parameters refer to.
	After []string `json:"after,omitempty"`
	// Tasks and TimeoutSeconds are as for any job.
	Tasks          int `json:"tasks,omitempty"`
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
}

// graphTask is a step as its task carries it: the step, and which tasks it
// waits for.
type graphTask struct {
	Step  GraphStep `json:"step"`
	After []int     `json:"after,omitempty"`
}

// stepOutput is what a step's task has for output.
type stepOutput struct {
	Step    string          `json:"step"`
	JobID   string          `json:"job_id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Outputs []string        `json:"outputs,omitempty"`
}

// MaxGraphSteps is how many steps a graph may have.
const MaxGraphSteps = 64

var (
	stepName  = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	reference = regexp.MustCompile(`\$\{([A-Za-z0-9_-]+)\.(result|outputs|job_id)((?:\.[A-Za-z0-9_-]+)*)\}`)
)

func (Graph) Name() string { return "graph" }

func (Graph) Describe() string {
	return `Runs several jobs as one, each a step that may wait for others and use what they produced. ` +
		`Parameters: {"steps": [{"name": "<a name>", "workload": "<any workload>", "params": {<its parameters>}, "after": [<optional names of steps to wait for>], "tasks": <optional>}, ...]}. ` +
		`In a step's parameters a string may refer to an earlier step: "${name.result}" for its result, "${name.result.<field>}" for a field of it, "${name.outputs.0}" for the first file it stored, "${name.job_id}". ` +
		`A step waits for every step it refers to; steps that wait for nothing run side by side. ` +
		`The result is {"steps": [{"step", "job_id", "result", "outputs"}, ...], "result": <the last step's result>}. If a step fails the graph fails and the steps not yet begun are not run.`
}

func (Graph) Split(_ context.Context, _ Blobs, params []byte, _ int) ([][]byte, error) {
	var graph GraphParams
	if err := json.Unmarshal(params, &graph); err != nil {
		return nil, fmt.Errorf("graph parameters: %w", err)
	}
	if len(graph.Steps) == 0 {
		return nil, errors.New("a graph must have steps")
	}
	if len(graph.Steps) > MaxGraphSteps {
		return nil, fmt.Errorf("a graph may have at most %d steps, and this has %d", MaxGraphSteps, len(graph.Steps))
	}
	index := make(map[string]int, len(graph.Steps))
	for i, step := range graph.Steps {
		if !stepName.MatchString(step.Name) {
			return nil, fmt.Errorf("step %d needs a name of letters, digits, - and _, not %q", i+1, step.Name)
		}
		if _, taken := index[step.Name]; taken {
			return nil, fmt.Errorf("two steps are called %q", step.Name)
		}
		if step.Workload == "" {
			return nil, fmt.Errorf("step %q names no workload", step.Name)
		}
		index[step.Name] = i
	}
	tasks := make([]graphTask, len(graph.Steps))
	for i, step := range graph.Steps {
		waits := append([]string(nil), step.After...)
		for _, found := range reference.FindAllSubmatch(step.Params, -1) {
			waits = append(waits, string(found[1]))
		}
		tasks[i].Step = step
		for _, name := range waits {
			at, known := index[name]
			if !known {
				return nil, fmt.Errorf("step %q waits for a step %q that there is none of", step.Name, name)
			}
			if at == i {
				return nil, fmt.Errorf("step %q waits for itself", step.Name)
			}
			if !slices.Contains(tasks[i].After, at) {
				tasks[i].After = append(tasks[i].After, at)
			}
		}
		slices.Sort(tasks[i].After)
	}
	// A step that waits, however indirectly, for itself would never begin.
	const (
		unseen = iota
		looking
		clear
	)
	seen := make([]int, len(tasks))
	var circles func(i int) bool
	circles = func(i int) bool {
		if seen[i] != unseen {
			return seen[i] == looking
		}
		seen[i] = looking
		for _, before := range tasks[i].After {
			if circles(before) {
				return true
			}
		}
		seen[i] = clear
		return false
	}
	payloads := make([][]byte, len(tasks))
	for i, task := range tasks {
		if circles(i) {
			return nil, fmt.Errorf("step %q waits for a step that waits for it", task.Step.Name)
		}
		payloads[i] = mustJSON(task)
	}
	return payloads, nil
}

// Execute is never asked for: a graph's steps are carried out by the
// coordinator, as jobs.
func (Graph) Execute(context.Context, Blobs, []byte) ([]byte, error) {
	return nil, errors.New("a graph's steps are jobs the coordinator submits, not tasks for a worker")
}

func (Graph) Aggregate(_ context.Context, _ Blobs, outputs [][]byte) ([]byte, error) {
	steps := make([]json.RawMessage, len(outputs))
	var last stepOutput
	for i, output := range outputs {
		if err := json.Unmarshal(output, &last); err != nil {
			return nil, fmt.Errorf("graph step %d output: %w", i, err)
		}
		steps[i] = output
	}
	out := map[string]any{"steps": steps}
	if last.Result != nil {
		out["result"] = last.Result
	}
	return mustJSON(out), nil
}

func (Graph) After(payload []byte) []int {
	var task graphTask
	json.Unmarshal(payload, &task) // a payload this workload made
	return task.After
}

func (Graph) Output(payload []byte, jobID string, result []byte, stored []string) []byte {
	var task graphTask
	json.Unmarshal(payload, &task) // a payload this workload made
	out := stepOutput{Step: task.Step.Name, JobID: jobID, Outputs: stored}
	// A result that is JSON is kept as JSON, and anything else as text.
	if json.Valid(result) {
		out.Result = result
	} else if len(result) > 0 {
		out.Result = mustJSON(string(result))
	}
	return mustJSON(out)
}

func (Graph) Child(payload []byte, outputs [][]byte) (ChildJob, error) {
	var task graphTask
	json.Unmarshal(payload, &task) // a payload this workload made
	step := task.Step
	done := make(map[string]map[string]any, len(task.After))
	for _, at := range task.After {
		var output map[string]any
		json.Unmarshal(outputs[at], &output) // an output this workload made
		name, _ := output["step"].(string)
		done[name] = output
	}
	child := ChildJob{Name: step.Name, Workload: step.Workload, Tasks: step.Tasks, Timeout: time.Duration(step.TimeoutSeconds) * time.Second}
	if len(step.Params) == 0 {
		return child, nil
	}
	var params any
	json.Unmarshal(step.Params, &params) // they were JSON when the graph was taken in
	filled, err := fill(params, done)
	if err != nil {
		return child, fmt.Errorf("step %q: %w", step.Name, err)
	}
	child.Params = mustJSON(filled)
	return child, nil
}

// fill puts what earlier steps produced where a step's parameters refer to
// it.
func fill(v any, done map[string]map[string]any) (any, error) {
	switch v := v.(type) {
	case map[string]any:
		for key, value := range v {
			filled, err := fill(value, done)
			if err != nil {
				return nil, err
			}
			v[key] = filled
		}
	case []any:
		for i, value := range v {
			filled, err := fill(value, done)
			if err != nil {
				return nil, err
			}
			v[i] = filled
		}
	case string:
		// A string that is one reference and nothing else becomes the
		// value referred to, whatever its kind.
		if whole := reference.FindStringSubmatch(v); whole != nil && whole[0] == v {
			return referred(whole, done)
		}
		var failed error
		filled := reference.ReplaceAllStringFunc(v, func(found string) string {
			value, err := referred(reference.FindStringSubmatch(found), done)
			if err != nil {
				failed = err
				return found
			}
			if text, is := value.(string); is {
				return text
			}
			return string(mustJSON(value))
		})
		return filled, failed
	}
	return v, nil
}

// referred returns what a reference, taken apart, refers to.
func referred(parts []string, done map[string]map[string]any) (any, error) {
	var value any = done[parts[1]][parts[2]]
	for _, field := range strings.Split(strings.TrimPrefix(parts[3], "."), ".") {
		if field == "" {
			break
		}
		switch in := value.(type) {
		case map[string]any:
			value = in[field]
		case []any:
			at, err := strconv.Atoi(field)
			if err != nil || at < 0 || at >= len(in) {
				value = nil
			} else {
				value = in[at]
			}
		default:
			value = nil
		}
	}
	if value == nil {
		return nil, fmt.Errorf("%s refers to something step %q did not produce", parts[0], parts[1])
	}
	return value, nil
}
