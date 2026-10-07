package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/planner"
	"github.com/sisyphus-network/Sisyphus/packages/ai"
	"github.com/sisyphus-network/Sisyphus/packages/nodedb"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
)

// assistant is a node's planner as its owner uses it: the model it is
// configured with, the conversations had with it, and the asking of
// questions. It is what the local API's planner calls are answered from.
type assistant struct {
	store assistantStore
	// pool is the pool the planner computes on, or nil on a node that
	// coordinates none.
	pool      planner.Pool
	workloads *runtime.Registry
	// offered says whether the pool has a worker that runs a workload now.
	offered func(workload string) bool
}

// assistantStore is where the configuration and conversations are kept. A
// nodedb.DB is one.
type assistantStore interface {
	ModelConfig() (nodedb.ModelConfig, bool, error)
	SetModelConfig(nodedb.ModelConfig) error
	Chats() ([]nodedb.Chat, error)
	CreateChat(nodedb.Chat) error
	DeleteChat(id string) error
	AppendChatMessages(chatID string, messages []string, now time.Time) error
	ChatMessages(chatID string) ([]string, error)
}

func (a *assistant) ModelConfig() (nodedb.ModelConfig, bool, error) { return a.store.ModelConfig() }

// SetModelConfig sets which model the node plans with, having checked that
// it names a provider there is.
func (a *assistant) SetModelConfig(cfg nodedb.ModelConfig) error {
	if _, err := ai.New(ai.Config{Provider: cfg.Provider}); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	if cfg.Model == "" {
		return status.Error(codes.InvalidArgument, "a model must be named")
	}
	return a.store.SetModelConfig(cfg)
}

// noModel is what a node says when asked to plan before it has been told
// what to plan with.
var errNoModel = status.Error(codes.FailedPrecondition, "this node has not been told which language model to use: set one with `sisyphusd model set`, or from the desktop")

// provider returns the model service the node is configured with.
func (a *assistant) provider() (ai.Provider, nodedb.ModelConfig, error) {
	cfg, found, err := a.store.ModelConfig()
	if err != nil {
		return nil, cfg, err
	}
	if !found {
		return nil, cfg, errNoModel
	}
	p, _ := ai.New(ai.Config{Provider: cfg.Provider, BaseURL: cfg.BaseURL, APIKey: cfg.APIKey}) // the provider was checked when it was set
	return p, cfg, nil
}

// service returns the model service at, or the configured one if at is nil.
func (a *assistant) service(at *nodedb.ModelConfig) (ai.Provider, error) {
	if at == nil {
		p, _, err := a.provider()
		return p, err
	}
	p, err := ai.New(ai.Config{Provider: at.Provider, BaseURL: at.BaseURL, APIKey: at.APIKey})
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return p, nil
}

// Models asks a service which models it offers: the one at, or the
// configured one if at is nil.
func (a *assistant) Models(ctx context.Context, at *nodedb.ModelConfig) ([]ai.Model, error) {
	p, err := a.service(at)
	if err != nil {
		return nil, err
	}
	return p.Models(ctx)
}

// library returns a service as one whose models are fetched.
func (a *assistant) library(at *nodedb.ModelConfig) (ai.Library, error) {
	p, err := a.service(at)
	if err != nil {
		return nil, err
	}
	library, is := p.(ai.Library)
	if !is {
		return nil, status.Error(codes.FailedPrecondition, "this service's models are not fetched or removed: it offers what it offers")
	}
	return library, nil
}

// PullModel fetches a model to the machine a service runs on.
func (a *assistant) PullModel(ctx context.Context, at *nodedb.ModelConfig, model string, progress func(ai.Progress)) error {
	library, err := a.library(at)
	if err != nil {
		return err
	}
	if model == "" {
		return status.Error(codes.InvalidArgument, "a model must be named")
	}
	return library.Pull(ctx, model, progress)
}

// RemoveModel deletes a fetched model from the machine a service runs on.
func (a *assistant) RemoveModel(ctx context.Context, at *nodedb.ModelConfig, model string) error {
	library, err := a.library(at)
	if err != nil {
		return err
	}
	if model == "" {
		return status.Error(codes.InvalidArgument, "a model must be named")
	}
	return library.Remove(ctx, model)
}

func (a *assistant) Chats() ([]nodedb.Chat, error) { return a.store.Chats() }

func (a *assistant) DeleteChat(id string) error { return a.store.DeleteChat(id) }

// Chat returns what has been said in a conversation.
func (a *assistant) Chat(id string) ([]ai.Message, error) {
	if err := a.known(id); err != nil {
		return nil, err
	}
	stored, err := a.store.ChatMessages(id)
	if err != nil {
		return nil, err
	}
	messages := make([]ai.Message, len(stored))
	for i, encoded := range stored {
		// Whatever is there was put there by Ask, as JSON.
		json.Unmarshal([]byte(encoded), &messages[i])
	}
	return messages, nil
}

// known reports, as an error, a conversation that is not on record.
func (a *assistant) known(id string) error {
	chats, err := a.store.Chats()
	if err != nil {
		return err
	}
	for _, c := range chats {
		if c.ID == id {
			return nil
		}
	}
	return status.Errorf(codes.NotFound, "chat %q not found", id)
}

// titleLength is how much of a conversation's first question names it.
const titleLength = 60

// Ask puts a question to the planner, in the conversation with the given ID
// or, if that is empty, a new one, and returns the conversation's ID. It
// reports what the planner does as it does it, each time with that ID.
//
// The question and whatever was said in answer are kept even if the planner
// fails part way, so that the conversation as recorded is the conversation
// as it happened.
func (a *assistant) Ask(ctx context.Context, chatID, text string, report func(chatID string, e planner.Event)) (string, error) {
	if a.pool == nil {
		return "", status.Error(codes.FailedPrecondition, "this node coordinates no pool, so its planner has nothing to compute on")
	}
	if text == "" {
		return "", status.Error(codes.InvalidArgument, "there is no question")
	}
	model, cfg, err := a.provider()
	if err != nil {
		return "", err
	}
	if chatID == "" {
		var raw [8]byte
		rand.Read(raw[:]) // never fails; see crypto/rand
		chatID = hex.EncodeToString(raw[:])
		title := text
		if len(title) > titleLength {
			title = title[:titleLength] + "…"
		}
		if err := a.store.CreateChat(nodedb.Chat{ID: chatID, Title: title, Created: time.Now()}); err != nil {
			return "", err
		}
	}
	history, err := a.Chat(chatID)
	if err != nil {
		return "", err
	}
	question := ai.Message{Role: ai.User, Content: text}
	p := &planner.Planner{Model: model, ModelName: cfg.Model, Pool: a.pool, Workloads: a.described()}
	said, planErr := p.Run(ctx, append(history, question), func(e planner.Event) { report(chatID, e) })

	record := make([]string, 0, len(said)+1)
	for _, m := range append([]ai.Message{question}, said...) {
		encoded, _ := json.Marshal(m) // a message is plain strings, which always encode
		record = append(record, string(encoded))
	}
	if err := a.store.AppendChatMessages(chatID, record, time.Now()); err != nil {
		return chatID, fmt.Errorf("the conversation could not be saved: %w", err)
	}
	return chatID, planErr
}

// described lists the node's workloads as the model is told of them.
func (a *assistant) described() []planner.Workload {
	var out []planner.Workload
	for _, name := range a.workloads.Names() {
		if a.offered(name) {
			out = append(out, planner.Workload{Name: name, Description: a.workloads.Describe(name)})
		}
	}
	return out
}
