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

Anything on the machine can **read** what the node is, who it knows and who works for it. Calls that **change** something, and calls that read jobs, conversations or what files contain, need the node's token, sent as `authorization: Bearer <token>` metadata. The app's main process already does this (`authorization()` in `src/main/index.ts`). The token is in `api.token` in the node's data directory.

## The calls

| Call | Token | What it gives |
| --- | --- | --- |
| `GetNodeInfo` | | The node's ID, version, addresses, the workloads it knows, and `country_code`, the country its address is registered in, worked out from a table the daemon carries, with nothing asked of anyone. Each entry of `ListPeers` has a `country_code` too, which is enough for a map. For a node behind a NAT, `country_code` is that of its address as its pool sees it, and `country_source` says how the country was found: `interface` from an address of its own, `pool` from its address as the pool sees it (behind a VPN, the VPN's), `gateway` from the external address its default gateway gave when asked by NAT-PMP (`--locate-gateway`), `stun` from the address a STUN server the user named saw its traffic come from (`--stun-server`), or `lookup` from the outside service it was allowed to ask. Either is empty when nothing better than home-network addresses is known. The desktop labels pool-derived country as “as seen by the pool” so it is not mistaken for the machine's physical location. |
| `GetLocationDiscoverySettings` | yes | Saved and running location-discovery choices, whether the daemon supports gateway lookup, and whether a restart is needed. |
| `SetLocationDiscoverySettings` | yes | Save the daemon owner's opt-in location-discovery choices. They take effect after the daemon restarts. A paired browser uses its existing same-origin session to make the call; the API token is not exposed to browser code. |
| `GetPoolLimits` | yes | Saved and running limits a coordinator holds each member to (`max_jobs_per_client`, `max_tasks_per_client`, `max_task_timeout_seconds`, `max_store_bytes_per_client`, `max_upload_bytes_per_attempt`; 0 is no limit), whether a restart is needed, and whether the node runs the coordinator role at all. |
| `SetPoolLimits` | yes | Save those limits. They take effect after the daemon restarts. Byte limits may be at most 2^63−1, counts at most 2^31−1, and the timeout what a Go duration holds; anything larger is `InvalidArgument`. A paired browser uses its same-origin session, and the desktop connected to a separately managed node uses that node's API token; the daemon checks either the same way. |
| `ListPeers`, `WatchPeers` | | Every node this one knows of: its pool's members and the nodes it has found. `WatchPeers` re-sends the whole list when anything changes. |
| `ConnectPeer` | yes | Connect to a node at an address ending in `/p2p/<node ID>`. |
| `GetBootstrapPeers` | | The address book: nodes connected to at each start. |
| `SetBootstrapPeers` | yes | Replace the address book. |
| `SetPeerComputePermissions` | yes | Set the two sides of trust in a peer, separately. |
| `SetPeerComputeTrust` | yes | Set both sides at once. Kept for clients with one switch. |
| `ListWorkers` | | The nodes connected as this node's workers: name, slots, running tasks, and what each machine has (processor, memory, graphics cards; zero and empty where unknown), `models`, the language models it serves to the pool, and `loaded_models`, those of them its model server had in memory when the worker last said (on starting, after each model task, and when the node changes its models), each with `on_card_percent`: how much of the model is on a graphics card, from 0 to 100, the rest being worked by the processor. `verified_agreed` and `verified_outvoted` count the results it has returned for verified tasks that were, and were not, among those the task was settled by, in every job so far; `probation` is how many agreeing results it still owes after being outvoted, zero if it is not on probation. |
| `ListWorkloads` | | The workloads this node takes jobs of: `name`, `description`, `workers_running_it`, how many connected workers offer it, busy or not, and `composite`, for a graph, which the coordinator runs itself and no worker offers. `fields` is its parameters as a form: each with `name`, its key in the job's JSON parameters; `kind` (file, given as a stored file's CID, integer, number, text, choice or toggle); `label` and `help`; `default_json`, the starting value as JSON, empty for none; `minimum`, `maximum` (each unset where there is no bound) and `step` (zero for none) for a number; `choices` for a choice; and `required`, whether a job is refused without it. A form sent with its defaults is a job the workload accepts. A workload with no `fields` takes its parameters as JSON, as its description gives them. |
| `SubmitJob` | yes | Give the pool a job. Returns it as it stands at once. |
| `GetJob` | yes | One job, with its tasks. It needs the token though it changes nothing, as do the three below: a job's parameters, result and log are its owner's. |
| `ListJobs`, `WatchJobs` | yes | Every job on record, newest first. `WatchJobs` re-sends the whole list when any job changes, progress included. |
| `CancelJob` | yes | Stop a job that has not finished. |
| `WatchJobEvents` | yes | What has happened to one job, then what happens next, ending when the job does: the steps of its life and the lines its tasks log. Pass `after_seq` to pick up where you left off. |
| `GetModelConfig` | | Which model the node plans with, and whether a key is set. Never the key. |
| `SetModelConfig` | yes | Set it. `keep_api_key` changes the model without sending the key again. The key is kept only for the provider and address it was saved with, where an empty address is the provider's usual one: change either and the configuration is saved with no key, which `has_api_key` in the answer shows. |
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
| `ListMembers` | | The nodes admitted to the pool, with their roles (`role` is the first; `roles` all of them, as a node can be both worker and client) and when they joined. |
| `RemoveMember` | yes | Take a node out of the pool, disconnect it, and release the files it kept here as a client. |
| `JoinPool` | yes | Join another node's pool with its address and an invitation from it, and start working for it. No restart. |
| `GetJoinedPool`, `WatchJoinedPool` | | On a node started with `--role worker`: the pool it has joined and how it stands with it. See below. |
| `LeavePool` | yes | On a node started with `--role worker`: leave the pool it works for, and stop. See below. |

