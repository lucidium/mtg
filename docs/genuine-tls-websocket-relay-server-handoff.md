# Genuine TLS/WebSocket MTProto Relay — Implementation Handoff

**Purpose:** durable implementation state for short, one-commit coding sessions

**Implementation plan:** `docs/genuine-tls-websocket-relay-server-plan.md`

**Branch at start:** `prep/v2.2.8`

**Baseline commit:** `76e074638ef1c723a5fed656b12dcb79c959e03f`

## Session discipline

- Implement one planned commit per coding session.
- End each session with a buildable commit and update this document in the same
  commit.
- Start the next session from the first unchecked commit below.
- Keep existing FakeTLS behavior backward compatible unless the implementation
  plan explicitly requires otherwise.

## Planned commit status

- [x] Commit 1 — Extract a transport-neutral authenticated stream core.
- [x] Commit 2 — Add the authenticated WebSocket ingress.
- [x] Commit 3 — Add configuration and process lifecycle.
- [x] Commit 4 — Add the Nginx reference deployment.
- [x] Commit 5 — Add end-to-end fixtures and operational hardening.

## Completed work

### Commit 1 — Transport-neutral authenticated stream core

Implemented in the current session:

- Added private ingress modes in `mtglib/proxy.go`:
  - `stockFakeTLS`;
  - `webSocketRelay`.
- Preserved the stock ingress ordering:
  - stream lifecycle/deadline setup;
  - FakeTLS authentication and anti-replay;
  - fake ServerHello and TLS record adapter;
  - Doppelgänger wrapping;
  - shared authenticated stream core.
- Extracted `serveAuthenticatedStream`, which owns:
  - client Obfuscated2 handshake parsing;
  - handshake-deadline clearing;
  - policy-aware Telegram DC selection and dialing;
  - Telegram-side Obfuscated2 handshake;
  - connected event emission;
  - idle-tracked bidirectional relay and traffic accounting.
- Added an explicit per-ingress authenticated stream policy:
  - stock FakeTLS uses the configured secret-derived obfuscator and preserves
    `AllowFallbackOnUnknownDC`;
  - WebSocket relay uses `obfuscation.Obfuscator{Secret: nil}` and does not
    permit unknown-DC fallback.
- Changed `streamContext` to store an explicit `net.IP` client identity instead
  of force-casting `clientConn.RemoteAddr()`.
- Extracted `Proxy.allowClientIP` so a future HTTP/WebSocket ingress can apply
  the existing allowlist/blocklist policy to validated `X-Real-IP` before
  upgrade, without deriving identity from the WebSocket adapter.
- Kept `EventStart -> EventConnectedToDC -> EventFinish` ordering for successful
  zero-traffic fixture sessions.

### Commit 1 tests

Added `mtglib/proxy_internal_test.go` coverage for:

- accepting a secretless padded-intermediate Obfuscated2 header through the
  WebSocket ingress mode;
- dialing only an address returned for the requested known Telegram DC;
- producing zero network dials for an unknown WSS DC;
- preserving stock unknown-DC fallback when explicitly enabled;
- carrying the explicit client IP into `EventStart`;
- applying the explicit client IP to allowlist/blocklist checks and rejection
  events;
- preserving start/connected/finish event ordering.

Updated `mtglib/stream_context_internal_test.go` so client identity no longer
depends on `RemoteAddr()`.

### Commit 2 — Authenticated WebSocket ingress

Implemented in the current session:

- Added `github.com/coder/websocket` v1.8.15 as a pinned direct dependency after
  consulting the versioned API documentation.
- Added `mtglib/websocket_relay.go` with `Proxy.NewWebSocketRelayHandler` and
  focused `WebSocketRelayOptions`; configuration parsing and listener/process
  lifecycle remain intentionally deferred to Commit 3.
- Added uniform pre-upgrade rejection after validating, in order:
  - the immediate socket peer is loopback;
  - the request path is an exact escaped and decoded match with no query;
  - the method is `GET`;
  - one Authorization field contains one Bearer credential;
  - the credential matches one active token;
  - the required `telegram-mtproto-relay.v1` subprotocol is offered;
  - one valid `X-Real-IP` value is supplied;
  - the existing allowlist/blocklist policy permits that client IP;
  - a bounded WSS connection slot is available.
