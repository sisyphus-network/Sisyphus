# Security

Sisyphus is an early prototype. It has not had an independent security review, and the way private jobs are sealed in particular is waiting for one ([#91](https://github.com/sisyphus-network/Sisyphus/issues/91)). Do not rely on it yet to protect data that matters.

## Reporting a vulnerability

Report it privately through GitHub: on the repository's **Security** tab, choose **Report a vulnerability**. Please do not open a public issue or pull request for it.

Say what you found, how to reproduce it, and which commit or release you saw it in. We will acknowledge the report, work out a fix with you, and credit you when it is published unless you would rather not be named.

## What is in scope

- The daemon, `sisyphusd`: its protocol between nodes, the local API and its token, pool membership and invitations, storage, sealing of private jobs, the MCP server.
- The desktop app in `apps/sisyphus`: the bridge between its window and the node.

## What to know before reporting

- A coordinator accepts whatever result a worker returns. A pool is only as trustworthy as the workers admitted to it; this is stated in the README and is not a vulnerability by itself.
- A worker that runs container jobs runs images chosen by the pool's clients. It is opt-in for that reason.
- There is no released version yet. Fixes land on `dev`.
