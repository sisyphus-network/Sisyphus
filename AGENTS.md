# Repository implementation rules

## Cross-platform support

- Treat cross-platform behavior as a design requirement for daemon code. Keep domain logic independent of operating-system-specific paths and APIs.
- Never hardcode home-directory paths, path separators, or assumptions about a writable current directory. Use `Path`/`PathBuf` and the shared platform helpers in `apps/sisyphusd-rs/src/platform.rs`.
- Put persistent node data in the OS-appropriate application data directory. The SQLite database contains the private node identity, so protect the database and its containing directory with restrictive permissions where the platform supports them; rely on the user's inherited ACL on Windows unless a dedicated ACL implementation is introduced.
- Isolate unavoidable OS-specific behavior behind small helpers and `#[cfg(...)]` blocks. Keep a portable fallback where possible.
- Add tests for path handling and platform-specific behavior. Verify supported targets in CI before claiming support; the Nix dev shell currently targets Linux and macOS.
