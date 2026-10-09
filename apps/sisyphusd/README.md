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
| `--relay <addr>` on a worker | It becomes a full node: other members connect to its libp2p host there, and it relays between them as the coordinator does. |
| `--swarm-port <port>` on a coordinator with `--kubo` | Workers' Kubo daemons connect to the coordinator's directly rather than through the tunnel. |

A worker's Kubo tries the coordinator's open port first, if it has one, and settles for the tunnel if nothing answers within two seconds, so opening the port and then forgetting the firewall rule costs speed, not the pool. The worker logs which it chose.

What the tunnel costs: every byte between a worker's Kubo and the coordinator's passes through both `sisyphusd` processes and is encrypted a second time. On one machine it moves about 300 MB/s against 2 GB/s for a direct connection, so it matters on a fast network with fast disks and not otherwise. Run `go test -run xxx -bench . ./apps/sisyphusd/tunnel` to measure it on yours.

**Full nodes.** A worker started with `--relay <host:port>`, on a port the other members can reach, relays between them. It tells the coordinator where it is; every worker asks the coordinator from time to time which members relay and keeps a place on each, so any two can be joined through the coordinator or through any full node. Each full node counts what it carries and reports it to the coordinator, and `sisyphusd nodes` shows it in the `RELAYED` column. That count is what a reward for relaying would be worked out from; nothing pays one yet.

**Both kinds of address.** A coordinator started with `--listen :7700` listens on every IPv4 and IPv6 address the machine has. One that names an address listens on that one.

What is not there yet:

- **Two workers on different private networks may stay on a relay.** libp2p can sometimes connect such a pair directly ("hole punching"), and it is switched on. On one machine, relayed pairs do go direct by themselves. Whether they do across two real home routers has not been tried, so do not count on it. Step 4 of [`docs/multi-machine-test.md`](../../docs/multi-machine-test.md) says how to tell.
- **Which relay carries a connection is not chosen.** A node asks through all of them at once and uses whichever answers first, so a full node takes load off the coordinator without being preferred to it.
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

## What each machine has

A worker looks at the machine it runs on and tells its coordinator what it finds: the processor, how much memory, and any NVIDIA graphics cards. `sisyphusd nodes` shows it.

```sh
bin/sisyphusd run --offer-memory-mb 16384 --offer-gpus 1 ...     # offer the pool less than the machine has
bin/sisyphusd job submit --min-memory-mb 8192 --gpus 1 ...       # a job that needs that much
```

- **A job can ask for memory and graphics cards**, and its tasks then go only to workers that have them. Asked to split itself, it splits to fit the workers that qualify. If none does, it waits until one connects.
- **The owner decides what to offer.** `--offer-memory-mb` and `--offer-gpus` cap what the node says it has. They do not hold a workload to it: nothing yet stops a task using more memory than its node offered.
- **What is found, and where.** Processor and memory are read on Linux. On other systems they are left unknown, and such a worker is given no job that asks for memory unless its owner says how much to offer. Graphics cards are found wherever NVIDIA's `nvidia-smi` is installed; other makes are not looked for.
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
- **`--models-from` is a decision about your machine.** Whoever may submit jobs to the pool can then use your models and the power they draw, and your machine sees every conversation it is given: there is no sealing of a conversation from the machine that has to read it.
- **Keep a conversation on a machine you trust** by asking for the model on a worker by name: `"model": "llama3.1:8b@rig"`. It goes to the worker named `rig` or, if that worker is not there, is refused; it never goes elsewhere. Asking for a model on your own machine's worker keeps the conversation at home while still going through the pool.
- **A reply arrives as it is written.** A client that asks for a stream gets the words a few times a second, and at the end the tools the model asks for and what it used. The worker logs the reply in pieces as it comes, so `job logs` shows it too.
- **A model may be named loosely** at the service: in another case, without its tag when the pool has a `latest`, or by its family alone (`llama3.1`) when the pool serves one model of that family. A name that could mean two models is refused, with the list of those served. A `chat` job submitted directly names its model exactly.
- **A model fetched later is offered within seconds.** A worker tells the coordinator when its models change, and a conversation that was waiting for one is then had.
- **Conversations are kept** with the pool's other jobs, parameters and result, for as long as jobs are kept (`--keep-jobs`).
- **From another machine**, the models are reached through that machine's own membership of the pool, not by opening the service to the network. Join as a client (`pool join` with a client invitation), then `sisyphusd inference --addr <coordinator>` offers the pool's models at `http://127.0.0.1:11435/v1` to programs on that machine, with the key in `api.token` in its data directory. Requests travel as jobs over the connection the pool already authenticates. A machine that is in the pool only as a worker cannot use it this way: workers run the pool's work and do not submit any.
- **A worker lost part way through a streamed reply** ends the stream with an error saying so, and the job is stopped. Another worker would begin the reply again, and a reply whose beginning came twice would look right and be wrong. Ask again. A reply not asked for as a stream is simply made again.
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

