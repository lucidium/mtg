# Genuine TLS/WebSocket Relay — Nginx Deployment

This guide deploys the optional authenticated WebSocket ingress behind Nginx.
Nginx owns genuine public TLS and the ordinary website; `mtg` accepts plaintext
HTTP only on loopback. The existing public FakeTLS listener is independent and
may remain enabled.

The tested reference is
[`nginx-websocket-relay.conf`](nginx-websocket-relay.conf). It is intentionally
not a drop-in file: every `__PLACEHOLDER__` must be replaced and the ordinary
site blocks must be merged with the deployment's real configuration.

## Security model

- Only the exact configured relay path reaches the `mtg` WebSocket listener.
- The bearer token is sent only in `Authorization: Bearer <token>` and never in
  a URL, path, cookie, Nginx variable name, or log field.
- Nginx overwrites `X-Real-IP` with its direct client address. `mtg` trusts that
  header only because the immediate HTTP peer and listener are loopback.
- Nginx converts all backend pre-upgrade rejections and backend availability
  failures into the ordinary site's response.
- After a successful `101 Switching Protocols`, WebSocket close behavior is
  visible to the authenticated client and cannot be replaced by decoy content.
- TLS certificates and private keys remain outside `mtg`.

The randomized path is defense in depth, not a credential. The bearer token is
the credential. Do not derive it from or reuse the legacy MTProxy secret.

## 1. Generate deployment values

Choose a DNS name whose certificate is valid and whose unrelated paths serve a
real site. Generate the relay token with a cryptographically secure command:

```console
openssl rand -base64 32 | tr '+/' '-_' | tr -d '='
```

Generate the deployment-specific path component separately. This value is not
the bearer credential, but it should still be unguessable enough to reduce
unsolicited probes:

```console
openssl rand -hex 24
```

The resulting endpoint has this shape:

```text
host:  relay.example.com
port:  443
path:  /relay/v1/<deployment-specific-random-path>
token: <current-unpadded-base64url-token>
```

Avoid shell history, process arguments, configuration-management logs, and
world-readable files when transferring the token.

## 2. Configure `mtg`

Add the following to the normal `mtg` configuration, replacing the path and
token with the generated values:

```toml
[websocket-relay]
enabled = true
bind-to = "127.0.0.1:8081"
path = "/relay/v1/<deployment-specific-random-path>"
tokens = [
    "<current-unpadded-base64url-token>",
]
max-connections = 2048
max-message-size = "16MiB"
```

`mtg` rejects an enabled relay unless `bind-to` is an IPv4 or IPv6 loopback
address. Keep port `8081` unavailable outside the host as defense in depth:

- do not publish it from a container;
- bind it only to `127.0.0.1` or `[::1]`;
- do not add a firewall accept rule for it; and
- if host policy can address loopback traffic, allow only the local Nginx
  worker identity or local loopback source.

Do not place Nginx and `mtg` in separate network namespaces and then weaken the
bind to `0.0.0.0`: version 1 intentionally requires the immediate peer to be
loopback. Run them in the same host network namespace.

## 3. Merge the Nginx reference

The reference file belongs in Nginx's `http` context. Replace:

| Placeholder | Meaning | Example |
| --- | --- | --- |
| `__MTG_WEBSOCKET_RELAY_ADDRESS__` | Exact `mtg` loopback listener | `127.0.0.1:8081` |
| `__DECOY_UPSTREAM_ADDRESS__` | Genuine ordinary-site upstream | `127.0.0.1:8080` |
| `__RELAY_SERVER_NAME__` | Public DNS name | `relay.example.com` |
| `__RELAY_PATH__` | Exact `mtg` relay path | `/relay/v1/<random>` |
| `__TLS_CERTIFICATE_PATH__` | Public certificate/full chain | `/etc/letsencrypt/live/relay.example.com/fullchain.pem` |
| `__TLS_CERTIFICATE_KEY_PATH__` | Certificate private key | `/etc/letsencrypt/live/relay.example.com/privkey.pem` |
| `__ACCESS_LOG_PATH__` | Credential-safe site access log | `/var/log/nginx/relay.access.log` |
| `__ERROR_LOG_PATH__` | Site error log | `/var/log/nginx/relay.error.log` |

Keep these invariants when merging it into an existing virtual host:

1. `location = <relay-path>` remains an exact-match location.
2. The backend uses HTTP/1.1 with explicit `Upgrade` and `Connection` headers.
3. `Authorization` and `Sec-WebSocket-Protocol` reach `mtg` unchanged.
4. `X-Real-IP` is overwritten with `$remote_addr`; client-supplied forwarding
   chains are not appended or preserved.
5. Request and response buffering and caching are disabled for the tunnel.
6. The named decoy location clears authentication, body framing, and WebSocket
   headers, discards the probe body, and makes a `GET` for the genuine site's
   root.
