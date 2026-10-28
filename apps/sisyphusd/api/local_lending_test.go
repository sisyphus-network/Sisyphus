package api

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sisyphus-network/Sisyphus/packages/nodedb"
	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
)

// brokenLending is a store whose disk has failed.
type brokenLending struct{}

func (brokenLending) ModelLending() (nodedb.ModelLending, error) {
	return nodedb.ModelLending{}, errors.New("load model lending: disk gone")
}
func (brokenLending) SetModelLending(nodedb.ModelLending) error {
	return errors.New("save model lending: disk gone")
}

func TestSharedStateSnapshotReportsModelLendingReadFailure(t *testing.T) {
	service := &localService{cfg: LocalConfig{Lending: brokenLending{}}}
	event := service.sharedStateSnapshot(withToken("the-token"), nodepb.SharedStateDomain_SHARED_STATE_DOMAIN_MODEL_LENDING)
	if event.GetError() == "" || event.GetModelLending() != nil {
		t.Fatalf("model-lending snapshot = %v; want an error without state", event)
	}
}

func TestModelLendingIsSavedForTheNextStartAndSaysSo(t *testing.T) {
	running := nodedb.ModelLending{GPULayers: -1}
	client := serveLocal(t, LocalConfig{Lending: newFileList(t), Lent: running})
	ctx := withToken("the-token")
	state, err := client.GetModelLending(ctx, &nodepb.GetModelLendingRequest{})
	if err != nil || state.GetRestartNeeded() || state.GetSaved().GetEnabled() || state.GetSaved().GetGpuLayers() != -1 {
		t.Fatalf("lending never set = %v, %v", state, err)
	}
	asked := &nodepb.ModelLending{Enabled: true, OllamaUrl: " http://127.0.0.1:11434 ", Only: []string{" llama3.1", "", "gemma*"}, GpuLayers: 0, Threads: 4, Context: 4096, KeepAlive: "10m"}
	state, err = client.SetModelLending(ctx, &nodepb.SetModelLendingRequest{Lending: asked})
	if err != nil || !state.GetRestartNeeded() || state.GetRunning().GetEnabled() {
		t.Fatalf("saving lending = %v, %v; want it saved for a restart", state, err)
	}
	if saved := state.GetSaved(); saved.GetOllamaUrl() != "http://127.0.0.1:11434" || strings.Join(saved.GetOnly(), ",") != "llama3.1,gemma*" || saved.GetContext() != 4096 {
		t.Errorf("saved %v", saved)
	}
	if again, err := client.GetModelLending(ctx, &nodepb.GetModelLendingRequest{}); err != nil || again.GetSaved().GetKeepAlive() != "10m" || !again.GetRestartNeeded() {
		t.Errorf("lending read back = %v, %v", again, err)
	}
}

func TestModelLendingSetByFlagsNeedsNoRestart(t *testing.T) {
	flags := nodedb.ModelLending{Enabled: true, URL: "http://127.0.0.1:11434", GPULayers: -1}
	client := serveLocal(t, LocalConfig{Lending: newFileList(t), Lent: flags, LentByFlags: true})
	state, err := client.GetModelLending(withToken("the-token"), &nodepb.GetModelLendingRequest{})
	if err != nil || !state.GetSetByFlags() || state.GetRestartNeeded() || !state.GetRunning().GetEnabled() {
		t.Errorf("lending set by flags = %v, %v", state, err)
	}
}

