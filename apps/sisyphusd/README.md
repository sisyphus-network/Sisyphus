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

The quick way, in one terminal, is `make demo`: it starts three nodes, runs a few jobs across them, and shuts them down.

To run the nodes yourself, use one terminal per node (or one `tmux` pane each). Every node keeps its key and data under `--data-dir` (default `~/.sisyphus`), so nodes sharing a machine each need their own.

```sh
# Terminal 1: a coordinator that is also a worker, using ~/.sisyphus
bin/sisyphusd run --name alpha --slots 4

# Terminal 2: ask alpha for an invitation, then start a second worker with it
bin/sisyphusd pool invite
bin/sisyphusd run --role worker --coordinator 127.0.0.1:7700 --join <invitation> \
    --name beta --slots 4 --data-dir ~/.sisyphus-beta

# Terminal 3
bin/sisyphusd nodes
bin/sisyphusd job submit --params '{"from":0,"to":3000000000}'
bin/sisyphusd job submit --mode full-worker --params '{"from":0,"to":3000000000}'
```

The first job is split across both workers; the second runs whole on one. Stop a worker with Ctrl-C while a job is running and its tasks move to the other. A worker needs `--join` only the first time. After that it remembers its coordinator, and an invitation left on its command line is ignored, so the same command restarts it.

## Try it across machines

On the coordinator machine, listen on an address the others can reach, and issue an invitation for each worker:

```sh
bin/sisyphusd run --listen 0.0.0.0:7700
bin/sisyphusd pool invite        # once per worker; each invitation works once
```

On each other machine:

```sh
bin/sisyphusd run --role worker --coordinator <coordinator-ip>:7700 --join <invitation>
```

To submit jobs from another machine, invite it as a client instead and join from there:

```sh
bin/sisyphusd pool invite --role client                       # on the coordinator
bin/sisyphusd pool join --addr <coordinator-ip>:7700 <invitation>   # on the other machine
bin/sisyphusd nodes --addr <coordinator-ip>:7700
```

## Who can connect

Every connection between nodes is encrypted, and each end proves who it is.

**Identity.** A node has a key pair, created the first time it runs and kept in `node.key` in its data directory, readable only by its owner. Its ID is derived from the public key and starts `12D3KooW`; `sisyphusd id` prints it. The name given with `--name` is only a label. Losing `node.key` loses the identity, and copying it to another machine gives that machine the same one.

**Encryption.** Connections use TLS 1.3. Each node presents a certificate signed with its own key, with no certificate authority involved: a node is recognised by its ID, not by a host name.

**Roles.** A node answers a caller according to what the caller has been admitted as:

| Role | Who | May |
| --- | --- | --- |
| owner | Whoever holds the node's own key: its own worker, and commands run from its data directory | Everything, including inviting and removing others |
| worker | A node invited with `pool invite` | Take tasks, and fetch and store the blobs they need |
| client | A node invited with `pool invite --role client` | Submit and watch jobs, list nodes, and manage stored data |

Anyone else can complete the handshake and is then refused.

**Joining.** An invitation is the inviting node's ID and a one-time token. The ID is what makes joining safe: the joiner will only talk to the node named in it, so nobody in between can pose as that node. It lasts an hour unless `--ttl` says otherwise, and is void if the inviting node restarts before it is used. Once joined, a node remembers which node it expects at that address in `known.json` and refuses any other.

```sh
bin/sisyphusd pool members            # who has been admitted, and as what
bin/sisyphusd pool remove <node-id>   # take a node off the list and disconnect it
bin/sisyphusd pool rekey              # change the key of the pool's private IPFS network
```

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

### Workers serving each other

A worker normally downloads a job's input from its coordinator. Started with `--serve`, it also lets the other workers of its pool download from it what it has cached, and every worker asks the coordinator who else holds a blob before asking the coordinator for the blob itself:

```sh
bin/sisyphusd run --role worker --coordinator <addr> --join <invitation> \
    --serve 0.0.0.0:7701 --advertise <this-machine's-address>:7701
```

- `--serve` is where it listens; `--advertise` is the address other workers are told, needed when `--serve` names every interface.
- A worker serves only nodes the coordinator confirms are members of the pool, only reads, and over the same encrypted, identified connections as everything else. What it receives is checked against its CID, so a worker cannot pass off different data.
- The coordinator's knowledge of who holds what comes from which workers ran tasks that used a blob. It is a hint; if no worker named can supply the blob, the coordinator does.
- A node removed from the pool may be served by a worker for up to a minute more, which is how long a worker remembers that a node is a member.
- This needs no Kubo. With `--kubo` the pool's private IPFS network does the same job and is tried first.

