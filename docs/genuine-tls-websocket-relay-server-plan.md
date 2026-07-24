# Genuine TLS/WebSocket MTProto Relay — Server Implementation Plan

**Status:** implementation-ready plan  
**Date:** 2026-07-24  
**Target repository:** `9seconds/mtg` v2 fork  
**Source revision inspected:** `2285e8f35b4580a511d25966ca67546c5deebfe8`  
**Companion client plan:** `~/git/Telegram-iOS/docs/custom/genuine-tls-websocket-relay-client-plan.md`

## 1. Purpose

Add an optional, authenticated WebSocket ingress to this `mtg` fork so the
custom Telegram-iOS client can carry an ordinary obfuscated MTProto byte stream
through genuine TLS and a standards-compliant WebSocket connection.

The durable path is:

```text
Telegram-iOS fork
    -> wss://relay.example.com:443/<hidden-relay-path>
    -> genuine TLS terminated by Nginx
    -> HTTP/1.1 WebSocket reverse proxy over loopback
    -> authenticated WebSocket ingress in mtg
    -> secretless Obfuscated2 padded-intermediate stream
    -> a known Telegram datacenter selected by the standard 64-byte header
    -> existing mtg Telegram dial and bidirectional relay
```

This is a second ingress into the existing relay core. It is not a replacement
for the current public FakeTLS MTProxy listener. Existing `ee` clients and
configuration must continue to work unchanged.

## 2. Background and evidence

The companion research artifacts in the Telegram-iOS repository establish the
motivation and client requirements:

- `docs/custom/mtproxy-research-handoff.md`
- `docs/custom/mtproxy-research-findings-2026-07-24.md`
- `docs/custom/genuine-tls-websocket-relay-client-plan.md`

The restricted-path captures showed that:

1. TCP connectivity to the deployed `mtg` endpoint was available.
2. Fake ClientHello traffic was initially accepted, but fake ServerHello
   traffic was silently suppressed.
3. Later attempts consistently lost the tail of the fake ClientHello.
4. The same client, proxy, secret, and server worked through a VPN control.
5. ClientHello-only fingerprint changes are therefore unlikely to be durable;
   stateful classification of the fake-TLS exchange is the stronger current
   hypothesis.

A genuine TLS handshake, valid public certificate, normal HTTP upgrade, and
encrypted WebSocket application traffic remove the fake ServerHello/CCS
ceremony that appears to trigger the path classifier.

## 3. Protocol contract correction

### 3.1 Important correction to the previous client plan

The previous client plan says that WSS should retain Telegram-iOS's ordinary
non-secret transport default. In the current client implementation, that means
the **abridged** marker `0xefefefef`.

This `mtg` implementation does not accept abridged framing. Its obfuscation
implementation is deliberately fixed to padded-intermediate framing:

```go
// mtglib/internal/obfuscation/handshake_frame.go
var hfConnectionType = [4]byte{0xdd, 0xdd, 0xdd, 0xdd}
```

The server also opens a padded-intermediate connection to Telegram and relays
the decrypted packet framing without translating abridged packets into padded
intermediate packets. Accepting `0xefefefef` at ingress alone would therefore
not be sufficient.

### 3.2 Contract to freeze for version 1

The WSS payload protocol is:

> **Secretless Obfuscated2 with padded-intermediate framing
> (`0xdddddddd`) inside a genuine WebSocket connection.**

Consequences:

- The first upstream bytes are the standard 64-byte Obfuscated2 header.
- Bytes 56–59 of the canonical plaintext header contain `0xdddddddd`.
- Bytes 60–61 contain the signed Telegram datacenter tag in little-endian
  form, as in the existing Telegram transport.
- AES keys and IVs are derived from the standard header without an MTProxy
  secret.
- There is no additional application handshake or destination field.
- The server uses `obfuscation.Obfuscator{Secret: nil}`.
- The Telegram-iOS WSS branch must set `_useIntermediateFormat = true` while
  keeping `_mtpSecret = nil`.
- Existing `ee` and other MTProxy branches retain their current behavior.

This reuses the existing well-tested `mtg` handshake parser and Telegram relay
without adding a framing translator.

