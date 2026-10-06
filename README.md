<p align="center">
  <img src="assets/logo.png" alt="Sisyphus logo" width="240" />
</p>

<h1 align="center">Sisyphus</h1>

**Sisyphus is a decentralized network for useful compute.** Independent participants contribute CPU and GPU capacity, and users and AI agents submit computational work to the network. Its planned economic layer uses SISY to coordinate incentives, supply, and demand.

AI makes the network easier to use: describe a problem, and Sisyphus can plan the work, find suitable capacity, coordinate execution, inspect the results, and decide what to compute next.

```text
Provide useful compute → earn SISY
Request compute        → spend SISY

Reason → Compute → Observe → Reason → Compute
```

The long-term goal is an open network where independent machines—from personal GPUs and homelabs to servers and cloud instances—contribute to shared computation. SISY and the network's economy are intended to reward useful contributions and coordinate access to capacity. The token serves the network; it is not the reason for building it.

## Why Sisyphus

Cloud providers sell compute from infrastructure they control. Sisyphus aims to coordinate compute supplied by many independent participants across an open network. Providers can put otherwise idle hardware to work, while users can access capacity contributed by the network.

The AI orchestration layer turns a high-level request into work the network can execute. For example, a user might ask Sisyphus to screen 100,000 molecular candidates. Sisyphus could inspect available resources, plan and schedule the computation, collect results, and propose a more expensive follow-up run for promising candidates.

The network should support direct requests from users and AI agents. The AI is an important interface and coordinator, while the underlying compute network remains useful to other clients and tools too.

## Proof of Useful Work

Sisyphus is working toward **Proof of Useful Work (PoUW)**: providers are rewarded for useful computational work whose results can be validated, rather than for computation performed only to secure a ledger.

```text
Work request
    ↓
Provider runs the workload
    ↓
Result is checked
    ↓
Provider receives a reward
```

Useful workloads may include scientific simulations, model inference, rendering, optimization, and data processing. Measuring runtime alone is not enough to prove useful work. A permissionless network will need verification methods suited to each workload, such as reproducible execution, result comparison, or replicated computation, along with reputation and dispute mechanisms.

PoUW is a long-term protocol goal, not a solved feature of the first version. V0 uses trusted machines and does not issue token rewards.

## The compute economy

SISY is intended to connect compute supply and demand:

```text
Users / agents ── request and pay for compute ──▶ Network
Providers      ◀── useful work and SISY rewards ─ Network
```

The token should have utility within the network, including paying for jobs and compensating providers. Supply, emissions, founder and contributor allocations, vesting, and governance need transparent rules. Early founders and contributors can help bootstrap the network by building it and providing its first compute, under rules that are visible and defined in advance.

Token design, trading, settlement, and governance are future protocol work. V0 has no token, blockchain, or open public network; it first tests whether independent machines can reliably contribute to one useful computation.

## Architecture

Each participating computer runs one `sisyphusd` daemon. It discovers local hardware, manages workloads and resources, communicates with other nodes, and stores that node's local state. The same binary can run different capabilities: a machine may act as coordinator and worker, while a headless server may act only as a worker.

```text
Web / Electron / agent clients
              │
       Sisyphus protocol
              │
       Coordinator node
       ├── local worker
       ├── remote worker
       └── remote worker
```

The coordinator plans and assigns work, tracks job state, handles retries, and aggregates results. Workers execute workloads using the resources their owners make available. These are roles inside `sisyphusd`, not necessarily separate services.

### User-managed compute pools

Power users can group machines they control into a private compute pool. One `sisyphusd` runs the coordinator role and connects to the pool's other daemons as workers. The coordinator schedules work based on each node's capabilities, availability, and the workload's requirements.

```text
Private compute pool
├── Coordinator + worker (`sisyphusd`)
├── GPU worker (`sisyphusd`)
└── CPU worker (`sisyphusd`)
```

Pools support two scheduling modes:

- **Distributed mode:** split a workload into tasks and run them across multiple workers.
- **Full-worker mode:** send the whole workload to one selected worker and let it use the resources allocated on that machine. The workload may still use multiple CPU cores or GPUs within that worker.

Full-worker mode suits workloads that cannot be split safely, need one machine's memory or attached data, or already parallelize internally. The worker owner controls resource limits and execution policy.

### Web and desktop clients

The UI is a client, not a compute runtime. The daemon owns hardware access, workload execution, jobs, results, and node-local state.

