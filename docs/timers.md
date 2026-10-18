# Timers in the daemon

Every place the Go code waits on a clock, and why. A timer is not a fault in itself. Plenty of work is about time: a record that expires, a heartbeat, a retry after a failure. What the event-driven work in [#393](https://github.com/sisyphus-network/Sisyphus/issues/393) removed is state polling, looking again and again at something that could have said when it changed.

Each loop is put in one of six kinds:

- **Event**: woken by the change it acts on. A timer, if any, only bounds how often it sends.
- **Deadline**: waits for the next moment something falls due, and is re-armed when that moment moves.
- **Liveness**: tells another node, at a steady rate, that this one is still there.
- **Retry**: tries again, after a pause, what has just failed.
- **Maintenance**: clears out, now and then, what has outlived its keeping.
- **Anti-entropy**: compares this node's view with another's on a timer, to catch a change that no event reported. It is a backstop beside the events, not a replacement for them.

A wait that bounds a single call (a request's timeout, a listening window, a grace period before a kill) is not a loop and is listed separately at the end. Paths are relative to `apps/sisyphusd` unless they start with `packages/`. Tests shorten the intervals, as [`development.md`](development.md#tests) describes. The daemon loops are below; desktop/browser timers are inventoried at the end.

## Loops

| Where | What it waits for | Kind | Interval | Why it is a timer |
| --- | --- | --- | --- | --- |
| `record.go`, `recordKeeper.run` | A change to what the node publishes about itself, or its record reaching half its lifetime | Event and deadline | Half of `recordLifetime` | A record expires whether or not anything changed, so its renewal is due by the clock (#394). |
| `coordinator/coordinator.go`, `watchAttemptExpirations` | The nearest deadline of a running task attempt | Deadline | Each job's task timeout | Re-armed whenever an attempt starts, ends or is cancelled, and when a worker leaves. Nothing scans the running work (#395). |
| `worker/worker.go`, `passOn` | A running task's progress or log changing | Event | At most one update every `updateGap` (250 ms) | The gap only holds a task that logs in a tight loop to a few messages a second. An idle task sends nothing (#397). |
| `daemon.go`, `replayFilePinsOnChanges` | A change to the queue of chat attachment pins | Event, with retry | Backoff from 2 s, doubling to 1 min, only after a failure | The pin store gives no notice when it recovers, so a failure is tried again on a timer (#396). |
| `worker/worker.go`, `Worker.session` (heartbeat) | The interval | Liveness | `defaultHeartbeatInterval`, 5 s | The coordinator tells a lost worker from a quiet one by its heartbeat. The heartbeat also carries how many tasks are running and any change to what the worker offers. |
| `worker/worker.go`, `Worker.Run` (reconnect) | A lost connection to the coordinator | Retry | Backoff from 1 s to 30 s | A coordinator that is away gives no notice when it is back. |
| `daemon.go`, `poolSwarm.adopt` | A failed move to the pool's new private-network key | Retry | `adoptRetry`, 5 s | Tried again until it succeeds or the node stops. |
| `p2p/p2p.go`, `Host.call` | A failed dial to a node that has just been found | Retry | Random, 150 to 300 ms, three tries | Two nodes that dial each other at the same instant both fail. Waiting a different time each lets one get through. |
| `daemon.go`, `collectPeriodically` | The interval | Maintenance | `--gc-interval`, 1 h; 0 turns it off | Deletes stored data nothing pins and prunes old job records. Nothing becomes garbage at a moment worth an event. |
| `cluster.go`, `clusterPins.run` | A change to the store's pins, or the interval | Event and anti-entropy | `clusterSync`, 1 min | IPFS Cluster can change without the store hearing of it, as when a peer restarts. The minute catches that. |
| `p2p/p2p.go`, `Host.keepPlaces` | A change to the pool's relays, a connection made or lost, or the nearest renewal or retry | Event and deadline, with retry | Renewal at half a reservation's life; a failed attempt again after `PlaceCheck`, 10 s | A reservation expires whether or not anything changed, and a relay that did not answer gives no notice when it is back. Nothing is rescanned while all is well (#410). |
| `worker/peers.go`, `Members.RunRelayWatch` | The coordinator's relay stream ending | Retry | Backoff from 1 s to 15 s, back to 1 s after a stream that delivered a list | The stream itself carries every change to the relays (#410). The timer only reconnects it. |
| `replication/follower.go`, `Follower.Run` | The interval, or at once after a change it made | Anti-entropy | `replication.Interval`, 1 min | A storage follower asks its coordinator which blobs to hold. The coordinator has no stream to push that list on. See the candidates below. |
| `reciprocal.go`, `reciprocity.workFor` and `followTrust` | A local trust or peer-address change; the authenticated peer's initial trust snapshot and live changes; a reconnect or work retry becoming due | Event, with retry | No periodic trust check; reconnect and work retry back off from 1 s to 1 min | The protocol-v2 trust stream reports the current admission state first, then every change. If it ends, guest work stops and reconnect gets a fresh snapshot; address and connection changes wake reconciliation. Older coordinators do not provide reciprocal guest work until upgraded (#414). |
| `pinning/client.go`, waiting for a remote pin | The interval, while a remote pinning service is still pinning | Retry (status poll) | The caller's `every` | The IPFS Pinning Service API has no notification, so status is the only way to learn a pin is done. It stops once the pin is pinned or failed. |
| `packages/kubo/daemon.go` and `packages/ipfscluster/daemon.go`, start | The subprocess answering its API, or its ready timeout | Readiness | Every 20 ms (Kubo) and 50 ms (Cluster) | Neither program says when it is ready, so its API is tried until it answers. Bounded by `ReadyTimeout`. |

## Waits bounded to one call

| Where | What it bounds |
| --- | --- |
| `pinning/service.go`, `promptness` (5 s) | How long a pin request waits to answer `pinned` at once, before it answers `queued`. |
| `mcpserver/mcpserver.go`, `listenFor` (2 s) | How long a running job's events are listened to before an agent is given what came. |
| `packages/kubo/daemon.go` and `packages/ipfscluster/daemon.go`, `stopGrace` (10 s) | How long a subprocess has to exit after an interrupt before it is killed. |

## Candidates

One remaining anti-entropy loop has an event that could take over its normal case, keeping its timer as a backstop:

- **Follower lists.** A coordinator already holds a stream to each worker. Sending a "your list changed" notice on it would let a follower ask straight away instead of within the minute. That needs a stream contract first, one that keeps what the minute gives today: signed-list recovery, blob challenges, convergence after a reconnect, and follower liveness. It is a design question for after V0 ([#413](https://github.com/sisyphus-network/Sisyphus/issues/413)), and the minute stays until it is settled.

Relay places were moved to events and deadlines in [#410](https://github.com/sisyphus-network/Sisyphus/pull/410); reciprocity was moved to the trust stream in [#414](https://github.com/sisyphus-network/Sisyphus/pull/414).

## Desktop and browser client timers

These are UI lifecycle, retry, or feedback waits; none is a steady-state node snapshot poll. Node/peer state uses an initial fetch followed by the shared stream. A retry fetches a new baseline only after the stream fails or ends.

| Where | What it waits for | Kind | Interval | Why it is a timer |
| --- | --- | --- | --- | --- |
| `apps/sisyphus/src/main/index.ts`, `scheduleReconnect` | A failed daemon connection or state stream | Retry | Exponential, 1 s to 15 s; reset after `getNodeInfo` succeeds | Restarts the Electron connection and obtains a fresh node baseline. |
| `apps/sisyphus/src/renderer/src/lib/node-api.ts`, browser state session | A failed/ended daemon stream | Retry | 1.5 s | Re-fetches the browser snapshot before reopening the stream; no snapshot calls while the stream is healthy. |
| `apps/sisyphus/src/renderer/src/pages/AccessPage.tsx`, paired-device stream | A failed/ended `WatchPairedBrowsers` stream | Retry | Exponential, 500 ms to 30 s; reset on update | Reopens the change stream after failure. |
| `apps/sisyphus/src/renderer/src/pages/NodeDetailsPage.tsx`, worker stream | A failed/ended `WatchWorkers` stream | Retry | Exponential, 500 ms to 30 s; reset on update | Reopens the worker change stream after failure. |
| `apps/sisyphus/src/renderer/src/pages/AccessPage.tsx`, invitation countdown | Invitation time remaining | User-facing countdown | 1 s while an invitation is open; otherwise 60 s | Refreshes displayed expiry time, not daemon state. |
| `apps/sisyphus/src/renderer/src/pages/AccessPage.tsx` and `NodeDetailsPage.tsx` | Temporary copied-code/identity confirmation | User feedback | 1.6 s | Clears transient confirmation UI. |
| `apps/sisyphus/src/renderer/src/App.tsx` | Persisting scroll position after a scroll | Debounce | 120 ms | Coalesces frequent user scroll events before a local-storage write. |
| `apps/sisyphus/src/renderer/src/pages/SettingsPage.tsx` | Loading models after connection inputs change | Debounce | 650 ms | Avoids issuing a model-list request for every keystroke. |
| `apps/sisyphus/src/renderer/src/pages/WorkspacePage.tsx` | Waiting before globe initialization | UI transition | 240 ms (0 with reduced motion) | Lets the view settle before initializing the visual. |
| `apps/sisyphus/src/renderer/src/components/navigation/advanced-sidebar.tsx` | Sidebar transition completion fallback | UI transition | 210 ms | Advances the sidebar's visual phase. |
| `apps/sisyphus/src/renderer/src/pages/OperationsPage.tsx` | Releasing a generated download URL | Resource cleanup | 1 s | Revokes a temporary browser object URL after download starts. |
| `apps/sisyphus/src/main/daemon-process.ts` | Local API readiness and graceful daemon shutdown | Readiness / deadline | 750 ms socket probe; bounded shutdown timeout | These bound process lifecycle calls; they do not refresh application state. |

Stream reconnection and stale-generation behavior are covered by `apps/sisyphus/dev/node-state-session.test.mjs` and `apps/sisyphus/dev/browser-node-state-session.test.mjs`; retry bounds are covered by `apps/sisyphus/dev/reconnect-backoff.test.mjs`.
