# sisyphusd

The Sisyphus node daemon. One binary runs the coordinator role, the worker role, or both, and doubles as the command-line client.

A coordinator splits a job into tasks, workers execute them, and the results are aggregated. Around that are the pieces a pool needs: who may connect, storage that outlasts a job, private and verified jobs, containers, a planner, and a server for AI agents. What is built and what is not is on the [roadmap](https://github.com/sisyphus-network/Sisyphus/issues/23); what it protects and what it does not is in the [security model](../../docs/security-model.md).

## Build

Needs Go 1.27 or newer.

From the repository root, `nix develop` provides the Go toolchain and development dependencies. The daemon is the Go binary at `bin/sisyphusd`.

```sh
make build   # produces bin/sisyphusd
make test    # go vet and the test suite with the race detector
make cover   # fails unless the tests execute every statement of hand-written code
```

## Releases

Pushing a tag such as `v0.1.0` makes GitHub build `sisyphusd` and publish the binaries, with checksums, as a release. `scripts/build-release.sh <version> <dir>` does the same build locally.

| System | Processors |
| --- | --- |
| Linux | x86-64, ARM64, 32-bit ARM (v7), RISC-V 64, 32-bit x86, POWER (ppc64le), IBM Z (s390x), LoongArch 64 |
| macOS | Intel, Apple silicon |
| Windows | x86-64, ARM64 |
| FreeBSD, OpenBSD | x86-64, ARM64 |
| NetBSD | x86-64 |
| Android | ARM64, for use in a terminal such as Termux |

The whole test suite runs on Linux x86-64. The known-answer workloads, which have to give the same result wherever they run, are also run on every other row of the table, and every row's daemon is built:

- on GitHub's own Linux, Windows and macOS machines, on x86-64 and ARM64;
- under an emulator for Linux's other processors;
- natively for Android;
- in virtual machines for FreeBSD, OpenBSD and NetBSD, emulated ARM64 ones for FreeBSD and OpenBSD on ARM64.

Solaris, illumos, AIX, Plan 9 and NetBSD on ARM64 are not built because a dependency does not support them.

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

**Watching the work.** Each node's own output says what it is doing for the pool, so a pool can be followed from any of its machines:

```text
coordinator  msg="task given" task=34b7…/0 worker=laptop attempt=1
laptop       msg="task started" job=34b7… task=34b7…/0 workload=chat attempt=1 model=llama3.1:8b
laptop       msg="task done" job=34b7… task=34b7…/0 took=8.076s output_bytes=283
coordinator  msg="task returned" task=34b7…/0 worker=laptop took=8.079s
coordinator  msg="job finished" job=34b7… state=succeeded took=8.95s by="laptop×1"
```

A task that fails says why on the worker (`task failed … error=…`); one the coordinator took back says `task stopped`. A private job's tasks are logged without the model they ask for. `-v` adds the lower-level detail.

## Who can connect

Every connection between nodes is encrypted, and each end proves who it is.

**Identity.** A node has a key pair, created the first time it runs and kept in `node.key` in its data directory, readable only by its owner. Its ID is derived from the public key and starts `12D3KooW`; `sisyphusd id` prints it. The name given with `--name` is only a label. Losing `node.key` loses the identity, and copying it to another machine gives that machine the same one.

**Encryption.** Connections use TLS 1.3. Each node presents a certificate signed with its own key, with no certificate authority involved: a node is recognised by its ID, not by a host name.

**Roles.** A node answers a caller according to what the caller has been admitted as:

| Role | Who | May |
| --- | --- | --- |
| owner | Whoever holds the node's own key: its own worker, and commands run from its data directory | Everything, including inviting and removing others |
| worker | A node invited with `pool invite` | Take tasks, fetch any stored blob it can name and store blobs, be given the key of the pool's private network, and publish its own name |
| client | A node invited with `pool invite --role client` | Submit jobs, and watch and cancel the ones it submitted; list nodes; store data, and pin and release its own pins. One client does not see another's jobs or pins |

A member can hold both roles. A worker that redeems a client invitation (`pool join` with it, from the worker's data directory) keeps working and may also submit jobs, and `pool members` lists it as `worker,client`. Ending its trust for work (`SetPeerComputePermissions` with no work) takes only the worker role and ends its worker connection; it stays a client.

Anyone else can complete the handshake and is then refused.

**How many connections.** A node takes as many connections at once as half the files its system lets a program have open, which on a server is hundreds of thousands and on a laptop is still thousands; `--max-connections` sets another number. An eighth of them, and never fewer than 256, may come from one address, since the machines of a house or an office share one, and each command run from another machine is a connection of its own. New connections from one address are slowed only after that many have come in a row. Raise the system's own limit (`ulimit -n`) to raise the node's.

**Joining.** An invitation is the inviting node's ID and a one-time token. The ID is what makes joining safe: the joiner will only talk to the node named in it, so nobody in between can pose as that node. It lasts an hour unless `--ttl` says otherwise, and is still good if the inviting node restarts before it is used. Once joined, a node remembers which node it expects at that address in `known.json` and refuses any other.

```sh
bin/sisyphusd pool members            # who has been admitted, as what, and what each client keeps here
bin/sisyphusd pool remove <node-id>   # take a node off the list and disconnect it
bin/sisyphusd pool leave --addr COORD:7700  # take this node out of that pool, letting go of its files there
bin/sisyphusd pool rekey              # change the key of the pool's private IPFS network
bin/sisyphusd pool cluster            # with --cluster: which nodes hold each thing pinned
```

`pool members` says, for each client, how much it keeps in the coordinator's store. That covers what it pinned and what its jobs keep. A blob it keeps several ways counts once, and a blob that several clients keep counts in full for each, so the figures add up to more than the disk holds when clients share data. The disk itself is what `--max-store-bytes` limits. What a client uploads counts towards it at once, until it is pinned, when the pin counts instead, or collected. A coordinator started with `--max-store-bytes-per-client` holds each client to that figure ([discussion #341](https://github.com/sisyphus-network/Sisyphus/discussions/341)): an upload that would take a client past it stops with `RESOURCE_EXHAUSTED` and is not kept (counted as stored, which for a file of more than one 256 KiB piece is a little more than was sent), and so is a pin of a blob it does not yet keep, which is given back. Pinning what it already keeps costs nothing more. A task of one of its jobs whose outputs would take it past fails at once, and its job with it: none of those outputs are kept, the task is not tried again, and its workers are not blamed, since the same task would store the same again. A task checked by several workers, or audited, is held to the share once it is settled, and then only for what the results it was settled by stored: until then each result's outputs are kept for the task and counted for nobody, so a worker that is outvoted costs the client nothing, and they are let go when the task is settled or its job ends. What workers upload for a client's tasks while they run counts towards its share too, shared between the attempts that run at once, until each attempt ends. The coordinator's own uploads, pins and jobs count for nobody, and workers are not held to a share.

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
- **Kubo's API is open to this machine unless you close it.** It listens on a loopback port, and any program on the machine that finds the port can read what is stored unsealed, and pin and unpin. `--kubo-api-secret` has it answer only a caller that shows a secret, which the node makes and keeps in `kubo.token` in its data directory. The `ipfs` command must then be given it: `IPFS_PATH=$(bin/sisyphusd data-dir)/ipfs ipfs --api-auth "basic:sisyphus:$(cat "$(bin/sisyphusd data-dir)/kubo.token")" cat <cid>`. It cannot be used with `--cluster`, whose peer pins through the same API and cannot show a secret. Each node chooses for itself: a pool may mix nodes that use it with nodes that do not.
- Files added with the `ipfs` command on a node's repository can be used as job inputs by CID.
- The node's own pins decide what it keeps, as described below. Anything pinned with `ipfs pin add` is also kept. Do not run `ipfs repo gc` on the repository: Kubo's collector does not know about the node's pins.
- Moving an existing node to `--kubo`, or back, does not carry its stored data across.

### IPFS Cluster: pins held by several nodes

With `--kubo` alone, the coordinator's disk holds the only copy of the pool's data that anything promises to keep; what workers hold is a cache. With `--cluster` as well, each node also runs an [IPFS Cluster](https://ipfscluster.io) peer beside its Kubo, the pool's peers form one cluster, and whatever the coordinator pins is pinned on several nodes. The `ipfs-cluster-service` program must be installed on every node that uses it.

```sh
bin/sisyphusd run --kubo --cluster --replicas 1             # the coordinator: one copy besides its own
bin/sisyphusd run --kubo --cluster --role worker --coordinator <addr> --join <invitation>
bin/sisyphusd pool cluster                                  # which nodes hold what
```

- **The node's pins are the cluster's pins.** Everything in `blob pins`, apart from the hour's grace every upload gets and the records of jobs, which are not files, is pinned on the cluster, and unpinned from it when the last pin on the node goes: unpinned by hand, or lapsed and collected. Retention is still managed in the one place, with `blob pin`, `blob unpin`, `--retain` and `--ttl`. The coordinator syncs a change as soon as the node's pins change, and compares the lists again every minute to catch changes made outside the node.
- **`--replicas N` is how many nodes hold each pin besides the coordinator**, one unless told otherwise; `pool cluster` counts the coordinator too, so it shows N + 1. The coordinator always holds everything: its Kubo is where the node stores it in any case. The others are chosen by the cluster, by free space, and fetch the data over the pool's private network.
- **With fewer nodes than that, a pin is held by as many as there are**, and gains holders within about a minute of more joining.
- **When a holder goes, another takes its place.** A node not heard from for thirty seconds is taken to be gone, and the coordinator's peer has another node fetch what it held, if there is one that does not hold it already. If the node comes back, the pin stays where it was moved to.
- **Only the coordinator says what is pinned.** A worker's peer is a follower: it keeps what it is allocated, refuses to pin or unpin anything itself, and every peer ignores changes that do not come from the coordinator's. Someone who gives a pool disk space gives it nothing else.
- **The cluster is closed as the private network is.** Its secret is derived from the swarm key, so nothing more is handed out, and `pool remove` and `pool rekey` change both: each node restarts its cluster peer after its Kubo.
- **It needs no port.** Every peer listens on its own machine only. A worker's reaches the coordinator's through the coordinator's `--listen` port, as its Kubo does; only word of what is pinned and who is there travels that way. Workers' peers on different machines are not connected to each other, and need not be.
- **Each peer is its node**, with the node's ID and name, as its Kubo is. Its settings and its record of the pins are in `ipfs-cluster` in the node's data directory, and its log is `daemon.log` there.

`pool cluster` lists the members the coordinator's peer has heard from lately, and for each pin how many nodes hold all of it, out of how many were asked for, and which are still fetching or have failed:

```text
MEMBER                                                NAME   STATE
12D3KooWEixYEKkqKVZuwtPDXJaQs2Jevnv3CRz1B1sCsnnnWtSS  hub    answering
12D3KooWK5P7C6zFJ9ozcn9cHgQn1niHdbeW8YWH2cZJ5baCJqza  north  answering
12D3KooWRgaq6Nb65k1ZzAtBrLrwGS1X8GCGTnBXiupDuzZ3Z6eb  south  answering

each pin is to be held by 2 of them

CID                                                          COPIES  HELD BY     WAITING FOR
bafybeifnshe3y2i4sl46bbjbagfdu5xbsbq55uxqras5an73qgcqocpyeq  2/2     hub, north  -
```

**Sizing.** Every copy costs its size on the node that holds it, so a pool storing S with `--replicas N` needs (N + 1) × S of disk in all, S of it on the coordinator. The cluster allocates by what each Kubo reports as free, which is its `Datastore.StorageMax`, 10 GB unless changed, less what its repository holds. To offer more or less on a node, set it and restart the node:

```sh
IPFS_PATH=$(bin/sisyphusd data-dir)/ipfs ipfs config Datastore.StorageMax 200GB
```

A cluster peer is one more process per node; with a handful of pins it used 60 to 75 MB of memory here.

**Adding a storage node.** Start a worker with `--kubo --cluster`. It appears in `pool cluster` within half a minute. Pins that had fewer holders than asked for get it as one; pins that already had enough are not moved onto it.

**Removing one.** Stop it, or `pool remove` it. Thirty seconds later the coordinator's peer starts moving what it held to other nodes, if there are any that do not hold it already; `pool cluster` shows `1/2` until they have fetched it. A removed node is shut out by the change of key, and keeps on its disk what it had.

**Next to the built-in replication.** `--replicas` and `--replica-dir`, described under [Copies on other nodes](#copies-on-other-nodes), are replication done by `sisyphusd` itself, with no other program to run. The two are alternatives: this one is for pools that already run Kubo and want IPFS Cluster's allocation by free space and its repair, at the price of a second program on every node. `--replicas` means the same for both, copies besides the coordinator's own, and with `--cluster` it is the cluster that keeps them: the built-in scheme is then off, and a worker's `--replica-dir` holds nothing.

What to know before relying on it:

- Everything said of `--kubo` above and under [Known limits](#known-limits) holds here too: a node killed outright leaves both programs running, and both must be stopped by hand before it will start again.
- While the coordinator is down the copies stay where they are, and nothing is moved or repaired.
- Data is safe against the loss of a node only once `pool cluster` shows it held by more than one. A pin made a moment ago is still being fetched.
- Losing the coordinator's data directory loses the list of what is pinned, on the node and on the cluster, though the other holders still have the blocks. There is no way yet to rebuild a coordinator from them.
- A worker's disk is freed of what the cluster has let go only when its cache is next trimmed, which happens only with `--max-cache-bytes`. That limit counts what the cluster has the node hold, so set it well above that.
- A node that works for other pools (see [Trust](#trust)) joins their private networks and not their clusters.
- A pin put on the cluster by hand, with `ipfs-cluster-ctl` and under any name but `sisyphus`, is left alone and not shown by `pool cluster`.
- Tried with IPFS Cluster 1.1.6 and Kubo 0.43.1, on one machine: three nodes, a 50 MB file copied to a second node in four seconds, and to a third forty seconds after the second was stopped. Not tried across machines, nor with more than three nodes or more than a few pins.

### One port

To take part in a pool a node needs to make outgoing connections to its coordinator and nothing else. It opens no port, so it works from behind a home router or a firewall as it is. The coordinator is the one node that has to be reachable, on `--listen`.

That one port carries everything:

- **The node's own protocol**: jobs, tasks, uploads and downloads, over TLS.
- **A tunnel to the coordinator's Kubo**, for pools that use `--kubo`, inside that same TLS connection, and another to its cluster peer for pools that use `--cluster`.
- **libp2p**, by which the pool's nodes reach each other. The coordinator tells the two kinds of caller apart by how they begin.

**Workers reach each other through the coordinator.** Every node runs a libp2p host under its own ID. A worker keeps a place on the coordinator, which relays: when one worker wants a blob another has cached, it asks for that worker by ID, and the coordinator joins the two. Having met, the two try each other's own addresses, and if either can reach the other, on the same network for instance, they connect directly and the coordinator carries nothing more. A worker that loses its place, because the coordinator restarted or the network broke, asks for it again every ten seconds, however long the coordinator was away, so it has its place back within ten seconds of the coordinator's return. Only the pool's members are let in or relayed for.

Opening more is optional, and buys speed:

| What a node opens | What it gets |
| --- | --- |
| Nothing (any worker) | Takes tasks. Fetches from the coordinator, and from other workers through it or directly where the network allows. With `--kubo`, reaches the coordinator's Kubo through the tunnel. |
| `--serve <addr>` on a worker | Other workers fetch from it at that address without the coordinator's help. |
| `--relay <addr>` on a worker | It becomes a full node: other members connect to its libp2p host there, and it relays between them as the coordinator does. |
| `--swarm-port <port>` on a coordinator with `--kubo` | Workers' Kubo daemons connect to the coordinator's directly rather than through the tunnel. |

A worker's Kubo tries the coordinator's open port first, if it has one, and settles for the tunnel if nothing answers within two seconds, so opening the port and then forgetting the firewall rule costs speed, not the pool. The worker logs which it chose.

What the tunnel costs: every byte between a worker's Kubo and the coordinator's passes through both `sisyphusd` processes and is encrypted a second time. On one machine it moves about 300 MB/s against 2 GB/s for a direct connection, so it matters on a fast network with fast disks and not otherwise. Run `go test -run xxx -bench . ./apps/sisyphusd/tunnel` to measure it on yours.

**Full nodes.** A worker started with `--relay <host:port>`, on a port the other members can reach, relays between them. It tells the coordinator where it is; every worker asks the coordinator from time to time which members relay and keeps a place on each, so any two can be joined through the coordinator or through any full node. Each full node counts what it carries and reports it to the coordinator, and `sisyphusd nodes` shows it in the `RELAYED` column. That count is what a reward for relaying would be worked out from; nothing pays one yet.

**Both kinds of address.** A coordinator started with `--listen :7700` listens on every IPv4 and IPv6 address the machine has. One that names an address listens on that one.

What is not there yet:

- **Two workers on different private networks may stay on a relay.** libp2p can sometimes connect such a pair directly ("hole punching"), and it is switched on. On one machine, relayed pairs do go direct by themselves. Whether they do across two real home routers has not been tried, so do not count on it. Step 4 of [`docs/multi-machine-test.md`](../../docs/multi-machine-test.md) says how to tell.
- **Full nodes are preferred as relays.** When several are available, a node rotates through them so the first one in the list does not take every connection. It gives full nodes two seconds in total to connect, then falls back to the explicitly configured relay (the coordinator in an ordinary pool) if none answers. Direct connection is still tried after the first contact, as before.
- **A full node's count is its own word.** Nothing checks it against what the nodes at either end saw.
- **Kubo daemons do not use the relay.** Two workers' Kubo daemons with no route between them exchange blocks through the coordinator's.

### Private jobs

A pool's storage is shared: any member can fetch any blob it can name, and with Kubo, whatever is on the pool's private network. A private job keeps its data from everyone but you, the coordinator, and the workers that run it.

```sh
bin/sisyphusd key new job.key                                   # make a key; keep the file safe
cid=$(bin/sisyphusd blob put --key-file job.key big.txt)        # sealed before it leaves this machine
bin/sisyphusd job submit --workload wordcount --key-file job.key --params "{\"input\":\"$cid\"}"
bin/sisyphusd blob get --key-file job.key <output CID>          # unsealed after it arrives
```

- **What is sealed.** The input you seal, everything the job's tasks store, and its stored result, so stores, caches and the private network hold only ciphertext. Nothing is sealed with your key itself: each stored file is sealed with a key of its own derived from yours, and what a job stores with a key derived for that job. Your key opens all of them.
- **Who gets which key.** The coordinator gets your key, when you submit the job. A worker assigned one of the job's tasks gets less: the job's own key, and the key of each sealed file the job's parameters name, wherever in them. With those it reads this job's inputs and what this job stored, and nothing else your key seals: not your other files, and not what your other jobs stored. Those workers still see this job's data in the clear, as they must to compute on it. No key is reported back in the job or logged.
- **A file a task reaches only through another file** is not one the job names, so its workers are given no key to it. Name every sealed input in the parameters.
- **Files sealed by an earlier version** were sealed with your key itself, and only that opens them. A job that names one runs, with its workers given your key whole as they once always were, and says so in its events. Store the file again and it gets a key of its own.
- **A worker from an earlier version is given no private job's tasks.** It would not know the new keys and would store unsealed what it was to seal, so a private job waits for a worker that is up to date.
- **Where the key is kept.** In the coordinator's database, readable by its owner only, and only until the job finishes: an unfinished job could not be taken up after a restart without it. When the job finishes the key is erased from the database's files. That is as much as software can promise; a disk may keep traces of what it once held.
- **What still shows.** The size of each blob, and that the job happened. The job's parameters, the small result it reports (for `wordcount`, the counts of words and of distinct words), what each task returns to the coordinator and error messages are not sealed; they travel over the pool's encrypted connections but are visible to clients of the coordinator. What a private job's tasks log is not passed on at all: a worker sends one line saying so in its place, since a log is kept unsealed and a `container` task's log is everything its command printed. An error may still quote the last line a failed command printed.
- **The same file is sealed the same way each time**, so storing it twice keeps one copy, and the workers of one job, who share its key, store the same blobs for the same work, which is how a private job is [verified](#verifying-results). Two jobs have different keys: run the same job twice and the results hold the same data under different CIDs, and nobody can tell from the store that they are alike. Within one job, and between two files of more than 4 MiB that begin with the same 4 MiB, which share a key, someone who can see two sealed blobs can tell whether they, or same-sized pieces at the same position in them, are identical.
- **Its record leaves the unsealed parts out.** A finished job's [record](#a-jobs-record) goes into the shared store, so a private job's holds no parameters, results or error messages.
- **Lose the key and the data is gone.** `key new` will not overwrite a key file for that reason.
- **A job with a key can still read unsealed inputs**, so public data and private can be mixed.
- **A job without a key is refused sealed data.** It has no key to it, and would otherwise compute on the sealed bytes as though they were the data. It is told the blob is sealed and to submit the job as private: when it is submitted, if the workload reads its input to split it, and as a failed task if not.

The encryption is put together from standard parts (AES-256-GCM, HMAC-SHA256, HKDF) but the arrangement is this project's own and has not been reviewed by a cryptographer. Treat it as keeping honest pool members out, not as proof against a determined attacker, until it has been.

### How long data is kept

A node keeps a blob for as long as something pins it, and deletes what nothing pins.

| Pin held for | Placed by | Lasts |
| --- | --- | --- |
| `user` | `blob put` and `blob pin`, by the node's owner | Until `blob unpin`, or for `--ttl` |
| `user:<node ID>` | `blob put` and `blob pin`, by a client of the pool | Until that client's `blob unpin`, or the owner's, or for `--ttl`. A client sees its own pins in `blob pins` and no one else's |
| `restored` | A coordinator that lost its store, on what it took back from its followers | Until the `blob unpin` of the node's owner, not of a client, or until the time the lost store kept it to; see [Copies on other nodes](#copies-on-other-nodes) |
| `job:<id>` | The coordinator, on a job's inputs and results | While the job runs, then for `--retain` (default 7 days) |
| `record:<id>` | The coordinator, on a finished job's [record](#a-jobs-record) | Until the job is forgotten (`--keep-jobs`) |
| `chat:<id>` | The node, on a file attached to a conversation with its planner | Until the conversation is deleted. The change is queued in the node's database with the conversation and applied to the store after, so one that fails, or that a stop cut short, is tried again every two seconds and when the node next starts. A pin of a file the node no longer holds can never be made, so it is reported to whoever attached it and dropped, and does not hold up the changes behind it |
| `chat-draft` | New private chat uploads through `StoreFile` with `chat_attachment` | 24 hours from the upload; another upload of the same content renews this grace period. Sending the message adds an independent `chat:<id>` pin |
| `recent` | Every upload | One hour, so there is time to pin it properly |
| `pool:<store>` | A storage follower, in its own store, on what its coordinator has it hold | Until the coordinator stops listing it; see [Copies on other nodes](#copies-on-other-nodes) |
| `pinning-service:<request>` | A request made through the [pinning service](#pinning-services) | Until the request is removed |

Blobs that only pass between a job's tasks are released as soon as the job ends. Several pins can hold one blob; it goes when the last has lapsed.

New chat attachments do not receive the permanent `user` pin used by ordinary file uploads. Deleting a conversation releases only that conversation's ownership; another conversation, a job or an explicit user pin can still keep the same content. Abandoned uploads, including completed uploads from a partially failed send, become eligible for collection after the 24-hour draft grace period. Deletion is not immediate: the event-driven collector sweeps after a pin is released or expires, and every other pin must have expired or been released. Explicitly setting `--gc-interval 0` disables automatic collection and requires manual `blob gc`.

The desktop checks both `chat-file-references-v1` and `chat-attachment-retention-v1` before uploading chat attachments. Older daemons cannot safely ignore the new upload flag. Existing permanently pinned uploads are not retroactively unpinned. The file list hides content that the store no longer holds, and reports storage failures rather than treating them as absence. Listing does not delete catalogue metadata or ownership: a concurrent upload or a later restore can make the content available again. Metadata compaction and paginated listing remain separate follow-ups.

```sh
bin/sisyphusd blob pins             # what is being kept, for whom, until when
bin/sisyphusd blob pin --ttl 72h <cid>
bin/sisyphusd blob unpin <cid>
bin/sisyphusd blob gc               # delete what nothing is keeping, now
```

A coordinator collects once at startup, after pin releases, and when the next pin expires. By default it does not run a periodic full-store scan; a positive `--gc-interval` adds an optional safety sweep, while explicitly setting it to `0` disables automatic collection. With `--max-store-bytes`, it refuses uploads once its store reaches that size.

A job's result must be stored by the workload's aggregation step to be kept; a blob a task stored is treated as intermediate unless the aggregation stores it again.

### Fetching a file with a browser

A node can serve what it holds over plain HTTP, read-only, by content ID, as any IPFS gateway does:

```sh
bin/sisyphusd run --gateway-listen 127.0.0.1:8080
curl -H "Authorization: Bearer $(cat "$(bin/sisyphusd data-dir)/api.token")" http://127.0.0.1:8080/ipfs/<cid>
```

- **It is its owner's unless opened.** A request must show the node's API token, as a bearer token or as `?token=` in the address. Started with `--gateway-open`, anyone who can reach the address and knows a file's content ID can fetch it, which is how a link to a result is shared; a content ID cannot be guessed, but anyone given one can pass it on.
- **It speaks plain HTTP.** At a loopback address that is this machine talking to itself. At an address other machines can reach, the token crosses the network unencrypted with each request, and the node says so in its log when it starts: put a proxy that speaks TLS in front of it, or open it with `--gateway-open` and serve only what may be public. A token in `?token=` also ends up in browser histories and proxy logs.
- **A sealed file is never served**, with or without the token: what the gateway holds of it is what was sealed. Private data is fetched through the node, by whoever has its key.
- `?filename=report.pdf` names the file for the browser and decides what kind it is taken for; without it the kind is told from how the file begins. Part of a file may be asked for, so video can be played and seeked from it.
- It serves single files only. A directory, or a path inside one, is not something a node stores.

### Names for what changes

A content ID names bytes that never change. A node's **name** is its ID, and stands for whichever file the node last pointed it at:

```sh
CID=$(bin/sisyphusd blob put report.txt)
bin/sisyphusd name publish "$CID"       # prints the name: this node's ID
bin/sisyphusd name resolve <node-id>    # prints the content ID the name now stands for
curl -H "Authorization: Bearer $(cat "$(bin/sisyphusd data-dir)/api.token")" http://127.0.0.1:8080/ipns/<node-id>
```

- **What is published is a signed record**: the content ID, a sequence number that only goes up, and the time until which the record is good, signed with the node's key. It is an IPNS record, the kind Kubo makes, so other IPFS programs can read and check it. `name publish` signs it on the machine it is run on, with the key in `--data-dir`, and hands it to the node at `--addr`, which keeps it in its database, across restarts, and answers with it from then on.
- **A node publishes under its own name only.** A coordinator takes a record from its owner's command line and from each node it has admitted, worker or client, and each record must be signed by the node that hands it over. So a worker's name is answered for by its coordinator once `sisyphusd name publish --addr <coordinator> <cid>` has been run on the worker's machine. A record that another node signed is refused, whoever signed it, and so is one no newer than the record held. A node removed from the pool is no longer answered for.
- **Whoever resolves a name checks the answer.** `name resolve` asks the node at `--addr`, which must be this machine's own node or one it has joined, then checks the record's signature against the name and refuses a record that has expired. The node asked is not taken at its word.
- **Through the gateway**, `/ipns/<node ID>` serves the file the name stands for, under the rules `/ipfs/<cid>` has: the token unless `--gateway-open`, and never a sealed file. The name may be spelled as the node prints it (`12D3KooW...`) or as IPFS programs do (`k51...`). Asked with `Accept: application/vnd.ipfs.ipns-record`, or with `?format=ipns-record`, the gateway returns the signed record itself instead, as the IPFS gateway specification describes, for a reader who would rather check it than trust the gateway. Neither answer is marked as keepable for good: a browser or cache may keep it for the record's `--ttl`, or for what is left of the record's life if that is less.
- **A record runs out.** It is good for `--lifetime`, 48 hours unless told otherwise. After that the gateway answers 404 and `name resolve` refuses it, until the node publishes again. Nothing republishes a record that was published by hand.
- **A node has one name.** Publishing again replaces what the name stood for. There is no way yet to have several names, or to unpublish one other than letting its record run out.
- **Naming a file does not keep it.** A name can point at a file the node does not hold, or stops holding; the gateway then answers 404. `blob put` pins what it stores, and `blob pin` pins anything else.
- **With `--kubo`**, a coordinator also gives each record it takes to its Kubo, which puts it on the pool's private network, so that `ipfs name resolve /ipns/<node ID>` and `ipfs cat /ipns/<node ID>` work against any member's Kubo. That happens when a record is published and not again: Kubo lets go of a record it has not been given afresh for 48 hours, and a member's Kubo that is asked later than that will not find it. If Kubo will not take a record the node says so in its log and holds the record all the same.
- Names are slow to change by design: whoever has an answer may go on using it for the TTL. They are not for anything that must be seen to change within seconds.
- Not built: DNSLink (a domain name standing for a name), publishing to the public IPFS network, and telling readers of a change as it happens (IPNS over pubsub).

#### A coordinator's description of itself

Started with `--publish-record`, a coordinator keeps a small JSON document about itself stored, and its name pointed at it:

```json
{
  "id": "12D3KooW...",
  "version": "dev",
  "workloads": ["chat", "container", "..."],
  "members": [
    {"id": "12D3KooW...", "role": "worker"}
  ]
}
```

- **What it reveals** is exactly that: the node's ID, the version of `sisyphusd` it runs, the names of the workloads it takes jobs for, and the ID and role of every node admitted to its pool. It does not hold addresses, the labels given with `--name`, hardware, which members are connected, or anything about jobs.
- **Who can read it** is whoever can resolve the name and fetch the file: every member of the pool; anyone with the node's API token, through its gateway; anyone at all who can reach the gateway, if it was started with `--gateway-open`; and, with `--kubo`, every member's Kubo. A list of members is its owner's to give out, which is why this is off unless asked for.
- At startup, and whenever the pool's membership or the record held under the node's name changes, the coordinator promptly publishes its current description. A failed publication is retried after 30 seconds; an unchanged record is renewed after 24 hours, halfway through its 48-hour lifetime. The document is pinned for as long as a record points at it.
- It uses the node's one name. If the record held under that name changes, the coordinator promptly republishes its description, replacing the other record. If publication fails, the retry takes place after 30 seconds.
- Only a coordinator has it. A worker-only node publishes no description of itself.

### In a bucket

A coordinator can keep the pool's stored data in a bucket of an object store that speaks S3, instead of on its own disk: Ceph, MinIO, SeaweedFS, Storj, Wasabi, AWS.

```sh
printf '%s\n%s\n' "$ACCESS_KEY" "$SECRET_KEY" > s3.keys && chmod 600 s3.keys
bin/sisyphusd run --s3-endpoint https://s3.eu-central-1.wasabisys.com --s3-bucket my-pool --s3-credentials s3.keys
```

- **The bucket must exist**; the node checks that it can reach it and stops if not. `--s3-prefix` puts the pool's objects under a name of their own, to share a bucket; `--s3-region` is for stores that have regions.
- **Nothing else changes.** A file has the same CID wherever its bytes are, so jobs, workers and clients need know nothing of it. Workers still fetch from the coordinator and each other; only the coordinator talks to the bucket, and the credentials never leave it.
- **Each block is an object**, of up to 256 KiB, under `blocks/`. What is kept and for how long is still the node's to decide: pins are in its data directory, and `blob gc` deletes from the bucket what nothing is keeping.
- **It is slower than a disk** when the bucket is far away: reading a gigabyte is four thousand requests, one after another. Nothing is cached on the coordinator. Use it for durability and room, and a bucket near the coordinator.
- It cannot be combined with `--kubo`, which is another answer to where the data is kept.
- Tried against SeaweedFS's S3 gateway. The others named speak the same dialect and have not been tried.

### Copies on other nodes

A coordinator holds the only durable copy of its pool's stored data. With `--replicas` it has other members each keep a copy of everything it has pinned, so that the data outlives the disk it is on. Those members are storage followers: worker-only nodes started with `--replica-dir`. There is nothing else to run.

```sh
# The coordinator: two copies of everything besides its own
bin/sisyphusd run --name alpha --replicas 2

# A follower, on another machine: a worker that also holds copies
bin/sisyphusd run --role worker --coordinator <addr> --join <invitation> \
    --replica-dir /mnt/big/sisyphus-copies

bin/sisyphusd blob replicas         # each pinned blob: copies held against copies wanted, and by whom
bin/sisyphusd blob replicas <cid>
```

- **What is copied** is every blob the coordinator has pinned for longer than the hour a new upload gets: what `blob put` and `blob pin` keep, and what jobs keep. When the last such pin on a blob is released or lapses, the followers delete their copies. The record of a job is not copied: it is linked data and not a file, and can be made again from the job.
- **A follower decides nothing.** Once a minute it asks the coordinator what to hold and says what it holds. It fetches what it lacks from the coordinator, checked against its CID, and deletes what is no longer listed. No other node can hand it anything to keep, and nothing it says changes what the coordinator keeps.
- **The coordinator signs each list**, and the follower keeps the last one beside its copies. That is what lets a coordinator that has lost its store take its data back without trusting anyone; see [below](#when-the-coordinators-store-is-lost).
- **The copies are in a store of their own**, in `--replica-dir`, written through to the disk like a coordinator's and apart from the worker's cache. Use the directory for nothing else: whatever is in it that the coordinator has not listed is deleted. It is always on the follower's disk, whether or not the pool uses `--kubo` or a bucket.
- **Which followers hold a blob** is settled by hashing each follower's ID with the blob's, so every blob ranks the followers in an order of its own and the first few hold it. Data therefore spreads evenly, and a follower that joins takes a share from each of the others while nothing moves between them.
- **A copy that is to move stays where it is** until the follower taking it over says it has it. Changing the followers never leaves fewer copies than there were.
- **A follower that is not heard from for ten minutes** is given up on, and the others are told to hold its share. One that is back sooner, after a restart say, causes nothing to move. Its copies stay on its disk while it is away.
- **A coordinator that has just started** waits the same ten minutes before handing blobs it already had to followers that lack them. Followers that held copies before the restart say so in that time, and nothing is copied twice. Blobs pinned after the start are handed out at once.
- **Workers can fetch from a follower** what it holds for the pool, as they fetch from each other.

#### How many followers

`--replicas N` asks for N copies besides the coordinator's own. The data then survives the loss of any N nodes.

| To survive losing | Run | Each follower holds |
| --- | --- | --- |
| Any one node | `--replicas 1` and one follower, or more | All of it, with one; half, with two |
| Any one node, with no wait for copies to be made again | `--replicas 2` and three followers | Two thirds |
| Any two nodes | `--replicas 2` and two followers, or more | All of it, with two |

- With F followers, each holds about N/F of what is pinned. Give each room for N/(F-1) of it, which is what it holds once another has been lost.
- With fewer followers than N, each blob is held by all there are, and `blob replicas` says how many are missing.
- **Adding a follower:** start it. It is given its share within a minute and fetches it; the others delete what it took over once it has it.
- **Removing one:** stop it. After ten minutes the others are told to hold its share, for which they need the room. `pool remove` then stops it coming back, and its directory can be deleted.
- A follower has no limit of its own on the disk it uses. The coordinator's `--max-store-bytes` bounds what there is to copy.

#### When the coordinator's store is lost

Start the coordinator again with the same key. It takes back what its followers hold, by itself.

```sh
bin/sisyphusd run --name alpha --replicas 2    # the same data directory, with node.key in it and a new, empty blobs/
bin/sisyphusd blob replicas                    # says what has come back, and what has yet to
bin/sisyphusd blob pins                        # what came back is held for "restored"
```

- **The coordinator signs what it tells each follower to hold.** Each answer to a follower is a list: the blobs, until when each is kept, the name of the store, and a number that only goes up, signed with the node's key. The follower keeps the last list in `kept.list` in its `--replica-dir`, beside the copies.
- **A store has a name**, kept beside its pins. A coordinator that comes back with its key and an empty store, because the disk was replaced, has a new name. A follower that holds copies for the old one is not told to delete them. It is asked for its list.
- **The coordinator checks its own signature**, and takes the follower's word for nothing. If the list is its own, it fetches each blob named from that follower, checks it against its CID, and pins it. A list that was changed, or that another key signed, is ignored and logged, and nothing is taken back from it.
- **What comes back is pinned for `restored`**, until the time the list gave: the latest of the pins the lost store had on the blob. What had no expiry has none. What has since run out is not taken back, and the follower deletes it. Who had pinned each blob is not in the list. `blob unpin` releases a restored blob as it does one pinned by hand.
- **Several followers each show a list.** Each is taken. A blob that two lists name is kept until the later of the two times.
- **It starts when the follower next asks**, within a minute of the follower finding the coordinator again, and is done when everything named has been fetched. What cannot be fetched yet, because the follower has not connected as a worker or a fetch failed, the coordinator tries again by itself: after a second, then twice as long each time, up to each minute, without waiting for the follower to show its list again. It keeps that list in memory only, so after a restart it waits for the follower to show it on reconnecting. `blob replicas` says how many blobs have come back and how many are still to come.
- **A blob that is asked for** before then is fetched back from a follower there and then, as it always was.
- To give the old data up instead, stop each follower and empty its `--replica-dir`.

##### When `blob restore` is needed

`sisyphusd blob restore` fetches back everything the followers hold from an earlier store, on the followers' word, and pins it for `user` until unpinned. With the key, it is not needed. It is for what the coordinator cannot check:

- **The key was lost too.** A new key makes a new node, and a list signed with the old key cannot be told from a forgery. Start the new coordinator with `--formerly <the old node ID>`, so that it keeps what followers hold for the old node instead of having them delete it. On each follower's machine run `sisyphusd pool join --addr <coordinator> <invitation>` with an invitation from the new coordinator, and start the follower again with the same `--replica-dir`. Then run `blob restore`. The old ID is in `pins.json` in each follower's `--replica-dir`: every pin there is owned by `pool:<the old node ID>/<the old store's name>`.
- **A follower has no list**: it last heard from a coordinator older than signed lists, or its `kept.list` is gone or damaged.
- **A follower holds blobs its list does not name.** While a follower is still holding for an earlier store, because something of it has yet to come back, it is given no new list, and what it is handed in that time is on none. If the store is lost again before that is over, those blobs are kept on the follower and not taken back.

`blob replicas` says which case applies: blobs "that it has yet to take back" need nothing, and blobs "that no list signed by this node names" need `blob restore`. Run when there is nothing of the kind, it says so and does nothing.

##### What a follower can still do

- **It cannot add to what is kept.** Only what the coordinator pinned is ever in a list it signed. Blobs from an earlier store that no signed list names are never in one.
- **It can show an old list.** A follower that kept a list from before a pin was released can show it to a coordinator that has lost its store, and the blob comes back, until the expiry that list gave it, or until unpinned if it had none. That brings back only what the coordinator once chose to keep, and it needs the coordinator to have lost its store: a list of the store it has now brings nothing back.
- **A blob taken back and then released stays released** while the coordinator runs. A coordinator that is started again while a follower still shows a list of the lost store takes it back once more.

#### What this does not protect

- **The coordinator's database and key.** `node.db` holds the jobs, the members and the invitations, and `node.key` is who the node is: followers know their coordinator by it, and it is what makes a list the coordinator's own. Neither is copied anywhere. Back them up. Without the key, getting the data back takes `--formerly`, a new invitation for every follower, and `blob restore`, and the expiries are lost.
- **Who pinned what.** Followers hold blobs and, in the list, until when each is kept. A blob a job was holding while it ran comes back with no expiry, and stays until `blob unpin`.
- **What is not pinned**, and what was pinned in the last minute or so, before a follower had asked and fetched it.
- **A follower is asked to show what it holds.** Each time it says what it holds, about once a minute, the coordinator picks up to four of those blobs and asks for the digest of a random stretch of each, up to 64 KiB, under a number made up for the occasion. Only something with those bytes to hand can answer, and it has thirty seconds. A copy that is not shown is not counted, so another follower is told to hold the blob, and the follower that failed is asked about it first the next time. `blob replicas` has a SHOWN column, and says how often a follower has failed. Blobs never shown come before those shown, and those shown longest ago before the rest, so every copy is asked about in turn.
- **What that does and does not show.** That the follower could produce those bytes of that blob in that time. Not that it holds the rest, though it cannot know which bytes will be asked; not that it will hold them tomorrow; not that it would hand them over; not that two followers are two disks. A follower that keeps nothing and fetches a blob from someone else when asked passes if it is quick. And a follower from before this check is asked nothing and is still taken at its word. It is a check against a disk that lost data and a follower that lies cheaply, not a proof of storage in the sense a paid network needs: see [the research note](../../docs/research/storage-and-assets.md).
- **A follower can read what it holds**, as any worker can fetch any blob. Private jobs seal their data, and the copies are of the sealed form.
- There is one number of copies for the whole pool, not one per pin, and copies are whole: no erasure coding.
- The intervals are fixed: a minute between asks, ten minutes before a follower is given up on.
- The list of what to hold travels whole each time, which bounds a pool at about sixty thousand pinned blobs. A follower that shows its list sends it along with what it holds, so a follower that holds more than about thirty thousand blobs of a lost store cannot show its list: it keeps what it has, and `blob restore` takes it back.
- A coordinator takes a list back while it answers the follower that showed it. That follower hears nothing new until it is done.

### Pinning services

The [IPFS Pinning Service API](https://ipfs.github.io/pinning-services-api-spec/) is how one machine asks another, over HTTP, to keep data by its CID. A node speaks it both ways: it can be the service, and it can ask one.

**A node as the service.** A coordinator started with `--pinning-listen` serves the API, so that the `ipfs` program, or anything else that speaks it, can have the node keep data:

```sh
bin/sisyphusd run --pinning-listen 127.0.0.1:7710
ipfs pin remote service add pool http://127.0.0.1:7710 "$(cat "$(bin/sisyphusd data-dir)/pinning.token")"
ipfs pin remote add --service=pool --name=results.tar <cid>
ipfs pin remote ls --service=pool
ipfs pin remote rm --service=pool --cid=<cid>
```

- **It has a key of its own**, in `pinning.token` in the node's data directory, made the first time the service starts. It is not the node's API token. That one controls the node; this one is handed to other programs and other machines, and lets its holder do three things: ask the node to keep data, list the requests made through this API, and remove them. To change it, delete the file and restart the node.
- **What the node holds is pinned at once.** The pin is held for `pinning-service:<request ID>` and shows in `blob pins`. It lasts until the request is removed, and then garbage collection takes the data if nothing else keeps it. Two requests for the same data are two pins.
- **What the node does not hold, it can fetch only with `--kubo`.** Its Kubo connects to the `origins` the request names and fetches the data, and the request goes from `queued` through `pinning` to `pinned`, or to `failed` if a block cannot be found within thirty seconds. Four requests are fetched at a time. Fetches left unfinished when the node stops are taken up when it starts again. Without `--kubo` the node has nothing to fetch with, and such a request is `failed` at once, with the reason in its `info`.
- **A node's Kubo fetches only from the pool's private network.** It can connect to a peer only if that peer holds the pool's swarm key. An `ipfs` daemon on the public IPFS network can ask this service to pin something, and will be answered, but cannot supply the data: the request fails unless the node already holds it. A daemon that is to supply data must be given the pool's `swarm.key`, which also lets it read everything on the pool's network that is not sealed.
- **It keeps files and directories**, which are raw and dag-pb blocks. A request for any other kind of IPLD data is refused.
- **Listing** takes the API's filters: `cid`, `name` with `match`, `status`, `before`, `after`, `limit` and `meta`. Replacing a request, with `POST /pins/<request ID>`, keeps the old data pinned until the new data is here.
- **The only limit is the store's.** With `--max-store-bytes`, a request that needs fetching is refused once the store is that full. The check is made before a fetch, not during it, so one fetch can take the store past the limit. There is one key, and no accounting of who asked for what.
- **It is plain HTTP.** The key crosses the network unencrypted. Listen on this machine only or on a network you trust, or put a server that does TLS in front of it.
- `ipfs pin remote add` says only that a failed request failed. The reason is in the request's `info`, which `blob pins-remote` shows.

**A node asking a service.** `blob pin-remote` asks a pinning service to keep a blob as well: another node, an IPFS Cluster of your own, or a commercial service.

```sh
bin/sisyphusd blob pin-remote --service https://pins.example.com/psa --key-file service.key --wait <cid> results.tar
bin/sisyphusd blob pins-remote --service https://pins.example.com/psa --key-file service.key
bin/sisyphusd blob unpin-remote --service https://pins.example.com/psa --key-file service.key <request ID>
```

- `pin-remote` prints the request's ID and its status. With `--wait` it asks every two seconds until the service says `pinned` or `failed`, and prints that. A failed request ends the command with an error and the service's reason.
- **What the service is told:** the blob's CID; the name, if you give one; and where the pool's Kubo daemons can be reached, as the request's `origins`, so that the service knows where to fetch from. Those are the IP addresses, ports and node IDs of the coordinator's Kubo, if it was started with `--swarm-port`, and of the Kubo daemons connected to it. Like any server, it also sees the address the request came from. `--no-origins` leaves the addresses out. A node without `--kubo` has none to give.
- **The data is not uploaded.** The service has to fetch it, and a service on the public IPFS network cannot fetch from a pool: the pool's Kubo daemons answer only holders of its swarm key. Such a service will take the request and then fail it, or leave it waiting. For it to succeed, either the data has to be public in some other way, such as added to a Kubo on the public network, or the service has to be on the pool's network.
- **A service on the pool's network** is one whose IPFS daemon holds the pool's `swarm.key` and can reach the pool's Kubo daemons. Another coordinator becomes one if a copy of the key is put in its data directory as `swarm.key` before it is started with `--kubo --pinning-listen`; the asking coordinator needs `--swarm-port` so that its Kubo can be reached. The key is a secret: whoever holds it can read everything on the pool's network that is not sealed. `pool rekey` and `pool remove` change it, and a service outside the pool then has to be given the new one by hand.
- The request is sent from the machine the command runs on. The key is read from a file so that it appears in no command line, and it is not given to the node.
- `pins-remote` lists every request the service has for the key, whoever made it.
- A sealed blob is kept as it is stored, sealed. The service never has its key.
- Tried between two nodes, and with the `ipfs` program as the client of a node. Not tried against a commercial service or IPFS Cluster.

## What each machine has

A worker looks at the machine it runs on and tells its coordinator what it finds: the processor, how much memory, and any NVIDIA graphics cards. `sisyphusd nodes` shows it.

```sh
bin/sisyphusd run --offer-memory-mb 16384 --offer-gpus 1 ...     # offer the pool less than the machine has
bin/sisyphusd job submit --min-memory-mb 8192 --gpus 1 ...       # a job that needs that much
```

- **A job can ask for memory and graphics cards**, and its tasks then go only to workers that have them. Asked to split itself, it splits to fit the workers that qualify. If none does, it waits until one connects.
- **The owner decides what to offer.** `--offer-memory-mb` and `--offer-gpus` cap what the node says it has. They do not hold a workload to it: nothing yet stops a task using more memory than its node offered.
- **What is found, and where.** Processor and memory are read from `/proc` on Linux, asked of `sysctl` on macOS and of PowerShell on Windows. Where none of those answers they are left unknown, and such a worker is given no job that asks for memory unless its owner says how much to offer. Graphics cards are found wherever NVIDIA's `nvidia-smi` is installed; other makes are not looked for.
- **A requirement is a count, not a choice.** A task that asks for a graphics card is sent to a machine with one. Which card it then uses, and keeping two tasks off the same one, is the workload's business for now.

## The planner

A node that coordinates a pool can be asked questions in plain words. A language model decides what needs computing, has the pool compute it, reads the result, and answers.

```sh
bin/sisyphusd model set --model qwen3:8b                         # a model served by Ollama on this machine
bin/sisyphusd model set --provider anthropic --model claude-opus-5-5 --api-key-file key.txt   # or Claude
bin/sisyphusd run --api-listen 127.0.0.1:50051                   # the node, with its local API on
bin/sisyphusd ask "How many primes are there below ten million?"
bin/sisyphusd ask --chat <id> "And below a hundred million?"     # carry the conversation on
```

- **Which model.** `model set` takes `--provider ollama` (the default), `--provider anthropic` for Claude, or `--provider openai`, which is for OpenAI and for anything that speaks as it does: most hosted services and local servers. `--url` says where the service is if not in its usual place, and `--api-key-file` gives a key if it wants one. `model list` asks the service what it offers, and marks any model that cannot call tools and so cannot plan; `model show` prints what is set, without the key; `model providers` lists the kinds of service. `model pull --model llama3.1:8b` fetches a model into Ollama and `model remove` deletes one, each taking `--url` for an Ollama somewhere else. A running node picks up a change at the next question.
- **Claude.** With `--provider anthropic` and no key file, the node uses the key Anthropic's own tools are set up with: `ANTHROPIC_API_KEY`, or a profile from `ant auth login`. Claude's replies are kept whole, with its reasoning, and handed back to it on the next question, as Anthropic asks; if the pool's workloads have changed since, so that Anthropic no longer accepts the reasoning, the question is asked again without it.
- **What the model can do.** Two things: run a job on the pool and read the result, and look up a job run earlier. It is told which workloads the pool has and what parameters each takes. It has no other reach: it cannot run commands, read files or touch the node's settings.
- **What you see.** `ask` prints the model's words as they come, each job it starts, and what each returned. The jobs are ordinary jobs: `job get` and `job logs` work on them.
- **Conversations are kept** in the node's database, with every request the model made and what came back, and are there for the desktop to list and read.
- **It needs the node's token**, like anything else that spends the pool's time. `ask` reads it from the data directory.

What to know before relying on it:

- **It is as good as the model.** A small model may ask for the wrong parameters, or answer without computing. The model must support tool calling; many small ones do not.
- **It has been tried with one real model**, `llama3.1:8b` from Ollama, which ran the right jobs and answered correctly over two turns. The tests use a stand-in. Anthropic's and OpenAI's services have not been tried for want of keys. Expect the instructions the model is given to need tuning as more models are tried.
- **The key is kept in the node's database**, which only its owner can read, not in the operating system's keychain.
- **One question at a time per conversation.** Two asked at once in the same conversation are recorded in whichever order they finish.
- **It gives up after eight rounds** of asking for more without answering.

## Jobs made of jobs

A `graph` job runs several jobs as one. Each step is a job of some workload; a step may use what an earlier one produced, and waits for it.

```sh
bin/sisyphusd job submit --workload graph --params '{"steps":[
  {"name":"count","workload":"wordcount","params":{"input":"<cid>"}},
  {"name":"again","workload":"wordcount","params":{"input":"${count.result.output}"}},
  {"name":"low","workload":"primes","params":{"from":0,"to":1000000},"tasks":2},
  {"name":"span","workload":"primes","params":{"from":0,"to":"${low.result.count}"},"after":["again"]}]}'
```

- **References.** In a step's parameters, `${name.result}` is an earlier step's result, `${name.result.field}` a field of it, `${name.outputs.0}` the first file it stored and `${name.job_id}` its job. A string that is nothing but one reference becomes the value itself, so a number stays a number.
- **Order.** A step waits for every step it refers to and any it names under `"after"`. Steps that wait for nothing run side by side. A graph in which a step waits, however indirectly, for itself is refused when submitted.
- **Each step is an ordinary job**, with `parent_job_id` and `step` saying what it is a step of. `job get` and `job logs` work on it, and its tasks go to workers as any job's do. The graph's own tasks are its steps, carried out by the coordinator; no worker needs to know of graphs.
- **Failure and stopping.** A step that fails, or whose job is cancelled, fails the graph, and steps not yet begun are not run. Cancelling the graph cancels the jobs its steps are running as.
- **What the steps stored is the graph's.** The files each step stored are among the graph's own `output_blobs`, and are kept for `--retain` after the graph finishes, as well as after the step does. A graph whose last step ends long after its first still has the first one's files to hand.
- **A restart** of the coordinator takes a graph up where it was: steps that had finished are not run again, and one that was running goes on as the job it was.
- **A private graph** gives its key to every step.
- Up to 64 steps. There are no loops or conditions: a step runs or the graph fails.

## Thinking on the pool

A pool can serve language models as well as run jobs. A worker whose owner runs Ollama offers its models; the coordinator offers all of them at one address, speaking OpenAI's dialect, so that whatever can talk to such a service can think on the pool.

```sh
bin/sisyphusd run --role worker --coordinator <addr> --join <invitation> --models-from http://127.0.0.1:11434   # a worker with models
bin/sisyphusd run --inference-listen 127.0.0.1:11435                                                            # the coordinator

curl -H "Authorization: Bearer $(cat "$(bin/sisyphusd data-dir)/api.token")" http://127.0.0.1:11435/v1/models
bin/sisyphusd model set --provider openai --url http://127.0.0.1:11435/v1 --model llama3.1:8b \
    --api-key-file "$(bin/sisyphusd data-dir)/api.token"      # the node's own planner, thinking on its pool
```

- **Each reply is a job** of the `chat` workload, given to a worker that serves the model asked for, and shows among the pool's jobs like any other. The local API's `ListWorkers`, and an agent's `pool_status`, show which models each worker serves.
- **The service** answers `GET /v1/models`, `POST /v1/chat/completions`, with tool calls, and `POST /v1/embeddings`, for up to 128 texts in a request, which are shared out among the workers serving the model.
- **Many prompts at once** is the `prompts` workload: `job submit --workload prompts --params '{"model":"llama3.1:8b","prompts":["...","..."],"system":"..."}'`, or `"input"` with the CID of a stored file that has a prompt on each line. The prompts are shared out among the workers serving the model and the answers come back in order. This is where several machines with models are faster than one. Its key is the node's API token, and it listens on this machine only.
- **What a worker's models are doing.** `sisyphusd nodes` adds a line for each worker that serves models, such as `laptop serves llama3.1:8b (loaded, 75% on a graphics card), qwen3:8b`. A model marked loaded is in its server's memory and answers at once; one that is not is loaded when first asked for, which can take a minute. The share is how much of it fits on the machine's graphics cards: at 100% the card does all the work, and below that the rest is done by the processor and replies come slower. A model that does not fit is a reason to serve a smaller one. The worker asks its Ollama for this when something it can see changes it, never on a timer: when it starts or reconnects, after every task that used a model (before that task's result goes back, so a model found missing is not asked for there again, when Ollama answers within a second; one that does not holds the result up no longer, is asked once more after it, and until it answers a task needing the missing model may be sent back to this worker), and when the node fetches or removes a model, changes what it lends, or `InspectOllama` is called. A model fetched or removed in Ollama directly, outside the node, is seen at the next of these. A server that is not an Ollama says nothing, and nothing is shown.
- **Which worker answers.** A reply that names a model without a worker goes to the worker with the fewest replies on that model in hand, since a model server answers about one at a time however many slots its worker has; among those, to the one with the model in memory and most of it on a graphics card; and then to the one with most slots free.
- **What the owner can limit.** `--slots` is how many tasks run at once, `--offer-memory-mb` and `--offer-gpus` are how much of the machine the pool is told about, and leaving out `--models-from` serves no models at all. With `--models-from`:
  - `--models-only` offers only the models it names (`--models-only llama3.1,qwen3:8b,gemma*`: a family, an exact model, a pattern), so that a large model kept on the machine for its owner's own use is not handed to the pool. A worker refuses a model it does not offer even if a job names it.
  - `--models-gpu-layers` is how many of a model's layers go on graphics cards for the pool's jobs: `0` keeps the pool's work off the card altogether, a number below the model's layer count shares the model between card and processor, and the default lets Ollama put on as many as fit.
  - `--models-threads` is how many of the processor's threads a model may use, and `--models-context` how many tokens of a conversation it holds (more takes more memory).
  - `--models-keep-alive` is how long a model stays loaded after it last answered for the pool: `0` frees its memory at once, `30m` keeps it ready.
  - These are put on every request the worker makes of its Ollama, over whatever a job asked. The worker speaks to Ollama in Ollama's own dialect for this, since its OpenAI-style service ignores such settings; jobs are still written and answered in OpenAI's.
  - The same settings can be saved in the node instead of given as flags, through the local API's `SetModelLending` (which the desktop app's Settings uses, #238). A node started without `--models-from` lends what is saved; one started with it lends what its flags say and ignores what is saved. While lending is active, changing the model allowlist or request limits applies live; enabling or disabling lending, or changing Ollama's address, takes effect after a node restart. `GetModelLending` says whether a restart is still needed. `InspectOllama` asks an address whether an Ollama answers there, its version, which of its models would be lent and which are loaded.
  - `--offer-gpus 0` hides the card from jobs that ask for one; on its own it does not stop Ollama using the card for the models served. `--models-gpu-layers 0` does.
- **What only Ollama itself can be told.** Some limits belong to the Ollama server as a whole, not to a request, and are set where Ollama runs, as environment variables, followed by a restart of Ollama: `OLLAMA_MAX_LOADED_MODELS` (how many models it holds in memory at once), `OLLAMA_NUM_PARALLEL` (how many requests one model answers at once), `OLLAMA_GPU_OVERHEAD` (memory on each card it leaves alone), `OLLAMA_KEEP_ALIVE` (the keep-alive for requests that do not give one, its own owner's included), `CUDA_VISIBLE_DEVICES` or `ROCR_VISIBLE_DEVICES` (which cards it may see at all), and `OLLAMA_HOST` (where it listens). On Windows they are set in the system's environment variables and Ollama is quit from the tray and started again; on Linux, with `systemctl edit ollama`; on macOS, with `launchctl setenv`. See Ollama's FAQ for each.
- **`--models-from` is a decision about your machine.** Whoever may submit jobs to the pool can then use your models and the power they draw, and your machine sees every conversation it is given: there is no sealing of a conversation from the machine that has to read it.
- **Keep a conversation on a machine you trust** by asking for the model on a worker by name: `"model": "llama3.1:8b@rig"`. It goes to the worker named `rig` or, if that worker is not there, is refused; it never goes elsewhere. A name is whatever a worker calls itself, so in a pool with workers you do not run, write the worker's node ID in place of its name, `"model": "llama3.1:8b@12D3KooW…"`, as `sisyphusd nodes` shows it: the coordinator knows that from the key the worker connected with. Asking for a model on your own machine's worker keeps the conversation at home while still going through the pool.
- **A reply arrives as it is written.** A client that asks for a stream gets the words a few times a second, and at the end the tools the model asks for and what it used. The worker logs the reply in pieces as it comes, so `job logs` shows it too.
- **A model may be named loosely** at the service: in another case, without its tag when the pool has a `latest`, or by its family alone (`llama3.1`) when the pool serves one model of that family. A name that could mean two models is refused, with the list of those served. A `chat` job submitted directly names its model exactly.
- **A model fetched later is offered within seconds.** A worker tells the coordinator when its models change, and a conversation that was waiting for one is then had.
- **Conversations are kept** with the pool's other jobs, parameters and result, for as long as jobs are kept (`--keep-jobs`).
- **From another machine**, the models are reached through that machine's own membership of the pool, not by opening the service to the network. Join as a client (`pool join` with a client invitation), then `sisyphusd inference --addr <coordinator>` offers the pool's models at `http://127.0.0.1:11435/v1` to programs on that machine, with the key in `api.token` in its data directory. Requests travel as jobs over the connection the pool already authenticates. A machine that is in the pool only as a worker cannot use it this way: workers run the pool's work and do not submit any. A worker that has also redeemed a client invitation can.
- **A worker lost part way through a streamed reply** ends the stream with an error saying so, and the job is stopped. Another worker would begin the reply again, and a reply whose beginning came twice would look right and be wrong. Ask again. A reply not asked for as a stream is simply made again.
- **A call a model writes out is still made.** A small model sometimes writes the tool call it means as text, `{"name": "run_job", "parameters": {...}}`, often with Python's `True`, `False` and `None`, rather than making it. When the whole of a reply is such an object and names one of the planner's tools, the planner takes it as the call. Text that opens the way such a call does is held back until it is known which it is, so the call is not shown as a line of JSON first; a reply that says anything besides is an answer, and is shown in full once that is plain, or once it has run past a few thousand characters. With `llama3.1:8b` through Ollama's OpenAI-style endpoint, three replies in six came this way and all three ran their job.
- **True, false and numbers in quotes are read as meant.** A small model also writes `"private": "false"` or `"tasks": "100"`. The planner takes a quoted `true` or `false` (in any case, with spaces around it) and a quoted whole number as if they had no quotes, rather than refusing the call and leaving the model to try again. Anything else in those places is still refused, and the model is told what was wrong.
- It has been tried on one machine with `llama3.1:8b` on a processor: the planner, set as above, ran the right job and answered correctly, and a streamed reply arrived a word or two at a time. It has not been tried on a graphics card, or with workers on other machines.

## Agents

The planner is the node's own agent. Any other agent that speaks the Model Context Protocol, such as Claude Code, Claude Desktop or Cursor, can use the pool too, through `sisyphusd mcp`:

```sh
bin/sisyphusd run --api-listen 127.0.0.1:50051        # the node, with its local API on
claude mcp add sisyphus -- /path/to/sisyphusd mcp     # Claude Code; other agents take the same command in their own settings
```

```json
{ "mcpServers": { "sisyphus": { "command": "/path/to/sisyphusd", "args": ["mcp"] } } }
```

The second form is what Claude Desktop (`claude_desktop_config.json`), Cursor (`~/.cursor/mcp.json`) and most others take. The agent starts `sisyphusd mcp` itself and talks to it over standard input and output. It takes `--data-dir`, `--api` and `--addr` if the node's data directory, `--api-listen` and `--listen` are not the usual ones.

There are 44 tools: 19 that only look, 14 more that use the pool, and 11 more with `--admin`.

| Tool | What it does |
| --- | --- |
| `pool_status` | The workers connected, with their cores, memory, graphics cards, the models they serve and how busy they are. |
| `list_workloads` | What the pool can run, the parameters each workload takes, and how many connected workers run it. |
| `run_job` | Runs a job and waits for its result, or returns at once with `detach`. It takes what `job submit` takes: `tasks`, `mode`, `private`, `min_memory_mb`, `min_gpus`, `task_timeout_seconds`, `verify` and `verify_share` to have [workers check each other](#verifying-results), and `audit_share` to have [the node check them itself](#checked-by-the-coordinator-itself). |
| `get_job`, `list_jobs`, `cancel_job`, `job_logs`, `wait_for_job` | Look at a job, list them, stop one, read what it logged, wait for one to finish. |
| `store_file`, `fetch_file`, `list_files`, `remove_file`, `fetch_outputs`, `save_result` | Put a file from this machine in the pool's store, bring one back, list them, stop keeping one, bring back everything a job stored, write a job's whole result to a file. |
| `list_pins`, `pin_file`, `unpin_file` | What the node keeps, for whom and until when ([How long data is kept](#how-long-data-is-kept)); keep a file, such as a job's output, for good or for so many hours; release it. |
| `storage_status` | Which other nodes hold copies of what is pinned, against how many should: the storage followers of [Copies on other nodes](#copies-on-other-nodes), or the members of the pool's [IPFS Cluster](#ipfs-cluster-pins-held-by-several-nodes), whichever the pool uses, and it says so plainly of a pool that uses neither. |
| `get_job_record` | A finished job's [record](#a-jobs-record): its ID and its nodes as JSON. With `verify` it is checked against the job as `job record --verify` checks it, and with `check_commitments` a private job's commitments are checked with the node's own `private.key`. |
| `resolve_name`, `publish_name` | What a node's [name](#names-for-what-changes) stands for, checked against the name as `name resolve` checks it; point this node's name at a file. |
| `ask_model`, `compare_texts` | Has a model served by one of the pool's workers answer a prompt; says which texts mean most alike, by an embedding model the pool serves. |
| `ask_planner`, `list_chats`, `get_chat`, `delete_chat` | Hand a whole question to the node's own planner, with `files` the content IDs of stored files it is about, which the node then keeps for as long as the conversation; read or forget its conversations. |
| `list_peers`, `list_members`, `list_address_book` | The nodes this node knows of, with their countries and which way work flows; who is in its pool; which nodes it connects to each time it starts. |
| `get_model`, `list_models`, `list_model_providers` | What the planner plans with, what its service offers, and the kinds of service there are. |
| With `--admin`: `create_invitation`, `remove_member`, `join_pool`, `connect_peer`, `forget_peer`, `set_peer_trust`, `set_model`, `pull_model`, `remove_model`, `collect_garbage`, `restore_files` | Change the node itself: who is in its pool, which nodes it trusts and connects to, what it plans with; clear its store of what nothing pins now, and take back from its followers what a lost store held (`blob gc`, `blob restore`). |

Besides tools, the server gives an agent things to read (`sisyphus://pool`, `sisyphus://workloads`, and the skill itself as `sisyphus://skill/SKILL.md` and `sisyphus://skill/recipes.md`), three prompts (`run-on-pool`, `pool-report`, `keep-result`), and, while `run_job` or `wait_for_job` waits, word of each task as it finishes.

What the agent may do is yours to set, when you give its settings the command:

| Flag | What it does |
| --- | --- |
| `--files-under <dir>` | The only directory `store_file` may read from and `fetch_file` write to. It is the directory the server is started in unless you say otherwise; `/` is anywhere you may. A link inside it that leads outside is outside. |
| `--read-only` | Only the tools that look. Nothing is run, stored, written or changed. |
| `--images a,b` | The only container images `run_job` may run, in a `container` job or in any step of a `graph`. An image a step would take from an earlier step's result is not a listed one. `ask_planner` is refused while images are listed, since the planner chooses what to run itself. Any, if not given. |
| `--admin` | Adds the tools that change the node itself. Without it there are none. |

- **It is a client of the local API** and holds the node's token, which it reads from the data directory.
- **Pins, copies, names and records are not in the local API**, so for those tools it reaches the node's main address, `--addr`, as the command line does: with the node's key in `--data-dir`. That connection is made the first time one of them is called. If it cannot be made, or the node is not at `--addr`, those tools fail saying why and every other tool goes on working. The token is not sent there.
- **`publish_name` signs with the node's key**, as `name publish` does, and `get_job_record` with `check_commitments` sends the node its own `private.key`, as `job record --key-file` would. It reads that key and never makes one, and no tool hands either key to the agent.
- **Pinning services are not offered.** `blob pin-remote` and its fellows need another service's key, and an agent is given no key to pass on.
- **With no flags it can use the pool and not reconfigure it.** It can spend the pool's time, run container images on workers that allow containers, and read and write files in the directory it was started in. It will not write over a file that is there unless told to in so many words.
- **`--admin` is the node's keys.** An agent with it can invite anyone into the pool and join the node to others. It is never given a model service's key: `set_model` keeps the one already set.
- **A skill comes with it**, carried in the daemon. `sisyphusd skill install` puts it where Claude Code keeps skills (`~/.claude/skills`), `--dir` puts it elsewhere, and `sisyphusd skill show` prints it. It tells an agent how to use a pool well: look before submitting, how to split work with a container, how to have the pool's models think, what goes wrong; `recipes.md` beside it has worked shapes for common jobs. It works with the tools or, without them, with the command line. Its source is [`skills/sisyphus`](../../skills/sisyphus/SKILL.md).
- **It has been tried from Claude Code**, with the skill installed and the server kept to one directory: asked to describe the pool, upper-case a file across three tasks and have one of the pool's models answer a question, it read the skill, ran a `container` job and `ask_model`, and wrote the right file. A second run, on Opus, labelled eight reviews with one `prompts` job and found the two most alike with `embed`; what it stumbled on (a result of vectors too long to hand back) is why `save_result` and `compare_texts` exist. A third, with the tools for what the node keeps and no skill, counted a file's words, pinned the result for a day, verified the job's record, found from `storage_status` that no other node held a copy, and published and resolved the node's name. It had to find out from `list_pins` that the job's own pin outlasts a day's pin, which is why `pin_file` now answers with every pin on the file. A fourth, with `--admin` and the skill, against three workers: it made an invitation and saved it, saw the worker it admitted join, counted primes with half the tasks verified and said which task had been run once and so how far the count could be trusted, counted a private file's words with every task verified, checked that job's record and its eighteen commitments and said what the record hides, published the other job's record under the node's name, found that one disk held everything, and removed its input and collected garbage, explaining that nothing was freed while the job's own pin stood. Nothing failed. It read `list_members` leaving out the node itself as something to remark on, which the tool now says, and it said `pin_file` was not among the tools it had loaded, though the server offers it; that looks like Claude Code holding some of a long list of tools back until they are searched for, and has not been looked into. Other agents have not been tried.

## Following and stopping a job

```sh
bin/sisyphusd job logs <job-id>                 # what has happened to it, and what happens next, until it is over
bin/sisyphusd job cancel <job-id>               # stop it
bin/sisyphusd job submit --timeout 10m ...      # stop and retry any attempt at a task that runs longer
```

- **Events.** A coordinator records what happens to each job: submitted, each task started, succeeded, failed, lost with its worker or timed out, and how the job ended. A [verified](#verifying-results) job also has each result a worker returned, each time the results for a task differed, and each task that was verified after all because of a [spot check](#spot-checks). `job logs` prints them and follows a running job to its end. They are kept in the database with the job.
- **Logs.** A task can log lines as it runs, and those appear among the job's events with the task and worker they came from. The two built-in workloads log one line each.
- **Progress.** A task can say how far along it is. `job get` and the desktop's API give it for each task, from 0 to 1, and for the job as the mean of its tasks. It is as true as the workload makes it, and is not kept across a restart.
- **Cancelling** stops the job at once: its running tasks are told to stop, their slots are free again, and nothing more of it is handed out. A cancelled job is over, like one that succeeded or failed, and stays on record.
- **Timeouts.** With `--timeout`, an attempt at a task that runs longer is stopped and counts as a failure, so it is tried again up to the usual three times. A coordinator started with `--max-task-timeout` holds its clients' jobs to that: a job may ask for less, never more, and one that names no timeout gets the coordinator's, so no client's task holds a worker's slot for ever. `job get` shows the timeout a job was given. The coordinator's own jobs keep what they ask for.
- **A job that fails stops its other tasks** the same way, rather than leaving them to finish for nothing.

## Verifying results

A coordinator takes what a worker returns as it is, unless the job asks otherwise. A job submitted with `--verify N` has each of its tasks run by `N` different workers, and takes a result only once `N` of them have returned the same one.

```sh
bin/sisyphusd job submit --verify 2 --params '{"from":0,"to":100000000}'   # each task on two workers, which must agree
bin/sisyphusd job logs <job-id>                                            # who returned what, and who disagreed
```

- **What "the same" means.** Two results are the same if the outputs are the same bytes and the tasks stored the same blobs, by CID. Stored data is named by its content, so two workers that wrote the same file named the same CID. A workload whose honest results agree only to some precision can say itself what is the same; see [within a tolerance](#within-a-workloads-tolerance).
- **Different workers.** A task goes to `N` workers at once, each a different node. A worker that has returned a result for a task, or is running it, is never given that task again, so the results that settle a task come from `N` different nodes.
- **When they agree**, which is the usual case, the task has succeeded with that result. The task is shown as the first of those workers'.
- **When they differ**, the task goes to one more worker that has not been asked, for each result the larger side still lacks, until some result has been returned by `N` workers. With `--verify 2`, two that differ are settled by a third. A task takes in at most `2N-1` results. If no result can reach `N` within that, or no connected worker is left that has not been asked, the task fails and the job with it. The error names each worker and a short digest of what it returned.
- **Only for work that gives the same result every time, or that compares its results itself.** A model's reply (`chat`, `prompts`, `embed`) and a container's output can differ between two honest runs, so comparing them would only fail the task after paying for every copy. A job of such a workload that asks for `--verify` or `--audit-share` is refused at submission, and says why (`coordinator.go`, `submit`; the workloads mark themselves with `runtime.Varying`), unless the workload also compares its results within a tolerance of its own (below). How to check such work is #300.
- **Each disagreement is an event.** `job logs` shows `task-result` for every result with the worker that returned it, `task-disagreed` with the workers on each side when the results in for a task differ, and `task-succeeded` with the workers that agreed and any that returned something else. The job's [record](#a-jobs-record) has the same for good: each receipt lists every result by worker, with a digest and whether it was one of those that settled the task.
- **A failed attempt is not a disagreement.** An attempt that fails, times out (`--timeout`) or is lost with its worker returned no result. It is tried again, by another worker if one could run it and otherwise by that one, and counts towards the usual three failures a task may have.
- **A job that asks for more workers than there are is refused** when it is submitted: `--verify 3` needs three connected workers that could take the job, counting those that are busy. If workers leave afterwards, a task that still needs one waits for it, as a task of any job waits for a worker; the coordinator cannot tell a worker that has gone for good from one that is restarting. Cancel the job if nobody is coming.
- **With no `--tasks`**, a job is split into one task for every `N` free slots, since each task takes a slot on `N` workers. `--mode full-worker` runs the whole job as one task on `N` workers.
- **A restart loses nothing.** The results in are kept in the database. A coordinator that is restarted asks only the workers that had not answered.
- **Logs and progress.** Every copy of a task logs its own lines, each shown with the worker it came from. A task's progress counts the results it has and the furthest of its running copies.
- **A graph hands it on.** A job made of jobs that is verified has each of its steps verified, by as many workers, and with the same share of each step's tasks if it asked for [spot checks](#spot-checks). The graph itself is taken in whoever is connected; a step that cannot be verified when it begins fails the graph and says why.

**What it costs.** `N` times the work, and more when workers differ. A task is as slow as the slowest of the workers it needs.

### Within a workload's tolerance

Science does not always give the same bits twice. A calculation in floating point can come out differently on different processors, in the last few decimals, and two honest workers then return different bytes. A workload like that can compare two outputs of one task itself, by implementing `runtime.Comparing`:

```go
Compare(a, b []byte) (same bool, why string)
```

- **What it changes.** Two results of such a workload are the same if they stored the same blobs, by CID, and its `Compare` says their outputs are; outputs whose bytes are equal are always the same. That is how `--verify` compares the workers' results and how `--audit-share` compares the node's own with the worker's. Everything else is as above: how many must agree, who is outvoted, standing, probation and the record.
- **A workload that varies is checked only if it compares.** One that says its results vary (`runtime.Varying`) and does not compare them is still refused `--verify` and `--audit-share`, with the same error. One that does both is accepted.
- **Which results settle a task.** A tolerance is not transitive: `a` can be within it of `b`, and `b` of `c`, with `a` not within it of `c`. So the results do not fall into sides of their own accord, and the coordinator draws them: a side is one result, its anchor, and every other result that is the same as it. The side that counts is the one around the result the most others are the same as, and of sides as large the one drawn around the result heard from first. A task succeeds once that side has `N` results, and its result is the anchor's, which is within the tolerance of every result that settled it. The results left over are drawn into sides the same way, for the events. For results compared byte by byte this is the same as before: the largest set of identical results, the first heard from of those as large.
- **Why around one result.** The result taken is within the tolerance of each of the `N` results that settled it, its own among them: it is vouched for by `N` workers each of whom got something within the tolerance of it, which is what `N` identical results say for work compared byte by byte. Results that settled a task together may be up to twice the tolerance apart from each other, and the tolerance should be set knowing that. Asking that every pair agree instead would let one honest result at the edge of the spread fail a task the others settle, and taking the first result heard from as the anchor would let whoever answers first decide which side counts.
- **What differed is told.** Where `Compare` says two results differ it says why, and that is in the events beside the worker: `task-disagreed` gives it for each side that differs from the leading one, and `task-succeeded` for each worker outvoted, such as `liar returned another (energy differs by 3.2e-4 Eh, more than 1e-8)`. An audit that catches a worker says it too. The same is in the coordinator's log and, for a task that cannot be settled, in the job's error.
- **The tolerance is the workload's responsibility.** It is all that stands between a verified result and a made-up one. It must be loose enough for honest results from any processor to agree and tight enough that a number that was not computed, or was computed wrongly, falls outside it. Set it from the spread measured across machines, with margin, and no wider.
- **In the record**, each result still has its own digest, and results with different digests can both be marked `agreed`: being one of those that settled the task is what `agreed` says.

### Spot checks

Verifying every task multiplies the work. With `--verify-share`, a job has only a share of its tasks verified and the rest run once:

```sh
bin/sisyphusd job submit --verify 2 --verify-share 0.25 --tasks 16 --params '{"from":0,"to":100000000}'   # four tasks on two workers each, twelve on one
```

- **Which tasks is not known beforehand.** That share of the tasks, rounded up to a whole task so that at least one always is, is drawn at random when the job is submitted. A task that is verified and one that is not reach a worker looking the same.
- **A worker that is caught is no longer taken at its word.** When a verified task is settled against what a worker returned, every task of the job that worker ran unverified, or is running, is verified after all: what it returned stands as one result, and the task goes to as many more workers as it now lacks. A task that had succeeded on that worker's word is no longer succeeded, and the job does not finish until it is settled. Whatever of the job that worker is handed afterwards is verified too. `job logs` shows each such task as `task-rechecked`.
- **`job get` and the job's record say which tasks were verified.** `job get` marks each task `verified by N workers` or `not verified`, and each receipt in the record has `verify`: how many workers the task was held to, `1` for one that was run once.
- **What it costs** is about `1 + share × (N - 1)` times the work while nobody is caught: a quarter more for `--verify 2 --verify-share 0.25`.

What a spot check does not do:

- **It does not catch every wrong result.** A worker that returns a wrong result for one task in a hundred is caught only if that task is one of those verified. A share of `s` catches a single wrong result with probability `s`, and a worker that is wrong on `k` of the tasks it ran with probability `1 - (1 - s)^k`, less whatever it gets right. What is not caught is in the job's result.
- **It does not shut a worker out.** A worker that is caught is still given tasks, in this job and the next. What is carried over to the next job is its [standing](#a-workers-standing): a count of its results, and a probation during which a job with spot checks verifies whatever it is handed.
- **A member that is both a worker and a client of the pool can tell.** Whoever may read a job can see which of its tasks went to two workers. Workers that are only workers cannot read jobs.
- **Workers that agree to lie** are no more caught by a spot check than by verifying everything.

### A worker's standing

What verified tasks show of a worker outlasts the job. The coordinator keeps three numbers for each worker, by its node ID:

- **Agreed and outvoted.** When a task that `N` workers had to agree on succeeds, each worker whose result was one of those `N` has one more result that agreed, and each worker that returned something else has one more that was outvoted. A task is counted once, when it is settled.
- **What is not counted.** A task that fails because its workers could not agree counts for nobody: nothing says which of them was right. An attempt that fails, times out or is lost returned no result. A task that was run once and taken as it was shows nothing, unless it is verified after all and settled, and then it counts like any other. A job that is not verified counts for nothing.
- **Probation.** A worker that is outvoted is on probation until ten results it returns after that have agreed. Being outvoted again starts the ten over.
- **What probation does.** In a job with `--verify-share`, a worker on probation is treated as one already caught in that job: a task handed to it is verified, even if it was one of those to be run once, and goes to as many more workers as that takes. `job logs` shows it as `task-rechecked`, saying that the worker was outvoted in another job and how many results it still owes.
- **What probation does not touch.** A job that verifies every task treats the worker as it treats any other. A job without `--verify` still takes its word: it asked for nothing to be checked, and probation does not change that. Only results for verified tasks work a probation off, so in a pool that runs no verified jobs it stays.
- **Where it shows.** `sisyphusd nodes` prints a line under the table for each connected worker that has any, such as `liar: results for verified tasks: 3 agreed, 2 outvoted; on probation, 7 more to agree`. The desktop's API gives the same in `ListWorkers`, and the MCP server in `pool_status`.
- **It is kept in the node's database**, so it survives a restart. It does not go when the jobs it came from are forgotten (`--keep-jobs`), and nothing makes it lapse with time.

What it does not do:

- **It is not a score, and nothing is scheduled by it.** A worker on probation is handed tasks as it was before, in the same order as the others. No worker is ranked, charged or banned, and there is no stake to lose. Removing a worker is still up to the pool's owner (`pool remove`).
- **Being on probation does not mean a worker lied.** A worker outvoted by workers that agreed to lie lands on probation, and so does an honest one whose work does not give the same result every time, when two others happened to agree. What it costs is the pool's time: the tasks such a worker is handed in a job with spot checks are run by `N` workers.
- **It goes by node ID, not by person.** A member that is removed and invited again under a new key starts clean, with no counts and no probation. What was counted under its old ID stays there, and comes back only if that same key is admitted again.
- **It does not go back over other jobs.** A task a worker was handed before it went on probation, in another job running at the same time, is not verified after all. Only the job that caught it does that.

**What it is for, and what it is not.**

- **It is for work that gives the same result every time it is run**, or whose workload compares its results [within a tolerance](#within-a-workloads-tolerance) of its own. `primes`, `wordcount`, `forecast`, `forecasts` and a container that is deterministic do. Work that involves a language model (`chat`, `prompts`, `embed`), `transcode`, and containers that use the time, random numbers or the network may not: two honest workers then differ, and the job fails saying so. Such work is comparable only through a workload that says what is the same; none of those does yet.
- **A private job is verified like any other.** Every worker asked is given the job's own key, and sealing gives the same blob for the same key and data, so workers that did the same work stored blobs with the same CIDs. Asking more workers shows the job's data to more of them. In the job's record, what each worker returned is not given by its digest but by a commitment only the job's key can check (`receipts/N/results/M/digest_commitment`), since a digest would tell someone who guessed a task's output that they were right; which workers agreed is still there to read.
- **It does not protect against workers that agree to lie.** `N` workers that return the same wrong result are believed. Nor does it protect, as it is, against one operator running several nodes: different node IDs are not different people. A coordinator started with `--verify-distinct-addresses` makes that harder; see [below](#workers-at-different-addresses).
- **It judges workers only this far.** A worker that was outvoted is named in the events and the record, has that result counted in its [standing](#a-workers-standing), and is on probation until ten of its results have agreed. Nothing else happens to it: no score that decides who is given work, no stake and no ban. Removing it is up to the pool's owner (`pool remove`).
- **It checks what workers return, not how the coordinator combines it.** The coordinator still splits the job and aggregates the outputs itself.


### Workers at different addresses

Two workers on one machine agree with each other because one person runs both. A coordinator started with `--verify-distinct-addresses` hands a verified task's copies only to workers that connected from different network addresses.

```sh
bin/sisyphusd run --role coordinator --verify-distinct-addresses
```

- **What counts as one address.** An IPv4 address as it is; an IPv6 address by its `/64`, which is what one site is usually given. It is the address the worker's connection to the coordinator comes from, so it is the coordinator that says where a worker is, not the worker.
- **Too few is refused at once.** A job with `--verify N` is refused unless workers at `N` different addresses could take it, and the refusal says it is "counting those at one address as one". A task whose workers disagree asks one more, at an address none of the others is at, and fails if there is none.
- **It is for a pool with strangers in it.** Workers behind one router, in one office or on one machine share an address and count as one, so in a pool of your own machines on one network it leaves nothing to verify with. That is why it is off unless asked for. Workers that reach the coordinator through one relay or one VPN exit share an address too.
- **It does not stop someone with several addresses**, and it does not stop two people who agree to lie. It raises what cheating takes from a second process to a second place on the network. [`--audit-share`](#checked-by-the-coordinator-itself) is what catches workers that agree.
- **Jobs that are not verified are not changed**, and nor is who a spot check or an audit picks.

### Checked by the coordinator itself

`--verify` needs at least two workers and is fooled by two that agree to lie. `--audit-share` needs neither a second worker nor anyone's honesty but the coordinator's:

```sh
bin/sisyphusd job submit --audit-share 0.2 --params '{"from":0,"to":100000000}'   # about a task in five is run again by this node
bin/sisyphusd job logs <job-id>                                                   # which, and how each came out
```

- **What it does.** As each task's result comes in, the coordinating node decides by chance, with the probability given, whether to check it. If so the result is not taken yet: the node runs the task itself and compares, as `--verify` compares. The same, and the worker's result is taken and counts for its [standing](#a-workers-standing). Different, and the node's own result is the task's, the worker is counted as outvoted and put on probation, and the job's events say so.
- **No worker knows which tasks.** The choice is made after the result is in. A worker that returns one wrong result in a job audited at a share of 0.2 is caught one time in five, each time.
- **It costs the coordinating machine the work** of every task it checks, on top of what the worker did. A share of 1 has the node do the whole job again.
- **It is for a node that is also a worker**, and for workloads it runs: a node started with `--role coordinator`, or one asked to audit `container` jobs without `--containers`, refuses, since it cannot run the task.
- **It is for work that gives the same result every time**, like `--verify`, or whose workload compares its results [within a tolerance](#within-a-workloads-tolerance): the node's own result and the worker's are then compared by the workload, a worker within it has its own result taken, and one beyond it is caught, with what differed in the events. A task the node fails to run is not held against the worker: its result is taken, and the events say it went unchecked.
- **Not with `--verify`.** A job asks other workers to check, or the coordinator, not both.
- **A private job is audited like any other**: the node has the job's key.
- **The job's record says what the audits found.** `audit_share` is among what was asked, and the root's `audits` lists each task that was run again: its number, the worker (node ID and name), and the `outcome`. That is `matched`, `caught` (the node got another result, and took its own), or `inconclusive` (the node could not run the task, so the worker's result stands unchecked). A task missing from the list was not audited, and a job with no audits has no list, so its record is the same as before audits were recorded. The entries name tasks and workers and nothing of what either computed, so a private job's record carries them too. The events (`job logs`) still tell the same in words.
- **A job made of steps** hands its audit share to each step.

## A job's record

When a job is over, whether it succeeded, failed or was cancelled, its coordinator writes its history as linked data: a few small nodes that name each other, and the job's stored data, by content ID. The ID of the root names the whole of it. Change anything in the history and the root's ID changes. That one ID is what a chain would commit to (issue #20).

```sh
bin/sisyphusd job record <job-id>            # the root's ID on a line, then the record's nodes as one JSON object, by ID
bin/sisyphusd job record --verify <job-id>   # also work it out again from the job, and fail if the two differ
bin/sisyphusd job record --key-file job.key <job-id>   # a private job: verify, then check its commitments with the job's key
```

A record is made of these nodes:

| Node | What it holds |
| --- | --- |
| root | `kind` (`sisyphus-job-record`), `version` (1), the job's ID, whether it is `private`, and links to the others. A step of a graph has `parent`: the graph's job ID and the step's name. A job whose tasks the coordinator [checked itself](#checked-by-the-coordinator-itself) has `audits`: for each task it ran again, the task, the worker and the `outcome`. |
| manifest | What was asked: `workload`, `params`, `mode`, `requirements` (tasks asked for, task timeout, memory, graphics cards, `verify` for a job that was [verified](#verifying-results), and `audit_share` for one the coordinator [checked itself](#checked-by-the-coordinator-itself)), links to the `inputs` it read, the node ID of the `submitter` (absent for a job the node gave itself, from the desktop app or the planner), `created_at`. |
| a receipt for each task | The task's number, how it ended, its `output`, how many attempts failed, and every attempt in order: which worker (node ID and name), how it went, and why it failed if it did. An attempt at a model task (`chat`, `prompts`, `embed`) also has what its worker said it `used`: the model server and the version it gave, the model with its digest and quantisation, and the options sent, the owner's included. That is the worker's own account, kept and not checked; see [verifying model inference](../../docs/research/verifying-inference.md). For a verified job also `results`: for each result a worker returned, the attempt, the worker, a `digest` that is the same for results that are the same, and whether it `agreed` with the result the task was settled by. |
| result | How the job ended, its `result` or `error`, links to the `outputs` it stored and to the `intermediate` data that passed between its tasks, `finished_at`. |
| graph | Only for a job made of jobs: each step's name, its job's ID, and a link to that job's record. |

- **It is ordinary IPLD.** Each node is DAG-CBOR and its ID is a version 1 CID of it with SHA-256. On a node started with `--kubo`, `ipfs dag get <root>/manifest/workload` reads it, following the links by name. The JSON printed is DAG-JSON: a link is `{"/": "<cid>"}` and bytes are `{"/": {"bytes": "<base64>"}}`.
- **The same job always gives the same record.** It is worked out from the job and nothing else, so the ID is known the moment the job ends: `job get` prints it, and the job carries it as `record_cid`. Storing the nodes follows at once; if that fails, or the store has lost them, they are stored again when the record is asked for. A restart changes nothing.
- **`--verify` checks the record against the job** as the coordinator holds it now, and each node read is checked against its ID. It finds a job whose row in the database, or whose record in the store, was changed after the job ended. On its own that is the coordinator checked against itself.
- **The coordinator signs each record.** Beside the record it keeps an attestation, a DAG-CBOR node of its own holding the record's CID, the coordinator's node ID and an Ed25519 signature by the node's key. `job record` prints the attestation's CID, and `--verify` also checks the signature and prints `signed by:` with the node ID. The record and its CID are the same with or without it.
  - **What is signed** is the text `sisyphus-job-record-signature/v1`, a zero byte, and the record root's CID in binary. That prefix keeps the signature from passing for anything else the node signs, such as a name.
  - **Anyone can check it** with the node ID alone, since the ID carries the public key: `jobrecord.CheckAttestation` in Go, or any Ed25519 library given that statement.
  - **What it proves** is that this coordinator vouched for this record, and that nothing in the record changed since. It does not prove the record is true: a coordinator can sign a record of work that never happened, as it could write one.
  - **No signing time.** The attestation has no time of its own, since a time from the coordinator's own clock would prove nothing. The record's `finished_at` says when the coordinator says the job ended.
- **Parameters, a result or a task's output longer than 16 KiB** are not held in the node but linked, by the CID they have as a stored blob, and stored beside the record for the retention period.
- **A record outlives the job's data.** It is kept for as long as the job is on record (`--keep-jobs`), while the data it links is kept for `--retain`. After that the links name data the node no longer holds. A graph's steps stay on record for as long as the graph does.
- **Times that are not kept across a restart are left out**: when each attempt began and ended. So is which task read or stored which blob; the record has that for the job as a whole.
- **A job from before records were kept has none.** `record_cid` is empty for it.
- **What a worker said it `used` is only there where a worker said it.** So the record of a job from before workers said it, or of work that asks no model server, is worked out again exactly as it was. That is why it did not change the record's `version`.

**What a record reveals.** A record sits in the coordinator's store, and on a node with `--kubo` in the pool's private network, where any member that knows or guesses its ID can fetch it. An ID cannot be guessed, and is told to clients of the coordinator with the job. Whoever has it reads everything in the table above: the workload, the parameters, the result, each task's output, the error messages, the node IDs and names of the submitter and of every worker that was handed a task, the times, and the IDs of the job's data.

For a **private job** the record says so and leaves out everything that was not sealed: the parameters, the result, each task's output, and every error message, since a message may quote data. It keeps the workload's name, the requirements, who ran what and how it went, the times, and the IDs of the sealed inputs and outputs, which are of no use without the key.

In place of each value left out, the record of a private job holds a **commitment** to it: 32 bytes that fix the value without showing it. So the record still commits to the job's result, and whoever holds the key can later show that a given result is the one the job produced.

| Where | What it commits to |
| --- | --- |
| manifest, `params_commitment` | The parameters. |
| each receipt, `output_commitment` | The task's output. |
| each attempt in a receipt, `error_commitment` | Why the attempt failed. |
| each attempt at a model task, `used_commitment` | What its worker said it ran with. The model and its options can say what a job was doing. |
| each result in a receipt of a verified job, `digest_commitment` | The digest of what the worker returned. Results that were the same do not have the same commitment; `agreed` says which side each was on. |
| result, `result_commitment` and `error_commitment` | The job's result, and its error. |

- **What a commitment is.** HMAC-SHA-256 of the job's ID, the commitment's path in the record (such as `receipts/0/output_commitment`) and the value. The ID and the path are each preceded by their length, as eight bytes, big-endian. The HMAC key is 32 bytes derived from the job's sealing key by HKDF-SHA-256 with no salt and the label `sisyphus job record: commitments`, so it is not the key the data is sealed with.
- **Without the key it reveals nothing about the value**: not the value, and not whether a guess at it is right. A plain hash would let anyone confirm a guess at a small result such as `{"count":25}`. It does not hide that there is a value to commit to: every private record has every commitment in the table, and an empty value, such as the error of an attempt that succeeded, is committed to like any other. The exception is `used_commitment`, which only attempts whose worker said what they ran with have. It shows which attempts were at a model task, as the workload's name already does.
- **Equal values do not give equal commitments.** The job's ID and the path go into each, so the same result in two jobs under one key, or the same output from two tasks of one job, gives commitments that cannot be told to be of equal values.
- **They are made when the job ends**, while the coordinator still has the key, and saved with the job. The key is not saved with a finished job. The record is worked out again from the saved commitments, so `--verify` works on a private job without the key, before and after a restart.
- **`--verify` without the key does not check the values.** The record holds the commitments and not the values, so a result changed in the coordinator's database after the job ended leaves the record as it was. Only the key finds that.
- **`--key-file` checks them.** It takes the job's key, the file `job submit --key-file` was given, or the node's `private.key` for a job made private in the desktop app. It verifies the record, then sends the key to the coordinator, which works each commitment out again from the parameters, result, outputs and errors it holds and says of each whether it `matches` or `does not match`. The coordinator uses the key for that and does not keep it. The command fails if any does not match. That happens when the key is not the job's, in which case none match, or when a value has changed since the job ended. It says nothing against the record itself, which was verified first.
- **A record with no commitments has nothing to check**, and `--key-file` says so: the record of a job that is not private, or of a private job that ended before commitments were made, whose record is as it was.

## Restarting a coordinator

A coordinator keeps its jobs in `node.db`, a SQLite database in its data directory, so stopping it loses nothing:

- **Finished jobs** are still there to be asked after, with their results.
- **Unfinished jobs** are taken up where they were. Tasks that had finished stay finished. Tasks that were running go back to wait for a worker, and that is not counted against them, however often it happens. A task of a [verified](#verifying-results) job keeps the results it had, and only the workers that had not answered are asked again. If every task had finished and only combining their outputs was cut short, the combining is done again, with no worker needed.
- **A job's data** stays pinned across the restart for as long as the job takes.
- **Workers** reconnect by themselves once the coordinator is back.
- **Storage followers** go on holding what they held. A coordinator that comes back without its stored data takes it back from them; see [Copies on other nodes](#copies-on-other-nodes).

A job's arrival and its finish are written through to the disk before anyone is told of them. The steps in between are not, so a power cut can lose the last few; the tasks concerned run again. A coordinator started without a workload that an unfinished job needs fails that job, and says why.

The database holds the rest of what a coordinator must not forget as well: which nodes have been admitted, and the invitations issued and not yet used, so an invitation is still good after a restart. Invitations are recorded by a hash of their token, so reading the database admits nobody. A coordinator from before this was so finds its old `access.json`, takes its members in, and sets the file aside as `access.json.imported`.

A finished job can be asked after for 30 days, or whatever `--keep-jobs` says; `--keep-jobs 0` keeps them for good. A client's job is kept at least as long as `--retain` too: its record is what says whose its data is, so that data counts towards the client in `pool members` until it lapses, across restarts. That is about the record of the job. How long its data is kept is `--retain`, described under [How long data is kept](#how-long-data-is-kept).

A node's own record of the coordinators it has joined, `known.json`, stays a file. Every command reads it, on machines that run no coordinator and have no database, and it is small enough to mend by hand when a coordinator's address changes.

The database also records every attempt at every task: which node, and how it went. A finished job's [record](#a-jobs-record) is written from it. It goes when the job does.

## The desktop app

[`apps/sisyphus`](../sisyphus/README.md) is a desktop client that shows a node and its peers. It talks to a node on the same machine through the local API, `sisyphus.node.v1` in [`proto/sisyphus/node/v1`](../../proto/sisyphus/node/v1/node.proto). The local API is off unless asked for:

```sh
./sisyphusd run --name alpha --api-listen 127.0.0.1:50051
```

`127.0.0.1:50051` is where the desktop app looks. The address must be a loopback one: the local API has no TLS, and is for programs on this machine only.

What it shows, in this daemon's terms:

Country markers ignore addresses inside this machine's directly connected interface networks, even when an address block is globally allocated. A LAN-only address cannot reliably place a peer on the world map.

| The desktop's word | What it is here |
| --- | --- |
| peer | A node of this node's pool, or one its libp2p host has found: on the same network, through nodes it knows, or because it was given the address. |
| connected | The node has a live connection to this one right now, as a worker or between the two hosts. |
| gives work, takes work | The two sides of this node's trust in the peer: that it gives the peer tasks, and that it takes the peer's. See [Trust](#trust) below. |
| trusted for compute | Either of the two. The call that sets it sets both, for clients that know only the one switch. |
| working for you, you work for it | Which way work is flowing now: the peer is connected as a worker of this node, and this node is working for the peer. Both need trust on both sides. |
| connect to an address | Connects this node's host to the node at an address ending in `/p2p/<node ID>`, which is the form a node lists its own in, and notes it in the address book. |
| bootstrap peers | The node's address book: the addresses it connects to each time it starts. |
| files | What was put in the pool's store through the app: kept until removed, listed by name, and given to jobs by CID. The same store `blob put` and `blob get` use. |
| invitation, members | `pool invite`, `pool members` and `pool remove`, from the app. |
| private | A file or job sealed with the key the node keeps in `private.key` in its data directory, made the first time one is asked for. The same as `--key-file` with that file. Lose the file and what it sealed is lost. It is one key for everything the node makes private: a worker handed any private job from this node can open all of its private files and the data of its other private jobs. |
| join a pool | `pool join` and a restart with `--join`, without the restart: the node redeems an invitation, says it takes that node's work, and starts on it. |
| country | Where the node's address is registered, and each peer's, from a table the daemon carries (`packages/geo`, made from the regional Internet registries' published allocations). Nothing is asked of anyone. A node that knows itself only by a home-network address is placed by its address as its pool sees it: a worker by the source address its coordinator sees it connect from, which the coordinator sends it on joining, and a coordinator by the address most of its directly connected workers say they reached it at, and nowhere on a tie. Either side takes only a global address, never a private one, one shared by a carrier's NAT (`100.64.0.0/10`), one the IANA special-purpose registries say is not globally reachable or set aside for documentation, benchmarks, translation or tunnelling (NAT64, 6to4, Teredo), an IPv6 one outside `2000::/3`, or one carried over a tunnel or relay, and uses it only to show where the node is; it is never taken as an address the node can be reached at. Behind a VPN this is the VPN's country: where the node's traffic comes from, as the pool sees it. Failing both, `--locate-gateway` asks the machine's default gateway, read from the routing table and never searched for, which address it shows the internet. It sends NAT-PMP's one read-only request (RFC 6886, opcode 0) and never asks for a mapping, so no port is opened; the routing table is read from `/proc/net/route` on Linux, from `route -n get default` on macOS and the BSDs, and from PowerShell's `Find-NetRoute` on Windows, each the route the system itself would use, and the answer is held to the same global-address check. PCP is not used, since its routers may hand out an address only by making a mapping. Failing that, `--stun-server host[:port]` asks the STUN server named (port 3478 if none is given) which address the node's traffic reaches it from, by a single Binding request (RFC 8489); no port is opened, but the server learns that address, so no server is asked unless one is named, and the one named is asked only when the node is first asked where it is and none of the sources before it has placed it, waiting for the gateway's answer if that was asked for. It is asked once, and the answer is held to the same check. Failing that, `--locate-country` asks ipapi.co instead, which thereby learns the address, and from the request's `User-Agent` (`Sisyphus/<version>`) that it is a Sisyphus node and which version, so that is not done unasked, and is done only when the node is asked where it is and none of the sources before it has placed it, after the gateway and the STUN server, if they were asked for, have answered or given up. It is never waited for: the country it gives is shown from then on, and those watching the node are told of it. A definitive answer is kept until the node restarts and the service is not asked again: a `200 OK`, whether or not it gives a usable country, or a refusal such as `404` or `403`, which places the node nowhere. A transient failure, a `408` request timeout, a `429` rate limit, a `5xx` or no answer within five seconds, places the node nowhere for now, and the service may be asked again the next time the node is asked where it is, once a wait has passed: a minute after the first failure, doubling with each failure in a row up to an hour, or as long as the service's `Retry-After` says (in seconds or as an HTTP date) if that is longer, but never longer than the hour. Only one lookup is ever in flight, and once any of the sources before it places the node, the service is not asked again. `GetNodeInfo` says which of the five the country came from, in `country_source`. The country is the one of registration, which now and then is not where the machine is. `scripts/update-geo.sh` remakes the table. |
| owner's location | Where the owner says the node is, in degrees, and how much of it other members of a pool may see. See below. |
| city | The city the node's public address is in, from DB-IP's free city table, which is downloaded only when the owner asks for it. See below. |

Location discovery flags take precedence over the node database only when explicitly supplied (for example, `--locate-country=false`); flags omitted at startup use the saved setting, then the flag's default. This lets an operator pin an opt-out on the command line while allowing paired browsers and Electron to manage saved settings through the local API. `GetLocationDiscoverySettings` reports the effective running settings and whether a restart is needed.

The limits a coordinator holds each member to, `--max-jobs-per-client`, `--max-tasks-per-client`, `--max-task-timeout`, `--max-store-bytes-per-client` and `--max-upload-bytes-per-attempt`, can be saved in the node database the same way, with `SetPoolLimits`, and take effect when the node restarts. A flag supplied explicitly (for example, `--max-jobs-per-client=0`) overrides its saved limit; an omitted flag uses the saved limit, then the flag's default of no limit. `GetPoolLimits` reports the saved and running limits, whether a restart is needed, and whether the node runs the coordinator role, without which the limits hold no one to anything. The task timeout is saved in whole seconds. So that what is reported is what is enforced, the node refuses to start with a job or task count past 2147483647 or a task timeout that is not a whole number of seconds, whether it comes from a flag or was saved.

The owner can say where the node is, more precisely than any address can place it ([#573](https://github.com/sisyphus-network/Sisyphus/issues/573)). `SetNodeLocation` saves a `NodeLocation`: a `latitude` from -90 to 90 and a `longitude` from -180 to 180, in degrees with north and east positive, and a `share`, which says how much of it other members of a pool may see: `LOCATION_SHARE_EXACT`, `LOCATION_SHARE_ABOUT_1KM` (rounded to 0.01°), `LOCATION_SHARE_CITY` (rounded to 0.1°), `LOCATION_SHARE_COUNTRY` or `LOCATION_SHARE_NOTHING`. A share left unspecified is saved as `LOCATION_SHARE_COUNTRY`, so that a stranger in a pool does not see where the owner lives unless the owner says so. A coordinate that is not a finite number in its range is refused with `InvalidArgument`, and a `SetNodeLocation` with no location clears the saved one. `GetNodeLocation` returns what is saved, with no location if none is. Both need the token. A change takes effect at once, with no restart: `GetNodeInfo` gives the location in `owner_location`, as it was given, whatever the share, and those watching the node with `WatchNodeState` are told of it. Since anything on the machine may call `GetNodeInfo`, `owner_location` is there only for a caller that presents the token; `country_code` and `country_source` are unchanged. From the command line, `sisyphusd location set --lat 51.5074 --lon -0.1278 --share city` saves a location, `location show` prints it and `location clear` forgets it; `--share` takes `exact`, `about-1km`, `city`, `country` (the default) or `nothing`. Like `model set`, these work on the node's database, so the node need not be running; a running one gives the new location the next time it is asked for its info, though a watcher is told of it only when something else changes.

**What the pool sees of it.** A worker tells its coordinator where it is in its `Hello`, as `shared_location`, already rounded on the worker by `geo.Shared` to the share its owner chose: the pin as given for `exact`, to 0.01° for `about-1km`, to 0.1° for `city`. For `country` or `nothing`, or with no pin, no coordinates are sent at all, and the exact pin never leaves the machine unless the share is `exact`. The coordinator keeps what it was told with the worker, checks only that it is a place on the Earth, and passes it on unchanged: in `ListNodes`, as `NodeInfo.shared_location`, to the pool's clients (the only members `ListNodes` is open to), and through each node's local API in `ListWorkers` and `WatchWorkers`, as `Worker.shared_location`. `ListWorkers` gives it only to a caller that presents the token, as `GetNodeInfo` does the owner's own pin. Absent means the owner shares no coordinates. A change made through `SetNodeLocation` reaches the pool at once, with no reconnection: the worker sends a heartbeat carrying a `LocationUpdate`, with the new place or, when the owner has stopped sharing it, none. One made with `sisyphusd location set` while the node runs is sent the next time the worker connects. A worker that works for other nodes besides its own pool tells each of them the same. Workers from before this send nothing and are listed at no place, and coordinators from before this ignore the field. `sisyphusd nodes` shows it below its table, as `laptop: shared place 51.5, -0.13`.

Finer than the country, a node can find the city its public address is in, from DB-IP's free "IP to City Lite" table ([db-ip.com/db/download/ip-to-city-lite](https://db-ip.com/db/download/ip-to-city-lite)), published monthly under the Creative Commons Attribution 4.0 licence. The table is not carried in the daemon: at some 86 MB gzipped it is a few hundred times the size of the country table, so it is downloaded only when the owner asks for it, never on its own. `sisyphusd location city-table download` fetches this month's, `https://download.db-ip.com/free/dbip-city-lite-YYYY-MM.csv.gz`, or last month's if this month's is not out yet, reads it and keeps it in the data directory as `dbip-city-lite.bin`, some 93 MB in a form of the daemon's own that loads in a fraction of a second; `location city-table status` says which month's is kept, how large it is and when it was downloaded, and `location city-table remove` deletes it. Like `location set`, these work on the data directory, so the node need not be running; one that is uses the table, or stops, the next time it is asked for its info. The local API's `DownloadCityTable` and `RemoveCityTable` do the same and need the token, and `GetCityTable` says whether it is there, for anyone on the machine. `DownloadCityTable` waits for the whole download and then tells those watching the node.

Downloading the table shows db-ip.com this machine's address, as any download does; nothing else is sent, and no request says it comes from Sisyphus. Looking an address up afterwards asks no one: the table is on the machine. The node looks up only the address that placed it in its country, its own, its pool's view of it, its gateway's or the STUN server's (`country_source`); a country from the lookup service comes with no address, so no city. The table is read only when an address not looked up before is asked about, and let go of after, so the node does not keep its tens of megabytes in memory. `GetNodeInfo` gives what it found in `city_estimate`: the `city`, `region`, `country_code`, `latitude` and `longitude`, the `source` as in `country_source`, and the credit the licence asks for wherever the table is used, `attribution` ("IP Geolocation by DB-IP") and `attribution_url` (https://db-ip.com), which `GetCityTable` and the command line give too. An owner's location wins: once one is set, `city_estimate` is left out. The city is an estimate. An address is placed where its network is, which for a home connection is often the provider's nearest city, tens of kilometres from the machine, and now and then further; IPv6 addresses are placed by their first 64 bits, the network a provider hands a home. Nothing of the city is sent to the pool.

With `--web-listen`, the same API is served to browser clients as Connect and gRPC-Web. Add `--web-ui-dir <renderer-build-directory>` to have this node serve the React UI from the same origin too. This development listener is loopback-only. For remote browser access, open Settings → Access and explicitly enable the HTTPS listener on one or more active, node-assigned interface addresses. It is off by default, uses a node-managed self-signed certificate, and exposes only the browser UI/API—not the gRPC control API or the node-to-node listener. The certificate's SHA-256 fingerprint is shown in Settings for verification. Pairing codes and browser sessions are sent only over HTTPS. [`docs/desktop-backend.md`](../../docs/desktop-backend.md#from-a-web-page) has the details.

Anything on the machine can read what the node is, who it knows and who works for it. Changing something, and reading jobs, conversations or what files contain, needs the token in `api.token` in the data directory, which the daemon makes on first use and only its own user can read. The desktop app looks for it in the daemon's default data directory, or in the file named by `SISYPHUS_API_TOKEN_FILE`.

## Finding other nodes

Location discovery flags take precedence over the node database only when explicitly supplied (for example, `--locate-country=false`); flags omitted at startup use the saved setting, then the flag's default. This lets an operator pin an opt-out on the command line while allowing paired browsers and Electron to manage saved settings through the local API. `GetLocationDiscoverySettings` reports the effective running settings and whether a restart is needed.
A node looks for others and lets them find it, unless started with `--discovery off`:

- **On its own network**, by multicast DNS. Two nodes on one LAN find each other within a second or two with nothing configured.
- **Through the nodes it knows**, by a distributed hash table (Kademlia) that every node both uses and serves. A node that knows one other can find the rest, and can reach a node knowing only its ID.
- **From its address book**, the addresses it connects to each time it starts. `--bootstrap <address>,<address>` adds some for one run; the desktop's "connect to peer" and bootstrap list add them for good.

The names it uses for both are Sisyphus's own, so nodes find each other and not every other libp2p program in reach.

**Being found is not being let in.** With discovery on, any libp2p node may connect to a node's host. It can then see the node's ID and addresses and take part in the hash table, and that is all. Everything else checks who is asking: jobs, stored data, the relay and the private IPFS network are for the pool's members. With `--discovery off` a node's host accepts connections from its pool's members only, and finds nobody.

## Trust

Trust in another node for compute has two sides, set separately:

- **Giving it work.** The node may work for this one: it is a worker in this node's pool.
- **Taking its work.** This node will work for it.

Work flows from one node to another when the first gives and the second takes. Nobody is put to work by someone else's choice, and nobody is given tasks by a node they did not agree to work for.

| To do this | From the desktop | From the command line |
| --- | --- | --- |
| Give a node work | "Give it work" beside the peer | `pool invite` here and `pool join` there, for a node not yet known; nothing for one already found, use the desktop |
| Stop giving it work | the same button | `pool remove <node ID>` |
| Take a node's work | "Take its work" beside the peer | `pool work-for <node ID>` |
| Stop taking it | the same button | `pool work-for --stop <node ID>` |

`pool members` lists both: the nodes admitted, and the nodes this one will work for.

**How two nodes come to work together**

- **Two nodes that each run a pool, on one network.** Each finds the other in its peer list. Each owner chooses what they want of the other: give, take, or both. Wherever one gives and the other takes, work starts within a few seconds, with neither node restarted. Both choosing both means each works for the other.
- **An invitation.** `pool invite` on a coordinator and `--join` on a worker-only node, as always. The coordinator gives; the worker, started for that coordinator, takes.
- **A worker-only node and a second coordinator.** A node started as one coordinator's worker can take another's work as well: that coordinator gives it work, and it is told to take it, from its desktop or, on a node that also runs a pool, `pool work-for`.

**What holds whichever way it was set up**

- **Either side ends it.** A node that stops giving cuts the other's worker off; a node that stops taking stops working. Both are at once.
- **It takes effect at once.** A trusted peer opens an authenticated libp2p trust stream. It starts with the current admission state, then carries changes as they happen. If the stream breaks, the worker stops guest work, reconnects with bounded backoff, and receives a fresh state before resuming; there is no periodic trust check while connected. This stream requires trust protocol v2, so older coordinators do not offer reciprocal guest work until upgraded.
- **A node's slots are shared, fairly.** However many nodes it works for, it runs no more tasks at once than `--slots`. A coordinator may hand it more than that, and those wait their turn. When a slot comes free it goes to the pool with the fewest of its tasks running on this node, so one pool's long queue does not hold up another's single task.
- **Each pool's data is kept apart.** What a node fetches while working for another is kept under `guest/<that node's ID>` in its data directory, away from its own pool's store and from every other node's.
- **With `--kubo`, it joins the other pool's private IPFS network too.** A private network has a key of its own, so the node runs one more Kubo daemon for each pool it works for, on a repository under that same directory, and follows that pool's key when it changes. Each costs about as much memory as the node's own. If the other node runs no such network, the work is done without one.
- **The desktop shows which way work is flowing**: beside each peer, "Working for you" and "You work for it", whichever hold.
- **`--work-for-trusted=false`** has a node take nobody's work but that of the coordinator it was started for, whatever its list says.

What this does not do yet:
- **Nothing limits what a node gives.** A node that gives another work gives it any task; there is no "only these workloads" or "only this much".
- A node works for at most sixteen others at once.

## Coming from the Rust daemon

Before this daemon there was one written in Rust, which kept a node's key, address book and trusted nodes in one database, `node.sqlite3`, in the same data directory. A node that ran it carries on here as itself:

- **The same key, so the same ID.** The first command that needs the node's key takes it from that database rather than making a new one.
- **The same address book and the same trusted nodes**, carried over the first time the node runs a pool. That daemon's trust had one side; a node it trusted is here both given work and taken from.
- The old database is then set aside as `node.sqlite3.imported`. Nothing is deleted.

If that database is there and cannot be read, the node stops and says so. It does not quietly start as a different node.

This was checked against a database made by the Rust daemon itself, which is kept with the tests.

## Examples

[`examples/`](../../examples/README.md) has a script that runs a pool on this machine and leaves it up, scripts that run each workload against a pool, and a sample input. [`docs/multi-machine-test.md`](../../docs/multi-machine-test.md) is a guided test of a pool across real machines.

## Workloads

- **`container`** runs a container image, once for each task of the job. This is how real programs run on a pool: anything that can be put in an image. See [Running containers](#running-containers) below.
- **`transcode`** re-encodes a video. It is the first workload that does a job people have: see [Transcoding video](#transcoding-video) below.
- **`md`** simulates a molecule with OpenMM, as replicas spread over the pool: see [Molecular dynamics](#molecular-dynamics) below.
- **`render`** renders a Blender scene, by frames or by strips of one frame, across the pool: see [3D rendering](#3d-rendering) below.
- **`qchem`** computes the energies, optimised geometries or frequencies of a file of conformers or molecules with xtb, in runs across the pool: see [Quantum chemistry](#quantum-chemistry) below.
- **`ocr`** reads the text in a scanned PDF or picture with Tesseract, by runs of pages across the pool, as plain text or a searchable PDF: see [Text from scans](#text-from-scans) below.
- **`traj`** analyses molecular dynamics trajectories with AmberTools' cpptraj, by runs of frames or whole trajectories across the pool: RMSD, radius of gyration, a distance, hydrogen bonds, secondary structure and RMSF: see [Trajectory analysis](#trajectory-analysis) below.
- **`prep`** prepares Amber systems with AmberTools' tleap, by runs of PDB files across the pool: each protein or nucleic acid given its hydrogens, a box of water, the ions that make it neutral and any salt asked for, as the prmtop and inpcrd `md` takes: see [Amber system preparation](#amber-system-preparation) below.
- **`primes`** counts the primes in `[from, to)`. Parameters and results are a few bytes and travel inline. It is a stand-in that exercises the pool.
- **`wordcount`** counts word frequencies in a stored file. It exercises the storage path: the input is fetched by CID, each task reads only its byte range, task outputs are stored as blobs, and the result is a blob too. The result's CID is the same however the job is split, so two runs can be compared by CID alone.

A workload may also describe its parameters as a form, so that a program can ask for them field by field instead of having JSON written by hand. Each field has its JSON key; its kind, which is a stored file's CID, a list of them, an integer, a number, a line of text, one of a list of choices, or a toggle; a label and a sentence of help; and a default, bounds and a step for a number, and whether a job is refused without it. A form sent with its defaults unchanged is a job the workload accepts. `primes`, `wordcount`, `forecast`, `forecasts`, `transcode`, `md`, `render`, `transcribe`, `qchem`, `ocr`, `traj` and `prep` have forms. The local API's `ListWorkloads` lists every workload the node takes jobs of, with what it does, its form, and how many of the connected workers offer it, busy or not. A graph is marked `composite`, since the coordinator carries it out itself. Like `ListWorkers`, it needs no token, as it says what the pool can do and nothing of what anyone has done with it. A node that coordinates no pool answers it with `FAILED_PRECONDITION`. An agent's `list_workloads` reads it, and the desktop's Run page (#585) is to build its forms from it.

### Running containers

```sh
bin/sisyphusd run --role worker --coordinator <addr> --containers ...     # a worker that will run them
bin/sisyphusd job submit --workload container --tasks 8 \
    --params '{"image":"alpine:3.20","command":["sh","-c","echo I am task $SISYPHUS_TASK_INDEX"]}'
examples/container.sh                                                    # the same, and prints each task's output
examples/render.sh                                                       # a picture rendered in strips across the pool
```

- **A worker runs containers only if its owner says so**, with `--containers`, and needs Docker. **This is real authority over the machine:** whoever may submit jobs to the pool can run any image with any command there. Give it only on pools whose clients you would let log in.
- **One copy for each task.** Every task runs the same image and command and is told which copy it is, in `SISYPHUS_TASK_INDEX` (from 0) and `SISYPHUS_TASK_COUNT`. A program that does a share of some larger work takes its share from those.
- **Input.** `"input": "<CID>"` gives every task that stored file at `/input/data`, read-only. The coordinator opens it when the job is submitted, so an input it does not hold is refused then, and the job keeps the file until it ends, however long that is: an upload's hour of grace, or the user unpinning it, does not take it from under tasks still to run.
- **More inputs, by name.** `"inputs": {"<name>": "<CID>", ...}` gives every task each of those stored files at `/input/<name>`, beside `/input/data`, read-only, and is kept the same way. A job may name 16. A name is 1 to 64 lower-case letters, digits, dots, dashes and underscores, does not end in a dot, and is not `data`, `.` or `..`, so it can only ever be a file in `/input`: no slash, no way up. Nor may it be a name Windows keeps for a device, with or without an extension (`nul`, `con`, `prn`, `aux`, `com0`–`com9`, `lpt0`–`lpt9`, so also `nul.txt`), which a Windows worker would open as the device instead of a file. The coordinator refuses any other when the job is submitted, and a worker refuses it again in a task it is handed. A job's inputs on one worker are fetched once for all its tasks there, into one directory mounted read-only.
- **Output.** What the command prints is stored and its CID returned for each task, and so is every file it leaves in `/output`, up to 4 GiB and 1,024 files for a task: a task that leaves more fails. This is checked after the command exits; it is not a disk limit while the command runs. `blob get <CID>` fetches them. What it prints is also the task's log, live, in `job logs`, of which a job keeps 20,000 lines; all of it, up to 16 MiB for a task, is in what was stored.
- **Limits.** `"memory_mb"` and `"cpus"` cap what each task may use, and unlike a node's offer these are enforced, by Docker. The worker's owner can set the most a task may have, with `--containers-memory-mb` and `--containers-cpus`: a job that asks for less gets what it asks, and one that asks for more, or for no limit, gets the owner's. A task has no network unless the job says `"network": true` and the worker's owner allows it with `--containers-network`. Such a job is given only to workers that allow it, and waits while none is connected; the worker also refuses it itself, whatever a coordinator hands it. It runs as the user the node runs as: not as root unless the node itself does, and with none of the capabilities Docker usually gives a container. Where the system has no user numbers, as on Windows, it runs as the image has it, with Docker's usual capabilities. Either way it cannot gain privilege it did not start with (`no-new-privileges`), so a setuid program in the image does nothing more than any other, and it may have 4,096 processes at once. The node's user has no home in the image, so the command's `HOME` is `/tmp`, which it can write to, unless the job's `env` gives another: `pip install` and the like, which install for the user, then work.
- **Holding tasks in further.** A container shares this machine's kernel, which is the weak point when the work is a stranger's. Additional worker settings ask more of Docker and can place task files on a separate filesystem:
  - `--container-runtime runsc` runs every task with the runtime named in place of Docker's usual: [gVisor](https://gvisor.dev)'s `runsc`, which stands between the task and the kernel, or Kata Containers' runtime, which gives each task a small virtual machine. The runtime is installed and made known to Docker by you; the node checks Docker has it and does not start otherwise, so it never runs tasks less held in than you asked. Neither has been tried here: this machine has only Docker's own runtime, and a graphics card complicates both.
  - `--containers-read-only` gives a task nowhere to write but `/output` and `/tmp`, with `/tmp` held in memory and counted against the task's memory. Nothing in the image can be changed. An image that writes elsewhere fails; `transcode` and what the examples run do not. With `--containers-tmp-mb` you set how much `/tmp` may hold. The `/output` byte limit is checked only after the task exits, so a running task can use more disk than that limit.
  - `--task-dir /mnt/sisyphus-tasks` puts both task workspaces and staged inputs under an existing directory. It does not set a byte limit itself. Put it on a dedicated filesystem or quota-backed mount if you need filesystem-enforced containment; the worker checks that the directory exists and is writable before starting. If the filesystem fills, staging or a task fails rather than the worker exiting. Several tasks share that filesystem, so its limit is per worker, not per task.
- **Stopping.** Cancelling the job, or a task running past the job's `--timeout`, kills the container. So does a task running past the worker's own `--containers-timeout`, which its owner sets whatever the job allows; the attempt then fails and is tried again like any other.
- **A job is taken in by any coordinator**, and waits until a worker that runs containers is connected, and one that allows the network if the job asks for it.
- **A coordinator can insist on hardened workers.** Started with `--containers-hardened`, it gives `container`, `transcode`, `md`, `render`, `transcribe`, `qchem`, `ocr`, `traj` and `prep` jobs only to workers that run containers with a sandboxing `--container-runtime` (gVisor's `runsc` or a Kata runtime, which the worker found Docker has before it started) and with `--containers-read-only` plus a sized `/tmp`. Such a worker says so in its labels, `container-sandboxed` and `container-bounded-tmp`, and `sisyphusd nodes` says it in words beneath the table, as in `laptop runs containers with the network allowed, sandboxed, with a read-only root and a sized /tmp`. These settings do not cap `/output` while a task runs; `--task-dir` can place all task files on a separate, quota-backed filesystem, but that bound is shared across tasks on the worker. A job waits while no qualifying worker is connected. It is for a pool that takes container work from people its workers' owners do not know; a private pool has no need of it.
- **Tasks of a job that run on one machine share one copy of its input**, fetched once. Tasks of different jobs share nothing.

- **Graphics cards.** `"gpus": 1` gives each task that many of the machine's cards, which needs Docker set up for them (the NVIDIA Container Toolkit). The job then goes only to machines with at least that many cards, as if submitted with `--min-gpus`, and a machine runs no more such tasks at once than it has cards, whatever its slots. This has not been tried on a machine with a card.

What it does not do yet: images are pulled by Docker when first used, from wherever the image name says, so a worker needs to reach that registry and trust it. To be sure of what runs, name the image by digest (`alpine@sha256:...`), which Docker checks.

### Transcoding video

```sh
cid=$(bin/sisyphusd blob put holiday.mov)
bin/sisyphusd job submit --workload transcode --params "{\"input\":\"$cid\",\"height\":720}"
examples/transcode.sh holiday.mov 720       # the same, and fetches the result
```

The video is cut into as many stretches as the job has tasks. Each task encodes one with ffmpeg, in a container, and the stretches are joined into one video: H.264 with AAC sound, in an MPEG transport stream (`.ts`), which players open and `ffmpeg -i out.ts -c copy out.mp4` puts in an MP4 without encoding again.

| Parameter | Meaning |
| --- | --- |
| `input` | The CID of the stored video. Anything ffmpeg reads. |
| `height` | Scale the picture to this many pixels high, keeping its shape. An even number. Left out, the size is kept. |
| `crf` | Quality, from 1 (best, largest) to 51. Default 23. |
| `preset` | Time against size, from `ultrafast` to `veryslow`. Default `medium`. |
| `segments` | How many stretches. Default: one for each free slot. |

- **It runs on workers started with `--containers`**, like any container job, but the image and the command are fixed: this workload cannot be made to run anything else.
- **Nothing to install but Docker.** A worker builds the image the first time it is needed, from two lines (Alpine and its ffmpeg package). That takes about a minute and the network, once.
- **When it helps.** ffmpeg already uses every core of one machine, so cutting a video up gains little there: on one 32-core machine, a one-minute 1080p clip at the `slow` preset took 43 s as one task and 31 s as eight, and a light encode was no faster cut up. The gain is across machines, where each encodes its stretch at the same time. Every worker fetches the whole video to encode its part, so a pool on a slow link is better served by fewer, longer stretches.
- **Where stretches meet**, the sound can overlap by a few hundredths of a second, and players step over it. Every frame of the picture is there, once.
- **Private jobs**: the workload reads and stores through the same sealed store the others do, so a sealed video and a job with its key should work; this has not been tried.

### Molecular dynamics

```sh
bin/sisyphusd job submit --workload md --params '{"picoseconds":10,"replicas":8}'
cid=$(bin/sisyphusd blob put protein.pdb)
bin/sisyphusd job submit --workload md --params "{\"input\":\"$cid\",\"picoseconds\":50}"
top=$(bin/sisyphusd blob put system.prmtop); crd=$(bin/sisyphusd blob put system.inpcrd)
bin/sisyphusd job submit --workload md --params "{\"prmtop\":\"$top\",\"inpcrd\":\"$crd\",\"picoseconds\":50}"
```

Each task simulates the same molecule from its own random start: a replica. Many short replicas side by side sample more of what a molecule does than one long run in the same time, and they need nothing from each other, so they spread over a pool as they are. Each task runs [OpenMM](https://openmm.org) (MIT and LGPL) in a container, with the Amber14 force field and TIP3P water, 2 fs a step, at constant temperature.

| Parameter | Meaning |
| --- | --- |
| `input` | The CID of a stored PDB file. A molecule without water is put in a box of it, 1 nm past the molecule on every side, with ions to make it neutral; missing hydrogens are added. Left out, the villin headpiece in water that OpenMM comes with (8,867 atoms). |
| `prmtop`, `inpcrd` | In place of `input`, the CIDs of a system prepared with Amber's tools: its topology and its coordinates, both or neither, and not with `input`. It is simulated as it is, with the force field its topology holds and nothing added: OpenMM reads both files itself. A `prep` job's systems are such files (see [Amber system preparation](#amber-system-preparation)). Coordinates with a box are simulated with periodic boundaries and PME, a 1 nm cutoff; without one, as a molecule in a vacuum or implicit solvent, with no cutoff. Bonds to hydrogen are held rigid either way. The last frame is written with the topology's atoms and residues. |
| `picoseconds` | How long each replica is simulated. Default 10. |
| `temperature` | In kelvin. Default 300. |
| `seed` | The first replica's random seed. Replica *n* uses `seed + n`. Default 1. |
| `frames` | How many snapshots of each replica to keep. Default 50. |
| `replicas` | How many. Default: one for each free slot, or with `gpu`, one for each graphics card. |
| `gpu` | `true` runs each replica on a graphics card. Default `false`, on the processor. |

The result gives the replicas' mean potential energy and its spread, and for each replica its seed, where it ran, how fast (in ns a day), its energy at the end, and the CIDs of its last frame (PDB), its energies as it went (CSV) and its trajectory (DCD, which VMD, PyMOL and MDAnalysis read).

- **It runs on workers started with `--containers`**, with the image and the script fixed. A worker builds the image the first time it is needed, from two lines (Python and OpenMM from PyPI), once.
- **On a graphics card, with `"gpu": true`** (the **Use a graphics card** switch on the Run page). Each replica then runs on OpenMM's CUDA platform, many times faster than on a processor, in an image of its own: the same two lines with `openmm[cuda12]`, built once on each worker that needs it, and larger, as it carries CUDA's libraries. The job goes only to workers that report a graphics card, and waits while none is connected, as one submitted with `--min-gpus 1` does. A replica that finds no card it can use fails, and its log says why, rather than run on the processor at a fraction of the speed the job asked for.
  - **What the worker needs.** An NVIDIA card, and Docker able to hand it to a container. On Windows: Docker Desktop with the WSL 2 backend, and a current NVIDIA driver for Windows, which passes the card through to WSL; nothing is installed inside WSL. On Linux: the NVIDIA driver and the [NVIDIA Container Toolkit](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/install-guide.html), with Docker configured to use it. `docker run --rm --gpus all ubuntu nvidia-smi` showing the card means it is ready.
  - **One task per card.** A worker runs no more tasks that need a card at once than it has cards, whatever its `--slots`; its other slots take work that needs none. A job that only asks for machines with cards, with `--min-gpus`, holds none of them: its tasks take every slot. A laptop with one card and twelve slots runs one replica on the card and up to eleven other tasks beside it. Several small replicas on one card might go faster together, but what limits sharing a card is its memory, so one at a time is the rule until measurements say otherwise.
  - **No thread count to set.** CUDA spreads a replica's work over the card's cores by itself.
  - Without `gpu`, a replica runs on the processor, as before, even on a worker with a card.
- **An Amber system's two files reach each task as `/input/prmtop` and `/input/inpcrd`**, as named inputs of the container job md is (see [Running containers](#running-containers)).
- **Results are not checked by a second worker.** OpenMM's numbers depend on the machine and the number of threads, so two honest replicas with the same seed need not agree to the last digit, and `verify` and `audit_share` refuse md jobs. Checking them by what they mean (energies in range, a structure that held together) is for later.
- **Private jobs**: as with transcode, a sealed PDB file and a job with its key should work; this has not been tried.

### 3D rendering

```sh
cid=$(bin/sisyphusd blob put scene.blend)
bin/sisyphusd job submit --workload render --params "{\"input\":\"$cid\",\"frame_start\":1,\"frame_end\":240}"
bin/sisyphusd job submit --workload render --params "{\"input\":\"$cid\",\"split\":\"tiles\",\"tasks\":8,\"samples\":512}"
```

Each task runs [Blender](https://www.blender.org) 4.5 LTS from its command line, with no window, in a container, on the stored `.blend` file. A job is shared out in one of two ways:

- **By frames** (`"split":"frames"`, the default), for an animation. The frames are cut into as many contiguous runs as the job has tasks, and each task renders its run. The result lists every frame, in order, and a zip file of them all.
- **By tiles** (`"split":"tiles"`), for one large or slow frame. The frame is cut into as many horizontal strips as the job has tasks; each task renders its strip with Blender's border rendering, cropped to the border, and the coordinator puts the strips back together into one PNG.

| Parameter | Meaning |
| --- | --- |
| `input` | The CID of the stored `.blend` file. A task has nothing but this file, so whatever it uses must be packed into it: in Blender, **File → External Data → Pack Resources** before saving. Images that are not are named in the task's log and render pink. Linked libraries are not supported. |
| `frame_start`, `frame_end` | The first and last frame to render, from 0. Either left out is the file's own, which each task reads from the file itself. Split by tiles, the one frame rendered is `frame_start`, or the file's current frame without it. The file's frame step is kept. |
| `engine` | `CYCLES` (default) or `BLENDER_EEVEE_NEXT`. Cycles runs on the processor. EEVEE needs OpenGL, which the image gives it through Mesa's software renderer, llvmpipe: it works with no graphics card, but slowly (below). Whichever is chosen replaces the file's. |
| `samples` | Samples per pixel, in place of the file's: Cycles' render samples, or EEVEE's. More is less noise and longer. |
| `resolution_percent` | The size to render at, as a percentage of the file's resolution, from 1 to 1000. Left out, the file's. |
| `format` | `PNG` (default), 8-bit RGBA, or `EXR`, OpenEXR's floating point, for frames to be graded later. Split by tiles, only PNG. |
| `split` | `frames` or `tiles`. |
| `tasks` | How many runs of frames, or strips. Default: one for each free slot. By frames, never more than there are frames, when the job names both ends; a task renders at most 1,000 frames, and more tasks are made for a longer animation. |

The result:

```json
{"split":"frames","frames":240,"images":["<CID of frame 1>","..."],"archive":"<CID of the zip>","width":1920,"height":1080,"engine":"CYCLES","seconds":5130.2}
{"split":"tiles","frames":1,"image":"<CID of the whole frame>","tiles":8,"width":1920,"height":1080,"engine":"CYCLES","seconds":402.7}
```

`seconds` is the time the tasks spent rendering, added up, without Blender's start. The frames, the zip file and the whole frame are all kept with the result, and the desktop app saves them as `images-000.png` and so on, `archive.zip` and `image.png`.

- **It runs on workers started with `--containers`**, with the image and the command fixed. A worker builds the image the first time it is needed: Debian with the few libraries Blender loads, and Blender's own release for Linux from download.blender.org, checked against the SHA-256 blender.org publishes for it, so a changed download stops the build. That is a 378 MB download, once; the image is 2.1 GB on disk.
- **Python in the file is not run.** A `.blend` file can carry Python that Blender would run on opening it: a text registered to run, a driver's expression. Blender is started with `--disable-autoexec`, and the tests check that a file carrying such a script does not run it. A scene that needs its own Python to render, rigs driven by expressions among them, renders without it.
- **Speed.** On the R620 (two 8-core Xeon E5-2660, no graphics card, shared with CI runners at the time), Blender took 4 to 25 s to start before a task rendered anything, so a task should have more than a few seconds of work. The test scene, a lit cube, rendered with Cycles at 640 by 640 pixels and 128 samples in 14 s for the one frame (18 s with Blender's start), and its four 64 by 64 frames at four samples in 2 s. EEVEE through llvmpipe took 20 to 80 s a frame for those 64 by 64 frames, most of it compiling shaders: it is offered because it works with no graphics card, but Cycles is the faster choice on processors.
- **When to split by tiles.** The strips of one frame are independent, so a frame that takes an hour can take minutes on a pool. Each task still loads the whole scene and builds its BVH, which is a fixed cost for every strip. Cycles' denoiser sees each strip on its own, which can leave a faint seam where strips meet: for tiles, render enough samples to need no denoising, or turn it off in the file. The coordinator holds the strips and the whole frame in memory as it puts them together, so a frame split by tiles may have at most 134 million pixels, 16,384 by 8,192.
- **No MP4.** Putting the frames into a video needs ffmpeg, and the coordinator, which combines results, runs no containers. Fetch the zip and run `ffmpeg -framerate 24 -i frame_%07d.png -pix_fmt yuv420p out.mp4`, or submit the frames to a container job.
- **No graphics card yet.** Cycles uses the processor only: rendering with CUDA or OptiX needs the job to ask for a card and the worker to give one, which follows [#601](https://github.com/sisyphus-network/Sisyphus/issues/601).
- **Results are not checked by a second worker.** Cycles' noise depends on the processor's arithmetic and how the work is shared among threads, so two honest renders need not agree to the bit, and `verify` and `audit_share` refuse render jobs.
- **Licences.** Blender is GPL 2 or later. The image runs Blender's own release, unmodified, as a separate program, which the GPL allows without touching the licence of this repository; what Blender renders belongs to whoever made the scene. Anyone given the image can get Blender's source from blender.org.
- **Private jobs**: as with transcode, a sealed `.blend` file and a job with its key should work; this has not been tried.

### Speech to text

```sh
cid=$(bin/sisyphusd blob put interview.m4a)
bin/sisyphusd job submit --workload transcribe --params "{\"input\":\"$cid\"}"
bin/sisyphusd job submit --workload transcribe --params "{\"input\":\"$cid\",\"language\":\"fr\",\"model\":\"small\",\"segments\":4}"
```

The recording, sound or video, is cut into as many stretches as the job has tasks. Each task transcribes one with [whisper.cpp](https://github.com/ggml-org/whisper.cpp) in a container, and what was said is put together in order: a plain transcript, a line for each thing said, and the same as SRT subtitles, timed from the start of the recording.

| Parameter | Meaning |
| --- | --- |
| `input` | The CID of the stored recording. Anything ffmpeg reads that has sound: WAV, MP3, M4A, Ogg, FLAC, or a video. |
| `language` | The spoken language as a code, such as `en`, `fr` or `de`. Default `auto`: whisper tells, for each stretch on its own. |
| `model` | One of the models below. Default `large-v3-turbo-q5_0`. |
| `segments` | How many stretches. Default: one for each free slot. |

The result is `{"text": "<CID of the transcript>", "subtitles": "<CID of the SRT file>", "segments": <stretches>, "seconds": <length of the recording>, "words": <words heard>}`; the desktop app saves the two as `.txt` and `.srt`.

The models are whisper.cpp's own conversions of OpenAI's Whisper, each pinned by its SHA-256 at one revision of the model repository. Those ending in `.en` understand only English: with them `auto` means `en`, and any other language is refused. The `q5_0` ones are quantised, about a third of the size of the model they are made from, and close to it in accuracy.

| Model | File | Speed on the R620 | When to pick it |
| --- | --- | --- | --- |
| `tiny`, `tiny.en` | 78 MB | 0.5× base | A quick draft, or a weak machine. Mishears often. |
| `base`, `base.en` | 148 MB | 1× (24 s for 13 s of speech) | Clear speech in a common language, where speed matters more than every word. |
| `small`, `small.en` | 488 MB | 4.5× | Superseded by `large-v3-turbo-q5_0`, which was quicker and more accurate here. |
| `large-v3-turbo-q5_0` | 574 MB | 3.5× | **The default.** It heard the test clip word for word, as the large models do, in less time than `small`. |
| `large-v3-q5_0` | 1.08 GB | 5.2× | The most accurate model, at a third of the size and with little of the accuracy lost. For hard audio, names and less common languages. |
| `medium`, `medium.en` | 1.53 GB | unmeasured, slower than `small` | Older; `large-v3-turbo` is usually better for the time. |
| `large-v3-turbo` | 1.62 GB | unmeasured, a little slower than its `q5_0` | The turbo model unquantised. |
| `large-v1`, `large-v2`, `large-v3` | 3.1 GB | unmeasured, slower than `large-v3-q5_0` | The full large models. `large-v2` is sometimes steadier than v3 on long or noisy recordings; v1 is there for comparison. |

The speeds come from one task transcribing the same 13 seconds of espeak-ng speech with each model, 8 threads each, while the R620 (two Xeon E5-2660, which have AVX but no AVX2) was busy with CI. They are a guide to how the models compare, not a benchmark. `large-v3-turbo-q5_0` is the default because on that clip `base` got about one word in four wrong, while the turbo model got none wrong in 3.5 times as long, and splitting spreads that time across the pool. The cost is fetching 574 MB on each worker once instead of 148 MB. Name `base` for speed.

- **It runs on workers started with `--containers`**, with the image and the script fixed. There is an image for each model, built only when a job asks for that model, so a worker never fetches a model it is not asked to run. A worker builds the one a job needs the first time, which takes the network once and some minutes: whisper.cpp (release `v1.9.5`, at a pinned commit) is compiled from source for that worker's processor, at the lowest priority, and the model is fetched from the [whisper.cpp model repository](https://huggingface.co/ggerganov/whisper.cpp) at a pinned revision and checked against its SHA-256. The rest is the transcoding image (Alpine and its ffmpeg), so a worker that has both keeps ffmpeg once, plus jq. Only the model's lines differ between the images, so every model's image shares the compiled whisper.cpp and ffmpeg layers, and a second model costs its own file and nothing more.
- **Image size**: the layers every model shares are about 38 MB of content; the rest is the model. As Docker reports them here (on disk, then content): `tiny` 343 MB (123 MB), `base` 476 MB (186 MB), `small` 1.13 GB (500 MB), `large-v3-turbo-q5_0` 1.3 GB (587 MB), `large-v3-q5_0` 2.31 GB (1.09 GB).
- **Where stretches meet.** Each cut is moved from where an even share would end to the middle of the nearest quiet moment (ffmpeg's `silencedetect`, below -35 dB for 0.3 s or more) within a quarter of a share or 5 seconds, whichever is less. The tasks on either side of a cut find the same place from the same recording, so nothing is heard twice or missed. Speech with no pause near a cut is cut where it is, and a word there may be split or lost; fewer, longer stretches make that rarer.
- **Speed.** whisper uses up to 8 threads in each task. On the R620 (two Xeon E5-2660, no AVX2), busy with other work at the time, one task took 108 s for 67 s of speech with `base` and 363 s with `small`; the table above compares the models. The gain from cutting is across machines, each transcribing its stretch at the same time. Every worker fetches the whole recording.
- **Accuracy** is whisper's: good for clear speech, weaker for names, crowds, music and accents its training saw little of. What it marks as not speech, such as `[BLANK_AUDIO]`, is left out. With `auto`, each stretch is identified on its own, so a short stretch may be taken for another language; name it when you know it. No translation, and no telling speakers apart.
- **Results are not checked by a second worker.** whisper's sums come out a little differently on different processors and thread counts, so `verify` and `audit_share` refuse transcribe jobs.
- **CPU only, for now.** A worker's graphics card is not used.
- **Licences**: whisper.cpp and the models it converts from OpenAI's Whisper are MIT; ffmpeg as Alpine packages it is GPL.
- **Private jobs**: as with transcode, a sealed recording and a job with its key should work; this has not been tried.

### Quantum chemistry

```sh
cid=$(bin/sisyphusd blob put conformers.xyz)
bin/sisyphusd job submit --workload qchem --params "{\"input\":\"$cid\"}"
bin/sisyphusd job submit --workload qchem --params "{\"input\":\"$cid\",\"job\":\"ohess\",\"solvent\":\"water\",\"tasks\":8}"
```

The input is one stored file of many structures: the conformers of a molecule, or a library of molecules. They are cut into as many contiguous runs as the job has tasks, and each task runs [xtb](https://github.com/grimme-lab/xtb), the Grimme group's semiempirical tight-binding program, on each structure of its run in turn, in a container. The structures need nothing from each other, so the work spreads over the pool as it is. A file of one molecule is one task.

| Parameter | Meaning |
| --- | --- |
| `input` | The CID of the stored structures. An XYZ file of one or more structures one after another (a count line, a title line, then an element and three coordinates in ångström for each atom; blank lines between structures are fine), or an SDF file of V2000 molfiles, each ending at `$$$$`. Of a molfile only the title, the atoms' elements and their coordinates are read; bonds, charges and data fields are not. V3000 molfiles, SMILES and other formats are refused: convert them to XYZ or SDF first, with Open Babel or RDKit. |
| `method` | `gfn2` (GFN2-xTB, the default and the usual choice), `gfn1` (GFN1-xTB), `gfn0` (GFN0-xTB, non-self-consistent and quicker), or `gfnff` (the GFN-FF force field, for large systems; it gives no orbitals, so no gap or dipole). |
| `job` | `sp`, the energy at the geometry given; `opt` (the default), the geometry optimised; `hess`, the vibrational frequencies and thermochemistry at the geometry given; `ohess`, the geometry optimised and then its frequencies. |
| `charge` | Each structure's total charge, from -50 to 50. Default 0. The same for every structure: a library of molecules with different charges is submitted as one job for each charge. |
| `uhf` | How many electrons are unpaired, from 0 to 50: 0 for a closed shell, 1 for a radical, 2 for a triplet. Default 0. |
| `solvent` | `none` (the default) for the gas phase, or a solvent for xtb's ALPB implicit solvation: `acetone`, `acetonitrile`, `aniline`, `benzaldehyde`, `benzene`, `ch2cl2`, `chcl3`, `cs2`, `dioxane`, `dmf`, `dmso`, `ethanol`, `ether`, `ethylacetate`, `furane`, `hexadecane`, `hexane`, `methanol`, `nitromethane`, `octanol`, `phenol`, `thf`, `toluene`, `water`, `woctanol` (wet octanol). These are the names xtb 6.7.1 has ALPB parameters for with GFN2-xTB, GFN1-xTB and GFN-FF alike. GFN0-xTB has none of its own (xtb would quietly use GFN-FF's), so a solvent with `gfn0` is refused. |
| `convergence` | How tightly to optimise: `crude`, `sloppy`, `loose`, `lax`, `normal` (the default), `tight`, `vtight` or `extreme`, xtb's own levels. |
| `tasks` | How many runs of structures. Default: one for each free slot. Never more than there are structures. |

The result:

```json
{"method":"gfn2","job":"opt","structures":3,"converged":3,"lowest":0,"lowest_energy_hartree":-13.665127723998,
 "csv":"<CID of the table>","xyz":"<CID of the optimised geometries>",
 "results":[{"index":0,"title":"butane anti","energy_hartree":-13.665127723998,"relative_kcal_mol":0,"gap_ev":13.8028,"dipole_debye":0,"converged":true,"geometry":"<CID, XYZ>"},
            {"index":1,"title":"butane gauche+","energy_hartree":-13.664176235431,"relative_kcal_mol":0.597,"gap_ev":14.2419,"dipole_debye":0.058,"converged":true,"geometry":"<CID, XYZ>"}]}
```

For each structure, in the file's order: its index from 0 and its title line; the total energy in hartree; how far above the lowest it is in kcal/mol, which ranks conformers; the HOMO-LUMO gap in eV and the dipole moment in debye, from the xTB methods; whether xtb finished (an optimisation that converged, or an energy found); and, for `opt` and `ohess`, the CID of its optimised geometry as XYZ. For `hess` and `ohess` it also gives the Gibbs free energy at 298.15 K in hartree, how many frequencies are imaginary (none at a true minimum), and the CID of a text file of the frequencies in cm⁻¹ with their infrared intensities. `lowest` is the structure of lowest energy among those that converged. `csv` is a table of all of it, a row for each structure, and `xyz` the optimised geometries one after another, each titled with its title and energy, which Avogadro, PyMOL, VMD and Open Babel read. The desktop app saves them as `.csv`, `.xyz`, and each structure's as `results-<n>-geometry.xyz` and `results-<n>-frequencies.txt`.

- **It runs on workers started with `--containers`**, with the image and the script fixed. A worker builds the image the first time it is needed: Alpine with jq, and xtb's statically linked release for Linux (6.7.1) from GitHub, checked against its SHA-256, with its parameter files and licence texts (fetched from the release's commit and checked too). That is a 27 MB download, once; the image is 97 MB on disk (24 MB of content). xtb is released for x86-64 alone: on an ARM worker the build stops with a message saying so.
- **A structure xtb fails on fails alone.** It is reported with `"converged": false` and an `error` saying how xtb exited, and the task goes on to the next structure; the job fails only if a task cannot run at all. An optimisation that does not converge reports the last geometry it reached. xtb's own output is not kept; where a structure failed, its last lines are in the task's log.
- **Speed.** Each structure runs on one thread, and a task's structures one after another, so a worker with *n* slots computes *n* structures at once. On the R620 (two 8-core Xeon E5-2660, no AVX2), with GFN2-xTB: butane (14 atoms) took 0.04 s for its energy, 0.07 s to optimise and 0.8 s for `ohess`; n-icosane (62 atoms) 0.15 s, 1.5 s and 34 s, where GFN-FF took 0.03 s, 0.07 s and 0.9 s. Each task also starts a container, about a second, so a task should have more than a handful of small molecules. The frequencies of large molecules with GFN2-xTB are the slow part.
- **Not checked by a second worker, yet.** On one machine, xtb gives the same bits every time: two runs of a task leave byte-identical outputs, and the job's result is byte-identical however it is shared out (the real-image test checks this with one, two and three tasks). Each task runs xtb on one thread, with Intel MKL held to the same arithmetic on every processor (`MKL_CBWR=COMPATIBLE`), and leaves no timings. But xtb's release is built with Intel's compiler, which picks its own arithmetic by the processor, and run under emulated processors the last digits xtb prints differed: with AVX2 and FMA (Haswell and later) or on AMD's EPYC, a HOMO-LUMO gap in its eleventh decimal, a free energy in its fourteenth, a coordinate in its tenth, while the total energies to twelve decimals and the frequencies agreed. Two honest workers on different processors would therefore disagree, so the workload says its results vary, and `verify` and `audit_share` refuse qchem jobs. Without `MKL_CBWR`, even two generations of Intel processor without AVX2 disagreed. A build of xtb whose arithmetic does not depend on the processor would make qchem the first scientific workload a pool can check exactly; that is for later.
- **Limits.** At most 1,000,000 structures of at most 20,000 atoms each, in at most 1 GiB. One charge and spin for the whole file. No constraints, scans, transition-state searches, conformer generation (CREST), excited states or periodic systems. Thermochemistry is at 298.15 K and 1 atm, xtb's defaults.
- **Licence and citation.** xtb is LGPL 3.0 or later. The image runs xtb's own release, unmodified, as a separate program, which the LGPL allows without touching the licence of this repository; its licence texts are in the image under `/usr/local/share/licenses/xtb`, and its source is on GitHub. The xtb authors ask that work using it cite C. Bannwarth, E. Caldeweyher, S. Ehlert, A. Hansen, P. Pracht, J. Seibert, S. Spicher and S. Grimme, *WIREs Comput. Mol. Sci.* **2021**, 11, e1493 ([doi:10.1002/wcms.1493](https://doi.org/10.1002/wcms.1493)), and the paper of the method used: GFN2-xTB, *J. Chem. Theory Comput.* **2019**, 15, 1652 ([doi:10.1021/acs.jctc.8b01176](https://doi.org/10.1021/acs.jctc.8b01176)); GFN1-xTB, *J. Chem. Theory Comput.* **2017**, 13, 1989 ([doi:10.1021/acs.jctc.7b00118](https://doi.org/10.1021/acs.jctc.7b00118)); GFN0-xTB, *ChemRxiv* 2019 ([doi:10.26434/chemrxiv.8326202.v1](https://doi.org/10.26434/chemrxiv.8326202.v1)); GFN-FF, *Angew. Chem. Int. Ed.* **2020**, 59, 15665 ([doi:10.1002/anie.202004239](https://doi.org/10.1002/anie.202004239)); and with a solvent, ALPB, *J. Chem. Theory Comput.* **2021**, 17, 4250 ([doi:10.1021/acs.jctc.1c00471](https://doi.org/10.1021/acs.jctc.1c00471)). The full list is in [xtb's README](https://github.com/grimme-lab/xtb#citations).
- **Why not Gaussian.** Gaussian is the program many chemists learn on, but its licence forbids running it on machines other than those it is licensed for, so a pool of other people's computers cannot offer it. xtb is free and open, quick on ordinary processors, and good enough for what a pool does best: screening many conformers or molecules, and preparing geometries for a higher level of theory. Psi4 (LGPL) and PySCF (Apache 2.0), for DFT and wavefunction methods, are the open programs to follow ([#584](https://github.com/sisyphus-network/Sisyphus/issues/584)).
- **Private jobs**: as with transcode, a sealed file of structures and a job with its key should work; this has not been tried.

### Text from scans

```sh
cid=$(bin/sisyphusd blob put scanned.pdf)
bin/sisyphusd job submit --workload ocr --params "{\"input\":\"$cid\"}"
bin/sisyphusd job submit --workload ocr --params "{\"input\":\"$cid\",\"language\":\"eng+heb\",\"output\":\"text+pdf\",\"dpi\":400,\"tasks\":16}"
```

[Tesseract](https://github.com/tesseract-ocr/tesseract) reads the text in a scan. A PDF's pages are shared out across the pool: each task counts the pages with `pdfinfo` and takes the contiguous run of them that is its share, turns each page into a picture with `pdftoppm` and reads it, one page at a time, and the coordinator puts the pages' text together in order. A single picture is read by one task. Every page can also be made into a searchable PDF: the scan as it was, with the text laid invisibly over it, to be searched and copied from.

| Parameter | Meaning |
| --- | --- |
| `input` | The CID of the stored scan: a PDF, or a PNG, JPEG or TIFF picture. A TIFF may have several pages, which the one task reads in order. What the file is, is read from its first bytes when the job is split, and anything else is refused then. |
| `language` | The text's language, as Tesseract names it: `eng` (the default), `spa`, `fra`, `deu`, `heb`, `ara`, `rus` or `pol`, the desktop app's seven languages and German. Several joined with `+`, such as `eng+heb`, for a page that mixes them; each one added makes reading slower. |
| `output` | `text` (the default), `pdf`, or `text+pdf`. |
| `dpi` | Dots per inch to turn a PDF's pages into pictures at, from 72 to 600. Default 300, which suits most print; small print reads better at 400 to 600, which is slower. A picture is read at its own. |
| `tasks` | How many runs of pages a PDF is shared out as. Default: one for each free slot. A task left no pages, where there are more tasks than pages, finishes at once. A picture is always one task. |

The result:

```json
{"pages":3,"text":"<CID of the text>","pdf":"<CID of the searchable PDF>","characters":55,"language":"eng","tasks":1}
{"pages":120,"text":"<CID of the text>","pdf":"","pdfs":["<CID of pages 1 to 8>","..."],"archive":"<CID of a zip of them>","characters":291404,"language":"eng+heb","tasks":16}
```

`text` is plain UTF-8, the pages in order with a form feed (`\f`) between each and the next, empty unless text was asked for. `characters` counts what was read, not counting spaces and line breaks. The desktop app saves the text as `text.txt`, a whole searchable PDF as `pdf.pdf`, and the parts as `pdfs-0.pdf` and so on, with `archive.zip`.

- **The searchable PDF of a job split among several tasks comes in parts.** Each task puts its own pages together with Poppler's `pdfunite`, so a part is every page of its run, in order, but the pool's coordinator runs no containers, and so puts no PDFs together itself. The result lists the parts in page order, `pdfs`, and a zip of them named by their pages, such as `pages-001-008.pdf`, padded so that they sort in order. `pdf` is the whole document only when one task read every page: ask for `"tasks":1` to have it whole, at the cost of reading on one processor, or join the parts with `pdfunite pages-*.pdf whole.pdf` or a `container` job. The pages are kept as the scan was, in colour, as JPEG at quality 90 at the `dpi` asked for: the test's short pages came to about 170 kB each at 300 dpi.
- **What Aggregate trusts.** It takes only `summary.json`, `text.txt` and `text.pdf` from each task, by those names; any other file a task leaves is not looked at. It checks that the tasks read every page once, in order, from the same scan, with the same settings, and that each task's text has as many pages as it says it read, before it believes any of it. The names in the zip file are made by the coordinator from those checked page numbers, never taken from a worker.
- **It runs on workers started with `--containers`**, with the image and the script fixed. A worker builds the image the first time it is needed, from Alpine 3.20's own packages: `tesseract-ocr` (Tesseract 5.3.4 with Leptonica 1.84), the data for the eight languages (`tesseract-ocr-data-*`, Tesseract's standard models, which have the LSTM and the older engine), and `poppler-utils` (Poppler 24.02). Alpine builds Tesseract with OpenCL, which times the machine's devices before it first reads anything and keeps what it found in the directory it runs in; the image does that once as it is built, so that no task spends time on it or needs to write anything outside `/output`.
- **Image size**: 251 MB on disk, 76 MB of content, of which the language data is 118 MB on disk. On the R620 it built in 48 s. Each further language would be another 2 to 25 MB.
- **Speed.** Each task reads its pages one after another, with one thread, since a worker runs as many tasks at once as it has slots, one for each processor by default; the gain is in running many pages at once across the pool. On the R620 (two 8-core Xeon E5-2660, 32 threads, shared with CI runners), a dense page of print, 52 lines of 11-point text, took about 15 s in one task at 300 dpi, the container's start included, so 32 such pages would take about 8 minutes in one task; read as 32 tasks at once on the same machine they took 27 s. The test's short pages, a line each, took 2 to 3 s a page.
- **Results are not checked by a second worker.** Tesseract reads the same picture the same way on one machine, and text-only results agreed byte for byte here between runs and between splits. But the searchable PDF carries the time it was made, so no two are the same bytes; each worker builds the image from Alpine's packages as they are on that day, so two workers can have different releases; and Tesseract and Poppler reckon in floating point, which differs a little between kinds of processor. `verify` and `audit_share` refuse ocr jobs rather than take honest workers for wrong ones.
- **Accuracy** is Tesseract's: very good for clean print at 300 dpi, weaker for poor scans, small print at low dpi, handwriting, and pages with complex layout such as tables or many columns, which come out as text in reading order as best Tesseract can tell. A PDF that already has a text layer is read from its pictures all the same: its own text is not used.
- **Limits.** A PDF protected by a password to open it cannot be read; one with only an owner password can. A task may leave at most 64 MiB of text. Every task fetches the whole scan, as with the other workloads that split one input.
- **Licences.** Tesseract and its language data are Apache-2.0, and Leptonica is BSD-2-Clause. Poppler is GPL 2 or 3; the image runs Alpine's build of its utilities, unmodified, as separate programs in a container, which the GPL allows without touching the licence of this repository. What is read from a scan belongs to whoever owns the scan.
- **Private jobs**: the coordinator reads the first eight bytes of the scan, with the job's key, to tell a PDF from a picture; a sealed scan and a job with its key should work, as with transcode, but this has not been tried.

### Amber system preparation

```sh
pdb=$(bin/sisyphusd blob put protein.pdb)
bin/sisyphusd job submit --workload prep --params "{\"input\":\"$pdb\"}"
bin/sisyphusd job submit --workload prep --params "{\"inputs\":[\"$a\",\"$b\",\"$c\"],\"forcefield\":\"ff14SB\",\"water\":\"tip3p\",\"box\":12,\"box_shape\":\"box\",\"salt_mm\":150,\"tasks\":3}"
```

[tleap](https://ambermd.org/AmberTools.php), AmberTools' program for building systems, makes each PDB file into a system Amber's force fields describe: it adds the hydrogens the file lacks from its residue templates, puts the molecule in a box of water, replaces waters with sodium or chloride ions until the system is neutral, and with pairs of both for any salt asked for, and saves the Amber topology and coordinates (prmtop and inpcrd) and a PDB file of the whole system. Each file is one system, and several are shared out across the pool, a contiguous run of them to each task, files `n*i/k` to `n*(i+1)/k-1` of `n` for task `i` of `k`; a task is given only its own. The force field, the water, the box and the salt are chosen from a fixed menu, and the worker writes tleap's commands itself (`packages/runtime/prep.go`, `sources` and `solvate`); no tleap commands are taken from a job.

To simulate a prepared system and analyse it, chain the three:

1. `prep` with the PDB file. Each of the result's `systems` has a `prmtop` and an `inpcrd`.
2. `md` with `{"prmtop": <that prmtop>, "inpcrd": <that inpcrd>}`. It runs the system as it is, in its box, with PME, the force field and water from the topology; each replica leaves a DCD `trajectory`.
3. `traj` with `{"topology": <the same prmtop>, "trajectory": <a replica's trajectory>}`, or several replicas as `trajectories`.

The real-image test does just that: it prepares alanine dipeptide in OPC water, simulates it for 0.1 ps with `md`, and finds the RMSD and radius of gyration of its five frames with `traj`.

| Parameter | Meaning |
| --- | --- |
| `input` | The CID of one stored PDB file, the system to prepare. |
| `inputs` | In place of `input`, the CIDs of up to 64, each a system of its own, prepared and listed in that order. |
| `forcefield` | The proteins' force field: `ff19SB` (the default), Amber's newest, made for OPC water, or `ff14SB`, made for TIP3P. Nucleic acids are given OL21 for DNA and OL3 for RNA whichever it is. |
| `water` | `opc`, `tip3p`, or `none` for the molecule alone in a vacuum, with no box and no ions. Default: the one the force field is made for, `opc` for ff19SB and `tip3p` for ff14SB. |
| `box` | How far the box's walls are from the molecule, at least, in Å, from 3 to 50. Default 10. |
| `box_shape` | `octahedron` (the default), a truncated octahedron, which holds the same molecule in less water, or `box`, a rectangular box. |
| `salt_mm` | Sodium chloride beyond the ions that make the system neutral, in mM, from 0 to 1000. Default 0, neutral only. The pairs are as many as there would be in the box's water at that concentration, `salt_mm/1000 × waters/55.5`, rounded; not with `water` `none`. |
| `tasks` | How many runs of systems to share them out as, at most one for each system. Default: one for each free slot. A task is given at most 16 systems, so a job of many is shared out in at least enough tasks for that. |

As tleap's commands, with `<water>` `OPCBOX` or `TIP3PBOX`:

```
source leaprc.protein.ff19SB          (or leaprc.protein.ff14SB)
source leaprc.DNA.OL21
source leaprc.RNA.OL3
source leaprc.water.opc               (or leaprc.water.tip3p; none for no water)
mol = loadpdb /input/system-000.pdb
charge mol
solvateoct mol <water> 10             (or solvatebox)
addionsrand mol Cl- 0                 (Na+ for a negative molecule; none for a neutral one)
addionsrand mol Na+ 5 Cl- 5           (the salt, if asked for)
charge mol
saveamberparm mol /output/system-000.prmtop /output/system-000.inpcrd
savepdb mol /output/system-000.pdb
```

tleap is run twice for each system: first as far as the water, to learn the molecule's charge, which chooses the ion that neutralises it, and how many waters fill the box, which sets the salt; then to make it.

The result:

```json
{"forcefield":"ff19SB","water":"opc","box_angstrom":10,"box_shape":"octahedron","salt_mm":150,"systems":[{"index":0,"prmtop":"<CID>","inpcrd":"<CID>","pdb":"<CID>","atoms":7728,"residues":1921,"waters":1903,"ions":{"Cl-":7,"Na+":5},"salt_pairs":5,"box":{"a":44.5757152,"b":44.5757152,"c":44.5757152,"alpha":109.471219,"beta":109.471219,"gamma":109.471219},"charge_before":2,"charge_after":0},{"index":1,"error":"tleap does not know the residues LIG: ...","log":"<CID>"}],"prepared":1,"failed":1,"archive":"<CID>","tasks":2}
```

Each system has its three files; its atoms, residues, waters and ions, of which `salt_pairs` pairs are salt; its box's edges in Å and angles in degrees; and its charge as the PDB file has it and after the ions, in elementary charges. A count of none is left out. `archive` is a zip file of every system's files, named by its index, `system-000.prmtop`, `system-000.inpcrd`, `system-000.pdb`, and of the log of each that failed, `system-001.log`. The desktop app saves each as `.prmtop`, `.inpcrd`, `.pdb` and `.txt`.

- **Standard residues only.** tleap knows what its libraries hold: the standard amino acids, with histidine as HIE unless the file names HID or HIP, terminal residues, ACE and NME caps, DNA and RNA nucleotides, water and the common ions. A ligand, a cofactor, a modified residue or a metal centre needs parameters of its own, made with antechamber and GAFF or by hand, and that is out of `prep`'s scope. A system with a residue tleap does not know fails, plainly: `tleap does not know the residues LIG: prep makes systems of standard amino acids and nucleotides, water and common ions only, ...`, with the lines of tleap's log that say so as its `log`. The other systems of the job are prepared all the same; a job none of whose systems is prepared fails with the first one's reason. Atoms a residue's template lacks, such as hydrogens named otherwise, fail the system too, with tleap's log. With `water` `none`, the file's own waters and ions are not known either: take them out, or give a water.
- **A PDB file is data.** It is loaded with `loadpdb`, which reads it as coordinates, never with `source`, which would run it as commands. The coordinator reads each file through when the job is split, and refuses one over 128 MiB, with a line over 1,024 characters or with anything but printable ASCII in it, with no `ATOM` or `HETATM` record, or with more than one `MODEL`: give each model of an NMR ensemble as a file of its own. The script checks the file again before tleap reads it.
- **What a job cannot do.** tleap's commands can run programs (`system`) and read other files of commands (`source`), so a job is not given them. Each setting is one of the menu's words or a number the coordinator checks against its bounds and writes itself, and the worker checks them again, writes the commands itself, refuses any input but the job's PDB files by their fixed names, `system-000.pdb` and on, and runs only the image and script of its own making, whatever a payload says. tleap is started with `-s`, so that it reads no startup file, with AmberTools' own directories as its search path, in a working directory of the task's own under `/output`, which the script alone writes to.
- **What Aggregate trusts.** It takes only `summary.json` from each task, and the files `system-NNN.prmtop`, `system-NNN.inpcrd`, `system-NNN.pdb` and `system-NNN.log` of the systems it says it prepared, by those names, made from indexes it has checked; any other file a task leaves is not looked at. It checks that every system was prepared, or failed, once, in order, with the same settings, as the coordinator would have given them; that each count is a whole number, at most ten million atoms, with no more residues than atoms and no more waters and ions than residues; that the charges are plain numbers, and the charge after neutralising within 0.01 of nought; that the box is a truncated octahedron's or a rectangular box's as asked, of edges up to 10,000 Å; that a system in no water was given nothing but hydrogens; that each file is there, from 1 byte to 2 GiB, the topology begins as an Amber topology, the PDB file as a PDB file, and the coordinates with as many atoms as the summary says; and that a failure is one of four reasons, with residues of at most four letters, digits or `+_*-`. The names in the zip file are the coordinator's.
- **It runs on workers started with `--containers`**, with the image and the script fixed. A worker builds the image the first time it is needed: the same AmberTools 25.3 build as `traj`'s, installed with micromamba 2.0.5 on Debian 12 in a first stage that is the same as `traj`'s, so a worker that has built one has that stage for the other, of which only tleap's program `teLeap`, the 34 libraries it loads, its force fields, libraries and commands (`dat/leap`) and AmberTools' licence are copied onto `debian:12.12-slim`, with Debian's `libfaketime` 0.9.10-2.1. It is an image of its own beside `traj`'s rather than `traj`'s made larger, so that a worker that only analyses trajectories need not rebuild its image or carry tleap's force fields.
- **Image size**: 243 MB on disk, 59 MB of content.
- **Speed.** tleap runs on one thread. The test's two peptides, of 7,728 and 2,806 atoms with their water, were each prepared in about a second, the container's start included. A large protein in water takes longer, and has not been timed here.
- **The same bytes, however it is shared out.** tleap picks the waters `addionsrand` makes into ions at random, seeded from the clock, and stamps each topology with the date. The script runs tleap with `libfaketime` holding the clock at 2000-01-01 00:00:00, so that the same file gives the same bytes every time: the test checks that two systems prepared in one task and in two give the same files and the same zip file, and each topology's `%VERSION` line says `DATE = 01/01/00  00:00:00`. Turning off the processor's AVX code in the C library's mathematics (`GLIBC_TUNABLES=glibc.cpu.hwcaps=-AVX,-AVX2,-FMA`) on the R620, a Xeon E5-2660 without FMA, changed no byte either.
- **Results are not checked by a second worker.** Each worker builds the image from conda-forge as it is that day, so two workers may run different builds of the libraries tleap loads, and the C library picks its mathematics by the processor, FMA among it, which the R620 has not got to compare; whether that moves a coordinate's seventh decimal place between machines has not been measured, so `verify` and `audit_share` refuse prep jobs rather than risk taking honest workers for wrong ones.
- **Limits.** At most 64 systems a job, and 16 a task; a PDB file of at most 128 MiB; a box padding from 3 to 50 Å, and salt up to 1 M. `md` takes one system a job, so a job of several systems is followed by an `md` job for each.
- **Licences.** AmberTools as conda-forge packages it is GPL 3, LGPL 3, BSD-3-Clause and MIT, and its force field files are AmberTools'; the image runs tleap unmodified, as a separate program in a container, and keeps AmberTools' licence in `/opt/leap/share/licenses`. libfaketime is GPL 2. The test's peptide, `testdata/amber/kakeak.pdb`, was built for it with tleap's `sequence` command.
- **Private jobs**: the coordinator reads each PDB file through with the job's key when it is split; a sealed file and a job with its key should work, as with transcode, but this has not been tried.

### Trajectory analysis

```sh
top=$(bin/sisyphusd blob put system.parm7)
dcd=$(bin/sisyphusd blob put production.dcd)
bin/sisyphusd job submit --workload traj --params "{\"topology\":\"$top\",\"trajectory\":\"$dcd\"}"
bin/sisyphusd job submit --workload traj --params "{\"topology\":\"$top\",\"trajectory\":\"$dcd\",\"analyses\":\"rmsd,radgyr,distance,hbond,dssp\",\"mask\":\":1-76@CA\",\"mask1\":\":10\",\"mask2\":\":42\",\"tasks\":16}"
bin/sisyphusd job submit --workload traj --params "{\"topology\":\"$top\",\"trajectories\":[\"$run1\",\"$run2\",\"$run3\"],\"analyses\":\"rmsd,hbond\"}"
```

[cpptraj](https://github.com/Amber-MD/cpptraj), AmberTools' trajectory program, analyses what a molecular dynamics run left behind. The analyses are chosen from a fixed menu, and the job gives only the menu's names and Amber masks: the cpptraj commands are written by the worker's own code (`packages/runtime/traj.go`, `actions`), never taken from the job. One trajectory is shared out across the pool by its frames: each task counts them with `cpptraj -tl` and takes the contiguous run that is its share, frames `n*i/k+1` to `n*(i+1)/k` of `n` for task `i` of `k`, read with `trajin <file> <first> <last>`. Several trajectories are shared out whole, a contiguous run of them to each task, and a task is given only its own. The coordinator puts each frame's results back together in order, and the result is the same bytes however the job was shared out: every frame is fitted onto the first frame of the first trajectory, which each task loads as its `reference`, not onto the first frame of its own run.

It pairs with `md`, and `md` with `prep` (see [Amber system preparation](#amber-system-preparation)): an `md` job's replicas each leave a DCD trajectory, which `traj` reads with the `md` job's topology. For an Amber system simulated from a prmtop and inpcrd, that is the prmtop; for a molecule given as a PDB file, `md` adds hydrogens and water, so the topology is the replica's `final` PDB, whose atoms are in the trajectory's order. That pairing was tried by hand with OpenMM's villin headpiece in water, 8,867 atoms, simulated as `md` does: cpptraj read the PDB OpenMM wrote and its DCD, took its box from the trajectory, and found the protein's hydrogen bonds and secondary structure with water left out. Several replicas can be given together as `trajectories`.

| Parameter | Meaning |
| --- | --- |
| `topology` | The CID of the stored topology: an Amber topology (prmtop or parm7), or a PDB file of the system with its atoms in the trajectories' order. What it is, is read from its first bytes when the job is split, and anything else is refused then. |
| `trajectory` | The CID of one stored trajectory: DCD (CHARMM's and OpenMM's, as `md` writes), NetCDF (Amber's, as `pmemd` and `sander` write with `ioutfm=1`), or Amber's text mdcrd. Its kind is read from its first bytes too. |
| `trajectories` | In place of `trajectory`, the CIDs of up to 256, in the order their frames are put together. |
| `analyses` | Some of `rmsd`, `radgyr`, `distance`, `hbond`, `dssp` and `rmsf`, joined with commas, each once; default `rmsd,radgyr`. They are run, and listed, in that order whatever order they are given in. |
| `mask` | The Amber mask of the atoms RMSD, RMSF and the radius of gyration are taken over. Default `@CA`, the alpha carbons. |
| `mask1`, `mask2` | For `distance`, and only for it, the masks of the two groups whose centres of mass are measured apart. |
| `region` | The mask of the atoms hydrogen bonds and secondary structure are looked for in. Default `*`, which cpptraj takes as the whole solute: water and ions are left out. |
| `tasks` | How many runs of frames a trajectory is shared out as, or runs of trajectories where there are several, at most one task for each. Default: one for each free slot. A task left no frames finishes at once. A job that asks for `rmsf` is one task. |

The analyses, as cpptraj's commands:

| Analysis | What it finds | cpptraj |
| --- | --- | --- |
| `rmsd` | Each frame's RMSD from the first frame of the first trajectory, in Å, after fitting `mask`'s atoms onto it. The frames themselves are left where they were. | `rms RMSD ref [first] <mask> nomod` |
| `radgyr` | Each frame's radius of gyration of `mask`'s atoms, unweighted, as cpptraj gives it by default, in Å. | `radgyr RADGYR <mask> nomax` |
| `distance` | Each frame's distance between the centres of mass of `mask1` and `mask2`, in Å, through the periodic box if the trajectory has one. | `distance DISTANCE <mask1> <mask2>` |
| `hbond` | How many hydrogen bonds `region`'s atoms make in each frame, and each bond with how many frames it was made in, by cpptraj's default distance (3.0 Å) and angle (135°). | `hbond HBOND <region> avgout` |
| `dssp` | Each residue's secondary structure in each frame, by DSSP. | `secstruct DSSP <region>` |
| `rmsf` | Each residue's RMSF, the mass-weighted mean of its atoms' root-mean-square fluctuations about their mean positions, over every frame, after fitting each onto the first. | `rms FIT ref [first] <mask>`, `atomicfluct RMSF <mask> byres` |

The result:

```json
{"frames":60,"trajectories":1,"trajectory_frames":[60],"analyses":["rmsd","radgyr","distance","hbond","dssp"],"series":"<CID>","rmsd_angstrom":{"mean":0.1635,"stdev":0.0242,"min":0,"max":0.1891},"radgyr_angstrom":{...},"distance_angstrom":{...},"hbonds_per_frame":{...},"hbonds":"<CID>","dssp":"<CID>","dssp_residues":"<CID>","structure":{"alpha":0.41,"none":0.32,"turn":0.12,...},"tasks":4}
```

`series` is a CSV table with a row for each frame: `frame`, numbered from 1 through all the trajectories, `trajectory`, which one it is from, numbered from 0, and a column for each of `rmsd`, `radgyr`, `distance` and `hbonds` that was asked for, at six decimal places; each is summed up as its mean, standard deviation, least and greatest. `hbonds` is a CSV table of each hydrogen bond, `acceptor,donor_hydrogen,donor,frames,fraction`, most often made first. `dssp` is a CSV table of each residue's DSSP letter in each frame (`-` none, `E` extended, `B` bridge, `G` 3-10 helix, `H` alpha helix, `I` pi helix, `T` turn, `S` bend), `dssp_residues` the fraction of the frames each residue spent in each, and `structure` the fraction of all residues' frames. `rmsf` is a CSV table of `residue,rmsf`. The desktop app saves each table as `series.csv`, `hbonds.csv`, `dssp.csv`, `dssp_residues.csv` and `rmsf.csv`.

- **RMSF is found in one task.** A residue's RMSF is about its mean position over every frame, which no task finds from its own frames. It could be put together from each task's sums of positions and of their squares, but cpptraj writes neither, and RMSF from its rounded per-task results would not be the bytes one task gives. So a job that asks for `rmsf` runs whole in one task, reading every frame, up to 15 trajectories; ask for it in a job of its own to keep the others shared out. cpptraj reads frames quickly, and the fit and fluctuation are cheap beside a simulation.
- **No clustering.** Clustering compares every frame with every other, or with centres found from all of them, so it does not share out by frames, and run in one task it would gain nothing from the pool. It is left out rather than offered as something the pool cannot speed up.
- **What a job cannot do.** cpptraj's input language can read and write files wherever the task may (`readinput`, `trajout`, `writedata`, `parmwrite` among its commands), so a job is not given it. The analyses are a menu, and a mask is checked against an allow-list before it is put in a command: it begins with `:`, `@`, `^`, `!`, `(` or `*`, so that it is never taken for one of cpptraj's keywords such as `out`; it has only letters, digits and `:@^!()*.,-+_=?<>&|%/`, so no space, line break, quote, `#`, `$` or `\`, which would make it more than one argument or more than one command; and it is at most 256 characters. The worker checks the settings again for itself, builds the commands itself, and runs only the image and script of its own making, whatever a payload says.
- **What Aggregate trusts.** It takes only `summary.json`, `rmsd.dat`, `radgyr.dat`, `distance.dat`, `hbonds.dat`, `hbonds-avg.dat`, `dssp.dat` and `rmsf.dat` from each task, by those names; any other file a task leaves is not looked at. It checks that the tasks analysed every frame once, in order, or every trajectory once, in order, with the same settings, as the coordinator would have given them; that each table has a row for each frame the task says it read, numbered from 1; that every value is a plain decimal number, every hydrogen bond and residue a name of letters, digits and `+-_'*` with its number, every DSSP code a digit from 0 to 7, and that no bond was made in more frames than there were; and that no table passes 64 MiB, before it believes any of it. Every table it stores is written by the coordinator from those checked values.
- **It runs on workers started with `--containers`**, with the image and the script fixed. A worker builds the image the first time it is needed: AmberTools 25.3 from conda-forge (`ambertools=25.3=cuda_None_nompi_py312hd652fd9_100`, cpptraj V6.29.13, no MPI and no CUDA) installed with micromamba 2.0.5 on Debian 12, of which only cpptraj, the 49 libraries it loads and AmberTools' licence are copied onto `debian:12.12-slim`. The rest of AmberTools, a few gigabytes of it, stays in the build stage.
- **Image size**: 381 MB on disk, 99 MB of content. Building it downloads about 470 MB of conda packages; on the R620 the AmberTools stage took about four minutes.
- **Speed.** cpptraj runs on one thread in each task, since a worker runs as many tasks at once as it has slots. The test's 60 frames of a 22-atom molecule took half a second in one task, the container's start included. For a solvated protein's trajectory of thousands of frames, reading and analysing the frames is most of the time, and that is what is shared out; such a trajectory has not been timed here.
- **Results are not checked by a second worker.** On one machine cpptraj gives the same bytes every time, and however the job is shared out: the test checks that one task and four give the same tables. But each worker builds the image from conda-forge as it is that day, whose builds of AmberTools' dependencies change, and the OpenBLAS it loads picks its arithmetic by the processor; whether that moves the sixth decimal place of these analyses between processors has not been measured, so `verify` and `audit_share` refuse traj jobs rather than risk taking honest workers for wrong ones.
- **Limits.** At most 256 trajectories, and 15 for a job that asks for `rmsf`; at most ten million frames in all, secondary structure for at most 100,000 residues, and at most 100,000 different hydrogen bonds listed. A task is given at most 14 trajectories of its own, besides the topology and the first trajectory, so a job of many is shared out in at least enough tasks for that. In a job of one trajectory every task fetches all of it, as with the other workloads that split one input; in a job of several each fetches only its own, and the first where it needs that trajectory's first frame. No table may pass 64 MiB, which a DSSP table of many residues over many frames can: ask for `dssp` with a `region` of the residues you need. Compressed trajectories (`.gz`, `.bz2`) and other formats cpptraj reads, such as GROMACS's XTC, are not taken.
- **Licences.** cpptraj is GPL 3 or later, and AmberTools as conda-forge packages it is GPL 3, LGPL 3, BSD-3-Clause and MIT; the image runs it unmodified, as a separate program in a container, which the GPL allows without touching the licence of this repository, and keeps AmberTools' licence in `/opt/cpptraj/share/licenses`. The real-image test uses OpenMM's alanine dipeptide topology, already in `testdata/openmm` under OpenMM's MIT licence, and makes its trajectory itself.
- **Private jobs**: the coordinator reads the first 512 bytes of the topology and of each trajectory, with the job's key, to tell what they are; a sealed system and a job with its key should work, as with transcode, but this has not been tried.

## Layout

| Path | What it holds |
| --- | --- |
| `proto/sisyphus/v1` | Message and service definitions. Edit these, then run `make proto`. |
| `proto/sisyphus/node/v1` | The local API the desktop app uses. |
| `packages/protocol` | Go code generated from `proto/`. Do not edit by hand. |
| `packages/job-model` | Job and task state machines. No I/O. |
| `packages/jobrecord` | A finished job as linked data: building the record and reading it back. |
| `packages/runtime` | The `Workload` interface and built-in workloads. |
| `packages/storage` | Content-addressed blob store with IPFS-compatible CIDs. |
| `packages/kubo` | Running a Kubo daemon beside the node, and calling it. |
| `packages/ipfscluster` | Running an IPFS Cluster peer beside the node's Kubo, and calling it. |
| `apps/sisyphusd/coordinator` | Scheduling, retries, aggregation, worker connections. |
| `apps/sisyphusd/worker` | Connects to a coordinator and executes tasks. |
| `apps/sisyphusd/api` | gRPC server wiring and the client-facing services. |
| `apps/sisyphusd/access` | Who has been admitted and in what role; invitations; checking every call. |
| `apps/sisyphusd/tunnel` | Carrying a TCP connection inside a gRPC stream. |
| `apps/sisyphusd/replication` | Copies of a coordinator's pinned data on storage followers: who holds what, and fetching it back. |
| `apps/sisyphusd/p2p` | The node's libp2p host: one port shared with gRPC, the relay, discovery, reaching a node by its ID. |
| `apps/sisyphusd/planner` | The loop in which a model reasons, has the pool compute, and reads the result. |
| `apps/sisyphusd/inference` | The pool's language models offered as an OpenAI-style service. |
| `apps/sisyphusd/mcpserver` | The node offered to AI agents as a Model Context Protocol server. |
| `apps/sisyphusd/pinning` | The IPFS Pinning Service API: the node as a service, and as a client of one. |
| `packages/ai` | Talking to language models: Ollama, OpenAI-style services and Anthropic. |
| `packages/hardware` | Finding out what a machine has: processor, memory, graphics cards. |
| `packages/identity` | Node keys, IDs, and the TLS settings built from them. |
| `packages/names` | The signed records behind names: making, checking and comparing them. |
| `packages/nodedb` | The node's SQLite database: jobs, tasks, attempts and what they returned, members and invitations. |
| `packages/storage` | Content-addressed blob store, pins, garbage collection. |

`make proto` needs `protoc` on your path and the plugins from `make tools`.

## Known limits

- A task may fail three times, and losing its worker counts as a failure. Losing its coordinator does not. Its next attempt goes to a worker it has not gone wrong on, waiting for one that is busy if it must, and back to one it has only when there is no other that could run it.
- A coordinator accepts whatever result a worker returns, unless the job asks to be [verified](#verifying-results), and then it believes any `N` workers that agree. Verification is for deterministic work only. Admit only workers you trust.
- A node is remembered by address. If a coordinator's address changes, its workers and clients must join again.
- A node's key cannot be changed without becoming a different node, and there is no way to stop a copied key being used other than removing that node.
- Encryption hides what nodes say to each other, not that they are talking, how much, or when.
- If a node using `--kubo` is killed outright rather than stopped, its Kubo daemon keeps running and must be stopped by hand before the node will start again. With `--cluster` the same goes for its cluster peer.
- With `--cluster`, only the coordinator's peer can change what is pinned or find a new holder for it. While the coordinator is down the copies stay as they are.
- A removed node keeps whatever it had already fetched from the pool's private network. Changing the key stops it fetching anything more.
- Run `ipfs` commands against a node's repository only while the node is up. With its Kubo down, including for the few seconds of a key change, the `ipfs` command takes the repository's lock and the node cannot start Kubo until the command ends.
- Whatever is on the pool's private network can be fetched by every member of it. The network keeps outsiders out; it is private jobs, which seal their data, that keep members from reading each other's.
- A job's record is signed only by the coordinator that kept it, and its root links every receipt, so a job of ten thousand tasks has a root of about 400 KiB. It is not served by the gateway, which serves files only.
- A node has one name, its ID, and a record published by hand is not renewed: it stops resolving when its lifetime is over.
- The records of names reach the pool's private IPFS network only when they are published; a record that has gone from there is not put back until its node publishes again.
- Without `--max-store-bytes` there is no limit on what a worker or client can upload. `--max-store-bytes-per-client` bounds each client's uploads and pins and what its jobs' tasks store. A worker uploads only for an attempt at a task it holds, which the coordinator checks against its own record of the attempt, not the worker's word ([discussion #364](https://github.com/sisyphus-network/Sisyphus/discussions/364)): what it uploads is held for that attempt, and let go of when the attempt ends, however it ends, unless its result names it. It is stored pinned for that attempt alone, with none of the hour's grace other uploads get, so what a result does not name is collected once the attempt is over. `--max-upload-bytes-per-attempt` limits how much one attempt may upload in all, counted piece by piece as it arrives, so uploads made for one attempt at the same time cannot together go past it. What the attempts at a client's jobs upload is held to that client's share as well, while they run and before any of it is kept for the client: the attempts running at once share what is left of it, as what the client keeps already leaves it, and the piece that would take them past it together is refused with `RESOURCE_EXHAUSTED`. An attempt's uploads stop counting towards it when the attempt ends, and what its result names is then held to the share as it is kept. Each copy of a task checked by several workers counts what it uploads, so such a task may upload, across its copies, only what fits the client's share once, and a refusal says how many verified copies the task has ([discussion #376](https://github.com/sisyphus-network/Sisyphus/discussions/376) keeps this strict, so that verifying never loosens the share). The outputs of a task checked by several workers are kept uncounted until it is settled, up to as many blobs as a task may name for each of its results. Uploads and pins a client makes at the same time cannot together take it past its share: each is checked against it and kept in one step, and one that would go past it is not kept.
- These per-client and per-attempt limits can also be saved through the local API's `SetPoolLimits`, as the desktop does; a flag given at startup overrides what was saved.
- Without `--max-jobs-per-client` one client can have as many jobs unfinished as it likes. With it, a client that has that many is refused another until one is over; the coordinator's own jobs, and the steps of a job, are not counted. It bounds jobs, not tasks; `--max-tasks-per-client` bounds tasks, waiting or running, across a client's jobs and their steps, and refuses whole a job that would take the client past it. The steps of a graph count once, as the jobs they are: a graph with a step that asks for more `tasks` than that is refused when submitted, and a step that would take the client past it when it begins fails the graph.
- Free slots are shared between clients. When a slot frees and several clients have tasks waiting, it goes to the client with the fewest tasks running, to the oldest of that client's jobs, so one client's job of many tasks submitted first does not hold every slot while another's waits. With only one client waiting, it has every slot there is. The coordinator's own jobs count as one more client. Each copy of a task that is verified across workers takes a slot and is shared out the same way. The steps of a graph are jobs of their own, whose tasks are shared out like any other's.
- Without `--replicas` and at least one follower, a pool's stored data is on its coordinator's disk and nowhere else. With them, the coordinator's database and key are still on that disk alone, and without the key the data comes back only by hand and without its expiries.
- The pinning service has one key and speaks plain HTTP. Whoever holds the key can fill the store up to `--max-store-bytes`, and a single fetch can go past it.
- A pool's data can be kept by an outside pinning service only if that service is on the pool's private IPFS network, or has the data from somewhere else.
- A worker downloads a whole input even when its tasks need only part of it.
- Two workers that cannot reach each other exchange data through the coordinator, which is no faster than fetching from it.
- Without `--max-cache-bytes` a worker's cache grows until the disk is full. The limit is not strict: blobs that tasks have open are kept even if they alone exceed it.
