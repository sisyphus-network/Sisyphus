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

const usage = `Usage:
  sisyphusd run [flags]            run a node (coordinator, worker or both)
  sisyphusd job submit [flags]     submit a job and follow it to completion
  sisyphusd job get <job-id>       show a job
  sisyphusd nodes                  list the workers connected to a coordinator
  sisyphusd blob put <file>        store a file on a node and print its CID
  sisyphusd blob get <cid>         fetch a blob, checking it against its CID
  sisyphusd blob stat <cid>        show a blob's size

Run "sisyphusd <command> -h" for a command's flags.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, "sisyphusd:", err)
		}
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
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
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	}
	return fmt.Errorf("unknown command %q", args[0])
}
