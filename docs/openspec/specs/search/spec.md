---
status: accepted
date: 2026-09-22
implements: [ADR-0028]
requires: [SPEC-0002, SPEC-0009]
related: [SPEC-0001, SPEC-0007, SPEC-0008, SPEC-0014, SPEC-0017, SPEC-0020, SPEC-0021, SPEC-0023]
---

# SPEC-0022: Search, Semantic Search, and Export by Tag

## Overview

This capability lets a person, and an agent acting for them, find artifacts by content and
by meaning. Lexical search uses Postgres full-text search. Optional semantic search uses
pgvector and an operator-configured, OpenAI-compatible embeddings endpoint, and it is off by
default. `cairn export --tag` writes matching artifacts to Markdown for external indexers
and repository PRs.

Results are strictly limited to artifacts the caller owns, or shares through a team (under
ADR-0029). This spec realizes ADR-0028.

It builds on SPEC-0002 (artifact core, tags and the Bin query) and SPEC-0009 (access and
expiry). It adds a tool to SPEC-0007's MCP surface, adds commands to SPEC-0008's closed
command list, and adds a search box to SPEC-0001's Bin.

It also leans on these records, linked as front-matter edges: ADR-0023/SPEC-0017 (redaction),
ADR-0026/SPEC-0020 (retention), ADR-0027/SPEC-0021 (receipt metadata) and ADR-0029/SPEC-0023
(teams).

## Requirements

### Requirement: REQ-1 Owner-Scoped Results

A search MUST return only artifacts whose owner is in the caller's reachable set. That set is
the caller's personal workspace, plus each team the caller belongs to (in any role) once
ADR-0029 lands. Under ADR-0029, ownership is `owner_user_id` XOR `owner_team_id`. The scope MUST
be a predicate in the same SQL statement that selects and ranks the rows, in every mode
(lexical, semantic, hybrid, list). It MUST NOT be a filter applied afterwards. As defense in
depth, every returned row MUST also pass ADR-0029's `authorizeRead`.

An artifact owned by any other workspace MUST never be returned, whatever its visibility, even
when the caller holds its link. No response MAY reveal anything about artifacts outside the
set: no counts, no snippets, and no approximate-nearest-neighbour candidates.

An agent token MUST reach exactly the set its human reaches (ADR-0004). Search MUST require
authentication and `artifacts:read`.

#### Scenario: Another user's artifact is never found

- **WHEN** user B owns a `link`-visibility artifact containing "migration plan", user A holds its link, and A searches "migration plan"
- **THEN** B's artifact MUST NOT appear in A's results, in any mode

#### Scenario: Semantic mode honors scope

- **WHEN** semantic search is enabled and user A runs a semantic query whose nearest vector belongs to user B
- **THEN** A's results MUST contain only A's artifacts, even if that means fewer than `limit` results

#### Scenario: Team scope

- **WHEN** a team member searches after ADR-0029 lands
- **THEN** results MAY include artifacts owned by that member's teams, and MUST NOT include artifacts of teams they do not belong to

#### Scenario: Unauthenticated search

- **WHEN** an unauthenticated client calls `GET /v1/search`
- **THEN** the server MUST return `401`

### Requirement: REQ-2 Indexed Content

For each live artifact, the indexer MUST build one search document from:

- the title;
- the tags;
- receipt metadata fields, when present;
- text bodies: `markdown`, `code` and `text/*` media types, each capped at
  `CAIRN_SEARCH_MAX_BODY_BYTES` (default 256 KiB);
- the text members of a bundle, under the same cap per member;
- the bodies of live, non-deleted comments.

The indexer MUST NOT index binary bodies, images, webhook-captured requests or trace span
payloads. A trace MUST contribute only its title and tags.

Indexing MUST run asynchronously after the creating transaction commits. It MUST NOT delay
or fail the create. A comment create, edit or soft-delete MUST re-index that artifact's
comment text.

Comments MUST be indexed lexically only. They MUST NOT be chunked or sent to the embeddings
endpoint.

#### Scenario: Markdown body is searchable

- **WHEN** a markdown artifact containing "thread_ts" is created and the indexer has run
- **THEN** its owner's search for `thread_ts` MUST return it

#### Scenario: Webhook captures are not indexed

- **WHEN** a webhook endpoint captures a request whose body contains "invoice"
- **THEN** a search for "invoice" MUST NOT return the webhook artifact

#### Scenario: Create unaffected by the indexer

- **WHEN** the indexer is stopped and an artifact is created
- **THEN** the create MUST succeed normally, and the artifact MUST become searchable once the indexer resumes

### Requirement: REQ-3 Lexical Search