func TestModelLendingPolicyAppliesLiveAtTheRunningAddress(t *testing.T) {
	running := nodedb.ModelLending{Enabled: true, URL: "http://127.0.0.1:11434", Only: []string{"first"}, GPULayers: -1}
	live := runtime.NewModelServer(running.URL, runtime.OwnerFromSettings(running.Only, running.GPULayers, running.Threads, running.Context, running.KeepAlive))
	var told atomic.Int32
	client := serveLocal(t, LocalConfig{Lending: newFileList(t), Lent: running, LiveLending: live, OffersChanged: func() { told.Add(1) }})

	asked := &nodepb.ModelLending{Enabled: true, OllamaUrl: running.URL, Only: []string{"second"}, GpuLayers: 0, Threads: 6, Context: 4096, KeepAlive: "10m"}
	state, err := client.SetModelLending(withToken("the-token"), &nodepb.SetModelLendingRequest{Lending: asked})
	if err != nil || state.GetRestartNeeded() {
		t.Fatalf("save live policy = %v, %v; want no restart", state, err)
	}
	if got := state.GetRunning(); !got.GetEnabled() || got.GetOllamaUrl() != running.URL || strings.Join(got.GetOnly(), ",") != "second" || got.GetThreads() != 6 || got.GetContext() != 4096 {
		t.Fatalf("running settings = %v", got)
	}
	url, owner := live.Settings()
	if url != running.URL || strings.Join(owner.Only, ",") != "second" || owner.Options["num_gpu"] != 0 || owner.Options["num_thread"] != 6 || owner.Options["num_ctx"] != 4096 || owner.KeepAlive != "10m" {
		t.Fatalf("live model server = %q, %+v", url, owner)
	}
	// Which models are lent has changed, and the worker is told.
	if told.Load() != 1 {
		t.Errorf("the worker was told %d times, want once", told.Load())
	}
}

func TestModelLendingAddressChangeStillNeedsRestart(t *testing.T) {
	running := nodedb.ModelLending{Enabled: true, URL: "http://127.0.0.1:11434", Only: []string{"first"}, GPULayers: -1}
	live := runtime.NewModelServer(running.URL, runtime.OwnerFromSettings(running.Only, running.GPULayers, running.Threads, running.Context, running.KeepAlive))
	client := serveLocal(t, LocalConfig{Lending: newFileList(t), Lent: running, LiveLending: live})
	state, err := client.SetModelLending(withToken("the-token"), &nodepb.SetModelLendingRequest{Lending: &nodepb.ModelLending{
		Enabled: true, OllamaUrl: "http://127.0.0.1:11435", Only: []string{"second"}, GpuLayers: -1,
	}})
	if err != nil || !state.GetRestartNeeded() || state.GetRunning().GetOllamaUrl() != running.URL {
		t.Fatalf("save address change = %v, %v; want restart and old running address", state, err)
	}
	url, owner := live.Settings()
	if url != running.URL || strings.Join(owner.Only, ",") != "first" {
		t.Fatalf("address change altered live runtime: %q, %+v", url, owner)
	}
}

func TestModelLendingRefusesWhatOllamaCouldNotUse(t *testing.T) {
	client := serveLocal(t, LocalConfig{Lending: newFileList(t)})
	ctx := withToken("the-token")
	for _, tt := range []struct {
		name    string
		lending *nodepb.ModelLending
		want    string
	}{
		{"no address", &nodepb.ModelLending{Enabled: true}, "Ollama's address"},
		{"an address that is no web address", &nodepb.ModelLending{Enabled: true, OllamaUrl: "ftp://host"}, "Ollama's address"},
		{"an address that cannot be read", &nodepb.ModelLending{Enabled: true, OllamaUrl: "http://[::1"}, "Ollama's address"},
		{"layers below -1", &nodepb.ModelLending{GpuLayers: -2}, "graphics cards"},
		{"threads below 0", &nodepb.ModelLending{Threads: -1}, "threads and context"},
		{"context below 0", &nodepb.ModelLending{Context: -1}, "threads and context"},
		{"a time that is no time", &nodepb.ModelLending{KeepAlive: "a while"}, "stays loaded"},
	} {
		_, err := client.SetModelLending(ctx, &nodepb.SetModelLendingRequest{Lending: tt.lending})
		if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v, want %q", tt.name, err, tt.want)
		}
	}
	// Lending turned off keeps its address, whatever it is, and a time
	// may be a number of seconds as Ollama allows.
	for _, keep := range []string{"0", "-1", "1h30m", ""} {
		if _, err := client.SetModelLending(ctx, &nodepb.SetModelLendingRequest{Lending: &nodepb.ModelLending{OllamaUrl: "not yet", KeepAlive: keep, GpuLayers: -1}}); err != nil {
			t.Errorf("keep-alive %q: %v", keep, err)
		}
	}
}