- Validated WebSocket upgrade headers and explicitly reject every present
  browser `Origin` before upgrade.
- Added startup token validation for one to four unique unpadded base64url
  values decoding to 32–64 bytes. Runtime matching stores SHA-256 digests,
  compares every configured digest with `crypto/subtle`, and does not expose
  token values in errors.
- Upgraded with exact subprotocol selection and compression disabled and
  adapted binary messages with `websocket.NetConn` plus
  `essentials.WrapNetConn`. Commit 5 replaced the initial library-limit
  restoration with the boundary-safe adapter guard described below.
- Reused `serveConn(conn, validatedClientIP, webSocketRelay)` synchronously, so
  existing handshake deadlines, strict known-DC routing, idle tracking,
  traffic/events, cancellation, and stream wait-group ownership remain intact.
- Added adapter close handling that maps oversized messages to 1009, normal
  session termination to 1000, and proxy shutdown to 1001 without logging
  peer-supplied close reasons.
- Kept `mtglib/proxy.go` unchanged from Commit 1 after self-review; all WSS
  adaptation and close behavior remain isolated in the new ingress file.

### Commit 2 tests

Added `mtglib/websocket_relay_test.go` coverage for:

- valid current and next rotation tokens, exact echoed subprotocol, and absent
  compression negotiation;
- token format/count/uniqueness validation without credential leakage;
- uniform rejection for wrong path, query, token, subprotocol, client IP,
  method, non-loopback peer, browser Origin, and blocked clients;
- duplicate Authorization fields;
- a 64-byte Obfuscated2 header fragmented across several binary messages;
- ordered upstream and downstream relay through real secretless Obfuscated2
  client and controlled Telegram peers;
- text-message close status 1003 and oversized-message close status 1009;
- connection-cap rejection and slot release;
- active-session shutdown close status 1001.

### Commit 3 — Configuration and process lifecycle

Implemented in the current session:

- Added `[websocket-relay]` TOML parsing with:
  - `enabled`;
  - `bind-to`;
  - `path`;
  - one to four rotation `tokens`;
  - `max-connections`, defaulting to 2048;
  - `max-message-size`, defaulting to 16 MiB.
- Added enabled-only startup validation that requires:
  - an IPv4 or IPv6 loopback bind address;
  - a printable ASCII, absolute, non-root, canonical path no longer than 1024
    bytes, with no query, fragment, percent encoding, backslash, control,
    whitespace, or path-normalization ambiguity;
  - one to four unique canonical unpadded base64url tokens decoding to 32–64
    bytes;
  - 1–65535 maximum connections;
  - a 64 KiB–64 MiB maximum WebSocket message size;
  - no bind collision with either the legacy listener or an enabled
    Prometheus listener, including wildcard-address collisions.
- Preserved the prior startup path when the relay is absent or explicitly
  disabled; relay fields are not required while disabled.
- Centralized diagnostic configuration rendering through a redacted JSON
  projection:
  - the legacy MTProxy secret always renders as `[redacted]`;
  - WebSocket relay tokens render only as a configured-count marker;
  - parsing and runtime credential values remain unchanged.
- Added `internal/cli/proxy_lifecycle.go` to own process serving and shutdown:
  - legacy and optional WebSocket listeners report unexpected serve exits as
    process-level errors;
  - root cancellation first stops HTTP upgrades with bounded
    `http.Server.Shutdown`;
  - the legacy listener is then closed and `Proxy.Shutdown` cancels and waits
    for active legacy and WebSocket streams;
  - serve goroutines are drained before process return.
- Constructed the loopback plaintext HTTP server only when enabled, with:
  - a 5-second `ReadHeaderTimeout`;
  - a 30-second idle timeout for non-upgraded HTTP connections;
  - a 16 KiB maximum header size;
  - a 10-second graceful-shutdown budget;
  - no TLS configuration, because Nginx termination remains Commit 4.
- Mapped the validated config directly to `WebSocketRelayOptions` and retained
  the Commit 2 handler's independent immediate-peer loopback check.