Lexical search MUST parse the query with `websearch_to_tsquery`, using the configured
text-search configuration (`CAIRN_SEARCH_LANGUAGE`, default `english`). It MUST rank with
`ts_rank_cd` over a weighted vector: title (A), tags and metadata (B), body (C), comments (D).
Each result MUST carry a snippet from `ts_headline`, with matches marked by plain-text
delimiters (`«` and `»`), never HTML.

Lexical search MUST need no Postgres extension.

#### Scenario: Title outranks body

- **WHEN** artifact X has "deploy" in its title and artifact Y has it only in its body
- **THEN** X MUST rank above Y for the query `deploy`

#### Scenario: Phrase and exclusion

- **WHEN** the query is `"rate limit" -redis`
- **THEN** results MUST contain the phrase "rate limit" and MUST NOT contain "redis"

### Requirement: REQ-4 Filters and List Mode

Search MUST accept these filters:

- `tag` (repeatable; every tag must match, the same containment rule as the Bin);
- `share_type`;
- `since` and `until` (on `created_at`);
- `retention` (`ephemeral` or `permanent`, once ADR-0026 lands);
- `schema` (a metadata schema id, such as `cairn.receipt/v1`).

Filters MUST combine with AND. A request with no query text MUST list the matching artifacts
newest first. This is list mode, which gives MCP the listing capability it lacks today.

#### Scenario: Tag list over MCP

- **WHEN** an agent calls `artifact_search` with `tags: ["learning"]` and no query
- **THEN** it MUST receive its human's live artifacts tagged `learning`, newest first

#### Scenario: Receipts pending a human

- **WHEN** a user searches with `schema = cairn.receipt/v1` and the query `approve`
- **THEN** only receipts matching "approve" MUST be returned

#### Scenario: Invalid tag filter

- **WHEN** a filter tag breaks SPEC-0002's tag rules
- **THEN** the server MUST return `400` naming `tag` and the reason

### Requirement: REQ-5 Result Shape and Pagination

Each result MUST carry:

- `id`, `url`, `mcp`, `title`, `share_type`, `tags`, `created_at` and `expires_at`;
- `retention` and `metadata.schema`, when those exist;
- `score`, a mode-specific number meaningful only for ordering;
- `snippet`;
- `matched_in`: a subset of `title`, `tags`, `body`, `metadata`, `comment` and
  `member:<name>`.

`limit` MUST default to 20 and MUST NOT exceed 50. Pagination MUST use an opaque `cursor`. The
total depth reachable through cursors MUST be bounded at 200 results per query, and the
response MUST say `truncated: true` when that bound is hit.

#### Scenario: Limit bounds

- **WHEN** a client requests `limit=500`
- **THEN** the server MUST return `400` naming `limit` with reason `out_of_range`

#### Scenario: Depth bound

- **WHEN** a client pages past 200 results
- **THEN** the last page MUST carry `truncated: true` and no further cursor

### Requirement: REQ-6 Semantic Search Is Opt-In

Semantic search MUST be disabled unless the operator sets `CAIRN_SEARCH_SEMANTIC=true` and
configures an embeddings endpoint. While it is disabled:

- `mode=semantic` MUST return `400` with reason `semantic_search_disabled`;
- `mode=hybrid` MUST do the same;
- the default mode MUST be `lexical`.

The pgvector extension and the chunk table MUST live in an optional migration set, applied
only while semantic search is enabled. An instance that never enables it MUST start and serve
lexical search on a Postgres without pgvector. Enabling semantic search on a Postgres without
the `vector` extension MUST fail startup with an error that names the missing extension.

#### Scenario: Plain Postgres works

- **WHEN** Cairn starts on stock `postgres:16` with semantic search unset
- **THEN** it MUST start, apply no vector migration, and serve lexical search

#### Scenario: Enabled without the extension

- **WHEN** `CAIRN_SEARCH_SEMANTIC=true` and the database lacks the `vector` extension
- **THEN** `cairnd` MUST refuse to start and log that the `vector` extension is required

#### Scenario: Semantic requested while disabled

- **WHEN** a client sends `mode=semantic` on an instance without semantic search
- **THEN** the server MUST return `400` with reason `semantic_search_disabled`

### Requirement: REQ-7 The Embeddings Endpoint

Cairn MUST call an OpenAI-compatible `POST {CAIRN_SEARCH_EMBEDDINGS_URL}/embeddings` with
`{model, input: [...]}` and read `data[].embedding`. The URL, model, dimensions, API key, batch
size and timeout MUST all be operator configuration. None of them MUST come from users.

