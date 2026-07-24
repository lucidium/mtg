# Genuine TLS/WebSocket Relay — Telegram-iOS Implementation Handoff

**Audience:** LLM/developer implementing the client in the Telegram-iOS fork  
**Server status:** complete, fixture-tested, and deployable  
**Server repository:** `~/git/mtg`  
**Detailed client plan:**
`~/git/Telegram-iOS/docs/custom/genuine-tls-websocket-relay-client-plan.md`

This document is the concise server-to-client contract. Treat the protocol
details below as frozen for version 1. The existing Telegram-iOS client plan
contains the proposed commit sequence and file-by-file client changes; this
handoff records what the implemented server actually requires.

## 1. What the server implements

The public connection path is:

```text
Telegram-iOS
    -> wss://<public-host>:443/<hidden-path>
    -> genuine system TLS terminated by Nginx
    -> authenticated HTTP/1.1 WebSocket upgrade
    -> loopback-only mtg WebSocket ingress
    -> secretless Obfuscated2 + padded-intermediate MTProto stream
    -> a known Telegram datacenter selected by the Obfuscated2 header
```

This is a second, explicit ingress into `mtg`. It is not FakeTLS and is not a
generic HTTP CONNECT/TCP tunnel. The existing SOCKS5 and MTProxy transports,
including `ee` FakeTLS, remain independent and unchanged.

The reason for this design is to put a genuine Apple TLS handshake, valid
certificate, and standards-compliant WebSocket exchange on the restricted
network path. A WSS connection must never execute Telegram-iOS's synthetic
ClientHello, fake ServerHello/CCS handling, TLS-record wrapping, or FakeTLS
secret derivation.

## 2. Endpoint data supplied to the client

One saved WSS relay entry contains four values:

```text
host:  relay.example.com
port:  443
path:  /relay/v1/<deployment-specific-random-component>
token: <unpadded-base64url bearer token>
```

The operator will provide the real values separately. Do not hard-code them,
commit them, or request server configuration/private keys.

Rules:

- `host` is a DNS hostname, not an IP literal. It is used for DNS resolution,
  SNI, and normal certificate hostname validation.
- Only `wss://` is supported.
- `port` is normally 443 and must be in `1...65535`.
- `path` is an exact, absolute, canonical path. It has no query or fragment.
- The deployed path is expected to use conservative ASCII such as
  `/relay/v1/<hex>`.
- Do not normalize, append a slash to, or otherwise rewrite the path.
- The token is the credential. The randomized path is only defense in depth.
- A valid token is canonical **unpadded base64url** decoding to 32–64 random
  bytes. The server may accept up to four tokens concurrently during rotation;
  one client entry uses one token.
- The WSS token is completely independent of the legacy MTProxy `ee` secret.

The network URL contains only host, port, and path:

```text
wss://relay.example.com:443/relay/v1/...
```

The token must never be placed in that URL, a query, cookie, WebSocket
subprotocol, DNS name, log field, object description, metric, or error string.

## 3. Required HTTP/WebSocket upgrade

The client must make a normal RFC 6455 WebSocket request over genuine TLS.
Conceptually, the relevant request fields are:

```http
GET /relay/v1/<random-component> HTTP/1.1
Host: relay.example.com
Authorization: Bearer <token>
Upgrade: websocket
Connection: Upgrade
Sec-WebSocket-Version: 13
Sec-WebSocket-Key: <standard random key>
Sec-WebSocket-Protocol: telegram-mtproto-relay.v1
```

Use `URLSessionWebSocketTask` with a `URLRequest` so the Authorization field can
be attached while retaining Apple's real TLS implementation.

### Mandatory client behavior

- Send exactly one `Authorization` field in the `Bearer <token>` form.
- Offer the exact subprotocol `telegram-mtproto-relay.v1`.
- Require the server to select/echo exactly that subprotocol.
- Do not send an `Origin` field. The server rejects every request containing
  any Origin value; browser clients are intentionally unsupported.
- Do not request or depend on WebSocket per-message compression. The server
  disables it and returns no `Sec-WebSocket-Extensions` negotiation.
- Reject every HTTP redirect, including same-origin redirects. Never forward
  Authorization to a redirect target.
- Use normal system certificate-chain and hostname validation. Do not add a
  custom CA, permissive trust callback, certificate pin, alternate SNI host, or
  TLS fingerprint generator.
- Treat every non-`101` result as a generic connection failure. Do not parse a
  response body for protocol diagnostics.

Nginx overwrites `X-Real-IP` with the real socket peer before forwarding the
request to `mtg`. The iOS client neither supplies nor trusts this header.

### Why failures may look like an ordinary website

Before upgrade, `mtg` uses one generic internal rejection for a wrong path,
query string, method, token, subprotocol, client address, policy decision, or
connection-cap condition. Nginx intercepts those responses and backend
availability errors and serves the genuine ordinary website instead.

