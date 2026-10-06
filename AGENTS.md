# Repository implementation rules

## Cross-platform support

- Treat cross-platform behavior as a design requirement for daemon code. Keep domain logic independent of operating-system-specific paths and APIs.
- Never hardcode home-directory paths, path separators, or assumptions about a writable current directory. Build paths with `path/filepath`, and take the default data directory from `defaultDataDir` in `apps/sisyphusd/daemon.go`.
- Put persistent node data in the OS-appropriate application data directory. The node's key (`node.key`) and its SQLite database (`node.db`) are private, so create them readable by their owner only where the platform supports that; rely on the user's inherited ACL on Windows unless a dedicated ACL implementation is introduced.
- Isolate unavoidable OS-specific behavior behind small functions that take the operating system as an argument, as `dataDirFor` does, so that every branch can be tested on one machine. Keep a portable fallback where possible.
- Add tests for path handling and platform-specific behavior. Every statement of hand-written Go must be executed by a test; `make cover` enforces it. Verify supported targets before claiming support: `scripts/build-release.sh` builds every one of them.
