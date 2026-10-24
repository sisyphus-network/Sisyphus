# Developing Sisyphus

## Setup

You need Go 1.27 or newer. `go.mod` names `toolchain go1.27.2`, the first 1.27 release with the fixes for this module's reachable standard-library advisories. The Go command fetches it on its own unless `GOTOOLCHAIN=local` is set, as the Nix shell sets it, and `setup-go` installs it in CI and for releases. To change the protocol you also need `protoc` (the generated code was last made with 36.2; get it from the protobuf releases on GitHub, since distributions carry old ones) and the plugins that `make tools` installs.

Some tests run a real IPFS daemon and need the `ipfs` program ([Kubo](https://github.com/ipfs/kubo/releases)) on the PATH; CI uses the version pinned in `.github/workflows/ci.yml`. Without it `make test` skips those tests, and `make cover` fails each of them, naming the program, since the code they exercise would go unrun. Each test daemon gets a repository of its own in a temporary directory and never touches `~/.ipfs` or a daemon already running on the machine.

CI downloads those pinned releases with a bounded retry for transient GitHub API/network errors. The Kubo SHA-512 and IPFS Cluster SHA-256 checks still run after download; a checksum mismatch or other permanent download error still fails the job.

The Docker-backed container-task integration test allows up to a minute for three tasks to finish. If it times out, it records any remaining `sisyphus-*` containers, whether the `alpine:3.20` image is still present, and Docker's own `create`/`start`/`die`/`destroy` events for that image since the job was submitted (with exit codes; read with `docker events --until` and a 10-second limit, so it cannot wait), then cancels the job and captures its event log. Tasks run with `--rm`, so the Docker events are what tell a container that never started from one that started and exited, and both from a task/engine stall or a scheduling delay.

The longer real-image workload checks (transcode, molecular dynamics, render, speech transcription, quantum chemistry, text from scans, trajectory analysis and the like) run only with the `realimages` build tag; without it each skips itself (`requireRealImages`). The ordinary suite leaves them out, so image builds do not use up its package timeout. `make cover` runs in two passes: the ordinary suite (`make cover-main`, writing `coverage.out`), then `packages/runtime` with `-tags=realimages` under the race detector (`make cover-realimages`, writing `realimages.out`, with a 60-minute timeout). `make cover-check` joins the two profiles and fails unless every statement ran in one or the other. CI runs the two passes as separate jobs side by side. To run just the runtime package with its real-image tests, use `make cover-realimages`, or `go test -tags=realimages ./packages/runtime`.

The same goes for the tests of `--cluster`, which run real cluster peers and need `ipfs-cluster-service` ([IPFS Cluster](https://github.com/ipfs-cluster/ipfs-cluster/releases)) on the PATH as well, at the version pinned in the same file. `ipfs-cluster-ctl` is not needed. Setting `SISYPHUS_REQUIRE_KUBO`, `SISYPHUS_REQUIRE_CLUSTER` or `SISYPHUS_REQUIRE_DOCKER` makes a test that finds its program missing fail instead of being skipped. CI sets all three, and so does `make cover` unless one is set already: `make cover SISYPHUS_REQUIRE_DOCKER=` skips the Docker tests, and then reports what they would have covered.

```sh
make build   # bin/sisyphusd
make test    # go vet, then the tests under the race detector, allowing each package 25 minutes
make cover   # fails unless the tests execute every statement of hand-written code
make lint    # go vet and staticcheck, for mistakes the compiler lets through
make demo    # a three-node pool and a few jobs on this machine, in one terminal
make proto   # regenerate packages/protocol and packages/client after editing proto/
make fmt
```

To check dependencies for known vulnerabilities, run `go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...` (older releases, 1.1.4 for one, crash on Go 1.27's syntax). It reports one advisory that has no fix: [GO-2024-3218](https://pkg.go.dev/vuln/GO-2024-3218), censorship of content by peers that abuse a Kademlia DHT, in `go-libp2p-kad-dht`. A node builds a DHT only with `--discover`, and it is Sisyphus's own table, under its own protocol prefix (`apps/sisyphusd/p2p`, `tablePrefix`), not the IPFS one. Its peers are the nodes that discover each other there.

CI runs formatting, `go vet`, staticcheck, the regular suite under the race detector, the ordinary coverage pass, the runtime package's real-image workload tests under the race detector in a job of their own beside it, a check that the two passes together cover every statement, a build and the container rehearsal on every pull request, and for the desktop app its typecheck, unit tests, build and end-to-end tests.

A pull request that changes only documentation skips the tests. One that touches neither the packages, the daemon, `go.mod` nor the workflows skips building the workloads on other systems (`workload-architecture`, `workload-elsewhere`), and desktop packaging builds on three of its six systems for every pull request. Adding the `all-platforms` label runs everything, everywhere: `.github/workflows/all-platforms.yml` re-runs the pull request's CI and desktop packaging, which read the label from the pull request. The two workflows do not start on labels themselves, because a label's run would list its skipped jobs under the same names as the real run's and hide a failure. For a pull request from a fork, add the label and re-run its CI by hand.

### What is tested how

| Layer | What it is here | Run with |
| --- | --- | --- |
| Formatting | `gofmt`, which has one style and no options | `make fmt` |
| Static checks | `go vet` and [staticcheck](https://staticcheck.dev), pinned in the `Makefile` | `make lint` |
| Unit tests | Go's own `testing` package, in `_test.go` files beside the code. A package's logic is tested alone, with stand-ins for what it talks to | `go test ./packages/...` |
| Integration tests | The same tool. `apps/sisyphusd`'s tests start real nodes in the test process, and where a test needs them, the real `ipfs`, `ipfs-cluster-service` and Docker | `make test` |
| Coverage | Every statement of hand-written Go must be run by some test, with Kubo, Docker and the cluster required so nothing is skipped | `make cover` |
| The race detector | The regular suite and the runtime package with its real-image workload tests | `make test`; `make cover-realimages` (CI's `realimages` job) |
| Across machines | `examples/rehearsal.sh`: each node in a container with its own address and disk, 26 checks; CI runs it on every pull request | `examples/rehearsal.sh` |
| Desktop unit tests | Node's own test runner over the app's modules | `npm test` in `apps/sisyphus` |
| Desktop end to end | Playwright driving the built Electron app against a node of its own; Settings selectors cover short-viewport placement and keyboard dismissal, Pool limits unit menus and Location & privacy sharing at desktop heights in LTR and RTL, Hebrew RTL controls, interface pickers in Access and pool hosting, and theme-mode switching, persistence and narrow-window fit | `npm run test:e2e` in `apps/sisyphus` |
| Forecast accuracy | The `forecast` workload scored on the M4 competition's 100,000 series against its Naive2 benchmark, as the competition scored its entries ([`forecast-benchmark.md`](forecast-benchmark.md)). Needs the M4 data, which is not in the repository, so CI does not run it | `SISYPHUS_M4=<dir> go test -run 'TestM4$' -v ./packages/runtime/forecastbench` |
| Agents | A live trial of the MCP server from Claude Code, by hand, recorded on [#88](https://github.com/sisyphus-network/Sisyphus/issues/88) | by hand |

For an animated UI step, wait for evidence that the step started before waiting for its idle state. An initially false `data-zooming` value is not proof that a click's state update has rendered. The globe's monotonic `data-zoom-step` counter makes that transition observable even when reduced motion makes the animation brief.

A node a test starts does not look for other nodes unless the test is about that, and then only on a discovery network of the run's own (`--discovery-network`). The rehearsal's nodes do the same. So no test meets the real nodes on the machine it runs on or on its network, and two runs at once never meet each other ([#533](https://github.com/sisyphus-network/Sisyphus/issues/533)).

What has no automated test: the desktop app has no coverage measure, and nothing drives the app on Windows or macOS beyond building its package and starting its daemon.

## Where things are

| Path | What it holds |
| --- | --- |
| `proto/sisyphus/v1` | Message and service definitions. The source of truth for the protocol. |
| `proto/sisyphus/node/v1` | The local API between a node and the desktop app on the same machine. Shared by the daemon and the app. |
| `packages/protocol` | Go generated from `proto/`. Never edited by hand; committed so a fresh clone builds without `protoc`. |
| `packages/ai` | Talking to language models: one interface, with adapters for Ollama, for OpenAI's dialect, and for Anthropic. |
| `packages/geo` | Which country an address is registered in, from a table carried in the program, and whether an address is a global one that says where anything is. |
| `packages/hardware` | What a machine has: processor, memory, graphics cards. Reads Linux's `/proc`, asks `sysctl` on macOS, PowerShell on Windows, and `nvidia-smi` on all three; unknown elsewhere. |
| `packages/identity` | A node's key pair and the ID derived from it; signing and verifying; the TLS settings nodes connect with. |
| `packages/ipfscluster` | Starting and stopping an IPFS Cluster peer beside the node's Kubo, and calling its REST API. |
| `packages/kubo` | Starting and stopping a Kubo daemon beside the node, and calling its API. |
| `packages/names` | The signed records behind names, which are IPNS records: making one with a node's key, checking one against the name, and telling which of two is newer. No network. |
| `packages/natpmp` | Asks the machine's default gateway, selected by the system on Linux, macOS, the BSDs and Windows, by NAT-PMP's one read-only request, which address it shows the internet. |
| `packages/job-model` | Job and task state machines, which blobs a job consumed and produced, and for a job that is verified, the results its workers returned and whether enough of them agree. No I/O, no locks. |
| `packages/jobrecord` | A finished job as linked data: the record's layout, building it from a job, reading it back. No I/O in the building, so the same job always gives the same CID. |
| `packages/nodedb` | The node's SQLite database, where a coordinator keeps its jobs, its members, the invitations it has issued, the requests made of it as a pinning service and the records of the names it answers for. Schema changes are new numbered files in `migrations/`, never edits to old ones. |
| `packages/runtime` | The `Workload` interface, the built-in workloads, and the recorder that notes what a workload reads and writes. |
| `packages/s3` | A bucket in an S3-style object store, for a store that keeps its blocks there. Its tests run against a real one, SeaweedFS in a container. |
| `packages/sealed` | Encrypting a private job's blobs: the format, sealing and seekable unsealing. |
| `packages/storage` | The blob store: IPFS-compatible import, pins, garbage collection, verification. Also holds nodes of linked data (DAG-CBOR), which a pin keeps without keeping the blobs they link. |
| `packages/stun` | Asks a STUN server the user names, by a single Binding request, which address the machine's traffic reaches it from. |
| `apps/sisyphusd/coordinator` | Scheduling, retries, aggregation, worker connections, pinning a job's data. `verify.go` has the rules of verification by replication and of spot checks. |
| `apps/sisyphusd/worker` | Connecting to a coordinator, running tasks, the blob cache. |
| `apps/sisyphusd/api` | The gRPC server and the client-facing services. |
| `apps/sisyphusd/access` | Which nodes have been admitted and in what role, invitations, and the check made on every call. |
| `apps/sisyphusd/tunnel` | Carrying a TCP connection inside a gRPC stream, which is how a worker's Kubo, and its cluster peer, reach their coordinator's without a port being opened for them. |
| `apps/sisyphusd/p2p` | The node's libp2p host. It shares the node's one port with gRPC, relays between members on a coordinator, and reaches a member by its ID. The Go counterpart of the Rust daemon's `networking` module. |
| `apps/sisyphusd/mcpserver` | The node as a Model Context Protocol server: 44 tools in three tiers (19 that look, 14 more that use the pool, 11 more with `--admin`) over the local API and, for what the node keeps, its names and its jobs' records, over its main address, for agents other than the node's own. Tested against a real node and against one that fails. |
| `apps/sisyphusd/planner` | The planner's loop: the model is asked, the tools it asks for are run, and it is asked again with what they returned. Tested against a scripted model. |
| `apps/sisyphusd/blobclient` | Uploading and downloading blobs, checked against their CIDs. |
| `apps/sisyphusd/replication` | Copies of a pool's stored data on its storage followers. The coordinator's side says which followers hold each pinned blob, by rendezvous hashing, signs what it tells each to hold, fetches a blob back, and takes back what a list it signed names when it has lost its store; the follower's side holds what it is told and keeps the last signed list. Tested without a network, and in `apps/sisyphusd` with real nodes. |
| `apps/sisyphusd/pinning` | The IPFS Pinning Service API, both ways: the service a coordinator offers with `--pinning-listen`, and the client behind `blob pin-remote`. The service is tested against a store and a Kubo that can be made to fail; the real `ipfs pin remote` commands are run against a node in `apps/sisyphusd/pinning_test.go`. |
| `apps/sisyphusd/api` | The node APIs. Its browser-facing local API registers the generated gRPC service through Vanguard's Connect v2 registrar and serves Connect and gRPC-Web; its web tests cover the JSON wire format, streaming calls and browser authorization. |
| `apps/sisyphusd` | The `sisyphusd` command: flags, startup, the command-line client. |
| `apps/sisyphus` | The desktop app: Electron and React. `npm ci`, then `npm run typecheck` and `npm run build`. |

## Tests

Every statement of hand-written code must be executed by some test; `make cover` and CI enforce it. Generated code is not counted. In practice this means:

- **Failure paths need tests too.** Most of them are reached by playing a misbehaving peer: `protocol_test.go` speaks the worker protocol to a real coordinator by hand, `worker_test.go` scripts a coordinator, and the blob tests use nodes and stores that fail on request.
- **A check that cannot fire should not be written.** If an error cannot happen, do not branch on it; say why in a comment instead.
- **Time is passed in, not waited for.** Garbage collection takes the current time as an argument so tests can ask what happens an hour or a week from now.
- **Timing-dependent tests are a bug.** Coverage and results must be the same on every run.
- **Give must-happen events a load-tolerant bound.** Waits for an event that should occur (such as a reconnect or a retry) use the package's `eventualWait` bound, not a one-second deadline; tests for something that must *not* happen keep a short observation window. A passing test returns as soon as the event arrives.
- **Intervals are shortened, not waited out.** What a real node does every few seconds or minutes (a worker reporting in, a follower asking what to hold, a host checking its place on a relay, a cluster comparing pins) is a package variable that a test makes shorter for its own length, with helpers such as `replicateQuickly`, `quickCluster`, `relayQuickly` and `reportQuickly`. A test then waits for the outcome with `waitFor` or `waitForOutput`, not with a sleep. [`timers.md`](timers.md) lists every timer in the daemon and why it is one.

Tests that need a node start a real one in-process on a loopback port (`startPool`, `startDaemon`). Every node in a test has its own key and connects over TLS like a real one; `cli` runs a command as the owner of the test node it is addressed to, and `as` runs one from another node's point of view.

A new call is open to the node's owner only until it is added to the table in `apps/sisyphusd/access/enforce.go`. Add a row to `TestEachRoleMayMakeOnlyItsOwnCalls` with it.

## The protocol

Edit the `.proto` files, run `make proto`, and commit the generated code with the change. Never renumber or reuse a field number; only add fields.

## Workloads

A workload implements `Split`, `Execute` and `Aggregate` in `packages/runtime`. `Split` and `Aggregate` run on the coordinator, `Execute` on a worker. Small values travel inline; anything large goes through the `Blobs` handle and is referred to by CID.

- Make results deterministic, so that two runs of a job can be compared by CID.
- Store a job's result in `Aggregate`. Blobs stored by tasks are treated as intermediate and released when the job ends unless `Aggregate` stores them again, or the workload names them in `ResultBlobs` (`runtime.ResultBlobs`), as `md`, `render` and `ocr` do for the files their results point at.

## Storage and IPFS

`packages/storage` is built from Boxo, the library set that Kubo, the main IPFS implementation, is itself built from. A blob gets the CID that `ipfs add --cid-version=1` gives the same bytes; the tests check the CIDs against values taken from a real Kubo node.

A store keeps its blocks in one of several places behind the same interface: files laid out as Kubo lays them out (`OpenLocal`, `OpenCache`), memory, or a running Kubo daemon (`OpenKubo`). Splitting files into blocks, pins, garbage collection and verification are the store's own and identical on all of them; only where a block is put and fetched differs. With Kubo the store talks to the daemon's HTTP API, one block per call.

Blobs move between nodes in one of two ways. Without Kubo, a worker downloads over Sisyphus's own protocol, from another worker if the coordinator knows of one that holds the blob and serves it (`--serve`), otherwise from the coordinator, and uploads results to the coordinator the same way. With Kubo on both, the pool's daemons form a private IPFS network, closed to anyone without its swarm key, and a worker's store simply asks its Kubo for a block: Kubo fetches it from whichever member has it. Uploading results to the coordinator still uses Sisyphus's protocol in both cases. Nodes never join the public IPFS network.

Tests that involve a swarm start several real Kubo daemons each and take a few seconds apiece; `packages/kubo` is the slowest package to test for that reason.

### IPFS Cluster

With `--cluster` a node runs `ipfs-cluster-service` beside its Kubo (`packages/ipfscluster`), as the same peer once more, and `apps/sisyphusd/cluster.go` ties it to the rest:

- **One cluster per pool, with nothing new handed out.** The cluster's secret is a hash of the swarm key, so a member that has been given the one can work out the other, and a change of key restarts both programs (`poolSwarm.recluster`). A worker learns from `PoolService.Swarm` whether its coordinator runs a peer, and reaches it through `TunnelService.Open` with the target `TUNNEL_TARGET_CLUSTER`.
- **Who decides.** The cluster uses its CRDT consensus, which suits members that come and go. Every peer's `trusted_peers` holds the coordinator's ID alone, and a worker's peer runs with `follower_mode`, so it refuses changes over its own API as well.
- **Pins.** `clusterPins` mirrors the store: `Store.OnPinChange` nudges it, it compares `Store.Kept` with what it knows the cluster to pin and makes the calls that differ, and every minute it asks the cluster afresh. Its pins are named `sisyphus`; any other is not its to touch. A pin asks for the coordinator and `--replicas` others as its holders at most and, at least, that many or as many members as there are, since the cluster refuses a pin whose least it cannot meet.
- **The peer's API** is on a loopback port picked before it starts, with a password made for that run and written to its `service.json`. The program cannot say which port it chose for itself, which is why one is chosen for it.

The tests of all this that run real programs are few and slow: one in `packages/ipfscluster` forms a cluster of three, loses a holder and sees it replaced; two in `apps/sisyphusd` run pools of two and three nodes. The rest use a stand-in for the peer's API. In tests the members' heartbeat is a second rather than fifteen (`clusterHeartbeat`). The pool tests wait for the workers to join the cluster with `waitForClusterMembers`. It logs how long each took, and if one never shows up it fails naming the late node, how long the others took and what `pool cluster` last said. That shows whether a slow runner (#314) delayed one node or all of them.

`TestAPoolRunsAContainerOnEachTask` runs a container job on a pool and gives it a minute. If the job misses that, the test records what Docker saw: the job's containers and their events, every network event, a probe container started with no network, and the end of the runner's own Docker daemon log (`/tmp/dockerd.log` in CI), each bounded in time and redacted. If the probe itself took more than two seconds, the host's Docker daemon was stalled, holding up every container alike. Then the test is skipped, and its output says `SKIPPED, NOT PASSED` with the probe's time, since the container path was not exercised to the end. A healthy probe and a missed deadline still fail the test. On the CI runners such stalls most likely reflect pressure on the disk the runners share, though no stall log has yet confirmed it: a container printed nothing for a minute, then finished with the others the moment the daemon caught up (#314).

## Releases

Push a tag such as `v0.1.0`. The release workflow builds every supported platform with `scripts/build-release.sh`, packages the desktop app on Linux, Windows and macOS (an AppImage, an installer and a disk image, and a plain archive of each), and publishes all of it with one `SHA256SUMS` file. The desktop packages are named by the version in `apps/sisyphus/package.json`, so set that to the tag's before tagging. Nothing is signed by a publisher; `docs/user-guide.md` says what people see because of it. Only Linux on x86-64 is covered by the tests.

### Icons

Every icon is drawn from `assets/logo.png` by `scripts/make-icons.py` (it needs Pillow), which sets the logo on a dark rounded plate so that it shows on a light taskbar. It also draws the tray icon in each node state into `apps/sisyphus/build/tray/`, at 16 pixels and `@2x`: the plated logo with a ring (connecting) or a slash (offline) badge, and for macOS a black template image of the globe, which the menu bar tints. What it writes is committed. After changing the logo:

```sh
python3 scripts/make-icons.py
(cd apps/sisyphusd && go run github.com/tc-hib/go-winres@v0.3.3 make --in winres/winres.json --arch amd64,arm64)
```

The second line rebuilds `apps/sisyphusd/rsrc_windows_*.syso`, which Go links into the Windows daemon by itself. They give `sisyphusd.exe` its icon and the name ("Sisyphus node") that Windows shows in its firewall prompt and in a file's properties. The desktop app's packages take theirs from `apps/sisyphus/build/`.

## Branches and merging

- `dev` is the default branch. Nothing is pushed to it directly: work goes on a branch, in a pull request against `dev`.
- A pull request merges once its checks are green, by **squash**, so that `dev` gets one commit for each pull request and every commit on it has passed. The branch is deleted on merging. (The first forty or so pull requests were merged with merge commits, which is why some early commits on `dev` carry a failed check: they are intermediate commits that a later commit in the same pull request put right.)
- `integration` is kept identical to `dev`, as a fixed name to build the desktop app against.
- CI runs again on `dev` after each merge, but at most one `dev` run is in progress and one waits: a newer merge replaces the waiting run rather than queuing behind it, so the latest commit is always checked without a backlog of older ones holding the self-hosted runners. A commit replaced this way has no run of its own on `dev`; it passed as its pull request's head, and the next run checks it together with what merged after it.
- Before pushing, run `gofmt -l apps packages skills` (nothing should be listed), `make cover`, and `go test -race -count=3` on any package with new tests: the coverage run checks neither formatting nor tests that depend on timing or order.
- When one piece of work builds on another that has not merged yet, its branch is cut from the earlier branch and its pull request targets that branch. Once the bottom of such a chain merges, point the next pull request at `dev`.
