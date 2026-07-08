---
status: accepted
date: 2026-07-08
decision-makers: joestump
extends: [ADR-0001]
related: [ADR-0003, ADR-0008]
---

# ADR-0005: Short Opaque Identifiers and the URL Scheme

## Context and Problem Statement

Every Cairn artifact is addressed by a short, opaque, URL-safe public identifier
that appears in the human web URL (`cairn.sh/9qz1a`), the trajectory sub-path
(`cairn.sh/run/8kd2p`), and the agent handle (`mcp://cairn/hook/<id>`). Because
Cairn's access model is link-based capability — possession of the URL grants read
(ADR-0007) — these identifiers *are* secrets. How do we design the id (its
alphabet, length, and entropy versus guessability), handle collisions, decide
whether the share type is encoded in the path, and relate the public id to the
internal storage key and the SHA-256 content hash (ADR-0008)?

## Decision Drivers

* **Capability-URL security (ADR-0007)** — possession equals read access, so ids
  must be unguessable enough that enumeration is infeasible. This is the dominant
  driver.
* **Short & shareable** — the design shows five-character examples (`9qz1a`,
  `8kd2p`); ids must be copy-pasteable, terminal-friendly, and pleasant in a URL.
* **Opaque** — no PII, no sequence, no ordering, no counts, no leak of internal
  structure or of whether two artifacts share content.
* **Consistent across surfaces (ADR-0003)** — the *same* id token must work in the
  web URL, the `◆ mcp` handle, and CLI output.
* **Decoupled from storage (ADR-0008)** — content addressing uses SHA-256; the
  public id must not be the content hash or the internal primary key.
* **House stack** — public ids are **base62**.

## Considered Options

* **Option A — Random base62, standardized length, opaque, with collision-retry.**
  Draw cryptographically random bytes, encode base62, check uniqueness, retry on
  the (astronomically rare) conflict. The id carries zero information.
* **Option B — Obfuscated sequential ids (Hashids/Sqids or base62 of a counter).**
  Very short and dense, but reversible/enumerable — Hashids is explicitly *not* a
  security boundary — and it leaks total counts and creation order. Fatal for a
  capability-URL model.
* **Option C — UUIDv4 or ULID rendered base62.** Strong entropy, but long (22+
  chars), visually heavy, and unlike the compact `9qz1a` aesthetic; ULID is also
  time-ordered, leaking creation time and partial ordering.
* **Option D — Truncated content hash (base62 of a SHA-256 prefix).** Reuses the
  ADR-0008 addressing, but ties the public id to the body: identical bodies would
  collide into one URL, revealing that two people shared the same bytes and
  enabling confirmation-of-file attacks.

## Decision Outcome

Chosen option: **"Random base62, standardized length, opaque, with
collision-retry"**, because it is the only option that satisfies the capability-URL
unguessability requirement while staying short and human-friendly; it leaks
nothing about ordering, counts, or content; and it cleanly decouples the public
namespace from internal storage keys and content hashes (ADR-0008).

**Alphabet.** Base62 (`0-9 A-Z a-z`), case-sensitive, matching the house stack and
the design's mixed-case examples. Case sensitivity roughly doubles the per-char
keyspace versus base36, keeping ids short. The trade-off — base62 is slightly less
robust to spoken transcription — is acceptable because capability-URLs are
copy-pasted, not dictated.

**Length and entropy — the honest tension.** The design's `9qz1a` is five base62
characters ≈ 62⁵ ≈ 916 million ≈ **~29.7 bits**, which on its own is too weak
against a determined scanner. The brief is prescriptive about the *aesthetic*
(short) but silent on exact length, so we standardize on a **default of 8 base62
characters ≈ 62⁸ ≈ 2.18 × 10¹⁴ ≈ ~47.6 bits** of entropy, treating the five-char
values in the brief as illustrative short forms of the same compact style. Entropy
is the *primary* defense; it is backstopped by defense-in-depth: server-side rate
limiting on id resolution, a **uniform 404** for unknown *and* unauthorized ids so
probing yields no signal, and the short default 7-day TTL (ADR-0007) that shrinks
the live keyspace an attacker could ever hit. The length is a tunable policy: if
occupancy of the keyspace ever grew non-trivial we would lengthen new ids to keep
random guesses missing. We accept this as a pragmatic compromise between the
design's short-id aesthetic and pure-secret strength, and record it plainly rather
than pretend five characters are cryptographically sufficient.

