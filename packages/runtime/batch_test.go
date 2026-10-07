package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// thinking stands in for a model server that answers each prompt with the
// prompt in capitals, and gives each text a vector of its length.
func thinking(t *testing.T) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	asked := &[]map[string]any{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &request)
		*asked = append(*asked, request)
		switch r.URL.Path {
		case "/v1/chat/completions":
			messages := request["messages"].([]any)
			prompt := messages[len(messages)-1].(map[string]any)["content"].(string)
			if prompt == "break" {
				http.Error(w, "out of memory", http.StatusInternalServerError)
				return
			}
			if prompt == "mumble" {
				io.WriteString(w, `{"choices":[]}`)
				return
			}
			fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":%q}}]}`, strings.ToUpper(prompt))
		case "/v1/embeddings":
			texts := request["input"].([]any)
			// In an order of its own, to be put right by their numbers.
			var data []string
			for i := len(texts) - 1; i >= 0; i-- {
				data = append(data, fmt.Sprintf(`{"index":%d,"embedding":[%d]}`, i, len(texts[i].(string))))
			}
			fmt.Fprintf(w, `{"model":"nomic:latest","data":[%s],"usage":{"prompt_tokens":%d}}`, strings.Join(data, ","), len(texts))
		}
	}))
	t.Cleanup(server.Close)
	return server, asked
}

// through runs a workload's job as a pool would: split, each part run,
// the outputs put together.
func through(t *testing.T, w Workload, blobs Blobs, params string, parts int) (string, int, error) {
	t.Helper()
	payloads, err := w.Split(ctx, blobs, []byte(params), parts)
	if err != nil {
		return "", 0, err
	}
	var outputs [][]byte
	for _, payload := range payloads {
		out, err := w.Execute(ctx, blobs, payload)
		if err != nil {
			return "", len(payloads), err
		}
		outputs = append(outputs, out)
	}
	// However the tasks come back, the result is in the order asked.
	for i, j := 0, len(outputs)-1; i < j; i, j = i+1, j-1 {
		outputs[i], outputs[j] = outputs[j], outputs[i]
	}
	result, err := w.Aggregate(ctx, blobs, outputs)
	return string(result), len(payloads), err
}

func TestManyPromptsAreSharedOutAndAnsweredInOrder(t *testing.T) {
	server, asked := thinking(t)
	store := storage.NewMemory()
	prompts := Prompts{URL: server.URL}
	got, tasks, err := through(t, prompts, store, `{"model":"llama3.1:8b@rig","system":"Shout.","prompts":["a","b","c","d","e"],"options":{"temperature":0}}`, 3)
	if err != nil || tasks != 3 || got != `{"answers":["A","B","C","D","E"],"count":5}` {
		t.Fatalf("%s in %d tasks, %v", got, tasks, err)
	}
	first := (*asked)[0]
	if first["model"] != "llama3.1:8b" || first["temperature"] != float64(0) || first["stream"] != false || len(first["messages"].([]any)) != 2 {
		t.Errorf("the model server was asked %v", first)
	}
	// More parts than prompts is a part for each, and the prompts may come
	// from a stored file, a line each, the empty ones passed over.
	stored, _ := store.Put(ctx, strings.NewReader("one\n\n  two  \nthree\n"))
	got, tasks, err = through(t, prompts, store, `{"model":"m","input":"`+stored.String()+`"}`, 8)
	if err != nil || tasks != 3 || got != `{"answers":["ONE","TWO","THREE"],"count":3}` {
		t.Fatalf("from a file: %s in %d tasks, %v", got, tasks, err)
	}
	if len((*asked)[len(*asked)-1]["messages"].([]any)) != 1 {
		t.Errorf("with nothing to say first, the model was told %v", (*asked)[len(*asked)-1])
	}
	// Many answers are stored, one to a line, and the result names the file.
	long := strings.Repeat("x", maxInlineAnswers/2+1)
	got, _, err = through(t, prompts, store, `{"model":"m","prompts":["`+long+`","`+long+`"]}`, 1)
	var named struct {
		Count  int
		Output string
	}
	if json.Unmarshal([]byte(got), &named); err != nil || named.Count != 2 || named.Output == "" {
		t.Fatalf("many answers: %.100s, %v", got, err)
	}
	if prompts.Name() != "prompts" || !strings.Contains(prompts.Describe(), `"prompts"`) || strings.Join(prompts.Needs([]byte(`{"model":"m@rig"}`)), " ") != "model:m worker:rig" {
		t.Errorf("prompts says %q and needs %v", prompts.Describe(), prompts.Needs([]byte(`{"model":"m@rig"}`)))
	}
}

