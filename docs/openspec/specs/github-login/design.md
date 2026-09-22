# Design: GitHub Login for the Web App Shell

## Context

Cairn authenticates humans against Pocket ID via a native OIDC relying-party
flow (ADR-0013, `internal/httpapi/oidc.go`): `GET /auth/login` stashes a state
cookie carrying state/nonce/PKCE verifier and redirects; `GET /auth/callback`
verifies the ID token and calls the shell's session-establishment path. The
web shell, the Bin, and the MCP consent screen (SPEC-0007) all read that
single ambient session. GitHub does not speak OIDC for user login — no ID
token, no discovery document — so a second provider cannot reuse the
verification step, only the surrounding machinery.

See SPEC-0013 (`spec.md`) for the normative requirements; this document covers
how and why.

## Goals / Non-Goals

### Goals

- GitHub login produces the same session object as Pocket ID login.
- Third providers are config + one file.
- The Pocket ID flow's behavior and its test suite are untouched.
- GitHub tokens never leave the callback.
- Only people the operator intends can enroll, through a mode that matches
  Switchboard's, and enabling GitHub without saying who fails closed (ADR-0024).

### Non-Goals

- Replacing Pocket ID or `dev_login_password` policy (ADR-0013 stands).
- Changes to the MCP OAuth 2.1 authorization server (ADR-0004) — agents still
  authenticate via client credentials / the consent screen, not GitHub.
- GitHub identity for the CLI or MCP surfaces; this is human web login only.
- Assurance-level plumbing (`amr`/`acr`): Cairn has no single-IdP
  issuer-trust assumption to unwind (unlike switchboard, see its ADR-0024);
  GitHub sessions are simply lower-assurance and that is recorded, not acted on.

## Decisions

### One provider interface, not two route trees

**Choice**: A minimal `AuthProvider` interface (start/finish a login, plus
provider id and display name) implemented by `oidcProvider` (wrapping today's
code) and `githubProvider`. Routes stay `/auth/login` and `/auth/callback`
with a `provider` query parameter.
**Rationale**: Option B (parallel `/auth/github/*` routes) duplicates state
handling and session establishment, and every future security fix lands twice.
With the interface, the handler branch is `switch provider` and everything
below it is shared.
**Alternatives considered**:
- Parallel GitHub-specific routes: rejected — code duplication, drift risk.
- OIDC-only, no GitHub: rejected — leaves the actual users (external
  collaborators, un-enrolled browsers) unserved.

### GitHub verification = code exchange + REST profile

**Choice**: Exchange the code for an access token, then `GET /user` and
`GET /user/emails`; identity = (login, primary verified email). Scope
`read:user user:email`. Reject login when no verified primary email exists.
**Rationale**: It is the only mechanism GitHub offers for user login. The
verified-email check is the anchor that makes the identity meaningful (any
GitHub user can set arbitrary emails; only verified ones prove control).
**Alternatives considered**:
- GitHub App instead of OAuth App: rejected for now — more setup, its extra
  permissions (repo access) are irrelevant to login; revisit if we ever want
  org-gated login.
- Accepting unverified emails: rejected — trivially spoofable identity.

### State cookie reuse, not a parallel mechanism

**Choice**: The GitHub flow reuses the OIDC state cookie (random value,
HMAC-bound, single-use, short TTL) for its `state` parameter, with
provider-specific fields opaque in the cookie payload.
**Rationale**: Login CSRF protection is identical for both flows and the
anti-replay machinery is already tested. A second state mechanism is a second
thing to get wrong.

### Session carries `iss`/`sub`; token is dropped

**Choice**: On success, the shared `establishSession` records the issuing
provider and subject alongside the existing fields; the access token is
dropped when the callback returns.
**Rationale**: Provenance stays auditable without changing what downstream
code sees; zero GitHub API traffic in steady state avoids rate limits.

### Enrollment modes and an allowlist, failing closed (ADR-0024)

**Choice**: Operator settings, parsed in `internal/config`, with the same names,
default and syntax as Switchboard's `SWITCHBOARD_ENROLLMENT_MODE` and
`SWITCHBOARD_ENROLLMENT_ALLOW`:

```go
EnrollmentMode  string        // CAIRN_ENROLLMENT_MODE: "allowlist" | "invite" | "open";
                              // unset = "invite" when GitHub is configured, else "open"
EnrollmentAllow []AllowEntry  // CAIRN_ENROLLMENT_ALLOW, parsed and lower-cased

type AllowEntry struct {
	Kind  string // "subject" (<issuer>|<subject>) | "email" | "domain" (@x) | "github-org"
	Value string
}
```

