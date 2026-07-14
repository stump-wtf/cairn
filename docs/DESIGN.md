# Cairn — Design Brief

> Source of truth for the Cairn product design. Distilled from the Claude design
> canvas `Cairn.dc.html` (an "AI-native artifact sharing" exploration, turns 1–7).
> This brief is the shared context for the ADRs (`docs/adrs/`) and specs
> (`docs/openspec/specs/`). It describes the *product*, not the implementation —
> the ADRs make the architectural decisions and the specs formalize behavior.

## Vision

**Cairn is an AI-native artifact-sharing service** — a pastebin / gist / requestbin
reimagined for the agent era. Humans and agents both produce ephemeral, shareable
artifacts; Cairn gives every artifact a short URL, a consistent viewer, provenance,
reactions, and comments, and makes the *same* artifact readable and writable by
agents over MCP.

The one-line pitch from the design: *"pbcopy for cairn — pipe or pass anything in,
get a shareable, agent-native link back. Same auth as your agent."*

A "cairn" is a trail marker — a small stack left to guide whoever comes next. The
product framing: **"what an agent has dropped for you"** … **"the human hands it
onward — to people and to agents."**

## Personas & the core loop

- **Human poster** — pipes output from a shell (`cat file | cairn`), adds files
  from the CLI, or lands artifacts via their agent. Wants a nice link to share.
- **Agent (Claude et al.)** — authorized over MCP OAuth to read, create, comment,
  and react on the human's behalf. Drops artifacts ("receipts") for the human.
- **Human reader / reviewer** — opens the link in the web viewer, reads, reacts,
  comments, and hands it onward.

Core loop: **index → read / react / comment → share**, symmetric across humans
(web + CLI) and agents (MCP).

## Core concepts

- **Artifact** — a single shared thing. Has: an id, a **share type**, a body
  (bytes / structured payload), metadata, provenance, access policy, expiry, plus
  an annotation stream (reactions + comments). Addressed by a short URL.
- **Share type** — the *kind* of artifact, which drives its viewer, its metadata
  panel, and its annotation affordances. The type set is **extensible** (a new
  type — trajectories — is the whole point of turn 7).
- **Bundle** — one artifact that contains many files (mixed media), browsed in a
  tabbed viewer; agents read the same files over MCP.
- **Workspace** — a person's/team's space of artifacts ("your Cairn workspace"),
  with people and access.
- **The Bin** — the listing/index of artifacts ("the same listing" in web and TUI).

## Share types (the type registry)

Badge codes seen in the design: `MD`, `PY`/code, `IMG`, `FILE`/`GZ`, `HK` (webhook),
`TRJ` (trajectory), plus multi-file **bundle**.

| Type | Badge | Viewer & affordances |
|------|-------|----------------------|
| Markdown | `MD` | Rendered markdown with TOC. React under blocks and to the left of bullets; select text → comment in the right margin. Example: `checkout-web-audit.md`. |
| Code | `PY` (lang) | Syntax-highlighted source with line numbers and a symbol outline. Comment a single line or a selection. Example: `parse_ledger.py`. |
| Image | `IMG` | Image with **pins**: drop a pin to anchor a comment to a region; react below. Example: `Movies.png` — "Jellyfin library banner mock". |
| File (generic) | `FILE`/`GZ` | Non-previewable blobs: size, gzip note, **checksum**, download; react & discuss. Example: `staging-db-dump.sql.gz`, 42.7 MB, "not previewable". |
| Bundle | — | Many files in one share; humans browse a **tabbed viewer**, agents read the files over MCP. Mixed media welcome. Created via `cairn add f1 f2 …`. |
| Webhook | `HK` | **Live** requestbin/inspector: an endpoint agents point at; requests stream in; highlighted JSON; **react on a single request** (no comment thread); status mix; agents read the same stream over MCP. |
| Trajectory | `TRJ` | **A whole agent run, shared** (turn 7). OTel-style **span waterfall** pinned up top + a readable **activity stream** below. React on any turn or tool call; comment on any span or any text selection. |

### Trajectory share type (the new one — turn 7)

The flagship addition. A trajectory is a captured agent run.

- **Header:** `cairn ▸ TRJ  checkout-web-audit · run · 11 spans · 34.2s · [link ◆ mcp
  cairn.sh/run/<id>] [copy] [Share]`.
- **Call-trace waterfall** pinned at the top: OTel-style spans nested by depth, with
  a time ruler (`0s … 34.2s`). Span **categories** (color-coded legend):
  `reason · exec · read · net · write · search · plan · tool · analyze · test · fix · fail · meta`\n  (recommended set; any non-empty string is accepted and rendered with a neutral default\n  color). A **sub-agent** appears as a nested group
  (e.g. "advisory lookup" containing `web_search`, `web_fetch`, `summarize`).
  Click any span to **jump to & expand** its event in the stream below.
- Example spans: `reason · plan the audit` (2.2s) → `bash npm ls --all` (2.9s) →
  `read package.json` → `reason · rank by severity` → `grep legacy-jwt · img-resize`
  → `sub-agent advisory lookup` (10.6s: `web_search CVE…`, `web_fetch nvd.nist.gov`,
  `summarize risk`) → `read yarn.lock` → `reason · compose findings` →
  `write checkout-web-audit.md` → `reason · final summary`.
- **Activity stream** below: the run as a readable timeline — the human PROMPT
  ("Sam · started the run"), each reasoning turn, each tool call (expandable args +
  output, e.g. `bash npm ls --all · exit 0`), the nested sub-agent, and the `write`
  that **produces `checkout-web-audit.md`** — which links to the existing markdown
  share, tying artifacts together (a run produces artifacts).