- **Sisyphus desktop:** the Electron + React client connects to the local `sisyphusd` over gRPC. The Electron main process owns the gRPC connection and exposes a narrow, isolated bridge to the renderer. The first screen visualizes node identity, daemon connectivity, and live peer state.
- **Public Web UI:** can show public network information without requiring a personal node.
- **Web UI with a linked node:** can control the user's personal node over a direct connection when possible, with a relay as fallback.

Chats, model settings, and jobs belong to the relevant personal node. The Web UI presents and controls that state when connected; public visitors do not gain access to private node data.

For daemon-specific implementation and development details—including networking, storage, AI provider configuration, and run instructions—see [`apps/sisyphusd/README.md`](apps/sisyphusd/README.md).

Workload portability and managed runtimes are longer-term goals. V0 uses a fixed environment across a small number of trusted machines; a universal runtime is not a prerequisite for proving the basic network loop.

## V0: prove the loop

The first milestone is a working computation coordinated across two or three trusted machines:

```text
User describes a problem
        ↓
AI creates a plan
        ↓
Coordinator schedules tasks
        ↓
Trusted nodes execute the work
        ↓
Results return and are aggregated
        ↓
AI observes results and chooses what to do next
```

V0 includes:

- One coordinator role inside `sisyphusd`, with worker capability on the same or other nodes.
- Private pools of a few trusted machines.
- Distributed and full-worker scheduling modes.
- A fixed environment and a small set of known workloads.
- Job state, progress, logs, retries, result collection, and AI streaming.
- Node-local model configuration, secure credential storage, and initial Ollama/OpenAI adapters.
- Local persistence for chats, jobs, and node state.

V0 deliberately excludes:

- Blockchain, SISY token, trading, and settlement.
- Public permissionless workers and decentralized discovery.
- Trustless result verification and PoUW rewards.
- A universal runtime for arbitrary workloads.
- IPFS, quantum workloads, and a large decentralized protocol stack.

## Trying it

You need Go 1.27 or newer. Everything here runs on one machine.

**The one-minute version.** Starts three nodes, runs a few jobs across them and shuts them down:

```sh
make demo
```

**A pool to play with.** With tmux, in one terminal:

```sh
examples/tmux-pool.sh
```

That starts a coordinator and two workers in the left pane and gives you a shell in the right pane that is already pointed at them. Without tmux, run `examples/local-pool.sh` in one terminal, and in a second paste the `export` line it prints. Then:

```sh
examples/primes.sh                # the same job split across the pool, then on one worker
examples/wordcount.sh             # store a file, count its words across the pool
examples/wordcount.sh my.txt 20   # your own file, the top 20 words
examples/private-wordcount.sh     # the same, with the data sealed from the rest of the pool
```

A `primes` run looks like this, here on one 32-core machine with two workers of four task slots each:

```text
counting the primes below 2000000000
  split into 8 tasks     1.307816647s     {"count":98222287}
  as a single task       9.163948934s     {"count":98222287}
```

