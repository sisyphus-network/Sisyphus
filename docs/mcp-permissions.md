# MCP permissions and destructive actions

The MCP server is a client of the same authenticated `NodeService` used by the app. It does not have a separate path into the daemon, and the node's API remains responsible for keeping secrets out of read responses.

The server classifies every tool in one of four tiers:

| Tier | Availability | Examples |
|---|---|---|
| Read | Always | Inspect the pool, node settings, files and conversations. |
| Use | Unless `--read-only` | Run jobs, ask models, and store or fetch files. |
| Admin | Only with `--admin`, and never with `--read-only` | Change settings, manage pool membership, and manage local models. |
| Destructive | Only with `--admin`, and never with `--read-only`; asks for confirmation on every call | Remove data or do something the tools cannot undo. |

An operation that can remove data or cannot be undone through the tools is destructive. Each call asks the MCP client to elicit confirmation for the specific target or effect. This includes removing a file or member, deleting a conversation, model or profile, removing the local city table, leaving a pool, releasing a file pin, and collecting garbage.

When the MCP client supports multi-round-trip elicitation, the server asks it to confirm the exact action and target. The action runs only after an explicit accepted response with `confirm: true`; the server binds a random, one-time request state to the action, expires it after five minutes, and prevents replay. A decline, dismissal, malformed response, changed request, or expired confirmation leaves the action unapplied.

When the client cannot elicit confirmation, the MCP server asks the node to hold the action as a pending request. The node refuses immediately if no desktop approval stream is connected, so the agent can ask the owner to open the app and retry. Otherwise, the desktop app streams pending requests to the node owner, who can approve or deny them there. If the app disconnects after a request is queued, it remains pending until the owner resolves it, the caller cancels, or it expires after five minutes. The queue is bounded, in-memory, and cleared when the daemon restarts. Requesting or resolving an action requires a token-authenticated loopback caller; the remote browser API cannot resolve requests. The approval prompt is shown only in the desktop app. If the node API or app-side confirmation path is unavailable, the MCP operation fails closed and makes no change.
