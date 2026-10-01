# Sisyphus

**Sisyphus is a decentralized market for useful compute.** Independent providers contribute CPU and GPU capacity; users bring computational work and pay for the resources they use. SISY is the network's intended settlement and incentive token.

AI makes the network easier to use: describe a problem, and Sisyphus can plan the work, find suitable capacity, coordinate execution, inspect the results, and decide what to compute next.

```text
Provide useful compute → earn SISY
Request compute        → spend SISY

Reason → Compute → Observe → Reason → Compute
```

The long-term goal is an open network where independent machines—from personal GPUs and homelabs to servers and cloud instances—can participate in a shared compute economy. Sisyphus is not a blockchain project looking for work for its token; it is a useful-compute network whose economy should emerge from real demand for computation.

## Why Sisyphus

Cloud providers sell compute from infrastructure they control. Sisyphus aims to coordinate compute supplied by many independent participants through an open market. Providers can put otherwise idle hardware to work, while users can access capacity from across the network.

The AI orchestration layer turns a high-level request into work the network can execute. For example, a user might ask Sisyphus to screen 100,000 molecular candidates. Sisyphus could inspect available resources, plan and schedule the computation, collect results, and propose a more expensive follow-up run for promising candidates.

The network should support direct requests from users and AI agents. The AI is an important interface and coordinator, while the underlying compute market remains useful to other clients and tools too.

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

Token design, trading, settlement, and governance are future protocol work. V0 has no token, blockchain, or public marketplace; it first tests whether independent machines can reliably contribute to one useful computation.

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

- **Electron desktop:** connects to the local `sisyphusd` and can expose local-machine capabilities through an Electron bridge.
- **Public Web UI:** can show public network information without requiring a personal node.
- **Web UI with a linked node:** can control the user's personal node over a direct connection when possible, with a relay as fallback.

Chats, model settings, and jobs belong to the relevant personal node. The Web UI presents and controls that state when connected; public visitors do not gain access to private node data.

### Protocol

Protocol Buffer schemas define Sisyphus messages and service contracts. gRPC is the primary RPC transport for daemon-to-daemon and native client communication, including streaming job and AI events. Browser clients may require a browser-compatible transport or gateway while reusing the same message definitions where practical.

The `sisyphusd` AI layer calls model providers locally and translates their output into Sisyphus plans and events. Provider credentials stay on the personal node and are never sent to compute workers.

## Local storage and future chain state

For V0, each daemon needs durable storage for conversations, jobs, task graphs, node and model configuration, execution metadata, and coordinator state. SQLite is the proposed embedded database for this local state. Large datasets, model artifacts, and job outputs should live in files or blob storage, with identifiers and content hashes recorded in the database.

If Sisyphus later adds a blockchain, consensus-critical records such as balances, settlements, and provider reputation will need authenticated protocol state. Its storage engine can be chosen with the chain implementation; RocksDB is one possible option. The database itself does not make state trustworthy: that depends on signed transactions, consensus, deterministic state transitions, and cryptographic commitments and proofs.

```text
V0 node state      → SQLite
Large artifacts    → files / blob storage, referenced by content hash
Future chain state → chain-specific database + authenticated commitments
```

## AI providers

The node owner configures the provider, model, and credentials on their personal `sisyphusd`. V0 is bring-your-own-key (BYOK), with initial adapters planned for Ollama and the OpenAI API. The daemon owns provider calls; the UI only configures the node and displays streamed responses.

The AI integration should be replaceable behind a small provider interface supporting streaming, tool calls, and structured plans. This keeps planning and compute orchestration independent from a single model vendor. Managed inference and billing may be added later as an optional convenience.

## Workloads and runtimes

Workloads can depend on very different software and hardware: Python, CUDA, PyTorch, GROMACS, Qiskit, Blender, and specific library versions. V0 keeps the environment fixed across trusted machines and supports a small set of known workloads.

Longer term, a managed runtime should let a workload describe its requirements and run across supported CPU and GPU backends without every user manually installing each dependency. Runtime portability, broad heterogeneous hardware support, and arbitrary untrusted workloads come after the basic network loop works.

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

## Monorepo

```text
sisyphus/
├── apps/
│   ├── sisyphusd/        # daemon: API, coordinator, planner, worker
│   └── desktop/          # Electron + React client
├── packages/
│   ├── protocol/         # generated/shared protocol code
│   ├── job-model/        # jobs, task graphs, results, state types
│   ├── planner/          # planning interfaces and schemas
│   └── runtime/          # workload execution abstractions
├── proto/                # Protocol Buffer service and message definitions
└── README.md
```

This is one product and one repository. The daemon owns execution and state; clients use its protocol to observe and control nodes.

## Project status

Sisyphus is in the concept and early architecture stage. The immediate goal is to demonstrate the AI orchestration loop across a small pool of trusted machines. That prototype is the foundation for the larger decentralized compute market and its eventual Proof of Useful Work economy.