The second form is what Claude Desktop (`claude_desktop_config.json`), Cursor (`~/.cursor/mcp.json`) and most others take. The agent starts `sisyphusd mcp` itself and talks to it over standard input and output. It takes `--data-dir` and `--api` if the node's are not the usual ones.

| Tool | What it does |
| --- | --- |
| `pool_status` | The workers connected, with their cores, memory, graphics cards, the models they serve and how busy they are. |
| `list_workloads` | What the pool can run, the parameters each workload takes, and how many connected workers run it. |
| `run_job` | Runs a job and waits for its result, or returns at once with `detach`. |
| `get_job`, `list_jobs`, `cancel_job`, `job_logs`, `wait_for_job` | Look at a job, list them, stop one, read what it logged, wait for one to finish. |
| `store_file`, `fetch_file`, `list_files`, `remove_file`, `fetch_outputs`, `save_result` | Put a file from this machine in the pool's store, bring one back, list them, stop keeping one, bring back everything a job stored, write a job's whole result to a file. |
| `ask_model`, `compare_texts` | Has a model served by one of the pool's workers answer a prompt; says which texts mean most alike, by an embedding model the pool serves. |
| `ask_planner`, `list_chats`, `get_chat`, `delete_chat` | Hand a whole question to the node's own planner, and read or forget its conversations. |
| `list_peers`, `list_members` | The nodes this node knows of, with their countries and which way work flows; who is in its pool. |
| `get_model`, `list_models`, `list_model_providers` | What the planner plans with, what its service offers, and the kinds of service there are. |
| With `--admin`: `create_invitation`, `remove_member`, `join_pool`, `connect_peer`, `set_peer_trust`, `set_model`, `pull_model`, `remove_model` | Change the node itself: who is in its pool, which nodes it trusts, what it plans with. |

Besides tools, the server gives an agent things to read (`sisyphus://pool`, `sisyphus://workloads`, and the skill itself as `sisyphus://skill/SKILL.md` and `sisyphus://skill/recipes.md`), two prompts (`run-on-pool`, `pool-report`), and, while `run_job` or `wait_for_job` waits, word of each task as it finishes.

What the agent may do is yours to set, when you give its settings the command:

| Flag | What it does |
| --- | --- |
| `--files-under <dir>` | The only directory `store_file` may read from and `fetch_file` write to. It is the directory the server is started in unless you say otherwise; `/` is anywhere you may. A link inside it that leads outside is outside. |
| `--read-only` | Only the tools that look. Nothing is run, stored, written or changed. |
| `--images a,b` | The only container images `run_job` may run. Any, if not given. |
| `--admin` | Adds the tools that change the node itself. Without it there are none. |

