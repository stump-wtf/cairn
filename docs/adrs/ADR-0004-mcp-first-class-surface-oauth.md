---
status: accepted
date: 2026-07-08
decision-makers: joestump
extends: [ADR-0003]
related: [ADR-0007]
---

# ADR-0004: MCP as a First-Class Surface with OAuth Authorization

## Context and Problem Statement

Cairn's core pitch is that agents read, create, comment, and react on the same
artifacts humans do — "same auth as your agent." ADR-0003 establishes that Web,
CLI, and MCP are three surfaces over one core service; this ADR decides how the
MCP surface is built and, more importantly, how an agent is *authorized* to act
**on the human's behalf**. How do we let Claude Desktop (or any MCP client)
connect to a human's Cairn workspace, obtain exactly the scopes the design's
consent screen shows ("Read artifacts you can access", "Create & push new
artifacts", "Comment & react on your behalf"), attribute every action back to the
human, and let the human revoke that access at any time?

## Decision Drivers

* **Delegated authority, not a second account** — an agent acts *as* the human;
  its actions must resolve to the human's workspace identity and their reachable
  artifacts, never a broader or separate principal.
* **Scoped, legible consent** — the design specifies an exact three-line consent
  screen; the grant must map one-to-one to what the human is shown.
* **Revocable anytime** — "Connected over MCP · revoke anytime in settings"
  demands per-connection, independently revocable credentials.
* **MCP-client compatibility** — Claude Desktop and other clients speak the MCP
  authorization spec (OAuth 2.1); Cairn must interoperate without bespoke setup.
* **CLI shares the agent's auth path** — the design shows `✓ authorized as
  sam@stump.rocks · via MCP OAuth` from the CLI; one auth mechanism, two clients.
* **Least privilege + auditability** — tokens should carry only granted scopes,
  be audience-bound to Cairn, and feed trustworthy provenance (`via MCP`, per
  ADR-0007).
* **House stack** — a Go MCP server; OAuth 2.1 authorization-code + PKCE.

## Considered Options

* **Option A — MCP OAuth 2.1 (authorization-code + PKCE) with scoped consent.**
  The MCP-native flow: the client discovers Cairn's authorization server, the
  human logs in and approves the three scopes in a browser, the client receives an
  audience-bound access token plus a rotating refresh token, and every connection
  is an independently revocable grant.
* **Option B — Static API keys / personal access tokens.** The human mints a
  token in settings and pastes it into the MCP client config. Simple, but there is
  no interactive per-scope consent, secrets are copy-pasted (and leak), scopes are
  coarse, and it is not the flow MCP clients expect.
* **Option C — Reuse the human's web session/cookie.** Bridge the browser session
  into MCP. Rejected: MCP clients are not browsers, there is no delegation
  boundary between "the human" and "the agent acting for the human," and a session
  cannot be scoped or revoked per-agent.
* **Option D — Mutual TLS client certificates.** Strong cryptographic client
  identity, but heavy operational burden (cert issuance/rotation), no consent UX,
  and a poor fit for consumer agent clients.

## Decision Outcome

Chosen option: **"MCP OAuth 2.1 authorization-code + PKCE with scoped consent"**,
because it is the standard MCP clients already implement, it renders exactly the
three-scope consent the design mandates, it mints per-agent revocable tokens bound
to the human's workspace identity, and — critically — the CLI is just another
OAuth client on the identical flow, delivering the "same auth as your agent"
promise with one code path.

Concretely:

**MCP server shape.** A Go MCP server exposes Cairn's core operations as MCP
tools/resources that map onto the same service layer the Web and CLI call
(ADR-0003): read an artifact or bundle file, create-and-push a new artifact,
comment, and react. Live streams — webhook requests and trajectory spans — are
**read** over MCP via the agent handles defined in ADR-0005
(`mcp://cairn/hook/<id>`, `mcp://cairn/run/<id>`). Streaming reads deliver
incremental data to the agent; there is no separate write path for streams from
agents in v1.

**Scope granularity — exactly three, matching the consent screen.**

| Scope | Consent line | Grants |
|-------|--------------|--------|
| `artifacts:read` | "Read artifacts you can access" | Read any artifact/bundle/stream the human can reach |
| `artifacts:write` | "Create & push new artifacts" | Create and push new artifacts (default policy only) |
| `annotations:write` | "Comment & react on your behalf" | Post comments and reactions as the human's agent |

Stream reads fall under `artifacts:read` — a webhook or trajectory is an
artifact-shaped resource, so reading its stream needs no fourth scope and the
consent screen stays at three checkboxes. We deliberately **do not** expose a
`sharing:manage` or `artifacts:delete` scope to agents in v1: changing sharing
and expiry is an explicit human action (ADR-0007), and agents create only with
the default `you + anyone with link` policy. The grant confers exactly the scopes
shown — no implicit extras.

**Token model — per-agent, short access + rotating refresh.** Each authorization
(one MCP client connection) is one grant issuing a token family: a short-lived
access token (~1 hour) and a longer-lived refresh token with rotation on use.
Access tokens are audience-bound to the Cairn resource server (RFC 8707 resource
indicators) so a token cannot be replayed against another service. Each grant is a
distinct row the human sees in settings (client name, scopes, last-used) and can
revoke; revocation (RFC 7009) kills that grant's refresh and access tokens without
touching the human's other connections or the CLI.

**Identity mapping — subject is the human, actor is the model.** The OAuth grant
authenticates the *human* (`sam@stump.rocks`) and binds the resulting tokens to
that workspace identity; that human is the owner/principal for every action the
agent takes. For provenance (ADR-0007), the action is additionally stamped with
the acting **model actor** (`claude · sonnet-4.6`) and the **channel** `via MCP`.
So the authorization subject and the displayed provenance actor are two facets of
one on-behalf-of relationship: access control resolves to the human; the
provenance record foregrounds the agent. An agent therefore can never reach an
artifact the human could not, and everything it creates is owned by the human.

**CLI on the identical path.** The CLI is a public OAuth client using the same
authorization-code + PKCE flow, with a loopback redirect and PKCE mandatory. It
hits the same `/authorize` and `/token` endpoints as an MCP agent; the only
difference is the recorded channel (`via CLI` vs `via MCP`, assigned server-side —
see ADR-0007). This is what "the CLI shares the same auth as the agent" means
architecturally: one authorization server, one flow, two client profiles.

**Authorization server & client onboarding.** Cairn hosts its own OAuth 2.1
authorization server inside the Go backend, protecting its own resource server. It
publishes authorization-server metadata for discovery and supports **Dynamic
Client Registration (RFC 7591)** so arbitrary MCP clients — Claude Desktop
included — can register without Cairn pre-provisioning each one, as the MCP
authorization spec recommends. The human-login step may delegate to the
workspace's upstream identity provider; the AS then issues Cairn-scoped tokens
regardless of how the human authenticated.

### Consequences

* Good, because it is MCP-standard: Claude Desktop connects via discovery + DCR
  with no bespoke Cairn plumbing, and the consent screen the human sees is exactly
  the three scopes the design specifies.
* Good, because per-grant tokens with rotation and RFC 7009 revocation deliver
  "revoke anytime in settings" literally — one connection can be cut without
  disturbing the CLI or other agents.
* Good, because the CLI and the agent collapse onto one auth path, reducing code
  and giving users a single mental model ("same auth as your agent").
* Good, because subject-vs-actor identity mapping makes agent actions auditable
  and correctly attributed for provenance (ADR-0007) while keeping access strictly
  bounded to the human's reach.
* Bad, because operating an OAuth 2.1 authorization server — DCR, refresh
  rotation, audience binding, revocation, metadata discovery — is substantially
  more to build and secure than static API keys (Option B).
* Bad, because the coarse three-scope model cannot express per-artifact
  delegation: an agent holding `artifacts:read` can read everything the human can.
  This is mitigated by the capability model (ADR-0007) — the agent only inherits
  the human's reach, and the shareable link is the real capability boundary.
* Neutral, because Cairn becomes an OAuth provider and must track the evolving MCP
  authorization spec as clients and the spec mature.

### Confirmation

Conformance tests run against the MCP authorization spec: metadata discovery
resolves, PKCE is required (a code exchange without a verifier is rejected), DCR
registers a fresh client, and the token/refresh/revocation endpoints behave per
RFC. A consent test asserts approving the three-line screen grants exactly
`artifacts:read artifacts:write annotations:write` and nothing more. A
scope-enforcement test proves a token lacking `annotations:write` is refused on
comment/react, and that no agent token can change sharing or delete another's
artifact. A revocation test proves a revoked grant's access and refresh tokens
both fail while a sibling grant (and the CLI) keep working. An integration test
drives the CLI and an MCP client through the same `/authorize` + `/token`
endpoints. Finally, provenance assertions (cross-checked in ADR-0007) confirm
agent-originated writes record the model actor and the server-assigned `via MCP`
channel.
