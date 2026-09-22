---
status: accepted
date: 2026-09-13
implements: [ADR-0019, ADR-0024]
requires: [SPEC-0001]
related: [SPEC-0023]
---

# SPEC-0013: GitHub Login for the Web App Shell

## Graph Edges

- **Implements:** **ADR-0019** — GitHub as an additional human auth provider behind a minimal provider interface.
- **Implements:** **ADR-0024** — enrollment is an operator mode (`allowlist`, `invite`, `open`), and GitHub sign-in fails closed.
- **Extends:** **SPEC-0001** — the web app shell whose login page and session model this capability extends.
- **Related:** **SPEC-0023** — Teams and tenancy: the users, identities and invitations the `invite` enrollment mode reads.

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

**Amended 2026-09-22 (ADR-0024).** Enrollment (an identity not yet linked to an
active Cairn user) is gated by an operator enrollment mode,
`CAIRN_ENROLLMENT_MODE=allowlist|invite|open`, defaulting to `invite` when GitHub
login is configured and `open` otherwise, and by an operator allowlist,
`CAIRN_ENROLLMENT_ALLOW` (requirements AL-1 to AL-6 below). Both match
Switchboard's `SWITCHBOARD_ENROLLMENT_MODE` and `SWITCHBOARD_ENROLLMENT_ALLOW` in
values, default and semantics. With GitHub configured and no invitations or
allowlist entries, nobody can enroll through GitHub, and the provider is treated
as unconfigured. Open signup requires the explicit `open` mode. This reverses
the design's original default of admitting any verified-email GitHub account.
The GitHub session subject becomes the numeric GitHub account id.

## Requirements

### Requirement: Provider Selection on the Login Page

The login page MUST render a "Log in with GitHub" button when the GitHub
provider is configured, and MUST NOT render it otherwise. Clicking it MUST
start the login flow at `GET /auth/login?provider=github`.

#### Scenario: GitHub provider configured

- **WHEN** a visitor loads the login page with `CAIRN_GITHUB_CLIENT_ID` and `CAIRN_GITHUB_CLIENT_SECRET` configured, and a GitHub sign-in could succeed (AL-3)
- **THEN** the page shows both the Pocket ID login control and a "Log in with GitHub" button, and the button links to `/auth/login?provider=github`

#### Scenario: GitHub provider not configured

- **WHEN** a visitor loads the login page on a deployment without GitHub credentials configured, or with credentials but no way for a GitHub sign-in to succeed (AL-3)
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
- **THEN** Cairn establishes a session for that identity (issuer `https://github.com`, subject the numeric GitHub account id as amended by ADR-0024, actor the primary verified email; the login is kept for display and logs) and redirects to the post-login destination, identical in behavior to an ADR-0013 login

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

### Requirement: AL-1 Enrollment Mode and Allowlist Configuration

The operator MUST be able to configure enrollment with:

- `CAIRN_ENROLLMENT_MODE`: `allowlist`, `invite` or `open`. When unset it MUST
  be `invite` if the GitHub provider is configured and `open` otherwise. The
  names, default and meanings MUST match Switchboard's
  `SWITCHBOARD_ENROLLMENT_MODE`:
  - `allowlist`: only identities matching `CAIRN_ENROLLMENT_ALLOW` may enroll;
  - `invite`: only identities whose verified email holds a pending team
    invitation, plus operators (Cairn ADR-0029), may enroll;
  - `open`: any identity the configured providers authenticate may enroll.
- `CAIRN_ENROLLMENT_ALLOW`: comma-separated entries in the syntax of
  Switchboard's `SWITCHBOARD_ENROLLMENT_ALLOW`:
  - `<issuer>|<subject>`, a provider-qualified subject; a GitHub user is
    `https://github.com|<numeric account id>`;
  - a verified email address;
  - `@<domain>`, matching a verified email in that domain;
  - `github-org:<login>`, matching an active member of that GitHub
    organisation.

Matching MUST be case-insensitive. A GitHub subject entry MUST match GitHub's
numeric account `id` from `GET /user`, never the login.

Startup MUST fail, naming the problem, when:

- an allowlist entry is malformed;
- `open` is combined with a non-empty `CAIRN_ENROLLMENT_ALLOW`.

Selecting `invite` MUST NOT fail startup, including on a build without team
invitations; there, `invite` admits nobody (AL-3).

