---
status: accepted
date: 2026-09-22
decision-makers: [joestump, joestump-agent]
extends: [ADR-0019]
related: [ADR-0013, ADR-0004, ADR-0029]
---

# ADR-0024: Enrollment Is an Operator Mode (allowlist, invite, open), and GitHub Sign-in Fails Closed

## Context and Problem Statement

ADR-0019 added "Log in with GitHub" as a second human-auth provider, and the
foundation shipped in #259. Once `CAIRN_GITHUB_CLIENT_ID` and
`CAIRN_GITHUB_CLIENT_SECRET` are set, **any GitHub account with a verified
primary email can sign in**. The code confirms it:

* `authprovider.GitHubProvider.FinishLogin` checks the state, exchanges the
  code, fetches `GET /user` and `GET /user/emails`, and returns an identity for
  the first primary verified email. There is no membership or identity check.
* `httpapi.handleGitHubCallback` passes that identity straight to
  `s.sessions.Create`.
* The requested scopes are `read:user user:email`. Cairn cannot see
  organisation membership even if it wanted to.

SPEC-0013's design left this open ("Should GitHub logins be allow-listed…?
Default plan: any verified-email GitHub account may log in"). That default is
now wrong:

1. **Cairn is multi-tenant.** There are two profiles: the operator, who runs the
   instance, and users, whom the operator lets in (Cairn ADR-0029, Teams and
   tenancy). An instance open to every GitHub account is one where
   strangers get an account, create artifacts, consume storage and receive MCP
   grants. The operator chose none of that.
2. **A self-hosting customer** wants GitHub login for their own organisation,
   without opening the instance to the world.
3. **The GitHub login docs are blocked on this.** The variables appear in no
   guide, no `.env.example` and no compose file. Documenting them as they stand
   would publish "set these two variables and anyone on GitHub can sign in".
4. **Switchboard has the same gap and is closing it** with enrollment modes
   (`SWITCHBOARD_ENROLLMENT_MODE=allowlist|invite|open`, Switchboard
   SPEC-0033). An operator running both products should meet one model, not two.

The question: how does an operator restrict GitHub sign-in to the people they
intend, and what happens when they enable the provider without saying who?

## Decision Drivers

* **Fail closed.** Enabling a login provider must never, by omission, admit
  everyone. The unsafe configuration must require an explicit, greppable setting.
* **Match how operators think.** "Members of my GitHub org" is the common case,
  and "these specific people" is the other.
* **One model across Cairn and Switchboard.** The same variables, mode names,
  default and semantics, and the same rule about whom the gate applies to. Joe
  decided this in design review (2026-09-22): the mode is operator config,
  `CAIRN_ENROLLMENT_MODE`, defaulting to `invite` when GitHub login is
  configured, aligned with Switchboard's `SWITCHBOARD_ENROLLMENT_MODE`.
* **No transition machinery.** Cairn is pre-1.0. Nothing here keeps an old
  behaviour alive behind a deprecation window (design review, 2026-09-22).
* **Least privilege at GitHub.** Do not request `read:org` unless an
  organisation check is configured.
* **Identity that does not drift.** GitHub logins can be renamed and later
  reclaimed by someone else. The numeric account id cannot.
* **No steady-state GitHub traffic.** ADR-0019's token containment stands: the
  access token lives only inside the callback.
* **OIDC-only instances are unaffected by default.** Where GitHub login is not
  configured the mode defaults to `open`, because the IdP already decides who
  exists. The dev login is untouched.

## Considered Options

* **A. Enrollment modes (`allowlist`, `invite`, `open`; `invite` by default
  when GitHub login is configured, otherwise `open`) with one operator
  allowlist, `CAIRN_ENROLLMENT_ALLOW`, in Switchboard's syntax.** The gate is
  checked in the callback before a session exists, applies to **enrollment**
  (an identity not yet linked to an active Cairn user), and denies on any
  check error.
* **B. The same allowlist, re-checked on every GitHub sign-in**, so leaving the
  organisation locks the person out at their next login.
* **C. An email-domain allowlist**, checking the verified primary email against
  operator domains.
* **D. Keep it open, and rely on the operator** to leave GitHub disabled unless
  they want open signup.