## 4. Frozen external protocol

### 4.1 Endpoint configuration

One saved relay endpoint contains:

```text
host:   relay.example.com
port:   443
path:   /relay/v1/<deployment-specific-random-path>
token:  at least 32 random bytes encoded as unpadded base64url
```

Rules:

- Only `wss://` is supported by the client.
- `host` is a DNS name used for resolution, SNI, and certificate validation.
- The path is absolute and credentials never appear in it.
- The token appears only in the WebSocket upgrade request:

  ```http
  Authorization: Bearer <token>
  ```

- The required versioned subprotocol is:

  ```http
  Sec-WebSocket-Protocol: telegram-mtproto-relay.v1
  ```

- The server must select and echo exactly that subprotocol.
- Redirects are not part of the server contract and are rejected by the
  client.

### 4.2 Stream semantics

- Only WebSocket binary messages carry application bytes.
- WebSocket message boundaries have no protocol meaning.
- Concatenating client binary messages reconstructs one ordered upstream byte
  stream.
- Concatenating server binary messages reconstructs one ordered downstream byte
  stream.
- One WebSocket connection maps to one Telegram TCP connection.
- There is no multiplexing or stream resumption in version 1.
- Compression is never negotiated.
- The first upstream bytes are the 64-byte secretless padded-intermediate
  Obfuscated2 header described above.
- Closing either side closes the other side.

The client bounds a received WebSocket message at 16 MiB. The server normally
writes much smaller messages; reusing `mtg`'s relay buffers produces messages
of approximately 16 KiB on non-constrained architectures.

### 4.3 Destination policy

The WSS endpoint is not a CONNECT proxy.

- The only destination selector is the standard datacenter tag in the
  Obfuscated2 header.
- The tag is accepted only if it resolves through `dc.Telegram.GetAddresses`.
- Unknown, malformed, test-only, or otherwise unsupported tags are rejected.
- `AllowFallbackOnUnknownDC` does not apply to WSS in version 1.
- No client-supplied hostname, IP address, or port is ever passed to a dialer.

## 5. Deployment architecture

### 5.1 Selected topology

Nginx owns the public HTTPS endpoint and ordinary website. `mtg` owns a second,
loopback-only HTTP listener for the relay route.

```text
public Internet
    |
    | TCP :443, genuine TLS
    v
Nginx
    |-- normal and unrelated routes --------> ordinary website
    |
    `-- exact configured relay path
          |-- valid authenticated upgrade --> 127.0.0.1:<wss-port> (mtg)
          `-- any rejection/error ----------> internal ordinary-site fallback

mtg legacy listener remains independently bound to its configured TCP address
```

### 5.2 Why TLS stays outside `mtg`

- Nginx already provides mature certificate issuance and rotation workflows.
- The public endpoint behaves like a real web server for unrelated traffic.
- `mtg` does not need to read private certificate keys.
- The Go relay remains independent of ACME, virtual-host, and static-site
  concerns.
- Failed relay probes can be replaced with the site's real content.
- The loopback boundary is small and easy to firewall.

### 5.3 Trust boundary

Version 1 requires the WebSocket listener to bind to a loopback address.
Nginx is the only trusted HTTP peer.

Nginx overwrites, rather than appends or preserves, the client address header:

```nginx
proxy_set_header X-Real-IP $remote_addr;
```

`mtg` accepts `X-Real-IP` only because the immediate socket peer is loopback.
If a future version allows non-loopback reverse proxies, it must add an explicit
trusted-proxy CIDR list before accepting forwarding headers.

## 6. Current server architecture and reusable seams

The current connection path in `mtglib/proxy.go` is:

```text
Proxy.ServeConn
    -> worker/concurrency gate
    -> streamContext
    -> FakeTLS ClientHello and anti-replay
    -> fake ServerHello
    -> TLS-record read adapter and Doppelganger write shaping
    -> secret-derived Obfuscated2 handshake
    -> Telegram address selection and dial
    -> relay.Relay
```

The following components should be reused:

