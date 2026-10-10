# Security model

What Sisyphus protects today, what it does not, and whom each party has to trust. It describes the code on `dev` as it is: a pool of invited nodes run by people who know each other (V0), not an open network of strangers.

Nothing here has had an independent security review. The sealing of private data in particular is waiting for one ([#91](https://github.com/sisyphus-network/Sisyphus/issues/91)); the [last section](#for-a-reviewer-of-the-sealing) is written for that reviewer. [`SECURITY.md`](../SECURITY.md) says how to report a vulnerability.

Paths are relative to the repository root. Links into [`apps/sisyphusd/README.md`](../apps/sisyphusd/README.md) lead to the detail; where that README and this document differ, the code decides.

## In short

- **A pool keeps outsiders out.** Every connection between nodes is TLS 1.3 with both ends identified by their node keys, and every call is checked against the caller's role.
- **A pool does not keep its members from each other.** Any member can fetch any stored blob it can name. Any client can read, cancel and delete what any other client submitted or stored.
- **The coordinator is trusted with everything**: every job's parameters and results, every private job's key, the member list, and how each job is split and combined.
- **A worker sees everything it is given to compute on**, private or not, and can return anything. Verification catches a worker that disagrees with others, not workers that agree to lie.
- **A private job** keeps stored data from members that are not the coordinator or one of its workers. It does not seal the job's parameters, small result or error messages.
- **A worker that runs containers runs whatever image and command a client names.** It is confined by Docker's defaults, with capabilities dropped and privilege gain blocked, and by no sandbox beyond that.

## Who the parties are

| Party | What it is | What it is trusted with |
| --- | --- | --- |
| Owner / coordinator | Whoever holds a coordinating node's `node.key` | Everything in its pool: members, jobs in the clear, private jobs' keys, the swarm key, what is pinned, splitting and combining. Nothing checks a coordinator. |
| Worker | A node admitted with `pool invite` | The payload, inputs and key of every task it is handed. Any blob in the pool by CID, and the swarm key. It cannot read jobs or list pins. |
| Client | A node admitted with `pool invite --role client` | Submitting jobs, and reading and cancelling the jobs it submitted; pinning, and listing and releasing its own pins; collecting stored data nothing pins; where copies are held, of everything pinned; the swarm key, and so any blob by CID. Another client's job is, to it, a job that is not there (`api/server.go`, `own`). |
| Storage follower | A worker started with `--replica-dir`, or a worker's IPFS Cluster peer | A copy of what the coordinator pinned, which it can read unless sealed. It cannot change what is kept. |
| Local user | Anything running on a node's machine | Without the token: what the node is, and its files' names, members, peers and workers. With `api.token`: everything the local API does. With the data directory: the node. |
| Desktop app | `apps/sisyphus`, a client of the local API | The token. Its window is sandboxed from its main process (`apps/sisyphus/src/main/index.ts`); the bridge between them has not been reviewed. |
| AI agent | A program driving `sisyphusd mcp` | What the flags leave it; see [below](#an-agent-on-the-mcp-server). |
| Model service | Ollama, an OpenAI-style service or Anthropic, set with `model set` | Every planner conversation in full: the questions, the names and CIDs of attached files, each job's parameters and its result. |
| Pinning service | A service asked with `blob pin-remote` | The CID, the name given, and the addresses and IDs of the pool's Kubo daemons unless `--no-origins`. Not the data, unless it holds the swarm key. |
| Relay | The coordinator, or a worker started with `--relay` | Who connects to whom, when and how much. The connections it carries are libp2p's own encrypted ones between the two members. |
| HTTP gateway | `--gateway-listen` on a node | Serves unsealed files by CID to holders of the API token, or to anyone with `--gateway-open`. |

**The node's own files.** `node.key` is the identity; `node.db` holds jobs, members, hashed invitations, unfinished private jobs' keys, planner conversations and the model service's API key in the clear; `api.token`, `pinning.token`, `kubo.token`, `private.key` and `swarm.key` are secrets. All are created readable by their owner only (`0600`, the directory `0700`). Nothing is encrypted at rest, and nothing uses the operating system's keychain. With `--kubo`, the node key is also written into Kubo's own configuration (`packages/kubo/daemon.go`, `configure`).

**The local API** (`apps/sisyphusd/api/local.go`) listens on a loopback address, which the daemon insists on, without TLS. Calls that change something, fetch a file's content, or read jobs or planner conversations need the token (`localService.authorize`): a job's parameters, result and log (`GetJob`, `ListJobs`, `WatchJobs`, `WatchJobEvents`) are read with it, the conversations that run as `chat` jobs among them. The rest need nothing: any program on the machine, under any user, can list the node's files by name and CID, its members, peers and workers. Served to web pages with `--web-listen`, every call needs the token, the page's origin must be listed, and the request must name the machine by a loopback address (`api/web.go`, `NewWebHandler`).

**With `--kubo`**, the node's Kubo daemon takes API calls on a loopback TCP port of its own, with no password unless asked for (`packages/kubo/daemon.go`, `configure`). A program on the machine that finds the port can read everything the node stores that is not sealed, and pin and unpin in it. Started with `--kubo-api-secret` as well, Kubo answers only a caller that shows a secret the node keeps in `kubo.token`, readable by its owner only; the `ipfs` command then needs `--api-auth` to be used on the repository. It is off by default because that command, used plainly, is how people look at what a node holds, and it cannot be combined with `--cluster`: an IPFS Cluster peer pins through the same API and has no setting for a secret. The cluster peer's own API has a random password.

### An agent on the MCP server

`sisyphusd mcp` holds the node's API token and, for some tools, connects to the node with its key (`apps/sisyphusd/agent.go`). The agent is given neither. What it can do is set by flags ([Agents](../apps/sisyphusd/README.md#agents)):

| Flag | What it holds the agent to | What it does not |
| --- | --- | --- |
| none | No tools that reconfigure the node | It can run jobs, including any container image on workers that allow containers, and read and write files under the directory it was started in. |
| `--read-only` | Only tools that look (`mcpserver/more.go`, `offers`) | Looking covers every job's parameters, results and logs. |
| `--files-under <dir>` | `store_file`, `fetch_file` and the other tools that touch disk stay inside it, links followed (`server.allowed`) | Which files inside it the agent reads or overwrites. |
| `--images a,b` | A `container` job submitted with `run_job`, or a `container` step of a `graph` at any depth, must name a listed image, and `ask_planner` is refused, since the planner chooses what to run (`server.permitted`, `server.listed`) | What a listed image is told to do: the command, the input and the environment are the agent's. |
| `--admin` | Nothing: it adds inviting and removing members, joining pools, trust, the model, garbage collection and restore | An agent with it can admit anyone to the pool. |

An agent reads job results and files, which are data from other parties. Nothing separates what it reads from what it then decides to do.

## Identity and admission

Detail: [Who can connect](../apps/sisyphusd/README.md#who-can-connect).

- **Node keys.** Ed25519, made on first run, in `node.key` (`packages/identity/identity.go`). The ID is the libp2p peer ID of the public key. A copied key is the same node; a key cannot be rotated or revoked, only its node removed.
- **Connections.** TLS 1.3, each side presenting a self-signed certificate for its node key (`packages/identity/tls.go`). The server requires a client certificate and accepts any (`ServerTLS`); who the caller is comes from the certificate's key (`PeerID`). The client refuses a server whose key is not the ID it expected (`ClientTLS`). There is no certificate authority and no host name check.
- **Roles.** Each call lists the roles that may make it, and one that is not listed is the owner's alone (`apps/sisyphusd/access/enforce.go`, `allowed`). A node has one role. Role is checked when a call begins; a stream already open is not ended by removal, apart from the worker's own connection.
- **Invitations.** The inviting node's ID and a 128-bit one-time token, good for an hour unless `--ttl` says otherwise. The joiner connects only to the node the invitation names, so the token cannot be taken by someone in between. The node keeps a SHA-256 of the token in `node.db`, so reading the database admits nobody and an invitation survives a restart (`access/list.go`, `Invite`, `Redeem`). Whoever first presents a token is admitted under whatever key they hold: an invitation is a bearer secret until used.
- **Trusting a found peer.** The owner can also admit a discovered node as a worker with no invitation ([Trust](../apps/sisyphusd/README.md#trust)). Being discovered gives a node nothing else ([Finding other nodes](../apps/sisyphusd/README.md#finding-other-nodes)).
- **Removal.** `pool remove` takes the node off the list, ends its worker connection and, with `--kubo`, changes the swarm key (`api/pool.go`, `removeMember`). A worker serving its cache with `--serve` may go on serving the removed node for up to a minute. The removed node keeps everything it had fetched and every private job key it was handed.
- **The private swarm.** With `--kubo` the pool's Kubo daemons share a 256-bit pre-shared key, handed to every member, worker or client, that asks (`PoolService.Swarm`). Its holder can read everything on that network that is not sealed. `pool rekey` and `pool remove` replace it; the IPFS Cluster secret is SHA-256 of a label and the swarm key, so it changes with it (`apps/sisyphusd/cluster.go`, `clusterSecret`). Re-keying shuts holders of the old key out of the network. It does not take back what they had fetched.
- **Names are labels.** A node's `--name` is whatever it says. A `chat` or `prompts` job that asks for a model on a named worker (`model@rig`) goes to a worker that calls itself `rig`. To have a particular node run it, ask by node ID (`model@12D3KooW…`): the coordinator labels each worker with the ID of the key it connected with, and takes no worker's word for who it is (`coordinator.go`, `labelsOf`).

## Data confidentiality

### A plain job

| What | Who can read it |
| --- | --- |
| Parameters, result, each task's state and error, log lines | The coordinator, every client, any program on the coordinator's machine through the local API. They are kept in `node.db` for `--keep-jobs`. |
| A task's payload and the inputs it opens | The worker handed the task. A `container` task's payload is the whole of the job's parameters, environment included. |
| Stored blobs: inputs, outputs, intermediates | Every member that knows the CID, from the coordinator (`BlobService.Get` is open to workers and clients) or, with `--kubo`, from the private network. Followers and cluster holders have copies. |
| The job's [record](../apps/sisyphusd/README.md#a-jobs-record) | Clients are told its CID; it is a blob like the others, and it names the CIDs of the job's data, the submitter and every worker. |

A CID cannot be guessed, and workers are not offered a list of them. That is the only thing between a member and another member's unsealed data. Whether a member's Kubo can learn CIDs from the private network's own traffic has not been examined.

### Private jobs

Detail: [Private jobs](../apps/sisyphusd/README.md#private-jobs). A private job is one submitted with a 32-byte key. Blobs read and written through the job are sealed with it (`packages/runtime/sealed.go`, `Sealed`): sealed inputs are opened, and everything a task or the combining step stores is sealed before it reaches a store.

- **Who holds the key.** The submitter; the coordinator, from submission; every worker handed one of the job's tasks, in the assignment itself (`coordinator.go`, `handLocked`). Verifying a private job hands it to more workers. A graph gives it to every step. A key once handed out cannot be taken back.
- **Where it is kept.** In the coordinator's `node.db` while the job is unfinished, so that a restart can take the job up. A finished job is saved without it (`packages/nodedb/jobs.go`, `SaveJob`), with SQLite's `secure_delete` on and the write-ahead log truncated. The disk may still hold traces. Workers hold it in memory only.
- **What is not sealed.** The parameters, the result the job reports, each task's inline output and error messages. These are readable as in the table above. A failed `container` task's error quotes the last line its command printed to standard error.
- **Logs are withheld.** A worker does not pass on what a private job's task logs, which for a `container` is every line its command prints; it sends one line saying so (`apps/sisyphusd/worker/worker.go`, `reports.Log`). That is the worker's doing: one on a build without it, or one that chooses to, sends them, and the coordinator keeps what it is sent.
- **What still shows of sealed data.** That it is sealed, its exact size, and equality; see determinism below.
- **Sealing is deterministic.** The same key and data give the same bytes and so the same CID. Someone who sees two blobs sealed with one key can tell whether they are identical, and whether 64 KiB chunks at the same position in them are. A blob's CID confirms a guess at its whole content only to someone with the key.
- **On a worker.** A `container` task's input is written unsealed to a temporary directory for as long as the job's tasks run there, and its output files sit unsealed in another until the task ends (`packages/runtime/container.go`, `Execute`, `sharedInputs`).
- **Unsealed inputs pass through.** A job with a key reads a blob that is not sealed as it is, so a private job does not prove its inputs were private.

**The record of a private job** leaves out the parameters, result, outputs and error messages, and holds a 32-byte commitment in place of each (`packages/jobrecord/jobrecord.go`, `Commit`): HMAC-SHA-256, under a key derived from the job's key, of the job's ID, the commitment's path and the value. Without the key a commitment does not confirm a guess. The record still shows the workload, the requirements, the times, who submitted, which workers ran what and which agreed, and the CIDs and so the sizes of the sealed data.

### Private files, and attachments in a chat

The desktop app's "private" is one key per node, `private.key`, made on first use (`apps/sisyphusd/desktop.go`, `sealingKey`). Every private file and every private job of that node is sealed with it.

- A worker handed any one private job from that node holds the key to all of its private files and to every other private job's data, past and future. There is no key per file or per job, and no way to change the key that keeps old data readable.
- Equality of blobs and of aligned chunks shows across everything the node has sealed.
- The desktop app stores a file attached to a planner conversation as a private file. Its name and CID go to the model service in the message; its content does not. Every job the planner runs in that conversation is made private (`apps/sisyphusd/planner/planner.go`, `hasPrivateAttachments`), so the workers get `private.key`, and the job's result goes back to the model service unsealed.
- The model chooses the jobs. A model service, or text that steers the model, can have a `container` job with `"network": true` run over an attached file on a worker that allows containers.

### Conversations with the pool's models

A `chat`, `prompts` or `embed` job carries its prompts in its parameters, or in a stored file they name, and its answers in its result. Parameters and result are unsealed, readable as any plain job's, and kept for `--keep-jobs`. The worker serving the model reads all of it ([Thinking on the pool](../apps/sisyphusd/README.md#thinking-on-the-pool)).

### On the wire

TLS and libp2p hide content from outsiders. They do not hide that nodes are talking, to whom, how much or when. The pinning service (`--pinning-listen`) and the gateway (`--gateway-listen`) are plain HTTP, and neither is held to a loopback address, since both are for other machines to use. Reached from another machine, the pinning service's key, or the node's API token where the gateway is not `--gateway-open`, crosses the network unencrypted. The node says so in its log when it starts either at such an address (`daemon.go`, `warnOfKeyInTheClear`); it does not refuse, and TLS is left to a proxy in front.

## Result integrity

Detail: [Verifying results](../apps/sisyphusd/README.md#verifying-results), [Spot checks](../apps/sisyphusd/README.md#spot-checks).

- **By default there is none.** The coordinator takes what a worker returns.
- **Stored bytes are checked against their CID** on upload and download, so nobody can substitute a blob under a name. This says nothing about whether the blob is the right answer.
- **`--verify N`** runs each task on `N` different node IDs and takes a result once `N` have returned the same output bytes and the same stored CIDs (`apps/sisyphusd/coordinator/verify.go`, `returnedLocked`).
- **`--verify-share`** verifies a random share of tasks, drawn from the system's randomness at submission (`shuffled`). A worker outvoted on one task has its other tasks in that job verified after all.
- **A worker's standing** is kept by node ID from one job to the next: how many of its results for verified tasks agreed and how many were outvoted. One that was outvoted is on probation until ten of its results have agreed, and while it is, a task handed to it in a job that asked for spot checks is verified (`coordinator/verify.go`, `countLocked`). It changes nothing in a job that did not ask for verification.
- **Private jobs are verified the same way**, because sealing is deterministic.

What none of it catches:

| Not caught | Why |
| --- | --- |
| Workers that agree on a wrong result | `N` matching results are believed. |
| One operator with several nodes | Different node IDs are not different people, and the owner admits whom it likes. |
| A wrong result in a task a spot check did not pick | A share `s` catches a single wrong result with probability `s`. |
| Honest non-determinism | Models, `transcode`, and containers that use time, randomness or the network differ between honest workers; the job fails. |
| The coordinator | It splits the job and combines the outputs itself (`Split` and `Aggregate` run on the coordinator, `packages/runtime/workload.go`). A wrong split, a wrong combination or an altered result is not detectable by anyone. |
| A lying worker's cost | Probation, which costs it nothing but being checked. No stake and no ban, and it is handed tasks like any other. Removal is by hand. An honest worker outvoted by ones that agreed to lie lands on probation too. |

**Job records.** A finished job's history is content-addressed, so a record cannot be changed without changing its CID. `job record --verify` has the coordinator rebuild the record from its own database and compare. That detects a database or store altered after the job ended. It proves nothing to someone who does not trust the coordinator: the record is not signed, and "verified" in a receipt means only that this coordinator says so many workers agreed. Checking a private record's commitments means sending the job's key to the coordinator again (`coordinator/record.go`, `Record`).

## Running other people's work

Detail: [Running containers](../apps/sisyphusd/README.md#running-containers).

**Built-in workloads** (`primes`, `wordcount`, `chat`, `prompts`, `embed`) are code in the daemon; their parameters are data. `transcode` runs ffmpeg in a container with a fixed image and command, on an input the submitter chose. `graph` runs on the coordinator and submits other jobs.

**The `container` workload** is off unless a worker's owner passes `--containers`. It then runs `docker run` with these arguments (`packages/runtime/container.go`, `Execute`):

| Set by the worker | Left to the job's submitter |
| --- | --- |
| `--rm`, a random name | The image, pulled from wherever its name says |
| `/input` read-only, `/output` writable, each a temporary directory | The command and environment |
| `--user` as the node's own user and group, and with it `--cap-drop ALL` and `HOME=/tmp` | `--network none`, unless the job says `"network": true` |
| `--security-opt no-new-privileges` | `--memory` and `--cpus`, absent unless the job sets them |
| `--pids-limit 4096` | `--gpus` |

What that does not do:

- **The image is whatever the submitter names.** The name is checked only so far that Docker cannot read it as an option: one that begins with a dash, or has a space or control character in it, is refused when the job is submitted and again by the worker (`checkImage`). Before that check a job could add Docker options of its own, mounts and `--privileged` among them; a worker on a build without it is still open to that. Naming an image by digest pins what runs only if the submitter wants it pinned.
- **No sandbox beyond that.** The root filesystem is writable, nothing limits what is written to the container's own layer or to the worker's disk through `/output` while the command runs, and there is no user namespace or sandboxed runtime: the container shares the worker's kernel. A node run as root runs containers as root, though with no capabilities; where the system has no user numbers, the image's user is used and Docker's usual capabilities are kept, since an image that installs what it needs as root would not run without them.
- **Limits are the submitter's, not the worker's.** A worker's `--offer-memory-mb` is what it advertises and is not enforced.
- **Output is bounded, loosely.** What the command prints is kept up to 16 MiB as the task's output. Each line of it is also a log line, cut at 4 KiB by the worker and again by the coordinator, which keeps 20,000 lines for a job and then says once that it keeps no more (`coordinator.go`, `logLocked`). Files left in `/output` are stored up to 4 GiB and 1,024 files for a task, beyond which the task fails; the limit is on what is stored, not on what the command may write while it runs.

So a worker with `--containers` must trust every client of every pool it works for, and those pools' coordinators, as it would someone it lets run programs of their choosing in a Docker container with default settings, on its network if they ask. A node that takes work from several pools ([Trust](../apps/sisyphusd/README.md#trust)) keeps their stores apart, not their containers.

**What a malicious job can do to a worker:** with containers, the above. Without, it can spend the worker's slots, fill its cache where `--max-cache-bytes` is not set, and feed crafted input to the built-in workloads and to ffmpeg.

**What a malicious worker can do to a job:** read every input, payload and key it is handed and keep them; return a wrong result; name blobs as stored that it did not store, which are no longer taken as stored unless the coordinator's store holds them, and at most 4,096 for a task; fail tasks, or hold them for as long as it likes where the job set no `--timeout`; claim hardware, models and a name it does not have to attract tasks; fetch any other blob in the pool it learns the CID of; and upload without limit unless the coordinator sets `--max-store-bytes`.

## Storage and availability

Detail: [How long data is kept](../apps/sisyphusd/README.md#how-long-data-is-kept), [Copies on other nodes](../apps/sisyphusd/README.md#copies-on-other-nodes), [IPFS Cluster](../apps/sisyphusd/README.md#ipfs-cluster-pins-held-by-several-nodes).

- **Pins decide what is kept**, and garbage collection deletes the rest, hourly and on `blob gc`. The node's own pins are held for `user`, and each client's for `user:<its node ID>` (`api/blobs.go`, `pinOwner`): a client lists and releases its own and no other's, and the node itself sees and releases them all. A client that is removed leaves its pins behind, for the node's owner to release.
- **Without `--replicas`**, the coordinator's disk holds the only copy anything promises to keep. A worker's cache is a cache.
- **With followers or a cluster**, other nodes hold copies of what is pinned, in the form it is stored: sealed data sealed, the rest readable by the holder. A follower is taken at its word that it holds a copy until the copy is fetched and fails its check. The coordinator signs the lists it gives followers, which is what lets it take data back after losing its store without trusting them ([When the coordinator's store is lost](../apps/sisyphusd/README.md#when-the-coordinators-store-is-lost)).
- **If the disk with the coordinator's data directory dies**, the pool loses `node.key`, `node.db`, `private.key` and `swarm.key`, which are copied nowhere: the identity, the jobs and members, and the key to every private file. Followers still hold blobs; getting them back without the node key is by hand, and sealed ones are of no use without their key.
- **Nothing limits use.** There are no per-member quotas or rate limits on jobs, uploads or pins; `--max-store-bytes` is one limit for the whole store. Any member can make a coordinator busy.
- **The gateway** only reads, serves single files by CID or by a node's name, needs the API token unless `--gateway-open`, and refuses a blob that begins with the sealed header whatever the caller shows (`api/gateway.go`, `NewGateway`). With `--gateway-open`, a CID is the only secret. The token may be sent as `?token=` in the address, where logs and browser history keep it.
- **Deletion is local.** Unpinning removes the coordinator's copy and, in time, followers'. It does not reach what a worker cached, what a removed node kept, or what a pinning service fetched.

## Not protected against today

| Threat | Today |
| --- | --- |
| A dishonest coordinator | Not protected. It reads every job, holds every private job's key, and can alter any result or record. |
| A worker reading the data it computes on | Not protected, private jobs included. |
| Members reading each other's unsealed data | Protected only by CIDs being unguessable. |
| Clients interfering with each other | A client reads and cancels only the jobs it submitted, and releases only its own pins. Not protected: a client still fetches any blob it knows the CID of, sees which CIDs are pinned by asking where copies are held, and can start garbage collection. |
| Colluding workers, or one operator with many nodes | Not protected. |
| Wrong results from non-deterministic work | Not detectable. |
| A client attacking a container worker's machine | Docker's defaults, no capabilities, no privilege gain, a process limit. No sandbox. |
| Programs on a node's machine reading jobs and conversations | Not protected for reads of the local API; changes need the token. |
| A stolen node key | No rotation or revocation; remove the node. |
| Denial of service by a member | No quotas or rate limits. |
| Traffic analysis | Not protected. |
| Loss of the coordinator | Data may survive on followers. The pool, its jobs and its private keys do not. |
| Proving a result to a third party | Not possible: records are unsigned. |
| Flaws in the sealing | Unreviewed. |

**What the open network in [#22](https://github.com/sisyphus-network/Sisyphus/issues/22) would have to add.** Strangers as workers and clients remove the assumption everything above rests on, that the owner knows whom it admitted. At the least: a sandbox for work that holds against its submitter (gVisor, Firecracker or Kata are named there), with the worker and not the job setting network, mounts and limits; separation of clients from each other, with per-client ownership of jobs and pins, and quotas; access to blobs by something other than knowing a CID; a key for each private job or file and a way to say which workers may be given it; an answer for a coordinator that is itself a stranger, starting with signed records; and handling of abuse. None of it makes data confidential from the machine that computes on it. That needs trusted hardware or cryptography the project does not have, and should be said to requesters plainly.

**What the research in [#19](https://github.com/sisyphus-network/Sisyphus/issues/19) would have to add.** Replication is the only method in the code. Open are: resistance to colluding workers and to one operator with many identities, which needs identities that cost something or workers chosen in a way the submitter cannot be gamed on; a consequence for a worker caught beyond being checked more, which needs stake or a reputation that work is handed out by; comparison of results that are not bit-for-bit equal; and verification of the coordinator's own splitting and combining.

## For a reviewer of the sealing

The code is `packages/sealed/sealed.go` (about 330 lines), used through `packages/runtime/sealed.go`. Commitments are in `packages/jobrecord/jobrecord.go`, `Commit`.

**Construction.**

- **Key.** 32 random bytes from `crypto/rand` (`NewKey`), written as 43 characters of unpadded URL-safe base64.
- **Subkeys** (`Key.sealer`). `encKey = HKDF-SHA-256(key, salt none, info "sisyphus sealed blob: encryption", 32 bytes)` and `macKey` likewise with info `"sisyphus sealed blob: nonces"`. Go's `crypto/hkdf`, extract then expand.
- **Format.** The eight bytes `SISYENC1`, then the plaintext in 64 KiB chunks, the last possibly shorter. An empty plaintext is one empty chunk. Each chunk is stored as `nonce (12) || ciphertext || tag (16)`. There is no key identifier, length field or per-blob value in the header.
- **Chunk** (`sealer.seal`). `pos = index as 8 bytes big-endian || 1 byte, 1 if this is the last chunk, else 0` (`position`). `nonce = first 12 bytes of HMAC-SHA-256(macKey, pos || plaintext)`. The chunk is AES-256-GCM under `encKey` with that nonce and `pos` as additional data.
- **Opening** (`Open`, `Reader.load`). The plaintext length and chunk count are worked out from the sealed length. Any chunk is read on its own, with the position and last-chunk flag it must have. Failure is one error, `ErrCorrupt`, for a wrong key and an altered blob alike. The nonce is read from the blob and not recomputed.
- **Commitments.** `commitKey = HKDF-SHA-256(key, salt none, info "sisyphus job record: commitments", 32)`. A commitment is `HMAC-SHA-256(commitKey, len(jobID) || jobID || len(path) || path || value)`, lengths as 8 bytes big-endian, the value last and without a length.

**What is claimed.** Confidentiality of content from anyone without the key, apart from size and the equalities above; detection of altered, reordered, truncated or extended blobs when read with the right key; the same output for the same key and plaintext. No claim is made against the coordinator or the job's workers, who hold the key.

**Questions worth asking.**

1. Is a 96-bit truncated HMAC over position and plaintext sound as a synthetic nonce for GCM? Two different chunks under one key share a nonce with the probability of a 96-bit collision. What limit on data sealed under one key follows, given that the desktop's `private.key` is used for everything a node ever seals?
2. Additional data binds a chunk to its index and to being last, not to its blob. Under one key, a chunk can stand at the same index of another blob, if the last-chunk flags match. Storage is content-addressed, so a reader who has the right CID is not exposed to this. Is there a path where the CID comes from the party that could do the splicing?
3. GCM does not commit to its key, and the header names none. Does anything rely on a sealed blob opening under one key only?
4. `runtime.Sealed` passes an unsealed blob through unopened, and the gateway and `FetchFile` decide a blob is sealed from its first eight bytes. Can either be turned into a downgrade or a confusion?
5. Is the leak from determinism, equality of whole blobs and of aligned 64 KiB chunks under one key, described fully? Is there more, for instance through the nonce being a function of the plaintext?
6. Are the HKDF labels and the absence of a salt adequate separation between the encryption key, the nonce key and the commitment key?
7. Do the commitments hide and bind as claimed, with a value of any length placed last?
8. The key reaches workers in the task assignment, and the coordinator's database until the job ends. Are there copies the code does not account for: logs, crash dumps, swap, the write-ahead log?
9. What should replace one long-lived key per node: a key per file wrapped for the owner, or per job derived from it?