## Decision Outcome

Chosen option: **A**.

### Configuration

| Variable | Meaning |
|---|---|
| `CAIRN_ENROLLMENT_MODE` | `allowlist`, `invite` or `open`. Default: `invite` when GitHub login is configured, otherwise `open`. The same names, default and meanings as Switchboard's `SWITCHBOARD_ENROLLMENT_MODE`. |
| `CAIRN_ENROLLMENT_ALLOW` | comma-separated entries, in the same syntax as Switchboard's `SWITCHBOARD_ENROLLMENT_ALLOW`: a provider-qualified subject `<issuer>\|<subject>` (a GitHub user is `https://github.com\|<numeric account id>`); a verified email address; `@<domain>`, matching a verified email in that domain; or `github-org:<login>`, matching an active member of that GitHub organisation. |

The modes:

* **`allowlist`**: an identity may enroll only if it matches an entry in
  `CAIRN_ENROLLMENT_ALLOW`.
* **`invite`**: an identity may enroll only if its verified email holds a
  pending team invitation, or it is an operator. Invitations and operators
  belong to Teams (ADR-0029). Before they exist nothing matches, so `invite`
  admits nobody and GitHub enrollment is closed (see "Fail closed"). Selecting
  it never fails startup.
* **`open`**: any identity the configured providers authenticate may enroll.
  This is the explicit open-signup setting. `open` combined with a non-empty
  `CAIRN_ENROLLMENT_ALLOW` is a startup error, because the operator's intent is
  ambiguous.

