package runtime

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// usedServer stands in for an Ollama that says which version it is and
// what it has, and answers chat and embeddings requests. It counts what it
// was asked, by path.
func usedServer(t *testing.T, version, tags string) (*httptest.Server, func(string) int) {
	t.Helper()
	var mu sync.Mutex
	asked := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked[r.URL.Path]++
		mu.Unlock()
		switch r.URL.Path {
		case "/api/version":
			io.WriteString(w, version)
		case "/api/tags":
			io.WriteString(w, tags)
		case "/api/embed":
			io.WriteString(w, `{"model":"nomic-embed-text:latest","embeddings":[[0.5]]}`)
		default:
			io.WriteString(w, `{"model":"llama3.1:8b","message":{"role":"assistant","content":"Hi."},"done":true}`)
		}
	}))
	t.Cleanup(server.Close)
	return server, func(path string) int { mu.Lock(); defer mu.Unlock(); return asked[path] }
}

const usedTags = `{"models":[` +
	`{"name":"llama3.1:8b","digest":"46e0c10c039e","details":{"quantization_level":"Q4_K_M"}},` +
	`{"name":"nomic-embed-text:latest","digest":"0a109f422b47","details":{"quantization_level":"F16"}}]}`

// listen returns a context whose model tasks say what they used into the
// slice it returns.
func listen() (context.Context, *[]ModelUse) {
	heard := new([]ModelUse)
	return WithModelUse(context.Background(), func(use ModelUse) { *heard = append(*heard, use) }), heard
}

func TestAModelTaskSaysWhatItRanWithTheOwnersOptionsIncluded(t *testing.T) {
	server, _ := usedServer(t, `{"version":"0.12.11"}`, usedTags)
	// The job asks for a seed and a temperature; the owner sets a seed of
	// their own and a context length, and the owner's seed is what is sent.
	chat := Chat{URL: server.URL, Owner: Owner{Options: map[string]any{"seed": 7, "num_ctx": 8192}}}
	ctx, heard := listen()
	if _, err := chat.Execute(ctx, nil, []byte(`{"model":"llama3.1:8b","seed":42,"temperature":0,"messages":[{"role":"user","content":"Hi"}]}`)); err != nil {
		t.Fatal(err)
	}
	if len(*heard) != 1 {
		t.Fatalf("heard %d uses, want 1", len(*heard))
	}
	use := (*heard)[0]
	if use.Server != "ollama" || use.Version != "0.12.11" || use.Model != "llama3.1:8b" || use.Digest != "46e0c10c039e" || use.Quantization != "Q4_K_M" {
		t.Fatalf("use = %+v", use)
	}
	var options map[string]float64
	if err := json.Unmarshal(use.Options, &options); err != nil || options["seed"] != 7 || options["temperature"] != 0 || options["num_ctx"] != 8192 || len(options) != 3 {
		t.Fatalf("options = %s, %v", use.Options, err)
	}
}

func TestAModelNamedWithoutATagIsTheLatest(t *testing.T) {
	server, _ := usedServer(t, `{"version":"0.12.11"}`, usedTags)
	ctx, heard := listen()
	if _, err := (Embed{URL: server.URL}).Execute(ctx, nil, []byte(`{"start":0,"request":{"model":"nomic-embed-text","input":["a"]}}`)); err != nil {
		t.Fatal(err)
	}
	if len(*heard) != 1 || (*heard)[0].Model != "nomic-embed-text" || (*heard)[0].Digest != "0a109f422b47" || (*heard)[0].Quantization != "F16" || (*heard)[0].Options != nil {
		t.Fatalf("heard %+v", *heard)
	}
}

func TestAShareOfPromptsSaysWhatItRanWithOnce(t *testing.T) {
	server, asked := usedServer(t, `{"version":"0.12.11"}`, usedTags)
	ctx, heard := listen()
	if _, err := (Prompts{URL: server.URL}).Execute(ctx, nil, []byte(`{"start":0,"model":"llama3.1:8b","prompts":["a","b","c"],"options":{"temperature":0}}`)); err != nil {
		t.Fatal(err)
	}
	if len(*heard) != 1 || (*heard)[0].Digest != "46e0c10c039e" || asked("/api/chat") != 3 || asked("/api/tags") != 1 {
		t.Fatalf("heard %+v; asked for tags %d times, chat %d", *heard, asked("/api/tags"), asked("/api/chat"))
	}
}

