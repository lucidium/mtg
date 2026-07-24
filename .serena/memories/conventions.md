# Code Conventions

## Formatting & style

- Format with `gofumpt -w --extra .` (stricter than gofmt). Run before committing.
- golangci-lint v2 config in `.golangci.toml` (linters currently use defaults — the `enable-all` block is commented out).

## Structure

- Package-by-feature layout. `mtglib` is the core library; other packages provide implementations for interfaces defined there.
- `internal/` holds private packages (cli, config, proxyprotocol, testlib, utils) — not importable externally.
- Custom config types live one-per-file in `internal/config/type_*.go`, each with a paired `*_test.go`.
- Platform-specific code uses build tags/suffixes: `sockopts_unix.go`, `sockopts_openbsd.go`, `sockopts_windows.go`, `root_context_windows.go`.
- Test helpers/mocks centralized in `internal/testlib/` (mocks for events observer, network, conn, antireplay cache).

## Testing

- Tests use `github.com/stretchr/testify` (assert/require/mock).
- Test files (`*_test.go`) sit alongside source files.
- Internal tests (`*_internal_test.go`) used where white-box access is needed.
- Fuzz tests live in `mtglib/internal/tls/fake` and `mtglib/internal/obfuscation`.
- HTTP mocking via `jarcoal/httpmock`; test HTTP server via `mccutchen/go-httpbin`.

## Config

- TOML format (see `example.config.toml`).
- Each config value type is a dedicated type implementing `encoding.TextUnmarshaler` (in `internal/config/type_*.go`).

## Logging

- `rs/zerolog` structured logging (see `logger/zerolog.go`).
- A `noop` logger variant exists for testing/benchmarks.