package ai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// shelf stands in for an Ollama with models on it.
type shelf struct {
	*httptest.Server
	// shows is what it says of each model when asked, and pulling what it
	// sends while fetching one.
	shows   map[string]string
	pulling string
	removed []string
	pulled  []string
}

func newShelf(t *testing.T) *shelf {
	t.Helper()
	s := &shelf{shows: map[string]string{}}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var asked struct {
			Model string `json:"model"`
		}
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &asked)
		switch r.URL.Path {
		case "/api/tags":
			io.WriteString(w, `{"models":[{"name":"llama3.1:8b","size":4900000000},{"name":"gemma3:4b","size":3300000000},{"name":"old:1b"},{"name":"odd:1b"},{"name":"mute:1b"}]}`)
		case "/api/show":
			shown, known := s.shows[asked.Model]
			if !known {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			io.WriteString(w, shown)
		case "/api/pull":
			s.pulled = append(s.pulled, asked.Model)
			io.WriteString(w, s.pulling)
		case "/api/delete":
			if r.Method != http.MethodDelete || asked.Model == "absent:1b" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			s.removed = append(s.removed, asked.Model)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func TestOllamaSaysWhatEachOfItsModelsCanDo(t *testing.T) {
	s := newShelf(t)
	s.shows = map[string]string{
		"llama3.1:8b": `{"capabilities":["completion","tools"]}`,
		"gemma3:4b":   `{"capabilities":["completion","vision"]}`,
		"old:1b":      `{}`,
		"odd:1b":      `not JSON`,
	}
	got, err := provider(t, Config{Provider: Ollama, BaseURL: s.URL}).Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Model{
		{Name: "llama3.1:8b", Size: 4900000000, Tools: Yes},
		{Name: "gemma3:4b", Size: 3300000000, Tools: No},
		{Name: "old:1b"}, {Name: "odd:1b"}, {Name: "mute:1b"},
	}
	if len(got) != len(want) {
		t.Fatalf("models = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("model %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestOllamaFetchesAModelAndSaysHowFarItHasGot(t *testing.T) {
	s := newShelf(t)
	library := provider(t, Config{Provider: Ollama, BaseURL: s.URL}).(Library)
	s.pulling = `{"status":"pulling manifest"}` + "\n" + `{"status":"pulling 6a0746a1ec1a","total":4900,"completed":1200}` + "\n" + `{"status":"success"}` + "\n"
	var steps []Progress
	if err := library.Pull(context.Background(), "llama3.1:8b", func(p Progress) { steps = append(steps, p) }); err != nil {
		t.Fatal(err)
	}
	if len(steps) != 3 || steps[1] != (Progress{Status: "pulling 6a0746a1ec1a", Done: 1200, Total: 4900}) || steps[2].Status != "success" || s.pulled[0] != "llama3.1:8b" {
		t.Fatalf("steps = %+v, pulled %v", steps, s.pulled)
	}

	quiet := func(Progress) {}
	s.pulling = `{"error":"pull model manifest: file does not exist"}`
	if err := library.Pull(context.Background(), "nonesuch", quiet); err == nil || !strings.Contains(err.Error(), "could not be fetched: pull model manifest") {
		t.Errorf("a model there is none of: %v", err)
	}
	s.pulling = `not JSON`
	if err := library.Pull(context.Background(), "x", quiet); err == nil || !strings.Contains(err.Error(), "not what Ollama sends") {
		t.Errorf("a reply that is none: %v", err)
	}
	s.Close()
	if err := library.Pull(context.Background(), "x", quiet); err == nil || !strings.Contains(err.Error(), "reach the model service") {
		t.Errorf("no Ollama: %v", err)
	}
}

func TestOllamaRemovesAModel(t *testing.T) {
	s := newShelf(t)
	library := provider(t, Config{Provider: Ollama, BaseURL: s.URL}).(Library)
	if err := library.Remove(context.Background(), "gemma3:4b"); err != nil || len(s.removed) != 1 || s.removed[0] != "gemma3:4b" {
		t.Fatalf("removed %v, %v", s.removed, err)
	}
	if err := library.Remove(context.Background(), "absent:1b"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("removing what is not there: %v", err)
	}
}

func TestTheKindsOfProviderAreAllOnesThereAre(t *testing.T) {
	kinds := Kinds()
	if len(kinds) != len(usualPlace) || kinds[0].ID != Ollama {
		t.Fatalf("kinds = %+v", kinds)
	}
	for _, k := range kinds {
		p, err := New(Config{Provider: k.ID, APIKey: "k"})
		if err != nil || k.Name == "" || k.About == "" || k.Place != usualPlace[k.ID] {
			t.Errorf("%+v: %v", k, err)
		}
		if _, fetches := p.(Library); fetches != k.Fetches {
			t.Errorf("%s: fetches is %v, and it is said to be %v", k.ID, fetches, k.Fetches)
		}
	}
}
