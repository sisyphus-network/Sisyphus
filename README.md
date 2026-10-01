# Sisyphus

Sisyphus is an AI-managed distributed compute platform.

You give Sisyphus a computational problem. The AI decides how to solve it, what compute is required, where each part should run, executes the work, observes the results, and decides what to do next.

```text
Reason → Compute → Observe → Reason → Compute
```

The platform follows a local-first principle:

```text
Local first → Hybrid → Distributed
```

Sisyphus should not send everything to the cloud or to a public network by default. If a user's own machine is sufficient, it should use that machine. If more capacity is needed, it should add trusted remote nodes. If a workload can be split, it should use a hybrid plan.

## The core idea

Consider a request such as:

> Screen 100,000 molecular candidates against this target.

Sisyphus should be able to:

1. Understand the problem.
2. Create a computational plan.
3. Inspect available CPU, GPU, memory, and runtime capabilities.
4. Split the plan into jobs.
5. Schedule those jobs across local and remote nodes.
6. Collect and aggregate results.
7. Analyze the results with AI.
8. Decide whether another round of computation is worthwhile.

The important distinction is that Sisyphus is not only a distributed job scheduler. The AI observes the output and can create the next computational step.

## Architecture

Every participating machine runs one `sisyphusd` daemon. The UI is a client, not a compute runtime. It reads state from nodes and sends commands to them; workloads, hardware access, job state, and execution remain owned by `sisyphusd`.

```text
                   Web UI / Electron UI
                     │              │
          public read API      local Electron API
                     │              │
                     ▼              ▼
              Public network    Local `sisyphusd`
                     │              │
                     └──────┬───────┘
                            │ gRPC / protocol
                    ┌───────┴────────┐
                    ▼                ▼
             Remote `sisyphusd`  Remote `sisyphusd`
                 Worker              Worker
```

`Sisyphusd` is a single binary with different capabilities or roles. A local machine may run planner, coordinator, and worker capabilities together. A headless server may run only the worker capability.

This keeps deployment simple while preserving clean internal boundaries:

- **Planner** — turns a user problem into a task graph.
- **Coordinator** — assigns jobs, tracks state, retries work, and aggregates results.
- **Worker** — executes jobs using local CPU/GPU resources.
- **Pool** — a user-managed group of `sisyphusd` nodes that can be scheduled as one compute resource.
- **Hardware discovery** — reports capabilities and current resource availability.
- **Runtime manager** — selects the execution environment for a workload.
- **UI API** — exposes state, logs, progress, and controls to the desktop app.

Power users should be able to create a private compute pool from machines they control. One node runs the coordinator role and registers additional `sisyphusd` nodes as workers. The coordinator sees each worker's capabilities and availability, then places work according to the pool's policy and the workload's requirements.

```text
User compute pool
├── Coordinator node (`sisyphusd`)
├── GPU worker (`sisyphusd`)
└── CPU worker (`sisyphusd`)
```

Pools can support different execution modes. In distributed mode, a workload is split into tasks that can run across several workers. In **full-worker mode**, one selected worker receives the whole workload and may use all resources allocated to it; Sisyphus does not divide that workload across pool members. The workload itself may still use multiple CPU cores or GPUs inside that one machine.

```text
Distributed mode:  one workload → task chunks → multiple workers
Full-worker mode:  one workload → one worker → its allocated resources
```

Full-worker mode is useful when a workload is not safely divisible, needs a single machine's memory or attached data, or already parallelizes internally. Resource access remains subject to the worker owner's configured limits and the node's execution policy.

The Electron application is a separate client application. It can use an Electron API bridge to communicate with a local `sisyphusd` instance and expose desktop-specific capabilities. It should be a control surface for the daemon, not the system's source of truth. The daemon owns execution state, job state, logs, results, and resource information.

The Web UI can be public without being connected to a user's personal machine. In that mode it provides a read-only view of public nodes, capabilities, network status, and other public data. A user can optionally pair the Web UI with a personal node for private control. The connection should prefer a direct path and fall back to a relay when the node is behind NAT.

```text
Web UI without a personal node
    → public network view

Web UI with a personal node
    → public network view
    → direct or relayed connection to personal `sisyphusd`
    → chats, jobs, results, and controls

Electron UI
    → Electron API bridge
    → local `sisyphusd`
    → optional remote nodes
```

The UI should render from runtime capabilities and connection state rather than scattering platform checks throughout the application:

```text
platform: electron | web
localNode: connected | unavailable
personalNode: connected | disconnected
permissions: read | control
```

## V0: prove the loop

The first version should be intentionally small and boring.

The goal is to prove that an AI can use two or three trusted computers in different locations to solve one computational problem:

```text
User problem
    ↓
AI creates a plan
    ↓
Coordinator splits the work
    ↓
Israel node ───── chunks 1–5
Austin node ───── chunks 6–10
    ↓
Results are collected and aggregated
    ↓
AI observes the result
    ↓
AI decides whether to run another round
```

V0 assumptions:

- Two or three trusted machines.
- A fixed, pre-installed environment.
- A central coordinator role running inside one `sisyphusd` instance.
- A small number of known workload types.
- Basic job scheduling, status, logs, retries, and result collection.
- Trusted user-managed pools of a few `sisyphusd` nodes, with distributed and full-worker scheduling modes.
- Node-local AI provider and model configuration.
- Secure local storage for the selected provider credentials.
- Streaming model responses and the minimum tool-calling interface required by the planner.
- No public or untrusted workers yet.

## Explicit non-goals for V0

We are deliberately not trying to build all of the long-term platform at once.

- No blockchain.
- No token.
- No IPFS requirement.
- No quantum computing.
- No giant decentralized protocol.
- No arbitrary untrusted code execution.
- No complete cross-platform runtime abstraction.
- No permissionless public worker marketplace.

The first milestone is not a whitepaper or a token launch. It is a working computation that can travel between trusted machines and return a result to the AI orchestration loop.

## AI providers

Sisyphus should be model-agnostic and provider-agnostic. The platform's job is to orchestrate reasoning and computation, not to force every user onto one AI provider.

For V0, the preferred model is bring your own key (BYOK):

```text
User selects provider and model
        ↓
User supplies the provider API key
        ↓
Sisyphus Planner / Observer
        ↓
Task Graph and compute orchestration
```

The provider key should be stored in secure local storage and must not be sent to public workers. When a user controls a personal `sisyphusd` node, that node can act as the AI gateway for the user's Web UI and call the selected provider on the user's behalf.

Model configuration belongs to the personal node, not to the public Web UI or to the public network:

```text
Personal `sisyphusd`
├── selected providers
├── selected models
├── API keys
├── model capabilities
└── usage limits
```

The Web UI can display the connected node's model configuration and capabilities, but the node remains responsible for storing credentials and making provider requests. A public Web visitor without a connected personal node can inspect the public network, but cannot use a user's private model configuration.

The AI layer should use a provider abstraction that supports multiple implementations:

- Cloud model providers.
- OpenAI-compatible endpoints.
- Self-hosted or local models.
- Custom/private inference endpoints.

The abstraction should cover structured outputs, tool calling, streaming, context limits, and model capabilities. The planner and execution loop should not depend on one provider's API.

In the future, Sisyphus may offer managed inference where users pay Sisyphus for convenience, usage limits, and routing. That is intentionally out of scope for V0; BYOK keeps the initial system decentralized, transparent, and financially simple.

## Workloads and runtimes

Scientific and technical workloads often require incompatible environments: Python, CUDA, PyTorch, GROMACS, Qiskit, Blender, Docker, and specific library versions.

Long term, Sisyphus will need a managed execution layer that hides most physical machine differences from workloads:

```text
Sisyphus Job
    ↓
Standard workload definition
    ↓
Sisyphus Runtime
    ↓
CPU / NVIDIA / AMD / future backends
```

V0 can use a fixed environment on trusted machines. Runtime portability, sandboxing, dependency management, and heterogeneous hardware support will be introduced incrementally after the orchestration loop works.

## Local storage and future protocol state

Each `sisyphusd` node needs durable local storage for conversations, jobs, task graphs, node configuration, provider settings, execution metadata, and coordinator state. SQLite is a suitable V0 default for this single-node state: it is embedded, transactional, and avoids requiring users to operate a separate database server.

Large datasets, model artifacts, and bulky job outputs should be stored as files or blobs, with the local database keeping their identifiers, metadata, and content hashes. This keeps operational state queries separate from large payload storage.

The long-term protocol may add an on-chain ledger and authenticated network state for balances, settlements, provider reputation, and other consensus-critical records. That state may use a purpose-built storage engine such as RocksDB or another engine selected with the chain implementation. The local database is only the storage mechanism: blockchain security comes from signed transactions, consensus, deterministic state transitions, and cryptographic commitments/proofs (for example, Merkle proofs), not from SQLite or RocksDB by themselves.

```text
V0 node state       → SQLite
Large artifacts     → files / blob storage, referenced by content hash
Future chain state  → chain-specific database + authenticated state commitments
```

The V0 storage model should keep compute records and state transitions explicit so a future protocol layer can derive or verify settlement data without making the local application database itself the source of network-wide trust.

## Decentralization and the compute economy

