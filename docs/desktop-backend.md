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
| `ListWorkers` | | The nodes connected as this node's workers: name, slots, running tasks, and what each machine has (processor, memory, graphics cards; zero and empty where unknown), and `models`, the language models it serves to the pool. |
| `SubmitJob` | yes | Give the pool a job. Returns it as it stands at once. |
| `GetJob` | | One job, with its tasks. |
| `ListJobs`, `WatchJobs` | | Every job on record, newest first. `WatchJobs` re-sends the whole list when any job changes, progress included. |
| `CancelJob` | yes | Stop a job that has not finished. |
| `WatchJobEvents` | | What has happened to one job, then what happens next, ending when the job does: the steps of its life and the lines its tasks log. Pass `after_seq` to pick up where you left off. |
| `GetModelConfig` | | Which model the node plans with, and whether a key is set. Never the key. |
| `SetModelConfig` | yes | Set it. `keep_api_key` changes the rest without sending the key again. |
| `ListProviders` | | The kinds of model service there are: Ollama, Anthropic, and OpenAI and whatever speaks as it does. Each with its usual address, whether it wants a key, and whether its models are fetched before use. |
| `ListModels` | for a named service | What a service offers, each model with its size and whether it can call tools. With no `service`, the configured one; with one, the service described, before anything is saved. |
| `PullModel` | yes | Fetch a model to the machine its service runs on (Ollama). A stream of progress, ending when the model is there. |
| `RemoveModel` | yes | Delete a fetched model. |
| `Ask` | yes | Put a question to the planner. A stream of what it does; see below. |
| `ListChats`, `GetChat`, `DeleteChat` | yes | Conversations on record, newest first; one in full; forget one. |
| `StoreFile` | yes | Put a file in the pool's store and keep it. A stream from you: the first message carries `name`, every message may carry `data`. Returns its `cid`, which is what a job takes as input. |
| `ListFiles` | | The files stored this way, newest first: name, CID, size, when. |
| `FetchFile` | yes | The content under a CID, as a stream: a stored file, or anything in a job's `output_blobs`. |
| `RemoveFile` | yes | Stop keeping a file. |
| `CreateInvitation` | yes | An invitation for another machine to join with, as a worker or as a client that only submits jobs. Works once; lasts an hour unless `ttl_seconds` says otherwise. |
| `ListMembers` | | The nodes admitted to the pool, with role and when they joined. |
| `RemoveMember` | yes | Take a node out of the pool and disconnect it. |
| `JoinPool` | yes | Join another node's pool with its address and an invitation from it, and start working for it. No restart. |

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

- `state`: pending, running, succeeded, failed or cancelled. The same apply to each task.
- `progress`: from 0 to 1, on the job and on each task. A task that does not report stays at 0 until it succeeds.
- `tasks`: one entry for each piece of the job, with the worker that ran it, how many attempts it took, and why the last one failed if one did.
- `params` and `result` are bytes. For the two built-in workloads they are JSON.
- `created_at_ms` and `finished_at_ms` are milliseconds since the Unix epoch; the second is zero until the job ends.

To submit one of the built-in workloads:

| Workload | `params` | Result |
| --- | --- | --- |
| `primes` | `{"from":0,"to":2000000}` | `{"count":148933}` |
| `wordcount` | `{"input":"<CID of a stored file>"}` | Counts, and the CID of the full table in `output_blobs`. |
| `transcode` | `{"input":"<CID of a stored video>","height":720}` | `{"output":"<CID of the encoded video>","segments":8,"bytes":5181092}`, the CID also in `output_blobs`. Needs a worker started with `--containers`. |
| `container` | `{"image":"alpine:3.20","command":["echo","hi"]}` | For each task, the CID of what it printed and of each file it wrote. Needs a worker started with `--containers`; without one the job waits. |

`min_memory_bytes` and `min_gpus` send its tasks only to workers that have that much. `max_tasks` says how many pieces to split into; zero means one for each free worker slot. `task_timeout_seconds`, if set, stops and retries any attempt that runs longer.

## What a job event tells you

Each has a `seq` (counting from one within the job), a time, a `kind`, the task it concerns (`-1` for the job as a whole), the worker concerned if one was, and some text.

| `kind` | When |
| --- | --- |
| `submitted`, `resumed` | The job arrived; the coordinator restarted with it unfinished |
| `task-started`, `task-succeeded` | A task was handed to a worker; it finished |
| `task-failed`, `task-lost`, `task-timed-out` | An attempt failed, its worker dropped off, or it ran too long |
| `log` | A line the task itself wrote |
| `succeeded`, `failed`, `cancelled` | How the job ended. Always the last event |

A job view can be built from `WatchJobs` for state and progress and `WatchJobEvents` for the timeline and log of the job that is open.

## A chat view

`Ask(chat_id, text)` streams `AskEvent`s. Leave `chat_id` empty to start a conversation; every event carries the ID, so the first one tells you what it is called.

| `kind` | Show it as |
| --- | --- |
| `text` | The next piece of the assistant's reply. Append `text` to the bubble. |
| `call` | The model asked for a tool: `tool` is its name, `text` its arguments as JSON. |
| `job` | A job was started: `job_id`. Link it to the jobs view; `WatchJobEvents` gives its progress and log. |
| `result` | What the tool returned, as JSON in `text`. |
| `done` | The answer is complete. Always last. |

