# Design: Search, Semantic Search, and Export by Tag

## Context

ADR-0028 adds owner-scoped search. Lexical search is Postgres full-text search, semantic
search is optional pgvector, hybrid ranking uses reciprocal rank fusion, and `cairn export
--tag` writes Markdown. SPEC-0022 holds the requirements. This document gives the concrete
shapes.

Current state:

* `store.ListBin(ctx, ownerID, cursor, limit, tags...)` is the only multi-artifact read. It
  is keyset-paginated over `(created_at, id)`, filters on `owner_id = $1 AND expires_at >
  now()`, and supports tag containment (`internal/store/list.go`). Its comment notes that
  there is no GIN index on `tags` yet, because every query is owner-narrowed.
* Bodies live in S3-compatible storage, keyed by SHA-256 (ADR-0008). Postgres holds no body
  text.
* Migrations are embedded (`internal/db/migrations/*.sql`) and applied in lexical order at
  startup by `db.Migrate`. There is no mechanism for optional migrations yet.
* CI and both compose files run `postgres:16-alpine`, which does not ship pgvector.
* Owner today is `artifacts.owner_id` (TEXT). ADR-0029 moves it to
  `owner_user_id` XOR `owner_team_id` and adds `authorizeRead(principal, artifact)` in the
  store.

## Goals / Non-Goals

### Goals
- Search that is correct by construction on tenancy: scope lives inside the ranking SQL.
- Lexical search with zero new infrastructure. Semantic search behind one config block and
  an extension.
- An index whose lifetime equals its artifact's, with no sync job.
- A Markdown export that external indexers and PR workflows can consume unchanged.

### Non-Goals
- Cross-workspace or instance-wide search, including for the operator. ADR-0029's operator
  role bounds data and never reads it through the product.
- Searching inside binary files (PDF text extraction, OCR). This is a possible later
  extractor.
- Query suggestions, facets with counts, or typo tolerance beyond what `websearch_to_tsquery`
  gives.
- Server-side export endpoints, or pushing to repositories.

## Decisions

### A search document table, not a column on `artifacts`

**Choice.** `search_documents(artifact_id PK → artifacts ON DELETE CASCADE, owner key,
created_at, expires_at, share_type, tags, schema, retention, doc tsvector, content text)`.
The filter and scope columns are denormalized from the artifact at index time and refreshed
on any change to them.

**Rationale.** It keeps the hot `artifacts` table narrow, and it lets the search query hit
one table with one GIN index plus B-tree filter indexes. `content` holds the capped,
redacted text for `ts_headline` snippets. It is a copy, deleted by cascade.

**Alternative considered.** A generated `tsvector` column on `artifacts`. It cannot include
body text, because bodies are not in Postgres, so a separate table is needed anyway.

### Scope as a bind-parameter array of owner keys

**Choice.** The store resolves the caller's reachable owner keys once per request
(`[user:<id>]`, plus `team:<id>` for each membership once ADR-0029 lands) and passes them as
arrays. The query applies `(owner_user_id = ANY($1) OR owner_team_id = ANY($2))`. Every
returned id then passes `authorizeRead` in Go before rendering. A mismatch is a logged bug,
dropped from the page.

**Rationale.** One predicate works for lexical, semantic and list modes. The Go-side check is
cheap, and it catches a future query that forgets the predicate.

### Chunking and embeddings

**Choice.** Chunks are about 1,500 characters, with 200 characters of overlap, cut on
paragraph and then sentence boundaries. The title and tags are prepended to every chunk as
context. Chunks are embedded in batches of `CAIRN_SEARCH_EMBEDDINGS_BATCH` (default 32),
with a timeout of `CAIRN_SEARCH_EMBEDDINGS_TIMEOUT` (default 30s).

**Rationale.** These sizes fit common local embedding models (512 to 8k token windows)
without per-model tuning, and the overlap preserves context across cuts.

### Optional migration set for pgvector

**Choice.** `internal/db/migrations/semantic/*.sql` is embedded separately.
`db.MigrateSemantic` runs only when `CAIRN_SEARCH_SEMANTIC=true`. Its first statement is
`CREATE EXTENSION IF NOT EXISTS vector`. On failure, startup aborts with "semantic search
needs the pgvector `vector` extension".

**Rationale.** A self-hoster on stock Postgres never needs pgvector. Enabling semantic search
later is one environment variable plus an image switch (`pgvector/pgvector:pg16`).

### The dimension is recorded, not assumed