The long-term vision is not only a distributed compute product. Sisyphus is intended to become an open, permissionless market for useful computation: a decentralized network where independent participants provide compute, users request compute, and a native economic layer coordinates supply and demand.

```text
Idle hardware → useful workloads → SISY rewards
Compute demand → SISY payments   → available capacity
```

The AI is the orchestration layer that makes this market usable. It understands a computational problem, finds or requests suitable capacity, breaks the work into jobs, coordinates execution, verifies and aggregates results, and decides what to compute next.

```text
AI agent / user
        ↓
Sisyphus compute market
        ↓
Independent CPU/GPU providers
```

This is the fundamental difference from a traditional cloud provider: the network is not one company's fixed inventory of servers. Anyone who meets the protocol's requirements can contribute capacity and earn rewards, while anyone with a legitimate compute need can buy access to the market. Cloud machines, personal workstations, homelabs, and specialized hardware can all become providers.

The long-term economic loop is:

```text
Provide compute → earn SISY
Need compute    → spend SISY
```

The first contributors and founders can bootstrap the network by providing the initial compute. Their rewards should come from measurable useful work and clearly defined protocol rules, not from an unlimited private mint. Founders participate as early providers and builders; they do not get an invisible permanent right to print supply.

Potential future principles:

- Transparent supply and emission rules.
- Fixed or formula-based founder and early-contributor allocations.
- Long vesting periods.
- Rewards tied to useful, verifiable computation.
- No hidden founder multiplier or unlimited mint authority.
- Governance that becomes progressively more decentralized.

The network will only have lasting value if SISY has real utility: purchasing compute, compensating providers, reserving capacity, providing collateral, or participating in reputation and verification mechanisms. A token by itself is not the product; the useful-compute market is the product.

### Proof of Useful Compute

Sisyphus is ultimately aiming toward a **Proof of Useful Compute** network.

Traditional proof-of-work systems spend computation primarily to secure a ledger. Sisyphus should direct participating hardware toward work that produces useful outputs: scientific simulations, model inference, rendering, optimization, data processing, and other computational workloads.

```text
Work request
    ↓
Provider executes a useful workload
    ↓
Result is returned and verified
    ↓
Provider earns SISY
```

This requires more than measuring CPU time. The protocol will eventually need workload-specific verification, reproducible execution, result comparison or replication, provider reputation, and penalties for invalid or dishonest results. These mechanisms are not part of V0, where nodes are trusted, but they are essential to a permissionless public network.

The economic layer is intentionally out of scope for the first implementation, but it is central to the long-term identity of Sisyphus. V0 must first demonstrate that the network can create useful compute demand and coordinate real workloads before adding permissionless providers, verification, settlement, and a traded token.

## Suggested repository shape

This project is intended to live as a monorepo. The repository has two top-level applications:

- `apps/sisyphusd` — the daemon/backend. Its internal modules include the API, coordinator, planner, and worker.
- `apps/desktop` — the Electron + React client that connects to a local `sisyphusd` instance.
- The Web UI can share the desktop UI code while using a browser-compatible transport for public reads and personal-node connections.

Shared contracts and domain models live in `packages/` rather than being duplicated between the daemon and the UI:

```text
sisyphus/
├── apps/
│   ├── sisyphusd/       # daemon/backend binary
│   │   ├── api/         # REST/WebSocket/gRPC-facing API
│   │   ├── coordinator/ # task assignment, state, retries, aggregation
│   │   ├── planner/     # AI planning integration
│   │   └── worker/      # local execution and resource management
│   └── desktop/         # Electron + React client
├── packages/
│   ├── protocol/        # shared API and wire types
│   ├── job-model/       # jobs, task graphs, results, and state types
│   ├── planner/         # shared planner interfaces and schemas
│   └── runtime/         # workload execution abstractions
├── proto/               # daemon/coordinator protocol definitions
└── README.md
```

The initial implementation can be smaller than this structure. The important boundary is that the daemon owns execution and system state, while the UI observes and controls it through an explicit API. The UI does not contain the workload runtime. This is one repository and one product, not a collection of independently deployed microservices.

## First milestone

Sisyphus is ready for its first serious demo when:

1. A user submits one computational problem.
2. The AI creates a task graph.
3. A coordinator distributes tasks to two remote `sisyphusd` nodes.
4. Both nodes execute the same known workload.
5. Results return and are aggregated.
6. The AI receives the results and proposes or launches the next computation.

If computation can travel from Israel to Kentucky and back while the AI manages the loop, Sisyphus has its skeleton.

## Status

Early concept and architecture phase.

The immediate priority is to build the smallest ugly system that proves:

> AI-managed computation can use multiple real machines as one coordinated system.
