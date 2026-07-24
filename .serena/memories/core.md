# MTG Project Core

## What is this

mtg (MTProto Proxy) - a Telegram MTProto proxy server written in Go.
Module: `github.com/9seconds/mtg/v2`. See `mem:tech_stack` for details.

## Top-level source map

- `main.go` — thin CLI entrypoint. Parses args via `kong`, delegates to `internal/cli`.
- `buildinfo.go` — build version info (`getVersion()`).
- `run_profile*.go` — pprof profiling server (build tag `prof`).
- `internal/cli/` — CLI commands (`run`, `run_proxy`, `simple_run`, `generate_secret`, `get_ip`, `doctor`, `sni_check`, `access`). Entry: `cli.CLI` struct parsed by kong.
- `internal/config/` — TOML config parsing + custom types (`type_*.go`). Parser: `config.go`/`parse.go`.
- `internal/proxyprotocol/` — PROXY protocol adapter.
- `internal/testlib/` — shared test mocks (events observer, network, conn, antireplay cache).
- `internal/utils/` — helpers (QR code URL, config reader, net listener, root context).
- `mtglib/` — **core library**. Start here to understand proxy logic. Key: `mtglib/proxy.go` (`Proxy` struct), `mtglib/conns.go`, `mtglib/secret.go`, `mtglib/stream_context.go`.
  - `mtglib/internal/dc/` — Telegram data center connection logic.
  - `mtglib/internal/doppel/` — Doppelgänger (TLS fingerprint camouflage).
  - `mtglib/internal/relay/` — data relay between client and Telegram.
  - `mtglib/obfuscation/` — MTProto obfuscation.
  - `mtglib/dcprobe/` — DC probe logic.
- `network/` — networking primitives (dialers, socks5, DNS resolver, circuit breaker, sockopts).
  - `network/v2/` — newer network API (under development).
- `stats/` — metrics (Prometheus `prometheus.go`, StatsD `statsd.go`, pools).
- `events/` — event stream + observers (observable events).
- `logger/` — zerolog-based logging + noop.
- `antireplay/` — anti-replay attack protection (stable bloom filter).
- `ipblocklist/` — IP blocklist support (FireHOL, local/http files).
- `essentials/` — basic types (addresses, connections).
- `contrib/sni-router/` — example SNI routing config (Caddy/haproxy/docker-compose).

## Invariants

- The application is a thin shell around `mtglib.Proxy`; most packages implement interfaces defined in `mtglib`.
- Config is TOML; custom types in `internal/config/type_*.go` each have their own `_test.go`.
- Tests use `github.com/stretchr/testify`.
- Cross-platform support: build tags used for `sockopts` (unix/openbsd/windows) and `root_context` (windows).

## Further reading

- Tech stack & tooling: `mem:tech_stack`
- Commands (build/test/lint/run): `mem:suggested_commands`
- Code conventions: `mem:conventions`
- Task completion checklist: `mem:task_completion`