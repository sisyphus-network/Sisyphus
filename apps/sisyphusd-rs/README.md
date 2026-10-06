# sisyphusd

`sisyphusd` is the Sisyphus node daemon. It runs headless or underneath a desktop client and owns node identity, P2P networking, local state, and—over time—hardware discovery, scheduling, workload execution, and AI orchestration.

This document covers daemon-specific architecture and development. The product vision, network economy, and V0 scope are described in the [repository README](../../README.md).

## Architecture and boundaries

One daemon binary can run different node roles. Coordinator and worker are capabilities inside `sisyphusd`, not separate services or binaries. A coordinator may also execute work locally; a headless daemon can contribute as a worker.

```text
Clients (Electron / Web / agents)
                 │
       daemon client protocol (planned)
                 │
              sisyphusd
      ┌──────────┼──────────┐
      │          │          │
   storage    network    compute (planned)
```

Keep domain logic independent of the transport, persistence implementation, and operating system. Add interfaces at boundaries where they enable real alternatives or testing; avoid speculative layers for components that do not exist yet.

## Local gRPC API

The daemon exposes a small Protobuf/gRPC API for local clients. Definitions live in the repository's [`proto/sisyphus/node/v1/node.proto`](../../proto/sisyphus/node/v1/node.proto); Rust client/server bindings are generated at build time using `protoc` from the Nix development shell.

V0 exposes:

- `NodeService.GetNodeInfo`: peer ID, daemon version, and listen addresses.
- `NodeService.ListPeers`: connected peers plus saved bootstrap peers, their connection state, and known addresses.
- `NodeService.WatchPeers`: an initial peer snapshot followed by a new full snapshot only when peer state changes; each update carries a monotonically increasing revision.
- `NodeService.GetBootstrapPeers` / `SetBootstrapPeers`: read or atomically replace the node's saved bootstrap address book. Addresses must include a matching terminal `/p2p/<peer-id>`.

The server binds to `127.0.0.1:50051` by default. `--api-listen` can select another loopback address/port; non-loopback binding is rejected until authentication is implemented. This is a local desktop/daemon API, not a public or browser-ready gRPC-Web endpoint. A hosted Web UI will need an authenticated gateway/relay path; this API alone does not provide remote access.

`GetNodeInfo.country_code` is an optional, approximate country inferred at daemon startup from the node's public egress IP using ipapi.co over HTTPS. Only the two-letter country code is returned to local clients; the daemon does not persist or return the IP. The lookup is best-effort and has a three-second timeout, so GeoIP outages do not prevent node startup. The provider necessarily sees the source IP of the HTTPS request; this prototype integration is for development and must be replaced with a production-appropriate provider or local GeoIP database before broad deployment. VPNs, relays, and some network setups can make the estimate inaccurate.

Bootstrap settings are stored in the node's SQLite database and updates are applied to the running networking loop. Removing a peer from the bootstrap list prevents future bootstrap dials but does not forcibly close an existing connection. Peer status is streamed; chat/job streaming, general node settings, jobs, and P2P application messages are not part of this first contract. Watch updates are snapshots rather than deltas, so a client that reconnects can resume from a fresh initial state. Protobuf fields use stable field numbers; never reuse a field number after removing a field.

The gRPC listener is bound before the daemon reports startup, so address conflicts fail startup with a contextual error. On Ctrl-C it stops accepting requests, signals active peer streams to finish, then waits for graceful server shutdown.

## P2P networking

Daemon-to-daemon connectivity uses Rust `libp2p`. The current scaffold configures TCP and QUIC transports, Noise encryption, Yamux stream multiplexing, Identify and Ping, mDNS local discovery, and Kademlia primitives. The local client API uses Protocol Buffers/gRPC; the separate application job protocol over P2P is not implemented yet.

