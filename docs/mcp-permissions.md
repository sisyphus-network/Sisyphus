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

The confirmation uses MCP's multi-round-trip elicitation. The node action runs only after the client returns an explicit accepted response with `confirm: true`. The server binds a random, one-time request state to the exact action and target; it expires after five minutes and cannot be replayed. A decline, dismissal, malformed response, changed request, expired confirmation, or client without elicitation support leaves the action unapplied. A client that cannot ask the person using it cannot perform these operations.

This confirmation is in the MCP client. The separate app-side pending-action prompt discussed in #649 and tracked in #650 is not implemented by this change.