**With IPFS.** If the `ipfs` program ([Kubo](https://github.com/ipfs/kubo/releases)) is installed, `examples/local-pool.sh --kubo` gives every node a Kubo daemon and the pool a private IPFS network of its own. Anything stored is then an ordinary IPFS file:

```sh
IPFS_PATH=sisyphus-pool/alpha/ipfs ipfs cat <a CID that wordcount.sh printed>
```

**By hand.** What the scripts do, a command at a time:

```sh
make build
bin/sisyphusd run --name alpha                      # a coordinator that is also a worker
bin/sisyphusd pool invite                           # prints an invitation for one worker
bin/sisyphusd run --role worker --coordinator 127.0.0.1:7700 --join <invitation> \
    --name beta --data-dir ~/.sisyphus-beta
bin/sisyphusd nodes
bin/sisyphusd job submit --params '{"from":0,"to":3000000000}'
```

**On more than one machine.** [`docs/multi-machine-test.md`](docs/multi-machine-test.md) is a step-by-step test of a pool across two or three machines, including what to break and what should happen when you do. Nobody has run it yet; if you do, the results belong on [#18](https://github.com/excho0/Sisyphus/issues/18).

More in [`examples/`](examples/README.md).

## Repository layout

```text
sisyphus/
├── apps/
│   ├── sisyphusd/        # the node daemon and command-line client, in Go:
│   │                     #   coordinator, worker, storage, security, node API
│   ├── sisyphus/         # Sisyphus Electron + React desktop client
│   └── sisyphusd-rs/     # an earlier Rust daemon: identity, libp2p discovery
│                         #   and the node API the desktop client was built on
├── packages/
│   ├── protocol/         # Go code generated from proto/
│   ├── identity/         # node key pairs and IDs
│   ├── job-model/        # job and task state machines
│   ├── kubo/             # running and calling a Kubo (IPFS) daemon
│   ├── runtime/          # the Workload interface and built-in workloads
│   ├── sealed/           # encryption of a private job's blobs
│   ├── storage/          # content-addressed blob store, pins, garbage collection
│   └── planner/          # planning interfaces and schemas (not started)
├── proto/                # versioned Protocol Buffer API definitions
├── examples/             # sample input and scripts to run against a pool
├── scripts/              # demo, release build, coverage check
├── docs/                 # developer guide; multi-machine test guide
└── README.md
```

This is one product and one repository: **Sisyphus** is the product and desktop app; **`sisyphusd`** is the headless node daemon. Clients use the daemon's protocol to observe and control nodes. Daemon guidance lives in [`apps/sisyphusd/README.md`](apps/sisyphusd/README.md), and desktop development details in [`apps/sisyphus/README.md`](apps/sisyphus/README.md).

**Two daemons, for now.** The work so far was done in two halves that have just met: a Rust daemon with node identity, libp2p peer discovery and a local API, with the desktop client on top; and a Go daemon with the job loop, storage, security and IPFS. `apps/sisyphusd` is the Go one and is what the examples, tests and releases use. It serves the same local API, so the desktop client runs against it. `apps/sisyphusd-rs` is the Rust one, kept as it was written. Which language the daemon ends up in, and what moves where, is being decided in [#1](https://github.com/excho0/Sisyphus/issues/1).

## Project status

Sisyphus is an early prototype. The network loop works without AI: a coordinator splits a job, workers on other nodes run the tasks, and the results are combined. Everything so far has run on one machine as separate processes; it has not yet been tried across physically separate machines.

**Working today**, in `sisyphusd`, written in Go:

- One binary that runs as coordinator, worker or both, with a command-line client.
- Distributed and full-worker scheduling, with tasks retried when a worker fails or disconnects.
- Content-addressed storage for job data. Files are named by the same CID that `ipfs add --cid-version=1` gives them. Nodes never join the public IPFS network.
- Jobs that pass large inputs and outputs by CID; workers fetch them from the coordinator or from each other, verify and cache them.
- Pins, retention periods, garbage collection and disk limits, so a node keeps data only as long as something needs it.
- Optionally, a Kubo (IPFS) daemon run beside each node, as the same peer as the node. A pool's daemons form a private IPFS network that only its members can join, and workers fetch job data over it from whichever member has it.
- Private jobs, whose inputs, intermediate data and results are sealed with a key held only by the submitter and the nodes working on the job.
- Encrypted connections between nodes, each identified by its own key; nodes join a pool by invitation, as a worker or a client, and can be removed.
- Jobs kept in a SQLite database, so a coordinator that is restarted takes up its unfinished jobs where they were.
- A desktop client that shows a node and its pool, and admits or removes workers.
- Two stand-in workloads, `primes` and `wordcount`, that exercise the network rather than compute anything valuable.
- Tests that execute every statement of hand-written code, enforced in CI, and release builds for Linux, macOS, Windows, the BSDs and Android.

**Not built yet**: the AI planner and model providers, real workloads in containers, hardware discovery, and finding peers without being told their address. A coordinator accepts whatever result a worker returns, so a pool is only as trustworthy as the workers admitted to it. There is no blockchain, token or public network.

Try it with `make demo`, or see [Trying it](#trying-it) below. [`apps/sisyphusd/README.md`](apps/sisyphusd/README.md) is the full guide to running nodes, and [`docs/development.md`](docs/development.md) covers building, testing and contributing. The plan, in order, is in the [roadmap issue](https://github.com/excho0/Sisyphus/issues/23).

Two things in this section go beyond what the sections above describe and are still open for discussion: the choice of Go ([#1](https://github.com/excho0/Sisyphus/issues/1)), and building storage on IPFS formats while V0 lists IPFS as out of scope ([#25](https://github.com/excho0/Sisyphus/issues/25)).
