# sisyphusd

The Sisyphus node daemon. One binary runs the coordinator role, the worker role, or both, and doubles as the command-line client.

This is the walking skeleton: a coordinator splits a job into tasks, workers execute them, and the results are aggregated. There is no AI planner, persistence, authentication or container runtime yet; see the [roadmap](https://github.com/excho0/Sisyphus/issues/23).

## Build

Needs Go 1.27 or newer.

```sh
make build   # produces bin/sisyphusd
make test    # go vet and the test suite with the race detector
```

## Try it on one machine

The quick way, in one terminal, is `make demo`: it starts three nodes, runs a job across them, and shuts them down.

To run the nodes yourself, use one terminal per node (or one `tmux` pane each):

```sh
# Terminal 1: a coordinator that is also a worker
bin/sisyphusd run --node-id alpha --slots 4

# Terminal 2: a second worker joining it
bin/sisyphusd run --role worker --coordinator 127.0.0.1:7700 --node-id beta --slots 4

# Terminal 3
bin/sisyphusd nodes
bin/sisyphusd job submit --params '{"from":0,"to":3000000000}'
bin/sisyphusd job submit --mode full-worker --params '{"from":0,"to":3000000000}'
```

The first job is split across both workers; the second runs whole on one. Stop a worker with Ctrl-C while a job is running and its tasks move to the other.

## Try it across machines

On the coordinator machine, listen on an address the others can reach:

```sh
bin/sisyphusd run --listen 0.0.0.0:7700
```

On each other machine:

```sh
bin/sisyphusd run --role worker --coordinator <coordinator-ip>:7700
```

**The daemon has no encryption or authentication yet.** Anyone who can reach the port can submit jobs and join as a worker. Only do this on a network you trust.

## Storing data

Every node running the coordinator role keeps a content-addressed store under `--data-dir` (default `~/.sisyphus`).

```sh
bin/sisyphusd blob put results.tar     # prints the blob's CID
bin/sisyphusd blob stat <cid>
bin/sisyphusd blob get -o copy.tar <cid>
```

A blob's CID is the one `ipfs add --cid-version=1` gives the same file, and blocks are laid out on disk the way Kubo lays them out, so data can move to IPFS later without being renamed. Both `put` and `get` check the bytes against the CID, so a node cannot substitute different data unnoticed.

Jobs do not use the store yet; that is the next step.

## The `primes` workload

The one built-in workload counts the primes in `[from, to)`. It is here to exercise the network, not because the answer matters: it splits cleanly by sub-range, always gives the same answer, and any task can be checked by running it again. Real workloads will be added behind the same `Workload` interface in `packages/runtime`.

## Layout

| Path | What it holds |
| --- | --- |
| `proto/sisyphus/v1` | Message and service definitions. Edit these, then run `make proto`. |
| `packages/protocol` | Go code generated from `proto/`. Do not edit by hand. |
| `packages/job-model` | Job and task state machines. No I/O. |
| `packages/runtime` | The `Workload` interface and built-in workloads. |
| `packages/storage` | Content-addressed blob store with IPFS-compatible CIDs. |
| `apps/sisyphusd/coordinator` | Scheduling, retries, aggregation, worker connections. |
| `apps/sisyphusd/worker` | Connects to a coordinator and executes tasks. |
| `apps/sisyphusd/api` | gRPC server wiring and the client-facing `NodeService`. |

`make proto` needs `protoc` on your path and the plugins from `make tools`.

## Known limits

- State is in memory: restarting the coordinator loses every job.
- A task is tried at most three times, and losing its worker counts as a try.
- When a job fails, its other running tasks are left to finish and their results discarded; there is no cancellation.
- Workers are chosen by free slots only, not by hardware.
- Stored blobs are never deleted and there is no size limit or quota, so anyone who can reach the port can fill the disk.
- Two coordinators on one machine need different `--data-dir` values.