- `mtglib/internal/obfuscation` for the 64-byte handshake and AES-CTR stream;
- `mtglib/internal/dc` for known Telegram datacenter addresses;
- the `Network` abstraction for Telegram dialing and configured upstream
  proxies;
- `connTraffic` and existing events for byte accounting;
- `connIdleTimeout` for active relay idle limits;
- `mtglib/internal/relay` for bounded bidirectional copying;
- `streamContext` for cancellation, structured logging, stream ID, and cleanup;
- `Proxy` lifecycle and shutdown context.

The WSS ingress must deliberately bypass:

- fake ClientHello parsing and authentication;
- fake-TLS replay cache use;
- domain-front fallback;
- fake ServerHello generation;
- the TLS record adapter;
- Doppelganger TLS-record shaping.

Those components model a fake TLS TCP stream and are neither useful nor correct
inside an already genuine WebSocket/TLS stream.

## 7. Implementation sequence

Implementation should be split into reviewable commits. Each commit should keep
the repository buildable and its focused tests passing.

### 7.1 Commit 1 — Extract a transport-neutral authenticated stream core

#### Files and domain

- `mtglib/proxy.go`: connection orchestration and Telegram relay
- `mtglib/stream_context.go`: client identity and stream lifetime
- `mtglib/proxy_test.go`: behavior and regression coverage
- relevant internal obfuscation tests

#### Changes

1. Introduce an internal ingress mode or policy value with at least:

   ```text
   stockFakeTLS
   webSocketRelay
   ```

   Keep it private until a stable public API is actually needed.

2. Extract the common post-ingress flow from `ServeConn`:

   ```text
   read/decrypt Obfuscated2 header
   -> validate datacenter according to ingress policy
   -> dial Telegram
   -> send Telegram-side obfuscated handshake
   -> emit connected event
   -> relay and account traffic
   ```

3. Preserve the legacy call graph and ordering. In particular, the existing
   FakeTLS authentication, replay check, fake ServerHello, record adapters, and
   Doppelganger stay before the common flow for `stockFakeTLS`.

4. For `webSocketRelay`, invoke the common flow with:

   ```go
   obfuscation.Obfuscator{Secret: nil}
   ```

   and a policy that requires an exact known DC.

5. Do not merely set `allowFallbackOnUnknownDC` globally. Unknown-DC behavior
   must be an explicit per-ingress policy so legacy compatibility cannot weaken
   the WSS endpoint.

6. Change `newStreamContext` to receive an explicit `net.IP` client identity.
   `streamContext.ClientIP()` currently force-casts `RemoteAddr()` to
   `*net.TCPAddr`; this is incorrect for a WebSocket adapter and would expose
   only Nginx's loopback address.

7. Keep stream event ordering stable:

   ```text
   EventStart
   -> optional EventConnectedToDC
   -> zero or more EventTraffic
   -> EventFinish
   ```

8. Keep the existing `relay.Relay` buffer pool. It provides bounded memory and
   natural downstream WebSocket message sizes without a new application queue.

#### Tests

- A nil-secret padded-intermediate handshake is accepted by the extracted
  core.
- The DC tag selects only an address returned for that known DC.
- Unknown DC does not call the network dialer for WSS.
- Legacy fallback-on-unknown-DC behavior remains unchanged where configured.
- Legacy FakeTLS still invokes its existing authentication and relay path.
- The explicit client IP reaches allow/block checks and event/log context.

### 7.2 Commit 2 — Add the authenticated WebSocket ingress

#### WebSocket library

Add `github.com/coder/websocket` as a direct dependency.

Reasons:

- focused API and no transitive dependency tree;
- context-aware reads and writes;
- explicit compression control;
- bounded read support;
- `websocket.NetConn` turns binary messages into the ordered `net.Conn`-like
  stream required by the existing obfuscation and relay code;
- text messages can be rejected rather than silently treated as bytes.

Pin the selected version in `go.mod`/`go.sum` and consult that version's API
documentation while implementing. Do not depend on default compression or
origin behavior implicitly.

#### Proposed files

- `mtglib/websocket_relay.go`
- `mtglib/websocket_relay_test.go`
- small internal helpers only where they materially improve isolation or tests

#### Pre-upgrade validation order

