# Sisyphus desktop

The Electron + React desktop client is the first visual client for `sisyphusd`. The product/UI is named **Sisyphus**; the headless node process remains **`sisyphusd`**.

## Development

**The quick way to a backend with something in it** is `examples/desktop-backend.sh` from the repository root: three nodes, a worker, a discovered peer and a finished job. [`docs/desktop-backend.md`](../../docs/desktop-backend.md) describes it and every call the app can make.

From this directory:

```sh
npm install
npm run dev
```

`package.json` temporarily overrides KaTeX to `^0.18.2`: the math and Mermaid rendering plugins currently constrain older KaTeX versions affected by [GHSA-238p-pmpm-9mq7](https://github.com/advisories/GHSA-238p-pmpm-9mq7). The lockfile resolves the override to a patched 0.18.x release. Keep the override until both upstream dependency ranges accept a patched release; the Electron regression test exercises math and Mermaid rendering against it.

It also overrides `global-agent` to `^4.1.3`. Electron Builder's copy of `@electron/get` 3.x asks for `global-agent` 3, which reaches `sprintf-js` 1.1.3 through `roarr`. That `sprintf-js` version is affected by [GHSA-hp3w-g68c-fv3c](https://github.com/advisories/GHSA-hp3w-g68c-fv3c), and no patched release exists (#507). `global-agent` 4 depends on neither, and it keeps the CommonJS `bootstrap()` that `@electron/get` calls when `ELECTRON_GET_USE_PROXY` is set. It is only a build tool and never ships in the app. Drop the override once Electron Builder moves to an `@electron/get` that no longer uses `global-agent`.

UI components come from shadcn, but the `shadcn` package isn't installed: its CLI pulled in `braces` ≤3.0.3 ([GHSA-vfj7-8cjw-p6xm](https://github.com/advisories/GHSA-vfj7-8cjw-p6xm), no patched release yet, #556), and the app only needs its stylesheet. That stylesheet is vendored, with its MIT notice, as `src/renderer/src/design-system/shadcn-tailwind.css`. To add a component, run the CLI at a pinned version, for example `npx shadcn@4.21.4 add dialog`. If a newer CLI changes `dist/tailwind.css`, copy the new file over the vendored one.

The Model provider page in the development build includes six sample profiles for previewing card layouts. They are gated to development and are not persisted or included in the production app; the end-to-end suite checks their absence from the built app.

Build the Go daemon from the repository root before starting the desktop:

```sh
nix develop
make build
bin/sisyphusd run --api-listen 127.0.0.1:50051
```

Peers are the nodes of its pool and the nodes it has found, and trusting one for compute admits it to the pool as a worker; [its README](../sisyphusd/README.md#the-desktop-app) has the details. It asks for a token before changing anything, which the client reads from `api.token` in the daemon's default data directory (`sisyphusd data-dir` prints it). If the daemon keeps its data elsewhere, name the file with `SISYPHUS_API_TOKEN_FILE`.

`node dev/check-daemon.ts [address] [token-file]` asks a running daemon the same questions the client does and prints the answers, which is a quick way to check one against the other without opening a window.

### Browser preview during development

The Vite renderer can be opened from another device for development. Build it once, keep the daemon web listener loopback-only, then expose only the Vite development server on your trusted network:

```sh
npm run build
bin/sisyphusd run --api-listen 127.0.0.1:50051 --web-listen 127.0.0.1:50052 --web-ui-dir apps/sisyphus/out/renderer
SISYPHUS_DEV_HOST=0.0.0.0 npm run dev
```

The dev server proxies browser API and pairing requests to the local daemon. Authenticate with the node API token once, then use **Settings → Devices** to create a short-lived code or QR link for another browser. The development server uses plain HTTP; only use it on a trusted network. This is not a production deployment.

The desktop client connects to the local gRPC API at `127.0.0.1:50051`. If that API is unavailable and the port is unused, it starts the repository's `bin/sisyphusd` (or `SISYPHUS_DAEMON_PATH`) and reconnects with bounded exponential backoff. An existing daemon is reused and is never stopped by the desktop. A desktop-owned daemon is stopped when the application quits. While the app owns its daemon it shows a tray icon (`src/main/app-tray.ts`) whose picture and menu follow the same snapshot the window does, with no polling of its own (the pictures are in `build/tray/`: a hollow-ring badge for connecting, a slash for offline, and on macOS black template images the menu bar tints), and closing the last window leaves the app and daemon running; the first such close shows a one-off notification, and `tray.json` in the user data directory records that and the tray's language. With a daemon it did not start, or on a Linux desktop with no StatusNotifier host (checked over D-Bus for `org.kde.StatusNotifierWatcher`), closing the last window quits as before, except on macOS, which keeps the app open as usual. The decisions are in `src/main/tray-policy.ts`.

Automatic startup uses the daemon's default data directory and disabled network discovery. It also starts the authenticated local web API on `127.0.0.1:50052` for managing paired browsers; this listener stays on loopback and does not enable remote browser access or serve the web UI. Its loopback-only pool listener prefers `127.0.0.1:7700`, so CLI and MCP defaults work. If that port is occupied (or is the API port), it uses an ephemeral port instead without touching the existing service. **Settings → This node → Let other machines join this pool** changes that, from the next start (**Restart node** beside **Save** restarts the app's node now): the pool listener moves to the chosen address and port (`--listen`, one of the machine's own IPv4 addresses, or `0.0.0.0` only when picked on purpose) and discovery is left on. The choice is kept in the app's `pool-hosting.json`. If the saved address is not the machine's when the app starts, the node stays on loopback and Settings says why. Read the node overview's listen address and pass its host/port with `--addr` to CLI/MCP commands in that case. The port probe is advisory: if another process wins the subsequent bind race, startup fails safely rather than taking it over. It does not enable remote access, nor containers unless the owner turns them on (below). A missing binary or failed startup is reported in the connection diagnostic and is not repeatedly spawned. Run `make build` first. `SISYPHUS_AUTO_START_DAEMON=0` disables startup. Setting a custom `SISYPHUS_API_ADDRESS` or `SISYPHUS_API_TOKEN_FILE` also disables it, since that node belongs to its operator, unless `SISYPHUS_AUTO_START_DAEMON=1` asks for the app's own node at that `SISYPHUS_API_ADDRESS` (with `SISYPHUS_WEB_API_ADDRESS` for its web API), as the end-to-end tests do. For example, `SISYPHUS_API_ADDRESS=127.0.0.1:50052 npm run dev` attaches only to that independently started node.

**Settings → This node → Where this node is** sets the owner's own pin, the most precise of the node's location sources: coordinates pasted as a map copies them (decimal, or with N/S/E/W), and how much of the place other members of a pool may see, from the exact place to nothing, with the country as the default. It is kept in the daemon's database through `SetNodeLocation` and applies at once, with no restart; the workspace globe puts this node at the pin. The node's workers pass the pin on to its pool rounded as chosen, and never more precisely. The workspace globe of a coordinator draws each member at the place it shares, from `ListWorkers`, and the others at their country's centre. Hovering a point shows a card with everything known about it, merged in `src/renderer/src/lib/globe-details.ts` from the snapshot (peer ID, connection, addresses and their IPs, country, trust and work flags), `ListWorkers` (name, hostname, system, processor, memory, graphics cards, models, task slots, shared place and its apparent rounding) and, for this node, `GetNodeInfo` (version, listen addresses, country source, `city_estimate`) and `GetNodeLocation` (pin and share). The card is HTML for react-globe.gl's `pointLabel`, so every value is escaped there, and peer IDs, addresses and coordinates are isolated left to right for right-to-left languages. Clicking a point keeps its card open over the pane, with **Copy peer ID**, a close button and Escape. A WebGL point cannot take focus, so each is also a visually hidden button, revealed on focus, that opens the same card; the end-to-end test uses it. The pin, workers and city estimate are read again whenever the workspace is shown, as well as when the node's state changes. The globe zooms by wheel, pinch, or its **+**/**−** buttons, between limits in `src/renderer/src/lib/globe-view.ts`. Past its fitted size, it lazily loads `src/renderer/src/data/admin1-lines.json`, Natural Earth's public-domain admin-1 lines simplified by `dev/build-region-lines.ts`, and draws them as one `LineSegments` in a react-globe.gl custom layer. A shader fades line segments on the globe's far side.

**Settings → This node → Run containers for the pool** is off by default. Turned on, the app's node starts with `--containers`, so it takes `transcode`, `md` and `container` work, confined by the options beside it: `--containers-read-only` with `--containers-tmp-mb` (on, 1024 MB, by default) and `--containers-network` (off by default). The sandboxing `--container-runtime`, and the memory, processor and time caps, are left to nodes run by hand. The choice is kept in the app's `containers.json` (`src/main/container-settings.ts`) and read each time the app starts its node. Before the switch can be turned on, and again as the node starts, the main process asks `docker version --format '{{.Server.Version}}'` with a 5-second timeout. Docker that is not installed and Docker that does not answer are told apart, and the page offers Docker's install page. Saved containers that Docker cannot run when the node starts are left off, so the node still starts, and the page says why. `SISYPHUS_DOCKER_CHECK` (`ready:<version>`, `missing` or `stopped`) stands in for that check in the end-to-end tests. As with hosting, a node the app did not start runs containers only if its operator gave it `--containers`.

**Settings → This node → City table** downloads, updates or removes DB-IP's free "IP to City Lite" table, some 90 MB, through the daemon's `GetCityTable`, `DownloadCityTable` and `RemoveCityTable`. Nothing is downloaded unless the owner presses **Download** or **Update**, and the card says first that downloading shows db-ip.com the machine's address and that lookups afterwards are local. It shows the kept edition, its size and the day it was downloaded, and the city estimate from `GetNodeInfo`'s `city_estimate`, if any, with the warning that it is often tens of kilometres off. The credit the CC BY 4.0 licence asks for, "IP Geolocation by DB-IP", links to https://db-ip.com; in the desktop it is opened by the fixed-address `app:open-db-ip` handler, since the window opens no links itself. The pure parts, reading either client's replies and formatting the status, are in `src/renderer/src/lib/city-table.ts`.

**Settings → This node → Location discovery** can opt into read-only NAT-PMP gateway lookup, a named STUN server, and the `ipapi.co` last-resort lookup. All are off by default. NAT-PMP lookup is supported on Linux, macOS, BSD and Windows and never requests a port mapping. A STUN server is contacted only if earlier address sources have not placed the node; that server sees the public IP address that reaches it, but no port is opened. Enabling the `ipapi.co` fallback discloses the public IP address, and that the node runs Sisyphus and which version, to that service only when earlier address sources—including a configured STUN server—have not placed the node, and the country it then reports is the one shown. Preferences are stored in the daemon's database and apply after that daemon restarts. An Electron client can configure a separately managed daemon when connected with its endpoint and API token; a paired browser can configure it through its same-origin session without receiving the token. A desktop-managed daemon applies saved changes when the app next starts it; **Restart node** does that now. It is shown only for the node the app started, stops that process and starts it again with the saved hosting and location settings, and asks first, with the counts, when the node has unfinished jobs or other workers connected.

**Settings → This node → Pool limits** sets what each other member of the node's pool may use while the node coordinates it: unfinished jobs and tasks per member, the longest one attempt at a member's task may run (entered in seconds, minutes or hours), storage per member, and upload per task attempt (in decimal MB, GB or TB: 1 GB is 1,000,000,000 bytes). An empty field means no limit, and the node's own work is never limited. Byte and second values are handled as exact 64-bit integers, and the daemon's own reason is shown if it refuses a value. The limits are stored only in the daemon's database (the desktop keeps no copy and passes no flags for them) and apply after that daemon restarts; a limit given explicitly on the daemon's command line overrides the saved one. As with location discovery, an Electron client can manage a separately managed daemon when connected with its endpoint and API token, and a paired browser through its same-origin session. A node without the coordinator role can still save limits, but they have no effect until it runs that role.

**Run** is a gallery of the workloads the pool can run, built from the daemon's `ListWorkloads`: a card for each workload that gives a form, with how many workers run it, and a list of the rest. A card opens its form, made from the fields the daemon describes (a drop zone or stored file for a file, number inputs checked against their bounds and step, a list for a choice, a switch for a toggle); **Run** submits the job and shows the chat's job tracker, then the result and its output files. The jobs started there are listed until the app closes, each able to run again with its values. The pure form logic is `src/renderer/src/lib/workload-form.ts`.

The Electron main process is the gRPC client. It reads the shared `proto/sisyphus/node/v1/node.proto`, takes an initial snapshot, then subscribes to `WatchNodeState`, a single versioned stream of node details and peers. The daemon publishes updates when discovery, peer, worker, or job state changes; paired-browser and worker lists have their own versioned streams. A narrow `contextBridge` API forwards snapshots and stream events to the React renderer; the renderer has no direct Node.js or gRPC access. Browser clients use the same streams.

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

What they check today (`e2e/app.e2e.ts`):

- the window shows the node it is connected to, its ID and its worker;
- a window with no WebGL is offered no topology (no pane, no handle, no dock entry when narrow), its chat fills the room and answers; a window with WebGL has the topology wide and narrow;
- a job submitted in Operations runs on the pool, and its result and events are shown;
- the Run page shows primes with its worker count, refuses a value out of bounds inline, runs it and tracks it to its result, and keeps its values to run again; the transcode form gives its file field a drop zone, and its native choice control remains in-bounds at phone widths in LTR and RTL;
- a job the planner runs is tracked in the chat: its tasks done, and each worker's share;
- a file stored as private is listed, and an invitation is issued;
- the language picker opens from the keyboard and keeps the focus among its choices, Hebrew turns the window right-to-left without a sideways scroll, and the choice is still there at the next start;
- the pool-hosting interface picker stays in a short desktop viewport in English/LTR and Hebrew/RTL, keeps the interface/IP list LTR, filters unmatched interfaces, and dismisses with Escape;
- representative surfaces on Node overview, Operations, General, Access, and Model provider settings change between light and dark themes; each page renders the selected accent, which remains selected after navigation;
- a node that stops is said to be gone with the sidebar left in place, and the window finds it again when it returns;
- containers for the app's own node: turned on only once (stand-in) Docker answers, saved, read back and shown as waiting for a restart; with Docker missing the switch stays off and the page says where to get it; a node run by hand is left to its own `--containers`;
- a question with a file attached is planned by a stand-in model, computed on the pool and answered; the model is told of the file as private and is never sent its content; a second turn carries on, and the conversation is in the history.

`e2e/harness.ts` has what a new test needs: `testNode()`, `testProfile()`, `launch()`, `launchOwnNode()` for the app starting a node of its own, `open()` for the sidebar's pages, and `standInModel()` for a planner that says what the test tells it to. Elements are found by role and name, as a screen reader finds them, so a test that cannot find a control is often a control that has no name. When a test fails, what the shared node logged while it ran is printed after it, and a node that never comes up says how it ended and what it said last.

The tests are TypeScript that Node runs with its types stripped, so `npm run typecheck` checks them too, through `tsconfig.test.json`. It allows only syntax Node can strip (`erasableSyntaxOnly`), and type-only imports must say so (`import type`), since Node removes only those.
