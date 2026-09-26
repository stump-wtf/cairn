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
is read from the `CAIRN_` namespace. The one file cairn ever reads is the
optional redaction allowlist that `CAIRN_REDACTION_ALLOWLIST_FILE` names (see
[Secret redaction](#secret-redaction)).

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
| `CAIRN_REDACTION_REJECT_TYPES` | `code,bundle` | Share types whose creates are refused, not masked, when a credential is found. See [Secret redaction](#secret-redaction). |
| `CAIRN_REDACTION_MAX_SCAN_BYTES` | `16777216` | Per-field credential-scan cap in bytes (16 MiB). |
| `CAIRN_REDACTION_OVERSIZE` | `reject` | What happens to a text field over the scan cap: `reject` or `store_unscanned`. **Leave it at `reject`.** |
| `CAIRN_REDACTION_ALLOWLIST_FILE` | *(empty)* | Path to the operator's value-only allowlist TOML. Empty means no allowlist. |

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
| `config CAIRN_REDACTION_…: …` | A redaction variable has a value cairn does not accept, such as a `CAIRN_REDACTION_OVERSIZE` other than `reject` or `store_unscanned`, or a scan cap that is not a positive integer. |
| `ingest redaction: redaction allowlist <path>: …` | The allowlist file is missing, does not parse, or has an entry cairn refuses. The message names the entry. See [the allowlist format](#operator-allowlist). |

All of them say which one failed in the log line — `db:`, `s3:`, `config`,
`ingest redaction:` — and fail closed rather than starting half-configured.

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
token, by design.

## Secret redaction

Cairn scans every text it stores for credentials before it stores it:
artifact bodies and titles, bundle members, comments, trace runs and spans, and
webhook captures. A hit is either **masked**, which replaces only the secret
value with the literal `[REDACTED]` and stores the rest, or **rejected**, which
refuses the write with an error that names the field, the rule, and the line,
never the value. The engine is the gitleaks v8 library with its default ruleset
plus cairn's own rules for the shapes agents leak most: `Authorization`
headers, API-key headers, passwords in URLs, secret-named assignments and flags,
and `curl -u`. See [ADR-0023](../decisions/ADR-0023.md) and
[SPEC-0017](../specs/ingest-redaction/index.md).

**There is deliberately no off switch.** No variable, request header, or tool
argument turns scanning off, and cairnd refuses to start if the scanner cannot
be built. A writer can only downgrade a rejecting type to masking for one
request (`X-Cairn-Redaction: mask`, `--redact=mask`, or `redaction: "mask"` over
MCP). Any other value is refused: the API answers `validation_failed`, and the
CLI stops with a usage error before it sends anything.

### What is masked and what is rejected

| Content | Fields scanned | Default |
|---|---|---|
| `markdown`, and a `file` whose body sniffs as text | body, title | mask |
| `code` | body, title | **reject** |
| `bundle` | each text member, title | **reject** |
| Trace run (batch create and append) | run title, prompt, span name, args, output | mask |
| Comment (create and edit) | body | mask |
| Webhook capture | query, headers, body | mask |

A `file` upload whose media type names a programming language is stored as
code, so it rejects like code. Masking changes the stored bytes, so the stored
SHA-256 is of the masked body and the create response says `redacted: true`
(see [Troubleshooting](./troubleshooting.md#a-checksum-differs-after-upload)).
The owner sees the outcome on each artifact: a status (`clean`, `masked`,
`not_scanned_binary`, `not_scanned_oversize`, or `unscanned` for content stored
before scanning existed), a count, and the rule IDs. Never the value.

### Variables

| Variable | Default | Accepted values |
|---|---|---|
| `CAIRN_REDACTION_REJECT_TYPES` | `code,bundle` | Comma-separated share types, case-insensitive, whitespace trimmed. Only artifact and bundle creates consult it: traces, comments, and webhook captures always mask. Names are not checked against the known share types, so a misspelt type silently masks. Empty or unset means the default. |
| `CAIRN_REDACTION_MAX_SCAN_BYTES` | `16777216` (16 MiB) | A positive integer number of bytes, applied to each scanned field. An artifact or bundle body over 1 MiB is scanned in overlapping windows read back from staging, never buffered whole. Zero, a negative number, or a non-integer stops startup. |
| `CAIRN_REDACTION_OVERSIZE` | `reject` | `reject` refuses a text field over the cap with `too_large_to_scan`. `store_unscanned` stores it unscanned with status `not_scanned_oversize`. Anything else, including a different case, stops startup. |
| `CAIRN_REDACTION_ALLOWLIST_FILE` | *(empty)* | A path to an operator allowlist TOML, described below. Empty means no allowlist. A file that is missing or does not parse stops startup. |

The compose file above passes none of these through, so the defaults apply.
To change one, add it to the `cairnd` service's `environment`. For an
allowlist, also mount the file read-only into the container and point the
variable at the path inside it.

Webhook captures are never refused, because the sender is an anonymous third
party. Under `reject`, a capture field over the cap is replaced with a notice
and named in the capture's `redaction_withheld` list instead.

:::warning[`CAIRN_REDACTION_OVERSIZE=store_unscanned` stores credentials]
With `store_unscanned`, any text field over `CAIRN_REDACTION_MAX_SCAN_BYTES` is
stored **exactly as sent, with no credential scan**, and everyone the link
reaches can read whatever secret it holds. Cairn logs a WARN naming
`CAIRN_REDACTION_OVERSIZE` at startup, and another WARN, with the artifact's id
and the field's size, each time it stores a field unscanned. Leave it at
`reject`. If large text needs sharing, raise the cap instead: a bigger cap costs
scan time, while `store_unscanned` costs the scan.
:::

### Operator allowlist

`CAIRN_REDACTION_ALLOWLIST_FILE` exempts values, never places. The file is
operator configuration read once at startup; no user, token, or request can set
or change it. It accepts only these three top-level keys, each optional:

```toml
# Values matching one of these regexes are not masked or rejected. Each regex is
# matched against the detected value and must be anchored with ^ and $.
regexes = [
  '''^cairn-docs-fixture-[0-9]{4}$''',
]

# A detected value containing any of these words, ignoring case, is exempt.
stopwords = [
  "cairn-demo-placeholder",
]

# Rule IDs to switch off entirely.
disabledRules = []
```

Cairn refuses to start, naming the file and the entry, when:

- a regex does not compile, is not anchored with `^` and `$` (a leading flag
  group such as `(?i)` is allowed), or matches the empty string;
- a stopword or a `disabledRules` entry is empty;
- a `disabledRules` entry is not a known rule ID;
- the file has `paths`, `commits`, or any other key. Path allowlists are
  refused because an uploader chooses their own file names, and an upload has
  no commits to match.

**List only inert fixture values**: the placeholder strings your own tests,
docs, and demos use, which authenticate nowhere. Never list a real credential,
even a revoked one, and never a pattern that could match one. A stopword
exempts every detected value that *contains* it, so keep stopwords long and
specific. Prefer an anchored regex for one exact value, and prefer either to
`disabledRules`, which blinds cairn to a whole shape of secret.

### What scanning does not catch

Scanning is defence in depth, not a guarantee. These pass through:

- **Secrets in shapes no rule recognises**, such as a bare random string with no
  label, prefix, or header around it.
- **Archives.** Zip, tar, gzip, and the like are never unpacked.
- **Images and other binary bodies.** A body whose leading bytes match a known
  binary signature is stored unscanned with status `not_scanned_binary`. A body
  declared as an image that is really text is scanned.
- **Anything over the cap** when `CAIRN_REDACTION_OVERSIZE=store_unscanned`.

Treat a shared link as readable by anyone it reaches, and rotate any credential
that was ever pasted into one.

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