Before returning `101 Switching Protocols`, validate:

1. the immediate peer is loopback;
2. request path exactly equals the configured path;
3. no query string is present;
4. method is `GET`;
5. exactly one `Authorization` field represents one valid Bearer credential;
6. the bearer token matches an active configured token;
7. the offered subprotocol contains `telegram-mtproto-relay.v1`;
8. the Nginx-supplied `X-Real-IP` is one valid IP address;
9. existing allowlist/blocklist policy permits that IP;
10. a WSS connection slot is available.

Authentication and quota checks occur before upgrade because an HTTP response
can still be intercepted by Nginx. After `101`, errors must be represented as a
WebSocket close and cannot be replaced with website content.

Every pre-upgrade failure should use one fixed internal response status and
small generic body. The public indistinguishability property comes from Nginx
intercepting that response and serving the real fallback website.

#### Token verification

- Configure a small list, initially capped at four entries, for rotation.
- Each token is unpadded base64url decoding to 32–64 bytes.
- Decode and validate at startup.
- Store only SHA-256 digests in the runtime verifier where practical.
- Hash the presented token and compare with `crypto/subtle`.
- Compare against every configured digest without returning on the first match.
- Reject duplicate/multiple `Authorization` header values.
- Do not accept credentials in cookies, query parameters, paths, or
  `Sec-WebSocket-Protocol`.
- Never include the token or digest in errors, logs, event fields, metrics,
  connection IDs, or `String()` methods.

#### Upgrade options

- Select exactly `telegram-mtproto-relay.v1`.
- Disable per-message deflate/compression.
- Set a 16 MiB read limit.
- Do not expose permissive origin matching merely to simplify tests. Native
  Telegram clients do not require browser-origin policy; accept an absent
  `Origin` and define any present-origin behavior explicitly.
- Use binary-message stream mode only.

#### Stream handling

1. Upgrade only after all preconditions pass.
2. Wrap the accepted WebSocket with `websocket.NetConn` in binary mode.
3. Adapt it to `essentials.Conn` if required by the existing core.
4. Apply the existing handshake deadline while reading the 64-byte Obfuscated2
   header.
5. Run the extracted WSS stream core synchronously for the lifetime of the
   handler.
6. Clear the handshake deadline after Telegram connection establishment.
7. Apply the existing relay idle timeout.
8. On either-direction completion or error, cancel the stream and close both
   connections.

`essentials.WrapNetConn` converts unavailable half-close behavior to full close.
That is correct for the version 1 rule that closing either side closes the
other.

#### Bounds and backpressure

- Add a WSS-specific bounded semaphore or worker limit.
- Acquire it before upgrade and release it on every exit path.
- Do not use one goroutine or an unbounded channel merely to queue each
  WebSocket message.
- Let `relay.Relay`, the WebSocket implementation, and the underlying sockets
  apply backpressure through blocking writes.
- The existing relay buffer is approximately 16 KiB outside constrained MIPS
  builds, so ordinary downstream messages remain well below 16 MiB.
- Retain a hard read-message limit because a malicious peer can choose its own
  message boundaries.

#### Close behavior

- Text application messages close with an appropriate unsupported-data or
  protocol-error status.
- Oversized messages close with message-too-big.
- Malformed Obfuscated2 data closes with policy/protocol error without exposing
  parser details.
- Server shutdown closes active sessions with going-away where possible.
- Do not log close reasons received from an unauthenticated peer verbatim.

#### Tests

- Valid token and required subprotocol produce `101`.
- Missing/wrong token, path, method, client IP, and subprotocol return the same
  internal rejection.
- Current and next rotation tokens work concurrently.
- A 64-byte handshake split over several binary messages is reassembled.
- Header and first packet in one message remain ordered.
- One read can span messages and one message can satisfy several reads.
- Text and oversized messages are rejected.
- Compression is not negotiated.
- The exact subprotocol is echoed.
- Cancellation, peer close, Telegram close, and idle timeout each terminate
  both sides exactly once.
- Repeated connect/disconnect does not leak goroutines.

### 7.3 Commit 3 — Add configuration and process lifecycle

#### Files

