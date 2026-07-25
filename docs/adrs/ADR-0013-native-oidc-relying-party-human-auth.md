---
status: accepted
date: 2026-07-09
decision-makers: joestump
# optional forward-only graph edges (extends / enables / related: lists of ADR IDs).
# Do NOT author inverse edges. Do NOT add `governs`.
extends: [ADR-0004]
related: [ADR-0012]
---

# ADR-0013: Native OIDC Relying Party for Human Auth

## Context and Problem Statement

Cairn's web app shell (ADR-0011) currently authenticates humans one of two ways:
an MVP `dev_login_password` form (any actor id, one shared password — SPEC-0001)
for local development and tests, and, at cairn.stump.rocks, an oauth2-proxy
forward-auth layer in front of the whole site that hands off to Pocket ID and
injects a trusted header Cairn never actually reads. That forward-auth layer is
a second login system bolted on at the infra layer: it duplicates the identity
Cairn already establishes for its own OAuth 2.1 authorization server
(ADR-0004), adds an extra hop and an extra point of failure to every request,
and leaves `dev_login_password` reachable in production as an unused-but-live
credential. How does Cairn become a self-contained OIDC **relying party** —
logging the human in directly against Pocket ID with no fronting proxy — while
still hosting its own OAuth 2.1 **authorization server** for MCP clients
(ADR-0004), and without a second login for the human who is both "the person
using the web app" and "the person whose agent is requesting MCP access"?

## Decision Drivers

* **One identity, no second login.** A human who signs in to browse the Bin and
  a human who approves an MCP consent screen are the same person in the same
  browser session; OIDC login must feed the *same* session the consent screen
  already reads (ADR-0004's `sessionPrincipal`), not a parallel identity.
* **Passwordless.** Pocket ID is the house identity provider; humans should
  never see a Cairn-specific password in production.
* **Fewer moving parts at the edge.** Every hop between the browser and Cairn
  (proxy, forward-auth callback, trusted-header injection) is something that can
  misconfigure or fail independently of Cairn itself. Collapsing "authenticate"
  into the app removes oauth2-proxy from the deployment entirely — plain Caddy
  `reverse_proxy`, matching how switchboard, Grafana, and open-webui already run
  in this house.
* **`dev_login_password` must not be a live production credential.** A shared
  password that authenticates as *any* actor id is acceptable only when there is
  no real IdP to fall back on (local dev, CI, unit tests) — never a standing
  credential on a deployment that has Pocket ID.
* **Don't disturb the OAuth 2.1 authorization server.** ADR-0004 committed Cairn
  to being its own AS for MCP clients; that stays exactly as-is. This decision is
  scoped to how the *human* logs into the *web app* — the identity the consent
  screen (`/oauth/authorize`) already reads from the ambient web session.
* **House stack.** Go, mirroring switchboard's proven OIDC relying party
  (`github.com/coreos/go-oidc/v3` + `golang.org/x/oauth2`) against the same
  Pocket ID instance.

## Considered Options

* **Option A — Cairn as a native OIDC relying party.** Cairn itself performs the
  authorization-code + PKCE + nonce flow against Pocket ID (`GET /auth/login` →
  redirect → `GET /auth/callback` → verify ID token → establish Cairn's existing
  session), exactly as switchboard already does against the same IdP. Remove
  oauth2-proxy from the deployment; Caddy plain-proxies to Cairn.
* **Option B — Keep oauth2-proxy forward-auth, teach Cairn to trust its
  header.** Cheaper to build (read a header, trust it), but keeps a second login
  system in front of the app, keeps the proxy as a single point of failure and an
  extra deploy artifact, and still leaves `dev_login_password` live in
  production as a bypass of the header-trust path unless separately locked down.
  Doesn't reduce moving parts — it adds a second auth code path (header-trust)
  alongside the AS.
* **Option C — Federate Cairn's own AS as the login mechanism (self-issue,
  loop back through `/oauth/authorize`).** Reuse the OAuth 2.1 authorization
  server as the human-login mechanism too, self-issuing a token to itself.
  Rejected: circular (the consent screen needs an authenticated session to
  render; using the AS to *produce* that session is a bootstrapping loop), and
  it conflates "Cairn is an AS for MCP clients" with "Cairn is an RP for human
  login" — two different protocol roles ADR-0004 deliberately kept distinct is
  worth keeping distinct here too.

## Decision Outcome

Chosen option: **"Cairn as a native OIDC relying party"** (Option A), because it
removes a whole deployment tier (oauth2-proxy) in favor of code Cairn already
has a proven reference for (switchboard, against the same Pocket ID), it feeds
the *existing* session mechanism the OAuth 2.1 consent screen already trusts
with zero changes to that screen, and it lets `dev_login_password` be correctly
demoted to a fallback that is structurally incapable of running in a deployment
that has OIDC configured.

Concretely:

**Cairn stays both an OIDC client AND an OAuth 2.1 authorization server.**
These are two independent protocol roles over the one process (ADR-0012 "one
core, thin adapters" applies to auth too): the OIDC relying party authenticates
the *human* into a **Cairn session** (the same `session.Store` /
`cairn_session` cookie the dev-password login has always minted); the OAuth 2.1
AS (ADR-0004) issues *agent* tokens to MCP clients, gated on that same session
being present when a human approves consent. Neither role is aware of the
other beyond that shared session — `/oauth/authorize`'s `sessionPrincipal` needs
no code change at all, because an OIDC-established session is, from its
perspective, indistinguishable from a dev-password one.

**Flow — authorization-code + PKCE + nonce, mirroring switchboard.**
`GET /auth/login` mints state, a nonce, and a PKCE verifier, stashes them (plus
the validated `?next=` redirect target) in a short-lived `HttpOnly` cookie, and
redirects to Pocket ID's authorization endpoint with an S256 challenge.
`GET /auth/callback` validates the state cookie against the callback's `state`
parameter, exchanges the code (presenting the PKCE verifier), verifies the ID
token against the issuer's JWKS (signature, issuer, audience, expiry), confirms
the nonce round-trips, and establishes Cairn's ordinary web session — the human
identity is the ID token's `email` claim, falling back to `sub` when no email is
asserted. `POST /logout` is unchanged: it already revokes whatever session
exists, OIDC-originated or not.

**Config: `CAIRN_OIDC_ISSUER` is the single on/off switch.** OIDC is "enabled"
iff `CAIRN_OIDC_ISSUER` is non-empty; `CAIRN_OIDC_CLIENT_ID` (default `cairn`)
and `CAIRN_OIDC_CLIENT_SECRET` complete the client registration, and the
redirect URI is always derived as `CAIRN_BASE_URL + /auth/callback` — never a
separately configured value that could drift from the origin Pocket ID was
registered against.

**`dev_login_password` demoted to a local-dev-only fallback.** It is honored
only when `CAIRN_OIDC_ISSUER` is unset (`loginEnabled` now requires
`s.oidc == nil` in addition to its prior checks); a deployment that configures
OIDC can never fall back to the shared dev password even if one is still
present in its environment. Local development and the test suite, which have no
Pocket ID to talk to, are unaffected — `dev_login_password` continues to work
exactly as before when OIDC is unconfigured.

**The login page adapts, not the consent screen.** When OIDC is enabled the
login page (`GET /login`) renders a single "Sign in with Pocket ID" action in
place of the dev-password form; when it is not, the dev-password form renders
as it always has. An unauthenticated visit to any session-gated web route
(`requireWebSession`, and the OAuth `/oauth/authorize` consent screen's own
unauthenticated redirect) goes straight to `/auth/login` — skipping the
intermediate `/login` page entirely — whenever OIDC is configured, so there is
no extra click between "not logged in" and "at Pocket ID." `/login` itself stays
reachable directly (a bookmark, a stale link) and still renders the correct
action for whichever mode is active.

**Infra order of operations.** The Ansible side (stumpcloud/ansible, tracked
alongside this ADR) registers a Pocket ID OIDC client `cairn`, stores its secret
in Vault, and removes `oauth2_proxy` from the cairn inventory entry in favor of
a plain Caddy `reverse_proxy`. That redeploy happens *after* this code merges
and builds — never before, or the site would briefly have no auth in front of
it at all.

### Consequences

* Good, because a whole deployment tier (oauth2-proxy: its own container, its
  own config, its own failure mode, its own header-trust surface) is removed —
  Caddy talks straight to Cairn, matching switchboard/Grafana/open-webui.
* Good, because the human is passwordless in production: Pocket ID is the only
  credential, and `dev_login_password` is structurally prevented from acting as
  a silent bypass once OIDC is configured.
* Good, because the OAuth 2.1 consent screen (ADR-0004) needed **zero** code
  changes — it already read the ambient session, and an OIDC-established
  session satisfies it exactly like a dev-password one always did.
* Good, because the implementation has a proven, tested reference in this same
  house (switchboard) against the same Pocket ID instance, reducing the risk of
  a subtly wrong OIDC implementation (missing nonce check, skipped PKCE, trusting
  an unverified `alg`, etc.).
* Bad, because Cairn now performs its own JWKS fetch/cache and ID-token
  cryptographic verification in-process — a small amount of security-critical
  code oauth2-proxy previously owned — though this is the same code switchboard
  already carries in production.
* Neutral, because Cairn is now simultaneously an OIDC client (of Pocket ID) and
  an OAuth 2.1 server (to MCP clients); this is an intentional two-role design,
  not an accidental one, and the roles share nothing but the session cookie.

### Confirmation

Integration tests (real Postgres, a hermetic in-test OIDC issuer — discovery +
JWKS + token endpoint, RS256-signed ID tokens, mirroring switchboard's fake IdP
test harness) prove: a full login round trip establishes a session resolvable
by the ordinary session store; a tampered `state` parameter is rejected before
any code exchange, with no session minted; an expired or badly-signed ID token
is rejected at verification, with no session minted; visiting a session-gated
web route while unauthenticated redirects to `/auth/login` when OIDC is
configured; `dev_login_password` continues to authenticate when
`CAIRN_OIDC_ISSUER` is unset, and is refused (403) when it is set; and the
`/v1` REST API and `/mcp` MCP transport are unaffected — bearer/OAuth-token
auth only, an OIDC session cookie grants them nothing beyond what any ordinary
web session already did (ADR-0004's agent/human scope split is unchanged).
