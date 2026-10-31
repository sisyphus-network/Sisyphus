package runtime

import (
	"fmt"
	"slices"
	"strings"
)

// The groups of workloads a worker's owner can name, besides the workloads
// themselves, to say which their machine takes: see Accept.
const (
	// GroupDefault is every workload but the opt-in ones: what a worker
	// takes when its owner says nothing.
	GroupDefault = "default"
	// GroupContainers is the workloads whose tasks run in containers: any
	// image a job names, or a fixed one given a stranger's input. On a node
	// that runs containers, forecast and forecasts are in it too, since
	// there they run the pretrained families' images.
	GroupContainers = "containers"
	// GroupModels is the workloads that ask the worker's model server.
	GroupModels = "models"
)

// Grouped is a workload in groups of its own, besides those it is in for
// running in containers or using models, which a worker's owner may name
// to take or refuse it with the rest of its group.
type Grouped interface {
	Groups() []string
}

// OptIn is a workload a worker takes only when its owner names it, or a
// group it is in, among the workloads to allow: one that costs the owner
// more than the machine's time, as mining does in power, heat and noise. It
// is not in GroupDefault, and a worker whose owner says nothing never takes
// it.
type OptIn interface {
	OptIn() bool
}

// modelled is a workload whose tasks ask the worker's model server.
type modelled interface{ modelGroup() string }

func (Chat) modelGroup() string    { return GroupModels }
func (Prompts) modelGroup() string { return GroupModels }
func (Embed) modelGroup() string   { return GroupModels }

// containerBacked is a workload that runs container images only on a node
// that runs containers, and is in GroupContainers there.
type containerBacked interface{ runsImages() bool }

func (f Forecast) runsImages() bool  { return f.Containers }
func (f Forecasts) runsImages() bool { return f.Containers }

// optIn reports whether a worker takes w only when its owner says so.
func optIn(w Workload) bool {
	o, ok := w.(OptIn)
	return ok && o.OptIn()
}

// GroupsOf returns the groups w is in, by what it is and what it says of
// itself.
func GroupsOf(w Workload) []string {
	var groups []string
	if !optIn(w) {
		groups = append(groups, GroupDefault)
	}
	if _, ok := w.(contained); ok {
		groups = append(groups, GroupContainers)
	}
	if c, ok := w.(containerBacked); ok && c.runsImages() {
		groups = append(groups, GroupContainers)
	}
	if m, ok := w.(modelled); ok {
		groups = append(groups, m.modelGroup())
	}
	if g, ok := w.(Grouped); ok {
		groups = append(groups, g.Groups()...)
	}
	return groups
}

// Acceptance is which workloads a worker's owner lets their machine take.
// The zero value takes every workload but the opt-in ones.
type Acceptance struct {
	allow, deny []string
}

// Accept makes an Acceptance of what a worker's owner allowed and denied,
// each a list of workload names and groups. A workload is taken if it, or
// a group it is in, is allowed, and neither it nor any group it is in is
// denied: denial wins. Allowing nothing allows GroupDefault. Every name
// must be a workload or a group of one of known, the workloads the node
// knows of, or one of the groups above; anything else is an error, so that
// a misspelt denial does not leave the machine taking what its owner meant
// to refuse.
func Accept(known *Registry, allow, deny []string) (Acceptance, error) {
	names := []string{GroupDefault, GroupContainers, GroupModels}
	for _, name := range known.Names() {
		names = append(names, name)
		names = append(names, GroupsOf(known.workloads[name])...)
	}
	for _, name := range slices.Concat(allow, deny) {
		if !slices.Contains(names, name) {
			slices.Sort(names)
			return Acceptance{}, fmt.Errorf("%q is neither a workload nor a group of them; known are %s", name, strings.Join(slices.Compact(names), ", "))
		}
	}
	return Acceptance{allow: slices.Clone(allow), deny: slices.Clone(deny)}, nil
}

// Accepts reports whether a worker takes tasks of w. An opt-in workload is
// allowed only by its name or a group of its own: allowing containers, say,
// does not allow mining, though mining runs in containers, since its owner
// may not have meant to lend the machine's power to it. Any group it is in
// denies it.
func (a Acceptance) Accepts(w Workload) bool {
	named := func(list, groups []string) bool {
		if slices.Contains(list, w.Name()) {
			return true
		}
		for _, group := range groups {
			if slices.Contains(list, group) {
				return true
			}
		}
		return false
	}
	allow := a.allow
	if len(allow) == 0 {
		allow = []string{GroupDefault}
	}
	allowedBy := GroupsOf(w)
	if optIn(w) {
		allowedBy = nil
		if g, ok := w.(Grouped); ok {
			allowedBy = g.Groups()
		}
	}
	return named(allow, allowedBy) && !named(a.deny, GroupsOf(w))
}

// Accepted returns the registry of those of r's workloads that a accepts.
func (r *Registry) Accepted(a Acceptance) *Registry {
	var kept []Workload
	for _, w := range r.workloads {
		if a.Accepts(w) {
			kept = append(kept, w)
		}
	}
	return NewRegistry(kept...)
}

// Unmatched returns those of the names and groups a allows that answer to
// none of r's workloads: workloads the node knows of but was not started
// able to run, as transcode is without containers, and groups with none of
// those in them. Allowing them takes nothing.
func (a Acceptance) Unmatched(r *Registry) []string {
	var unmatched []string
	for _, name := range a.allow {
		matched := false
		for _, w := range r.workloads {
			matched = matched || w.Name() == name || slices.Contains(GroupsOf(w), name)
		}
		if !matched {
			unmatched = append(unmatched, name)
		}
	}
	return unmatched
}
