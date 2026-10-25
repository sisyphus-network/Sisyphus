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

When the client cannot elicit confirmation, the MCP server asks the node to hold the action as a pending request. The desktop app streams pending requests to the node owner, who can approve or deny them there. This queue is in-memory, bounded, expires after five minutes, and is cleared when the daemon restarts. Only an authenticated local app can request or resolve these actions; remote browser clients cannot approve them. If the node API or app-side confirmation path is unavailable, the MCP operation fails closed and makes no change.
