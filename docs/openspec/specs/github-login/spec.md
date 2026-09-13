---
status: draft
date: 2026-09-13
implements: [ADR-0019]
extends: [SPEC-0001]
---

# SPEC-0013: GitHub Login for the Web App Shell

## Graph Edges

- **Implements:** [ADR-0019](../../adrs/ADR-0019-github-as-additional-human-auth-provider.md) — GitHub as an additional human auth provider behind a minimal provider interface.
- **Extends:** [SPEC-0001](../web-app-shell-and-bin/spec.md) — the web app shell whose login page and session model this capability extends.

## Overview

The web app shell (SPEC-0001) authenticates humans today via Pocket ID OIDC
(ADR-0013) and, outside production, `dev_login_password`. This capability adds
**"Log in with GitHub"** as a second production identity provider, realized
through the provider interface defined in ADR-0019. A GitHub login establishes
the *same* Cairn session a Pocket ID login does: the ambient web session that
the Bin (SPEC-0001) and the MCP OAuth consent screen (SPEC-0007) already read.

GitHub issues no OIDC ID token for user login, so identity verification is
OAuth 2.0 code exchange plus `GET /user` and `GET /user/emails`, with the
primary verified email as the identity anchor. The provider difference is
confined to one implementation; routes, session establishment, and CSRF are
shared and unchanged.

## Requirements

### Requirement: Provider Selection on the Login Page

The login page MUST render a "Log in with GitHub" button when the GitHub
provider is configured, and MUST NOT render it otherwise. Clicking it MUST
start the login flow at `GET /auth/login?provider=github`.

#### Scenario: GitHub provider configured

- **WHEN** a visitor loads the login page with `CAIRN_GITHUB_CLIENT_ID` and `CAIRN_GITHUB_CLIENT_SECRET` configured
- **THEN** the page shows both the Pocket ID login control and a "Log in with GitHub" button, and the button links to `/auth/login?provider=github`

#### Scenario: GitHub provider not configured

- **WHEN** a visitor loads the login page on a deployment without GitHub credentials configured
- **THEN** no GitHub login control is rendered and `/auth/login?provider=github` returns 404 without leaking whether the route exists

### Requirement: GitHub OAuth Callback Exchange

Cairn MUST complete the GitHub authorization-code flow at
`GET /auth/callback?provider=github&code=...&state=...`: validate `state`
against the login cookie (same machinery as the OIDC flow), exchange the code
at `https://github.com/login/oauth/access_token`, fetch `GET /user` and
`GET /user/emails` with the resulting token, and select the primary email
where `verified == true`.

#### Scenario: Successful GitHub login

- **WHEN** a user completes GitHub consent with a verified primary email
- **THEN** Cairn establishes a session for that identity (issuing user, subject `login`, primary verified email) and redirects to the post-login destination, identical in behavior to an ADR-0013 login

#### Scenario: Unverified or missing primary email

- **WHEN** GitHub returns no primary email with `verified == true`
- **THEN** login is rejected with a user-visible error, no session is established, and the reason is logged with the GitHub user id but never the token

#### Scenario: Invalid or replayed state

- **WHEN** the callback's `state` does not match the state cookie, or the state cookie is expired or absent
- **THEN** the callback is rejected before any token exchange, per the existing OIDC state rules

### Requirement: Session Parity and Issuer Provenance

A session established via GitHub MUST be indistinguishable from a Pocket ID
session to all downstream consumers (Bin handlers, MCP consent screen), except
that the stored session MUST record the issuing provider (`iss`) and the
provider's subject (`sub`) so provenance is auditable.

#### Scenario: GitHub session in the Bin

- **WHEN** a user logged in via GitHub opens the Bin
- **THEN** every handler behaves exactly as it would for a Pocket ID session with the same actor identity

#### Scenario: Session provenance is recorded

- **WHEN** a session is established by either provider
- **THEN** the session record carries `iss` and `sub` values identifying the provider and provider-side subject

### Requirement: Token Containment

GitHub access tokens MUST be used only inside the callback to fetch the
profile and MUST NOT be persisted in sessions, cookies, logs, or the database.
GitHub API calls in steady state MUST be zero — the profile is captured at
login time only.

#### Scenario: Token lifetime

- **WHEN** a GitHub callback completes and the session is established
- **THEN** the access token is dropped and never appears in any persisted store or log line

## Security Requirements

This is a web-facing spec. The following apply per ADR-0019 and the project's
security baseline; anything not restated here follows the existing shell
behavior (SPEC-0001 and ADR-0013):

- **Authentication**: Per ADR-0013, sessions are HttpOnly, SameSite=Lax,
  signed cookies with server-side revocation. GitHub login reuses that
  machinery verbatim; no new cookie or session format is introduced.
- **Rate limiting**: The `/auth/login` and `/auth/callback` routes inherit
  whatever edge limits exist at the reverse proxy. The state cookie's
  single-use, short-TTL property is the primary anti-replay control; per-IP
  callback throttling SHOULD be added if abuse is observed.
- **Security headers**: Per the existing shell middleware (CSP and friends) —
  unchanged by this capability; the login page's new button adds no inline
  script.
- **Request body size limits**: Per the existing shell middleware. This
  capability introduces no new body-consuming endpoints.
- **CSRF protection**: Per ADR-0013 — the OIDC state cookie (random value,
  HMAC-bound, single-use, short TTL) is reused for the GitHub flow's
  `state` parameter, giving login CSRF protection identical to the existing
  flow.
- **Redirect validation**: The callback redirect target after login MUST be
  restricted to the allow-listed post-login destinations already used by the
  OIDC flow; the `state` cookie is the only carrier of the destination and
  arbitrary `next` parameters MUST NOT be accepted from the query string of
  the GitHub-initiated callback.

## Accessibility Requirements

This spec touches the login page UI. The following are MANDATORY for the
GitHub login button, per WCAG 2.1 AA:

- **WCAG 2.1 AA compliance** — the minimum conformance target
- **ARIA landmarks** — the login page's existing landmarks are preserved
- **`aria-label` on icon-only controls** — the GitHub button MUST carry an accessible name ("Log in with GitHub"); if rendered with only the GitHub mark, the label is still required
- **`aria-live` regions for dynamic content** — login error messages (rejected email, state mismatch) MUST be announced via a polite live region
- **Keyboard navigation** — the button is reachable in tab order and activates with Enter/Space like the existing login controls
- **Focus management in modals and dialogs** — not applicable; the login flow introduces no modal
