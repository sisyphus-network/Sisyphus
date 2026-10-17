# Timers in the daemon

Every place the Go code waits on a clock, and why. A timer is not a fault in itself. Plenty of work is about time: a record that expires, a heartbeat, a retry after a failure. What the event-driven work in [#393](https://github.com/sisyphus-network/Sisyphus/issues/393) removed is state polling, looking again and again at something that could have said when it changed.

Each loop is put in one of six kinds:

- **Event**: woken by the change it acts on. A timer, if any, only bounds how often it sends.
- **Deadline**: waits for the next moment something falls due, and is re-armed when that moment moves.
- **Liveness**: tells another node, at a steady rate, that this one is still there.
- **Retry**: tries again, after a pause, what has just failed.
- **Maintenance**: clears out, now and then, what has outlived its keeping.
- **Anti-entropy**: compares this node's view with another's on a timer, to catch a change that no event reported. It is a backstop beside the events, not a replacement for them.

A wait that bounds a single call (a request's timeout, a listening window, a grace period before a kill) is not a loop and is listed separately at the end. Paths are relative to `apps/sisyphusd` unless they start with `packages/`. Tests shorten the intervals, as [`development.md`](development.md#tests) describes. This page covers the daemon. The desktop app's timers (stream reconnects, the pairing countdown, debounce) are covered in #393.

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
| `p2p/p2p.go`, `Host.keepPlaces` | The interval | Anti-entropy | `PlaceCheck`, 10 s | A relay that restarts drops this host's reservation without telling it. This check is what takes it back. See the candidates below. |
| `replication/follower.go`, `Follower.Run` | The interval, or at once after a change it made | Anti-entropy | `replication.Interval`, 1 min | A storage follower asks its coordinator which blobs to hold. The coordinator has no stream to push that list on. See the candidates below. |
| `reciprocal.go`, `reciprocity.pause` | A change to this node's trust, word from another node that its trust changed, or the interval | Event and anti-entropy | `reciprocityCheck`, 1 min | Whether another node trusts this one back is that node's state, so it is asked. Word of a change from either side wakes it at once, and the minute catches word that was lost. |
| `pinning/client.go`, waiting for a remote pin | The interval, while a remote pinning service is still pinning | Retry (status poll) | The caller's `every` | The IPFS Pinning Service API has no notification, so status is the only way to learn a pin is done. It stops once the pin is pinned or failed. |
| `packages/kubo/daemon.go` and `packages/ipfscluster/daemon.go`, start | The subprocess answering its API, or its ready timeout | Readiness | Every 20 ms (Kubo) and 50 ms (Cluster) | Neither program says when it is ready, so its API is tried until it answers. Bounded by `ReadyTimeout`. |

## Waits bounded to one call

| Where | What it bounds |
| --- | --- |
| `pinning/service.go`, `promptness` (5 s) | How long a pin request waits to answer `pinned` at once, before it answers `queued`. |
| `mcpserver/mcpserver.go`, `listenFor` (2 s) | How long a running job's events are listened to before an agent is given what came. |
| `packages/kubo/daemon.go` and `packages/ipfscluster/daemon.go`, `stopGrace` (10 s) | How long a subprocess has to exit after an interrupt before it is killed. |

## Candidates

Two anti-entropy loops have an event that could take over their normal case. Each would keep its timer as a backstop, so neither is urgent:

- **Relay places.** libp2p reports a lost connection to a relay (`EvtPeerConnectednessChanged`). Taking the place back on that event would make recovery immediate instead of up to 10 s late. The timer would stay for a reservation that lapses while the connection survives.
- **Follower lists.** A coordinator already holds a stream to each worker. Sending a "your list changed" notice on it would let a follower ask straight away instead of within the minute. The minute would stay for a coordinator that restarts.

Each would be its own small PR, with tests for the event and for the backstop.