`config.Load` rejects:

- `open` combined with a non-empty allowlist;
- a malformed entry (an unknown prefix, an empty value, a GitHub subject that is
  not a number).

`invite` is always accepted. There is no alias for the first draft's
`CAIRN_ENROLLMENT`, `CAIRN_GITHUB_ALLOWED_ORGS` or `CAIRN_GITHUB_ALLOWED_USERS`;
they never shipped.

`Server.EnableGitHub` registers the provider only when credentials are set
**and** a GitHub sign-in could succeed: `open` mode, `allowlist` mode with at
least one entry a GitHub identity could match, `invite` mode once the ADR-0029
invitation store exists, or at least one user with a linked GitHub identity once
ADR-0029 adds identities. Otherwise it logs:

```
WARN github login disabled: CAIRN_GITHUB_CLIENT_ID/SECRET are set but
     CAIRN_ENROLLMENT_MODE=invite and this build has no team invitations —
     nobody could enroll. Set CAIRN_ENROLLMENT_MODE=allowlist with
     CAIRN_ENROLLMENT_ALLOW entries, or CAIRN_ENROLLMENT_MODE=open
```

It leaves `s.gh` nil, so the existing 404 behaviour covers the route.

`GET /user` additionally decodes `id` (int64). The identity becomes
`Identity{Issuer: "https://github.com", Subject: strconv.FormatInt(id, 10),
Actor: <verified email>, Login: <login>}`. `Login` is new: it is display and
logs only.

The enrollment decision is a function the callback calls after `FinishLogin`
succeeds, and before `sessions.Create`:

```go
type EnrollmentPolicy struct {
	Mode     string          // allowlist | invite | open
	Subjects map[string]bool // "<issuer>|<subject>", lower-cased
	Emails   map[string]bool
	Domains  []string
	Orgs     []string        // from github-org: entries
	Linked   func(ctx context.Context, iss, sub, email string) (bool, error) // ADR-0029; nil before it
	Invited  func(ctx context.Context, email string) (bool, error)          // ADR-0029; nil before it
	Operator func(iss, sub string) bool                                     // ADR-0029; nil before it
}

// Admit returns ok=false with a reason from {not_allowlisted,
// org_restricted_oauth_app, membership_check_failed}.
func (p EnrollmentPolicy) Admit(ctx context.Context, gh *http.Client, id Identity, numericID int64) (reason string, ok bool)
```

The membership call must use the user's token, which lives only inside
`FinishLogin`. So `FinishLogin` takes the policy and runs `Admit` before it drops
the token. The token still never leaves the provider.

The membership call is `GET {APIBase}/user/memberships/orgs/{org}`, with the same
`Accept: application/vnd.github+json` header `getJSON` sets:

| GitHub answer | Result |
|---|---|
| 200, `state: active` | admit |
| 200, `state: pending` | not admitted by this org; try the next |
| 404 | not admitted by this org; try the next |
| 403 | not admitted by this org (the org restricts the OAuth app); try the next, reason `org_restricted_oauth_app` |
| anything else, or a transport error | deny now, reason `membership_check_failed` |

`handleGitHubCallback` distinguishes `ErrNotPermitted` from other `FinishLogin`
errors. It renders a generic 403 "not permitted" page, which names no
organisation, team or invitation. Other errors keep today's 502 "github login
failed". The state cookie is cleared first on both paths, as today.

`NewGitHubProvider` appends `read:org` to `oauth2.Config.Scopes` only when the
mode is `allowlist` and `Orgs` is non-empty (AL-5).

The OIDC callback calls the same `EnrollmentPolicy.Admit` (without the GitHub
membership step) once ADR-0029 provides `Linked`; before then it is not gated,
because every OIDC sign-in would look like an enrollment.

**Rationale**:

- Organisations are how GitHub users are already grouped, and numeric ids are the
  only rename-proof key.
- Gating enrollment rather than every sign-in matches Switchboard's
  `SWITCHBOARD_ENROLLMENT_MODE` (Switchboard SPEC-0033), and keeps a GitHub API
  outage from locking out existing users.
- One allowlist variable in Switchboard's syntax, rather than GitHub-specific
  ones, keeps the two products configured identically and covers OIDC
  subjects too.
- Treating a closed provider as unconfigured reuses the tested 404 path.

**Alternatives considered**:

- Re-check on every sign-in: rejected. It diverges from Switchboard, and fails
  closed against existing users during GitHub outages. Offboarding is suspension
  (ADR-0029).
- Email-domain allowlist as the mechanism: rejected, because a verified email
  proves control of an address, not membership. `@<domain>` entries stay
  available in `CAIRN_ENROLLMENT_ALLOW` for parity with Switchboard, documented
  with that caveat.