- Updated `example.config.toml` with a disabled-by-default relay block and
  comments for loopback trust, hidden canonical paths, token rotation, and
  bounded defaults.

### Commit 3 tests

Added focused coverage for:

- valid one-token and two-token rotation configurations and defaults;
- explicitly disabled relay configuration preserving legacy validation;
- IPv4/IPv6 loopback binds, non-loopback rejection, and legacy/Prometheus bind
  collisions;
- malformed, ambiguous, normalized, percent-encoded, oversized, and otherwise
  invalid paths;
- missing, malformed, padded, short, oversized, duplicate, and excessive
  tokens without credential leakage in errors;
- zero/undersized/oversized connection and message limits;
- redacted `Config.String()` output and captured real debug-log output;
- bounded HTTP server settings with no TLS configuration;
- disabled legacy-only process shutdown;
- actual loopback HTTP serving followed by clean bounded shutdown;
- process-level propagation of unexpected legacy and WebSocket serve failures.

### Commit 4 — Nginx reference deployment

Implemented in the current session:

- Added `docs/nginx-websocket-relay.conf`, a placeholder-driven Nginx `http`
  context reference with:
  - a loopback `mtg` WebSocket upstream and separate genuine-site decoy
    upstream;
  - an exact-match relay location and HTTP/1.1 WebSocket forwarding;
  - explicit forwarding of `Authorization` and the required subprotocol;
  - overwritten `X-Real-IP` and cleared `X-Forwarded-For`;
  - disabled proxy buffering, request buffering, and caching;
  - read/send timeouts longer than the default `mtg` relay idle timeout;
  - interception of backend pre-upgrade and availability failures;
  - a loop-free named decoy location that makes a bodyless `GET` for ordinary
    root content and strips credentials, body framing, and WebSocket headers;
  - ordinary-site routing that also strips relay-sensitive headers when a
    client uses a wrong path;
  - a credential-safe access log based on normalized `$uri`, without request
    headers, query strings, or referrers.
- Added `docs/genuine-tls-websocket-relay-deployment.md` documenting:
  - the trust boundary and independent legacy FakeTLS listener;
  - secure token and random-path generation;
  - loopback binding, container/network-namespace constraints, and firewalling;
  - placeholder replacement and required Nginx invariants;
  - timeout ordering and the absence of a version 1 WebSocket ping policy;
  - external certificate issuance and renewal;
  - public probe, backend failure, real-IP, and log-safety validation;
  - old/new token overlap for zero-downtime rotation;
  - staged rollout and independent rollback.
- Added `docs/nginx_websocket_relay_test.go` to statically lock down the
  deployment-critical routing, forwarding, decoy, timeout, placeholder, and
  credential-safe logging properties.
- Kept Commit 5 end-to-end fixtures and operational hardening unstarted.

### Commit 4 tests

Passed:

```bash
go test -race ./docs -count=1 -timeout=60s
go test ./... -skip 'TestProxy/TestHTTPSRequest|TestNetwork/TestRealHTTPRequest' -count=1 -timeout=180s
go vet ./...
go build ./...
git diff --check
```

The reference was also rendered with concrete temporary loopback upstreams,
path, hostname, log paths, and a generated one-day self-signed certificate,
then passed the locally installed Nginx 1.31.2 syntax check:

```bash
nginx -t -p "$tmpdir/" -c "$tmpdir/nginx.conf"
```

The full repository suite was not rerun against the live `httpbin.org` fixtures;
the two tests excluded above are the same externally dependent tests documented
under Commit 3. No Go production code changed in Commit 4. `golangci-lint` and
Docker were not rerun for this documentation/reference-only commit.

### Commit 5 — End-to-end fixtures and operational hardening

Implemented in the current session:

- Added `mtglib/websocket_relay_integration_test.go`, a controlled end-to-end
  fixture that uses a real loopback HTTP/WebSocket server, the production
  authenticated handler, a generated secretless padded-intermediate
  Obfuscated2 client header, and a fake Telegram endpoint connected through
  `net.Pipe`.