- `internal/config/config.go`
- `internal/config/parse.go`
- new focused scalar/config validation files and tests if needed
- `internal/config/config_test.go`
- `internal/cli/run_proxy.go`
- `example.config.toml`
- README or a focused deployment document

#### Proposed TOML

```toml
[websocket-relay]
enabled = true
bind-to = "127.0.0.1:8081"
path = "/relay/v1/<deployment-specific-random-path>"
tokens = [
  "<current-unpadded-base64url-token>",
  # "<next-token-during-zero-downtime-rotation>",
]
max-connections = 2048
max-message-size = "16MiB"
```

The following stay fixed in version 1 rather than becoming configuration
surface:

- WebSocket subprotocol;
- `X-Real-IP` forwarding header;
- binary-only messages;
- compression disabled;
- secretless padded-intermediate Obfuscated2;
- strict known-DC routing.

#### Validation

- `bind-to` is loopback-only when enabled.
- `path` starts with `/`, is canonical, is not `/`, has no query/fragment,
  control characters, percent-encoded separator ambiguity, or trailing
  normalization ambiguity, and is length-bounded.
- At least one and at most four valid unique tokens are configured.
- `max-connections` is nonzero and bounded.
- `max-message-size` has a safe lower bound and a hard upper bound, with 16 MiB
  as the initial default.
- The WSS bind address does not equal the legacy listener or Prometheus bind
  address.
- Disabled configuration preserves current startup behavior exactly.

#### Listener lifecycle

- Construct a dedicated `http.Server` with:
  - `ReadHeaderTimeout`;
  - bounded `IdleTimeout` for non-upgraded HTTP requests;
  - bounded `MaxHeaderBytes`;
  - no TLS configuration, because the listener is loopback HTTP only.
- Start it from the existing `runProxy` process orchestration after validation.
- Treat unexpected listener failure as a process-level failure rather than
  silently disabling the configured relay.
- On root cancellation:
  1. stop accepting new HTTP requests/upgrades;
  2. call `http.Server.Shutdown` with a bounded grace period;
  3. cancel active WSS stream contexts through `Proxy`;
  4. wait for both legacy and WSS streams;
  5. continue the existing observer, stats, blocklist, network, and logger
     shutdown order.

#### Configuration redaction

`internal/cli/run_proxy.go` currently logs `conf.String()` in debug mode, and
the current JSON rendering includes the MTProxy secret. The WSS token must not
repeat this issue.

Implement centralized redacted rendering:

- WSS tokens render as a fixed marker or count, never as values.
- The existing MTProxy secret should be redacted in the same fix.
- Machine-readable parsing remains unchanged; only diagnostic rendering is
  redacted.
- Tests search captured debug output for the literal configured credentials and
  fail if either appears.

### 7.4 Commit 4 — Nginx reference deployment

Provide a tested reference configuration. Keep hostnames, paths, certificates,
ports, and decoy upstreams as explicit placeholders.

The configuration should have the following shape:

```nginx
map $http_upgrade $connection_upgrade {
    default upgrade;
    ''      close;
}

upstream mtg_websocket_relay {
    server 127.0.0.1:8081;
    keepalive 8;
}

server {
    listen 443 ssl http2;
    server_name relay.example.com;

    # Existing certificate and genuine website configuration lives here.

    location = /relay/v1/<deployment-specific-random-path> {
        proxy_pass http://mtg_websocket_relay;
        proxy_http_version 1.1;

        proxy_set_header Host $host;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection $connection_upgrade;
        proxy_set_header Authorization $http_authorization;
        proxy_set_header Sec-WebSocket-Protocol $http_sec_websocket_protocol;
        proxy_set_header X-Real-IP $remote_addr;

        proxy_buffering off;
        proxy_request_buffering off;
        proxy_read_timeout 6m;
        proxy_send_timeout 6m;

        proxy_intercept_errors on;
        error_page 400 401 403 404 405 409 413 426 429 500 502 503 504 = @decoy;
    }

    location @decoy {
        # Internal redirect to the same site's ordinary content/upstream.
        # The final implementation must avoid redirect loops.
    }

    location / {
        # Existing genuine site.
    }
}
```

