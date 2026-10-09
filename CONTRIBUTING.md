# Contributing to Sisyphus

Sisyphus is an early prototype built in the open. Issues, questions and pull requests are welcome.

## Before you start

- The plan, in order, is in the [roadmap issue](https://github.com/sisyphus-network/Sisyphus/issues/23). For anything larger than a fix, open an issue or comment on an existing one first, so that the work is not done twice or in a direction that will not merge.
- [`docs/development.md`](docs/development.md) covers setting up, where things are, running the tests and how the protocol is generated. [`apps/sisyphusd/README.md`](apps/sisyphusd/README.md) is the guide to running nodes.

## Pull requests

- Branch from `dev` and open the pull request against `dev`. Nothing is pushed to `dev` directly.
- A pull request merges by squash once its checks are green, so each commit on `dev` has passed.
- Commit and pull request titles use a conventional prefix: `feat:`, `fix:`, `docs:`, `test:`, `chore:`.
- Keep documentation current in the same pull request: the root README, `apps/sisyphusd/README.md` and the files under `docs/`.

## What the checks expect

For the daemon (Go):

```sh
gofmt -l apps packages skills   # lists nothing
go vet ./...
make cover                      # every statement of hand-written code is executed by a test
go test -race ./...
```

New code needs tests for every statement, failure paths included. Tests that run real programs (Kubo, IPFS Cluster, Docker) are skipped where the program is missing and required in CI.

For the desktop app (`apps/sisyphus`): `npm ci`, then `npm run typecheck`, `npm test` and `npm run build`.

## License

Sisyphus is licensed under the [Apache License, Version 2.0](LICENSE). By contributing you agree that your contribution is licensed under the same terms.

## Security

Do not open a public issue for a vulnerability. See [SECURITY.md](SECURITY.md).