**Type is not encoded in the id; the path prefix carries presentation.** The id
itself is type-agnostic random bytes — it resolves to whatever artifact it names,
and the viewer is chosen from the stored share type via the registry (ADR-0002).
The *path*, however, distinguishes trajectories as a human-facing affordance,
exactly as the design mandates:

| Surface | Scheme | Example |
|---------|--------|---------|
| Web, default artifact | `cairn.sh/<id>` | `cairn.sh/9qz1a` |
| Web, trajectory | `cairn.sh/run/<id>` | `cairn.sh/run/8kd2p` |
| Agent handle, artifact | `mcp://cairn/<id>` | `mcp://cairn/9qz1a` |
| Agent handle, webhook stream | `mcp://cairn/hook/<id>` | `mcp://cairn/hook/7m3xq` |
| Agent handle, trajectory stream | `mcp://cairn/run/<id>` | `mcp://cairn/run/8kd2p` |

Markdown, code, image, file, and bundle all live at the bare `cairn.sh/<id>` and
are disambiguated by their stored type, not the URL. Trajectories get the `/run/`
sub-path and webhooks get the `mcp://cairn/hook/` handle because the design calls
for those specific, legible forms — they are routing and presentation choices, not
bits inside the secret. The `◆ mcp` affordance beside any web URL emits the
corresponding `mcp://` handle for the same id.

**One id, every surface.** There is exactly one public id per artifact, reused
verbatim across web, MCP, and CLI; only the surrounding scheme/prefix changes.
This is what makes ADR-0003's triple-surface parity concrete at the URL level — a
human and an agent name the same thing with the same token.

**Collision handling.** Generation is generate → attempt an atomic unique insert
(a `UNIQUE` constraint on `public_id`) → on the rare conflict, regenerate and
retry. At the low keyspace occupancy we target, collisions are vanishingly
unlikely but are nonetheless resolved deterministically at write time. A retired
id (expired, ADR-0007) is **not** reused within TTL-plus-grace, so a stale link
can never silently resolve to a different, newer artifact.

**Relationship to storage keys and content hashes (ADR-0008).** Three distinct
namespaces, deliberately non-overlapping:

1. **`public_id`** — the random base62 capability handle; the only one in URLs.
2. **Internal primary key** — a database key (e.g. bigint/UUID); never exposed.
3. **Content address** — SHA-256 of the body, the object-storage/dedup key
   (ADR-0008); never exposed.

Resolution walks `public_id → artifact row → content hash → blob`. Because the
public id is independent of the content hash, bodies can be content-addressed and
deduplicated internally without two shares that happen to have identical bytes
ever sharing a URL or leaking their equivalence, and the id reveals nothing about
where or how the body is stored.

**Reserved prefixes.** Route words (`run`, `hook`, `api`, `settings`,
`.well-known`, and future additions) are reserved and excluded from the generator,
so an id can never collide with a route prefix. Given randomness this is a
belt-and-suspenders validation, but it is enforced.

### Consequences

* Good, because ids are unguessable-enough, short, and fully opaque — leaking no
  order, count, content, or storage detail — and the same token is consistent
  across web, CLI, and MCP (ADR-0003).
* Good, because the `/run/` sub-path and `mcp://cairn/hook/<id>` handles stay
  human- and agent-legible without encoding type into the secret, so presentation
  can evolve without touching id generation.
* Good, because decoupling the public id from the SHA-256 content hash (ADR-0008)
  lets storage dedup freely while preserving privacy of what was shared.
* Bad, because there is a genuine tension with the brief's five-char examples
  (~30 bits): we standardize on a longer 8-char default and lean on rate limiting,
  uniform 404s, and short TTLs as defense-in-depth — a compromise, not pure secret
  strength.
* Bad, because random ids are not time-ordered, so the Bin listing (ADR-0003) must
  sort by a stored `created_at`, never by id.
* Neutral, because the reserved-prefix list must be maintained as new top-level
  routes are added.

### Confirmation

Unit tests cover the generator: base62 alphabet only, correct default length,
sufficient entropy, reserved-word exclusion, and the collision-retry path
(exercised by injecting a forced first-attempt conflict). A property test asserts
that an unknown id and an unauthorized/expired id both return an identical 404
(indistinguishability). Routing tests assert `cairn.sh/<id>`, `cairn.sh/run/<id>`,
and the `mcp://cairn/...` handles all resolve to the same underlying artifact where
applicable, and that the `◆ mcp` affordance emits the correct handle for a given
id. A security test asserts a `public_id` never equals its artifact's content hash
or internal primary key. Cross-checks are shared with the storage/content model
(ADR-0008) and the capability access model (ADR-0007).
