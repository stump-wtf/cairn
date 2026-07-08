---
status: draft
date: 2026-07-08
implements: [ADR-0004, ADR-0003]
requires: [SPEC-0002]
---

# SPEC-0007: MCP Server and OAuth Authorization

## Overview

This capability defines Cairn's **MCP server surface** — the agent interface — and
the **OAuth 2.1 authorization** that lets an agent (or the CLI) act *on a human's
behalf*. It realizes ADR-0004 (MCP as a first-class surface with OAuth) and
ADR-0003 (triple-surface parity: the MCP tools are thin adapters over the same core
service the web and CLI use). It builds on SPEC-0002 (artifact core and share
types).

The MCP surface exposes exactly the operations humans have — read an artifact or
bundle file, create & push a new artifact, comment, and react — plus **read** access
to live webhook and trajectory streams. Authorization is the MCP-native OAuth 2.1
flow (authorization-code + PKCE, dynamic client registration, audience-bound access
tokens, rotating refresh tokens, RFC 7009 revocation). The human approves an exact
three-line consent screen — "Read artifacts you can access", "Create & push new
artifacts", "Comment & react on your behalf" — and every grant is independently
revocable from settings. The subject of every token is the **human**; the acting
**model** is recorded as the provenance actor. Agents never exceed the human's reach
and are granted no delete or sharing scopes. The CLI is another OAuth client on the
identical flow, delivering "same auth as your agent."

## Requirements

### Requirement: MCP Tool Surface — Artifact & Bundle Read

The MCP server MUST expose an artifact-read tool that returns any artifact — or a
named file within a bundle — that the authorizing human can reach, calling the same
core read operation the web and CLI surfaces call (ADR-0003). The tool MUST require
the `artifacts:read` scope. It MUST resolve a public id (or `mcp://cairn/<id>`
handle, ADR-0005) and return the body plus metadata (share type, provenance, expiry)
exactly as the core produces them. Reads MUST NOT return artifacts the human
principal could not reach, and MUST return a uniform not-found result for unknown,
unauthorized, or expired ids (per SPEC-0009 / ADR-0007).

#### Scenario: Read a reachable artifact

- **WHEN** an agent holding `artifacts:read` invokes the read tool with a valid id its human can reach
- **THEN** the server MUST return the artifact body and metadata via the core read operation

#### Scenario: Read an unreachable id

- **WHEN** an agent invokes the read tool with an id the human principal cannot reach (or that is expired/unknown)
- **THEN** the server MUST return a uniform not-found result and disclose nothing about existence

### Requirement: MCP Tool Surface — Create & Push

The MCP server MUST expose a create-and-push tool that creates a new artifact (or
bundle) owned by the human principal, calling the same core create operation as the
other surfaces. It MUST require the `artifacts:write` scope. New artifacts MUST be
created only with the default access policy (`you + anyone with link`) and default
TTL; the tool MUST NOT accept parameters that broaden sharing, disable expiry beyond
workspace policy, or otherwise exceed the human's own create permissions. The
returned artifact MUST carry provenance stamping the model actor and the `via MCP`
channel (SPEC-0009).

#### Scenario: Agent creates an artifact

- **WHEN** an agent holding `artifacts:write` pushes a new body
- **THEN** the server MUST create an artifact owned by the human, with the default policy and TTL, stamped `via MCP`, and return its id and link

#### Scenario: Create attempts a non-default policy

- **WHEN** a create call includes a request to make the artifact owner-only, no-expiry, or otherwise non-default
- **THEN** the server MUST reject the non-default policy request and either create with defaults or return an error, never broadening access

### Requirement: MCP Tool Surface — Comment & React

The MCP server MUST expose comment and react tools that post annotations as the
human's agent, calling the same core annotation operations as the web and CLI. Both
MUST require the `annotations:write` scope. Anchors MUST be validated by the core
(ADR-0006); the annotation's provenance MUST record the model actor and `via MCP`.

#### Scenario: Agent reacts to a span

- **WHEN** an agent holding `annotations:write` reacts to a trajectory turn with a valid anchor
- **THEN** the server MUST persist the reaction with provenance `via MCP` and the model actor

#### Scenario: Comment without the annotation scope