The exact tested configuration may vary, but it must preserve these properties:

- HTTP/1.1 is used toward the WebSocket backend.
- `Upgrade` and `Connection` are set explicitly.
- `Authorization` and `Sec-WebSocket-Protocol` reach `mtg`.
- `X-Real-IP` is overwritten with Nginx's direct peer address.
- Proxy buffering is disabled for the tunnel.
- Proxy read/send timeouts exceed `mtg`'s configured relay idle timeout, or the
  endpoint has a deliberately documented ping policy.
- All backend pre-upgrade failures and backend-unavailable failures render the
  real decoy site publicly.
- Other paths never reach the relay backend.
- Access/error log formats do not include `$http_authorization`.
- The token never appears in a URL, Nginx route, variable name, or log field.

The reference deployment should also document:

- firewalling the WSS backend port to loopback;
- certificate issuance/renewal outside `mtg`;
- token generation with a cryptographically secure command;
- token rotation by temporarily configuring old and new values;
- removal of the old token only after clients have migrated;
- independent handling of the existing public FakeTLS listener, if retained.

### 7.5 Commit 5 — End-to-end fixtures and operational hardening

Add an integration fixture that can:

1. establish a WebSocket with the bearer token and required subprotocol;
2. generate a valid secretless padded-intermediate Obfuscated2 header;
3. fragment that header across arbitrary binary message boundaries;
4. send and receive framed MTProto fixture bytes through a fake Telegram
   endpoint or controlled network mock;
5. verify close and timeout behavior.

Add abuse and lifecycle coverage for:

- slow headers;
- authenticated clients that never send the 64-byte handshake;
- oversized WebSocket messages;
- rapid failed-auth probes;
- connection-limit exhaustion;
- shutdown during authentication, header read, Telegram dial, and active relay;
- slow consumers in both directions;
- repeated token rotation configuration.

## 8. Security analysis

### 8.1 Authentication is independent of the MTProxy secret

The WSS bearer token is a separate credential. Do not derive it from or reuse
the stock `ee` secret.

This separation is required because:

- the WSS inner Obfuscated2 stream is intentionally secretless;
- compromise/rotation of one ingress should not force rotation of the other;
- the MTProxy secret has legacy serialization and logging behavior not suited
  to an HTTP bearer credential;
- independent credentials make migration and rollback safer.

### 8.2 No open proxy capability

The implementation must make arbitrary dialing structurally impossible:

- parse only the established Obfuscated2 DC tag;
- use only the existing in-memory Telegram DC map;
- reject unknown DCs before calling `Network.DialContext`;
- never add host, port, URL, CONNECT, or DNS fields to the application protocol;
- test the network mock to prove invalid tags produce zero dials.

### 8.3 Probe behavior

`mtg` returns a uniform internal pre-upgrade failure. Nginx transforms that
into ordinary site content. Public observers should not learn whether:

- the hidden path was correct;
- a bearer token was present or close to valid;
- the subprotocol was offered;
- the relay was at capacity;
- the backend process was available.

Perfect response indistinguishability also depends on the Nginx decoy route.
Integration tests should compare status, principal headers, body class, and
obvious timing differences for representative failures.

### 8.4 Rate and resource controls

The initial server must have:

- a bounded HTTP header size and header-read timeout;
- a WSS connection cap acquired before upgrade;
- the 16 MiB message read cap;
- the existing obfuscated-handshake timeout;
- the existing active-relay idle timeout;
- bounded relay buffers;
- cancellation on either side closing;
- optional Nginx connection/request rate limits documented as defense in depth.

Do not add a token value as a metrics label; that creates both credential and
cardinality problems.

### 8.5 Anti-replay scope

The existing anti-replay mechanism keys the FakeTLS ClientHello session ID and
exists to make fake-TLS probing harder. The WSS endpoint has:

- genuine TLS anti-replay properties;
- explicit bearer authentication;
- a randomized hidden path;
- a fresh random Obfuscated2 header per stream.

It should therefore not invoke the FakeTLS anti-replay cache. If later evidence
justifies WSS replay protection, design it separately around an authenticated,
bounded protocol field rather than misusing the FakeTLS session cache.