- The fixture verifies:
  - a header and first encrypted packet in one WebSocket message;
  - headers and subsequent packets split over arbitrary binary-message
    boundaries;
  - ordered padded-intermediate bytes in both directions after independently
    completing the Telegram-side Obfuscated2 handshake;
  - per-message limits rather than cumulative stream limits;
  - connection-slot exhaustion and release after handshake timeout;
  - active-relay idle timeout closing both endpoints;
  - bounded concurrent failed-auth probes without stream events or Telegram
    dials;
  - shutdown during authentication, Obfuscated2 header read, Telegram dial,
    and active relay;
  - ordered 512 KiB transfers while each direction is deliberately consumed
    slowly;
  - repeated current/next token rotation configurations rejecting the retired
    token;
  - shutdown unblocking a relay write whose fake Telegram consumer is stalled.
- Changed Telegram connection attempts in `mtglib/proxy.go` from
  `Network.Dial` to `Network.DialContext(ctx, ...)`. This preserves all address
  selection and fallback behavior while allowing root/stream cancellation to
  stop an in-progress Telegram dial instead of waiting for the underlying
  network timeout.
- Hardened `webSocketRelayConn` message-size enforcement. The adapter now
  probes one byte beyond the configured limit before returning bytes at the
  limit boundary, closing with WebSocket status 1009 when that byte belongs to
  the same message. This fixes the exact-boundary case where an oversized
  first message could otherwise be consumed as a complete 64-byte handshake
  before `coder/websocket` surfaced its size error.
- Kept WebSocket stream adaptation, close status selection, and deadlines
  isolated in `mtglib/websocket_relay.go`; no protocol, configuration, Nginx,
  event, or legacy FakeTLS surface changed.

### Commit 5 tests

Passed:

```bash
go test -race ./mtglib -run '^TestWebSocketRelay' -count=1 -timeout=180s
go test ./mtglib -run 'TestWebSocketRelay(EndToEnd|MessageLimit|HandshakeTimeout|IdleTimeout|RapidFailed|Shutdown|SlowConsumers|RepeatedToken|BlockedOperations|RejectsTextAndOversizedMessages)' -count=10 -timeout=180s
go test -race ./mtglib/... ./internal/config/... ./internal/cli/... -skip 'TestProxy/TestHTTPSRequest' -count=1 -timeout=240s
go test ./... -skip 'TestProxy/TestHTTPSRequest|TestNetwork/TestRealHTTPRequest' -count=1 -timeout=240s
go vet ./...
go build ./...
git diff --check
docker build -t mtg-wss-commit5-validation .
```

The unskipped focused race command again reached the existing live
`https://httpbin.org` fixture and received HTTP 503 in
`mtglib/TestProxy/TestHTTPSRequest`; the same command with only that external
test skipped passed. `golangci-lint` was not installed in the session
environment, so lint could not be run. Docker Desktop was available and the
repository image built successfully.

## Validation performed

Commit 3 passed:

```bash
go test ./internal/config ./internal/cli -count=1 -timeout=60s
go test -race ./mtglib/... ./internal/config/... ./internal/cli/... -count=1 -timeout=180s
go test ./... -skip 'TestProxy/TestHTTPSRequest|TestNetwork/TestRealHTTPRequest' -count=1 -timeout=180s
go vet ./...
git diff --check
```

The changed Go files were formatted with the available repository formatter.
`golangci-lint` was not installed in the session environment, so lint could not
be run.

The full repository command was also run:

```bash
go test ./... -count=1 -timeout=180s
```

All packages passed except the same two existing tests that depend on the live
`https://httpbin.org` service. Both endpoints returned HTTP 503 in this session:

- `mtglib/TestProxy/TestHTTPSRequest` expected 200 but received 503;
- `network/TestNetwork/TestRealHTTPRequest` expected 200 but received 503.

The repository suite excluding exactly those two external fixtures passed as
shown above. Docker validation was attempted with:

```bash
docker build -t mtg-wss-commit3-validation .
```

It could not start because the local Docker daemon socket was unavailable at
`~/.docker/run/docker.sock`; no image build was executed.

Commit 2 previously passed:

```bash
go test ./mtglib -run '^TestWebSocketRelay' -count=1 -timeout=30s
go test -race ./mtglib -run '^TestWebSocketRelay' -count=1 -timeout=60s
go test ./mtglib -count=1 -timeout=120s
go test ./... -skip 'TestProxy/TestHTTPSRequest|TestNetwork/TestRealHTTPRequest' -count=1 -timeout=180s
go vet ./...
git diff --check
gofmt -d mtglib/websocket_relay.go mtglib/websocket_relay_test.go
```

The full repository command was also run:

```bash
go test ./... -count=1 -timeout=180s
```

All packages passed except the same two existing tests that depend on the live
`https://httpbin.org` service:

- `mtglib/TestProxy/TestHTTPSRequest` timed out waiting for response headers;
- `network/TestNetwork/TestRealHTTPRequest` timed out waiting for response
  headers and then hit the existing nil-response test panic.

These are external fixture/service failures, not compile, race, or WebSocket
relay failures. The repository suite excluding only those two live fixtures
passed as shown above. `golangci-lint` was not installed in the session
environment, so lint was not run; `go vet ./...` passed.

## Important implementation notes

- `WebSocketRelayOptions` and `Proxy.NewWebSocketRelayHandler` remain the narrow
  construction seam. `runProxy` maps only already-validated TOML values into
  those options.
- Both configuration validation and the handler enforce the loopback boundary:
  the former rejects non-loopback binds, while the latter rejects non-loopback
  immediate HTTP peers.
- Every pre-upgrade rejection uses one internal 404 response with a fixed small
  body. Nginx decoy interception remains Commit 4 deployment work.
- Token validation happens once during handler construction. Runtime state
  retains only SHA-256 digests, and token comparison checks every configured
  digest before deciding.
- Every present `Origin` is rejected. Native Telegram clients are expected to
  omit it; browser support remains a non-goal.
- `websocket.NetConn` is intentionally constructed with a background lifetime
  context. The existing stream lifecycle closes the wrapper on root
  cancellation, allowing the adapter to send 1001 rather than racing an
  internal context-driven socket teardown.
- `websocket.NetConn` resets the library read limit. The handler deliberately
  leaves that internal limit disabled and applies its own boundary-safe stream
  guard so the exact configured size is accepted while an additional byte in
  the same message is rejected.
- `webSocketRelayConn.Read` preserves 1009 for oversized messages. Other
  post-upgrade parser/relay failures close generically without exposing
  details; normal closes use 1000 and root cancellation uses 1001.
- The message-size guard probes one byte past the configured limit before
  exposing a boundary-sized read. Because `websocket.NetConn` preserves the
  active message reader across reads, a byte from the next WebSocket message is
  not consumed by that probe.
- Telegram dialing now receives the stream context through
  `Network.DialContext`, so cancellation during DNS/proxy/connect work returns
  through the normal stream cleanup path.
- The WSS connection semaphore is acquired before upgrade and released by a
  handler-scoped defer on every later exit path.
- `serveConn` remains unchanged and owns handshake deadlines, active-session
  cancellation, event ordering, strict WSS DC policy, relay idle timeout, and
  stream wait-group accounting.
- The Nginx reference is intentionally placeholder-driven and must be merged
  with the deployment's genuine site and certificate workflow; `nginx -t`
  should be rerun after every substitution or merge.
- The ordinary and named-decoy proxy locations explicitly clear Authorization
  and WebSocket headers so wrong-path requests and rejected upgrades cannot
  disclose relay credentials to the ordinary site upstream.
- The named decoy disables error interception, rewrites to `/`, discards the
  probe body, and uses `GET`, avoiding recursion through the exact relay route
  while hiding relay request details from the ordinary site upstream.
- All five planned implementation commits are complete. The remaining work is
  deployment/client integration, physical-network validation, and rollout as
  described in the implementation plan rather than another server commit.

## Next coding session

The planned server implementation sequence is complete. Continue with the
client/server integration order in section 10.4 and staged rollout in section
11 of the implementation plan:

1. Update and validate the Telegram-iOS WSS branch against this fixture-tested
   server contract.
2. Run the reference Nginx deployment with a real certificate and decoy site.
3. Exercise certificate, hostname, redirect, path, token, subprotocol, transfer,
   reconnect, and network-transition cases.
4. Repeat physical-device tests on unrestricted and affected restricted paths,
   then proceed through staged rollout and token rotation.
