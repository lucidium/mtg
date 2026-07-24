# Docker Compose deployment — Ubuntu 24.04 amd64

This project runs the authenticated genuine-TLS/WebSocket relay as two
containers:

```text
Internet :<HTTP_PORT>/:<HTTPS_PORT>
        -> Nginx (genuine TLS + static disguise site)
        -> exact hidden path only
        -> http://127.0.0.1:8081
        -> mtg WebSocket relay
        -> Telegram DC
```

Both containers use the Linux host network namespace. This is deliberate:
version 1 of the relay requires both the `mtg` listener and its immediate Nginx
peer to be loopback. Nginx is the only service listening publicly. The Compose
project is therefore intended for a dedicated Linux VPS, not Docker Desktop,
Swarm, Kubernetes, or a shared container host.

Certificate issuance and renewal are external to this project. Existing host
certificate and key files are mounted read-only into Nginx.

## Included files

| Path | Purpose |
| --- | --- |
| `compose.yaml` | Hardened `mtg` and Nginx services |
| `.env.example` | Hostname, relay path, ports, certificate paths, UID/GID |
| `mtg.toml.example` | Minimal WSS-focused `mtg` configuration |
| `nginx/relay.conf.template` | TLS, WebSocket routing, and static decoy config |
| `site/` | Static disguise-site document root |
| `DISGUISE_SITE_HANDOFF.md` | Contract for a separate website developer/LLM |
| `state/mtg.toml` | Runtime secrets; created locally and ignored by Git |

## 1. Prerequisites

Before deployment, ensure:

1. The VPS runs Ubuntu 24.04 on amd64.
2. DNS `A` and, if used, `AAAA` records point the selected hostname to the VPS.
3. A valid full-chain certificate and matching private key already exist on
   the VPS and cover that exact hostname.
4. Three free host ports are available for Nginx's public HTTP, public HTTPS,
   and loopback health listeners.
5. The server can make outbound TCP connections to Telegram.

Check the architecture and currently occupied ports:

```console
dpkg --print-architecture
sudo ss -lntp
```

The first command must print `amd64`. Choose unused values for `HTTP_PORT`,
`HTTPS_PORT`, and `NGINX_HEALTH_PORT` in `.env`. The defaults are `80`, `443`,
and `8080`, but none is required. The configured public ports bind on all host
interfaces; the health port binds only to `127.0.0.1`. The `mtg` ports `8081`
and `3128` come from `state/mtg.toml`, are loopback-only in the template, and
must also remain free in the shared host network namespace.

## 2. Install Docker Engine and Compose

Use Docker's official Ubuntu repository:

```console
sudo apt-get update
sudo apt-get install -y ca-certificates curl git openssl
sudo install -m 0755 -d /etc/apt/keyrings
sudo curl -fsSL https://download.docker.com/linux/ubuntu/gpg \
  -o /etc/apt/keyrings/docker.asc
sudo chmod a+r /etc/apt/keyrings/docker.asc

echo \
  "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu \
  $(. /etc/os-release && echo \"${UBUNTU_CODENAME:-$VERSION_CODENAME}\") stable" \
  | sudo tee /etc/apt/sources.list.d/docker.list >/dev/null

sudo apt-get update
sudo apt-get install -y docker-ce docker-ce-cli containerd.io \
  docker-buildx-plugin docker-compose-plugin
sudo systemctl enable --now docker
sudo docker run --rm hello-world
sudo docker compose version
```

The commands below use `sudo docker` and do not add an administrator to the
privileged `docker` group.

## 3. Put the repository on the VPS

The `mtg` image is built from this fork so it contains the completed WebSocket
relay implementation. Clone the intended revision rather than substituting an
upstream image that may not contain it:

```console
sudo install -d -m 0755 -o "$USER" -g "$USER" /opt/mtg
git clone <YOUR_FORK_GIT_URL> /opt/mtg
cd /opt/mtg/deployments/genuine-tls-websocket-relay
```

For repeatable production deployment, check out a reviewed commit or signed
tag before building.

## 4. Create the environment file

```console
cp .env.example .env
chmod 0600 .env
```

Edit `.env` and set:

- `DOMAIN` to the public DNS hostname;
- `RELAY_PATH` to `/relay/v1/` followed by a random path component;
- `HTTP_PORT` to an unused public plaintext port (for example `18080`);
- `HTTPS_PORT` to the public genuine-TLS port clients will use (for example
  `18443`);
- `NGINX_HEALTH_PORT` to an unused loopback-only port (for example `18081`);
- `TLS_CERTIFICATE_PATH` to the absolute host full-chain file;
- `TLS_CERTIFICATE_KEY_PATH` to the absolute host private-key file.

