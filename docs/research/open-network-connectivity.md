# Open-network connectivity: discovery, NAT traversal and relays

A source-based status note for [#22](https://github.com/sisyphus-network/Sisyphus/issues/22). It separates mechanisms already wired into `sisyphusd` from deployment choices and real-network evidence that remain. It is not a claim that arbitrary peers can always connect, nor a proposal to make pool admission public.

## What the daemon already does

The current dependency is `go-libp2p v0.50.0` ([`go.mod`](../../go.mod)). The daemon combines several distinct mechanisms:

| Need | Current implementation | What that does not prove |
| --- | --- | --- |
| Find nodes on the same LAN | When `--discovery on`, the host announces and listens with Sisyphus-specific mDNS service names. | mDNS does not cross routers and may be unavailable on managed, guest, or mobile networks. |
| Find a peer by ID beyond the LAN | Discovery-enabled nodes serve and use a Sisyphus-prefixed Kademlia DHT. A node connects to configured starting points, then calls DHT `Bootstrap`. Starting points come from the saved address book and optional `--bootstrap` arguments. | A DHT is not a source of first contact: a fresh node with no reachable starting point cannot join the DHT merely because it is configured to use one. No public bootstrap fleet or its operations are defined here. |
| Establish a connection through NAT | `libp2p.EnableHolePunching()` is enabled. Sisyphus also keeps relay reservations: the coordinator and eligible full nodes provide relays, while workers receive the coordinator and pool relay addresses. | Hole punching is opportunistic and depends on a working relayed connection and compatible NAT behavior. It cannot promise a direct route; the relay remains the fallback. |
| Help another node reach a private node | Relay addresses are supplied by Sisyphus's pool membership/relay machinery (`Via` and `MoreRelays`); full-node relays are preferred and the coordinator is a fallback. | This is not a general public AutoRelay candidate-discovery service. Public relay capacity, operator policy, redundancy and abuse controls are not settled. |
| Decide where to place this node on the globe | Location discovery can ask the gateway, a configured STUN server, or a country lookup, and a local city table can map a public address. | Learning a public address or country is not the same as making an inbound libp2p address reachable. NAT-PMP address discovery is also not a substitute for a relay reservation or a successful inbound dial. |

The relevant wiring is in [`p2p.New`](../../apps/sisyphusd/p2p/p2p.go), and startup seeding is in [`daemon.go`](../../apps/sisyphusd/daemon.go). The user-facing description of discovery and the `--bootstrap` option is in the daemon guide's [Finding other nodes](../../apps/sisyphusd/README.md#finding-other-nodes) section.

## Discovery is not admission

With discovery enabled, an arbitrary libp2p peer can connect to the host and participate in the DHT. The pool's services still apply their own membership and role checks; being discoverable does not grant permission to submit jobs, read pool data, or use a pool relay. This distinction is already stated in the daemon guide's [Finding other nodes](../../apps/sisyphusd/README.md#finding-other-nodes) section. A public DHT would nevertheless expose nodes to unsolicited handshakes and routing traffic, which needs resource and abuse review before deployment.

## What is tested, and what is not

The daemon tests exercise discovery, relay fallback, relay preference, and transition away from a relay when peers can reach one another in an isolated test network. [`docs/multi-machine-test.md`](../multi-machine-test.md) separately calls for a real two-router hole-punch check and asks operators to record whether the route becomes direct or remains relayed. The latter is still a manual V0/hardware check; an in-process test is not evidence that consumer routers, carrier-grade NAT, or restrictive firewalls will punch through.

The current tests and code therefore establish that libp2p mechanisms are wired and that the expected paths work in their test topology. They do not establish internet-wide cold-start discovery, reachability under varied NATs, or production relay availability.

## Decisions and evidence still needed

1. **First contact:** choose who operates bootstrap peers, how many are required, how their addresses are distributed and rotated, and how clients fail over when they are unavailable. A public DHT needs this operational entry point; the code's `--bootstrap` flag is a mechanism, not a deployment plan.
2. **Address and reachability policy:** decide which observed/public addresses may be advertised, how stale addresses are withdrawn, and whether the system needs explicit reachability signals. Do not infer routability from a location result.
3. **Relay service:** define who may operate public relays, capacity and abuse limits, discovery/trust rules for relay candidates, and what users see when all relays are unavailable. Current pool relays are membership-oriented, not a public relay marketplace.
4. **Real topology matrix:** record direct and relayed outcomes for two separate NATs, at least one restrictive/CGNAT-style network, IPv4 and IPv6 where available, relay loss, and address change. Keep “could not punch through” as a valid result, not a test failure if relay fallback still works.
5. **Privacy and abuse:** document that discovery exposes peer IDs and advertised addresses to network participants. Align the DHT's public availability with the open-network abuse controls in [`open-network-abuse.md`](open-network-abuse.md).

## Conclusion

The code already uses libp2p DHT/mDNS discovery, hole punching, and Sisyphus-managed relay fallback. The open work in #22 is not simply “add NAT traversal”: it is to decide and operate a reliable cold-start/bootstrap and relay topology, then gather real multi-network evidence and review the exposure of a public DHT. Keep the issue open until those deployment and hardware checks are answered.

## Sources

- Sisyphus source at the current research baseline: [`apps/sisyphusd/p2p/p2p.go`](../../apps/sisyphusd/p2p/p2p.go), [`apps/sisyphusd/daemon.go`](../../apps/sisyphusd/daemon.go), [`apps/sisyphusd/p2p/p2p_test.go`](../../apps/sisyphusd/p2p/p2p_test.go), [`docs/multi-machine-test.md`](../multi-machine-test.md).
- go-libp2p [`EnableHolePunching` option, v0.50.0](https://github.com/libp2p/go-libp2p/blob/v0.50.0/options.go): the library documents hole punching as a way to attempt direct connectivity over an existing relayed connection, with relay discovery/AutoRelay as a separate option.
- libp2p [Hole Punching](https://docs.libp2p.io/concepts/hole-punching/) and [AutoNAT](https://docs.libp2p.io/concepts/nat/autonat/) concepts, read 28 October 2026. These describe the protocol mechanisms, not Sisyphus deployment guarantees.