Entries match case-insensitively. A GitHub user is named by numeric account id,
which `GET /user` already returns, never by login, because a renamed login can
be reclaimed by a stranger. An `@<domain>` entry proves control of an address in
that domain, not membership of anything (option C's weakness); it exists for
parity with Switchboard, and the operator guide says so next to it.

`CAIRN_ENROLLMENT_MODE` and `CAIRN_ENROLLMENT_ALLOW` replace the
`CAIRN_ENROLLMENT`, `CAIRN_GITHUB_ALLOWED_ORGS` and `CAIRN_GITHUB_ALLOWED_USERS`
of this ADR's first draft outright. Those never shipped, and nothing reads them.

The GitHub session's `sub` becomes that numeric id, with the login kept for
display and logs. This amends SPEC-0013's "subject `login`" and matches the
identity key ADR-0029 uses: `(issuer, subject)`, linked to an existing user only
by verified, lower-cased email.

### Enrollment, not every sign-in

The gate decides whether an identity may **become or join** a Cairn user. A
sign-in is an enrollment when its identity is not yet linked to an active user,
and its verified email does not link it to one. The mode applies to every
provider, as Switchboard's does.

* **Until ADR-0029 introduces persistent users**, no identity is ever "already
  linked", so **every** GitHub sign-in is an enrollment and is checked. OIDC
  sign-ins are not gated until then, because Cairn cannot yet tell a returning
  OIDC user from a new one. This is build order, not a transition window: the
  gate covers OIDC in the release that adds users.
* **Afterwards**, an existing user is never locked out by the allowlist. To
  revoke access, the operator suspends the user (ADR-0029). This matches
  Switchboard. It also means a GitHub API outage cannot lock out people who
  already have accounts.

### Fail closed

In `allowlist` mode with an empty allowlist, and in `invite` mode before
invitations exist, no GitHub identity can enroll. While no GitHub sign-in could
succeed (true before ADR-0029 under the default `invite` mode, unless the
operator sets `allowlist` with entries, or `open`), the provider is treated as
unconfigured:

* no login button;
* `/auth/login?provider=github` returns 404, indistinguishable from an unknown
  provider (SPEC-0013);
* cairnd logs a startup warning naming the variables that would enable it.

### Check order in the callback

After identity verification (unchanged), and **before** a session is created:

1. Is the identity already linked to an active user? Admit. (This applies only
   after ADR-0029.)
2. Is `CAIRN_ENROLLMENT_MODE=open`? Admit.
3. In `invite` mode: is the identity an operator, or does its verified email
   hold a pending invitation? Admit. Otherwise deny.
4. In `allowlist` mode: does a subject, email or `@<domain>` entry match? Admit.
5. For each `github-org:` entry, `GET /user/memberships/orgs/{org}` with the
   user's token:
   - `200` with `state: "active"` admits;
   - `404` (not a member) or `state: "pending"` means try the next organisation;
   - `403` means the organisation restricts third-party OAuth apps and has not
     approved Cairn's app. This is logged with its own reason, because the fix
     is an organisation owner approving the app, and the next organisation is
     then tried;
   - any other response, or a network error, **denies**. Fail closed.
6. Otherwise, deny.

A denial returns a generic 403 page ("This GitHub account is not permitted on
this Cairn instance") and creates no session. The page **reveals nothing** about
which organisations, users, teams or invitations exist. The log records the
GitHub login, the numeric id and the reason, never the token.

### Scopes

`read:org` is requested **only when** the mode is `allowlist` and
`CAIRN_ENROLLMENT_ALLOW` holds a `github-org:` entry. Every other instance keeps
`read:user user:email`, so the consent screen matches what the instance
actually checks.

### Security and tenancy

* The enrollment mode and allowlist are **operator** configuration. No user,
  team admin, token or request can change them.
* The gate controls who may have an account. What an account may do (teams,
  ownership, visibility) is ADR-0029's concern.
* Linking by verified email only is safe for enrollment, because a verified
  email proves control of the address. An allowlisted identity cannot acquire
  another user's account unless it controls that user's verified email.
* Offboarding is suspension, not re-checking. Someone who leaves an allowed
  organisation keeps their account until the operator suspends them. After
  ADR-0029 this is a deliberate trade for cross-product consistency and
  outage-resilience (option B was rejected), and the operator guide says so.

### How it composes with Switchboard and Harness

* **Switchboard** uses the same variables (`SWITCHBOARD_ENROLLMENT_MODE`,
  `SWITCHBOARD_ENROLLMENT_ALLOW`), mode names, default and allowlist syntax, and
  the same "gate enrollment, suspend to revoke" rule (Switchboard SPEC-0033). An
  operator configures both products with one mental model. Switchboard's GitHub login design carried the
  same "allow-list?" open question, and its SPEC-0033 answers it.
* **Harness** is unaffected. Harness agents authenticate to Cairn with MCP OAuth
  or tokens, never with GitHub login.

### Consequences

* Good, because enabling GitHub login can no longer admit strangers by omission.
  Open signup is an explicit, auditable setting.
* Good, because "my org" is one entry (`github-org:acme`), and "these people"
  pins immutable ids.
* Good, because operators meet the same enrollment model in Cairn and
  Switchboard.
* Good, because `read:org` is asked for only when it is used.
* Good, because the GitHub login docs can now be published, with a safe
  configuration as the documented default.
* Bad, because once persistent users exist (ADR-0029), leaving the organisation
  does not revoke Cairn access. The operator must suspend the user.
* Bad, because organisation checks depend on the organisation's OAuth app
  policy. An organisation with third-party restrictions must approve Cairn's app
  before its members can enroll. The 403 is logged distinctly, and the guide
  explains the fix.
* Bad, because a deployment that relied on #259's open behaviour stops admitting
  GitHub users until it sets `CAIRN_ENROLLMENT_MODE=allowlist` with entries, or
  `open`. No such deployment is documented, because the variables were never
  published. The startup warning names exactly what to set.
* Bad, because once users exist, the default `invite` mode on an instance that
  offers GitHub also gates new OIDC users. An operator who adds someone to their
  IdP either invites them or sets another mode. Switchboard has the same rule.
* Bad, because `sub` changes from the login to the numeric id for new GitHub
  sessions. Sessions are short-lived, and the migration is to let old sessions
  expire.

### Confirmation

Callback tests run against a fake GitHub. They cover:

* an allowlisted `https://github.com|<id>` entry after a simulated rename;
* verified-email and `@<domain>` entries;
* an active organisation member, a pending member, a non-member (404), and a
  restricted organisation (403);
* a network error on the membership call, which must deny;
* `open` mode;
* the default `invite` mode with GitHub configured and no invitations, where
  the provider must be absent and startup must succeed with a warning;
* `allowlist` mode with an empty allowlist, where the provider must be absent.

A test also asserts that `read:org` appears in the authorisation URL if and only
if a `github-org:` entry is configured in `allowlist` mode. The existing token-containment tests must still
pass, with no token appearing in logs on the denial paths.

## Pros and Cons of the Options

### A. Enrollment modes plus allowlist, gate enrollment, fail closed *(chosen)*

* Good, because it maps onto how GitHub-using teams are organised, and onto
  Switchboard's model.
* Good, because a GitHub outage cannot lock out existing users.
* Bad, because it needs one extra GitHub API call per organisation at enrollment,
  and the `read:org` scope when organisations are used.
* Bad, because offboarding requires an operator action (suspension).

### B. Re-check the allowlist on every sign-in

* Good, because leaving the organisation revokes access at the next login, with
  no operator action.
* Bad, because a GitHub API error at login locks out every existing user of an
  org-gated instance (fail closed cuts both ways).
* Bad, because it diverges from Switchboard's enrollment rule, so an operator
  running both products gets two behaviours for one configuration.
* Neutral, because an existing session outlives the membership change either way.
  Only the next login differs.

### C. Email-domain allowlist

* Good, because it needs no extra scope and no extra API call.
* Bad, because a verified email proves control of an address, not membership of
  anything. Former employees keep verified addresses on personal accounts, and
  many organisations' members use personal email addresses on GitHub.
* Neutral, because it is rejected as *the* mechanism, not forbidden:
  `CAIRN_ENROLLMENT_ALLOW` accepts `@<domain>` entries for parity with
  Switchboard, and an operator who uses one accepts this weakness, which the
  guide states beside it.

### D. Keep it open

* Good, because it needs no code.
* Bad, because the safe state depends on every operator reading a warning, and
  the unsafe state is the default. That is the inverse of the driver.

## Architecture Diagram

```mermaid
flowchart TD
    CB["/auth/callback?provider=github"] --> V[verify state, exchange code,<br/>GET /user + /user/emails<br/>ADR-0019, unchanged]
    V --> L{identity linked to an<br/>active user? ADR-0029}
    L -- yes --> SESS[create session]
    L -- no, enrollment --> M{CAIRN_ENROLLMENT_MODE}
    M -- open --> SESS
    M -- invite --> INV{operator, or a pending<br/>invite for the email?}
    INV -- yes --> SESS
    INV -- no --> DENY
    M -- allowlist --> U{subject, email or @domain<br/>in CAIRN_ENROLLMENT_ALLOW?}
    U -- yes --> SESS
    U -- no --> ORG{each github-org: entry:<br/>GET /user/memberships/orgs/org}
    ORG -- 200 active --> SESS
    ORG -- 404 / pending / 403 --> ORG
    ORG -- other error --> DENY
    ORG -- none left --> DENY[generic 403, no session,<br/>log login + id + reason]
```

## More Information

* **Extends ADR-0019.** This reverses SPEC-0013's design default ("any
  verified-email GitHub account may log in") and resolves that open question.
  SPEC-0013 is amended in the same change (new requirements AL-1 to AL-6).
* SPEC-0013 is the GitHub login spec's number on `main`. The renumbering in
  `fix/spec-0013-collision` merged as #257 and moved embedded docs to
  SPEC-0015, so SPEC-0013 is stable, and it is amended rather than replaced.
* **Related records** (front-matter edges): Cairn ADR-0029 / SPEC-0023 (Teams
  and tenancy: users, identities, invitations, suspension). Cross-repo, cited
  in prose: Switchboard SPEC-0033 (teams and tenancy, including
  `SWITCHBOARD_ENROLLMENT_MODE` and `SWITCHBOARD_ENROLLMENT_ALLOW`, aligned in
  its PR #357).
* Design review, Joe, 2026-09-22: the mode is operator config,
  `CAIRN_ENROLLMENT_MODE`, defaulting to `invite` when GitHub login is
  configured, with the same values and semantics as Switchboard. There are no
  deprecation windows or back-compat shims.
* GitHub REST: "Get an organization membership for the authenticated user"
  (`GET /user/memberships/orgs/{org}`, which returns `state` of `active` or
  `pending`, or 404 when the user is not a member).