The two `Watch` calls are the ones to build live views on. Each message carries a `revision` that counts up from one.

Location discovery preferences are stored by the daemon and apply after restart. On startup, an explicitly supplied `--locate-gateway`, `--locate-country` (including `=false`), or `--stun-server` overrides that field; an omitted flag uses the saved value, then its default. This means an operator can enforce an opt-out on the command line, while a paired browser or Electron can manage saved preferences through the authenticated local API. `GetLocationDiscoverySettings` reports both the saved and effective running values, with `restart_needed` showing when they differ.

Pool limits follow the same rules. An explicitly supplied `--max-jobs-per-client`, `--max-tasks-per-client`, `--max-task-timeout`, `--max-store-bytes-per-client` or `--max-upload-bytes-per-attempt` (including `=0`) overrides that limit; an omitted flag uses the saved value, then its default of no limit. `GetPoolLimits` reports both, with `restart_needed` showing when they differ, which stays so while a flag overrides a saved limit. `coordinator` is false on a node without the coordinator role, where the limits apply to no one. The desktop's **Settings → This node → Pool limits** card manages them through these calls, for the daemon it started, for a separately managed daemon reached with its endpoint and token, and from a paired browser.

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
- `record_cid`: empty until the job is over, then the content ID of the job's record, its history as linked data (see the daemon's README, "A job's record"). It is one short string that names everything about the job, fit to show and to copy. It is also empty for a job that ended before records were kept.

To submit one of the built-in workloads:

