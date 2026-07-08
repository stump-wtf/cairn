---
status: accepted
date: 2026-07-08
decision-makers: joestump
extends: [ADR-0001]
related: [ADR-0004]
---

# ADR-0007: Provenance, Link-Based Access Control, and Default Expiry

## Context and Problem Statement

Three intertwined policies govern every Cairn artifact and annotation, and they
only make sense together. **Provenance** records who or what produced a thing
(`claude · sonnet-4.6` or a human email), over which channel (`via MCP`, `via
CLI`), and when (`captured 2h ago`). **Access control** is link-based capability:
`🔒 you + anyone with link` — possession of the short URL (ADR-0005) grants read,
the owner controls sharing, and changing sharing is an explicit act. **Retention**
is ephemeral-by-default: a visible TTL (`⧗ expires 7d`), owner-adjustable, whose
expiry deletes both body and metadata. How do we design these so the capability
link is the access token, provenance is trustworthy rather than client-asserted,
and expiry genuinely reclaims data — including blobs in object storage?

## Decision Drivers

* **Design mandates** — provenance actor/channel/time visible on every artifact
  *and* annotation; `🔒 you + anyone with link`; `⧗ expires 7d` default, adjustable.
* **Frictionless share loop** — the `cat file | cairn` and agent-drops-a-receipt
  loops must not require the *reader* to hold an account.
* **Trustworthy attribution** — provenance feeds audit and ties to the MCP
  identity mapping (ADR-0004); it must not be spoofable by a client.
* **Real ephemerality** — expiry must reclaim body + metadata, not merely hide
  them, and must cooperate with S3-compatible object storage (ADR-0008).
* **Coherence with the capability-URL** — access and revocation must compose with
  the opaque, unguessable ids of ADR-0005.

## Considered Options

* **Option A — Link-based capability + trusted-channel provenance + ephemeral hard
  delete.** Read is granted by possession of the unguessable URL (no reader
  login); provenance channel/time are server-assigned and immutable; artifacts
  expire by default and expiry hard-deletes body and metadata. This is the design's
  model.
* **Option B — Account/ACL-based access.** Every reader authenticates and access
  is granted per-user via an ACL, alongside the same provenance and retention.
  Rejected: it contradicts `anyone with link`, adds login friction to the core
  share loop, and is especially wrong for agent-produced receipts meant to be
  handed onward.
* **Option C — Public-and-permanent (classic pastebin).** No expiry, world-listed,
  simplest to build. Rejected: no privacy, no ephemerality, and it discards the
  visible-TTL and capability semantics the design is built around.

## Decision Outcome

Chosen option: **"Link-based capability + trusted-channel provenance + ephemeral
hard delete"**, because it matches the design's `🔒 you + anyone with link` and
`⧗ expires 7d` exactly, keeps the pipe-and-share and agent-receipt loops
frictionless (no reader account), makes provenance trustworthy by deriving the
sensitive fields server-side, and makes ephemerality real by actually reclaiming
bodies and metadata.

### (1) Provenance

Recorded on every artifact and every annotation (comment and reaction) as an
immutable, append-only record set at creation:

* **`actor`** — a typed principal, either `human:sam@stump.rocks` or
  `model:claude/sonnet-4.6` (rendered `claude · sonnet-4.6`). For agent actions the
  acting model is captured from the MCP session and bound to the human principal
  from the OAuth grant (ADR-0004).
* **`channel`** — `via Web` | `via CLI` | `via MCP`, **derived server-side** from
  the authenticated surface, never taken from a client claim. This is what makes
  provenance trustworthy: a client cannot forge `via CLI` while acting over MCP.
* **`captured_at`** — a server timestamp at creation, rendered relative
  (`captured 2h ago`, `via mcp · 1d`).

Provenance is never edited; a correction is a new record, not a mutation of an old
one. For on-behalf-of actions the record carries **both** the model actor and the
human principal — the human owns the artifact (relevant to access below), while the
display foregrounds the model, exactly as ADR-0004's subject-vs-actor mapping
describes.

### (2) Access control — link-based capability

* The public capability-URL (ADR-0005) **is** the read capability: anyone
  presenting a valid id may read, with no reader account. This is the `anyone with
  link` half.
* The `you` half: the creating human principal is the **owner** and the only party
  who may mutate policy (sharing, expiry). Mutation requires authenticated
  ownership, not mere possession of the link.
* **Read is the only thing a bare link grants.** Commenting and reacting require an
  authenticated identity (web login, or a CLI/MCP OAuth token), because every
  annotation carries a provenance `actor` (above) and that actor must be real. So a
  pure link-holder can read but must sign in to annotate. Whether an annotator must
  belong to the artifact's workspace or may be any authenticated Cairn user is a
  policy knob; the invariant is that annotation is never anonymous. We record this
  as a deliberate decision where the brief is silent on anonymous annotation — the
  alternative (pseudonymous/anonymous reactions) was rejected to keep provenance
  trustworthy.
