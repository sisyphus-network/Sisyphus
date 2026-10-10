# Sisyphus desktop

The Electron + React desktop client is the first visual client for `sisyphusd`. The product/UI is named **Sisyphus**; the headless node process remains **`sisyphusd`**.

## Development

**The quick way to a backend with something in it** is `examples/desktop-backend.sh` from the repository root: three nodes, a worker, a discovered peer and a finished job. [`docs/desktop-backend.md`](../../docs/desktop-backend.md) describes it and every call the app can make.

From this directory:

```sh
npm install
npm run dev
```

Build the Go daemon from the repository root before starting the desktop:

```sh
nix develop
make build
bin/sisyphusd run --api-listen 127.0.0.1:50051
```

Peers are the nodes of its pool and the nodes it has found, and trusting one for compute admits it to the pool as a worker; [its README](../sisyphusd/README.md#the-desktop-app) has the details. It asks for a token before changing anything, which the client reads from `api.token` in the daemon's default data directory (`sisyphusd data-dir` prints it). If the daemon keeps its data elsewhere, name the file with `SISYPHUS_API_TOKEN_FILE`.

`node dev/check-daemon.mjs [address] [token-file]` asks a running daemon the same questions the client does and prints the answers, which is a quick way to check one against the other without opening a window.

The desktop client connects to the local gRPC API at `127.0.0.1:50051`. If that API is unavailable and the port is unused, it starts the repository's `bin/sisyphusd` (or `SISYPHUS_DAEMON_PATH`) and reconnects with bounded exponential backoff. An existing daemon is reused and is never stopped by the desktop. A desktop-owned daemon is stopped when the application quits; closing a window on macOS does not quit the application.

Automatic startup uses the daemon's default data directory and disabled network discovery. Its loopback-only pool listener prefers `127.0.0.1:7700`, so CLI and MCP defaults work. If that port is occupied (or is the API port), it uses an ephemeral port instead without touching the existing service. Read the node overview's listen address and pass its host/port with `--addr` to CLI/MCP commands in that case. The port probe is advisory: if another process wins the subsequent bind race, startup fails safely rather than taking it over. It does not enable containers or remote access. A missing binary or failed startup is reported in the connection diagnostic and is not repeatedly spawned. Run `make build` first. `SISYPHUS_AUTO_START_DAEMON=0` disables startup. Setting a custom `SISYPHUS_API_ADDRESS` or `SISYPHUS_API_TOKEN_FILE` also disables it, since that node belongs to its operator. For example, `SISYPHUS_API_ADDRESS=127.0.0.1:50052 npm run dev` attaches only to that independently started node.

The Electron main process is the gRPC client. It reads the shared `proto/sisyphus/node/v1/node.proto`, calls `GetNodeInfo`, and subscribes to `WatchPeers`. A narrow `contextBridge` API forwards snapshots and reconnect requests to the React renderer; the renderer has no direct Node.js or gRPC access.

## Current capabilities and limits

The desktop client connects to one local daemon. It shows live node and peer status, manages peer compute permissions, and exposes the Go daemon's chat/model, job, file, and pool APIs. Chat model-provider configuration and credentials belong to the local daemon; the desktop UI does not call model providers directly. Files are limited to 256 MiB in this client and are sent to the daemon in bounded gRPC chunks.

This is still a development client, not a packaged product or remotely accessible Web UI. Production packaging must include the matching daemon under `resources/bin/sisyphusd` (`sisyphusd.exe` on Windows); packaging and platform acceptance remain unfinished. Public-node browsing, remote authentication and resource-policy controls remain future work. Shutdown requests SIGTERM and allows up to thirty seconds before forced termination; Windows process termination is immediate, not graceful POSIX signal handling. The desktop currently starts no Kubo sidecar; a future sidecar mode also needs an explicit child-process shutdown policy, not just this longer grace period.

Run checks with:

```sh
npm run typecheck
npm run build
npm test
# After make build: starts a separate temporary node and never uses your data.
npm run test:daemon
```