| Workload | `params` | Result |
| --- | --- | --- |
| `primes` | `{"from":0,"to":2000000}` | `{"count":148933}` |
| `wordcount` | `{"input":"<CID of a stored file>"}` | Counts, and the CID of the full table in `output_blobs`. |
| `forecast` | `{"input":"<CID of a stored CSV, a value on each line in its last column, oldest first>","horizon":12,"season":12,"folds":4,"models":"all"}`, `models` being `all` or any of `naive`, `mean`, `drift`, `snaive`, `ses`, `holt`, `holtwinters` and `ar`, separated by commas | `{"points":120,"last":"2025-12","horizon":12,"folds":4,"best":{"model":"holtwinters alpha=0.2 beta=0.05 gamma=0.3 season=12","mae":1.45,"rmse":1.76},"forecast":[{"step":1,"value":160.2,"lo80":158.0,"hi80":162.4,"lo95":156.8,"hi95":163.6},…],"candidates":[…],"forecast_table":"<CID of a CSV table>","scores_table":"<CID of a CSV table>","tasks":4}`. Any worker runs it, with no containers, and two workers return the same bytes, so `verify` checks it. |
| `transcode` | `{"input":"<CID of a stored video>","height":720}` | `{"output":"<CID of the encoded video>","segments":8,"bytes":5181092}`, the CID also in `output_blobs`. Needs a worker started with `--containers`. |
| `md` | `{"picoseconds":10,"replicas":8}`, or with `"input":"<CID of a stored PDB file>"`, or an Amber system with `"prmtop"` and `"inpcrd"` (CIDs of its stored topology and coordinates) in place of `input`, and `"gpu":true` to run on graphics cards | `{"replicas":8,"atoms":8867,"total_nanoseconds":0.08,"mean_potential_energy_kj_mol":-115390.2,"runs":[…]}`, each run with the CIDs of its last frame, energies and trajectory. Needs a worker started with `--containers`, and with `gpu` one that has an NVIDIA card Docker can use. |
| `render` | `{"input":"<CID of a stored .blend file>","split":"frames"}`, or `"split":"tiles"` for one frame in strips | `{"split":"frames","frames":240,"images":[…],"archive":"<CID of a zip>","width":1920,"height":1080,"engine":"CYCLES","seconds":5130.2}`; by tiles, `"image"` is the CID of the whole frame. Needs a worker started with `--containers`. |
| `transcribe` | `{"input":"<CID of a stored recording>","language":"auto","model":"large-v3-turbo-q5_0"}` | `{"text":"<CID of the transcript>","subtitles":"<CID of the SRT file>","segments":4,"seconds":61.5,"words":140}`, both CIDs also in `output_blobs`. Needs a worker started with `--containers`. |
| `qchem` | `{"input":"<CID of a stored XYZ or SDF file of structures>","method":"gfn2","job":"opt","solvent":"none"}`, and `charge`, `uhf`, `convergence` and `tasks` | `{"method":"gfn2","job":"opt","structures":3,"converged":3,"lowest":0,"lowest_energy_hartree":-13.665,"csv":"<CID of the table>","xyz":"<CID of the optimised geometries>","results":[…]}`, each structure with its energy, relative energy, gap, dipole and the CIDs of its geometry and, for `hess` and `ohess`, its frequencies; all of them also in `output_blobs`. Needs a worker started with `--containers`. |
| `ocr` | `{"input":"<CID of a stored PDF or PNG, JPEG or TIFF picture>","language":"eng","output":"text+pdf","dpi":300}` | `{"pages":120,"text":"<CID of the text>","pdf":"","pdfs":[…],"archive":"<CID of a zip of the PDFs>","characters":291404,"language":"eng","tasks":16}`: the pages' text in order with a form feed between pages, and a searchable PDF for each task's run of pages, or `"pdf"` whole when one task read them all. The PDFs are also in `output_blobs`. Needs a worker started with `--containers`. |
| `traj` | `{"topology":"<CID of a stored prmtop, parm7 or PDB file>","trajectory":"<CID of a stored DCD, NetCDF or mdcrd trajectory>","analyses":"rmsd,radgyr,hbond","mask":"@CA"}`, or `"trajectories":[…]` in place of `"trajectory"`, and `mask1`, `mask2`, `region` and `tasks` | `{"frames":60,"trajectories":1,"trajectory_frames":[60],"analyses":[…],"series":"<CID of a CSV table, a row for each frame>","rmsd_angstrom":{"mean":0.16,"stdev":0.02,"min":0,"max":0.19},"hbonds":"<CID of a CSV table>","tasks":4}`, with `dssp`, `dssp_residues` and `rmsf` as tables too where asked for. Needs a worker started with `--containers`. |
| `container` | `{"image":"alpine:3.20","command":["echo","hi"]}` | For each task, the CID of what it printed and of each file it wrote. Needs a worker started with `--containers`; without one the job waits. |

`min_memory_bytes` and `min_gpus` send its tasks only to workers that have that much. `max_tasks` says how many pieces to split into; zero means one for each free worker slot. `task_timeout_seconds`, if set, stops and retries any attempt that runs longer.

