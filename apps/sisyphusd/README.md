# sisyphusd

The Sisyphus node daemon. One binary runs the coordinator role, the worker role, or both, and doubles as the command-line client.

This is the walking skeleton: a coordinator splits a job into tasks, workers execute them, and the results are aggregated. There is no AI planner, persistence, authentication or container runtime yet; see the [roadmap](https://github.com/excho0/Sisyphus/issues/23).

## Build

Needs Go 1.27 or newer.

```sh
make build   # produces bin/sisyphusd
make test    # go vet and the test suite with the race detector
make cover   # fails unless the tests execute every statement of hand-written code
```

## Releases

Pushing a tag such as `v0.1.0` makes GitHub build `sisyphusd` and publish the binaries, with checksums, as a release. `scripts/build-release.sh <version> <dir>` does the same build locally.

| System | Processors |
| --- | --- |
| Linux | x86-64, ARM64, 32-bit ARM (v7), RISC-V 64 |
| macOS | Intel, Apple silicon |
| Windows | x86-64, ARM64 |
| FreeBSD | x86-64, ARM64 |
| OpenBSD, NetBSD | x86-64 |
| Android | ARM64, for use in a terminal such as Termux |

Only Linux on x86-64 is run by the test suite. The others are compiled and have not been run. Solaris, illumos, AIX and Plan 9 are not built because a dependency does not support them.

## Try it on one machine

The quick way, in one terminal, is `make demo`: it starts three nodes, runs a job across them, and shuts them down.

To run the nodes yourself, use one terminal per node (or one `tmux` pane each):

```sh
# Terminal 1: a coordinator that is also a worker
bin/sisyphusd run --node-id alpha --slots 4

# Terminal 2: a second worker joining it
bin/sisyphusd run --role worker --coordinator 127.0.0.1:7700 --node-id beta --slots 4 --data-dir ~/.sisyphus-beta

# Terminal 3
bin/sisyphusd nodes
bin/sisyphusd job submit --params '{"from":0,"to":3000000000}'
bin/sisyphusd job submit --mode full-worker --params '{"from":0,"to":3000000000}'
```

Every node keeps its data under `--data-dir` (default `~/.sisyphus`), so nodes sharing a machine each need their own. The first job is split across both workers; the second runs whole on one. Stop a worker with Ctrl-C while a job is running and its tasks move to the other.

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

## Node identity

Every node has a key pair, created the first time it runs and kept in `node.key` in its `--data-dir`, readable only by its owner. The node's ID is derived from the public key and starts `12D3KooW`.

```sh
bin/sisyphusd id                      # the ID of the node whose data is in ~/.sisyphus
bin/sisyphusd id --data-dir /srv/node
```

Losing `node.key` loses the identity; copying it to another machine gives that machine the same one. **The key is not used for anything yet.** Nodes still connect without encryption and are known to each other by the name given with `--node-id`. Securing connections with these keys is the next step.

## Storing data

Every node keeps a content-addressed store under `--data-dir`. A coordinator's store is durable: a blob it has accepted survives a crash or power loss. A worker-only node's store is a cache of what its coordinator holds. It skips waiting for the disk on each write, which makes fetching several times faster on a slow disk, and it is kept across restarts. Because a power cut can then leave a cached block incomplete, a worker checks a cached blob in full the first time it is used in each run and downloads it again if it does not hold up. On a fast disk, `--sync-cache` makes the cache wait for the disk after all, so that it survives a power cut without downloading again. With `--max-cache-bytes`, a worker evicts cached blobs once the cache passes that size: first those it has not used since it started, then the least recently used, and never one a task has open. The `blob` commands talk to a node running the coordinator role.

```sh
bin/sisyphusd blob put results.tar     # prints the blob's CID
bin/sisyphusd blob stat <cid>
bin/sisyphusd blob get -o copy.tar <cid>
```

A blob's CID is the one `ipfs add --cid-version=1` gives the same file, and blocks are laid out on disk the way Kubo lays them out, so data can move to IPFS later without being renamed. Both `put` and `get` check the bytes against the CID, so a node cannot substitute different data unnoticed.

Jobs pass large data by CID instead of inline. A worker downloads an input from its coordinator the first time a task needs it, checks it against the CID, and keeps it; whatever a task stores is uploaded to the coordinator before the task reports success.

```sh
cid=$(bin/sisyphusd blob put big.txt)
bin/sisyphusd job submit --workload wordcount --params "{\"input\":\"$cid\"}"
bin/sisyphusd blob get <output CID from the result> | head
```

### How long data is kept

A node keeps a blob for as long as something pins it, and deletes what nothing pins.

| Pin held for | Placed by | Lasts |
| --- | --- | --- |
| `user` | `blob put` and `blob pin` | Until `blob unpin`, or for `--ttl` |
| `job:<id>` | The coordinator, on a job's inputs and results | While the job runs, then for `--retain` (default 7 days) |
| `recent` | Every upload | One hour, so there is time to pin it properly |

Blobs that only pass between a job's tasks are released as soon as the job ends. Several pins can hold one blob; it goes when the last has lapsed.

```sh
bin/sisyphusd blob pins             # what is being kept, for whom, until when
bin/sisyphusd blob pin --ttl 72h <cid>
bin/sisyphusd blob unpin <cid>
bin/sisyphusd blob gc               # delete what nothing is keeping, now
```

A coordinator also collects every `--gc-interval` (default one hour), and with `--max-store-bytes` refuses uploads once its store reaches that size.

A job's result must be stored by the workload's aggregation step to be kept; a blob a task stored is treated as intermediate unless the aggregation stores it again.

## Workloads

Both built-in workloads are stand-ins that exercise the network rather than compute anything valuable. Real workloads will sit behind the same `Workload` interface in `packages/runtime`.

- **`primes`** counts the primes in `[from, to)`. Parameters and results are a few bytes and travel inline.
- **`wordcount`** counts word frequencies in a stored file. It exercises the storage path: the input is fetched by CID, each task reads only its byte range, task outputs are stored as blobs, and the result is a blob too. The result's CID is the same however the job is split, so two runs can be compared by CID alone.

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
- Without `--max-store-bytes` there is no limit on what can be uploaded, and anyone who can reach the port can pin, unpin and collect.
- Jobs are forgotten when a coordinator restarts. Pins they held open are given the retention period to lapse.
- Without `--max-cache-bytes` a worker's cache grows until the disk is full. The limit is not strict: blobs that tasks have open are kept even if they alone exceed it.
- A worker downloads a whole input even when its tasks need only part of it.
