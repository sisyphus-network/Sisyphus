# Sisyphus desktop

The Electron + React desktop client is the first visual client for `sisyphusd`. The product/UI is named **Sisyphus**; the headless node process remains **`sisyphusd`**.

## Development

From this directory:

```sh
npm install
npm run dev
```

Start the daemon separately from the repository root:

```sh
cargo run -p sisyphusd
```

The desktop client connects to the local gRPC API at `127.0.0.1:50051`. Override it for development with `SISYPHUS_API_ADDRESS=127.0.0.1:50052 npm run dev`. The daemon currently accepts loopback API addresses only. Without a running daemon, the UI remains useful as a connection-state view and retries with bounded exponential backoff.

The Electron main process is the gRPC client. It reads the shared `proto/sisyphus/node/v1/node.proto`, calls `GetNodeInfo`, and subscribes to `WatchPeers`. A narrow `contextBridge` API forwards snapshots and reconnect requests to the React renderer; the renderer has no direct Node.js or gRPC access.

## Scope

This is a local development client, not a packaged or remotely accessible Web UI. It demonstrates the existing node info and peer-stream APIs. Public-node browsing, remote authentication, daemon lifecycle management, jobs, chat, and production packaging are future work.

Run checks with:

```sh
npm run typecheck
npm run build
```