Consequently, a bad token or path can appear to the client as `200 OK` HTML
rather than `401`, `403`, or a Telegram-specific error. This is intentional.
Only a successful `101` plus the exact selected subprotocol establishes the
transport.

## 4. Frozen inner transport contract

The WebSocket application data is:

> One ordered, secretless standard Obfuscated2 byte stream using
> padded-intermediate framing (`0xdddddddd`).

This is the most important compatibility rule. Telegram-iOS's ordinary
secretless default is abridged (`0xefefefef`); that default is **not accepted**
by this server. The explicit WSS branch must force padded-intermediate framing
while keeping the MTProxy secret nil.

In the existing `MTTcpConnection` implementation, the intended WSS state is:

```text
_mtpSecret = nil
_useIntermediateFormat = true
```

Set this combination only in the new explicit WSS branch. Do not change how
those fields are selected for direct, SOCKS5, 16-byte MTProxy, `dd`, or `ee`
connections.

### 4.1 First 64 upstream bytes

Immediately after the WebSocket opens, send the existing standard 64-byte
Obfuscated2 header. There is no preceding custom relay handshake or
destination message.

The canonical plaintext header layout is:

| Offset | Length | Meaning |
| ---: | ---: | --- |
| 0 | 8 | standard random/non-forbidden prefix bytes |
| 8 | 32 | client-to-server AES key |
| 40 | 16 | client-to-server AES IV |
| 56 | 4 | `dd dd dd dd` padded-intermediate connection type |
| 60 | 2 | signed Telegram DC tag, little-endian `int16` |
| 62 | 2 | random bytes |

Reuse Telegram-iOS's existing Obfuscated2 header generator and forbidden-prefix
checks. Do not invent a second implementation if the current code can produce
this exact header by selecting secretless padded-intermediate mode.

Key derivation is the standard **secretless** algorithm:

- upstream AES-CTR key/IV come directly from plaintext bytes 8–39 and 40–55;
- downstream key/IV come from the standard reversed bytes 8–55;
- do not hash either key with the WSS bearer token;
- do not use the saved `ee` secret;
- encrypt/consume the full 64-byte header in the normal Obfuscated2 cipher
  state, while transmitting the standard unencrypted key/IV window as the
  existing implementation already does;
- subsequent application encryption continues from the AES-CTR position after
  those 64 bytes.

Send the header promptly after the valid WebSocket open callback. The deployed
server's default Obfuscated2 handshake timeout is 10 seconds after upgrade.

### 4.2 Datacenter selection

The standard DC field at plaintext offsets 60–61 is the only destination
selector. Preserve Telegram-iOS's current signed little-endian DC behavior,
including the established media-DC convention.

The server resolves only DC identifiers present in its Telegram address map.
An unknown, malformed, or test-only DC produces no network dial and the WSS
session closes. There is no hostname, IP, port, DNS name, URL, or CONNECT target
inside the WSS application protocol.

### 4.3 Packet framing after the header

After the header, use Telegram-iOS's existing padded-intermediate packet codec:

- four-byte little-endian frame length;
- existing MTProto packet bytes;
- existing bounded random padding behavior;
- the same continuous Obfuscated2 AES-CTR streams initialized by the header.

The `mtg` server decrypts this stream, opens its own independent Obfuscated2
connection to the selected Telegram DC, and relays the padded-intermediate
framing without translating it. Abridged framing cannot be made compatible by
changing only the header marker.

## 5. WebSocket messages must behave like a byte stream

WebSocket message boundaries have no application meaning.

- Concatenating all outbound binary messages must reproduce exactly the one
  client-to-server Obfuscated2 byte stream.
- Concatenating all inbound binary messages must reproduce exactly the one
  server-to-client Obfuscated2 byte stream.
- The 64-byte header may be split across several binary messages.
- A message may contain the header followed by encrypted packet bytes.
- One exact-length MTProto read may span several messages.
- One message may satisfy several exact-length reads.
- No byte may be dropped, duplicated, reordered, or decrypted twice.
- Text messages are protocol failures.
- One WSS connection maps to one `MTTcpConnection`; there is no multiplexing.

The implemented server uses a stream adapter over binary messages and has
integration coverage for all of the fragmentation cases above.

### Recommended `MTTcpConnectionInterface` adapter design

Implement the new WSS backend as a message-to-stream adapter with:

- one serial state queue;
- a FIFO outbound queue;
- at most one in-flight WebSocket send;
- `.data` messages only;
- a continuously re-armed receive call;
- one bounded inbound byte buffer;
- a FIFO queue of exact-length read requests;
- idempotent cancellation and one disconnect callback;
- generation/state guards that ignore late URLSession callbacks;
- no delegate callbacks after `resetDelegate`.