- **WHEN** an agent whose token lacks `annotations:write` invokes the comment tool
- **THEN** the server MUST refuse the call and make no annotation

### Requirement: MCP Resource Surface — Stream Reads

The MCP server MUST expose webhook and trajectory streams as **readable resources**
addressed by their agent handles (`mcp://cairn/hook/<id>`, `mcp://cairn/run/<id>`;
ADR-0005). Reading a stream MUST require only `artifacts:read` — a stream is an
artifact-shaped resource and MUST NOT require a fourth scope. Streaming reads MUST
deliver incremental data (webhook requests as they arrive, trajectory spans as they
land). The MCP surface MUST provide **no** write path into a stream in v1.

#### Scenario: Agent tails a webhook stream

- **WHEN** an agent with `artifacts:read` opens `mcp://cairn/hook/<id>` for a reachable webhook
- **THEN** the server MUST stream captured requests incrementally with no additional scope required

#### Scenario: Agent attempts to write a stream

- **WHEN** an agent attempts to push data into a webhook or trajectory stream over MCP
- **THEN** the server MUST reject it — streams are read-only to agents in v1

### Requirement: OAuth 2.1 Authorization-Code + PKCE

Authorization MUST use the OAuth 2.1 authorization-code grant with **PKCE
mandatory** for every client (public and confidential). The token endpoint MUST
reject a code exchange that lacks a valid `code_verifier` matching the registered
`code_challenge`. Authorization codes MUST be single-use, short-lived, and bound to
the client, redirect URI, and PKCE challenge.

#### Scenario: Code exchange without a verifier

- **WHEN** a client exchanges an authorization code without a valid PKCE `code_verifier`
- **THEN** the token endpoint MUST reject the exchange and issue no tokens

#### Scenario: Replayed authorization code

- **WHEN** a previously redeemed authorization code is presented a second time
- **THEN** the token endpoint MUST reject it and SHOULD revoke any tokens already issued for that code

### Requirement: Metadata Discovery & Dynamic Client Registration

Cairn MUST publish OAuth 2.1 authorization-server metadata (RFC 8414) and
protected-resource metadata so MCP clients can discover endpoints without bespoke
configuration. It MUST support **Dynamic Client Registration (RFC 7591)** so an
arbitrary MCP client (e.g. Claude Desktop) can self-register a redirect URI and
receive client credentials. The registration endpoint MUST be rate-limited and MUST
validate redirect URIs (exact match, loopback allowance for native/CLI clients).

#### Scenario: Client discovers and registers

- **WHEN** a new MCP client fetches the AS metadata and posts a valid dynamic registration
- **THEN** the server MUST return client metadata (client id, registered redirect URI) usable for the authorization-code flow

#### Scenario: Registration with an invalid redirect URI

- **WHEN** a registration request supplies a malformed or non-allowed redirect URI
- **THEN** the server MUST reject the registration

### Requirement: Exactly Three Consent Scopes

The authorization server MUST recognize exactly three scopes, mapping one-to-one to
the design's consent lines: `artifacts:read` ("Read artifacts you can access"),
`artifacts:write` ("Create & push new artifacts"), and `annotations:write`
("Comment & react on your behalf"). Approving the consent screen MUST grant exactly
the approved subset and nothing more. The server MUST NOT define or issue a
`sharing:manage` or `artifacts:delete` scope to agents in v1.

#### Scenario: Full consent grants exactly three scopes

- **WHEN** a human approves all three lines of the consent screen
- **THEN** the issued token MUST carry exactly `artifacts:read artifacts:write annotations:write` and no other scope

#### Scenario: Unknown or elevated scope requested

- **WHEN** a client requests a scope outside the three (e.g. `artifacts:delete`)
- **THEN** the server MUST reject the request or issue only the recognized subset, never the elevated scope

### Requirement: Token Model — Short Audience-Bound Access + Rotating Refresh

Each authorization (one client connection) MUST issue a token family: a short-lived
access token (~1 hour) and a longer-lived refresh token. Access tokens MUST be
**audience-bound** to the Cairn resource server (RFC 8707 resource indicators) so a
token cannot be replayed against another service. Refresh tokens MUST **rotate on
use** — redeeming a refresh token issues a new one and invalidates the old;
detection of a reused (already-rotated) refresh token MUST revoke the token family.

