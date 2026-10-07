// Package runtime defines how Sisyphus workloads are split, executed and
// aggregated. In the walking skeleton workloads are compiled into the daemon;
// containerised workloads come later.
package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/ipfs/go-cid"

	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// Blobs gives a workload access to content-addressed data: bulk inputs and
// outputs that are too large to pass inline. On a worker, opening a blob
// fetches it from the coordinator if it is not already held locally, and
// storing one also uploads it to the coordinator.
type Blobs interface {
	Open(ctx context.Context, c cid.Cid) (storage.Blob, error)
	Put(ctx context.Context, r io.Reader) (cid.Cid, error)
}

// Workload is a unit of computation the network knows how to run.
//
// Split and Aggregate run on the coordinator. Execute runs on a worker.
// Parameters, payloads and outputs are small and travel inline; anything
// large goes through Blobs and is referred to by CID.
type Workload interface {
	Name() string
	// Split divides params into at most parts task payloads. Asking for one
	// part must return a single payload covering the whole job.
	Split(ctx context.Context, blobs Blobs, params []byte, parts int) ([][]byte, error)
	// Execute runs one task payload and returns its output. It must stop
	// promptly when ctx is cancelled.
	Execute(ctx context.Context, blobs Blobs, payload []byte) ([]byte, error)
	// Aggregate combines task outputs, given in task order, into the job
	// result.
	Aggregate(ctx context.Context, blobs Blobs, outputs [][]byte) ([]byte, error)
}

// Described is a workload that can say, for someone deciding whether and
// how to use it, what it does and what parameters it takes.
type Described interface {
	Describe() string
}

// Needy is a workload whose tasks cannot run on just any worker that has
// the workload: they need something more of it, which depends on the job.
type Needy interface {
	// Needs returns the labels a worker must have to be given the tasks
	// of a job with these parameters.
	Needs(params []byte) []string
}

// Offering is a workload that gives the worker running it something a job
// may need, which it can name.
type Offering interface {
	// Offers returns the labels the worker has by running this workload.
	Offers(ctx context.Context) []string
}

// Needs returns the labels a worker must have to be given the tasks of a
// job of the named workload with the given parameters.
func (r *Registry) Needs(name string, params []byte) []string {
	if n, ok := r.workloads[name].(Needy); ok {
		return n.Needs(params)
	}
	return nil
}

// Offers returns the labels a worker has by running these workloads.
func (r *Registry) Offers(ctx context.Context) []string {
	var labels []string
	for _, name := range r.Names() {
		if o, ok := r.workloads[name].(Offering); ok {
			labels = append(labels, o.Offers(ctx)...)
		}
	}
	return labels
}

// With returns a registry of these workloads and more. One of the same
// name as one already there takes its place.
func (r *Registry) With(more ...Workload) *Registry {
	all := make([]Workload, 0, len(r.workloads)+len(more))
	for _, w := range r.workloads {
		all = append(all, w)
	}
	return NewRegistry(append(all, more...)...)
}

// Describe returns what the workload with the given name says of itself, or
// nothing if it says nothing or is not there.
func (r *Registry) Describe(name string) string {
	if d, ok := r.workloads[name].(Described); ok {
		return d.Describe()
	}
	return ""
}

// mustJSON encodes a value of a type that always encodes: the plain structs
// and string-keyed maps workloads pass around. It panics on anything else,
// which would be a programming error.
func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
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

// Builtin returns the workloads shipped with the daemon that any node can
// run, having nothing to run them with but itself.
func Builtin() *Registry {
	return NewRegistry(Primes{}, WordCount{})
}

// WithContainers returns the built-in workloads and those that run in
// containers: any image a job names, and the ones made of a fixed image.
// It also has chat, as a coordinator has it: able to take such a job in
// and pass it on, with no models of its own to run it.
func WithContainers() *Registry {
	return NewRegistry(Primes{}, WordCount{}, Container{}, Transcode{}, Chat{})
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