- The API key MUST come from `CAIRN_SEARCH_EMBEDDINGS_API_KEY` or a `_FILE` variant, and MUST
  NOT be logged.
- The client MUST NOT follow redirects, MUST apply a request timeout, and MUST NOT log request
  or response bodies.
- A returned vector whose length differs from `CAIRN_SEARCH_EMBEDDING_DIMENSIONS` MUST be
  rejected as a configuration error, logged once per batch, and counted.

When the endpoint is unavailable, lexical search MUST keep working. The affected chunks MUST
retry with backoff, and the backlog MUST show in a lag gauge.

#### Scenario: Local model

- **WHEN** the operator points the endpoint at a local OpenAI-compatible server with a 768-dimension model
- **THEN** chunks MUST be embedded, and semantic search MUST return the owner's artifacts

#### Scenario: Endpoint down

- **WHEN** the embeddings endpoint times out
- **THEN** lexical search MUST return results, and `cairn_search_embed_lag_seconds` MUST rise

#### Scenario: Wrong dimensions

- **WHEN** the endpoint returns 1024-dimension vectors while 768 is configured
- **THEN** no vector MUST be stored, and the error MUST name both dimensions

### Requirement: REQ-8 Redaction Before Indexing and Embedding

The indexer MUST index only stored text, which ADR-0023's ingest scanner has already
processed. Before sending any chunk to the embeddings endpoint, Cairn MUST run the ADR-0023
scanner over the chunk again. A chunk with a finding MUST NOT be sent. It MUST be counted in
`cairn_search_chunks_withheld_total`, and its artifact MUST remain lexically searchable
(masked text only).

#### Scenario: Planted secret never leaves

- **WHEN** a body stored before redaction shipped contains a credential, and semantic indexing runs against a stub endpoint
- **THEN** the stub MUST receive no chunk containing that credential, and no snippet MUST show it

### Requirement: REQ-9 Hybrid Ranking

`mode=hybrid` MUST merge the lexical and semantic ranked lists by reciprocal rank fusion:
`score = Σ 1/(k + rank)`, with `k = 60`. Ties MUST break by `created_at` descending, then by
id, so ordering is deterministic. `hybrid` MUST be the default mode when semantic search is
enabled.

#### Scenario: Paraphrase found

- **WHEN** a receipt says "Slack parent event carries thread_ts", and the user searches in hybrid mode for "why replies landed outside the thread"
- **THEN** the receipt MUST appear in the results, although it shares few keywords with the query

### Requirement: REQ-10 The Index Is Deleted with the Artifact

Search documents and chunks MUST reference the internal artifact id with `ON DELETE CASCADE`.
The reaper's expiry, an owner delete, and every tombstoning removal (ADR-0026) MUST remove
them in the same transaction as the artifact row. No search path MAY return an expired or
deleted artifact, including during the window before the reaper runs: the query MUST also
require `expires_at > now()`.

#### Scenario: Vectors deleted on expiry

- **WHEN** an artifact with 12 embedded chunks expires and the reaper removes it
- **THEN** zero `search_chunks` rows and zero `search_documents` rows MUST remain for it, as observed in the same transaction boundary

#### Scenario: Expired but not yet reaped

- **WHEN** an artifact's `expires_at` has passed and the reaper has not yet run
- **THEN** search MUST NOT return it

### Requirement: REQ-11 Reindex and Model Changes

`cairnd search reindex [--semantic] [--owner <id>]` MUST rebuild documents (and, with
`--semantic`, chunks and vectors). Changing the embeddings model or its dimensions MUST
require a semantic reindex. On startup, `cairnd` MUST detect a stored dimension that differs
from the configured one and refuse semantic queries until the reindex completes. It MUST
report this through a readiness message and a metric. Lexical search MUST keep working
throughout.

#### Scenario: Model swap

- **WHEN** the operator changes the model from a 768-dimension one to a 1024-dimension one and restarts
- **THEN** semantic and hybrid queries MUST return `503` with reason `semantic_reindex_required` until `cairnd search reindex --semantic` completes, while lexical queries succeed

### Requirement: REQ-12 Surfaces

The capability MUST be exposed as:

- **MCP** `artifact_search`, with the REQ-4 filters plus `query`, `mode`, `limit` and
  `cursor`. It requires `artifacts:read` and is subject to REQ-14.
- **REST** `GET /v1/search`, with the same parameters as query strings.
- **CLI** `cairn search <query> [--tag] [--type] [--since] [--until] [--mode] [--limit]
  [--json]`. This amends SPEC-0008's closed command list.
- **Web** a search box on the Bin that renders the same results, with snippets escaped.

#### Scenario: CLI JSON output

