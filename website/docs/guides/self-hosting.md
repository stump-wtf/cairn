---
title: Run your own cairn
sidebar_position: 9
---

# Run your own cairn

Cairn is a single Go binary, a Postgres database, and an S3-compatible bucket.
This guide takes you from nothing to a running instance with a verified
web-create → web-read → agent loop.

Every command here was run against a fresh stack while writing this guide, and
the outputs are the real ones. The one step that cannot be run for you is
registering an OIDC client in your identity provider, since that happens in
software this project doesn't ship — that section says exactly what cairn needs
and how to tell it worked.

## What you need

| | Requirement | Notes |
|---|---|---|
| **Database** | Postgres | The schema is plain SQL — 15 migrations as of this writing, no extensions, no version-specific syntax — so any supported Postgres works. Verified on **16**; that is what the compose path below brings. Cairn creates and migrates its own schema at startup; there is no separate migrate step. |
| **Object storage** | Any S3-compatible store | Artifact bodies are content-addressed (SHA-256) and stored in a bucket, not in Postgres (ADR-0008). Verified with MinIO; Garage and S3 itself work the same way — it is plain the S3 API. Cairn creates its bucket on connect. |
| **Identity provider** | Any OIDC provider | Needed for humans to sign in to the web UI. Without it, the deployment is bearer-token-only — no interactive login exists at all. See [Sign-in (OIDC)](#sign-in-oidc). |
| **TLS** | A reverse proxy | Cairn speaks plain HTTP and expects something in front terminating TLS. The compose path below brings Caddy, which obtains certificates automatically. |

There is no external secret manager, no message broker, and no sidecar. One
process, one database, one bucket.

## Which variable is which

Several names look alike but belong to different programs. The server,
`cairnd`, reads its settings from its own environment. The `cairn` CLI reads
different names, from the shell of whoever is running it. Setting a server
variable in your shell does nothing for the CLI, and the reverse is also true.

| Name | Read by | What it is |
|---|---|---|
| `CAIRN_API_TOKENS` | the server | The list of static bearer credentials the server accepts, `secret:actor[:role]`. See [First run: bootstrap a credential](#first-run-bootstrap-a-credential). |
| `CAIRN_TOKEN` | the `cairn` CLI | The one bearer token the CLI sends, the same as `--token`. The MCP client configs in [Connect your agent](./connect-your-agent.md) expand it from your shell too, but `/mcp` accepts only a personal access token or an OAuth token there, never a `CAIRN_API_TOKENS` secret. |
| `CAIRN_BASE_URL` | the server | The public origin the server builds short links and its OIDC redirect URI from. |
| `CAIRN_URL` | the `cairn` CLI | The server the CLI talks to, the same as `--url`. It defaults to the hosted service, so a self-hoster sets it. |
| `CAIRN_OUTBOUND_WEBHOOK_URLS` | the server | Where the server sends `artifact.created` events. |
| `CAIRN_OUTBOUND_WEBHOOK_SECRET` | the server | The secret the server signs those events with. A receiver checks signatures against the same value. |
| `CAIRN_API_TOKEN` (singular) | nothing | A common slip. You want `CAIRN_API_TOKENS` on the server, or `CAIRN_TOKEN` for the CLI. |

The pairs connect like this: one secret in the server's `CAIRN_API_TOKENS` is
the value a REST or CLI client puts in `CAIRN_TOKEN`, and the server's
`CAIRN_BASE_URL` is the address a client puts in `CAIRN_URL`. The MCP endpoint
is the exception: it rejects `CAIRN_API_TOKENS` secrets with `401`.

## Configuration

Everything comes from the environment; `cairnd` takes no flags. Every variable
is read from the `CAIRN_` namespace and nothing is ever loaded from a file.

| Variable | Default | Purpose |
|---|---|---|
| `CAIRN_HTTP_ADDR` | `:8080` | Listen address. The container image defaults to this too. |
| `CAIRN_DATABASE_URL` | `$DATABASE_URL` | **Required.** Postgres DSN. |
| `CAIRN_BASE_URL` | `https://cairn.stump.wtf` | **Set this.** The public origin used to build short URLs — and the OAuth redirect URI is *always* derived as `<base>/auth/callback`, so it must match the origin your users actually reach. |
| `CAIRN_S3_ENDPOINT` | `localhost:9000` | S3-compatible endpoint (host:port, no scheme). |
| `CAIRN_S3_ACCESS_KEY` / `CAIRN_S3_SECRET_KEY` | `minioadmin` / `minioadmin` | **Do not ship the defaults on a public host.** |
| `CAIRN_S3_BUCKET` | `cairn` | Bucket name; created on connect if missing. |
| `CAIRN_S3_REGION` | `us-east-1` | Region string most S3 implementations accept. |
| `CAIRN_S3_USE_SSL` | `false` | Set `true` when the endpoint speaks HTTPS. |
| `CAIRN_API_TOKENS` | *(empty)* | Static bearer credentials for headless agents, comma-separated `secret:actor[:role]`. **Empty means the bearer surface accepts no tokens** — it fails closed. Real per-agent tokens come from OAuth or a personal access token minted in Settings. Wired through the compose file above; on the Docker path with no OIDC provider yet this is the only way to get a working credential. See [First run: bootstrap a credential](#first-run-bootstrap-a-credential). |
| `CAIRN_OIDC_ISSUER` | *(empty)* | Issuer URL of your OIDC provider. **Its presence is the switch that turns OIDC on** and, just as importantly, turns the dev-password login off (below). |
| `CAIRN_OIDC_CLIENT_ID` | `cairn` | Client id registered at your provider. |
| `CAIRN_OIDC_CLIENT_SECRET` | *(empty)* | Client secret. The redirect URI is not configurable — it is always `<base>/auth/callback`. |
| `CAIRN_DEV_LOGIN_PASSWORD` | *(empty)* | Shared-secret web login accepted for any actor id. Honored **only while `CAIRN_OIDC_ISSUER` is unset** — a deployment that configures OIDC can never fall back to it. Empty disables interactive login entirely. Development seam: deliberately **not** wired through the compose file above. |
| `CAIRN_DEV_INSECURE_BEARER_AUTH` | `false` | Makes the API trust any bearer token as its own actor id with no verification. A local-development shortcut that must never be enabled in production. Development seam: deliberately **not** wired through the compose file above. |
| `CAIRN_OUTBOUND_WEBHOOK_URLS` | *(empty)* | Comma-separated URLs that receive a signed `artifact.created` event. Empty = the feature is inert. These URLs are bearer capabilities; never log or share them. |
| `CAIRN_OUTBOUND_WEBHOOK_SECRET` | *(empty)* | When set, every delivery carries `X-Cairn-Signature: sha256=<hex>` over the raw body. |
| `CAIRN_DEFAULT_TTL` | `168h` | Default artifact expiry (Go duration; 7 days). |
| `CAIRN_MAX_UPLOAD_BYTES` | `67108864` | Max upload size, enforced incrementally — an oversize upload is rejected mid-stream with 413, not after buffering. |
| `CAIRN_PREVIEW_MAX_BYTES` | `5242880` | Bodies above this skip the rich viewer and use the generic file path. |
| `CAIRN_SESSION_TTL` | `168h` | Web session and cookie lifetime. |
| `CAIRN_RATE_PER_SECOND` / `CAIRN_RATE_BURST` | `20` / `40` | Per-IP rate limit on public/ingress endpoints. |
| `CAIRN_OAUTH_ACCESS_TTL` | `1h` | OAuth access-token lifetime. |
| `CAIRN_OAUTH_REFRESH_TTL` | `720h` | Rotating refresh-token lifetime (30 days). |
| `CAIRN_OAUTH_RATE_PER_SECOND` / `CAIRN_OAUTH_RATE_BURST` | `10` / `30` | Dedicated per-IP limit on the OAuth bootstrap endpoints. |
| `CAIRN_HOOK_INGRESS_RATE_PER_SECOND` / `CAIRN_HOOK_INGRESS_RATE_BURST` | `5` / `20` | Per-source-IP limit on the anonymous-write `ANY /h/{id}` webhook route — deliberately separate from, and tighter than, the general budget. |
| `CAIRN_HOOK_ENDPOINT_RATE_PER_SECOND` / `CAIRN_HOOK_ENDPOINT_RATE_BURST` | `10` / `50` | Per-endpoint limit on the same route. |
| `CAIRN_REAP_INTERVAL` / `CAIRN_REAP_BATCH` / `CAIRN_REAP_OBJECT_GRACE` | `1h` / `500` / `1h` | Background expiry-reaper tuning. |
| `CAIRN_STAGING_LIFECYCLE_TTL` | `168h` | S3 lifecycle expiration applied to the `staging/` prefix only, so crashed-upload debris is reaped. Never applied to committed blobs. |

Three settings are easy to get wrong:

- **`CAIRN_BASE_URL` decides whether login works at all.** The OIDC redirect
  URI is derived from it, never configured separately, so it can never drift
  from the origin your provider's client was registered against — but that
  also means a wrong base URL points your login flow at the wrong origin.
- **`CAIRN_API_TOKENS` fails closed.** Unset, nothing can call the API with a
  bearer token until a human mints a personal access token or an agent
  completes OAuth. That is the safe direction.
- **`CAIRN_DEV_LOGIN_PASSWORD` and `CAIRN_DEV_INSECURE_BEARER_AUTH` are
  development seams.** The dev password loses to OIDC the moment
  `CAIRN_OIDC_ISSUER` is set; the insecure bearer shortcut defaults off and
  has no production reason to exist. Leave both alone on a real deployment.

## Get the image

```bash
docker pull ghcr.io/stump-wtf/cairn:latest
```

The image is multi-arch (`linux/amd64`, `linux/arm64`), built and Trivy-scanned
by the release workflow on every `v*` tag. If the pull is denied, the GHCR
package is still marked private — package visibility is a one-time flip in the
GitHub package settings, not something code fixes. There is also a
build-from-source path below.

## Start it

There are two paths. Docker is the shorter one; the binary path suits an
existing database or a systemd unit.

### With Docker

Save this as `compose.yaml` — it is the self-hoster shape: published image, no
build step, secrets from a `.env`, Caddy for TLS:

```yaml
services:
  postgres:
    image: postgres:16-alpine
    environment:
      POSTGRES_USER: cairn
      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD:?set POSTGRES_PASSWORD in .env}
      POSTGRES_DB: cairn
    volumes: [pgdata:/var/lib/postgresql/data]
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U cairn"]
      interval: 5s
      timeout: 3s
      retries: 20
    restart: unless-stopped

  minio:
    image: quay.io/minio/minio:latest
    command: server /data
    environment:
      MINIO_ROOT_USER: ${CAIRN_S3_ACCESS_KEY:?set CAIRN_S3_ACCESS_KEY in .env}
      MINIO_ROOT_PASSWORD: ${CAIRN_S3_SECRET_KEY:?set CAIRN_S3_SECRET_KEY in .env}
    volumes: [miniodata:/data]
    restart: unless-stopped

  cairnd:
    image: ghcr.io/stump-wtf/cairn:latest
    depends_on:
      postgres: {condition: service_healthy}
    environment:
      CAIRN_HTTP_ADDR: ":8080"
      CAIRN_DATABASE_URL: "host=postgres port=5432 user=cairn password=${POSTGRES_PASSWORD} dbname=cairn sslmode=disable"
      CAIRN_S3_ENDPOINT: minio:9000
      CAIRN_S3_ACCESS_KEY: ${CAIRN_S3_ACCESS_KEY}
      CAIRN_S3_SECRET_KEY: ${CAIRN_S3_SECRET_KEY}
      CAIRN_S3_BUCKET: ${CAIRN_S3_BUCKET:-cairn}
      CAIRN_S3_REGION: ${CAIRN_S3_REGION:-us-east-1}
      CAIRN_S3_USE_SSL: "false"
      CAIRN_BASE_URL: https://${CAIRN_DOMAIN:?set CAIRN_DOMAIN in .env}
      CAIRN_OIDC_ISSUER: ${CAIRN_OIDC_ISSUER:-}
      CAIRN_OIDC_CLIENT_ID: ${CAIRN_OIDC_CLIENT_ID:-cairn}
      CAIRN_OIDC_CLIENT_SECRET: ${CAIRN_OIDC_CLIENT_SECRET:-}
      CAIRN_API_TOKENS: ${CAIRN_API_TOKENS:-}
      CAIRN_OUTBOUND_WEBHOOK_URLS: ${CAIRN_OUTBOUND_WEBHOOK_URLS:-}
      CAIRN_OUTBOUND_WEBHOOK_SECRET: ${CAIRN_OUTBOUND_WEBHOOK_SECRET:-}
    restart: unless-stopped

  caddy:
    image: caddy:2-alpine
    depends_on: [cairnd]
    ports: ["80:80", "443:443"]
    environment:
      CAIRN_DOMAIN: ${CAIRN_DOMAIN}
      CAIRN_ACME_EMAIL: ${CAIRN_ACME_EMAIL:-}
    volumes:
      - ./Caddyfile:/etc/caddy/Caddyfile:ro
      - caddydata:/data
      - caddyconfig:/config
    restart: unless-stopped

volumes: {pgdata: {}, miniodata: {}, caddydata: {}, caddyconfig: {}}
```

and next to it a three-line `Caddyfile`:

```text
{
	email {$CAIRN_ACME_EMAIL}
}

{$CAIRN_DOMAIN} {
	encode gzip
	reverse_proxy cairnd:8080 {
		header_up X-Forwarded-For {remote_host}
	}
}
```

Then:

```bash
cat > .env <<EOF
CAIRN_DOMAIN=cairn.example.com
CAIRN_ACME_EMAIL=you@example.com
POSTGRES_PASSWORD=$(openssl rand -hex 24)
CAIRN_S3_ACCESS_KEY=$(openssl rand -hex 16)
CAIRN_S3_SECRET_KEY=$(openssl rand -hex 24)
EOF
docker compose up -d
```

Note the unquoted heredoc delimiter (`<<EOF`, not `<<'EOF'`): it is what makes the
`$(openssl …)` substitutions run. With quotes, the file would contain the literal command
string as the password — it *looks* random in the file and the stack still starts, which is
the worst kind of wrong.

Point an A/AAAA record for `CAIRN_DOMAIN` at the host with 80 and 443 open, and
Caddy obtains the certificate on first request.

<details>
<summary>MinIO came from quay.io above — why?</summary>

MinIO's canonical Docker Hub repository was unreachable to anonymous pulls
while writing this guide, so the compose file pins the same images from
quay.io. Any S3-compatible store works; substitute Garage or MinIO from your
preferred registry freely.
</details>

`docker compose logs cairnd` on a healthy start looks like this:

```json
{"time":"…","level":"INFO","msg":"retention reaper started","interval":"1h0m0s","batch":500}
{"time":"…","level":"INFO","msg":"cairnd listening","addr":":8080"}
```

Migrations run on first boot before that line — there is no migrate step to
run. The reaper line is your confirmation that the schema exists and the
background workers came up.

The one thing `docker compose up` cannot do is authenticate anybody. Without
OIDC configured there is no interactive login, and without tokens nothing can
call the API — see [Sign-in (OIDC)](#sign-in-oidc) and
[Tokens for agents](#tokens-for-agents) before exposing the instance. To get
a first credential without an identity provider, see
[First run: bootstrap a credential](#first-run-bootstrap-a-credential).

### As a binary

```bash
git clone https://github.com/stump-wtf/cairn && cd cairn
go build -o cairnd ./cmd/cairnd

export CAIRN_DATABASE_URL='host=localhost port=5432 user=cairn password=secret dbname=cairn sslmode=disable'
export CAIRN_BASE_URL='https://cairn.example.com'
export CAIRN_S3_ENDPOINT='s3.example.com'
export CAIRN_S3_ACCESS_KEY='…'
export CAIRN_S3_SECRET_KEY='…'
export CAIRN_S3_USE_SSL=true
./cairnd
```

Go 1.26.5 or newer (see `go.mod`). `sslmode=disable` above assumes the
database is on the same host or a private network; across a network, use
`sslmode=verify-full`. Cairn accepts both the keyword/value DSN shown here and
the URL form — the keyword form is used throughout this guide. The
healthy-start logs are the same two lines as the Docker path.

### When it won't start

| Message | Cause |
|---|---|
| `db: ping: failed to connect … connection refused` | The DSN is right but nothing is listening — the database isn't up yet, or the hostname is wrong inside the compose network. |
| `s3: …` connect errors on boot | Wrong `CAIRN_S3_ENDPOINT` (host:port, no scheme) or the store isn't reachable. Cairn retries via the container restart policy in the Docker path; as a binary it exits and it is yours to restart. |

All of them say which one failed in the log line — `db:`, `s3:` — and fail
closed rather than starting half-configured.

### Behind a reverse proxy

Two things matter more than the rest:

- **Pass a trusted `X-Forwarded-For`.** The per-IP rate limiters key on the
  client IP, and with TLS terminated upstream every agent otherwise arrives
  from one address and shares a bucket. The Caddyfile above does the
  `header_up`.
- **Don't buffer the MCP route.** `/mcp` speaks Streamable HTTP with
  long-lived responses; a buffering proxy holds tool results until the
  connection closes. Caddy is fine by default; on nginx set
  `proxy_buffering off;` for the `/mcp` location.

Don't put forward-auth in front of cairn for sign-in: cairn is a complete OIDC
relying party with its own sessions.

## Sign-in (OIDC)

Register cairn as a confidential client in your provider:

- **Redirect URI:** `<CAIRN_BASE_URL>/auth/callback` — derived, never
  separately configured
- **Grant:** authorization code, with PKCE
- **Client id:** anything; `cairn` is the default

Then set `CAIRN_OIDC_ISSUER`, `CAIRN_OIDC_CLIENT_ID`, and
`CAIRN_OIDC_CLIENT_SECRET` and recreate the service.

This is the one step written but not run while writing this guide — it needs
an identity provider this project doesn't ship. What cairn does with it: on
startup it discovers the provider from the issuer URL (failing closed if the
discovery document is missing), and the login page gains a sign-in button
where without OIDC it offers nothing at all. Users are created on first
sign-in, keyed on the OIDC subject, so whoever can authenticate at your
provider can sign in here — restrict access at the provider.

To confirm it took: open `/login`. With OIDC configured you get the provider
button; with nothing configured there is no login at all — the deployment
relies on bearer tokens only.

## Tokens for agents

Two ways to authorize an agent, both documented in
[Connect your agent](./connect-your-agent.md):

- **A personal access token**, minted by a signed-in human in Settings →
  **API tokens**. This is the everyday path for headless agents. The token
  starts `cairn_pat_` and is shown exactly once.
- **OAuth 2.1**, the path interactive MCP clients take automatically — they
  discover `/.well-known/oauth-protected-resource/mcp` and run the flow.

Minting a PAT is deliberately a browser-only action (Settings is
session-authenticated and CSRF-guarded); there is no API to mint tokens with a
token, by design. Both paths need someone who can sign in, so a new instance
starts with the bootstrap credential below.

## First run: bootstrap a credential

A fresh instance has no users, and minting a personal access token needs a
browser session, which needs a sign-in provider such as
[OIDC](#sign-in-oidc). Until sign-in works, the only credential
that exists is one you put in `CAIRN_API_TOKENS` yourself. The server logs
this WARN at startup while it has neither:

```text
no API tokens (CAIRN_API_TOKENS), no OIDC (CAIRN_OIDC_ISSUER), and no dev web login (CAIRN_DEV_LOGIN_PASSWORD) configured: all authenticated endpoints will reject every caller
```

**1. Generate a secret and give it to the server.** In the directory that
holds `compose.yaml` and `.env`:

```bash
SECRET=$(openssl rand -hex 32)
printf 'CAIRN_API_TOKENS=%s:you@example.com\n' "$SECRET" >> .env
```

The entry is `secret:actor`. Use the email address you will sign in with as
the actor: today cairn names an OIDC user by the provider's `email` claim, so
what you create now stays attributed to you afterwards. Append `:agent`
(`secret:actor:agent`) for an agent-role token, which gets the three agent
scopes and never `sharing:manage`. Several entries are separated by commas.

**2. Recreate the server** so it reads the new value. `docker compose up -d`
does that when `.env` changes; `docker compose restart` does not, because it
keeps the old environment.

```bash
docker compose up -d
```

The WARN above is gone from `docker compose logs cairnd`. On the binary
path, export `CAIRN_API_TOKENS` in the environment `cairnd` starts from and
restart it instead.

**3. Use it as `CAIRN_TOKEN`.** The server's secret is the client's token:

```bash
export CAIRN_TOKEN="$SECRET"
curl -sS https://cairn.example.com/v1/whoami -H "Authorization: Bearer $CAIRN_TOKEN"
```

```json
{"actor_id":"you@example.com","channel":"via API","authenticated":true}
```

If you use the `cairn` CLI, also set `CAIRN_URL=https://cairn.example.com`,
since the CLI talks to the hosted service unless told otherwise. Then
`cairn whoami` prints `✓ authorized as you@example.com · via API`.

The bootstrap secret works on the REST API under `/v1` and with the `cairn`
CLI. It does not work on `/mcp`, which answers `401` to any static
`CAIRN_API_TOKENS` secret: an agent connecting over MCP needs a personal access
token or OAuth, so it waits for sign-in.

**4. Replace it once sign-in works.** When OIDC is configured, sign in, mint
personal access tokens in Settings → **API tokens**, then delete the
`CAIRN_API_TOKENS` line from `.env` and run `docker compose up -d` again. The
old secret answers `401` from then on.

Treat the bootstrap token as the long-lived secret it is. It never expires and
cannot be revoked from Settings; it works until you remove it from the
environment and recreate the server. `cairnd` keeps only its SHA-256 digest in
memory, but the plaintext sits in your `.env` and in the container's
environment, so protect that file like the token itself.

## Verify the whole loop

Do this once, on a fresh instance. It takes a few minutes and proves storage,
the REST API, and the agent surface all work together. The outputs below are
from the run made while writing this guide, against a local stack.

**1. Health.** No auth, no dependencies hidden:

```bash
curl -sS http://127.0.0.1:8080/healthz
```

```text
ok
```

**2. Get a token.** On a fresh instance, use the secret from
[First run: bootstrap a credential](#first-run-bootstrap-a-credential). Once
OIDC works, sign in at `/login` in a browser and mint one in Settings →
**API tokens** instead. Either way, it goes in `CAIRN_TOKEN`.

**3. Create an artifact over REST.**

```bash
export CAIRN_TOKEN='…'   # the bootstrap secret, or a cairn_pat_… token

curl -sS http://127.0.0.1:8080/v1/artifacts \
  -H "Authorization: Bearer $CAIRN_TOKEN" \
  -H 'Content-Type: text/markdown' \
  -H 'X-Cairn-Title: self-host verify' \
  --data-binary @notes.md
```

```json
{"id":"mibnFwfO","url":"http://127.0.0.1:8080/mibnFwfO","mcp":"mcp://cairn/mibnFwfO","share_type":"markdown","title":"self-host verify","size":52,"media_type":"text/markdown","previewable":true,"checksum":"33894da4…","provenance":{"actor":"you@example.com","channel":"via API","captured_at":"2026-09-13T07:40:44Z"},"visibility":"link","expires_in":"in 6d"}
```

**4. Read it back.**

```bash
curl -sS http://127.0.0.1:8080/v1/artifacts/mibnFwfO/body \
  -H "Authorization: Bearer $CAIRN_TOKEN"
```

```text
# self-host verify

Hello from a self-hosted cairn.
```

That round-trip is the split store working end to end: the metadata came from
Postgres, the bytes from object storage.

**5. Prove auth is enforced.** The same create with a junk token:

```text
HTTP 401  {"error":{"code":"unauthorized",…}}
```

Nothing is stored for a rejected request.

**6. Connect an agent over MCP.** Any MCP client speaking Streamable HTTP can
reach `<base>/mcp`. The verification used a bare JSON-RPC exchange — what a
real client does for you — with a personal access token. The bootstrap secret
from step 2 gets `401` here, because `/mcp` accepts only a personal access
token or an OAuth token, so on a fresh instance this step waits until sign-in
works:

```bash
curl -sS -X POST http://127.0.0.1:8080/mcp \
  -H "Authorization: Bearer $CAIRN_TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"verify","version":"0"}}}'
```

```text
HTTP 200
event: message
data: {"jsonrpc":"2.0","id":1,"result":{"capabilities":{"logging":{},…,"tools":{"listChanged":true}},"instructions":"Cairn is an AI-native artifact-sharing service…"},"serverInfo":{"name":"cairn","version":"0.1.0"}}
```

Complete the handshake (`notifications/initialized`, echoing the
`Mcp-Session-Id` response header), then `tools/list` shows the surface —
`artifact_create`, `artifact_read`, `artifact_comment`, `artifact_react`,
`bundle_create`, `run_create`, `run_append_spans` — and a `tools/call` of
`artifact_read` for the artifact from step 3 returns its body.

If all six steps behave that way, the instance is real: auth is enforced,
bodies round-trip through the bucket, and agents can work.

## What to do next

- [What Cairn is for](./what-cairn-is-for.md) — the model your users will
  work in
- [Create your first share](./first-share.md) and
  [connect your agent](./connect-your-agent.md) — the everyday surfaces
- [Outbound webhooks](./outbound-webhooks.md) — point `artifact.created`
  events at a Switchboard or anything else listening
- [Troubleshooting](./troubleshooting.md)
