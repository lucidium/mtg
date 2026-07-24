# Tech Stack

## Language & runtime

- **Go** (module declares `go 1.26`). Use the latest Go toolchain.
- Module path: `github.com/9seconds/mtg/v2`.

## Tooling (managed via mise, see `.mise.toml`)

- **mise** for tool/version management (`.mise.toml`, `mise.lock`).
- **golangci-lint** — linter (config: `.golangci.toml`, v2 format).
- **gofumpt** (`mvdan.cc/gofumpt`) — formatter, stricter than gofmt.
- **goreleaser** — release builds (config: `.goreleaser.yml`).
- **govulncheck** — vulnerability scanner.
- **pkgsite** — local doc server.

## Key dependencies

- `alecthomas/kong` — CLI parser.
- `pelletier/go-toml/v2` — TOML config parsing.
- `rs/zerolog` — structured logging.
- `prometheus/client_golang` — Prometheus metrics.
- `smira/go-statsd` — StatsD metrics.
- `panjf2000/ants/v2` — goroutine pool.
- `stretchr/testify` — test assertions/mocks.
- `jarcoal/httpmock` — HTTP mocking in tests.
- `mccutchen/go-httpbin` — test HTTP server.
- `tylertreat/BoomFilters` — bloom filters (antireplay).
- `yl2chen/cidranger` — CIDR matching (ipblocklist).
- `pires/go-proxyproto` — PROXY protocol.
- `armon/go-socks5`, `things-go/go-socks5`, `txthinking/socks5` — SOCKS5 support.

## Build/release

- GoReleaser for cross-platform release tarballs (`.goreleaser.yml`).
- Docker support (`Dockerfile`, `.dockerignore`).
- PGO profile: `default.pgo`.
- Build tags: `prof` (profiling), `netgo` (static networking).