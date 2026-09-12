# Deploying Cairn (self-host)

Cairn is a single static Go binary (`cairnd`) plus **PostgreSQL** for metadata and
an **S3-compatible object store** for artifact bodies (ADR-0008, ADR-0012). This
directory ships a compose bring-up behind Caddy with automatic HTTPS.

> **There is no image to pull.** The compose file *builds* `cairnd` from this
> repository, which works from anywhere — but nothing is published to a public
> container registry, and there are no tagged releases or prebuilt binaries yet.
> Building from source is the only route today.

## Get the source

```sh
git clone https://github.com/stump-wtf/cairn.git
cd cairn
```

No credentials needed: the repository is public and MIT-licensed. The build
pulls its dependencies from the public Go module proxy, so nothing here depends
on access to a private network.

## What it needs

- A host with Docker and Docker Compose.
- **PostgreSQL.** Not optional: all queryable metadata lives there, and `cairnd`
  applies its embedded migrations to it on boot.
- **An S3-compatible object store.** Not optional: artifact bodies are
  content-addressed blobs. The compose file runs MinIO; Garage, Ceph or AWS S3
  work the same way.
- **DNS + ports 80/443** if you want Caddy to obtain a Let's Encrypt certificate:
  an `A`/`AAAA` record for your domain pointing at the host.

## Configure

```sh
cp .env.example .env
```

Then edit `.env`. Three things are genuinely mandatory:

| Variable | Why it is mandatory |
|---|---|
| `CAIRN_BASE_URL` | **No safe default.** It is baked into every artifact URL the server mints. Set it to your own public origin, with no trailing slash. If you leave it unset, your instance advertises someone else's hostname on your artifacts (cairn#198). |
| `POSTGRES_PASSWORD` | The compose file refuses to start without it. |
| `CAIRN_S3_ACCESS_KEY` / `CAIRN_S3_SECRET_KEY` | Object-store credentials. Never ship the `minioadmin` development defaults on a reachable host. |

Everything else has a working default; `.env.example` documents the common knobs
(TTL, upload and preview size caps, rate limits) and `internal/config/config.go`
is the full list.

## Choose how people sign in

Configure at least one. `cairnd` will **start without any of them**, logging
`no API tokens (CAIRN_API_TOKENS), no OIDC (CAIRN_OIDC_ISSUER), and no dev web
login (CAIRN_DEV_LOGIN_PASSWORD) configured: all authenticated endpoints will
reject every caller` — so the instance comes up healthy, serves reads, and
refuses every create. If that is what you are seeing, this is why.

- **OIDC (recommended for humans).** Set `CAIRN_OIDC_ISSUER`,
  `CAIRN_OIDC_CLIENT_ID` and `CAIRN_OIDC_CLIENT_SECRET`. Humans then sign in
  through your identity provider, and agents authorize over OAuth.
- **Static API tokens (fine for scripts and agents).** `CAIRN_API_TOKENS` takes a
  comma-separated list of `secret:actor[:role]` entries, where `role` is `human`
  (default) or `agent`:

  ```sh
  CAIRN_API_TOKENS="s3cret-one:you@example.com,s3cret-two:bot@example.com:agent"
  ```

  Secrets are held as SHA-256 digests and a presented token must hash to a
  registered secret. An `agent` token is additionally barred from human-only
  operations such as deleting an artifact.
- **A shared web-login password**, for a single-user or development instance:
  `CAIRN_DEV_LOGIN_PASSWORD`. If `CAIRN_OIDC_ISSUER` is also set, OIDC wins and
  this login is disabled (ADR-0013); `cairnd` warns at startup when both are
  configured.

**What `actor_id` guarantees.** It is the identity Cairn authenticated: the actor
a static token was minted for, or the OIDC subject. It is the only field on an
artifact that is not client-asserted. `on_behalf_of` is an MCP client's
self-reported name, and tags and titles are whatever the creator sent — useful
context, never proof. Route on them if you like; never authorize on them.

`CAIRN_DEV_INSECURE_BEARER_AUTH` exists for local development only. It makes the
API trust a raw bearer token *as* an actor id without verification. It defaults
to **off**, and turning it on logs a warning at startup. Never set it on a host
anyone else can reach.

## Bring it up

```sh
docker compose -f docker-compose.prod.yml up -d --build
```

On boot `cairnd` applies its embedded migrations and creates the object-storage
bucket if it is missing.

## Verify it works

Replace `https://cairn.example` with your `CAIRN_BASE_URL`.

```sh
# 1. liveness
curl -fsS https://cairn.example/healthz            # -> ok

# 2. create something (use one of your CAIRN_API_TOKENS secrets)
export TOKEN=s3cret-one
curl -sS -X POST https://cairn.example/v1/artifacts \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: text/markdown' \
  -H 'X-Cairn-Title: hello.md' \
  --data-binary $'# Hello from Cairn\n\nThis is **markdown**.\n'
# -> 201 with {"id":"…","url":"https://cairn.example/…","checksum":"…", …}

# 3. read it back anonymously — a link grants read, by design
curl -sS https://cairn.example/v1/artifacts/<id>

# 4. prove the bytes survived: this must equal the checksum above
curl -sS https://cairn.example/v1/artifacts/<id>/body | shasum -a 256

# 5. your Bin (authenticated)
curl -sS -H "Authorization: Bearer $TOKEN" https://cairn.example/v1/bin
```

A healthy instance answers `200` on `/healthz`, returns `201` from step 2, serves
step 3 with no credential, and produces a checksum in step 4 identical to the one
step 2 returned.

**Check the instance is yours.** The `url` in step 2's response must start with
*your* `CAIRN_BASE_URL`. If it names somebody else's host, that value is wrong or
unset, and every link your instance mints — including the ones it sends to any
outbound webhook target — points at their deployment instead of yours. Fix the
variable and restart before sharing anything; artifacts already created keep the
old URL until their id is rotated. Two useful negative checks: an unregistered bearer token must get
`401`, and an unknown artifact id must get `404 not_found` rather than anything
that distinguishes "wrong id" from "not yours".

## Security

- **Links are capabilities.** Anyone holding an artifact's URL can read it. That
  is the design (ADR-0007), not a gap — but it means a link is a secret.
- **Creation requires a credential.** An unregistered token is rejected with
  `401`; only OIDC sessions and registered tokens can create.
- **Outbound webhooks are instance-wide.** If you set
  `CAIRN_OUTBOUND_WEBHOOK_URLS`, *every* artifact created on your instance
  announces its id, title, URL and creator to those targets (ADR-0017, SPEC-0012).
  The URLs are bearer capabilities: never log or share them.
- The app sets `nosniff`, serves bodies as non-executable attachment downloads,
  rate-limits per IP, and caps upload size. Caddy terminates TLS and adds HSTS.

## Operations

- **Logs:** `docker compose -f docker-compose.prod.yml logs -f cairnd`
- **Backups:** snapshot both volumes — `pgdata` (metadata, the source of truth)
  and `miniodata` (bodies). Neither is sufficient alone.
- **Config:** every knob is a `CAIRN_*` environment variable; change `.env` and
  `up -d` to apply.
- **Retention:** artifacts expire (7 days by default, `CAIRN_DEFAULT_TTL`). A
  background reaper deletes expired artifacts and garbage-collects orphaned
  blobs, so storage does not grow without bound.

## Known gaps

- `/healthz` is a static liveness probe. It does not check Postgres or object
  storage, so it answers `ok` even when a dependency is down.
- There is no boot-time refusal of development defaults: an instance started with
  `minioadmin` credentials will run.