The scaffold supports encrypted peer connections and basic discovery. Workload transfer, relay support, robust NAT traversal, bootstrap-node operations, and the compute/job protocol are future work. Kademlia currently uses an in-memory address store, so learned DHT state does not survive restarts. The local gRPC client API is separate from this P2P transport; it does not tunnel gRPC between nodes.

## Local state and storage

Each daemon owns a separate local SQLite database. Nodes do not share a database. Storage access is behind the `StateStore` boundary so daemon domains do not depend directly on SQLx or SQLite.

Current implementation:

- SQLx SQLite with embedded, versioned migrations.
- WAL journal mode and a bounded connection pool for this single-node process.
- Persistent libp2p identity stored as a singleton private-key record. Concurrent first startup converges on the same stored identity.
- Database and data directory are restricted on Unix; Windows relies on inherited user ACLs.

SQLite is for local node state, not a distributed database or consensus ledger. Keep large datasets, model artifacts, and job outputs in files/blob storage and persist identifiers, hashes, and metadata in SQLite. If a blockchain is introduced, its consensus-critical state and storage engine should be designed with that protocol rather than treating the local node DB as authoritative network state.

### Data location and identity

By default the daemon stores `node.sqlite3` under the operating system's local application-data directory (`sisyphus/`). `--data-dir` selects a different directory. Reusing the same database preserves the node's `PeerId`; deleting it causes a new identity to be generated.

Bootstrap peers supplied with `--bootstrap` are added to the node's persistent bootstrap list and dialed on subsequent starts. The CLI option is additive. Use `--clear-bootstrap-peers` to remove the saved list; combine it with one or more `--bootstrap` options to replace the list. `--list-bootstrap-peers` prints the persisted address book and exits. While running, the daemon logs peer connection and disconnection events.

The database contains the node's private identity key and is not encrypted by the application. Protect the database and backups as secrets. Do not copy one node database to multiple live nodes: that duplicates identity.

## AI provider boundary

AI provider configuration belongs to the node, not the UI or remote workers. V0 is intended to support bring-your-own-key with initial adapters for Ollama and the OpenAI API. Provider calls and credentials stay on the configured node; the UI configures it and presents responses. Keep provider integrations behind a small replaceable interface that can support streaming, tool calls, and structured plans.

## Cross-platform rules

- Treat cross-platform behavior as a design requirement. Keep domain logic independent of OS paths and APIs.
- Never hardcode home-directory paths, path separators, or assumptions about a writable current directory. Use `Path`/`PathBuf` and shared helpers in `src/platform.rs`.
- Store persistent node data in the OS-appropriate application-data directory. Protect the database and its containing directory with restrictive permissions where supported; rely on the user's inherited ACL on Windows unless a dedicated ACL implementation is introduced.
- Isolate unavoidable OS-specific behavior behind small helpers and `#[cfg(...)]` blocks. Keep a portable fallback where possible.
- Add tests for path handling and platform-specific behavior. Verify supported targets in CI before claiming support; the Nix dev shell currently targets Linux and macOS.

## Development

The repository Nix flake provides the Rust toolchain, formatter, linter, language server, Protobuf compiler, and native build tools.

```sh
nix develop
cargo run -p sisyphusd
```

Run checks from the repository root:

```sh
cargo fmt --check
cargo check -p sisyphusd
cargo test -p sisyphusd
```

## Running and connecting nodes

On first startup, the daemon initializes the SQLite database and node identity. It logs its `PeerId`, listen multiaddresses, bootstrap peers, and live peer connection/disconnection events. To run a second local node and connect it to the first, use the first node's full `/p2p/<peer-id>` address:

```sh
cargo run -p sisyphusd -- \
  --data-dir ./sisyphus-peer-b \
  --listen /ip4/0.0.0.0/tcp/7410 \
  --bootstrap /ip4/127.0.0.1/tcp/7400/p2p/<peer-id>
```

Use `cargo run -p sisyphusd -- --help` to see current command-line options. For example, `cargo run -p sisyphusd -- --list-bootstrap-peers` inspects the saved peer address book.