- **WHEN** a user runs `cairn search "rate limit" --json`
- **THEN** the CLI MUST print the server's result array verbatim, and exit 0 when it is empty

#### Scenario: Bin search box

- **WHEN** a signed-in user searches from the Bin
- **THEN** the Bin MUST show ranked results with snippets and a clear-search control, using the REQ-1 scope

### Requirement: REQ-13 Export by Tag

`cairn export --tag <t> [--tag …] [--since <date>] [--out <dir>]` MUST write one Markdown file
per matching artifact the caller can reach. Each file MUST contain:

- YAML front-matter with `id`, `url`, `title`, `tags`, `created_at`, `expires_at`,
  `actor`, `model`, `channel`, `checksum`, `retention`, and the full `metadata` object for a
  receipt;
- the body: markdown as-is; code in a fenced block with its language; each text member of a
  bundle as a `##` section; a binary as a link.

File names MUST be `<YYYY-MM-DD>-<slug>-<id>.md`, and output MUST be byte-stable across runs
for unchanged artifacts. The export MUST use `GET /v1/search` in list mode plus the existing
body reads, so it inherits REQ-1's scope. It MUST NOT push to any repository.

#### Scenario: Stable re-export

- **WHEN** a user runs `cairn export --tag learning --out out/` twice with no changes in between
- **THEN** the second run MUST leave every file byte-identical

#### Scenario: Receipt front-matter

- **WHEN** an exported artifact is a receipt
- **THEN** its front-matter MUST include the full `cairn.receipt/v1` metadata, and its body MUST be the server-rendered receipt markdown

#### Scenario: Incremental export

- **WHEN** a user runs `cairn export --tag learning --since 2026-09-01`
- **THEN** only artifacts created on or after that date MUST be written

### Requirement: REQ-14 Agent Access Switch

`CAIRN_SEARCH_AGENTS` (default `true`) MUST control whether agent tokens may call search.
When it is `false`, `artifact_search` and agent calls to `GET /v1/search` MUST fail with
`403` and reason `agent_search_disabled`. Human sessions and human tokens MUST be unaffected.
The consent screen line for `artifacts:read` MUST read "Read and search artifacts you can
access" while agent search is enabled.

#### Scenario: Operator withholds agent search

- **WHEN** `CAIRN_SEARCH_AGENTS=false` and an agent calls `artifact_search`
- **THEN** the call MUST fail with reason `agent_search_disabled`, while `artifact_read` still works

### Requirement: REQ-15 Metrics

Cairn MUST add these metrics to SPEC-0014's endpoint:

- `cairn_search_requests_total{mode,outcome}`;
- `cairn_search_duration_seconds{mode}`;
- `cairn_search_index_lag_seconds`;
- `cairn_search_embed_lag_seconds`;
- `cairn_search_embed_failures_total{reason}`;
- `cairn_search_chunks_withheld_total`;
- `cairn_search_documents`;
- `cairn_search_chunks`.

No metric MAY carry the query text, an owner, an actor or an artifact label (SPEC-0014 REQ-5).

#### Scenario: No query text in metrics

- **WHEN** a user searches for "acme contract"
- **THEN** no metric label or value MUST contain that text

### Requirement: Error Handling Standards

Search, indexing and embedding failures MUST be typed errors. Each is rendered in the
ADR-0012 envelope with `details.reason` (for example `semantic_search_disabled`,
`semantic_reindex_required`, `agent_search_disabled` and `out_of_range`), wrapped with context
at each layer, and logged with structured fields. The indexer MUST NOT swallow an extraction
or embedding error. It MUST record the failure against the queue item and retry with bounded
backoff.

#### Scenario: Extraction failure is retried

- **WHEN** the object store is briefly unavailable while indexing an artifact
- **THEN** the queue item MUST be retried with backoff, and the error MUST be logged with the artifact's internal id

### Requirement: Concurrency Safety

The indexer MUST be a bounded worker pool with explicit startup and graceful shutdown on
context cancellation, like the reaper. Queue items MUST be claimed with
`FOR UPDATE SKIP LOCKED`, so several `cairnd` processes can share the work. An artifact
deleted while it is being indexed MUST NOT leave a document or chunk behind: the write MUST
fail on the foreign key, and that failure MUST be treated as a no-op. Tests MUST run with race
detection.

#### Scenario: Delete during indexing

- **WHEN** an artifact is deleted while its chunks are being embedded
- **THEN** no chunk row MUST remain, and the indexer MUST log the skip at debug level without a failure metric

### Requirement: Database Operation Standards