- **It is a client of the local API** and holds the node's token, which it reads from the data directory.
- **With no flags it can use the pool and not reconfigure it.** It can spend the pool's time, run container images on workers that allow containers, and read and write files in the directory it was started in. It will not write over a file that is there unless told to in so many words.
- **`--admin` is the node's keys.** An agent with it can invite anyone into the pool and join the node to others. It is never given a model service's key: `set_model` keeps the one already set.
- **A skill comes with it**, carried in the daemon. `sisyphusd skill install` puts it where Claude Code keeps skills (`~/.claude/skills`), `--dir` puts it elsewhere, and `sisyphusd skill show` prints it. It tells an agent how to use a pool well: look before submitting, how to split work with a container, how to have the pool's models think, what goes wrong; `recipes.md` beside it has worked shapes for common jobs. It works with the tools or, without them, with the command line. Its source is [`skills/sisyphus`](../../skills/sisyphus/SKILL.md).
- **It has been tried from Claude Code**, with the skill installed and the server kept to one directory: asked to describe the pool, upper-case a file across three tasks and have one of the pool's models answer a question, it read the skill, ran a `container` job and `ask_model`, and wrote the right file. A second run, on Opus, labelled eight reviews with one `prompts` job and found the two most alike with `embed`; what it stumbled on (a result of vectors too long to hand back) is why `save_result` and `compare_texts` exist. Other agents have not been tried.

## Following and stopping a job

```sh
bin/sisyphusd job logs <job-id>                 # what has happened to it, and what happens next, until it is over
bin/sisyphusd job cancel <job-id>               # stop it
bin/sisyphusd job submit --timeout 10m ...      # stop and retry any attempt at a task that runs longer
```

