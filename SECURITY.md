# Security policy

## Reporting a vulnerability

Please do not open a public issue, discussion or pull request for a vulnerability.

Report it privately through GitHub instead: on this repository's **Security** tab, choose **Report a vulnerability**. That opens a private advisory only the maintainers can see, where we can talk it through, agree a fix and credit you when it is published.

Say what you found, how to reproduce it, and what an attacker could do with it. A failing test or a short script is the most useful thing you can send.

We aim to acknowledge a report within three days, and to say within two weeks what we will do about it.

## What is in scope

Sisyphus is pre-release. Only the `dev` branch is supported; there are no released versions to backport fixes to.

Read [`docs/security-model.md`](docs/security-model.md) first: it sets out what a pool protects today, what it does not, and whom each party has to trust. A way around any protection it describes is in scope. This includes:

- a node joining a pool, or calling something, without the invitation or role that should be needed;
- reading a private job's stored data, or a private file, from a node that should not be able to;
- one client reading or changing another client's jobs or pins;
- a worker breaking out of the confinement described for container jobs;
- the local API acting for a caller without the node's token.

Something the security model already lists under **Not protected against today** is a known limitation, not a vulnerability. If you think one of those matters more than the document says, an issue or a discussion is the right place.

## Independent review

Nothing here has had an independent security review yet. The sealing of private data is waiting for one ([#91](https://github.com/sisyphus-network/Sisyphus/issues/91)).