func TestNobodyListeningMeansTheServerIsAskedNothingMore(t *testing.T) {
	server, asked := usedServer(t, `{"version":"0.12.11"}`, usedTags)
	if _, err := (Chat{URL: server.URL}).Execute(context.Background(), nil, []byte(`{"model":"llama3.1:8b","messages":[{"role":"user","content":"Hi"}]}`)); err != nil {
		t.Fatal(err)
	}
	if asked("/api/version") != 0 || asked("/api/tags") != 0 || asked("/api/chat") != 1 {
		t.Fatalf("asked for version %d times, tags %d, chat %d", asked("/api/version"), asked("/api/tags"), asked("/api/chat"))
	}
}

func TestAServerThatWillNotSayLeavesWhatItWouldNotSayEmpty(t *testing.T) {
	// One that answers with something that is not a version or a list,
	// and one that has no model by the name asked for.
	for name, server := range map[string]func(*testing.T) *httptest.Server{
		"not as Ollama does": func(t *testing.T) *httptest.Server { s, _ := usedServer(t, `not json`, `not json`); return s },
		"no such model":      func(t *testing.T) *httptest.Server { s, _ := usedServer(t, `{}`, usedTags); return s },
	} {
		t.Run(name, func(t *testing.T) {
			ctx, heard := listen()
			if _, err := (Chat{URL: server(t).URL}).Execute(ctx, nil, []byte(`{"model":"qwen3:8b","messages":[{"role":"user","content":"Hi"}]}`)); err != nil {
				t.Fatal(err)
			}
			if len(*heard) != 1 || !reflect.DeepEqual((*heard)[0], ModelUse{Server: "ollama", Model: "qwen3:8b"}) {
				t.Fatalf("heard %+v", *heard)
			}
		})
	}

	// One that cannot be reached at all is still said to have been asked,
	// since the task was, and the task fails for its own reason.
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	ctx, heard := listen()
	if _, err := (Chat{URL: gone.URL}).Execute(ctx, nil, []byte(`{"model":"qwen3:8b","messages":[{"role":"user","content":"Hi"}]}`)); err == nil || !strings.Contains(err.Error(), "reach the model server") {
		t.Fatalf("err = %v", err)
	}
	if len(*heard) != 1 || (*heard)[0].Model != "qwen3:8b" || (*heard)[0].Version != "" {
		t.Fatalf("heard %+v", *heard)
	}
}

func TestATaskWithNoModelServerSaysNothingOfOne(t *testing.T) {
	ctx, heard := listen()
	if _, err := (Prompts{}).Execute(ctx, nil, []byte(`{"start":0,"model":"llama3.1:8b","prompts":["a"]}`)); err == nil {
		t.Fatal("prompts ran with no model server")
	}
	if len(*heard) != 0 {
		t.Fatalf("heard %+v", *heard)
	}
}

func TestAModelMetadataRequestsShareOneBoundedDeadlineAndRunTogether(t *testing.T) {
	oldWait := tagsWait
	tagsWait = 500 * time.Millisecond
	t.Cleanup(func() { tagsWait = oldWait })

	var mu sync.Mutex
	var inFlight, maxInFlight int
	var canceledAt []time.Time
	var metadataHandlers sync.WaitGroup
	metadataHandlers.Add(2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/version" || r.URL.Path == "/api/tags" {
			defer metadataHandlers.Done()
			mu.Lock()
			inFlight++
			if inFlight > maxInFlight {
				maxInFlight = inFlight
			}
			mu.Unlock()
			<-r.Context().Done()
			mu.Lock()
			canceledAt = append(canceledAt, time.Now())
			inFlight--
			mu.Unlock()
			return
		}
		io.WriteString(w, `{"model":"llama3.1:8b","message":{"role":"assistant","content":"Hi."},"done":true}`)
	}))
	t.Cleanup(server.Close)

	ctx, heard := listen()
	if _, err := (Chat{URL: server.URL}).Execute(ctx, nil, []byte(`{"model":"llama3.1:8b","messages":[{"role":"user","content":"Hi"}]}`)); err != nil {
		t.Fatal(err)
	}
	metadataHandlers.Wait()
	mu.Lock()
	max, cancellations := maxInFlight, append([]time.Time(nil), canceledAt...)
	mu.Unlock()
	if max != 2 {
		t.Fatalf("maximum simultaneous metadata requests = %d, want 2", max)
	}
	if len(cancellations) != 2 {
		t.Fatalf("metadata request cancellations = %d, want 2", len(cancellations))
	}
	delta := cancellations[0].Sub(cancellations[1])
	if delta < 0 {
		delta = -delta
	}
	if delta >= tagsWait/2 {
		t.Fatalf("metadata requests were canceled %s apart, want a shared deadline", delta)
	}
	if len(*heard) != 1 || (*heard)[0].Version != "" || (*heard)[0].Digest != "" {
		t.Fatalf("heard %+v", *heard)
	}
}