Do not declare the connection ready to `MTTcpConnection` until the genuine TLS
and WebSocket upgrade has succeeded and the exact subprotocol is selected.

## 6. Bounds, timeouts, backpressure, and closure

Deployed defaults are:

```text
maximum WebSocket message size: 16 MiB per message
maximum active WSS sessions:    2048
Obfuscated2 handshake timeout:  10 seconds
active relay idle timeout:      5 minutes
Nginx tunnel timeouts:          6 minutes
```

Client requirements:

- Set `URLSessionWebSocketTask.maximumMessageSize` to 16 MiB.
- Never send one binary message larger than 16 MiB. Smaller messages are
  preferred; splitting a large stream write is valid because boundaries are
  irrelevant.
- Bound aggregate unread input as well as individual received messages.
- Serialize sends instead of creating unbounded concurrent tasks.
- Let the queues and URLSession apply backpressure; do not create a goroutine/
  task per packet without a bound.
- Do not rely on WebSocket ping frames to keep the MTProto relay alive. The
  server's idle timer observes relayed application-stream activity, and version
  1 defines no independent ping policy.
- Let the existing MTProto connection machinery own retry/reconnect decisions.

Closing either endpoint closes the other endpoint and its Telegram TCP
connection. Server shutdown attempts WebSocket status 1001. Text input is
closed with 1003 and an oversized message with 1009. Many other post-upgrade
failures intentionally close generically, often with no useful reason.

Do not build client logic around close-code details or peer-provided reason
strings. Treat any close/read/write failure as one terminal disconnect, clean
up all pending operations exactly once, and allow the existing connection
manager to reconnect.

There is no stream resumption. A reconnect starts a new TLS connection, a new
WebSocket upgrade, a fresh random 64-byte Obfuscated2 header, new AES streams,
and a new Telegram TCP connection.

## 7. Required Telegram-iOS transport separation

Add WSS as a new explicit persisted proxy type. Do not infer it from malformed
MTProxy settings and do not overload the existing secret field.

The branch matrix must remain explicit:

```text
direct                -> existing direct TCP
SOCKS5                -> existing SOCKS5 path
16-byte MTProxy       -> existing obfuscated MTProxy path
dd MTProxy            -> existing intermediate/padded MTProxy path
ee MTProxy            -> existing FakeTLS path
WSS relay             -> URLSessionWebSocketTask + secretless padded-intermediate Obfuscated2
```

For WSS specifically:

- preserve the DNS hostname all the way to URLSession; do not resolve it to an
  IP through `MTDNS` before connection;
- bypass SOCKS5 negotiation;
- bypass all FakeTLS ClientHello/ServerHello/CCS and TLS-record code;
- keep the WSS bearer token out of `_mtpSecret`;
- use a separate optional WSS interface factory rather than changing the
  existing raw-TCP factory contract;
- copy that factory into temporary `MTContext` instances used by proxy status
  checks and backup-address logic;
- fail closed if the WSS factory is unavailable;
- never fall back from WSS to FakeTLS, raw MTProxy, SOCKS5, or direct TCP;
- make endpoint identity include host, port, path, and token so any edit causes
  the normal connection reset, without logging the token.

The existing Telegram-iOS client plan proposes the relevant implementation
files and a three-commit split (model/bridge, transport, UI/links). Re-run code
discovery against the current Telegram-iOS revision before editing because
factory copy sites and exhaustive Swift switches can move between releases.

## 8. Authentication and credential handling

The bearer token authenticates only the HTTP WebSocket upgrade. It does not
participate in MTProto, Obfuscated2, AES, DC routing, or WebSocket payloads.

Requirements:

- store it in the explicit WSS settings model;
- mask it in the editor and previews;
- redact it in `description`, logs, assertions, errors, crash breadcrumbs,
  metrics, network diagnostics, and test snapshots;
- disable URL/cache/cookie/credential persistence in the ephemeral URLSession
  where the APIs permit;
- never include the full request headers in logs;
- treat custom configuration links and QR codes as credential-bearing;
- ensure equality/reset logic compares the token without exposing it;
- do not derive one credential from the legacy MTProxy secret or vice versa.

The server stores only SHA-256 token digests at runtime and compares all active
slots in constant time. Token rotation is transparent to the protocol: old and
new tokens may both work during migration, and removing an old token affects
only new upgrades, not sessions already established.

## 9. Client failure expectations

Fail closed for all of these cases:

- invalid/expired/untrusted certificate;
- certificate hostname mismatch;
- any HTTP redirect;
- wrong path or a path containing a query string;
- missing, malformed, duplicated, or wrong Authorization value;
- missing/wrong selected WebSocket subprotocol;
- any present Origin rejected by the server;
- text application message;
- message or aggregate-buffer limit violation;
- malformed/abridged/secret-derived Obfuscated2 header;
- unknown Telegram DC;
- handshake timeout, relay idle timeout, capacity rejection, backend outage,
  server shutdown, or Telegram-side close.

