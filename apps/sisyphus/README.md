# Sisyphus desktop

The Electron + React desktop client is the first visual client for `sisyphusd`. The product/UI is named **Sisyphus**; the headless node process remains **`sisyphusd`**.

## Development

From this directory:

```sh
npm install
npm run dev
```

Start a daemon separately from the repository root. Either of the two in this repository serves the API the client uses:

```sh
# The Go daemon, which runs pools and jobs. Its local API is off unless asked for.
go build ./apps/sisyphusd && ./sisyphusd run --api-listen 127.0.0.1:50051

# The Rust daemon, which discovers peers over libp2p.
cargo run -p sisyphusd
```

Against the Go daemon, peers are the nodes of its pool and the nodes it has found, and trusting one for compute admits it to the pool as a worker; [its README](../sisyphusd/README.md#the-desktop-app) has the details. It asks for a token before changing anything, which the client reads from `api.token` in the daemon's default data directory (`sisyphusd data-dir` prints it). If the daemon keeps its data elsewhere, name the file with `SISYPHUS_API_TOKEN_FILE`. The Rust daemon ignores the token.

`node dev/check-daemon.mjs [address] [token-file]` asks a running daemon the same questions the client does and prints the answers, which is a quick way to check one against the other without opening a window.

The desktop client connects to the local gRPC API at `127.0.0.1:50051`. Override it for development with `SISYPHUS_API_ADDRESS=127.0.0.1:50052 npm run dev`. The daemon currently accepts loopback API addresses only. Without a running daemon, the UI remains useful as a connection-state view and retries with bounded exponential backoff.

The Electron main process is the gRPC client. It reads the shared `proto/sisyphus/node/v1/node.proto`, calls `GetNodeInfo`, and subscribes to `WatchPeers`. A narrow `contextBridge` API forwards snapshots and reconnect requests to the React renderer; the renderer has no direct Node.js or gRPC access.

## Scope

This is a local development client, not a packaged or remotely accessible Web UI. It demonstrates the existing node info and peer-stream APIs. Public-node browsing, remote authentication, daemon lifecycle management, jobs, chat, and production packaging are future work.

Run checks with:

```sh
npm run typecheck
npm run build
```
