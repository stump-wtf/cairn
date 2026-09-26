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
| `CAIRN_API_TOKENS` | *(empty)* | Static bearer credentials for headless agents, comma-separated `secret:actor[:role]`. **Empty means the bearer surface accepts no tokens** — it fails closed. Real per-agent tokens come from OAuth or a personal access token minted in Settings. Wired through the compose file above; on the Docker path with no OIDC provider yet this is the only way to get a working credential. |
| `CAIRN_OIDC_ISSUER` | *(empty)* | Issuer URL of your OIDC provider. **Its presence is the switch that turns OIDC on** and, just as importantly, turns the dev-password login off (below). |
| `CAIRN_OIDC_CLIENT_ID` | `cairn` | Client id registered at your provider. |
| `CAIRN_OIDC_CLIENT_SECRET` | *(empty)* | Client secret. The redirect URI is not configurable — it is always `<base>/auth/callback`. |
| `CAIRN_OPERATORS` | *(empty)* | Who runs this instance: comma-separated `<issuer>\|<subject>` sign-in identities, for example `https://id.example.com\|abc-123`. Listed users get the [operator console](#the-operator). **Empty (with `CAIRN_OPERATOR_GROUP` empty too) means there is no operator** and every operator route answers `404`. A malformed entry fails boot. |
| `CAIRN_OPERATOR_GROUP` | *(empty)* | An OIDC group whose members are operators, read from the `groups` claim at sign-in. Setting it makes cairn request the `groups` scope. Removal from the group at the IdP takes effect only when that user's session ends ([details](#the-operator)). |
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
  development seams.** The dev password is disabled the moment any real
  provider is configured, OIDC (`CAIRN_OIDC_ISSUER`) or GitHub
  (`CAIRN_GITHUB_CLIENT_ID`), and its route then answers `404`. The insecure
  bearer shortcut defaults off, and `cairnd` refuses to start with it on
  while `CAIRN_BASE_URL` is `https`. Leave both alone on a real deployment.
- **Your IdP must mark emails verified.** A sign-in is keyed on the provider's
  `(issuer, subject)`. Its email only identifies a person when the ID token
  says `email_verified: true`; otherwise the user is keyed on their subject
  and does not see artifacts owned by that email. Check that your OIDC
  provider sends the claim before upgrading.

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
      CAIRN_OPERATORS: ${CAIRN_OPERATORS:-}
      CAIRN_OPERATOR_GROUP: ${CAIRN_OPERATOR_GROUP:-}
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
[Tokens for agents](#tokens-for-agents) before exposing the instance.

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

## The operator

The **operator** is whoever runs the instance. Name yourself in
`CAIRN_OPERATORS` as `<issuer>|<subject>`: the issuer is your
`CAIRN_OIDC_ISSUER` exactly as configured (or `https://github.com` for a GitHub
sign-in, whose subject is the numeric account id), and the subject is your
account's ID-token `sub`. You do not have to work it out: sign in once and
Settings → **Account** shows your **Sign-in identity** in that exact form. Or
set `CAIRN_OPERATOR_GROUP` to a group your IdP puts in the `groups` claim.
Operator status is decided on every request from the current configuration, so
removing an entry takes effect on that user's next request; a group operator
must sign in again after being added to the group.

Group membership works the other way too: the `groups` claim is read only at
sign-in and kept with the session. **Removing someone from the operator group at
your IdP takes effect only when their session ends**, up to `CAIRN_SESSION_TTL`
(7 days by default). To demote them now, rename `CAIRN_OPERATOR_GROUP` (every
group operator then signs in again under the new name), or delete their
sessions from the database.

An operator is also an ordinary user. Their Bin holds their own artifacts, and
being an operator never lets them open anyone else's. The console at
`/operator`, linked from Settings, is deliberately small:

- a **directory** of users with how many artifacts each owns and how many bytes
  they take up. It never shows an artifact's title, body, link, tags or
  comments;
- **suspension**. Suspending a user signs them out everywhere at once: their
  browser sessions, personal access tokens and OAuth grants (with every access
  and refresh token) stop working on their next request, and they cannot sign
  in until reinstated. Reinstating restores nothing; they sign in again and
  mint new tokens. A user listed in `CAIRN_OPERATORS` cannot be suspended from
  the console: remove them from the list first;
- an **audit log**. Every action needs a reason, and is recorded in the same
  database transaction as the action itself. The affected user sees the rows
  about them on their Settings page.

Every operator route needs the operator's browser session; an API token, even
the operator's own, gets `404`, and so does everyone else. With neither
variable set there is no operator at all and those routes answer `404` to
everyone.

:::warning The operator can still read everything underneath
The console removes the easy path, not the possibility. Whoever controls the
Postgres database and the object store can read every artifact, comment and
captured request in them, whatever the product shows. Treat access to those
the way you treat the data itself, and tell your users.
:::

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
token, by design.

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

**2. Sign in and mint a token.** With OIDC configured, sign in at `/login`
in a browser, then Settings → **API tokens**. (The verification run used the
dev-password login: the form posts `actor`, `password`, and the CSRF field
the login page embeds — a browser does all of that for you.)

**3. Create an artifact over REST.**

```bash
export CAIRN_TOKEN='cairn_pat_…'

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
real client does for you — with the PAT from step 2:

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
