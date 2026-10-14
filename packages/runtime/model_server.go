package runtime

import "sync/atomic"

// ModelServer is the live, owner-controlled view of a worker's Ollama.
// Its address is fixed for the life of the worker; the owner may change
// what it offers and how requests use it without reconnecting the worker.
type ModelServer struct {
	url   string
	owner atomic.Pointer[modelServerOwner]
}

type modelServerOwner struct{ owner Owner }

// NewModelServer gives model workloads a stable address and replaceable,
// immutable owner settings.
func NewModelServer(url string, owner Owner) *ModelServer {
	s := &ModelServer{url: url}
	s.Update(owner)
	return s
}

// Settings returns one consistent snapshot of the address and owner policy.
func (s *ModelServer) Settings() (string, Owner) {
	if s == nil {
		return "", Owner{}
	}
	current := s.owner.Load()
	if current == nil {
		return s.url, Owner{}
	}
	return s.url, cloneOwner(current.owner)
}

// Update atomically replaces the policy used by subsequent model offers and
// requests.
func (s *ModelServer) Update(owner Owner) {
	if s == nil {
		return
	}
	owner = cloneOwner(owner)
	s.owner.Store(&modelServerOwner{owner: owner})
}

func cloneOwner(owner Owner) Owner {
	owner.Only = append([]string(nil), owner.Only...)
	if owner.Options != nil {
		options := make(map[string]any, len(owner.Options))
		for key, value := range owner.Options {
			options[key] = value
		}
		owner.Options = options
	}
	return owner
}

// WithLiveModels returns model workloads backed by one live model server.
func WithLiveModels(server *ModelServer) []Workload {
	return []Workload{Chat{Live: server}, Prompts{Live: server}, Embed{Live: server}}
}