`verify`, if two or more, has each task run by that many different workers and takes a result only once that many have returned the same one; `Job.verify` gives it back. Zero or one runs each task once, as before. It multiplies the work by that number, is only for work that gives the same result every time it is run, and works with `private` too, where every worker asked is given the key. `SubmitJob` refuses it with `FAILED_PRECONDITION` when fewer workers that could take the job are connected, and a task whose workers cannot agree fails the job with an error that names them. While it runs, a task's `attempt` counts every copy handed out and `peer_id` is the worker last given one; once it has succeeded, `peer_id` is the first worker that returned the result agreed on. The job's events have two more kinds, `task-result` and `task-disagreed`. The desktop app does not offer the field yet.

`audit_share`, from 0 to 1, has the coordinating node run that share of the tasks again itself, picked by chance as results come in, and take its own result where a worker's differs; `Job.audit_share` gives it back, and it is refused with `verify` or for a workload the node does not run. `verify_share`, with `verify`, has only that share of the tasks verified, from 0 to 1, drawn at random and rounded up to a whole task; the rest run once. `Job.verify_share` gives it back, and `JobTask.verify` says how many workers each task is held to: `verify` for one that is verified, 1 for one that is run once. A worker whose result for a verified task is outvoted has the tasks it ran unverified in the job verified after all, so a task that had succeeded can go back to running, with `JobTask.verify` raised and a `task-rechecked` event. The same happens to a task handed to a worker that is on probation (`Worker.probation` above zero) for having been outvoted in another job, so a job can come back from `SubmitJob` with more of its tasks verified than its share. Zero, and one, verify every task. A share outside 0 to 1, or one without `verify`, is `INVALID_ARGUMENT`.

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

A stored file is kept until `RemoveFile`. If the store cannot let it go, `RemoveFile` fails and the file stays listed. If the node was started with `--max-store`, `StoreFile` fails with `RESOURCE_EXHAUSTED` when the file would not fit.

**Inviting a machine.** `CreateInvitation` returns one string. The person at the other machine starts their node with it:

```sh
sisyphusd run --role worker --coordinator <this node's address> --join <invitation>
```

Show the invitation with a copy button and its expiry. Once used, the machine appears in `ListMembers`, and in `ListWorkers` while it is connected. An invitation is the only secret involved, and it works once.

Invitations exist only on a node that coordinates a pool. The file calls work there, and on a worker node that holds the client role in the pool it joined, where they keep files in that pool (see "Jobs in a pool this node has joined" below). On any other node, both fail with `FAILED_PRECONDITION`.

## Private files and jobs

Set `private` in the first `StoreFile` message and the file is sealed before the pool holds it. Set `private` on `SubmitJob` and everything the job stores is sealed too, and it can read private files. `FetchFile` unseals what this node sealed, so the UI treats private and ordinary files alike; `File.private` and `Job.private` are there to show a lock.

What this protects: the pool's other members, and anyone who gets at the store, hold only what they cannot read. What it does not: the workers that run a private job's tasks read that job's data, as they must to do the work. They are given keys to this job's inputs and outputs only, derived from the node's key, and not the node's key itself, so they cannot open the node's other private files or other jobs' data. Run private jobs on workers you trust with the data of those jobs.

The key is one the node makes the first time it is needed and keeps in `private.key` in its data directory. It is an ordinary key: `sisyphusd blob get --key-file <data dir>/private.key <cid>` reads the same files. **If that file is lost, so is everything sealed with it.** A private job cannot read a file that was not stored as private by this node, and a job that is not private cannot read one that was.

## Joining someone else's pool

`JoinPool` takes the other node's address as `host:port` and an invitation its owner made (`CreateInvitation` on their side, or `pool invite`). On success this node is a worker of that pool and starts taking its tasks within a moment, beside its own work; the peer appears in `ListPeers` with `takes_work` and then `this_node_works_for` set. To leave, set `takes_work` to false with `SetPeerComputePermissions`.