- **Events.** A coordinator records what happens to each job: submitted, each task started, succeeded, failed, lost with its worker or timed out, and how the job ended. `job logs` prints them and follows a running job to its end. They are kept in the database with the job.
- **Logs.** A task can log lines as it runs, and those appear among the job's events with the task and worker they came from. The two built-in workloads log one line each.
- **Progress.** A task can say how far along it is. `job get` and the desktop's API give it for each task, from 0 to 1, and for the job as the mean of its tasks. It is as true as the workload makes it, and is not kept across a restart.
- **Cancelling** stops the job at once: its running tasks are told to stop, their slots are free again, and nothing more of it is handed out. A cancelled job is over, like one that succeeded or failed, and stays on record.
- **Timeouts.** With `--timeout`, an attempt at a task that runs longer is stopped and counts as a failure, so it is tried again up to the usual three times.
- **A job that fails stops its other tasks** the same way, rather than leaving them to finish for nothing.

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
| gives work, takes work | The two sides of this node's trust in the peer: that it gives the peer tasks, and that it takes the peer's. See [Trust](#trust) below. |
| trusted for compute | Either of the two. The call that sets it sets both, for clients that know only the one switch. |
| working for you, you work for it | Which way work is flowing now: the peer is connected as a worker of this node, and this node is working for the peer. Both need trust on both sides. |
| connect to an address | Connects this node's host to the node at an address ending in `/p2p/<node ID>`, which is the form a node lists its own in, and notes it in the address book. |
| bootstrap peers | The node's address book: the addresses it connects to each time it starts. |
| files | What was put in the pool's store through the app: kept until removed, listed by name, and given to jobs by CID. The same store `blob put` and `blob get` use. |
| invitation, members | `pool invite`, `pool members` and `pool remove`, from the app. |
| private | A file or job sealed with the key the node keeps in `private.key` in its data directory, made the first time one is asked for. The same as `--key-file` with that file. Lose the file and what it sealed is lost. |
| join a pool | `pool join` and a restart with `--join`, without the restart: the node redeems an invitation, says it takes that node's work, and starts on it. |
| country | Where the node's address is registered, and each peer's, from a table the daemon carries (`packages/geo`, made from the regional Internet registries' published allocations). Nothing is asked of anyone. It is empty for a node that knows itself only by a home-network address, until other nodes have told it the address they see it at; `--locate-country` then asks ipapi.co instead, which thereby learns the address, so that is not done unasked. The country is the one of registration, which now and then is not where the machine is. `scripts/update-geo.sh` remakes the table. |

With `--web-listen` and `--web-origin`, the same API is served to web pages from the origins listed, as Connect and gRPC-Web; there every call needs the token. [`docs/desktop-backend.md`](../../docs/desktop-backend.md#from-a-web-page) has the details.

Anything on the machine can read from the local API. Changing something needs the token in `api.token` in the data directory, which the daemon makes on first use and only its own user can read. The desktop app looks for it in the daemon's default data directory, or in the file named by `SISYPHUS_API_TOKEN_FILE`.

## Finding other nodes

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
- **It takes effect at once.** When a node's giving changes, it sends word to the node concerned over libp2p, which looks again on hearing. Should word not arrive, each asks again once a minute anyway.
- **A node's slots are shared, fairly.** However many nodes it works for, it runs no more tasks at once than `--slots`. A coordinator may hand it more than that, and those wait their turn. When a slot comes free it goes to the pool with the fewest of its tasks running on this node, so one pool's long queue does not hold up another's single task.
- **Each pool's data is kept apart.** What a node fetches while working for another is kept under `guest/<that node's ID>` in its data directory, away from its own pool's store and from every other node's.
- **With `--kubo`, it joins the other pool's private IPFS network too.** A private network has a key of its own, so the node runs one more Kubo daemon for each pool it works for, on a repository under that same directory, and follows that pool's key when it changes. Each costs about as much memory as the node's own. If the other node runs no such network, the work is done without one.
- **The desktop shows which way work is flowing**: beside each peer, "Working for you" and "You work for it", whichever hold.
- **`--work-for-trusted=false`** has a node take nobody's work but that of the coordinator it was started for, whatever its list says.

What this does not do yet:

- **Word of a change can be sent by any node that can connect.** All it does is make this node ask sooner, so the worst a stranger can do with it is cause some needless asking.
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
- **`primes`** counts the primes in `[from, to)`. Parameters and results are a few bytes and travel inline. It is a stand-in that exercises the pool.
- **`wordcount`** counts word frequencies in a stored file. It exercises the storage path: the input is fetched by CID, each task reads only its byte range, task outputs are stored as blobs, and the result is a blob too. The result's CID is the same however the job is split, so two runs can be compared by CID alone.

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
- **Input.** `"input": "<CID>"` gives every task that stored file at `/input/data`, read-only.
- **Output.** What the command prints is stored and its CID returned for each task, and so is every file it leaves in `/output`. `blob get <CID>` fetches them. What it prints is also the task's log, live, in `job logs`.
- **Limits.** `"memory_mb"` and `"cpus"` cap what each task may use, and unlike a node's offer these are enforced, by Docker. A task has no network unless the job says `"network": true`. It runs as the user the node runs as, not as root.
- **Stopping.** Cancelling the job, or a task running past the job's `--timeout`, kills the container.
- **A job is taken in by any coordinator**, and waits until a worker that runs containers is connected.
- **Tasks of a job that run on one machine share one copy of its input**, fetched once. Tasks of different jobs share nothing.

- **Graphics cards.** `"gpus": 1` gives each task that many of the machine's cards, which needs Docker set up for them (the NVIDIA Container Toolkit). Submit the job with `--min-gpus` as well, so that its tasks go only to machines that have them. This has not been tried on a machine with a card.

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
| `apps/sisyphusd/planner` | The loop in which a model reasons, has the pool compute, and reads the result. |
| `apps/sisyphusd/inference` | The pool's language models offered as an OpenAI-style service. |
| `apps/sisyphusd/mcpserver` | The node offered to AI agents as a Model Context Protocol server. |
| `packages/ai` | Talking to language models: Ollama, OpenAI-style services and Anthropic. |
| `packages/hardware` | Finding out what a machine has: processor, memory, graphics cards. |
| `packages/identity` | Node keys, IDs, and the TLS settings built from them. |
| `packages/nodedb` | The node's SQLite database: jobs, tasks, attempts, members and invitations. |
| `packages/storage` | Content-addressed blob store, pins, garbage collection. |

`make proto` needs `protoc` on your path and the plugins from `make tools`.

## Known limits

- A task may fail three times, and losing its worker counts as a failure. Losing its coordinator does not.
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