### Kubo and the pool's private IPFS network

By default a node keeps blocks in files of its own and sends them to other nodes itself. With `--kubo` it instead starts [Kubo](https://github.com/ipfs/kubo), the main IPFS implementation, beside itself, keeps its blocks there, and lets the pool's Kubo daemons exchange them directly. The `ipfs` program must be installed on every node that uses it.

```sh
bin/sisyphusd run --kubo                                   # the coordinator
bin/sisyphusd run --kubo --role worker --coordinator <addr> --join <invitation>
IPFS_PATH=~/.sisyphus/ipfs ipfs cat <cid>                  # anything stored is an ordinary IPFS file
```

- **Each node's Kubo is that node.** `ipfs id` and `sisyphusd id` print the same ID. Its repository is `ipfs` in the node's data directory, and it is started and stopped with the node.
- **The pool has a network of its own.** A coordinator started with `--kubo` creates a swarm key, kept in `swarm.key` in its data directory. Only Kubo daemons holding that key can connect to each other; they have no bootstrap peers, no gateway, and no way to reach or be reached from the public IPFS network.
- **Workers are given the key when they start**, over their encrypted connection to the coordinator, along with where to find the coordinator's Kubo and the other members'. A worker using `--kubo` needs a coordinator that does.
- **Data comes from whoever has it.** When a task opens a blob its worker does not hold, the worker's Kubo fetches the blocks from the members that do, the coordinator or other workers. If the network cannot supply it within thirty seconds, the worker falls back to asking the coordinator directly, and logs a warning saying so: the job still succeeds, but that warning means the private network is not working for that worker.
- **Removing a member changes the key.** A removed node still holds the key it was given, so `pool remove` gives the network a new one. The coordinator's Kubo restarts on it, which takes a few seconds, and tells the remaining workers, who fetch the new key and restart theirs. The removed node cannot ask for it. `pool rekey` does the same without removing anyone, for when a key may have leaked.
- **The coordinator's Kubo must be reachable** by the workers' on `--swarm-port` (default 4101), over TCP.
- Files added with the `ipfs` command on a node's repository can be used as job inputs by CID.
- The node's own pins decide what it keeps, as described below. Anything pinned with `ipfs pin add` is also kept. Do not run `ipfs repo gc` on the repository: Kubo's collector does not know about the node's pins.
- Moving an existing node to `--kubo`, or back, does not carry its stored data across.

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

## Examples

[`examples/`](../../examples/README.md) has a script that runs a pool on this machine and leaves it up, scripts that run each workload against a pool, and a sample input. [`docs/multi-machine-test.md`](../../docs/multi-machine-test.md) is a guided test of a pool across real machines.

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
| `apps/sisyphusd/api` | gRPC server wiring and the client-facing services. |
| `apps/sisyphusd/access` | Who has been admitted and in what role; invitations; checking every call. |
| `packages/identity` | Node keys, IDs, and the TLS settings built from them. |
| `packages/storage` | Content-addressed blob store, pins, garbage collection. |

`make proto` needs `protoc` on your path and the plugins from `make tools`.

## Known limits

- State is in memory: restarting the coordinator loses every job. Pins those jobs held open are given the retention period to lapse.
- A task is tried at most three times, and losing its worker counts as a try.
- When a job fails, its other running tasks are left to finish and their results discarded; there is no cancellation.
- Workers are chosen by free slots only, not by hardware.
- A coordinator accepts whatever result a worker returns. Admit only workers you trust.
- A node is remembered by address. If a coordinator's address changes, its workers and clients must join again.
- A node's key cannot be changed without becoming a different node, and there is no way to stop a copied key being used other than removing that node.
- Encryption hides what nodes say to each other, not that they are talking, how much, or when.
- If a node using `--kubo` is killed outright rather than stopped, its Kubo daemon keeps running and must be stopped by hand before the node will start again.
- A removed node keeps whatever it had already fetched from the pool's private network. Changing the key stops it fetching anything more.
- Run `ipfs` commands against a node's repository only while the node is up. With its Kubo down, including for the few seconds of a key change, the `ipfs` command takes the repository's lock and the node cannot start Kubo until the command ends.
- Whatever is on the pool's private network can be read by every member of it. The network keeps outsiders out; it does not keep members apart.
- Without `--max-store-bytes` there is no limit on what a worker or client can upload.
- A worker downloads a whole input even when its tasks need only part of it.
- Workers that serve each other need a port of their own open to the other workers.
- Without `--max-cache-bytes` a worker's cache grows until the disk is full. The limit is not strict: blobs that tasks have open are kept even if they alone exceed it.