## 9. Observability

### 9.1 Reuse existing events

Retain the existing event lifecycle and traffic accounting so Prometheus and
StatsD continue to see:

- stream starts and finishes;
- connected Telegram DC/IP;
- upstream/downstream traffic;
- blocklist and concurrency outcomes where applicable.

### 9.2 Transport differentiation

If operational testing requires differentiating WSS from legacy FakeTLS, add a
bounded transport enum, not endpoint or token values. Prefer one of:

- an internal field on start/finish events with values such as `faketls` and
  `websocket`; or
- dedicated low-cardinality WSS counters owned by the HTTP ingress.

Before changing public event interfaces, evaluate compatibility with every
observer in `events/`, `logger/`, and `stats/`. Do not expand interfaces merely
for convenience in the first functional commit.

### 9.3 Safe logs

Allowed fields include:

- stream ID;
- ingress enum;
- validated client IP according to current logging policy;
- selected DC and Telegram IP;
- stable error category;
- byte counts and duration.

Forbidden fields include:

- bearer token or digest;
- `Authorization` value;
- full request headers;
- credential-bearing configuration rendering;
- raw Obfuscated2 header or key/IV material;
- arbitrary peer-supplied close reason.

## 10. Detailed validation plan

### 10.1 Unit tests

#### Configuration and authentication

- disabled relay preserves current defaults;
- valid one-token and two-token rotation configurations parse;
- malformed, padded, short, oversized, duplicate, and excessive tokens fail;
- non-loopback bind fails;
- invalid or ambiguous paths fail;
- bind collisions fail;
- token and MTProxy secret literals do not appear in debug output;
- token comparison evaluates all configured slots;
- duplicate Authorization values fail;
- all pre-upgrade failures have the same internal response shape.

#### WebSocket stream adapter

- handshake split across 1, 2, and many binary messages;
- handshake plus packet in one message;
- arbitrary read sizes preserve all bytes and order;
- arbitrary server writes are delivered as ordered binary bytes;
- text message fails closed;
- 16 MiB boundary succeeds and over-boundary fails;
- compression extension is absent;
- exact subprotocol is selected;
- peer close unblocks reads and writes;
- cancellation during blocked read/write returns promptly;
- repeated sessions pass leak/race checks.

#### Relay core

- nil-secret `0xdddddddd` handshake succeeds;
- nil-secret `0xefefefef` fails in version 1;
- secret-derived legacy handshake behavior remains unchanged;
- known positive and media/negative DC tags route correctly;
- unknown/test/arbitrary tags never dial;
- no WSS path enters FakeTLS, fronting, replay-cache, TLS record, or
  Doppelganger code;
- traffic and connection events retain ordering;
- handshake timeout, Telegram dial failure, idle timeout, and shutdown close
  both sides.

### 10.2 Nginx integration tests

Run Nginx with a local test CA/certificate, a real decoy fixture, and the WSS
backend on loopback.

Verify:

- ordinary HTTPS route succeeds;
- valid WSS request receives `101` and the exact subprotocol;
- wrong path is ordinary site content;
- correct path with wrong/missing token is ordinary site content;
- wrong method and subprotocol are ordinary site content;
- unavailable/overloaded backend is ordinary site content;
- inbound spoofed `X-Real-IP` is overwritten;
- the real client IP reaches allow/block policy;
- `Authorization` is absent from Nginx and `mtg` logs;
- message fragmentation does not alter stream bytes;
- sustained bidirectional transfer stays ordered and bounded;
- Nginx timeout is not shorter than the `mtg` active idle policy.

### 10.3 Repository regression checks

Run at each relevant commit:

```bash
go test ./...
go test -race ./mtglib/... ./internal/config/... ./internal/cli/...
golangci-lint run
docker build -t mtg-wss-plan-validation .
```

Use the repository's exact lint/build workflow if it differs at implementation
time. Existing FakeTLS, domain-fronting, network, events, and stats tests must
remain green.

### 10.4 Client/server integration order

1. Build a Go fixture first to validate the server independently.
2. Update the Telegram-iOS plan and implementation so only WSS forces padded
   intermediate while retaining a nil MTProxy secret.