With nonstandard ports, the public relay endpoint is
`wss://<DOMAIN>:<HTTPS_PORT><RELAY_PATH>`. The HTTP listener redirects ordinary
requests to that HTTPS port. If plaintext HTTP is not reachable from the
Internet, the WSS endpoint still works; only the redirect and any HTTP-based
certificate workflow are unavailable.

Generate a random path component:

```console
openssl rand -hex 24
```

The path is defense in depth and **not** the bearer credential. It must contain
no token, query string, fragment, percent encoding, whitespace, or trailing
slash. It will appear in both `.env` and `state/mtg.toml` and must match exactly.

Validate the existing certificate before continuing:

```console
sudo test -r /absolute/path/to/fullchain.pem
sudo test -r /absolute/path/to/privkey.pem
sudo openssl x509 -in /absolute/path/to/fullchain.pem \
  -noout -subject -issuer -dates -ext subjectAltName
```

The first certificate in `fullchain.pem` must be the server certificate,
followed by intermediate certificates. Never copy the private key into the
repository.

## 5. Create the runtime `mtg` configuration

Create a dedicated unprivileged identity used inside the scratch-based `mtg`
container:

```console
sudo useradd --system --user-group --no-create-home \
  --home-dir /nonexistent --shell /usr/sbin/nologin mtg-relay
id -u mtg-relay
id -g mtg-relay
```

Set those numeric values as `MTG_UID` and `MTG_GID` in `.env`. Then install the
configuration template with restrictive permissions:

```console
sudo install -d -m 0750 -o root -g mtg-relay state
sudo install -m 0640 -o root -g mtg-relay mtg.toml.example state/mtg.toml
```

Build the local relay image:

```console
sudo docker compose build --pull mtg
```

Generate an independent legacy FakeTLS secret. Although the legacy listener is
loopback-only in this deployment, `mtg` still requires a valid secret:

```console
sudo docker compose run --rm --no-deps mtg \
  generate-secret storage.googleapis.com --hex
```

Generate the WSS bearer token separately:

```console
openssl rand -base64 32 | tr '+/' '-_' | tr -d '=\n'; printf '\n'
```

Use `sudoedit state/mtg.toml` to replace all placeholders:

1. Put the generated `ee...` value in `secret`.
2. Copy the exact `.env` `RELAY_PATH` into `[websocket-relay].path`.
3. Put the unpadded base64url value in `[websocket-relay].tokens`.

Do not reuse one credential for the other. Do not put the bearer token in
`.env`, Nginx, a URL, a shell command argument, or the disguise website.

The template binds the independent legacy FakeTLS listener to
`127.0.0.1:3128`, so it is not publicly reachable. To retain a public legacy
listener, change that bind deliberately and open its chosen port separately;
this is not required for WSS operation.

## 6. Install the disguise website

The included placeholder lets the stack start immediately. For production,
replace the contents of `site/` with the website artifact described in
[`DISGUISE_SITE_HANDOFF.md`](DISGUISE_SITE_HANDOFF.md). `site/index.html` must
always exist.

The web root is mounted read-only. A static-site update normally becomes
visible immediately and does not require rebuilding either container.

## 7. Firewall the VPS

Allow the actual SSH port before enabling UFW. For a standard SSH service:

```console
sudo ufw allow OpenSSH
sudo ufw allow <HTTP_PORT>/tcp
sudo ufw allow <HTTPS_PORT>/tcp
sudo ufw enable
sudo ufw status verbose
```

Replace the angle-bracketed values with the numeric ports from `.env`. If the
plaintext redirect is intentionally unreachable, omit the `HTTP_PORT` rule. Do
not add public allow rules for `NGINX_HEALTH_PORT`, `8081`, or `3128`; those
listeners bind only to `127.0.0.1`. Verify that after startup. If SSH uses a
custom port, allow that port before enabling UFW instead of relying on the
`OpenSSH` profile.

## 8. Validate and start

Render and validate the Compose model:

```console
sudo docker compose config --quiet
```

Validate Nginx with the real environment, site, and certificate mounts without
starting the public listener:

```console
sudo docker compose run --rm --no-deps nginx nginx -t
```

Start the stack:

```console
sudo docker compose up -d
sudo docker compose ps
sudo docker compose logs --tail=100 mtg nginx
```

Confirm the trust boundary, substituting the values from `.env`:

```console
sudo ss -lntp
```

Expected bindings:

- `0.0.0.0:<HTTP_PORT>`, `[::]:<HTTP_PORT>`,
  `0.0.0.0:<HTTPS_PORT>`, and `[::]:<HTTPS_PORT>`: Nginx;
- `127.0.0.1:<NGINX_HEALTH_PORT>`: Nginx health endpoint;
- `127.0.0.1:8081`: `mtg` WebSocket HTTP ingress;
- `127.0.0.1:3128`: disabled-publicly legacy FakeTLS ingress.

## 9. Public validation