When the effective mode is `open` and the GitHub provider is configured, anyone
with a GitHub account can enroll. That is a risky setting, so it is never the
default with GitHub on: `cairnd` MUST log a WARN at startup saying so, and the
self-hosting guide MUST call the combination out in a warning admonition. These settings are operator
configuration. No user, team admin, token or request may change them. There is
no deprecated alias for any earlier variable name.

#### Scenario: Numeric id survives a rename

- **WHEN** `CAIRN_ENROLLMENT_MODE=allowlist`, `CAIRN_ENROLLMENT_ALLOW=https://github.com|583231`, and that account has renamed its GitHub login since the entry was written
- **THEN** it may enroll, and a different account now holding the old login may not

#### Scenario: Conflicting configuration refused

- **WHEN** `CAIRN_ENROLLMENT_MODE=open` and `CAIRN_ENROLLMENT_ALLOW=github-org:acme` are both set
- **THEN** cairnd refuses to start and names both variables

#### Scenario: Default mode with GitHub configured

- **WHEN** GitHub credentials are set and `CAIRN_ENROLLMENT_MODE` is unset
- **THEN** the effective mode is `invite`, and cairnd starts

#### Scenario: Open signup with GitHub warns

- **WHEN** GitHub credentials are set and `CAIRN_ENROLLMENT_MODE=open`
- **THEN** cairnd starts, any GitHub account may enroll, and the startup log carries a WARN that enrollment is open to every GitHub account

#### Scenario: Default mode without GitHub

- **WHEN** only the OIDC provider is configured and `CAIRN_ENROLLMENT_MODE` is unset
- **THEN** the effective mode is `open`, and OIDC sign-in behaves as before

### Requirement: AL-2 The Gate Applies to Enrollment

A sign-in is an **enrollment** when its identity (`<issuer>`, `<subject>`; for
GitHub, `https://github.com` and the numeric id) is not linked to an active
Cairn user, and its verified, lower-cased email does not link it to one. The
gate (AL-4) MUST apply to every enrollment from every provider, and MUST NOT
apply to an identity already linked to an active user. Until persistent users
exist (Cairn ADR-0029), every GitHub sign-in is an enrollment, and OIDC
sign-ins are not gated, because a returning OIDC user cannot yet be told from a
new one; the gate MUST cover OIDC in the release that adds users. Revoking an
existing user's access is suspension (ADR-0029), not the allowlist.

#### Scenario: Today every sign-in is checked

- **WHEN** a GitHub user signs in on a build without persistent users
- **THEN** the full enrollment gate runs for that sign-in

#### Scenario: Existing user is not locked out by a GitHub API error

- **WHEN** persistent users exist, an already-linked user signs in, and the organisation membership API is failing
- **THEN** they are signed in, because no membership call is made for a linked identity

### Requirement: AL-3 Fail Closed Without a Way In

In `allowlist` mode with an empty `CAIRN_ENROLLMENT_ALLOW`, and in `invite` mode
before team invitations exist, no enrollment may succeed. While no GitHub
sign-in could succeed at all (such a condition holding and no Cairn user having
a linked GitHub identity), the GitHub provider MUST be treated as unconfigured:

- no button is rendered;
- `/auth/login?provider=github` and a GitHub callback return 404,
  indistinguishable from an unknown provider;
- cairnd logs a startup warning naming the variables that would enable it.

#### Scenario: Credentials alone admit nobody

- **WHEN** only `CAIRN_GITHUB_CLIENT_ID` and `CAIRN_GITHUB_CLIENT_SECRET` are set, on a build without team invitations
- **THEN** the effective mode is `invite`, the login page shows no GitHub button, `/auth/login?provider=github` returns 404, and the startup log warns that GitHub enrollment is closed until `CAIRN_ENROLLMENT_MODE=allowlist` with `CAIRN_ENROLLMENT_ALLOW` entries, or `CAIRN_ENROLLMENT_MODE=open`, is set

### Requirement: AL-4 Enrollment Check Before Session

After the existing identity verification succeeds, and before any session is
created, an enrollment MUST be admitted only when one of these holds:

1. `CAIRN_ENROLLMENT_MODE=open`;
2. in `invite` mode, the identity is an operator, or its verified email holds a
   pending team invitation;
3. in `allowlist` mode, a subject, verified-email or `@<domain>` entry in
   `CAIRN_ENROLLMENT_ALLOW` matches;
4. in `allowlist` mode, for a GitHub identity, `GET /user/memberships/orgs/{org}`
   with the user's token returns 200 with `state == "active"` for some
   `github-org:` entry.

For each organisation checked:

- 404 (not a member), `state == "pending"`, and 403 (the organisation restricts
  the OAuth app) MUST be treated as not admitted by that organisation;
