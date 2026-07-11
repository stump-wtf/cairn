# Deploying Cairn (self-host)

Cairn is a single static Go binary + PostgreSQL + an S3-compatible object store
(ADR-0012). This directory ships a one-command bring-up behind Caddy with
automatic HTTPS.

> **Read this first — current scope of the merged code.** `main` today is the
> SPEC-0002 foundation: the **`/v1` JSON API** (paste/read/download/list/delete,
> bundles). There is **no web UI, no CLI, no MCP server, and no trajectory
> viewer yet** (those are later specs / the fork's MVP milestone). And auth is a
> **development stub**: any `Authorization: Bearer <token>` is accepted as its
> own user id — it is *not* an access-control boundary. Do not expose this to
> untrusted users until real OAuth (SPEC-0007) lands; gate it (see Security).

## Prerequisites

- A host with Docker + Docker Compose.
- DNS: an `A`/`AAAA` record for your domain (e.g. `cairn.stump.rocks`) pointing
  at the host, with ports **80** and **443** reachable (Caddy needs them for
  Let's Encrypt).

## Bring it up

```sh
cp .env.example .env
# edit .env: set CAIRN_DOMAIN, CAIRN_BASE_URL, and strong POSTGRES_PASSWORD /
# CAIRN_S3_ACCESS_KEY / CAIRN_S3_SECRET_KEY (do NOT keep the change-me defaults)
docker compose -f docker-compose.prod.yml up -d --build
```

On boot `cairnd` applies its embedded migrations and creates the object-storage
bucket automatically. Check health:

```sh
docker compose -f docker-compose.prod.yml ps
curl -fsS https://cairn.stump.rocks/healthz     # -> ok
```

## Paste some Markdown (the API today)

```sh
BASE=https://cairn.stump.rocks
TOKEN=you@example.com     # dev stub: the token becomes the actor id

# Paste
curl -sS -XPOST "$BASE/v1/artifacts?type=markdown&title=hello.md" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: text/markdown" \
  --data-binary $'# Hello from Cairn\n\nThis is **markdown**.\n'

# -> {"id":"r4gbcBnH","url":"https://cairn.stump.rocks/r4gbcBnH","badge":"MD",
#     "previewable":true,"checksum":"262cc1…","share_type":"markdown", …}

# Read it back (public, link-capability) and re-verify the bytes
curl -sS "$BASE/v1/artifacts/r4gbcBnH"
curl -sS "$BASE/v1/artifacts/r4gbcBnH/body" | sha256sum   # == checksum above

# Your Bin (authenticated)
curl -sS -H "Authorization: Bearer $TOKEN" "$BASE/v1/bin"
```

Bundles: `POST /v1/artifacts` as `multipart/form-data` with N file parts creates
one bundle; members read at `/v1/artifacts/<id>/members/<name>`.

## Security (important while auth is a stub)

Because any bearer is accepted, **anyone who can reach the host can create
artifacts**, and reads are public-by-link by design. Until SPEC-0007 (OAuth) and
SPEC-0009 (access policy) land, do one of:

- Keep the instance on a private network / VPN, **or**
- Enable Caddy `basic_auth` (uncomment the block in `deploy/Caddyfile`) so only
  you can reach it. Generate a hash with
  `docker run --rm caddy:2-alpine caddy hash-password --plaintext '…'`.

The app already sets `nosniff`, serves bodies as non-executable
`attachment` downloads, rate-limits per IP, and enforces upload size limits.
Caddy terminates TLS and adds HSTS on HTTPS.

## Operations

- **Logs:** `docker compose -f docker-compose.prod.yml logs -f cairnd`
- **Backups:** snapshot the `pgdata` (metadata, the source of truth) and
  `miniodata` (bodies) volumes.
- **Config:** every knob is a `CAIRN_*` env var (see `.env.example`); change and
  `up -d` to apply.
- **Upgrade:** `git pull && docker compose -f docker-compose.prod.yml up -d --build`.

## Not yet included (roadmap)

- Web UI + the Bin browser (SPEC-0001), rich viewers (SPEC-0003), trajectory
  capture/waterfall (SPEC-0004), webhooks (SPEC-0005), annotations (SPEC-0006),
  the MCP server + real OAuth (SPEC-0007), the CLI (SPEC-0008), and the retention
  reaper (SPEC-0009). Track these on the active milestone.
- A `/readyz` that checks Postgres + object storage (today `/healthz` is a static
  liveness probe) and boot-time refusal of default dev secrets.