7. `proxy_intercept_errors` is off in the named decoy location, avoiding an
   error-page loop if the real site itself returns an error.
8. No other location proxies to the `mtg` WebSocket upstream.
9. No log format includes `$request`, `$args`, `$http_authorization`, arbitrary
   request headers, or referrer fields. The reference logs only the normalized
   `$uri`, not a query string.

If the ordinary website is served from local static files instead of an
upstream, replace both decoy proxy blocks with an internal named location that
serves the same genuine root content. Do not internally redirect back through
the relay's exact-match location.

## 4. Order timeouts correctly

`[network.timeout].idle` is the `mtg` active-relay idle timeout and defaults to
5 minutes. The reference sets both Nginx tunnel timeouts to 6 minutes:

```nginx
proxy_read_timeout 6m;
proxy_send_timeout 6m;
```

Both Nginx values must remain greater than the configured `mtg` idle timeout so
`mtg`, not Nginx, owns idle-session termination. If `mtg` is configured with a
longer idle timeout, raise both proxy timeouts by a deliberate margin. Version
1 defines no independent application-level WebSocket ping policy.

Nginx's public keepalive timeout is unrelated to an upgraded tunnel. The
`mtg` HTTP server separately bounds non-upgraded requests with a 5-second
header timeout, 30-second idle timeout, and 16 KiB maximum header size.

## 5. Manage genuine TLS externally

Issue and renew the public certificate through the existing Nginx/ACME process.
The certificate must cover the configured DNS hostname and the client must
perform normal hostname and chain validation. Do not copy the certificate key
into the `mtg` configuration or terminate public TLS in the loopback listener.

After each automated renewal, test and reload Nginx using the service manager's
normal workflow. A typical non-containerized deployment uses:

```console
sudo nginx -t
sudo nginx -s reload
```

Use the equivalent zero-downtime validation/reload commands supplied by the
operating system or container platform when they differ.

## 6. Validate before exposing the route

First validate the merged configuration:

```console
sudo nginx -t
```

Then verify these behaviors from a machine that resolves the public hostname:

1. An ordinary HTTPS path returns the genuine site.
2. The relay path without a token returns the same class of genuine site
   response, not `mtg`'s internal rejection body.
3. The relay path with a wrong token, wrong subprotocol, query string, or wrong
   method also returns genuine site content.
4. Stopping `mtg` causes the relay path to return genuine site content rather
   than expose a gateway error.
5. A valid WebSocket request receives `101` and exactly
   `Sec-WebSocket-Protocol: telegram-mtproto-relay.v1`.
6. Spoofing inbound `X-Real-IP` does not change the address seen by `mtg`.
7. Access and error logs contain neither the token nor the `Authorization`
   value. Also inspect central log collectors and tracing middleware.

Do not put a real token directly in an interactive `curl` command: process
listings and shell history may retain it. Use a purpose-built client fixture or
a protected temporary header file and remove that file after testing.

Optional Nginx `limit_conn` and `limit_req` zones may be applied to the exact
relay location as defense in depth. Their rejection status must be included in
the intercepted status set so public probes still receive the decoy response.
The authoritative active-session bound remains `max-connections` in `mtg`.

## 7. Rotate a token without downtime

`mtg` accepts up to four tokens to support rotation:

1. Generate a new independent token using the secure command above.
2. Configure both old and new values:

   ```toml
   tokens = [
       "<old-token>",
       "<new-token>",
   ]
   ```

3. Restart `mtg` and verify a client configured with the new token.
4. Migrate all clients to the new token.
5. Allow existing sessions and delayed clients an appropriate migration
   window. Tokens are checked only during the HTTP upgrade; removing a token
   does not retroactively authenticate or terminate an established session.
6. Remove the old value, restart `mtg`, and verify that only the new token can
   establish a fresh session.

Never add either token to Nginx. Nginx forwards the request's Authorization
field but does not know or validate its value.

## Rollout and rollback

Recommended rollout:

1. Start the fork with `[websocket-relay]` disabled and verify the existing
   FakeTLS service.
2. Enable the loopback listener without adding the public exact-match route;
   confirm it is reachable only from the local host.
3. Merge and validate the Nginx route and decoy interception.
4. Test with a temporary fixture token, then rotate that token out.
5. Enable a small client cohort before broader rollout.

Rollback does not require changing the legacy listener:

1. Remove or disable the exact Nginx relay location and reload Nginx.
2. Set `[websocket-relay].enabled = false` and restart `mtg`.
3. Leave the existing public FakeTLS listener and ordinary site running.

Clients configured for WSS should fail deterministically during rollback; do
not silently reinterpret that endpoint as FakeTLS.