#### Scenario: Access token bound to audience

- **WHEN** an access token issued for Cairn is presented to a different audience/resource
- **THEN** it MUST be rejected as not audience-valid

#### Scenario: Refresh rotation and reuse detection

- **WHEN** a refresh token is redeemed
- **THEN** the server MUST issue a new refresh token, invalidate the prior one, and revoke the family if the prior (rotated-out) token is ever presented again

### Requirement: Token Revocation (RFC 7009)

Each grant MUST be an independently revocable connection the human sees in settings
(client name, scopes, last-used). The server MUST implement an RFC 7009 revocation
endpoint. Revoking a grant MUST invalidate that grant's access and refresh tokens
without affecting the human's other connections or the CLI.

#### Scenario: Revoke one connection

- **WHEN** a human revokes a specific MCP connection from settings
- **THEN** that grant's access and refresh tokens MUST stop working while sibling grants and the CLI continue to work

#### Scenario: Revoked token used

- **WHEN** a revoked access token is presented to the MCP endpoint
- **THEN** the server MUST respond 401 and perform no operation

### Requirement: Subject/Actor Identity Mapping & Least Privilege

The OAuth grant MUST authenticate the **human** and bind tokens to that workspace
identity; the human is the owner/principal for every action the agent takes. Every
agent action MUST additionally be stamped with the acting **model actor** and the
`via MCP` channel for provenance (SPEC-0009 / ADR-0007). An agent MUST NOT reach any
artifact its human could not, MUST NOT create anything the human does not own, and
MUST NOT change sharing/expiry or delete another party's artifact (no such scope
exists).

#### Scenario: On-behalf-of resolution

- **WHEN** an agent reads or creates over MCP
- **THEN** access MUST resolve to the human principal, ownership MUST be the human, and provenance MUST foreground the model actor with channel `via MCP`

#### Scenario: Agent attempts to exceed the human

- **WHEN** an agent attempts to read an artifact outside the human's reach or to change sharing/delete
- **THEN** the server MUST refuse — agents inherit, never exceed, the human's permissions

### Requirement: CLI on the Identical OAuth Flow

The CLI MUST be a public OAuth client using the same authorization-code + PKCE flow
against the same `/oauth/authorize` and `/oauth/token` endpoints, with a loopback
redirect and PKCE mandatory. The only difference from an MCP agent MUST be the
server-assigned channel (`via CLI` vs `via MCP`, per SPEC-0009). No separate auth
mechanism may exist for the CLI.

#### Scenario: CLI authorizes

- **WHEN** the CLI runs its login flow
- **THEN** it MUST use the identical `/oauth/authorize` + `/oauth/token` endpoints via loopback redirect + PKCE and display `authorized as <email> · via MCP OAuth`

#### Scenario: Channel distinguishes CLI from MCP

- **WHEN** the CLI creates an artifact with its token
- **THEN** the recorded channel MUST be `via CLI`, assigned server-side, not `via MCP`

### Requirement: Consent Screen Content

