# Sisyphus desktop

The Electron + React desktop client is the first visual client for `sisyphusd`. The product/UI is named **Sisyphus**; the headless node process remains **`sisyphusd`**.

## Development

**The quick way to a backend with something in it** is `examples/desktop-backend.sh` from the repository root: three nodes, a worker, a discovered peer and a finished job. [`docs/desktop-backend.md`](../../docs/desktop-backend.md) describes it and every call the app can make.

From this directory:

```sh
npm install
npm run dev
```

The Model provider page in the development build includes six sample profiles for previewing card layouts. They are gated to development and are not persisted or included in the production app; the end-to-end suite checks their absence from the built app.

Build the Go daemon from the repository root before starting the desktop:

```sh
nix develop
make build
bin/sisyphusd run --api-listen 127.0.0.1:50051
```

Peers are the nodes of its pool and the nodes it has found, and trusting one for compute admits it to the pool as a worker; [its README](../sisyphusd/README.md#the-desktop-app) has the details. It asks for a token before changing anything, which the client reads from `api.token` in the daemon's default data directory (`sisyphusd data-dir` prints it). If the daemon keeps its data elsewhere, name the file with `SISYPHUS_API_TOKEN_FILE`.

`node dev/check-daemon.mjs [address] [token-file]` asks a running daemon the same questions the client does and prints the answers, which is a quick way to check one against the other without opening a window.

### Browser preview during development

The Vite renderer can be opened from another device for development. Build it once, keep the daemon web listener loopback-only, then expose only the Vite development server on your trusted network:

```sh
npm run build
bin/sisyphusd run --api-listen 127.0.0.1:50051 --web-listen 127.0.0.1:50052 --web-ui-dir apps/sisyphus/out/renderer
SISYPHUS_DEV_HOST=0.0.0.0 npm run dev
```

The dev server proxies browser API and pairing requests to the local daemon. Authenticate with the node API token once, then use **Settings → Devices** to create a short-lived code or QR link for another browser. The development server uses plain HTTP; only use it on a trusted network. This is not a production deployment.

The desktop client connects to the local gRPC API at `127.0.0.1:50051`. If that API is unavailable and the port is unused, it starts the repository's `bin/sisyphusd` (or `SISYPHUS_DAEMON_PATH`) and reconnects with bounded exponential backoff. An existing daemon is reused and is never stopped by the desktop. A desktop-owned daemon is stopped when the application quits; closing a window on macOS does not quit the application.

Automatic startup uses the daemon's default data directory and disabled network discovery. Its loopback-only pool listener prefers `127.0.0.1:7700`, so CLI and MCP defaults work. If that port is occupied (or is the API port), it uses an ephemeral port instead without touching the existing service. Read the node overview's listen address and pass its host/port with `--addr` to CLI/MCP commands in that case. The port probe is advisory: if another process wins the subsequent bind race, startup fails safely rather than taking it over. It does not enable containers or remote access. A missing binary or failed startup is reported in the connection diagnostic and is not repeatedly spawned. Run `make build` first. `SISYPHUS_AUTO_START_DAEMON=0` disables startup. Setting a custom `SISYPHUS_API_ADDRESS` or `SISYPHUS_API_TOKEN_FILE` also disables it, since that node belongs to its operator. For example, `SISYPHUS_API_ADDRESS=127.0.0.1:50052 npm run dev` attaches only to that independently started node.

**Settings → This node → Location discovery** can opt into read-only NAT-PMP gateway lookup, a named STUN server, and the `ipapi.co` last-resort lookup. All are off by default. NAT-PMP lookup is supported on Linux, macOS, BSD and Windows and never requests a port mapping. A STUN server is contacted only if earlier address sources have not placed the node; that server sees the public IP address that reaches it, but no port is opened. Enabling the `ipapi.co` fallback discloses the public IP address to that service only when earlier address sources—including a configured STUN server—have not placed the node, and the country it then reports is the one shown. Preferences are stored in the daemon's database and apply after that daemon restarts. An Electron client can configure a separately managed daemon when connected with its endpoint and API token; a paired browser can configure it through its same-origin session without receiving the token. A desktop-managed daemon applies saved changes when the app next starts it.

**Settings → This node → Pool limits** sets what each other member of the node's pool may use while the node coordinates it: unfinished jobs and tasks per member, the longest one attempt at a member's task may run (entered in seconds, minutes or hours), storage per member, and upload per task attempt (in decimal MB, GB or TB: 1 GB is 1,000,000,000 bytes). An empty field means no limit, and the node's own work is never limited. Byte and second values are handled as exact 64-bit integers, and the daemon's own reason is shown if it refuses a value. The limits are stored only in the daemon's database (the desktop keeps no copy and passes no flags for them) and apply after that daemon restarts; a limit given explicitly on the daemon's command line overrides the saved one. As with location discovery, an Electron client can manage a separately managed daemon when connected with its endpoint and API token, and a paired browser through its same-origin session. A node without the coordinator role can still save limits, but they have no effect until it runs that role.

The Electron main process is the gRPC client. It reads the shared `proto/sisyphus/node/v1/node.proto`, takes an initial snapshot, then subscribes to `WatchNodeState`, a single versioned stream of node details and peers. The daemon wakes that stream when asynchronous location discovery completes and checks peer changes at the local API interval, so the overview globe updates without a restart. A narrow `contextBridge` API forwards snapshots and reconnect requests to the React renderer; the renderer has no direct Node.js or gRPC access. Browser clients use the same stream.

## Current capabilities and limits

The desktop client connects to one local daemon. It shows live node and peer status, manages peer compute permissions, and exposes the Go daemon's chat/model, job, file, and pool APIs. Chat model-provider configuration and credentials belong to the local daemon; the desktop UI does not call model providers directly. Files are limited to 256 MiB in this client and are sent to the daemon in bounded gRPC chunks.

The node can serve the same React client to a browser from its own web listener; browser sessions are paired and revocable per node. Public-node browsing and resource-policy controls remain future work. Shutdown requests SIGTERM and allows up to thirty seconds before forced termination; Windows process termination is immediate, not graceful POSIX signal handling. The desktop currently starts no Kubo sidecar; a future sidecar mode also needs an explicit child-process shutdown policy, not just this longer grace period.

## Native packaging

Build on the target operating system and CPU architecture with Go and Node installed. The packaging hook builds the matching daemon and includes it outside ASAR under `resources/bin/sisyphusd` (`sisyphusd.exe` on Windows). It refuses cross-platform/architecture builds rather than silently shipping a mismatched binary. The protocol is already embedded in the main-process bundle, so no external `.proto` is required at runtime.

```sh
npm ci
npm run package:dir   # runnable native app directory under dist/
npm run test:package  # inspect archive and test its actual embedded daemon
npm run package:dist  # Linux tar.gz, Windows zip, macOS zip
```

These are unsigned development artifacts, not signed/notarized releases or installers. `--publish never` prevents release publication. The `Desktop packaging` workflow builds on Linux, Windows and macOS, verifies archive contents and exercises the embedded daemon with temporary data, then uploads seven-day CI artifacts. Green packaging/smoke tests do not prove interactive window, graphics, keyboard or operating-system security-prompt acceptance. Signing/notarization and full platform/window acceptance remain required before a production release.

Run checks with:

```sh
npm run typecheck
npm run build
npm test
# After make build: starts a separate temporary node and never uses your data.
npm run test:daemon
# After make build and npm run build: drives the real app against a temporary node.
npm run test:e2e
```

## End-to-end tests

`npm run test:e2e` starts the built app with [Playwright](https://playwright.dev) and drives it as a person would, against a node of its own: the repository's `bin/sisyphusd` on ports nothing else has, with a temporary data directory, and a temporary profile for the app. Your node, your data and your app are not touched. On Linux with no display it runs under `xvfb-run`, which is how CI runs it.

What they check today (`e2e/app.e2e.mjs`):

- the window shows the node it is connected to, its ID and its worker;
- a window with no WebGL is offered no topology (no pane, no handle, no dock entry when narrow), its chat fills the room and answers; a window with WebGL has the topology wide and narrow;
- a job submitted in Operations runs on the pool, and its result and events are shown;
- a file stored as private is listed, and an invitation is issued;
- the language picker opens from the keyboard and keeps the focus among its choices, Hebrew turns the window right-to-left without a sideways scroll, and the choice is still there at the next start;
- a node that stops is said to be gone with the sidebar left in place, and the window finds it again when it returns;
- a question with a file attached is planned by a stand-in model, computed on the pool and answered; the model is told of the file as private and is never sent its content; a second turn carries on, and the conversation is in the history.

`e2e/harness.mjs` has what a new test needs: `testNode()`, `testProfile()`, `launch()`, `open()` for the sidebar's pages, and `standInModel()` for a planner that says what the test tells it to. Elements are found by role and name, as a screen reader finds them, so a test that cannot find a control is often a control that has no name.
