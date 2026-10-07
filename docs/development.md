# Developing Sisyphus

## Setup

You need Go 1.27 or newer. To change the protocol you also need `protoc` and the plugins that `make tools` installs.

Some tests run a real IPFS daemon and need the `ipfs` program ([Kubo](https://github.com/ipfs/kubo/releases)) on the PATH; CI uses the version pinned in `.github/workflows/ci.yml`. Without it those tests are skipped and `make cover` fails, since the code they exercise goes unrun. Each test daemon gets a repository of its own in a temporary directory and never touches `~/.ipfs` or a daemon already running on the machine.

```sh
make build   # bin/sisyphusd
make test    # go vet, then the tests under the race detector
make cover   # fails unless the tests execute every statement of hand-written code
make demo    # a three-node pool and a few jobs on this machine, in one terminal
make proto   # regenerate packages/protocol after editing proto/
make fmt
```

CI runs formatting, `go vet`, the tests under the race detector, `make cover` and a build on every pull request.

## Where things are

| Path | What it holds |
| --- | --- |
| `proto/sisyphus/v1` | Message and service definitions. The source of truth for the protocol. |
| `packages/protocol` | Go generated from `proto/`. Never edited by hand; committed so a fresh clone builds without `protoc`. |
| `packages/identity` | A node's key pair and the ID derived from it; signing and verifying; the TLS settings nodes connect with. |
| `packages/kubo` | Starting and stopping a Kubo daemon beside the node, and calling its API. |
| `packages/job-model` | Job and task state machines, and which blobs a job consumed and produced. No I/O, no locks. |
| `packages/runtime` | The `Workload` interface, the built-in workloads, and the recorder that notes what a workload reads and writes. |
| `packages/storage` | The blob store: IPFS-compatible import, pins, garbage collection, verification. |
| `apps/sisyphusd/coordinator` | Scheduling, retries, aggregation, worker connections, pinning a job's data. |
| `apps/sisyphusd/worker` | Connecting to a coordinator, running tasks, the blob cache. |
| `apps/sisyphusd/api` | The gRPC server and the client-facing services. |
| `apps/sisyphusd/access` | Which nodes have been admitted and in what role, invitations, and the check made on every call. |
| `apps/sisyphusd/blobclient` | Uploading and downloading blobs, checked against their CIDs. |
| `apps/sisyphusd` | The `sisyphusd` command: flags, startup, the command-line client. |

## Tests

Every statement of hand-written code must be executed by some test; `make cover` and CI enforce it. Generated code is not counted. In practice this means:

- **Failure paths need tests too.** Most of them are reached by playing a misbehaving peer: `protocol_test.go` speaks the worker protocol to a real coordinator by hand, `worker_test.go` scripts a coordinator, and the blob tests use nodes and stores that fail on request.
- **A check that cannot fire should not be written.** If an error cannot happen, do not branch on it; say why in a comment instead.
- **Time is passed in, not waited for.** Garbage collection takes the current time as an argument so tests can ask what happens an hour or a week from now.
- **Timing-dependent tests are a bug.** Coverage and results must be the same on every run.

Tests that need a node start a real one in-process on a loopback port (`startPool`, `startDaemon`). Every node in a test has its own key and connects over TLS like a real one; `cli` runs a command as the owner of the test node it is addressed to, and `as` runs one from another node's point of view.

A new call is open to the node's owner only until it is added to the table in `apps/sisyphusd/access/enforce.go`. Add a row to `TestEachRoleMayMakeOnlyItsOwnCalls` with it.

## The protocol

Edit the `.proto` files, run `make proto`, and commit the generated code with the change. Never renumber or reuse a field number; only add fields.

## Workloads

A workload implements `Split`, `Execute` and `Aggregate` in `packages/runtime`. `Split` and `Aggregate` run on the coordinator, `Execute` on a worker. Small values travel inline; anything large goes through the `Blobs` handle and is referred to by CID.

- Make results deterministic, so that two runs of a job can be compared by CID.
- Store a job's result in `Aggregate`. Blobs stored by tasks are treated as intermediate and released when the job ends unless `Aggregate` stores them again.

## Storage and IPFS

`packages/storage` is built from Boxo, the library set that Kubo, the main IPFS implementation, is itself built from. A blob gets the CID that `ipfs add --cid-version=1` gives the same bytes; the tests check the CIDs against values taken from a real Kubo node.

A store keeps its blocks in one of several places behind the same interface: files laid out as Kubo lays them out (`OpenLocal`, `OpenCache`), memory, or a running Kubo daemon (`OpenKubo`). Splitting files into blocks, pins, garbage collection and verification are the store's own and identical on all of them; only where a block is put and fetched differs. With Kubo the store talks to the daemon's HTTP API, one block per call.

Nodes do not yet join an IPFS network: Kubo, when used, runs offline, and blobs move between Sisyphus nodes over Sisyphus's own protocol.

## Branches and pull requests

`dev` is the main branch. Work goes on a branch and reaches `dev` through a pull request.

When one piece of work builds on another that has not merged yet, its branch is cut from the earlier branch and its pull request targets that branch, not `dev`. This keeps each pull request small enough to review on its own, at the cost of a chain that has to merge in order. Once the bottom of a chain merges, point the next pull request at `dev`. A branch can be deleted as soon as its pull request has merged.

## Releases

Push a tag such as `v0.1.0`. The release workflow builds every supported platform with `scripts/build-release.sh` and publishes the binaries and a `SHA256SUMS` file. Only Linux on x86-64 is covered by the tests.
