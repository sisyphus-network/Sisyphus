package api

import (
	"context"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sisyphus-network/Sisyphus/packages/nodedb"
	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
)

// LendingStore keeps how a machine lends its models. A nodedb.DB is one.
type LendingStore interface {
	ModelLending() (nodedb.ModelLending, error)
	SetModelLending(nodedb.ModelLending) error
}

// inspect asks an Ollama what it has. Tests put another in its place.
var inspect = runtime.Inspect

// GetModelLending says how the machine lends its models, as saved and as
// the node runs now. Where its Ollama is, and what it offers, are the
// owner's business, so it needs the token.
func (s *localService) GetModelLending(ctx context.Context, _ *nodepb.GetModelLendingRequest) (*nodepb.ModelLendingState, error) {
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	if s.cfg.Lending == nil {
		return nil, errNoLending
	}
	saved, err := s.cfg.Lending.ModelLending()
	if err != nil {
		return nil, asError(err)
	}
	return s.lendingState(saved), nil
}

// SetModelLending saves how the machine lends its models. If the worker is
// already lending from the same Ollama, its allowlist and request limits
// take effect immediately; changing whether it lends or where Ollama is
// still requires a restart.
func (s *localService) SetModelLending(ctx context.Context, req *nodepb.SetModelLendingRequest) (*nodepb.ModelLendingState, error) {
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	if s.cfg.Lending == nil {
		return nil, errNoLending
	}
	l := fromLendingPB(req.GetLending())
	if err := checkLending(l); err != nil {
		return nil, err
	}
	if err := s.cfg.Lending.SetModelLending(l); err != nil {
		return nil, asError(err)
	}
	s.lendingMu.Lock()
	running := s.cfg.Lent
	if !s.cfg.LentByFlags && sameLendingRuntime(l, running) {
		if l.Enabled {
			if s.cfg.LiveLending != nil {
				s.cfg.LiveLending.Update(lendingOwner(l))
				s.cfg.Lent = l
			}
		} else {
			// There is no active model server to reconfigure while lending is
			// off; remember the current saved values without a pointless restart.
			s.cfg.Lent = l
		}
	}
	s.lendingMu.Unlock()
	return s.lendingState(l), nil
}

// InspectOllama asks an Ollama what it is and which of its models would be
// lent. One that cannot be reached is an answer, not a failed call.
func (s *localService) InspectOllama(ctx context.Context, req *nodepb.InspectOllamaRequest) (*nodepb.InspectOllamaResponse, error) {
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	if err := checkOllamaURL(req.GetUrl()); err != nil {
		return nil, err
	}
	server, err := inspect(ctx, req.GetUrl(), runtime.Owner{Only: trimmed(req.GetOnly())})
	if err != nil {
		return &nodepb.InspectOllamaResponse{Error: err.Error()}, nil
	}
	response := &nodepb.InspectOllamaResponse{Version: server.Version, Models: server.Models}
	for _, m := range server.Loaded {
		response.Loaded = append(response.Loaded, &nodepb.LoadedModel{Name: m.Model, OnCardPercent: uint32(m.OnCard)})
	}
	return response, nil
}

var errNoLending = status.Error(codes.FailedPrecondition, "this node keeps no settings for lending models")

func (s *localService) lendingState(saved nodedb.ModelLending) *nodepb.ModelLendingState {
	s.lendingMu.RLock()
	running, byFlags, live := s.cfg.Lent, s.cfg.LentByFlags, s.cfg.LiveLending
	s.lendingMu.RUnlock()
	restartNeeded := !byFlags && !sameLendingRuntime(saved, running)
	if !byFlags && saved.Enabled && sameLendingRuntime(saved, running) && live == nil && !reflect.DeepEqual(saved, running) {
		restartNeeded = true
	}
	return &nodepb.ModelLendingState{
		Saved:         lendingPB(saved),
		Running:       lendingPB(running),
		SetByFlags:    byFlags,
		RestartNeeded: restartNeeded,
	}
}

func sameLendingRuntime(saved, running nodedb.ModelLending) bool {
	return saved.Enabled == running.Enabled && (!saved.Enabled || saved.URL == running.URL)
}

func lendingOwner(l nodedb.ModelLending) runtime.Owner {
	owner := runtime.Owner{Only: append([]string(nil), l.Only...), Options: map[string]any{}, KeepAlive: l.KeepAlive}
	for setting, value := range map[string]int{"num_gpu": l.GPULayers, "num_thread": l.Threads, "num_ctx": l.Context} {
		if value > 0 || (setting == "num_gpu" && value == 0) {
			owner.Options[setting] = value
		}
	}
	return owner
}

// checkLending refuses settings Ollama could not use. Settings that lend
// nothing are kept as they are, so that turning lending off loses none.
func checkLending(l nodedb.ModelLending) error {
	if l.Enabled {
		if err := checkOllamaURL(l.URL); err != nil {
			return err
		}
	}
	switch {
	case l.GPULayers < -1:
		return status.Error(codes.InvalidArgument, "the layers on graphics cards are a number, 0 for none, or -1 for as many as fit")
	case l.Threads < 0 || l.Context < 0:
		return status.Error(codes.InvalidArgument, "threads and context are numbers, or 0 for Ollama to decide")
	}
	if k := l.KeepAlive; k != "" {
		if _, err := time.ParseDuration(k); err != nil {
			if _, err := strconv.Atoi(k); err != nil {
				return status.Error(codes.InvalidArgument, "how long a model stays loaded is a time such as 10m or 1h, or 0 to unload it at once")
			}
		}
	}
	return nil
}

func checkOllamaURL(address string) error {
	if u, err := url.Parse(address); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return status.Error(codes.InvalidArgument, "Ollama's address is a web address, such as http://127.0.0.1:11434")
	}
	return nil
}

// trimmed drops the spaces around each name and the names that are empty,
// so that "a, ,b" offers a and b. None at all is nil, which offers all.
func trimmed(names []string) []string {
	var kept []string
	for _, name := range names {
		if name = strings.TrimSpace(name); name != "" {
			kept = append(kept, name)
		}
	}
	return kept
}

func fromLendingPB(l *nodepb.ModelLending) nodedb.ModelLending {
	return nodedb.ModelLending{Enabled: l.GetEnabled(), URL: strings.TrimSpace(l.GetOllamaUrl()), Only: trimmed(l.GetOnly()),
		GPULayers: int(l.GetGpuLayers()), Threads: int(l.GetThreads()), Context: int(l.GetContext()), KeepAlive: strings.TrimSpace(l.GetKeepAlive())}
}

func lendingPB(l nodedb.ModelLending) *nodepb.ModelLending {
	return &nodepb.ModelLending{Enabled: l.Enabled, OllamaUrl: l.URL, Only: l.Only,
		GpuLayers: int32(l.GPULayers), Threads: int32(l.Threads), Context: int32(l.Context), KeepAlive: l.KeepAlive}
}
