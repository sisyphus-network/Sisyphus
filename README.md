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

## Repository layout

```text
sisyphus/
├── apps/
│   ├── sisyphusd/        # Rust daemon
│   │   ├── Cargo.toml
│   │   ├── README.md     # daemon design, local gRPC API, and development
│   │   ├── migrations/
│   │   └── src/          # daemon, networking, RPC, and storage modules
│   └── sisyphus/         # Sisyphus Electron + React desktop client
├── packages/
│   ├── protocol/         # generated/shared protocol code
│   ├── job-model/        # jobs, task graphs, results, state types
│   ├── planner/          # planning interfaces and schemas
│   └── runtime/          # workload execution abstractions
├── proto/                # versioned Protocol Buffer API definitions
└── README.md
```

This is one product and one repository: **Sisyphus** is the product and desktop app; **`sisyphusd`** is the headless node daemon. The daemon owns node identity and networking today; API, coordinator, planner, and worker modules will be added when those responsibilities are implemented. Clients will use the daemon protocol to observe and control nodes. Daemon-specific guidance lives in [`apps/sisyphusd/README.md`](apps/sisyphusd/README.md), and desktop development details in [`apps/sisyphus/README.md`](apps/sisyphus/README.md).

## Project status

Sisyphus is in the concept and early architecture stage. The immediate goal is to demonstrate the AI orchestration loop across a small pool of trusted machines. That prototype is the foundation for the larger decentralized compute network and its eventual Proof of Useful Work economy.
