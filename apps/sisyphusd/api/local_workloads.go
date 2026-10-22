package api

import (
	"context"
	"encoding/json"
	"slices"

	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
)

// fieldKinds are the kinds of form field as the local API names them.
var fieldKinds = map[runtime.Kind]nodepb.WorkloadFieldKind{
	runtime.KindFile:    nodepb.WorkloadFieldKind_WORKLOAD_FIELD_KIND_FILE,
	runtime.KindInteger: nodepb.WorkloadFieldKind_WORKLOAD_FIELD_KIND_INTEGER,
	runtime.KindNumber:  nodepb.WorkloadFieldKind_WORKLOAD_FIELD_KIND_NUMBER,
	runtime.KindText:    nodepb.WorkloadFieldKind_WORKLOAD_FIELD_KIND_TEXT,
	runtime.KindChoice:  nodepb.WorkloadFieldKind_WORKLOAD_FIELD_KIND_CHOICE,
	runtime.KindToggle:  nodepb.WorkloadFieldKind_WORKLOAD_FIELD_KIND_TOGGLE,
	runtime.KindFiles:   nodepb.WorkloadFieldKind_WORKLOAD_FIELD_KIND_FILES,
}

// ListWorkloads lists the workloads the node can take jobs of: what each
// does, the form its parameters are asked for with, and how many of the
// connected workers offer it. Like the list of workers it needs no token,
// as it says what the pool can do and nothing of what anyone has done
// with it.
func (s *localService) ListWorkloads(context.Context, *nodepb.ListWorkloadsRequest) (*nodepb.ListWorkloadsResponse, error) {
	if err := s.jobsAvailable(); err != nil {
		return nil, err
	}
	workers := s.cfg.Jobs.Nodes()
	registry := s.workloads()
	res := &nodepb.ListWorkloadsResponse{}
	for _, name := range registry.Names() {
		workload := &nodepb.Workload{Name: name, Description: registry.Describe(name), Composite: registry.Composite(name)}
		for _, f := range registry.Form(name) {
			workload.Fields = append(workload.Fields, localField(f))
		}
		// A worker offers it whether or not it is busy now. None offers
		// a composite one, which no worker runs.
		for _, w := range workers {
			if slices.Contains(w.GetCapabilities().GetWorkloads(), name) {
				workload.WorkersRunningIt++
			}
		}
		res.Workloads = append(res.Workloads, workload)
	}
	return res, nil
}

// workloads returns the workloads the node knows, which are none if it
// was not told of any.
func (s *localService) workloads() *runtime.Registry {
	if s.cfg.Workloads == nil {
		return runtime.NewRegistry()
	}
	return s.cfg.Workloads
}

func localField(f runtime.Field) *nodepb.WorkloadField {
	field := &nodepb.WorkloadField{
		Name: f.Name, Kind: fieldKinds[f.Kind], Label: f.Label, Help: f.Help,
		Minimum: f.Minimum, Maximum: f.Maximum, Step: f.Step, Choices: f.Choices, Required: f.Required,
	}
	if f.Default != nil {
		// A default is a plain number, string or truth value, which always
		// encodes.
		encoded, _ := json.Marshal(f.Default)
		field.DefaultJson = string(encoded)
	}
	return field
}