**Choice.** `search_semantic_state(singleton, model, dimensions, reindexed_at)`. At startup,
Cairn compares it with the configuration. On a mismatch it refuses semantic queries (`503
semantic_reindex_required`) until `cairnd search reindex --semantic` rebuilds and updates the
row.

**Rationale.** A `vector(N)` column has a fixed N. Silently mixing models produces garbage
similarity. It should fail loudly instead.

### Hybrid = reciprocal rank fusion in SQL

**Choice.** Two CTEs take the top 100 lexical and the top 100 semantic rows, each under the
scope predicate. A full outer join then computes `Σ 1/(60 + rank)`, ordered with a
deterministic tie-break.

**Rationale.** No score normalization is needed, the whole search is one statement, and the
standard k is well studied.

### Export is client-side

**Choice.** `cairn export` pages `GET /v1/search?tag=…` in list mode, then fetches
`GET /v1/artifacts/{id}` and `/body` (and `/members/*` for bundles), and renders files
locally.

**Rationale.** It inherits scope and auth with no new endpoint. SPEC-0008 already makes the
CLI a pure REST client.

## Architecture

```mermaid
sequenceDiagram
    autonumber
    participant Cl as Client (MCP / REST / Bin)
    participant API as httpapi
    participant SS as search.Service
    participant PG as Postgres
    participant EP as embeddings endpoint
    Cl->>API: artifact_search {query, tags, mode}
    API->>API: auth, artifacts:read, agent switch, rate limit
    API->>SS: Search(principal, params)
    SS->>SS: reachable owner keys (me + my teams)
    alt mode includes semantic
        SS->>EP: embed(query)  [query only; no artifact text]
        EP-->>SS: vector
    end
    SS->>PG: one statement: scope ∧ filters ∧ expires_at > now() → lexical CTE, semantic CTE, RRF
    PG-->>SS: ranked ids + snippets
    SS->>SS: authorizeRead each row (defense in depth)
    SS-->>API: results, cursor, truncated
    API-->>Cl: 200
```

Embedding the **query** sends only the searcher's own query text to the endpoint. The query is
not redacted by the ADR-0023 scanner, because it is the caller's input to the caller's own
search, but it is never logged.

### Schema

Core, always applied (next free migration number at implementation time):

```sql
-- Search documents (ADR-0028, SPEC-0022). One row per indexed artifact; the index lives
-- and dies with its artifact via ON DELETE CASCADE.
CREATE TABLE search_documents (
    artifact_id  BIGINT      PRIMARY KEY REFERENCES artifacts(id) ON DELETE CASCADE,
    owner_id     TEXT        NOT NULL,        -- becomes owner_user_id / owner_team_id with ADR-0029
    share_type   TEXT        NOT NULL,
    tags         TEXT[]      NOT NULL DEFAULT '{}',
    schema       TEXT,                        -- metadata schema id, e.g. cairn.receipt/v1
    retention    TEXT        NOT NULL DEFAULT 'ephemeral',
    created_at   TIMESTAMPTZ NOT NULL,
    expires_at   TIMESTAMPTZ NOT NULL,
    doc          TSVECTOR    NOT NULL,
    content      TEXT        NOT NULL,        -- capped, redacted text for ts_headline
    indexed_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX search_documents_doc_idx   ON search_documents USING GIN (doc);
CREATE INDEX search_documents_owner_idx ON search_documents (owner_id, created_at DESC, artifact_id DESC);
CREATE INDEX search_documents_tags_idx  ON search_documents USING GIN (tags);

-- Work queue for the indexer (FOR UPDATE SKIP LOCKED).
CREATE TABLE search_queue (
    artifact_id  BIGINT      PRIMARY KEY REFERENCES artifacts(id) ON DELETE CASCADE,
    reason       TEXT        NOT NULL,        -- created | comment | metadata | reindex
    attempts     INT         NOT NULL DEFAULT 0,
    not_before   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_error   TEXT        NOT NULL DEFAULT ''
);
```

The `doc` vector is built as:

```sql
setweight(to_tsvector($cfg, title), 'A') ||
setweight(to_tsvector($cfg, array_to_string(tags, ' ') || ' ' || metadata_text), 'B') ||
setweight(to_tsvector($cfg, body_text), 'C') ||
setweight(to_tsvector($cfg, comment_text), 'D')
```

The optional semantic set (`migrations/semantic/`), applied only when enabled:

```sql
CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE search_semantic_state (
    id           BOOLEAN PRIMARY KEY DEFAULT true CHECK (id),
    model        TEXT    NOT NULL,
    dimensions   INT     NOT NULL,
    reindexed_at TIMESTAMPTZ
);

-- The vector column's dimension is set from configuration when this table is created;
-- a model change drops and recreates it through `cairnd search reindex --semantic`.
CREATE TABLE search_chunks (
    artifact_id  BIGINT  NOT NULL REFERENCES artifacts(id) ON DELETE CASCADE,
    chunk_no     INT     NOT NULL,
    owner_id     TEXT    NOT NULL,
    content      TEXT    NOT NULL,
    embedding    VECTOR  NOT NULL,            -- typed vector(N) at creation
    PRIMARY KEY (artifact_id, chunk_no)
);
CREATE INDEX search_chunks_owner_idx ON search_chunks (owner_id);
-- CREATE INDEX search_chunks_hnsw ON search_chunks USING hnsw (embedding vector_cosine_ops);
-- created by the reindex command once the dimension is known.
```

CI changes: the integration job's Postgres image becomes `pgvector/pgvector:pg16`, a superset
of the stock image. A second test pass runs with `CAIRN_SEARCH_SEMANTIC=true` against a stub
embeddings server that returns deterministic vectors.

### Configuration

| Variable | Default | Meaning |
|---|---|---|
| `CAIRN_SEARCH_LANGUAGE` | `english` | Postgres text-search configuration |
| `CAIRN_SEARCH_MAX_BODY_BYTES` | `262144` | Per-body index cap |
| `CAIRN_SEARCH_AGENTS` | `false` | Allow agent tokens to search (SPEC-0022 REQ-14); WARN at startup when `true` |
| `CAIRN_SEARCH_RATE_PER_MINUTE` | `60` | Per-principal search rate |
| `CAIRN_SEARCH_INDEX_WORKERS` | `2` | Indexer pool size |
| `CAIRN_SEARCH_SEMANTIC` | `false` | Enable semantic search (needs pgvector) |
| `CAIRN_SEARCH_EMBEDDINGS_URL` | — | OpenAI-compatible base URL, e.g. `http://localhost:11434/v1` |
| `CAIRN_SEARCH_EMBEDDINGS_MODEL` | — | e.g. `nomic-embed-text` |
| `CAIRN_SEARCH_EMBEDDING_DIMENSIONS` | — | e.g. `768`; must match the model |
| `CAIRN_SEARCH_EMBEDDINGS_API_KEY` / `_FILE` | — | Optional bearer key |
| `CAIRN_SEARCH_EMBEDDINGS_BATCH` | `32` | Inputs per request |
| `CAIRN_SEARCH_EMBEDDINGS_TIMEOUT` | `30s` | Per-request timeout |

### REST: `GET /v1/search`

`GET /v1/search?q=thread%20replies&tag=receipt&mode=hybrid&limit=10` returns `200`:

```json
{
  "results": [
    {
      "id": "R4m2x",
      "url": "https://cairn.example.com/R4m2x",
      "mcp": "mcp://cairn/R4m2x",
      "title": "Receipt: Support replies now reach the original thread",
      "share_type": "markdown",
      "tags": ["receipt", "repo:example/app"],
      "created_at": "2026-09-20T14:02:11Z",
      "expires_at": "2026-09-27T14:02:11Z",
      "retention": {"mode": "ephemeral"},
      "schema": "cairn.receipt/v1",
      "score": 0.0325,
      "snippet": "…the Slack API returns «thread_ts» only on the parent event…",
      "matched_in": ["metadata", "body"]
    }
  ],
  "mode": "hybrid",
  "next_cursor": "eyJvIjoxMCwicSI6IjNmOWEifQ",
  "truncated": false
}
```

The cursor encodes the offset and a hash of the normalized query and filters. A cursor reused
with different parameters returns `400` with reason `cursor_mismatch`.

### MCP: `artifact_search`

The input takes `query`, `tags`, `share_type`, `since`, `until`, `retention`, `schema`, `mode`,
`limit` and `cursor`, all optional. The output is the REST object. The tool description says
that an empty query lists newest first, and that results are only the authorizing human's own
and team artifacts.

### CLI

```
cairn search "thread replies" --tag receipt --mode hybrid
cairn search --tag learning --since 2026-09-01 --json      # list mode
cairn export --tag learning --since 2026-09-01 --out docs/learnings/
```

