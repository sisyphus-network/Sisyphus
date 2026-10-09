# Sisyphus desktop

The Electron + React desktop client is the first visual client for `sisyphusd`. The product/UI is named **Sisyphus**; the headless node process remains **`sisyphusd`**.

## Development

**The quick way to a backend with something in it** is `examples/desktop-backend.sh` from the repository root: three nodes, a worker, a discovered peer and a finished job. [`docs/desktop-backend.md`](../../docs/desktop-backend.md) describes it and every call the app can make.

From this directory:

```sh
npm install
npm run dev
```

Start the Go daemon separately from the repository root:

```sh
nix develop
make build
bin/sisyphusd run --api-listen 127.0.0.1:50051
```

Peers are the nodes of its pool and the nodes it has found, and trusting one for compute admits it to the pool as a worker; [its README](../sisyphusd/README.md#the-desktop-app) has the details. It asks for a token before changing anything, which the client reads from `api.token` in the daemon's default data directory (`sisyphusd data-dir` prints it). If the daemon keeps its data elsewhere, name the file with `SISYPHUS_API_TOKEN_FILE`.

`node dev/check-daemon.mjs [address] [token-file]` asks a running daemon the same questions the client does and prints the answers, which is a quick way to check one against the other without opening a window.

The desktop client connects to the local gRPC API at `127.0.0.1:50051`. Override it for development with `SISYPHUS_API_ADDRESS=127.0.0.1:50052 npm run dev`. The daemon currently accepts loopback API addresses only. Without a running daemon, the UI remains useful as a connection-state view and retries with bounded exponential backoff.

The Electron main process is the gRPC client. It reads the shared `proto/sisyphus/node/v1/node.proto`, calls `GetNodeInfo`, and subscribes to `WatchPeers`. A narrow `contextBridge` API forwards snapshots and reconnect requests to the React renderer; the renderer has no direct Node.js or gRPC access.

## Current capabilities and limits

The desktop client connects to one local daemon. It shows live node and peer status, manages peer compute permissions, and exposes the Go daemon's chat/model, job, file, and pool APIs. Chat model-provider configuration and credentials belong to the local daemon; the desktop UI does not call model providers directly. Files are limited to 256 MiB in this client and are sent to the daemon in bounded gRPC chunks.

This is still a development client, not a packaged product or remotely accessible Web UI. It does not start or manage the daemon process: start `sisyphusd` separately. Public-node browsing, remote authentication, daemon lifecycle management, and production packaging remain future work.

Run checks with:

```sh
npm run typecheck
npm run build
```
