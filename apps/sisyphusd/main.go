// Command sisyphusd is the Sisyphus node daemon and its command-line client.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

const defaultAddr = "127.0.0.1:7700"

// version is set by release builds; see scripts/build-release.sh.
var version = "dev"

const usage = `Usage:
  sisyphusd run [flags]            run a node (coordinator, worker or both)
  sisyphusd job submit [flags]     submit a job and follow it to completion
  sisyphusd job get <job-id>       show a job
  sisyphusd job logs <job-id>      print what has happened to a job, and follow it until it is over
  sisyphusd job cancel <job-id>    stop a job that has not finished
  sisyphusd job record <job-id>    print a finished job's record, its history as linked data (--verify to check it against the job, and --key-file to check a private job's commitments)
  sisyphusd nodes                  list the workers connected to a coordinator
  sisyphusd blob put <file>        store a file on a node and print its CID
  sisyphusd blob get <cid>         fetch a blob, checking it against its CID
  sisyphusd blob stat <cid>        show a blob's size
  sisyphusd blob pin <cid>         keep a blob until unpinned, or for --ttl
  sisyphusd blob unpin <cid>       stop keeping a blob, whether it was pinned by hand or taken back from followers
  sisyphusd blob pins              list what the node is keeping, and for whom
  sisyphusd blob gc                delete stored data that nothing is keeping
  sisyphusd name publish <cid>     point this node's name, which is its ID, at a stored file (--lifetime, --ttl)
  sisyphusd name resolve <node-id> ask a node what a name stands for, check the answer and print the CID
  sisyphusd blob replicas [<cid>]  show which storage followers hold copies of what the node has pinned, and what it has taken back from them
  sisyphusd blob restore           fetch back, on the followers' word, what they hold from a store this node has lost; needed only if it lost its key too
  sisyphusd blob pin-remote <cid> [name]  ask a pinning service to keep a blob too (--service <url> --key-file <file>, --wait)
  sisyphusd blob pins-remote       list what a pinning service has been asked to keep
  sisyphusd blob unpin-remote <request-id>  have a pinning service stop keeping something
  sisyphusd key new <file>         make a key for sealing a private job's data
  sisyphusd pool invite            issue an invitation for another node to join this one
  sisyphusd pool join <invitation> join another node, to use it from this machine
  sisyphusd pool members           list the nodes admitted to this one
  sisyphusd pool remove <node-id>  take a node off that list and disconnect it
  sisyphusd pool work-for <node-id>  take tasks from a node whenever it will have this one (--stop to stop)
  sisyphusd pool rekey             change the key of the pool's private IPFS network
  sisyphusd pool cluster           show which of the pool's nodes hold each thing pinned, in a pool run with --cluster
  sisyphusd model set|show|list    which language model this node plans with (set --provider --model [--url] [--api-key-file])
  sisyphusd model providers|pull|remove  the kinds of model service, and fetching a model into Ollama or deleting one
  sisyphusd ask <question>         put a question to a running node's planner (--chat <id> to carry on a conversation)
  sisyphusd mcp                    offer a running node to an AI agent, as a Model Context Protocol server on standard input and output
  sisyphusd inference              offer the language models of a pool this machine has joined, to programs on this machine (--addr, --listen)
  sisyphusd skill show|install     the skill that teaches an AI agent to use a pool: print it, or put it where the agent keeps skills (--dir)
  sisyphusd id                     print this node's ID, creating its key if it has none
  sisyphusd data-dir               print where this node keeps its data unless told otherwise
  sisyphusd version                print the version

Run "sisyphusd <command> -h" for a command's flags.
`

// exit is os.Exit; tests replace it.
var exit = os.Exit

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	exit(exitCode(run(ctx, os.Args[1:])))
}

// exitCode reports err to the user and returns the process's exit status.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	// The flag package has already printed its own message for ErrHelp.
	if !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(stderr, "sisyphusd:", err)
	}
	return 1
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return flag.ErrHelp
	}
	switch args[0] {
	case "run":
		return runDaemon(ctx, args[1:])
	case "job":
		if len(args) > 1 {
			switch args[1] {
			case "submit":
				return jobSubmit(ctx, args[2:])
			case "get":
				return jobGet(ctx, args[2:])
			case "cancel":
				return jobCancel(ctx, args[2:])
			case "logs":
				return jobLogs(ctx, args[2:])
			case "record":
				return jobRecord(ctx, args[2:])
			}
		}
		return errors.New(`expected job submit, get, logs, cancel or record`)
	case "nodes":
		return listNodes(ctx, args[1:])
	case "blob":
		return blobCommand(ctx, args[1:])
	case "name":
		return nameCommand(ctx, args[1:])
	case "key":
		return keyCommand(args[1:])
	case "pool":
		return poolCommand(ctx, args[1:])
	case "model":
		return modelCommand(ctx, args[1:])
	case "ask":
		return ask(ctx, args[1:])
	case "mcp":
		return serveAgent(ctx, args[1:])
	case "skill":
		return skillCommand(args[1:])
	case "inference":
		return serveInference(ctx, args[1:])
	case "id":
		return showIdentity(args[1:])
	case "data-dir":
		fmt.Fprintln(stdout, defaultDataDir())
		return nil
	case "version":
		fmt.Fprintln(stdout, "sisyphusd", version)
		return nil
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return nil
	}
	return fmt.Errorf("unknown command %q", args[0])
}