Export file example (`2026-09-20-slack-thread-replies-R4m2x.md`):

```markdown
---
id: R4m2x
url: https://cairn.example.com/R4m2x
title: "Receipt: Support replies now reach the original thread"
tags: [receipt, learning, repo:example/app]
created_at: 2026-09-20T14:02:11Z
expires_at: 2026-09-27T14:02:11Z
actor: sam@example.com
model: claude-opus-5
channel: via MCP
checksum: 9f2c…e1
retention: ephemeral
metadata:
  schema: cairn.receipt/v1
  changed: Support replies now reach the customer's original Slack thread.
  learned: The Slack API returns thread_ts only on the parent event.
  # … every receipt field
---

<the artifact body, verbatim>
```

Keys are emitted in a fixed order, and YAML is emitted with a deterministic encoder, which
gives byte stability.

### Code map

| Area | Files |
|---|---|
| Search service | `internal/search/` (new): `service.go` (query builder, RRF), `indexer.go` (worker, extraction, chunking), `embed.go` (OpenAI-compatible client), `scope.go` |
| Store hooks | `store/create.go`, `bundle.go`, `annotation/service.go` (enqueue on commit) |
| Migrations | `internal/db/migrations/NNNN_search.sql`, `internal/db/migrations/semantic/*.sql`, `internal/db/migrate.go` (`MigrateSemantic`) |
| HTTP | `internal/httpapi/search.go` (new), `api.go` (route), `webbin.go` and `templates/bin.html` (search box) |
| MCP | `internal/httpapi/mcp.go` (`artifact_search`), `mcp_schema.go` |
| CLI | `internal/clicmd/search.go`, `export.go` (the current `export.go` holds error helpers, so the file is renamed, not reused), `internal/cliclient` |
| Operator | `cmd/cairnd` (`search reindex`) |
| Config | `internal/config/config.go` |
| CI | `.gitea/workflows/pipeline.yaml` (pgvector image, semantic test pass), `docker-compose*.yml` |

## Risks / Trade-offs

- **Filtered approximate-nearest-neighbour search loses recall for small owners on big
  instances.** Mitigation: pgvector ≥ 0.8 iterative scans (`hnsw.iterative_scan =
  relaxed_order`), a raised `ef_search`, and lexical fallback in hybrid mode.
- **The index copy increases database size.** Mitigation: the per-body cap, no binaries, and a
  `cairn_search_documents` gauge.
- **A prompt-injected agent searches its human's corpus.** Mitigation: agent search is off by
  default, and turning it on logs a WARN; then redaction at ingest, rate limits, and the consent
  copy. The residual risk is stated in
  ADR-0028.
- **Query text is sent to a hosted embeddings API.** Mitigation: it is off by default, local
  models are documented first, and the operator docs state the data flow.
- **Indexer backlog after a bulk import.** Mitigation: a bounded worker pool, the lag gauge,
  and `cairnd search reindex --owner` for targeted rebuilds.

## Migration Plan

1. Core migration: the new tables and indexes. No change to `artifacts`.
2. Deploy the indexer, then backfill with `cairnd search reindex` (live artifacts only;
   expired rows never get indexed).
3. Ship REST, MCP and CLI search, the Bin box, then export.
4. Semantic search: an operator switches to a pgvector image, sets the `CAIRN_SEARCH_*`
   variables, restarts (the optional migrations apply), and runs `cairnd search reindex
   --semantic`.
5. When ADR-0029 lands, the owner columns on `search_documents` and `search_chunks` follow its
   `owner_user_id` / `owner_team_id` split in the same migration that splits `artifacts`.

## Open Questions

- Should comments by other people on an artifact be searchable by its owner in semantic mode
  too, or stay lexical only? The current design keeps them lexical only, to limit what is
  sent to the embeddings endpoint. **Resolved (design review 2026-09-22):** lexical only, as
  proposed.
- Is a per-workspace switch (a team opting out of semantic indexing) needed at launch, or is
  the instance switch enough? **Resolved (design review 2026-09-22):** the instance switch is
  enough at launch; a per-workspace switch waits for a team that asks for one.
- Should agent search be on by default (`CAIRN_SEARCH_AGENTS=true`)? The PR argued yes, because
  an agent token can already list its human's Bin. **Resolved (design review 2026-09-22):** no.
  It is off by default and WARNs when enabled (REQ-14), because it widens what a prompt-injected
  agent can discover. When on, it is scoped to what the human can read.
