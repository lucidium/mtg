# Suggested Commands

All project tasks are defined in `.mise.toml` and run via `mise run <task>`.

## Build

- `mise run build` — build the `mtg` binary (`go build`).
- `mise run build:prof` — build with profiling (`-tags prof`); serves pprof on `localhost:${MTG_PROF_PORT:-6000}`.
- `mise run static` — static binary with version injected from git (`-tags netgo`, `-trimpath`, `-ldflags`).
- `mise run image` — build Docker image (`docker buildx build --pull -t mtg .`).
- `mise run release` — GoReleaser snapshot build.

## Test & quality

- `mise run test` — run all tests with race detector (`go test -v -race ./...`).
- `mise run covtest` — tests + coverage (`-coverprofile=coverage.txt -covermode=atomic -count=2 -race`).
- `mise run test:fuzz:client-hello` / `:client-handshake` / `:server-handshake-frame` — fuzz tests (120s each).
- `mise run lint` — `golangci-lint run`.
- `mise run vuln` — `govulncheck ./...`.
- `mise run fmt` — format with `gofumpt -w --extra .`.

## Development

- `mise run docs` — local pkgsite server on `0.0.0.0:10000`.
- `mise run update` — `go get -u` then `go mod tidy -go=1.26`.

## Running

The binary is CLI-based (kong). Examples:
- `./mtg run config.toml` — run proxy with config file.
- `./mtg simple_run` — quick run without config.
- `./mtg generate_secret` — generate a proxy secret.
- `./mtg get_ip` — detect public IP.
- `./mtg doctor` — diagnostic checks.
- `./mtg sni_check` — SNI verification.

See `example.config.toml` for config reference.

## System notes (Darwin/macOS)

- Commands above work as-is on macOS. No platform-specific overrides needed.