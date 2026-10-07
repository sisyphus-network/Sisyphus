package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/planner"
	"github.com/sisyphus-network/Sisyphus/packages/nodedb"
	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
)

// modelCommand sets and shows which language model a node plans with. It
// works on the node's database directly, so the node need not be running;
// one that is picks up a change the next time it is asked something.
func modelCommand(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("expected model set, show or list")
	}
	fs := flag.NewFlagSet("sisyphusd model "+args[0], flag.ContinueOnError)
	dataDir := fs.String("data-dir", defaultDataDir(), "directory holding the node's data")
	provider := fs.String("provider", "ollama", "set: the kind of model service: ollama, openai (which is also for anything that speaks as OpenAI does) or anthropic")
	url := fs.String("url", "", "set: where the service is, if not in that provider's usual place")
	model := fs.String("model", "", "set: the model to plan with")
	keyFile := fs.String("api-key-file", "", "set: a file holding the service's key, if it wants one")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		return err
	}
	db, err := nodedb.Open(filepath.Join(*dataDir, "node.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	a := &assistant{store: db}

	switch args[0] {
	case "set":
		cfg := nodedb.ModelConfig{Provider: *provider, BaseURL: *url, Model: *model}
		if *keyFile != "" {
			key, err := os.ReadFile(*keyFile)
			if err != nil {
				return fmt.Errorf("read the service's key: %w", err)
			}
			cfg.APIKey = strings.TrimSpace(string(key))
		}
		if err := a.SetModelConfig(cfg); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "this node now plans with %s, from %s\n", cfg.Model, cfg.Provider)
		return nil
	case "show":
		cfg, found, err := a.ModelConfig()
		if err != nil {
			return err
		}
		if !found {
			fmt.Fprintln(stdout, "no model has been set")
			return nil
		}
		key := "none"
		if cfg.APIKey != "" {
			key = "set"
		}
		fmt.Fprintf(stdout, "provider: %s\nmodel:    %s\nurl:      %s\nkey:      %s\n", cfg.Provider, cfg.Model, cfg.BaseURL, key)
		return nil
	case "list":
		models, err := a.Models(ctx)
		if err != nil {
			return err
		}
		for _, name := range models {
			fmt.Fprintln(stdout, name)
		}
		return nil
	}
	return errors.New("expected model set, show or list")
}

// ask puts a question to a running node's planner, through its local API,
// and prints what the planner does as it does it.
func ask(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sisyphusd ask", flag.ContinueOnError)
	dataDir := fs.String("data-dir", defaultDataDir(), "directory holding the node's data, where its API token is")
	apiAddr := fs.String("api", "127.0.0.1:50051", "address of the node's local API, as given to its --api-listen")
	chat := fs.String("chat", "", "continue the conversation with this ID rather than start one")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("expected a question")
	}
	token, err := os.ReadFile(filepath.Join(*dataDir, "api.token"))
	if err != nil {
		return fmt.Errorf("read the node's API token (is it running with --api-listen?): %w", err)
	}
	conn, err := grpc.NewClient(*apiAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+strings.TrimSpace(string(token)))
	stream, err := nodepb.NewNodeServiceClient(conn).Ask(ctx, &nodepb.AskRequest{ChatId: *chat, Text: strings.Join(fs.Args(), " ")})
	if err != nil {
		return err
	}
	for {
		event, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		fmt.Fprint(stdout, describeAsk(event))
	}
}

// maxShown is how much of what a tool returned is printed.
const maxShown = 300

// describeAsk writes one thing the planner did as it is printed.
func describeAsk(e *nodepb.AskEvent) string {
	switch e.GetKind() {
	case planner.Text:
		return e.GetText()
	case planner.Calling:
		return fmt.Sprintf("\n[%s %s]\n", e.GetTool(), e.GetText())
	case planner.Job:
		return fmt.Sprintf("[job %s]\n", e.GetJobId())
	case planner.Result:
		shown := e.GetText()
		if len(shown) > maxShown {
			shown = shown[:maxShown] + "…"
		}
		return fmt.Sprintf("[%s]\n", shown)
	}
	// The end, with what to say to carry on.
	return fmt.Sprintf("\n\n(chat %s)\n", e.GetChatId())
}
