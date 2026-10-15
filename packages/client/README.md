# `@sisyphus/client`

The shared TypeScript client package is built from `proto/sisyphus/node/v1/node.proto`.
Generated message schemas and the service descriptor, both in `node_pb.ts` (Protobuf-ES v2),
are checked in under `src/gen`; regenerate them from the repository root with:

```sh
nix develop --command npm --prefix apps/sisyphus run proto:generate
```

`createNodeServiceClient()` creates a typed Connect client for a node API. In a
browser it defaults to the current origin, so a node can eventually serve the UI
and API together. Supply `getAuthorization` to provide a short-lived credential
when the node's web API requires one. The SDK does not read, persist, or embed
node tokens.

`createSisyphusClient()` detects the Electron preload bridge or creates a
protobuf-derived Connect client for the current browser origin. The renderer
uses this factory to select its runtime once, rather than checking Electron
globals throughout the UI. It does not discover remote nodes or infer network
exposure. Electron continues to use its narrow preload IPC bridge; browser
access is subject to the daemon's web API, authentication, origin, and listener
policies.

The Electron IPC method types are derived from the generated service contract,
then narrowed to the methods intentionally allowlisted by the main process.
The node-served browser client uses the same protobuf methods for chat, jobs,
files, and pool operations after the user connects with the local node token or
pairs the browser with a one-time code. Paired sessions are HttpOnly and
revocable; their random credentials remain inside cookies and are never
exposed to this package. The SDK does not embed or persist the node's
`api.token`.