Search queries MUST be parameterized. The query text MUST only ever be a bind parameter to
`websearch_to_tsquery`. Document and chunk writes for one artifact MUST happen in one
transaction that replaces the previous version. Connections MUST be returned to the pool with
timeouts, and search statements MUST carry a statement timeout.

#### Scenario: Injection attempt

- **WHEN** the query is `'); DROP TABLE artifacts; --`
- **THEN** it MUST be treated as literal search text and match nothing harmful

## Endpoint Table

| Endpoint | Method | Purpose | Auth |
|---|---|---|---|
| `/v1/search` | GET | Ranked or list search over the caller's reachable artifacts | Required: `artifacts:read`; agents subject to `CAIRN_SEARCH_AGENTS` |
| `/` (Bin) with `?q=` | GET | Web search results | Required: web session |

## Security Requirements

### Requirement: Authentication & Authorization

Every search surface MUST require authentication. Scope MUST follow REQ-1. Export MUST use the
caller's own credentials and inherit that scope.

#### Scenario: Token for another user

- **WHEN** user A's token searches with a filter that names user B's team
- **THEN** the filter MUST be ignored or rejected, and no B-owned or team-owned artifact outside A's reach MUST be returned

### Requirement: Rate Limiting

`GET /v1/search` and `artifact_search` MUST have a per-principal limit
(`CAIRN_SEARCH_RATE_PER_MINUTE`, default 60) in addition to the per-IP limiter. Exceeding it
MUST return `429` with `Retry-After`.

#### Scenario: Search flood

- **WHEN** an agent issues 200 searches in a minute
- **THEN** requests beyond the limit MUST receive `429` with `Retry-After`

### Requirement: Security Headers

The search responses and the Bin results page MUST carry the existing strict CSP,
`X-Content-Type-Options: nosniff`, `Referrer-Policy`, `X-Frame-Options: DENY` and (over HTTPS)
HSTS. Snippets and titles MUST be HTML-escaped. Highlight delimiters MUST be converted to
`<mark>` only after escaping.

#### Scenario: Script in a snippet

- **WHEN** a matching body contains `<script>`
- **THEN** the Bin MUST render it as text inside the snippet

### Requirement: Request Body Size Limits

Search is a GET. The query string MUST be capped at 2 KiB, and the `q` parameter at 512
characters. Oversize requests MUST return `414` or `400` naming `q`.

#### Scenario: Oversize query

- **WHEN** `q` exceeds 512 characters
- **THEN** the server MUST return `400` naming `q` with reason `too_long`

### Requirement: CSRF Protection

Search is a safe, read-only GET and changes no state, so it needs no CSRF token. It MUST NOT
be implemented as a state-changing request.

#### Scenario: Search is side-effect free

- **WHEN** a cross-site page triggers a GET to `/v1/search` with a victim's cookies
- **THEN** no state MUST change, and the CORS policy MUST NOT expose the response to the foreign origin

### Requirement: Redirect & SSRF Validation

No search endpoint redirects to a user-supplied URL. The only server-side fetch is to the
operator-configured embeddings endpoint. It MUST NOT follow redirects, and it MUST NOT be
configurable by users.

#### Scenario: Endpoint redirect

- **WHEN** the embeddings endpoint answers with a 302
- **THEN** Cairn MUST treat it as a failure and MUST NOT follow it

## Accessibility Requirements

### Requirement: WCAG 2.1 AA and Semantics

The Bin search box MUST have a visible label and live inside a `role="search"` landmark.
Results MUST be a list with each title as a link. The highlight markup MUST use `<mark>`, so
it is exposed to assistive technology.

#### Scenario: Search landmark

- **WHEN** a screen-reader user lists landmarks on the Bin
- **THEN** a "search" landmark containing the labelled search input MUST be present

### Requirement: Icon-Only Controls

The clear-search control and any icon-only submit button MUST carry an `aria-label` ("Clear
search", "Search").

#### Scenario: Clear control label

- **WHEN** a screen reader reaches the clear-search control
- **THEN** it MUST announce "Clear search"

### Requirement: Dynamic Content Regions

A result count or "no results" message updated without a full page load MUST be announced
through an `aria-live="polite"` region.

#### Scenario: Results announced

- **WHEN** an HTMX search swap returns 7 results
- **THEN** "7 results" MUST be announced politely

### Requirement: Keyboard Navigation & Focus Management

`/` MUST focus the search box from anywhere on the Bin, as the TUI keymap does. Enter MUST
submit and Escape MUST clear. After results load, focus MUST remain in the search box, and Tab
MUST move into the results in order.

#### Scenario: Keyboard search

- **WHEN** a keyboard user presses `/`, types a query and presses Enter
- **THEN** results MUST load with focus still in the search box, and Tab MUST reach the first result
