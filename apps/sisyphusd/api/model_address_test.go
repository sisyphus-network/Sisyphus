package api

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/sisyphus-network/Sisyphus/packages/nodedb"
	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
)

func TestAModelServiceAddressIsShownWithoutItsCredentials(t *testing.T) {
	for address, want := range map[string]string{
		"":                                   "",
		"https://api.openai.com/v1":          "https://api.openai.com/v1",
		"http://127.0.0.1:11434":             "http://127.0.0.1:11434",
		"https://user:pass@models.test/v1":   "https://redacted@models.test/v1",
		"https://the-token@models.test":      "https://redacted@models.test",
		"https://models.test/v1?api-key=s&v": "https://models.test/v1?redacted",
		"https://models.test/v1?sk-secret":   "https://models.test/v1?redacted",
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
	for _, given := range []string{"", "https://models.test/v1", "https://models.test/%zz"} {
		if got, err := modelAddress(given, held); err != nil || got != given {
			t.Errorf("%q means %q, %v, want itself", given, got, err)
		}
	}
	for _, given := range []string{"https://user:other@models.test/v1", "https://models.test/v1?key=s", "https://models.test/v1#s"} {
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

// unsavable is a lending store that reads but cannot save.
type unsavable struct{ held nodedb.ModelLending }

func (u unsavable) ModelLending() (nodedb.ModelLending, error) { return u.held, nil }
func (unsavable) SetModelLending(nodedb.ModelLending) error {
	return errors.New("save model lending: disk gone")
}

func TestTheOllamaAModelIsLentFromIsShownWithoutItsCredentials(t *testing.T) {
	was := inspect
	t.Cleanup(func() { inspect = was })
	var asked string
	inspect = func(_ context.Context, url string, _ runtime.Owner) (runtime.Server, error) {
		asked = url
		return runtime.Server{Version: "0.21.0"}, nil
	}
	secret, shown := "http://user:pass@ollama.test:11434", "http://redacted@ollama.test:11434"
	store := newFileList(t)
	if err := store.SetModelLending(nodedb.ModelLending{Enabled: true, URL: secret}); err != nil {
		t.Fatal(err)
	}
	client := serveLocal(t, LocalConfig{Lending: store, Lent: nodedb.ModelLending{Enabled: true, URL: secret}})
	ctx := withToken("the-token")

	state, err := client.GetModelLending(ctx, &nodepb.GetModelLendingRequest{})
	if err != nil || state.GetSaved().GetOllamaUrl() != shown || state.GetRunning().GetOllamaUrl() != shown {
		t.Fatalf("GetModelLending = %v, %v", state, err)
	}
	// Given back as shown, the address stays the one held.
	state, err = client.SetModelLending(ctx, &nodepb.SetModelLendingRequest{Lending: &nodepb.ModelLending{Enabled: true, OllamaUrl: shown, Threads: 4}})
	if err != nil || state.GetSaved().GetOllamaUrl() != shown {
		t.Errorf("SetModelLending with the address shown = %v, %v", state, err)
	}
	if saved, _ := store.ModelLending(); saved.URL != secret || saved.Threads != 4 {
		t.Errorf("saved %+v", saved)
	}
	if _, err := client.InspectOllama(ctx, &nodepb.InspectOllamaRequest{Url: shown}); err != nil || asked != secret {
		t.Errorf("InspectOllama with the address shown asked %q: %v", asked, err)
	}

	given := "http://other:pass@ollama.test:11434"
	if _, err := client.SetModelLending(ctx, &nodepb.SetModelLendingRequest{Lending: &nodepb.ModelLending{Enabled: true, OllamaUrl: given}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("SetModelLending with a credential: %v", err)
	}
	if _, err := client.InspectOllama(ctx, &nodepb.InspectOllamaRequest{Url: given}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("InspectOllama with a credential: %v", err)
	}

	broken := serveLocal(t, LocalConfig{Lending: brokenLending{}})
	if _, err := broken.InspectOllama(ctx, &nodepb.InspectOllamaRequest{Url: "http://127.0.0.1:11434"}); status.Code(err) != codes.Internal {
		t.Errorf("InspectOllama with a broken store: %v", err)
	}
	full := serveLocal(t, LocalConfig{Lending: unsavable{}})
	if _, err := full.SetModelLending(ctx, &nodepb.SetModelLendingRequest{Lending: &nodepb.ModelLending{}}); status.Code(err) != codes.Internal {
		t.Errorf("SetModelLending to a store that cannot save: %v", err)
	}
}