Do not distinguish authentication failure from a normal website response in
the UI. A generic proxy-unavailable state is appropriate and preserves the
server deployment's probe-resistance objective.

## 10. Minimum test matrix for the client patch

### Model and URL construction

- Existing SOCKS5 and all MTProxy settings decode and behave unchanged.
- WSS settings round-trip host, port, exact path, and token.
- Invalid host, port, path, query/fragment, CR/LF, base64url, token size, and
  excessive lengths are rejected before transport creation.
- The actual `wss://` URL never contains the token.
- Configuration-link percent encoding decodes to the exact canonical network
  path; the final WSS request does not send percent-encoded separator tricks.
- Descriptions and diagnostics never contain the literal token.

### Message-to-stream adapter

- One message satisfies multiple exact reads.
- One exact read spans multiple messages.
- A 64-byte header split at arbitrary boundaries is ordered correctly.
- Header plus first packet in one message is ordered correctly.
- Concurrent writes leave the adapter in FIFO order with one send in flight.
- Text input and oversized input fail closed.
- Cancellation, send failure, receive failure, and peer close notify once.
- Late completions and callbacks after delegate reset are ignored.
- Repeated connect/disconnect does not leak tasks, sessions, buffers, or
  delegates.

### Live server integration

- Valid endpoint obtains genuine TLS, `101`, exact subprotocol, and MTProto
  connectivity.
- The first canonical plaintext header uses `0xdddddddd`, nil MTProxy secret,
  and the requested DC tag.
- Four-byte padded-intermediate frames work in both directions.
- Wrong token/path/subprotocol and a query-bearing path fail before upgrade.
- Redirect, bad certificate, and hostname mismatch fail.
- Large upload/download, slow consumer, and reconnect remain ordered and
  bounded.
- Client close, server close, 5-minute application idle, and server restart all
  return through the normal Telegram connection recovery path.
- Background/foreground and Wi-Fi/cellular transitions are tested.
- Trials succeed repeatedly on an unrestricted control network and on the
  affected restricted Wi-Fi/cellular paths.

### Regression matrix

Test direct, SOCKS5, 16-byte MTProxy, `dd`, `ee`, and WSS independently. Test
saved-proxy status checks for every type. WSS behavior must not depend on the
debug setting that chooses the ordinary raw-TCP backend.

## 11. Version 1 non-goals

Do not add any of the following while implementing this contract:

- browser support;
- HTTP/2 extended CONNECT;
- QUIC, HTTP/3, MASQUE, WebTransport, or NaiveProxy compatibility;
- fake-TLS fingerprint or fragmentation changes;
- domain fronting or separate connect/SNI hosts;
- custom CA roots or certificate pinning;
- WebSocket compression;
- application-level destination negotiation;
- arbitrary TCP/UDP tunneling;
- multiplexing or stream resumption;
- WSS-to-FakeTLS automatic fallback;
- Telegram call/VoIP proxying;
- abridged `0xefefefef` WSS support;
- a client- or server-side framing translator.

## 12. Implemented server references

Use these only if a detail must be verified:

- `mtglib/websocket_relay.go` — HTTP validation, token auth, subprotocol,
  binary stream adapter, limits, and close handling.
- `mtglib/proxy.go` — explicit WSS ingress policy, nil-secret Obfuscated2,
  strict DC routing, Telegram dial, relay, deadlines, and shutdown.
- `mtglib/internal/obfuscation/` — exact existing 64-byte header and AES-CTR
  algorithm used by the server fixtures.
- `mtglib/websocket_relay_test.go` — upgrade, auth, fragmentation, text/size,
  capacity, and shutdown behavior.
- `mtglib/websocket_relay_integration_test.go` — complete secretless
  padded-intermediate fixture, per-message bounds, timeout, slow-consumer,
  rotation, and lifecycle coverage.
- `docs/genuine-tls-websocket-relay-server-handoff.md` — completed server work
  and validation record.
- `deployments/genuine-tls-websocket-relay/` — production Compose/Nginx shape.

## 13. Completion definition

The Telegram-iOS modification is compatible when a selected WSS entry:

1. performs a genuine Apple TLS handshake with normal public trust;
2. upgrades the exact path using bearer auth and the required subprotocol;
3. sends no Origin and accepts no redirect or compression dependency;
4. starts one secretless padded-intermediate Obfuscated2 stream with a fresh
   standard 64-byte header;
5. preserves every byte across arbitrary WebSocket message boundaries;
6. reaches only the known Telegram DC encoded in that header;
7. closes and reconnects through existing MTProto lifecycle machinery;
8. exposes no credential in URLs, logs, diagnostics, or UI previews; and
9. leaves every pre-existing connection mode behavior unchanged.