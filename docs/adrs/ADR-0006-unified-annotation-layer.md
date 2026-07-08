---
status: accepted
date: 2026-07-08
decision-makers: joestump
extends: [ADR-0001]
related: [ADR-0002]
---

# ADR-0006: Unified Annotation Layer — Reactions & Comments with Typed Anchors

## Context and Problem Statement

Every Cairn share type — markdown, code, image, generic file, bundle, webhook, and
trajectory — needs the *same* two social affordances: emoji **reactions** and
threaded **comments**. But each type anchors those annotations to a different kind
of location: a markdown block or bullet, a code line or selection, a pixel region on
an image, a webhook request, a trajectory turn / tool-call / span, or the whole
artifact. The design also imposes a deliberate asymmetry — webhook requests are
*reactable but not comment-threaded*. How do we model **one** annotation subsystem
that serves all current and future share types, keeps anchors stable, and surfaces
counts cheaply in the Bin and in artifact headers?

## Decision Drivers

* **Uniformity across surfaces.** Reactions and comments must behave identically
  whether created from the web viewer, the CLI, or an agent over MCP (ADR-0003).
  One subsystem, three thin adapters — not three implementations.
* **Extensibility without migrations.** A new share type (trajectories were added in
  the design's turn 7, and more will follow) must be able to introduce a new anchor
  kind by registering it in the viewer registry (ADR-0002), *not* by adding a table
  or a column.
* **Anchor stability.** An anchor created today must still resolve to the same
  location tomorrow. Bodies are content-addressed and immutable (ADR-0008), so the
  substrate is stable; the render-time identifiers must be deterministic too.
* **The webhook asymmetry.** The rule "webhook requests are reactable but not
  comment-threaded" must be enforced structurally, not by convention, and must be
  expressed as a capability of the share type rather than special-cased code.
* **Cheap aggregation.** The Bin lists many artifacts with `💬 2 · 👀 3`; headers
  show `6 comments · 16 reactions`, `2 pins · 8 reactions`. Per-anchor emoji tallies
  and "did I react" must render without scanning the whole table.
* **Idempotent reactions.** An actor reacting 🔥 to the same anchor twice is a
  no-op / toggle, not two rows.

## Considered Options

* **Option A — Polymorphic anchor `{artifact_id, anchor_type, anchor_ref}`.** Two
  tables (`reactions`, `comments`) that embed a shared anchor shape: an
  `anchor_type` discriminator plus a JSONB `anchor_ref` holding the type-specific
  locator. The set of legal `anchor_type` values and the schema of each `anchor_ref`
  are owned by the viewer registry (ADR-0002).
* **Option B — Per-type annotation tables.** `markdown_comments`,
  `code_comments`, `image_pins`, `trajectory_reactions`, … — one table (or table
  pair) per share type, each with a strongly typed anchor column.
* **Option C — Normalized anchor entity.** A separate `anchors` table (one row per
  distinct location) that annotations foreign-key into, so an anchor is a first-class
  addressable row shared by all reactions/comments landing on it.

## Decision Outcome

Chosen option: **"Polymorphic anchor `{artifact_id, anchor_type, anchor_ref}`"
(Option A)**, because it gives us exactly one reaction subsystem and one comment
subsystem for all share types, lets ADR-0002's registry add new anchor kinds as
data rather than schema, and keeps every query scoped to a single `artifact_id`
so aggregation stays cheap. Per-type tables (Option B) would multiply the write
paths, the API handlers, and the MCP tool surface by the number of share types and
turn "add a share type" into a schema migration — the opposite of the extensibility
ADR-0002 demands. The normalized anchor entity (Option C) buys deduplicated anchor
rows we do not need (annotations already carry their anchor cheaply as JSONB) at the
cost of an extra join on every read and an upsert dance on every write.

The subsystem is two tables sharing one embedded anchor:

```
-- The polymorphic anchor is three columns, embedded (not a separate table):
--   artifact_id  → the artifact being annotated
--   anchor_type  → registry-owned discriminator (see ADR-0002)
--   anchor_ref   → JSONB, a type-specific locator (schema owned by the registry)
--   anchor_key   → canonical text serialization of anchor_ref, for uniqueness/index

CREATE TABLE reactions (
  id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  artifact_id   BIGINT      NOT NULL REFERENCES artifacts(id) ON DELETE CASCADE,
  anchor_type   TEXT        NOT NULL,
  anchor_ref    JSONB       NOT NULL DEFAULT '{}',
  anchor_key    TEXT        NOT NULL,           -- canonical(anchor_ref)
  emoji         TEXT        NOT NULL,           -- unicode grapheme, e.g. '🔥'
  actor_id      BIGINT      NOT NULL REFERENCES actors(id),
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (artifact_id, anchor_type, anchor_key, emoji, actor_id)
);
CREATE INDEX ON reactions (artifact_id, anchor_type, anchor_key);

CREATE TABLE comments (
  id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  artifact_id   BIGINT      NOT NULL REFERENCES artifacts(id) ON DELETE CASCADE,
  anchor_type   TEXT        NOT NULL,
  anchor_ref    JSONB       NOT NULL DEFAULT '{}',
  anchor_key    TEXT        NOT NULL,
  parent_id     BIGINT      REFERENCES comments(id) ON DELETE CASCADE, -- thread root = NULL
  actor_id      BIGINT      NOT NULL REFERENCES actors(id),
  body          TEXT        NOT NULL,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  edited_at     TIMESTAMPTZ,
  deleted_at    TIMESTAMPTZ                     -- soft delete keeps thread structure
);
CREATE INDEX ON comments (artifact_id, anchor_type, anchor_key);
CREATE INDEX ON comments (parent_id);
```

### The anchor model

`anchor_type` is an open, registry-governed string; `anchor_ref` is a small JSONB
locator whose shape is defined per type. The canonical set at v1:

| `anchor_type`         | Applies to        | `anchor_ref` shape                                   |
|-----------------------|-------------------|------------------------------------------------------|
| `artifact`            | all               | `{}` (the whole artifact)                            |
| `md_block`            | markdown          | `{"block_id":"b_3f2a"}`                              |
| `md_bullet`           | markdown          | `{"block_id":"b_9c1","path":[2,0]}`                  |
| `code_line`           | code              | `{"line":42}`                                        |
| `code_range`          | code              | `{"start":40,"end":47}`                              |
| `text_selection`      | markdown, code    | `{"start":1201,"end":1240,"quote":"…"}`             |
| `image_region`        | image             | `{"x":0.42,"y":0.31}` (optional `w`,`h` for a box)  |
| `webhook_request`     | webhook           | `{"request_id":"req_7Kx…"}`                          |
| `trajectory_turn`     | trajectory        | `{"span_id":"sp_a1…"}`                               |
| `trajectory_toolcall` | trajectory        | `{"span_id":"sp_b2…"}`                               |
| `trajectory_span`     | trajectory        | `{"span_id":"sp_c3…"}`                               |

The registry entry for each share type (ADR-0002) declares two capability sets:
which `anchor_type` values accept **reactions** and which accept **comments**. This
is where the webhook rule lives, as data rather than a code branch:

```
webhook.reactions  = { artifact, webhook_request }
webhook.comments   = { }        -- no comment anchors at all: discussion happens
                                --  on the artifacts the hook produces, not here
markdown.reactions = { artifact, md_block, md_bullet }
markdown.comments  = { artifact, text_selection }
code.reactions     = { artifact, code_line, code_range }
code.comments      = { artifact, code_line, text_selection }
image.reactions    = { artifact, image_region }
image.comments     = { artifact, image_region }   -- the "pin"
trajectory.reactions = { artifact, trajectory_turn, trajectory_toolcall }
trajectory.comments  = { artifact, trajectory_span, text_selection }
file.reactions     = { artifact }
file.comments      = { artifact }
```

The core service validates every write against the annotated artifact's registry
entry: reject a reaction/comment whose `anchor_type` is not in the corresponding
capability set for that share type, and reject an `anchor_ref` that fails the type's
locator schema. Because the whole matrix is registry data, adding a share type in
ADR-0002 automatically defines its annotation surface with no change to this table.

### How anchors stay stable

Anchors resolve reliably because the thing they point into does not move:

* **Immutable bodies.** Artifact bodies are content-addressed blobs (ADR-0008);
  editing a body means minting a *new* artifact with a *new* id, never mutating an
  existing one. An anchor is therefore always paired with a byte-stable substrate.
* **Deterministic render ids.** Markdown `block_id`s are derived deterministically
  at ingest by hashing the block's structural position and content, so the same body
  renders the same block ids on every surface and every request. Bullets extend a
  block id with an ordinal `path`. Code lines are intrinsic to the immutable body,
  so a `line` number is a permanent coordinate; `code_line` may additionally carry a
  short hash of the line's text so a client can detect it is annotating against a
  stale render.
* **Resolution-independent image pins.** `image_region` uses normalized fractional
  coordinates (0..1), so a pin survives display scaling and thumbnailing.
* **Capture-time stream ids.** Webhook `request_id`s (ADR-0010) and trajectory
  `span_id`s (ADR-0009) are assigned once, at capture, and never reissued.
* **Self-describing selections.** `text_selection` stores character offsets into the
  canonical body text *plus* the quoted substring, so a selection can be
  re-highlighted and verified (and gracefully shown as "context changed" if the
  quote no longer matches — a safeguard, not an expected case given immutability).

### Count aggregation

Two tiers, because the Bin and the viewer have different access patterns:

* **Artifact-level rollups for the Bin.** `artifacts` carries denormalized counters
  — `comment_count`, `reaction_count`, and `pin_count` (image-region reactions +
  pinned comments) — updated in the *same transaction* as the annotation
  insert/soft-delete (or via a trigger). Listing the Bin then needs no per-row
  subquery: the `💬 2 · 👀 3` on each row and the `6 comments · 16 reactions`
  header read straight off the artifact.
* **Per-anchor tallies at view time.** When a single artifact is opened, its emoji
  tallies (`🔥 7 · 🙏 3 · 🎉 5`), the per-anchor grouping, and the viewer's
  "did *I* already react" flags are computed with a `GROUP BY (anchor_key, emoji)`
  over that one artifact's reactions. This is bounded to a single artifact's
  annotation set and backed by the `(artifact_id, anchor_type, anchor_key)` index,
  so it stays cheap without materializing per-anchor counters.

Reactions are idempotent via the `UNIQUE (artifact_id, anchor_type, anchor_key,
emoji, actor_id)` constraint: a repeated react is an upsert no-op, and "un-react" is
a delete of that row. `anchor_key` is the canonical (sorted-key, whitespace-free)
serialization of `anchor_ref`, computed by the service so the unique index and the
grouping key are stable regardless of JSON field ordering.

### Consequences

* Good, because there is exactly **one** reactions path and **one** comments path
  for every share type, so the REST endpoints (ADR-0012), the MCP tools (ADR-0004),
  and the web viewers (ADR-0011) each implement annotations once.
* Good, because a new share type ships its full annotation surface as registry data
  (ADR-0002) — a locator schema plus two capability sets — with **no schema
  migration** to this subsystem.
* Good, because every read is scoped to a single `artifact_id` and backed by a
  composite index, keeping per-anchor aggregation and viewer rendering cheap.
* Good, because immutable content-addressed bodies (ADR-0008) plus deterministic
  render ids make anchors durable without a re-anchoring engine.
* Bad, because JSONB `anchor_ref` sacrifices column-level typing: locator shape is
  enforced in the service layer and by a canonicalization step, not by the database,
  so a registry bug could persist a malformed anchor. Mitigated by validating
  against the registry schema on write and by round-trip tests.
* Bad, because denormalized counters can drift if a write path forgets to update
  them; mitigated by funneling all annotation writes through the core service (or a
  trigger) and a periodic consistency check.
* Neutral, because comment threading is deliberately shallow — `parent_id` gives one
  level of reply nesting per anchor, matching the design's margin/panel threads
  rather than deep forum trees.

### Confirmation

* A per-share-type **anchor capability matrix test** asserts that each registry entry
  accepts exactly its declared reaction/comment anchor types and rejects all others —
  including the explicit invariant that a `comment` on any webhook anchor is refused
  while a `reaction` on `webhook_request` is accepted.
* A **locator schema test** feeds valid and malformed `anchor_ref` payloads per
  `anchor_type` and asserts service-layer validation and canonicalization.
* An **idempotency test** issues duplicate reactions and confirms a single row plus
  a working un-react, exercising the unique constraint on `anchor_key`.
* A **count-consistency test** creates/soft-deletes annotations and asserts the
  denormalized `comment_count`/`reaction_count`/`pin_count` on the artifact match a
  fresh aggregate, and that Bin rows and headers render those counters.
* A **cross-surface parity test** creates a reaction over MCP and a comment over the
  REST API against the same anchor and confirms both render identically in the web
  viewer, satisfying ADR-0003.
