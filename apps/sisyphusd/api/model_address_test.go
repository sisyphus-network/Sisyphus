package api

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/sisyphus-network/Sisyphus/packages/nodedb"
	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
)

func TestAModelServiceAddressIsShownWithoutItsCredentials(t *testing.T) {
	for address, want := range map[string]string{
		"":                                   "",
		"https://api.openai.com/v1":          "https://api.openai.com/v1",
		"http://127.0.0.1:11434":             "http://127.0.0.1:11434",
		"https://user:pass@models.test/v1":   "https://redacted@models.test/v1",
		"https://the-token@models.test":      "https://redacted@models.test",
		"https://models.test/v1?api-key=s&v": "https://models.test/v1?api-key=redacted&v=redacted",
		"https://models.test/v1?":            "https://models.test/v1",
		"https://models.test/v1#s":           "https://models.test/v1#redacted",
		"https://models.test/v1?a=%zz":       "https://models.test/v1?redacted",
		"https://models.test/%zz":            "redacted",
	} {
		if got := maskAddress(address); got != want {
			t.Errorf("%q is shown as %q, want %q", address, got, want)
		}
	}
}

func TestAModelServiceAddressIsTheOneHeldOrOneWithoutCredentials(t *testing.T) {
	held := "https://user:pass@models.test/v1"
	for _, given := range []string{held, "https://redacted@models.test/v1"} {
		if got, err := modelAddress(given, "", "https://other.test", held); err != nil || got != held {
			t.Errorf("%q means %q, %v, want the held %q", given, got, err, held)
		}
	}
	for _, given := range []string{"", "https://models.test/v1"} {
		if got, err := modelAddress(given, held); err != nil || got != given {
			t.Errorf("%q means %q, %v, want itself", given, got, err)
		}
	}
	for _, given := range []string{"https://user:other@models.test/v1", "https://models.test/v1?key=s", "https://models.test/v1#s", "https://models.test/%zz"} {
		if _, err := modelAddress(given, held); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%q: %v, want it refused", given, err)
		}
	}
}

// holding is a planner whose configuration can be set as well as read.
type holding struct{ profiledAssistant }

func (h *holding) SetModelConfig(cfg nodedb.ModelConfig) error {
	h.config = cfg
	return nil
}

func TestModelServiceAddressesAreMaskedEverywhereTheNodeShowsThem(t *testing.T) {
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer the-token"))
	secret := "https://user:pass@models.test/v1"
	shown := "https://redacted@models.test/v1"
	planner := &holding{profiledAssistant{
		config:   nodedb.ModelConfig{Provider: "openai", BaseURL: secret, Model: "m", APIKey: "k"},
		profiles: []nodedb.ModelProfile{{ID: "cloud", Name: "Cloud", Config: nodedb.ModelConfig{Provider: "openai", BaseURL: secret, Model: "m"}}},
	}}
	s := &localService{cfg: LocalConfig{Token: "the-token", Assistant: planner}}

	if got, err := s.GetModelConfig(ctx, &nodepb.GetModelConfigRequest{}); err != nil || got.GetBaseUrl() != shown {
		t.Errorf("GetModelConfig = %v, %v", got, err)
	}
	if got, err := s.ListModelProfiles(ctx, &nodepb.ListModelProfilesRequest{}); err != nil || got.GetProfiles()[0].GetBaseUrl() != shown {
		t.Errorf("ListModelProfiles = %v, %v", got, err)
	}

	// What the app was shown comes back when it switches to the profile,
	// and the node keeps the address it holds.
	got, err := s.SaveModelProfile(ctx, &nodepb.SaveModelProfileRequest{Id: "cloud", Name: "Cloud", Provider: "openai", BaseUrl: shown, Model: "m", Active: true})
	if err != nil || got.GetBaseUrl() != shown || planner.saved.Config.BaseURL != secret {
		t.Errorf("SaveModelProfile = %v, %v, saved %+v", got, err, planner.saved)
	}
	// A new profile made from the planner's configuration keeps its
	// address too.
	if _, err := s.SaveModelProfile(ctx, &nodepb.SaveModelProfileRequest{Id: "copy", Name: "Copy", Provider: "openai", BaseUrl: shown}); err != nil || planner.saved.Config.BaseURL != secret {
		t.Errorf("SaveModelProfile from the planner's = %v, saved %+v", err, planner.saved)
	}
	set, err := s.SetModelConfig(ctx, &nodepb.SetModelConfigRequest{Provider: "openai", BaseUrl: shown, Model: "m2", KeepApiKey: true})
	if err != nil || set.GetBaseUrl() != shown || planner.config.BaseURL != secret || planner.config.APIKey != "k" {
		t.Errorf("SetModelConfig = %v, %v, saved %+v", set, err, planner.config)
	}
	at, err := s.modelService(ctx, &nodepb.ModelService{Provider: "openai", BaseUrl: shown})
	if err != nil || at.BaseURL != secret {
		t.Errorf("modelService = %+v, %v", at, err)
	}

	// A new address with a credential in it is refused wherever it is given.
	given := "https://models.test/v1?api-key=s"
	if _, err := s.SaveModelProfile(ctx, &nodepb.SaveModelProfileRequest{Id: "new", Name: "New", Provider: "openai", BaseUrl: given}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("SaveModelProfile with a credential: %v", err)
	}
	if _, err := s.SetModelConfig(ctx, &nodepb.SetModelConfigRequest{Provider: "openai", BaseUrl: given}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("SetModelConfig with a credential: %v", err)
	}
	if _, err := s.modelService(ctx, &nodepb.ModelService{Provider: "openai", BaseUrl: given}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("modelService with a credential: %v", err)
	}

	// The node's own failures are passed on.
	planner.profilesErr = errAbsent
	if _, err := s.modelService(ctx, &nodepb.ModelService{Provider: "openai"}); status.Code(err) != codes.Internal {
		t.Errorf("modelService when the profiles cannot be read: %v", err)
	}
	planner.profilesErr = nil
	planner.configErr = errAbsent
	if _, err := s.modelService(ctx, &nodepb.ModelService{Provider: "openai"}); status.Code(err) != codes.Internal {
		t.Errorf("modelService when the configuration cannot be read: %v", err)
	}
	if _, err := s.SaveModelProfile(ctx, &nodepb.SaveModelProfileRequest{Id: "new", Name: "New", Provider: "openai", BaseUrl: shown}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("SaveModelProfile of another's address with no planner configuration: %v", err)
	}
}
