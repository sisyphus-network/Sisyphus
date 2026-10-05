// Package runtime defines how Sisyphus workloads are split, executed and
// aggregated. In the walking skeleton workloads are compiled into the daemon;
// containerised workloads come later.
package runtime

import (
	"context"
	"fmt"
	"sort"
)

// Workload is a unit of computation the network knows how to run.
//
// Split and Aggregate run on the coordinator. Execute runs on a worker.
type Workload interface {
	Name() string
	// Split divides params into at most parts task payloads. Asking for one
	// part must return a single payload covering the whole job.
	Split(params []byte, parts int) ([][]byte, error)
	// Execute runs one task payload and returns its output. It must stop
	// promptly when ctx is cancelled.
	Execute(ctx context.Context, payload []byte) ([]byte, error)
	// Aggregate combines task outputs, given in task order, into the job
	// result.
	Aggregate(outputs [][]byte) ([]byte, error)
}

// Registry is a fixed set of workloads, looked up by name.
type Registry struct {
	workloads map[string]Workload
}

func NewRegistry(workloads ...Workload) *Registry {
	r := &Registry{workloads: make(map[string]Workload, len(workloads))}
	for _, w := range workloads {
		r.workloads[w.Name()] = w
	}
	return r
}

// Builtin returns the workloads shipped with the daemon.
func Builtin() *Registry {
	return NewRegistry(Primes{})
}

func (r *Registry) Get(name string) (Workload, error) {
	w, ok := r.workloads[name]
	if !ok {
		return nil, fmt.Errorf("unknown workload %q", name)
	}
	return w, nil
}

func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.workloads))
	for name := range r.workloads {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