- any other status, or a transport error, MUST deny the enrollment. Fail closed.

A denied enrollment MUST:

- create no session;
- clear the state cookie;
- render a generic 403 page that reveals nothing about which organisations,
  users, teams or invitations exist;
- log the GitHub login, the numeric id and the reason (`not_allowlisted`,
  `org_restricted_oauth_app`, `membership_check_failed`), and never the token.

#### Scenario: Active org member admitted

- **WHEN** `CAIRN_ENROLLMENT_MODE=allowlist`, `CAIRN_ENROLLMENT_ALLOW=github-org:acme`, and the enrolling user is an active member of `acme`
- **THEN** a session is established exactly as for any GitHub login

#### Scenario: Non-member denied without disclosure

- **WHEN** the enrolling user matches no allowlist entry, no organisation, and no invitation
- **THEN** the response is the generic 403 page, which names no organisation or team, no session cookie is set, and the log reason is `not_allowlisted`

#### Scenario: Invited friend enrolls under the default mode

- **GIVEN** GitHub is configured, `CAIRN_ENROLLMENT_MODE` is unset, and a pending team invitation exists for `friend@example.com` (Cairn ADR-0029)
- **WHEN** a GitHub account whose primary verified email is `friend@example.com` completes login
- **THEN** a session is established and no organisation membership call is made

#### Scenario: Pending organisation membership denied

- **WHEN** the user's `acme` membership has `state: "pending"` and nothing else admits them
- **THEN** the enrollment is denied

#### Scenario: Organisation restricts the OAuth app

- **WHEN** the membership call for `acme` returns 403
- **THEN** that organisation does not admit, the log reason is `org_restricted_oauth_app`, and the next allowed organisation (if any) is checked

#### Scenario: GitHub API failure denies an enrollment

- **WHEN** the membership call times out or returns 502 during an enrollment
- **THEN** the enrollment is denied with reason `membership_check_failed`, and no session is created

### Requirement: AL-5 Scope Minimisation

The authorisation request MUST include the `read:org` scope if and only if the
mode is `allowlist` and `CAIRN_ENROLLMENT_ALLOW` holds a `github-org:` entry.
Otherwise the scopes MUST remain `read:user user:email`.

#### Scenario: Subject-only allowlist does not ask for org access

- **WHEN** `CAIRN_ENROLLMENT_MODE=allowlist` and `CAIRN_ENROLLMENT_ALLOW` holds only `https://github.com|583231`
- **THEN** the redirect to GitHub requests `read:user user:email` and not `read:org`

### Requirement: AL-6 Numeric Subject and Login-Time Evaluation

A GitHub session MUST record:

- `iss`: `https://github.com`;
- `sub`: the numeric account id;
- the login, for display and logs only.

All enrollment checks MUST run only in the callback. The access token MUST still
be dropped when the callback returns (REQ "Token Containment"), and no GitHub
API call may happen outside the callback. The operator documentation MUST state
that leaving an allowed organisation does not end an existing session or
(after ADR-0029) an existing account: the operator suspends the user.

#### Scenario: Subject is the numeric id

- **WHEN** a GitHub session is established
- **THEN** its stored `sub` is the numeric account id, not the login

#### Scenario: Removed member keeps a live session

- **WHEN** a signed-in user is removed from the allowed organisation
- **THEN** their existing session continues until it expires or is revoked, and no GitHub API call is made in between

## Security Requirements

This is a web-facing spec. The following apply per ADR-0019 and the project's
security baseline; anything not restated here follows the existing shell
behavior (SPEC-0001 and ADR-0013):

- **Authentication**: Per ADR-0013, sessions are HttpOnly, SameSite=Lax,
  signed cookies with server-side revocation. GitHub login reuses that
  machinery verbatim; no new cookie or session format is introduced.
- **Authorization (enrollment)**: An identity enrolls only through the
  operator's enrollment mode and allowlist (AL-1 to AL-4). GitHub enrollment
  fails closed on an empty allowlist, on `invite` with no invitation, and on any
  membership-check error, and a refusal discloses nothing about organisations,
  teams or invitations. Mode and allowlist are operator configuration, never
  user-settable.
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
- **`aria-live` regions for dynamic content** — login error messages (rejected email, state mismatch, account not permitted) MUST be announced via a polite live region
- **Keyboard navigation** — the button is reachable in tab order and activates with Enter/Space like the existing login controls
- **Focus management in modals and dialogs** — not applicable; the login flow introduces no modal