* **Changing sharing is explicit.** New artifacts default to `you + anyone with
  link` (an unlisted capability). The owner can explicitly change it. Two concrete
  primitives fall out of the capability model: **rotating the id** mints a new
  capability and invalidates the old link (the revoke-a-leaked-link operation), and
  **restricting to owner-only** unshares. Because access is defined by link
  possession, "revoke a link" is precisely "rotate the id" (ADR-0005 forbids
  reusing a retired id within its grace window, so the old link cannot later
  resolve to something else).
* **No enumeration.** Ids are unguessable (ADR-0005) and resolution returns a
  uniform 404 for unknown and unauthorized/expired ids alike, so the link is the
  entire secret and probing leaks nothing.
* **Agents inherit, never exceed, the human's reach (ADR-0004).** An agent holding
  `artifacts:read` reads exactly what its human principal can reach; artifacts it
  creates are owned by the human and default to `you + anyone with link`. Agents get
  no `sharing:manage` scope, so they cannot broaden sharing or delete others' work.

### (3) Retention / expiry

* **Ephemeral by default.** `expires_at = created_at + 7 days`, shown everywhere
  (`⧗ expires 7d`, `expires in 5d`) in headers and CLI output.
* **Owner-adjustable.** The owner may extend, shorten, or set no-expiry (subject to
  any workspace cap). Like sharing, changing the TTL is an explicit owner action.
* **Expiry hard-deletes.** Past `expires_at`, Cairn deletes both the metadata
  (PostgreSQL rows: artifact, annotations, provenance) and the body. Hard delete,
  not soft — ephemerality must be real, and the id thereafter returns the same
  uniform 404 as a never-existed id.
* **Mechanism — application-authoritative reaper with a storage backstop.** A Cairn
  reaper job is the source of truth: past `expires_at` it removes the DB rows and
  issues the blob delete. Because bodies are content-addressed and may be
  deduplicated across artifacts (ADR-0008), blob deletion is **reference-counted** —
  the artifact and its annotations are deleted immediately, but the underlying blob
  is garbage-collected only when no live artifact still references its content hash.
* **Object-storage lifecycle interaction.** Two layers. Primary deletion is
  application-driven (the reaper, with refcounting). The S3/Garage/MinIO **object
  lifecycle rules** are a conservative *backstop* that sweeps truly orphaned objects
  — blobs whose refcount reached zero but whose delete failed, plus upload debris.
  The lifecycle TTL is set **longer than the maximum artifact TTL** so it can never
  race ahead of a live reference; it reaps only orphans, never live bodies.
* Annotations and streams follow their artifact: a trajectory or webhook expires on
  the same TTL as any artifact, and its annotations expire with it.

### Consequences

* Good, because it matches the design exactly and keeps the core loop frictionless
  — a reader needs only the link, so `cat file | cairn` and agent receipts "hand
  onward" cleanly.
* Good, because ephemerality is real: refcounted deletion plus the conservative
  lifecycle backstop actually reclaims bodies and metadata rather than hiding them.
* Good, because deriving `channel`/`captured_at` server-side and making provenance
  immutable yields an audit trail that agents (ADR-0004) cannot forge, and id
  rotation gives a concrete revoke-a-leaked-link primitive.
* Bad, because a capability-URL is only as safe as its handling — a leaked link
  grants read until rotation or expiry; mitigated by unguessable ids (ADR-0005),
  the short default TTL, and uniform 404s.
* Bad, because hard delete is unrecoverable: an accidental early expiry or a
  wrong TTL loses data with no undo; mitigated by making TTL changes explicit and
  showing countdowns in every surface.
* Neutral, because requiring authentication to annotate means a pure link-holder
  reads but cannot react/comment without signing in — a deliberate trade of some
  spontaneity for trustworthy provenance.
* Neutral, because refcounted blob GC plus a tuned lifecycle backstop adds
  operational machinery (a refcount table, the reaper, carefully set lifecycle
  rules).

### Confirmation

Provenance tests assert every artifact/annotation write persists
`actor` + `channel` + `captured_at`, that `channel` is server-derived (a client
attempting to assert `via CLI` while authenticated over MCP is overridden to
`via MCP`), and that no API path can edit an existing provenance record. Access
tests assert a valid link reads without authentication; an unauthenticated
annotate is rejected; a non-owner cannot change sharing or TTL; and rotating the id
invalidates the old link while the new one resolves. Retention tests assert a
past-`expires_at` artifact returns a uniform 404 with its rows gone, that its blob
is deleted only once its content-hash refcount hits zero (a blob shared by a still
live artifact survives), and — as an integration check against object storage
(ADR-0008) — that the lifecycle backstop is configured longer than the maximum
artifact TTL and reaps only orphans. These cross-check the agent actor/channel
mapping (ADR-0004) and the uniform-404 / id-rotation behavior (ADR-0005).
