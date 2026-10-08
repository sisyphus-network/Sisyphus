# Repository implementation rules

## Cross-platform support

- Treat cross-platform behavior as a design requirement for daemon code. Keep domain logic independent of operating-system-specific paths and APIs.
- The daemon is `apps/sisyphusd`, written in Go. Build it with `make build`; run `bin/sisyphusd run`. The earlier Rust implementation was retired; do not restore it as the development runtime.
- Never hardcode home-directory paths, path separators, or assumptions about a writable current directory. Use Go's `path/filepath` and the existing daemon data-directory helpers.
- Put persistent node data in the OS-appropriate application data directory. The SQLite database contains the private node identity, so protect the database and its containing directory with restrictive permissions where the platform supports them; rely on the user's inherited ACL on Windows unless a dedicated ACL implementation is introduced.
- Isolate unavoidable OS-specific behavior behind small helpers and Go build tags. Keep a portable fallback where possible.
- Add tests for path handling and platform-specific behavior. Verify supported targets in CI before claiming support; the Nix dev shell currently targets Linux and macOS.
