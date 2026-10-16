package runtime

import (
	"context"
	"encoding/json"
)

// ModelUse is what a model task says it ran with: the server that answered,
// and the model and options it was asked with. It is the worker's own
// account, for a job's record, so that an owner's settings show and a later
// check has something to check against. It proves nothing.
type ModelUse struct {
	// Server is the kind of model server, "ollama", and Version the
	// version it says it is; empty when it would not say.
	Server, Version string
	// Model is the model as the request named it. Digest and Quantization
	// are what the server says of the model it has by that name; empty
	// when it has none by that name or would not say.
	Model, Digest, Quantization string
	// Options are the options sent to the server, as JSON, the owner's
	// own included; nil when none were sent.
	Options json.RawMessage
}

type modelUseKey struct{}

// WithModelUse returns a context whose model tasks tell said what they ran
// with, once each, before they ask the server for anything.
func WithModelUse(ctx context.Context, said func(ModelUse)) context.Context {
	return context.WithValue(ctx, modelUseKey{}, said)
}

// tellModelUse says what the request body, as sent to the server at url,
// runs with, to whoever ctx says is listening. Where nobody is, or there is
// no server, it asks nothing and says nothing.
func tellModelUse(ctx context.Context, url string, body []byte) {
	said, listening := ctx.Value(modelUseKey{}).(func(ModelUse))
	if !listening || url == "" {
		return
	}
	var sent struct {
		Model   string          `json:"model"`
		Options json.RawMessage `json:"options"`
	}
	json.Unmarshal(body, &sent) // toOllama wrote it, so it is an object
	use := ModelUse{Server: "ollama", Model: sent.Model, Options: sent.Options}
	asker := Chat{URL: url}
	if reply, err := asker.ask(ctx, "/api/version"); err == nil {
		var version struct {
			Version string `json:"version"`
		}
		json.Unmarshal(reply, &version) // a reply that is not a version says none
		use.Version = version.Version
	}
	if reply, err := asker.ask(ctx, "/api/tags"); err == nil {
		var listed struct {
			Models []struct {
				Name    string `json:"name"`
				Digest  string `json:"digest"`
				Details struct {
					Quantization string `json:"quantization_level"`
				} `json:"details"`
			} `json:"models"`
		}
		json.Unmarshal(reply, &listed) // a reply that is not a list lists nothing
		for _, m := range listed.Models {
			// Ollama takes a name without a tag to mean the latest.
			if m.Name == sent.Model || m.Name == sent.Model+":latest" {
				use.Digest, use.Quantization = m.Digest, m.Details.Quantization
				break
			}
		}
	}
	said(use)
}