- **Right metadata panel ("RUN"):**
  - **Provenance:** `Claude · sonnet-4.6 · via MCP · captured 2h ago · expires in 7d`.
  - **Run stats:** wall time `34.2s`, `11` spans, `8` tool calls, `48.1k` tokens.
  - **Time by category:** bar/breakdown (`reason 13.7s · net 10.8s · exec 4.8s ·
    read 3.4s · write 2.7s`).
  - **Comments** thread.
- Reactions on turns/tool calls (🔥, 🙏, 🎉, 👀 …); text-selection comments land in
  the panel. "select text or use ＋ to react on a turn."
- **Try-next ideas** noted in the design (future, not v1): token-cost lane,
  run-vs-run diff, an errored run with a red span, filter stream by category.

## Surfaces

All surfaces share **one app shell**: *"same size, same header (logo · type · one
URL control · share), same collapsible metadata + comments panel."*

### Web app shell

- Consistent chrome for **every** share type: `cairn` logo/wordmark, a **type**
  badge, the artifact title, **one URL control** (the short link, with a `copy`
  action and an `◆ mcp` affordance), a **Share** button, and a **collapsible**
  right-hand metadata + comments panel.
- Body area is type-specific (markdown / code / image / file / bundle tabs /
  webhook inspector / trajectory waterfall+stream).
- **The Bin (web):** the artifact listing — rows with type badge, title, provenance
  (`claude · via mcp · 1d · 💬 2 · 👀 3`), reaction/comment counts.

### CLI (`cairn`)

- **`cat file | cairn`** — "pbcopy for cairn": pipe or pass anything in, get a
  shareable, agent-native link back.
- **`cairn add f1 f2 f3 …`** — push many artifacts at once as a **bundle**
  (mixed media): shows per-file upload progress, then `✓ bundle → cairn.sh/<id>
  (copied to clipboard)`, with summary `N files · <size> · ⧗ expires 7d ·
  🔒 you + anyone with link`.
- **`cairn ls`** — the Bin as a **TUI**: keyboard-driven
  (`↑/k up · ↓/j down · / filter · enter open · s share · q quit`).
- Auth: `✓ authorized as sam@stump.rocks · via MCP OAuth` — the CLI shares the
  same auth as the agent.
- A **terminal / dev-minimal** aesthetic is the retained visual base (design note:
  "retained base — the terminal direction you preferred (1b)").

### MCP (agent interface)

- Agents **read artifacts, create & push new artifacts, and comment & react** on
  the human's behalf — the same operations humans get, over MCP.
- Webhook and trajectory streams are **read over MCP** (`mcp://cairn/hook/<id>`);
  "agents read the same stream over MCP."
- **MCP OAuth** authorization flow: *"Claude Desktop wants to connect to your Cairn
  workspace over MCP."* Consent screen — **"THIS WILL ALLOW CLAUDE TO: Read
  artifacts you can access · Create & push new artifacts · Comment & react on your
  behalf"** — "Connected over MCP · revoke anytime in settings."

## Cross-cutting concerns

### Annotations — reactions & comments (uniform layer)

One annotation model, type-specific **anchors**:

- **Reactions** — emoji (🔥 7, 🙏 3, 🎉 5, 👀 …) with a `＋` picker. Anchorable to:
  a markdown block or bullet, a code line/selection, an image region (pin), a
  webhook request, a trajectory turn or tool call, or the artifact as a whole.
- **Comments** — threaded. Anchors: text selection (lands in the right margin /
  panel), a code line, an image-region pin, a whole artifact. Webhook requests are
  **reactable but not comment-threaded** (deliberate: "discussion happens on the
  artifacts they produce, not here").
- Counts surface in the Bin (`💬 2 · 👀 3`) and in headers (`6 comments · 16
  reactions`, `2 pins · 8 reactions`).

### Provenance

Every artifact records who/what produced it and how: actor (human email or
`claude · sonnet-4.6`), channel (`via MCP`, `via CLI`), and capture time
(`captured 2h ago`, `via mcp · 1d`).

### Retention / expiry

Artifacts are **ephemeral by default** with a visible TTL: `⧗ expires 7d`,
`expires in 5d`. Expiry is shown in headers and CLI output.

### Access control

Link-based sharing with a visible policy: `🔒 you + anyone with link`. (Managing
sharing/permissions is an explicit, deliberate action.)

### Identifiers & URLs

- Human short links: `cairn.sh/<id>` (e.g. `cairn.sh/9qz1a`).
- Trajectories: `cairn.sh/run/<id>` (e.g. `cairn.sh/run/8kd2p`).
- Agent/MCP handles: `mcp://cairn/hook/<id>` for webhook streams; artifacts carry
  an `◆ mcp` affordance next to their URL.
- IDs are short, opaque, URL-safe.

## Design language

- **Dark, terminal-minimal** aesthetic. Canvas background `#0A0B0D`.
- Type: **IBM Plex Sans** (UI) + **JetBrains Mono** (code / spans / IDs).
- OTel-style category colors for trajectory spans: reason / exec / read / net / write.
- Consistent, compact chrome; the metadata/comments panel is collapsible; one URL
  control is the visual anchor of the header.

## Explicitly out of scope for v1 (noted as "try next")

Token-cost lane on the waterfall; run-vs-run (two-push) diff/compare; errored-run
rendering (red span); empty-state for a webhook endpoint awaiting its first request;
filtering the webhook stream by status/event; fully keyboard-driving the web shell
like the TUI.
