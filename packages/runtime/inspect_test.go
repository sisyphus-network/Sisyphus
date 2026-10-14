package runtime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestInspectSaysWhatAnOllamaHasAsTheOwnerWouldLendIt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/version":
			w.Write([]byte(`{"version":"0.21.0"}`))
		case "/api/tags":
			w.Write([]byte(`{"models":[{"name":"llama3.1:8b"},{"name":"qwen3:8b"}]}`))
		case "/api/ps":
			w.Write([]byte(`{"models":[{"name":"llama3.1:8b","size":100,"size_vram":75},{"name":"qwen3:8b","size":100,"size_vram":100}]}`))
		}
	}))
	defer server.Close()
	got, err := Inspect(context.Background(), server.URL, Owner{Only: []string{"llama3.1"}})
	want := Server{Version: "0.21.0", Models: []string{"llama3.1:8b"}, Loaded: []Loaded{{Model: "llama3.1:8b", OnCard: 75}}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("Inspect = %#v, %v; want %#v", got, err, want)
	}
}

func TestInspectTellsNoOllamaFromOne(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("<html>hello</html>")) }))
	defer other.Close()
	if _, err := Inspect(context.Background(), other.URL, Owner{}); err == nil || err.Error() != "it answers, but not as Ollama does" {
		t.Errorf("a server that is not an Ollama: %v", err)
	}
	other.Close()
	if _, err := Inspect(context.Background(), other.URL, Owner{}); err == nil {
		t.Error("a server that is gone was inspected")
	}
}
