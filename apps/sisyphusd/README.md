# sisyphusd

The Sisyphus node daemon. One binary runs the coordinator role, the worker role, or both, and doubles as the command-line client.

This is the walking skeleton: a coordinator splits a job into tasks, workers execute them, and the results are aggregated. There is no AI planner, persistence, authentication or container runtime yet; see the [roadmap](https://github.com/sisyphus-network/Sisyphus/issues/23).

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

To run the nodes yourself, use one terminal per node (or one `tmux` pane each). Every node keeps its key and data under `--data-dir`, so nodes sharing a machine each need their own. Left to itself a node uses the place its operating system sets aside for a program's data (`~/.local/share/sisyphus` on Linux, `~/Library/Application Support/sisyphus` on macOS, `%LOCALAPPDATA%\sisyphus` on Windows), or `~/.sisyphus` if that is already there from an earlier version. `sisyphusd data-dir` prints which.

```sh
# Terminal 1: a coordinator that is also a worker, using the default data directory
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
IPFS_PATH=$(bin/sisyphusd data-dir)/ipfs ipfs cat <cid>                  # anything stored is an ordinary IPFS file
```

- **Each node's Kubo is that node.** `ipfs id` and `sisyphusd id` print the same ID. Its repository is `ipfs` in the node's data directory, and it is started and stopped with the node.
- **The pool has a network of its own.** A coordinator started with `--kubo` creates a swarm key, kept in `swarm.key` in its data directory. Only Kubo daemons holding that key can connect to each other; they have no bootstrap peers, no gateway, and no way to reach or be reached from the public IPFS network.
- **Workers are given the key when they start**, over their encrypted connection to the coordinator, along with where to find the coordinator's Kubo and the other members'. A worker using `--kubo` needs a coordinator that does.
- **Data comes from whoever has it.** When a task opens a blob its worker does not hold, the worker's Kubo fetches the blocks from the members that do, the coordinator or other workers. If the network cannot supply it within thirty seconds, the worker falls back to asking the coordinator directly, and logs a warning saying so: the job still succeeds, but that warning means the private network is not working for that worker.
- **Removing a member changes the key.** A removed node still holds the key it was given, so `pool remove` gives the network a new one. The coordinator's Kubo restarts on it, which takes a few seconds, and tells the remaining workers, who fetch the new key and restart theirs. The removed node cannot ask for it. `pool rekey` does the same without removing anyone, for when a key may have leaked.
- **It needs no port of its own.** The coordinator's Kubo listens to its own machine only. A worker's Kubo reaches it through the connection the worker already has with the coordinator on `--listen`, so a pool needs one open port in all, and a worker needs none. See [One port](#one-port) below.
- Files added with the `ipfs` command on a node's repository can be used as job inputs by CID.
- The node's own pins decide what it keeps, as described below. Anything pinned with `ipfs pin add` is also kept. Do not run `ipfs repo gc` on the repository: Kubo's collector does not know about the node's pins.
- Moving an existing node to `--kubo`, or back, does not carry its stored data across.

### One port

To take part in a pool a node needs to make outgoing connections to its coordinator and nothing else. It opens no port, so it works from behind a home router or a firewall as it is. The coordinator is the one node that has to be reachable, on `--listen`.

That one port carries everything:

- **The node's own protocol**: jobs, tasks, uploads and downloads, over TLS.
- **A tunnel to the coordinator's Kubo**, for pools that use `--kubo`, inside that same TLS connection.
- **libp2p**, by which the pool's nodes reach each other. The coordinator tells the two kinds of caller apart by how they begin.

**Workers reach each other through the coordinator.** Every node runs a libp2p host under its own ID. A worker keeps a place on the coordinator, which relays: when one worker wants a blob another has cached, it asks for that worker by ID, and the coordinator joins the two. Having met, the two try each other's own addresses, and if either can reach the other, on the same network for instance, they connect directly and the coordinator carries nothing more. Only the pool's members are let in or relayed for.

Opening more is optional, and buys speed:

| What a node opens | What it gets |
| --- | --- |
| Nothing (any worker) | Takes tasks. Fetches from the coordinator, and from other workers through it or directly where the network allows. With `--kubo`, reaches the coordinator's Kubo through the tunnel. |
| `--serve <addr>` on a worker | Other workers fetch from it at that address without the coordinator's help. |
| `--swarm-port <port>` on a coordinator with `--kubo` | Workers' Kubo daemons connect to the coordinator's directly rather than through the tunnel. |

A worker's Kubo tries the coordinator's open port first, if it has one, and settles for the tunnel if nothing answers within two seconds, so opening the port and then forgetting the firewall rule costs speed, not the pool. The worker logs which it chose.

What the tunnel costs: every byte between a worker's Kubo and the coordinator's passes through both `sisyphusd` processes and is encrypted a second time. On one machine it moves about 300 MB/s against 2 GB/s for a direct connection, so it matters on a fast network with fast disks and not otherwise. Run `go test -run xxx -bench . ./apps/sisyphusd/tunnel` to measure it on yours.

What is not there yet:

- **Two workers on different private networks stay on the relay.** libp2p can sometimes connect such a pair directly ("hole punching"), and it is switched on, but it has not been tried across real home routers, so do not count on it.
- **Only the coordinator relays.** A worker with a port open is connected to directly, but does not yet carry traffic for others.
- **Kubo daemons do not use the relay.** Two workers' Kubo daemons with no route between them exchange blocks through the coordinator's.
- **IPv6**: a coordinator started with `--listen :7700` listens on IPv4 only. Give it an IPv6 address to listen on one.

### Private jobs

A pool's storage is shared: any member can fetch any blob it can name, and with Kubo, whatever is on the pool's private network. A private job keeps its data from everyone but you, the coordinator, and the workers that run it.

```sh
bin/sisyphusd key new job.key                                   # make a key; keep the file safe
cid=$(bin/sisyphusd blob put --key-file job.key big.txt)        # sealed before it leaves this machine
bin/sisyphusd job submit --workload wordcount --key-file job.key --params "{\"input\":\"$cid\"}"
bin/sisyphusd blob get --key-file job.key <output CID>          # unsealed after it arrives
```

- **What is sealed.** The input you seal, everything the job's tasks store, and its stored result. Each is encrypted with the key, so stores, caches and the private network hold only ciphertext.
- **Who gets the key.** The coordinator, when you submit the job, and from it each worker that is assigned one of the job's tasks. It is never reported back in the job, logged, or given to anyone else. Those nodes see the data in the clear, as they must to compute on it.
- **Where the key is kept.** In the coordinator's database, readable by its owner only, and only until the job finishes: an unfinished job could not be taken up after a restart without it. When the job finishes the key is erased from the database's files. That is as much as software can promise; a disk may keep traces of what it once held.
- **What still shows.** The size of each blob, and that the job happened. The job's parameters and the small result it reports (for `wordcount`, the counts of words and of distinct words) are not sealed; they travel over the pool's encrypted connections but are visible to clients of the coordinator.
- **Same result every time.** Sealing with the same key always gives the same bytes, so a private job's result has the same CID however the job is split, like any other. The other side of that: someone who can see two sealed blobs can tell whether they, or same-sized pieces at the same position in them, are identical under one key. Use a new key for data where that matters.
- **Lose the key and the data is gone.** `key new` will not overwrite a key file for that reason.
- **A job with a key can still read unsealed inputs**, so public data and private can be mixed.

The encryption is put together from standard parts (AES-256-GCM, HMAC-SHA256, HKDF) but the arrangement is this project's own and has not been reviewed by a cryptographer. Treat it as keeping honest pool members out, not as proof against a determined attacker, until it has been.

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

## Restarting a coordinator

A coordinator keeps its jobs in `node.db`, a SQLite database in its data directory, so stopping it loses nothing:

- **Finished jobs** are still there to be asked after, with their results.
- **Unfinished jobs** are taken up where they were. Tasks that had finished stay finished. Tasks that were running go back to wait for a worker, and that is not counted against them, however often it happens. If every task had finished and only combining their outputs was cut short, the combining is done again, with no worker needed.
- **A job's data** stays pinned across the restart for as long as the job takes.
- **Workers** reconnect by themselves once the coordinator is back.

A job's arrival and its finish are written through to the disk before anyone is told of them. The steps in between are not, so a power cut can lose the last few; the tasks concerned run again. A coordinator started without a workload that an unfinished job needs fails that job, and says why.

The database holds the rest of what a coordinator must not forget as well: which nodes have been admitted, and the invitations issued and not yet used, so an invitation is still good after a restart. Invitations are recorded by a hash of their token, so reading the database admits nobody. A coordinator from before this was so finds its old `access.json`, takes its members in, and sets the file aside as `access.json.imported`.

A finished job can be asked after for 30 days, or whatever `--keep-jobs` says; `--keep-jobs 0` keeps them for good. That is about the record of the job. How long its data is kept is `--retain`, described under [How long data is kept](#how-long-data-is-kept).

A node's own record of the coordinators it has joined, `known.json`, stays a file. Every command reads it, on machines that run no coordinator and have no database, and it is small enough to mend by hand when a coordinator's address changes.

The database also records every attempt at every task: which node, and how it went. Nothing reads that yet. It is there for accounting later, and goes when the job does.

## The desktop app

[`apps/sisyphus`](../sisyphus/README.md) is a desktop client that shows a node and its peers. It talks to a node on the same machine through the local API, `sisyphus.node.v1` in [`proto/sisyphus/node/v1`](../../proto/sisyphus/node/v1/node.proto). The local API is off unless asked for:

```sh
./sisyphusd run --name alpha --api-listen 127.0.0.1:50051
```

`127.0.0.1:50051` is where the desktop app looks. The address must be a loopback one: the local API has no TLS, and is for programs on this machine only.

What it shows, in this daemon's terms:

| The desktop's word | What it is here |
| --- | --- |
| peer | A node of this node's pool, or one its libp2p host has found: on the same network, through nodes it knows, or because it was given the address. |
| connected | The node has a live connection to this one right now, as a worker or between the two hosts. |
| trusted for compute | The node may work for this one. See [Trust](#trust) below. |
| connect to an address | Connects this node's host to the node at an address ending in `/p2p/<node ID>`, which is the form a node lists its own in, and notes it in the address book. |
| bootstrap peers | The node's address book: the addresses it connects to each time it starts. |
| country | Empty unless the node was started with `--locate-country`. Finding it means asking ipapi.co, which thereby learns the node's address, so it is not done unasked. |

Anything on the machine can read from the local API. Changing something needs the token in `api.token` in the data directory, which the daemon makes on first use and only its own user can read. The desktop app looks for it in the daemon's default data directory, or in the file named by `SISYPHUS_API_TOKEN_FILE`.

## Finding other nodes

A node looks for others and lets them find it, unless started with `--discovery off`:

- **On its own network**, by multicast DNS. Two nodes on one LAN find each other within a second or two with nothing configured.
- **Through the nodes it knows**, by a distributed hash table (Kademlia) that every node both uses and serves. A node that knows one other can find the rest, and can reach a node knowing only its ID.
- **From its address book**, the addresses it connects to each time it starts. `--bootstrap <address>,<address>` adds some for one run; the desktop's "connect to peer" and bootstrap list add them for good.

The names it uses for both are Sisyphus's own, so nodes find each other and not every other libp2p program in reach.

**Being found is not being let in.** With discovery on, any libp2p node may connect to a node's host. It can then see the node's ID and addresses and take part in the hash table, and that is all. Everything else checks who is asking: jobs, stored data, the relay and the private IPFS network are for the pool's members. With `--discovery off` a node's host accepts connections from its pool's members only, and finds nobody.

## Trust

A node works for another only if that other trusts it for compute. There are two ways to come to be trusted, and they end in the same place:

- **By invitation.** `pool invite` on the coordinator, `pool join` or `--join` on the worker. Use this for a node you cannot see from here, or to admit a client rather than a worker.
- **Directly.** The coordinator's owner trusts a node they can see, from the desktop's peer list. No token changes hands. The trusted node's owner then starts it as a worker with the coordinator's ID in place of an invitation:

  ```sh
  sisyphusd run --role worker --coordinator <address> --join <coordinator's node ID>
  ```

Either way the node is a worker in the pool, listed by `pool members`, and ending trust removes it as `pool remove` does. Trust is granted by the node whose jobs will run; it is the trusted node's owner who decides whether to take them, by starting it as that node's worker. A node cannot be made to work by being trusted.

What this does not do yet: a node works for one coordinator at a time, chosen when it starts. Two nodes that trust each other do not begin working for each other by themselves.

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
| `proto/sisyphus/node/v1` | The local API the desktop app uses. |
| `packages/protocol` | Go code generated from `proto/`. Do not edit by hand. |
| `packages/job-model` | Job and task state machines. No I/O. |
| `packages/runtime` | The `Workload` interface and built-in workloads. |
| `packages/storage` | Content-addressed blob store with IPFS-compatible CIDs. |
| `apps/sisyphusd/coordinator` | Scheduling, retries, aggregation, worker connections. |
| `apps/sisyphusd/worker` | Connects to a coordinator and executes tasks. |
| `apps/sisyphusd/api` | gRPC server wiring and the client-facing services. |
| `apps/sisyphusd/access` | Who has been admitted and in what role; invitations; checking every call. |
| `apps/sisyphusd/tunnel` | Carrying a TCP connection inside a gRPC stream. |
| `apps/sisyphusd/p2p` | The node's libp2p host: one port shared with gRPC, the relay, discovery, reaching a node by its ID. |
| `packages/identity` | Node keys, IDs, and the TLS settings built from them. |
| `packages/nodedb` | The node's SQLite database: jobs, tasks, attempts, members and invitations. |
| `packages/storage` | Content-addressed blob store, pins, garbage collection. |

`make proto` needs `protoc` on your path and the plugins from `make tools`.

## Known limits

- A task may fail three times, and losing its worker counts as a failure. Losing its coordinator does not.
- When a job fails, its other running tasks are left to finish and their results discarded; there is no cancellation.
- Workers are chosen by free slots only, not by hardware.
- A coordinator accepts whatever result a worker returns. Admit only workers you trust.
- A node is remembered by address. If a coordinator's address changes, its workers and clients must join again.
- A node's key cannot be changed without becoming a different node, and there is no way to stop a copied key being used other than removing that node.
- Encryption hides what nodes say to each other, not that they are talking, how much, or when.
- If a node using `--kubo` is killed outright rather than stopped, its Kubo daemon keeps running and must be stopped by hand before the node will start again.
- A removed node keeps whatever it had already fetched from the pool's private network. Changing the key stops it fetching anything more.
- Run `ipfs` commands against a node's repository only while the node is up. With its Kubo down, including for the few seconds of a key change, the `ipfs` command takes the repository's lock and the node cannot start Kubo until the command ends.
- Whatever is on the pool's private network can be fetched by every member of it. The network keeps outsiders out; it is private jobs, which seal their data, that keep members from reading each other's.
- Without `--max-store-bytes` there is no limit on what a worker or client can upload.
- A worker downloads a whole input even when its tasks need only part of it.
- Two workers that cannot reach each other exchange data through the coordinator, which is no faster than fetching from it.
- Without `--max-cache-bytes` a worker's cache grows until the disk is full. The limit is not strict: blobs that tasks have open are kept even if they alone exceed it.
