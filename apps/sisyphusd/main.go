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
  sisyphusd nodes                  list the workers connected to a coordinator
  sisyphusd blob put <file>        store a file on a node and print its CID
  sisyphusd blob get <cid>         fetch a blob, checking it against its CID
  sisyphusd blob stat <cid>        show a blob's size
  sisyphusd blob pin <cid>         keep a blob until unpinned, or for --ttl
  sisyphusd blob unpin <cid>       stop keeping a blob
  sisyphusd blob pins              list what the node is keeping, and for whom
  sisyphusd blob gc                delete stored data that nothing is keeping
  sisyphusd key new <file>         make a key for sealing a private job's data
  sisyphusd pool invite            issue an invitation for another node to join this one
  sisyphusd pool join <invitation> join another node, to use it from this machine
  sisyphusd pool members           list the nodes admitted to this one
  sisyphusd pool remove <node-id>  take a node off that list and disconnect it
  sisyphusd pool rekey             change the key of the pool's private IPFS network
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
			}
		}
		return errors.New(`expected "job submit" or "job get"`)
	case "nodes":
		return listNodes(ctx, args[1:])
	case "blob":
		return blobCommand(ctx, args[1:])
	case "key":
		return keyCommand(args[1:])
	case "pool":
		return poolCommand(ctx, args[1:])
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