func TestPromptsThatCannotBeSharedOutOrAnswered(t *testing.T) {
	server, _ := thinking(t)
	store := storage.NewMemory()
	absent, _ := storage.NewMemory().Put(ctx, strings.NewReader("elsewhere"))
	held, _ := store.Put(ctx, strings.NewReader("a\nb\n"))
	prompts := Prompts{URL: server.URL}
	for params, want := range map[string]string{
		`not JSON`:                          "prompts parameters",
		`{"prompts":["a"]}`:                 "must name a model",
		`{"model":"m"}`:                     "there are no prompts",
		`{"model":"m","input":"not-a-cid"}`: "input is not a CID",
		`{"model":"m","input":"` + absent.String() + `"}`: "prompts input",
		`{"model":"m","prompts":["a","break"]}`:           "prompt 2: the model server answered 500",
		`{"model":"m","prompts":["mumble"]}`:              "prompt 1: the model server's reply is not a chat reply",
	} {
		if _, _, err := through(t, prompts, store, params, 1); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", params, err, want)
		}
	}
	if _, err := prompts.Split(ctx, failingBlobs{Store: store, failRead: true}, []byte(`{"model":"m","input":"`+held.String()+`"}`), 1); !errors.Is(err, errBlobs) {
		t.Errorf("a file of prompts that cannot be read: %v", err)
	}
	long := strings.Repeat("x", maxInlineAnswers+1)
	if _, _, err := through(t, prompts, failingBlobs{Store: store, failPut: true}, `{"model":"m","prompts":["`+long+`"]}`, 1); !errors.Is(err, errBlobs) {
		t.Errorf("answers that cannot be stored: %v", err)
	}
	if _, err := prompts.Execute(ctx, store, []byte(`not JSON`)); err == nil || !strings.Contains(err.Error(), "prompts task") {
		t.Errorf("a task that is none: %v", err)
	}
	if _, err := prompts.Aggregate(ctx, store, [][]byte{[]byte(`not JSON`)}); err == nil || !strings.Contains(err.Error(), "prompts task 0 output") {
		t.Errorf("an output that is none: %v", err)
	}
}

func TestTextsAreTurnedIntoVectorsInTheOrderGiven(t *testing.T) {
	server, asked := thinking(t)
	embed := Embed{URL: server.URL}
	got, tasks, err := through(t, embed, nil, `{"model":"nomic@rig","input":["a","bb","ccc","dddd","eeeee"],"dimensions":1}`, 2)
	if err != nil || tasks != 2 {
		t.Fatalf("%s in %d tasks, %v", got, tasks, err)
	}
	var reply struct {
		Object, Model string
		Data          []struct {
			Object    string
			Index     int
			Embedding []int
		}
		Usage struct{ PromptTokens, TotalTokens int }
	}
	if err := json.Unmarshal([]byte(strings.NewReplacer("prompt_tokens", "PromptTokens", "total_tokens", "TotalTokens").Replace(got)), &reply); err != nil {
		t.Fatal(err)
	}
	if reply.Object != "list" || reply.Model != "nomic:latest" || len(reply.Data) != 5 || reply.Usage.PromptTokens != 5 || reply.Usage.TotalTokens != 5 {
		t.Fatalf("the reply: %s", got)
	}
	for i, d := range reply.Data {
		if d.Index != i || d.Object != "embedding" || d.Embedding[0] != i+1 {
			t.Errorf("vector %d: %+v", i, d)
		}
	}
	if first := (*asked)[0]; first["model"] != "nomic" || first["dimensions"] != float64(1) {
		t.Errorf("the model server was asked %v", first)
	}
	// One text may be given by itself.
	if got, tasks, err := through(t, embed, nil, `{"model":"nomic","input":"alone"}`, 4); err != nil || tasks != 1 || !strings.Contains(got, `"embedding":[5]`) {
		t.Errorf("one text: %s in %d tasks, %v", got, tasks, err)
	}
	if embed.Name() != "embed" || !strings.Contains(embed.Describe(), "128") || embed.Needs([]byte(`{"model":"nomic"}`))[0] != "model:nomic" {
		t.Errorf("embed says %q", embed.Describe())
	}

	many := `["x"` + strings.Repeat(`,"x"`, MaxEmbedInputs) + `]`
	for params, want := range map[string]string{
		`not JSON`:                           "not an embeddings request",
		`{"input":"a"}`:                      "must name a model",
		`{"model":"m"}`:                      `must have "input"`,
		`{"model":"m","input":[]}`:           `must have "input"`,
		`{"model":"m","input":7}`:            `must have "input"`,
		`{"model":"m","input":` + many + `}`: "at most 128 texts, and this has 129",
	} {
		if _, _, err := through(t, embed, nil, params, 1); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", params, err, want)
		}
	}
	if _, err := embed.Execute(ctx, nil, []byte(`not JSON`)); err == nil || !strings.Contains(err.Error(), "embed task") {
		t.Errorf("a task that is none: %v", err)
	}
	if _, err := embed.Aggregate(ctx, nil, [][]byte{[]byte(`not JSON`)}); err == nil || !strings.Contains(err.Error(), "embed task 0 output") {
		t.Errorf("an output that is none: %v", err)
	}
}

func TestAModelServerThatFailsABatchFailsTheTask(t *testing.T) {
	share := []byte(`{"start":0,"request":{"model":"m","input":["a"]}}`)
	muddled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"data":[]}`) }))
	defer muddled.Close()
	if _, err := (Embed{URL: muddled.URL}).Execute(ctx, nil, share); err == nil || !strings.Contains(err.Error(), "not an embeddings reply") {
		t.Errorf("a reply with no vectors: %v", err)
	}
	short := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		io.WriteString(w, `{"data":`)
	}))
	defer short.Close()
	if _, err := (Embed{URL: short.URL}).Execute(ctx, nil, share); err == nil || !strings.Contains(err.Error(), "read the model server's reply") {
		t.Errorf("a reply cut short: %v", err)
	}
	short.Close()
	for url, want := range map[string]string{short.URL: "reach the model server", "http://bad\x00address": "model server address", "": "serves no language models"} {
		if _, err := (Embed{URL: url}).Execute(ctx, nil, share); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("a model server at %q: %v, want %q", url, err, want)
		}
	}
}
