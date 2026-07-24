# Task Completion Checklist

Before declaring a coding task done, run these and ensure they pass:

## Mandatory

1. **Format**: `mise run fmt` (runs `gofumpt -w --extra .`)
2. **Lint**: `mise run lint` (runs `golangci-lint run`) — must be clean.
3. **Test**: `mise run test` (runs `go test -v -race ./...`) — all tests pass.
4. **Build**: `mise run build` (runs `go build`) — compiles cleanly.

## Recommended

5. **Vulnerability scan**: `mise run vuln` (`govulncheck ./...`) — check for known CVEs in deps.
6. **Vet** (implicitly covered by golangci-lint, but can run directly): `go vet ./...`

## Notes

- If you modified config types (`internal/config/type_*.go`), run the paired `_test.go` specifically.
- If you touched platform-specific files (build-tagged suffixes), consider testing on the target OS where feasible.
- Coverage runs: `mise run covtest` for `-coverprofile=coverage.txt` output.