3. Test the client on an unrestricted network.
4. Test invalid certificate, hostname, redirect, path, token, and subprotocol.
5. Test large upload/download, reconnect, app background/foreground, and
   Wi-Fi/cellular transitions.
6. Repeat on the restricted Wi-Fi and cellular paths.
7. Capture near the server and confirm:
   - a genuine complete TLS handshake;
   - valid SNI and certificate behavior;
   - an HTTP `101` WebSocket upgrade;
   - no fake ServerHello/CCS sequence;
   - sustained encrypted application traffic;
   - successful Telegram relay events.

## 11. Rollout and rollback

### 11.1 Staged rollout

1. Deploy the fork with WSS disabled and confirm no legacy regression.
2. Enable the loopback listener with Nginx routing unavailable publicly; test
   locally.
3. Add the exact Nginx location and decoy interception.
4. Test with a fixture token and rotate it out.
5. Enable the custom Telegram-iOS client for a small research cohort.
6. Compare connection success, setup latency, session lifetime, byte counts,
   and errors on unrestricted and restricted paths.
7. Expand only after repeated-device testing.

### 11.2 Rollback

- Disable or remove the Nginx relay location.
- Disable `[websocket-relay]` and restart `mtg`.
- Existing FakeTLS service remains operational throughout.
- Do not make the client silently fall back from a configured WSS endpoint to
  FakeTLS; endpoint selection remains explicit and deterministic.

## 12. Non-goals for version 1

- TLS termination or ACME inside `mtg`;
- browser client support;
- HTTP/2 extended CONNECT;
- QUIC, HTTP/3, MASQUE, or WebTransport;
- stock Telegram client support for the WSS endpoint;
- arbitrary TCP/UDP tunneling;
- calls or VoIP proxying;
- multiple MTProto streams per WebSocket;
- stream resumption after reconnect;
- WebSocket per-message compression;
- custom destination negotiation;
- automatic WSS-to-FakeTLS fallback;
- ClientHello profile rotation or fragmentation work;
- accepting abridged framing or adding a framing translator.

## 13. Deliverables

The server implementation task should produce:

1. a transport-neutral authenticated relay seam in `mtglib`;
2. an authenticated loopback WebSocket ingress;
3. secretless padded-intermediate Obfuscated2 support through that ingress;
4. strict known-Telegram-DC routing;
5. bounded configuration, token rotation, and credential redaction;
6. clean startup and shutdown integration;
7. unit, race, integration, and regression coverage;
8. a tested Nginx reference deployment;
9. operational token-generation, rotation, rollout, and rollback guidance;
10. a corrected companion Telegram-iOS plan.

## 14. Success criteria

The work is complete when all of the following are true:

1. Existing raw `ee` MTProxy behavior is unchanged and its test suite passes.
2. A valid client reaches a requested known Telegram DC through genuine TLS and
   WebSocket using a secretless padded-intermediate header.
3. Unknown DCs and malformed handshakes cannot produce an arbitrary dial.
4. Invalid public probes and backend failures receive the configured genuine
   website response from Nginx.
5. Bearer tokens and the existing MTProxy secret do not appear in URLs, logs,
   metrics, events, errors, or diagnostic configuration output.
6. Stream ordering, fragmentation independence, message bounds, backpressure,
   quotas, timeouts, close propagation, and shutdown are covered by tests.
7. Repository tests, focused race tests, lint, and Docker build pass.
8. Physical-device trials repeatedly work on the affected restricted networks
   and continue to work on unrestricted controls.

## 15. Implementation guardrails

- Keep FakeTLS and WSS ingress branches explicit; do not infer one from malformed
  traffic intended for the other.
- Authenticate before upgrade and route only after parsing the standard DC tag.
- Prefer extraction and reuse over copying `Proxy` internals.
- Keep credentials out of generic `String()` and structured log fields by
  construction.
- Keep limits fixed or tightly bounded; avoid broad tuning surfaces in v1.
- Follow existing Go conventions, dependency injection, event ordering, and
  shutdown structure.
- Implement and review one commit boundary at a time.