It needs a node that runs a worker, which is every node unless it was started with `--role coordinator`. An invitation for a client is accepted too, and then no work is taken. A worker that redeems a client invitation holds both roles: it goes on working, and may submit jobs to that pool as well.

**Which pool a worker node has joined.** A node started with `--role worker --coordinator <address>` answers `GetJoinedPool` with its coordinator's ID and name, the address it reaches it at, its `state` (`CONNECTING` until first welcomed, `CONNECTED`, `DISCONNECTED` once it has lost the connection and is trying again) its `roles` in that pool and, while it is `CONNECTED` and holds the client role, `worker_count`: how many workers are connected to the pool, counting itself. `WatchJoinedPool` sends that at once, and again, whole, each time it changes: on connecting, on losing the connection, when the coordinator says its roles have changed, and when a worker joins or leaves the pool. While the node is `DISCONNECTED`, the ID, name and roles are the last it knew, not live, and `worker_count` is absent, as it is for a node that is only a worker, so a count is never shown that is not live. A node that coordinates its own pool has joined none, and is answered `FAILED_PRECONDITION`. Electron's Node overview fetches this once and then renders `WatchJoinedPool` updates; it keeps the last-known pool identity visible during reconnects and shows the worker count only while connected with the client role.

**Jobs in a pool this node has joined.** On a worker node that holds the client role in its pool, the jobs calls work on that pool:
- `SubmitJob`, `GetJob`, `CancelJob` and `WatchJobEvents` go to the pool's coordinator, as this node, and the coordinator checks the client role on every call. A `private` job is sealed with this node's own key, which goes to the pool with the job, as with `job submit --key-file`.
- `ListJobs` and `WatchJobs` list the jobs this node submitted there, newest first. `WatchJobs` is sent the list again as each of them changes. A job the pool no longer has drops out.
- `Ask` plans on that pool. The model is told only of the workloads its workers run, and the jobs it asks for run there as this node's own. The conversation and the model's key stay on this machine.
- `ListWorkers` and `WatchWorkers` list the pool's workers. They are another pool's machines, so show them in the joined-pool context, not as this node's peers.
- `StoreFile`, `FetchFile`, `ListFiles` and `RemoveFile` keep the user's files in that pool. A stored file goes to the coordinator as this node and is kept there for it as a client, counted against the client's share (`max_store_bytes_per_client`). A `private` file is sealed with this node's key first, so the coordinator holds only ciphertext. The file's name stays in this node's own list, scoped by coordinator ID: switching pools shows only files stored through the current pool, and returning to a pool restores its list. A `RemoveFile` releases the client's pin in that pool without erasing another pool's catalogue entry for the same CID. A file's CID can go straight into a job's parameters. Chat attachments are kept there the same way. The coordinator keeps one pin of each file for this node, and the node remembers who on it wants the file: the user, each chat that has it, and the draft of an attachment not yet sent, for a day. The pin lasts as long as the longest of those, and goes when the last of them lets go. A chat's change to a pin that cannot be made while the node is away is made once it is back. A `RemoveFile` that the store cannot carry out, as while the node is away (`UNAVAILABLE`), fails with the store's own code and leaves the file on the list, to be removed again (#500).

A worker node without the client role is answered `FAILED_PRECONDITION`. While it is not connected to the pool, the answer is `UNAVAILABLE`, so the app can tell "not allowed" from "not reachable".

