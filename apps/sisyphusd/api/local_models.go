package api

import (
	"context"

	"github.com/sisyphus-network/Sisyphus/packages/ai"
	"github.com/sisyphus-network/Sisyphus/packages/nodedb"
	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
)

// ListProviders lists the kinds of model service there are. It is the same
// on every node, planner or not.
func (s *localService) ListProviders(context.Context, *nodepb.ListProvidersRequest) (*nodepb.ListProvidersResponse, error) {
	out := &nodepb.ListProvidersResponse{}
	for _, k := range ai.Kinds() {
		out.Providers = append(out.Providers, &nodepb.Provider{
			Id: k.ID, Name: k.Name, About: k.About, DefaultUrl: k.Place,
			NeedsKey: nodepb.Support(k.Key), FetchesModels: k.Fetches,
		})
	}
	return out, nil
}

// modelService works out which service a request about models is for: nil
// for the configured one, which anyone may ask about, or the one the
// request describes, which makes the node call wherever it says and so
// needs the node's token.
func (s *localService) modelService(ctx context.Context, named *nodepb.ModelService) (*nodedb.ModelConfig, error) {
	if s.cfg.Assistant == nil {
		return nil, errNoPlanner
	}
	if named.GetProvider() == "" {
		return nil, nil
	}
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	at := &nodedb.ModelConfig{Provider: named.GetProvider(), BaseURL: named.GetBaseUrl(), APIKey: named.GetApiKey()}
	if named.GetKeepApiKey() {
		saved, _, err := s.cfg.Assistant.ModelConfig()
		if err != nil {
			return nil, asError(err)
		}
		// The saved key goes only where it was saved for.
		if sameService(saved, *at) {
			at.APIKey = saved.APIKey
		}
	}
	return at, nil
}

// ListModels asks a service which models it offers.
func (s *localService) ListModels(ctx context.Context, req *nodepb.ListModelsRequest) (*nodepb.ListModelsResponse, error) {
	at, err := s.modelService(ctx, req.GetService())
	if err != nil {
		return nil, err
	}
	models, err := s.cfg.Assistant.Models(ctx, at)
	if err != nil {
		return nil, asError(err)
	}
	out := &nodepb.ListModelsResponse{}
	for _, m := range models {
		out.Models = append(out.Models, m.Name)
		out.Details = append(out.Details, &nodepb.Model{Name: m.Name, Label: m.Label, SizeBytes: uint64(m.Size), Tools: nodepb.Support(m.Tools)})
	}
	return out, nil
}

// PullModel fetches a model to the machine its service runs on, sending
// how far it has got.
func (s *localService) PullModel(req *nodepb.PullModelRequest, stream nodepb.NodeService_PullModelServer) error {
	ctx := stream.Context()
	if err := s.authorize(ctx); err != nil {
		return err
	}
	at, err := s.modelService(ctx, req.GetService())
	if err != nil {
		return err
	}
	return asError(s.cfg.Assistant.PullModel(ctx, at, req.GetModel(), func(p ai.Progress) {
		// A send that fails means whoever asked has gone, which ends ctx
		// and with it the fetching.
		stream.Send(&nodepb.PullModelProgress{Status: p.Status, CompletedBytes: uint64(p.Done), TotalBytes: uint64(p.Total)})
	}))
}

// RemoveModel deletes a fetched model.
func (s *localService) RemoveModel(ctx context.Context, req *nodepb.RemoveModelRequest) (*nodepb.RemoveModelResponse, error) {
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	at, err := s.modelService(ctx, req.GetService())
	if err != nil {
		return nil, err
	}
	if err := s.cfg.Assistant.RemoveModel(ctx, at, req.GetModel()); err != nil {
		return nil, asError(err)
	}
	return &nodepb.RemoveModelResponse{}, nil
}

// sameService reports whether two configurations are of one model service,
// whichever model of it each names.
func sameService(a, b nodedb.ModelConfig) bool {
	return ai.SameService(ai.Config{Provider: a.Provider, BaseURL: a.BaseURL}, ai.Config{Provider: b.Provider, BaseURL: b.BaseURL})
}
