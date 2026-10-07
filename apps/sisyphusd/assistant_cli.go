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
	"github.com/sisyphus-network/Sisyphus/packages/ai"
	"github.com/sisyphus-network/Sisyphus/packages/nodedb"
	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
)

// modelCommand sets and shows which language model a node plans with, and
// fetches and removes models where a service keeps them on its machine. It
// works on the node's database directly, so the node need not be running;
// one that is picks up a change the next time it is asked something.
func modelCommand(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errModelCommand
	}
	fs := flag.NewFlagSet("sisyphusd model "+args[0], flag.ContinueOnError)
	dataDir := fs.String("data-dir", defaultDataDir(), "directory holding the node's data")
	provider := fs.String("provider", "ollama", "set, pull, remove: the kind of model service: ollama, openai (which is also for anything that speaks as OpenAI does) or anthropic")
	url := fs.String("url", "", "set, pull, remove: where the service is, if not in that provider's usual place")
	model := fs.String("model", "", "the model to plan with, to pull or to remove")
	keyFile := fs.String("api-key-file", "", "set, pull, remove: a file holding the service's key, if it wants one")
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

	cfg := nodedb.ModelConfig{Provider: *provider, BaseURL: *url, Model: *model}
	if *keyFile != "" {
		key, err := os.ReadFile(*keyFile)
		if err != nil {
			return fmt.Errorf("read the service's key: %w", err)
		}
		cfg.APIKey = strings.TrimSpace(string(key))
	}

	switch args[0] {
	case "set":
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
		models, err := a.Models(ctx, nil)
		if err != nil {
			return err
		}
		for _, m := range models {
			fmt.Fprintln(stdout, describeModel(m))
		}
		return nil
	case "providers":
		for _, k := range ai.Kinds() {
			fmt.Fprintf(stdout, "%-10s %s\n", k.ID, k.About)
		}
		return nil
	case "pull":
		// The same step is reported many times as it advances, and is
		// printed when what there is to say of it changes.
		last := ""
		err := a.PullModel(ctx, &cfg, *model, func(p ai.Progress) {
			line := p.Status
			if p.Total > 0 {
				line = fmt.Sprintf("%s %d%%", p.Status, 100*p.Done/p.Total)
			}
			if line != last {
				fmt.Fprintln(stdout, line)
				last = line
			}
		})
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%s is there to plan with: sisyphusd model set --model %s\n", *model, *model)
		return nil
	case "remove":
		if err := a.RemoveModel(ctx, &cfg, *model); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "removed %s\n", *model)
		return nil
	}
	return errModelCommand
}

var errModelCommand = errors.New("expected model set, show, list, providers, pull or remove")

// describeModel writes a model as `model list` prints it: its name, and
// after it whatever else is known.
func describeModel(m ai.Model) string {
	line := m.Name
	if m.Label != "" {
		line += "  " + m.Label
	}
	if m.Size > 0 {
		line += fmt.Sprintf("  %.1f GB", float64(m.Size)/1e9)
	}
	if m.Tools == ai.No {
		line += "  (cannot call tools, so cannot plan)"
	}
	return line
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
	conn, token, err := localAPI(*dataDir, *apiAddr)
	if err != nil {
		return err
	}
	defer conn.Close()
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
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

// localAPI reaches a running node's local API, and returns with it the
// token the node wants shown.
func localAPI(dataDir, addr string) (*grpc.ClientConn, string, error) {
	token, err := os.ReadFile(filepath.Join(dataDir, "api.token"))
	if err != nil {
		return nil, "", fmt.Errorf("read the node's API token (is it running with --api-listen?): %w", err)
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, "", err
	}
	return conn, strings.TrimSpace(string(token)), nil
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