**Leaving the pool a worker node has joined.** `LeavePool` asks the coordinator to let this node go. The coordinator first releases every file it kept for the node as a client, then takes it out of the pool (#509). Only once the coordinator has confirmed does the node forget the pool: the files it stored there, who on the node held them, the jobs it submitted, the coordinator's addresses, and the coordinator's entry in `known.json`. A chat keeps the files it attached. An attachment shows as gone only once fetching it fails, since the same file may still be in this node's store or in another pool (#518). If the coordinator cannot be reached, the call fails with the coordinator's own code (`UNAVAILABLE` while it is away), nothing is forgotten, and calling again is the retry. If the coordinator has let the node go but the node cannot forget the pool, the call fails `INTERNAL`; the node stays up, and calling again finishes the leave. The node was started to work for that pool, so once it has left, it stops. Started again with the same `--coordinator`, it says it has not joined that pool; start it with another pool's `--coordinator` and an invitation from that pool. A temporary disconnect, or a restart with another `--coordinator` without leaving, forgets nothing, and going back to the pool brings its file list back. A node that coordinates its own pool has joined none, and is answered `FAILED_PRECONDITION`.

## What the API does not have yet

- **Choosing the key** private files are sealed with, or having more than one.
- **Reading a job's record.** The API gives its ID in `record_cid`. The record itself is read with `sisyphusd job record <job-id>`, or `GetJobRecord` on the pool's own API.

## Changing the API

## Graphs

A job whose `workload` is `graph` is several jobs run as one (see the daemon's README, "Jobs made of jobs"). For a client it needs nothing new: its `tasks` are its steps, in order, with `worker_name` set to `coordinator`, and each step is a job of its own in `ListJobs` with `parent_job_id` naming the graph and `step` the step. A jobs view can nest those under their graph, or leave them flat.

## From a web page

A page in a browser cannot speak native gRPC as the desktop app does. Started with `--web-listen 127.0.0.1:50052`, the node serves the same calls in two ways a browser can make them: **Connect** and **gRPC-Web**. The `@sisyphus/client` package generates its typed Connect client from `proto/sisyphus/node/v1/node.proto`.

```js
const res = await fetch("http://127.0.0.1:50052/sisyphus.node.v1.NodeService/ListWorkers", {
  method: "POST",
  headers: { "Content-Type": "application/json", "Connect-Protocol-Version": "1", Authorization: `Bearer ${token}` },
  body: "{}",
})
```

- **Calls that return a stream** (`Ask`, `WatchJobs`, `WatchJobEvents`, `PullModel`, `FetchFile`) work as Connect server streams.
- **Storing a file** is the one call a browser cannot make, since it sends a stream. Instead `PUT /files/<name>` with the file as the body, and `?private=true` to seal it; the answer is the `File` as JSON.
- **API calls need authentication, reading included.** On loopback, the UI can exchange the token from `api.token` for a 12-hour `HttpOnly; SameSite=Strict` cookie. A remote browser pairs with a one-time eight-digit code generated by an already authenticated browser. Codes expire after five minutes and work once; each paired browser receives its own 30-day revocable cookie. Same-origin browser API calls are authorized server-side through that session; the node token is never exposed to browser code. SQLite stores only SHA-256 verifiers for random browser credentials. Cookies are marked `Secure` on HTTPS.
- **Remote access is opt-in HTTPS.** The development `--web-listen` option is loopback-only. To expose the browser UI, use Settings → Access and explicitly enable the node-managed listener on one or more active IP addresses assigned to the node. It uses the same port on each selected interface; wildcard, loopback, multicast, and unassigned addresses are rejected. A self-signed certificate is generated and its SHA-256 fingerprint is shown in Settings. Browsers may require accepting the certificate warning and checking the fingerprint before pairing. The listener serves only the browser UI/API; it does not expose the native gRPC control API or P2P listener. It is off on a fresh node.
- A separately hosted page must still be explicitly listed with `--web-origin`; browser management and pairing routes remain same-origin only.

To serve the built React UI from the node itself, build the desktop renderer and pass its renderer output directory:

```sh
npm --prefix apps/sisyphus run build
bin/sisyphusd run --web-listen 127.0.0.1:50052 --web-ui-dir apps/sisyphus/out/renderer
```

Open `http://127.0.0.1:50052`. The daemon serves the UI, Connect API, SPA routes, and file PUT endpoint from one origin. `--web-ui-dir` requires `--web-listen`; this development listener remains loopback-only. For remote use, enable HTTPS in the running desktop app's **Settings → Access**, then open the configured HTTPS browser address and use **Settings → Devices** to create a QR/code. Revoke devices from the same Devices page. The owner is responsible for firewall/router forwarding and accepting the self-signed certificate on each client.

`node.proto` is shared. After editing it:

1. `make proto` regenerates the Go side and the desktop's TypeScript client in `packages/client`. Commit both with the change.
2. Add new fields and calls; do not renumber or remove existing ones.