The authorization endpoint MUST render a consent screen naming the requesting client
("<Client> wants to connect to your Cairn workspace over MCP") and listing exactly
the three scope lines under a "THIS WILL ALLOW <CLIENT> TO:" heading, each with an
approve affordance. It MUST state that the connection is revocable ("Connected over
MCP · revoke anytime in settings"). The human MUST be authenticated before the
consent screen is shown; approval MUST create a grant limited to the checked scopes.

#### Scenario: Consent screen mirrors the three scopes

- **WHEN** an authenticated human reaches the consent screen for a client requesting all three scopes
- **THEN** it MUST display exactly the three lines ("Read artifacts you can access", "Create & push new artifacts", "Comment & react on your behalf") and a revoke-in-settings notice

#### Scenario: Consent denied

- **WHEN** the human denies consent
- **THEN** no grant or token is created and the client receives an `access_denied` authorization error

### Requirement: Error Handling Standards

Errors across the MCP transport, OAuth endpoints, and the core-call boundary MUST be
wrapped with context at each layer boundary. Domain and protocol failures callers
must distinguish (invalid_grant, invalid_scope, insufficient_scope, expired token,
unknown id) MUST be represented as sentinel/typed errors and mapped to the correct
OAuth/MCP error codes; failures MUST NOT be silently swallowed. All errors MUST be
recorded with structured (key-value) logging that never logs token secrets or PKCE
verifiers.

#### Scenario: Insufficient scope surfaces distinctly

- **WHEN** a tool call is refused for lacking a scope
- **THEN** the server MUST return a distinct `insufficient_scope` error (not a generic failure) and log it structurally without secrets

#### Scenario: Layer boundary wrapping

- **WHEN** a core operation returns a domain error to an MCP tool adapter
- **THEN** the adapter MUST wrap it with context and map it to the correct MCP/OAuth error rather than leaking or dropping it

### Requirement: Database Operation Standards

Multi-step OAuth state changes (issuing a grant with its token family; refresh
rotation; revocation cascade) MUST execute in transactions so a grant is never left
half-written. All database access MUST use parameterized queries only (no string
interpolation) and explicit connection lifecycle with timeouts. Token and code
lookups MUST be constant-time / hashed-at-rest where they represent secrets.

#### Scenario: Atomic refresh rotation

- **WHEN** a refresh token is rotated
- **THEN** invalidating the old token and issuing the new one MUST occur in a single transaction so a crash cannot leave two valid or zero valid refresh tokens

#### Scenario: Parameterized token lookup

- **WHEN** the server looks up a grant or token by identifier
- **THEN** it MUST use a parameterized query and never string-interpolate client input

## Endpoint Table

Auth-by-default: every endpoint is `Auth: Required` unless explicitly justified
otherwise. Discovery, registration, and token/revoke endpoints are unavoidably
reachable without a Cairn bearer token because they *bootstrap* authorization; each
is justified below and MUST be rate-limited (see Security Requirements).

| Endpoint | Method | Purpose | Auth |
|----------|--------|---------|------|
| `/.well-known/oauth-authorization-server` | GET | AS metadata (RFC 8414) discovery | **Public** — clients must read metadata before they can authenticate; contains no secrets |
| `/.well-known/oauth-protected-resource` | GET | Protected-resource metadata | **Public** — same discovery bootstrap; no secrets |
| `/oauth/register` | POST | Dynamic Client Registration (RFC 7591) | **Public** — DCR must accept unregistered clients per the MCP auth spec; rate-limited, redirect-URI validated |
| `/oauth/authorize` | GET | Render login + consent screen | Required — human session; unauthenticated visitors are redirected to login |
| `/oauth/authorize` | POST | Submit consent (approve/deny) | Required — human session + CSRF token |
| `/oauth/token` | POST | Code exchange, refresh rotation | **Client-authenticated (PKCE)** — no Cairn session; authenticated by PKCE `code_verifier` / client credentials |
| `/oauth/revoke` | POST | Revoke a grant's tokens (RFC 7009) | **Client-authenticated** — presented token + client credentials authenticate the call |
| `/mcp` | POST / GET (SSE) | MCP transport: tools + resource reads | Required — OAuth 2.1 bearer access token, audience-bound to Cairn |

## Security Requirements

This capability is web-facing (OAuth endpoints, the consent UI, and the MCP
transport). The following are MANDATORY.

### Requirement: Authentication & Authorization

Mutating and workspace-scoped endpoints MUST require authentication: a human session
for the consent UI, and an OAuth 2.1 bearer access token (audience-bound to Cairn)
for the MCP transport. Every MCP tool/resource MUST enforce its required scope
(`artifacts:read`, `artifacts:write`, or `annotations:write`) before acting. Agents
MUST NOT exceed the human's permissions and MUST hold no delete/sharing scope.

#### Scenario: Unauthenticated mutation

- **WHEN** an unauthenticated client calls a mutating MCP tool (create/comment/react)
- **THEN** the server MUST respond 401 and make no change

#### Scenario: Missing required scope

- **WHEN** a bearer token lacks the scope a tool requires
- **THEN** the server MUST refuse with `insufficient_scope` and perform no operation

### Requirement: Rate Limiting

All public and bootstrap endpoints (metadata discovery, `/oauth/register`,
`/oauth/token`, `/oauth/revoke`, `/oauth/authorize`) and the `/mcp` transport MUST
be rate-limited per-identity/per-IP; exceeding a limit MUST return 429 with
Retry-After. Registration and token endpoints MUST additionally throttle to blunt
client-spraying and code/refresh brute force.

#### Scenario: Burst on the token endpoint

- **WHEN** a client exceeds the configured rate on `/oauth/token` or `/oauth/register`
- **THEN** the server MUST respond 429 with Retry-After without processing the request

### Requirement: Security Headers

Responses from the OAuth/consent web pages MUST set a strict Content-Security-Policy,
X-Content-Type-Options: nosniff, Referrer-Policy, and (over HTTPS) HSTS. The consent
page MUST NOT reflect untrusted client-supplied strings (client name, scopes) without
contextual escaping, so a malicious registration cannot inject active content into
the consent origin.

#### Scenario: Untrusted client name on the consent screen

- **WHEN** a dynamically registered client supplies a name containing HTML/script
- **THEN** the consent page MUST escape/sanitize it so it cannot execute in Cairn's origin

### Requirement: Request Body Size Limits

Every endpoint MUST enforce a maximum request size; oversize requests MUST be
rejected with 413 before buffering the full body. This applies to `/oauth/register`
payloads, token requests, and MCP create-and-push tool bodies (delegating the
artifact-body ceiling to the core, SPEC-0002).

#### Scenario: Oversize create over MCP

- **WHEN** a create-and-push call exceeds the configured body limit
- **THEN** the server MUST reject it with a size error and not persist a partial blob

### Requirement: CSRF Protection

Cookie/session-authenticated state-changing requests — specifically the consent
approval POST on `/oauth/authorize` — MUST be CSRF-protected (token and/or SameSite
strategy). Token-authenticated MCP requests are exempt (bearer tokens are not ambient
credentials).

#### Scenario: Cross-site consent post

- **WHEN** a consent-approval POST arrives without a valid CSRF token on the session-auth route
- **THEN** the server MUST reject it and create no grant

### Requirement: Redirect & SSRF Validation

The authorization endpoint MUST redirect only to a client's **pre-registered**
redirect URI (exact match; loopback allowed for native/CLI clients); no
request-supplied absolute redirect URI outside the registered set is honored. Any
delegation to an upstream identity provider MUST validate its callback and MUST NOT
follow user-supplied URLs server-side (SSRF guard).

#### Scenario: Redirect to an unregistered URI

- **WHEN** an authorization request supplies a redirect URI not matching the client's registration
- **THEN** the server MUST reject the request and redirect nowhere

## Accessibility Requirements

This capability renders a user-facing consent/login page. WCAG 2.1 AA is the minimum
target.

### Requirement: WCAG 2.1 AA & Semantics

The consent and login pages MUST meet WCAG 2.1 AA. Structure MUST use ARIA landmarks
(banner, main, contentinfo). The distinction between granted and denied, and between
the three scope lines, MUST NOT be conveyed by color alone — each scope MUST carry
its text label and an explicit control state.

#### Scenario: Color-only scope state

- **WHEN** a scope's approved/denied state is shown with color
- **THEN** an equivalent text or shape cue (checkbox state, label) MUST also be present

### Requirement: Icon-Only Controls

Any icon-only control on the consent/login pages (e.g. a client-logo, an info
disclosure, a close/back affordance) MUST expose an aria-label describing its action.

#### Scenario: Icon button on consent

- **WHEN** a control on the consent page has no visible text label
- **THEN** it MUST expose an aria-label describing its action

### Requirement: Dynamic Content Regions

Any live/validation region on the login or consent flow (e.g. an inline error such as
"consent expired", "invalid request", or a post-approval status) MUST use an
aria-live region (polite for normal updates, assertive for errors).

#### Scenario: Inline consent error

- **WHEN** the consent request is invalid or expired and an inline message replaces the form
- **THEN** it MUST be announced via an aria-live region

### Requirement: Keyboard Navigation & Focus Management

All interactive elements on the consent/login pages MUST be keyboard-operable
(logical tab order; Enter/Space activate the approve/deny controls; Escape dismisses
any popover). If consent is presented in a modal/dialog, focus MUST move into it on
open, be trapped while open, and return to the triggering context on close.

#### Scenario: Keyboard-only approval

- **WHEN** a keyboard user tabs through the consent screen
- **THEN** every scope control and the approve/deny buttons MUST be reachable and operable via keyboard in a logical order