- `GET /user/orgs` listing: rejected, because it silently omits organisations
  that restrict OAuth apps. The per-organisation membership call returns a
  distinguishable 403.

## Architecture

```mermaid
flowchart LR
    LP[Login page] -->|provider=github| GH[githubProvider]
    LP -->|default| PI[oidcProvider]
    subgraph shared
        ST[State cookie: single-use, HMAC, short TTL]
        CB["/auth/callback"]
        ES[establishSession: iss + sub + email]
    end
    GH --> ST --> CB --> ES
    PI --> ST --> CB --> ES
    ES --> BIN[Bin + MCP consent screen]
```

Package layout: `internal/httpapi/authprovider/` (interface, registry,
githubProvider); `internal/httpapi/oidc.go` stays where it is and grows an
adapter implementing the interface. Login page template gains one
conditionally rendered button.

## Risks / Trade-offs

- **Organisation OAuth app restrictions (ADR-0024)** → an organisation that
  restricts third-party apps returns 403 on the membership check until an owner
  approves Cairn's OAuth app. It is logged as `org_restricted_oauth_app` and
  documented, and fails closed.
- **Leavers keep access until suspended (ADR-0024)** → the gate applies to
  enrollment only. A removed organisation member keeps their session and (after
  ADR-0029) their account until the operator suspends them. This is documented,
  and is the same rule as Switchboard.
- **Phishable login vs Pocket ID passkey** → accepted and recorded: the
  session stores provider provenance so a future assurance policy can
  distinguish them; no capability in Cairn currently requires step-up.
- **GitHub outage blocks logins** → acceptable: Pocket ID remains available
  and existing sessions survive; no failover needed for a household deployment.
- **Two client secrets to rotate** → stored like the OIDC secret today
  (config/env, never committed); rotation is an ops task, not code.
- **Shared interface temptation to lowest-common-denominator** → the OIDC
  state struct is kept intact; provider-specific state is opaque bytes to the
  handler.

## Migration Plan

1. Merge provider interface + GitHub provider + login-page button (feature
   flag: absent unless GitHub credentials configured).
2. Configure credentials on the deployed instance; verify a real login.
3. Rollback: clear the GitHub credentials — the button disappears and the
   route 404s; Pocket ID flow is unaffected at every step.
4. (ADR-0024) Ship the enrollment gate. An instance that set only the
   credentials gets the `invite` default, stops offering GitHub login until
   invitations exist, and logs why. The operator sets
   `CAIRN_ENROLLMENT_MODE=allowlist` with `CAIRN_ENROLLMENT_ALLOW` entries, or
   knowingly sets `CAIRN_ENROLLMENT_MODE=open`. The CHANGELOG says so. New GitHub
   sessions record the numeric id as `sub`, and old sessions simply expire.
5. (ADR-0024) Publish the operator docs, which were blocked until step 4:
   - add `CAIRN_ENROLLMENT_MODE`, `CAIRN_ENROLLMENT_ALLOW` and the two
     `CAIRN_GITHUB_CLIENT_*` variables to
     `.env.example` and to the
     environment passthrough in `docker-compose.yml` and
     `docker-compose.prod.yml`;
   - add a "GitHub login" section to the self-hosting guide: creating the GitHub
     OAuth app, the callback URL `CAIRN_BASE_URL + /auth/callback`, the
     mode and allowlist syntax (including the `@<domain>` caveat), why
     `read:org` appears only with `github-org:` entries, the
     organisation-approval step for organisations that restrict OAuth apps, and
     the login-time-only membership window;
   - add troubleshooting entries for the startup warning and each denial reason.
   Also correct the code comments that cite the wrong design IDs (`github.go`,
   `authprovider.go` and `config.go` name SPEC-0012 / ADR-0017; the records are
   SPEC-0013 / ADR-0019). Stop hard-coding "Pocket ID" in the login page's note,
   which is wrong when GitHub is also offered.

## Open Questions

- ~~Should GitHub logins be allow-listed rather than open to any GitHub
  account?~~ **Resolved by ADR-0024 (2026-09-22):** yes. Enrollment is gated by
  `CAIRN_ENROLLMENT_MODE` (default `invite` with GitHub configured) and
  `CAIRN_ENROLLMENT_ALLOW`, failing closed, with open signup only as the explicit
  `CAIRN_ENROLLMENT_MODE=open`, matching Switchboard's enrollment modes.
- Does the CLI (`cairn login`) ever want GitHub as a device-flow option?
  Deferred — out of scope for this spec.
