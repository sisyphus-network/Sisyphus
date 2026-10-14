package runtime

import (
	"context"
	"encoding/json"
	"errors"
)

// Server is what an owner is shown of the Ollama they would lend: whether
// it answers, which version it is, the models it has that the owner offers,
// and those it has in memory now.
type Server struct {
	Version string
	Models  []string
	Loaded  []Loaded
}

// Inspect asks the Ollama at url what it is and what it has, as the owner
// would lend it. A server that does not say which version it is is taken
// to be no Ollama, or none at all.
func Inspect(ctx context.Context, url string, owner Owner) (Server, error) {
	c := Chat{URL: url, Owner: owner}
	body, err := c.ask(ctx, "/api/version")
	if err != nil {
		return Server{}, err
	}
	var said struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(body, &said); said.Version == "" {
		return Server{}, errors.New("it answers, but not as Ollama does")
	}
	labels := c.Offers(ctx)
	return Server{Version: said.Version, Models: Models(labels), Loaded: LoadedModels(labels)}, nil
}