If the planner fails, the stream ends with an error instead of `done`, and what was said up to then is still saved. `GetChat` returns a conversation as messages with a `role` of `user`, `assistant` or `tool`; an assistant message may carry `calls` instead of text.

Before any model is set, `Ask` fails with `FAILED_PRECONDITION` and a message saying so, which is the cue to show the model picker.

### The model picker

Everything a picker needs comes from the node, so the app holds no list of providers or models of its own.

1. **Pick the provider.** `ListProviders` gives the cards: `name` and `about` to show, `default_url` to prefill the address, `needs_key` to say whether to ask for a key (`SUPPORT_YES`: always; `SUPPORT_UNKNOWN`: offer the field, do not require it; `SUPPORT_NO`: hide it), and `fetches_models` to say whether to offer downloads. Ollama is first because it needs no account.
2. **Pick the model.** `ListModels` with `service` set to what the user has entered so far lists that service's models without saving anything. Show `label` if there is one, else `name`, with `size_bytes` where given. A model whose `tools` is `SUPPORT_NO` cannot plan: it would answer without computing anything, so grey it out and say why. `SUPPORT_UNKNOWN` means the service does not say; allow it. An error here is the service's own, for instance a refused key or nothing listening at that address, and is worth showing as it is.
3. **Or download one.** Where `fetches_models` is set, offer a field for a model's name (for Ollama, anything from its library, such as `llama3.1:8b`) and call `PullModel`. Each message carries `status`, and for the parts that are measured `completed_bytes` of `total_bytes`, which restart for each part: show the status and a bar for the current part. The stream ending without an error means the model is there; list the models again. Cancelling the call stops the download. `RemoveModel` deletes one.
4. **Use it.** `SetModelConfig` saves the choice. When the user later changes only the model, send `keep_api_key`; the same flag in `service` lets `ListModels` use the saved key, which the node sends only to the provider and address it was saved for.

Anthropic may show as having no key set and still work: with none saved, the node uses the key Anthropic's own tools are set up with on that machine.

## Files and invitations

**Files.** A file picker maps onto `StoreFile` directly: send the file in pieces of any size up to a few megabytes (256 KiB is what the daemon itself uses), with the name in the first. The same content stored twice is one file, under its latest name. The answer's `cid` goes into a job's parameters, for example `{"input":"<cid>"}` for `wordcount` or `container`. When the job finishes, each entry of `output_blobs` is fetched with `FetchFile`. `FetchFile` needs the token even though it changes nothing, because it returns what the files contain.

A stored file is kept until `RemoveFile`. If the node was started with `--max-store`, `StoreFile` fails with `RESOURCE_EXHAUSTED` when the file would not fit.

**Inviting a machine.** `CreateInvitation` returns one string. The person at the other machine starts their node with it:

```sh
sisyphusd run --role worker --coordinator <this node's address> --join <invitation>
```

Show the invitation with a copy button and its expiry. Once used, the machine appears in `ListMembers`, and in `ListWorkers` while it is connected. An invitation is the only secret involved, and it works once.

These calls exist only on a node that coordinates a pool. On a worker-only node they fail with `FAILED_PRECONDITION`.

## Private files and jobs

Set `private` in the first `StoreFile` message and the file is sealed before the pool holds it. Set `private` on `SubmitJob` and everything the job stores is sealed too, and it can read private files. `FetchFile` unseals what this node sealed, so the UI treats private and ordinary files alike; `File.private` and `Job.private` are there to show a lock.

What this protects: the pool's other members, and anyone who gets at the store, hold only what they cannot read. What it does not: the workers that run a private job's tasks are given the key for the length of the job, as they must be to do the work. Run private jobs on workers you trust with the data.

The key is one the node makes the first time it is needed and keeps in `private.key` in its data directory. It is an ordinary key: `sisyphusd blob get --key-file <data dir>/private.key <cid>` reads the same files. **If that file is lost, so is everything sealed with it.** A private job cannot read a file that was not stored as private by this node, and a job that is not private cannot read one that was.

## Joining someone else's pool

`JoinPool` takes the other node's address as `host:port` and an invitation its owner made (`CreateInvitation` on their side, or `pool invite`). On success this node is a worker of that pool and starts taking its tasks within a moment, beside its own work; the peer appears in `ListPeers` with `takes_work` and then `this_node_works_for` set. To leave, set `takes_work` to false with `SetPeerComputePermissions`.

It needs a node that runs a worker, which is every node unless it was started with `--role coordinator`. An invitation for a client is accepted too, and then no work is taken.

## What the API does not have yet

- **Submitting jobs to a pool this node has joined as a client.** `SubmitJob` gives work to this node's own pool.
- **Choosing the key** private files are sealed with, or having more than one.

## Changing the API

`node.proto` is shared. After editing it:

1. `make proto` regenerates the Go side.
2. `apps/sisyphusd-rs/src/rpc.rs` must implement any new call, even if only to say it is not supported, or the Rust daemon stops building.
3. Add new fields and calls; do not renumber or remove existing ones.