From another machine, verify the ordinary site. Include `:<HTTPS_PORT>` when
it is not `443`:

```console
curl --fail --show-error --silent --output /dev/null \
  --write-out '%{http_code}\n' https://relay.example.com:<HTTPS_PORT>/
```

It should return `200`. The relay path without valid WebSocket authentication
must return the same genuine entry page rather than `mtg`'s internal response:

```console
curl --show-error --silent --dump-header - \
  https://relay.example.com:<HTTPS_PORT>/relay/v1/<random-path> \
  --output /tmp/relay-decoy-response.html
```

Also test a wrong path and a query string. Inspect the body and principal
headers, not just the status. Do not put a real bearer token in `curl` command
history or a process argument.

Temporarily stopping only `mtg` must still produce the genuine site response at
the exact relay path:

```console
sudo docker compose stop mtg
# Repeat the unauthenticated remote request here.
sudo docker compose start mtg
```

A purpose-built relay client should then verify that a valid request receives:

```text
HTTP/1.1 101 Switching Protocols
Sec-WebSocket-Protocol: telegram-mtproto-relay.v1
```

and can relay a secretless padded-intermediate Obfuscated2 stream. Browser
WebSocket clients are not valid test clients: version 1 rejects every present
`Origin` and requires an `Authorization` header.

Finally, inspect logs and ensure neither the bearer token nor Authorization
value appears:

```console
sudo docker compose logs nginx mtg
```

The Nginx log format intentionally records `$uri` only, never `$request`,
`$args`, referrers, arbitrary request headers, or `$http_authorization`.

## Operations

### View status and logs

```console
sudo docker compose ps
sudo docker compose logs -f --tail=200 mtg nginx
```

### Update the relay build

Check out a reviewed revision, then rebuild and recreate only what changed:

```console
git fetch --all --tags
git checkout <REVIEWED_COMMIT_OR_TAG>
sudo docker compose build --pull mtg
sudo docker compose up -d
```

### Renew or replace the external certificate

Renew the host files using your existing certificate workflow. Because a
single-file bind mount may continue referencing an old inode when a renewal
tool replaces a symlink, validate and recreate Nginx rather than assuming a
signal alone is sufficient:

```console
sudo docker compose run --rm --no-deps nginx nginx -t
sudo docker compose up -d --force-recreate --no-deps nginx
```

Then verify the certificate served publicly:

```console
openssl s_client -connect relay.example.com:<HTTPS_PORT> \
  -servername relay.example.com </dev/null 2>/dev/null \
  | openssl x509 -noout -subject -issuer -dates
```

### Rotate the WSS bearer token without downtime

1. Generate a new token with the command in section 5.
2. Add it beside the old token in `state/mtg.toml` (maximum four tokens).
3. Recreate `mtg`:

   ```console
   sudo docker compose up -d --force-recreate --no-deps mtg
   ```

4. Verify and migrate clients to the new token.
5. After the migration window, remove the old token and recreate `mtg` again.

Tokens are checked only during WebSocket upgrade. Removing a token does not
terminate sessions that are already established.

### Update the disguise site

Replace files inside `site/` atomically while preserving `site/index.html`.
Do not add server-side code, a second web server, secrets, or a route whose
filesystem path equals the configured hidden relay path. Test the result at an
ordinary path and at the unauthenticated relay path.

### Stop or roll back WSS

Stop the whole project:

```console
sudo docker compose down
```

For a WSS-only rollback while keeping the genuine website, remove the exact
relay `location` from the Nginx template (or deploy a previously reviewed
template), validate it, and recreate Nginx. Then set
`[websocket-relay].enabled = false` and recreate `mtg`. Never silently reinterpret
a configured WSS endpoint as FakeTLS.

## Security checklist

- [ ] Only Nginx owns the public `HTTP_PORT` and `HTTPS_PORT` from `.env`.
- [ ] `NGINX_HEALTH_PORT` and `mtg` WebSocket ingress remain loopback-only.
- [ ] `mtg` WebSocket ingress remains `127.0.0.1:8081`.
- [ ] Nginx and `mtg` both use `network_mode: host`; do not weaken the relay
      bind to `0.0.0.0` to use a Docker bridge.
- [ ] The bearer token exists only in root/group-readable `state/mtg.toml` and
      client configuration.
- [ ] The hidden path and token are independent random values.
- [ ] Certificate and key mounts are read-only and the private key is not in
      Git.
- [ ] Nginx's exact relay location overwrites `X-Real-IP`, clears
      `X-Forwarded-For`, disables buffering, and intercepts backend errors.
- [ ] Nginx tunnel timeouts remain longer than `[network.timeout].idle`.
- [ ] Logs and any central collector omit Authorization, request headers,
      query strings, and referrers.
- [ ] The ordinary site is real, complete, and served for wrong paths,
      authentication failures, capacity failures, and backend outages.