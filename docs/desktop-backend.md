# Building the desktop app against the daemon

Everything the desktop app can ask a node is in one file, [`proto/sisyphus/node/v1/node.proto`](../proto/sisyphus/node/v1/node.proto), served by `sisyphusd` on a loopback port. This page says how to get a backend running with something in it, and what each call gives you.

## A backend in one command

```sh
examples/desktop-backend.sh
```

It builds the daemon and starts three nodes on this machine:

| Node | What it is | Why it is there |
| --- | --- | --- |
| alpha | The node the app talks to, on `127.0.0.1:50051`. Runs a pool and a worker. | Your node. |
| beta | A worker of alpha's pool, joined by invitation. | A peer alpha gives work to, connected and working. |
| gamma | A node with its own pool that alpha was never told of. | Shows up in the peer list by discovery, untrusted: something to press the trust buttons on. |

It also runs one job, so the jobs list is not empty. Then, in another terminal, with the two variables the script prints:

```sh
cd apps/sisyphus
SISYPHUS_API_ADDRESS=127.0.0.1:50051 SISYPHUS_API_TOKEN_FILE=<printed path> npm run dev
```

`node dev/check-daemon.mjs <address> <token file>` asks the daemon everything below and prints the answers. It is short and uses the same gRPC library as the app, so it doubles as sample code for each call.

## The token

Anything on the machine can **read** from the local API. Calls that **change** something need the node's token, sent as `authorization: Bearer <token>` metadata. The app's main process already does this (`authorization()` in `src/main/index.ts`). The token is in `api.token` in the node's data directory.

## The calls

| Call | Token | What it gives |
| --- | --- | --- |
| `GetNodeInfo` | | The node's ID, version, addresses, the workloads it knows, and its country if it was started with `--locate-country`. |
| `ListPeers`, `WatchPeers` | | Every node this one knows of: its pool's members and the nodes it has found. `WatchPeers` re-sends the whole list when anything changes. |
| `ConnectPeer` | yes | Connect to a node at an address ending in `/p2p/<node ID>`. |
| `GetBootstrapPeers` | | The address book: nodes connected to at each start. |
| `SetBootstrapPeers` | yes | Replace the address book. |
| `SetPeerComputePermissions` | yes | Set the two sides of trust in a peer, separately. |
| `SetPeerComputeTrust` | yes | Set both sides at once. Kept for clients with one switch. |
| `ListWorkers` | | The nodes connected as this node's workers: name, hardware, slots, running tasks. |
| `SubmitJob` | yes | Give the pool a job. Returns it as it stands at once. |
| `GetJob` | | One job, with its tasks. |
| `ListJobs`, `WatchJobs` | | Every job on record, newest first. `WatchJobs` re-sends the whole list when any job changes. |

The two `Watch` calls are the ones to build live views on. Each message carries a `revision` that counts up from one.

## What a peer tells you

Each `Peer` has four flags worth showing, in two pairs.

**What this node has decided:**

- `gives_work`: this node gives the peer tasks. The peer may work for it.
- `takes_work`: this node takes the peer's tasks. It will work for the peer.

**What is happening now:**

- `works_for_this_node`: the peer is connected as a worker here.
- `this_node_works_for`: this node is working for the peer.

Work flows one way when one side gives and the other takes, so the second pair can lag or differ from the first: giving a peer work does nothing until that peer's owner chooses to take it. `trusted_for_compute` is set if either of the first pair is.

## What a job tells you

- `state`: pending, running, succeeded or failed. The same four apply to each task.
- `tasks`: one entry for each piece of the job, with the worker that ran it, how many attempts it took, and why the last one failed if one did.
- `params` and `result` are bytes. For the two built-in workloads they are JSON.
- `created_at_ms` and `finished_at_ms` are milliseconds since the Unix epoch; the second is zero until the job ends.

To submit one of the built-in workloads:

| Workload | `params` | Result |
| --- | --- | --- |
| `primes` | `{"from":0,"to":2000000}` | `{"count":148933}` |
| `wordcount` | `{"input":"<CID of a stored file>"}` | Counts, and the CID of the full table in `output_blobs`. |

`max_tasks` says how many pieces to split into; zero means one for each free worker slot.

## What the API does not have yet

These exist in the daemon but not in the local API. Ask for them when the UI wants them.

- **Storing and fetching files** (`wordcount` inputs, job outputs). For now: `sisyphusd blob put <file>` and `blob get <cid>`.
- **Invitations and the member list**, other than as peers. For now: `sisyphusd pool invite`, `pool members`.
- **Private jobs**, which take a key.
- **Progress within a task, and logs.** A task is pending, running or done; there is nothing finer yet.
- **Chat and the AI planner.** Not built in the daemon at all.

## Changing the API

`node.proto` is shared. After editing it:

1. `make proto` regenerates the Go side.
2. `apps/sisyphusd-rs/src/rpc.rs` must implement any new call, even if only to say it is not supported, or the Rust daemon stops building.
3. Add new fields and calls; do not renumber or remove existing ones.