func TestModelLendingCallsNeedTheTokenAndAStore(t *testing.T) {
	without := serveLocal(t, LocalConfig{})
	ctx := context.Background()
	if _, err := without.GetModelLending(ctx, &nodepb.GetModelLendingRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("GetModelLending without the token: %v", err)
	}
	if _, err := without.SetModelLending(ctx, &nodepb.SetModelLendingRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("SetModelLending without the token: %v", err)
	}
	if _, err := without.InspectOllama(ctx, &nodepb.InspectOllamaRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("InspectOllama without the token: %v", err)
	}
	ctx = withToken("the-token")
	if _, err := without.GetModelLending(ctx, &nodepb.GetModelLendingRequest{}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("GetModelLending with no store: %v", err)
	}
	if _, err := without.SetModelLending(ctx, &nodepb.SetModelLendingRequest{}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("SetModelLending with no store: %v", err)
	}
	broken := serveLocal(t, LocalConfig{Lending: brokenLending{}})
	if _, err := broken.GetModelLending(ctx, &nodepb.GetModelLendingRequest{}); status.Code(err) != codes.Internal {
		t.Errorf("GetModelLending from a broken store: %v", err)
	}
	if _, err := broken.SetModelLending(ctx, &nodepb.SetModelLendingRequest{}); status.Code(err) != codes.Internal {
		t.Errorf("SetModelLending to a broken store: %v", err)
	}
}

func TestModelLendingReportsRestartWhenLiveServerIsMissing(t *testing.T) {
	running := nodedb.ModelLending{Enabled: true, URL: "http://127.0.0.1:11434", Only: []string{"old"}, GPULayers: -1}
	saved := newFileList(t)
	if err := saved.SetModelLending(nodedb.ModelLending{Enabled: true, URL: running.URL, Only: []string{"new"}, GPULayers: -1}); err != nil {
		t.Fatal(err)
	}
	client := serveLocal(t, LocalConfig{Lending: saved, Lent: running})
	state, err := client.GetModelLending(withToken("the-token"), &nodepb.GetModelLendingRequest{})
	if err != nil || !state.GetRestartNeeded() {
		t.Fatalf("saved policy without live model server = %v, %v; want restart", state, err)
	}
}

func TestInspectOllamaSaysWhatWouldBeLentOrWhyNothingCould(t *testing.T) {
	was := inspect
	t.Cleanup(func() { inspect = was })
	var offered []string
	inspect = func(_ context.Context, url string, owner runtime.Owner) (runtime.Server, error) {
		offered = owner.Only
		if url == "http://127.0.0.1:1" {
			return runtime.Server{}, errors.New("connection refused")
		}
		return runtime.Server{Version: "0.21.0", Models: []string{"llama3.1:8b"}, Loaded: []runtime.Loaded{{Model: "llama3.1:8b", OnCard: 75}}}, nil
	}
	var told atomic.Int32
	client := serveLocal(t, LocalConfig{OffersChanged: func() { told.Add(1) }})
	ctx := withToken("the-token")
	got, err := client.InspectOllama(ctx, &nodepb.InspectOllamaRequest{Url: "http://127.0.0.1:11434", Only: []string{" llama3.1 ", ""}})
	if err != nil || got.GetVersion() != "0.21.0" || strings.Join(got.GetModels(), ",") != "llama3.1:8b" || got.GetLoaded()[0].GetOnCardPercent() != 75 || strings.Join(offered, ",") != "llama3.1" {
		t.Errorf("InspectOllama = %v, %v (offering %q)", got, err, offered)
	}
	if got, err := client.InspectOllama(ctx, &nodepb.InspectOllamaRequest{Url: "http://127.0.0.1:1"}); err != nil || got.GetError() != "connection refused" || got.GetVersion() != "" {
		t.Errorf("an Ollama not there = %v, %v", got, err)
	}
	if _, err := client.InspectOllama(ctx, &nodepb.InspectOllamaRequest{Url: "127.0.0.1:11434"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("an address with no scheme: %v", err)
	}
	// Looking at an Ollama that answered is when the worker looks again too.
	if told.Load() != 1 {
		t.Errorf("the worker was told %d times, want once", told.Load())
	}
}
