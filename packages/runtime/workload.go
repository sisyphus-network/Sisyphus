// Package runtime defines how Sisyphus workloads are split, executed and
// aggregated. In the walking skeleton workloads are compiled into the daemon;
// containerised workloads come later.
package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
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

// MaxTasks is the most tasks one job may be split into. The coordinator
// asks for no more than this, and a workload whose parameters choose their
// own count refuses to go past it.
const MaxTasks = 10_000

// ResultBlobs names task-produced blobs referenced by a workload's aggregate
// result. Such blobs are part of the result and must be retained with it.
type ResultBlobs interface {
	ResultBlobs(result []byte) []string
}

// Described is a workload that can say, for someone deciding whether and
// how to use it, what it does and what parameters it takes.
type Described interface {
	Describe() string
}

// Varying is a workload whose output for one payload may differ from one
// run to the next, or from one machine to another: a model's reply, which
// is sampled and whose arithmetic differs between a processor and a
// graphics card, or whatever a container's image does. Its work cannot be
// checked by comparing the bytes two runs return, which is how a job is
// verified or audited, since honest workers would disagree. Varies says
// whether that is so, unless it is also Comparing: then its results are
// compared with its own Compare, and it is checked like any other.
type Varying interface {
	Varies() bool
}

// Comparing is a workload that says itself whether two outputs of one task
// are the same result, for work whose honest results agree only to some
// precision: numbers computed on different processors that differ in their
// last digits. Two outputs of one task are compared with Compare, where the
// workload has it, and are otherwise the same only if their bytes are; see
// Compare below.
//
// The tolerance is the workload's to choose, and it is all that stands
// between a verified result and a made-up one: it must be tight enough that
// a result that was not computed, or was computed wrongly, falls outside
// it. Compare must be symmetric, and must hold for an output compared with
// itself; it need not be transitive, and the coordinator does not take it
// to be.
type Comparing interface {
	// Compare reports whether a and b, two outputs of the same task
	// payload, are the same result, and if they are not, says why: what
	// differed, and by how much, for whoever looks into it. It must not
	// fail on outputs it cannot read: those are not the same.
	Compare(a, b []byte) (same bool, why string)
}

// Compare reports whether two outputs of one task of a workload are the
// same result: by the workload's own Compare, if it is Comparing, and
// otherwise by their bytes. Outputs whose bytes are equal are the same
// either way. why says what differed, when the workload said; it is empty
// for outputs whose bytes alone were compared.
func Compare(w Workload, a, b []byte) (same bool, why string) {
	if bytes.Equal(a, b) {
		return true, ""
	}
	if c, ok := w.(Comparing); ok {
		return c.Compare(a, b)
	}
	return false, ""
}

// Checkable reports whether a workload's tasks can be checked by comparing
// two runs' results, as verify and audit_share do: it gives the same result
// every time, or it compares its results itself.
func Checkable(w Workload) bool {
	varying, varies := w.(Varying)
	_, compares := w.(Comparing)
	return !varies || !varying.Varies() || compares
}

// Needy is a workload whose tasks cannot run on just any worker that has
// the workload: they need something more of it, which depends on the job.
type Needy interface {
	// Needs returns the labels a worker must have to be given the tasks
	// of a job with these parameters.
	Needs(params []byte) []string
}

// Carded is a workload whose tasks may need graphics cards of the worker
// that runs them, depending on the job.
type Carded interface {
	// GPUsNeeded returns how many graphics cards each task of a job with
	// these parameters holds while it runs: none for one that needs none.
	GPUsNeeded(params []byte) int
}

// GPUsNeeded returns how many graphics cards each task of a job of the
// named workload with the given parameters needs.
func (r *Registry) GPUsNeeded(name string, params []byte) int {
	if c, ok := r.workloads[name].(Carded); ok {
		return c.GPUsNeeded(params)
	}
	return 0
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

// contained is a workload whose tasks run in containers, held in as its
// worker's owner says.
type contained interface{ confinement() Confinement }

func (c Container) confinement() Confinement  { return c.Confined }
func (t Transcode) confinement() Confinement  { return t.Confined }
func (m MD) confinement() Confinement         { return m.Confined }
func (r Render) confinement() Confinement     { return r.Confined }
func (t Transcribe) confinement() Confinement { return t.Confined }
func (q Qchem) confinement() Confinement      { return q.Confined }
func (o OCR) confinement() Confinement        { return o.Confined }
func (t Traj) confinement() Confinement       { return t.Confined }
func (p Prep) confinement() Confinement       { return p.Confined }
func (im Images) confinement() Confinement    { return im.Confined }

// InContainers reports whether the named workload runs its tasks in
// containers: on a stranger's image, or on a stranger's input.
func (r *Registry) InContainers(name string) bool {
	_, ok := r.workloads[name].(contained)
	return ok
}

// Offers returns the labels a worker has by running these workloads, each
// once, though two workloads offer it, as forecast and forecasts both offer
// the Chronos-Bolt and TimesFM images.
func (r *Registry) Offers(ctx context.Context) []string {
	var labels []string
	for _, name := range r.Names() {
		if o, ok := r.workloads[name].(Offering); ok {
			for _, label := range o.Offers(ctx) {
				if !slices.Contains(labels, label) {
					labels = append(labels, label)
				}
			}
		}
	}
	return labels
}

// Composite reports whether the named workload is one whose tasks the
// coordinator carries out itself, as jobs, so that no worker runs it.
func (r *Registry) Composite(name string) bool {
	_, is := r.workloads[name].(Composite)
	return is
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
	return NewRegistry(Primes{}, WordCount{}, Forecast{}, Forecasts{})
}

// WithContainers returns the built-in workloads and those that run in
// containers: any image a job names, and the ones made of a fixed image.
// It also has the workloads that use language models, as a coordinator has
// them: able to take such a job in and pass it on, with no models of its
// own to run it.
func WithContainers() *Registry {
	return ContainersConfined(Confinement{})
}

// ContainersConfined returns what WithContainers does, with the containers
// held in as a worker's owner asked.
func ContainersConfined(confined Confinement) *Registry {
	return NewRegistry(Primes{}, WordCount{}, Forecast{Containers: true, Confined: confined}, Forecasts{Containers: true, Confined: confined}, Container{Confined: confined}, Transcode{Confined: confined}, MD{Confined: confined}, Render{Confined: confined}, Transcribe{Confined: confined}, Qchem{Confined: confined}, OCR{Confined: confined}, Traj{Confined: confined}, Prep{Confined: confined}, Images{Confined: confined}).With(WithModels("", Owner{})...)
}

// WithModels returns the workloads that use the language models of the
// model server at url, as far as its owner allows: a worker's own, or none
// for a node that only takes such jobs in.
func WithModels(url string, owner Owner) []Workload {
	return []Workload{Chat{URL: url, Owner: owner}, Prompts{URL: url, Owner: owner}, Embed{URL: url, Owner: owner}}
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
