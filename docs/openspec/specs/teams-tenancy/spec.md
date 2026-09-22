---
status: accepted
date: 2026-09-22
implements: [ADR-0029]
requires: [SPEC-0001, SPEC-0002, SPEC-0007, SPEC-0009, SPEC-0012, SPEC-0013]
related: [SPEC-0005, SPEC-0006, SPEC-0008, SPEC-0014, SPEC-0016, SPEC-0020, SPEC-0021, SPEC-0022]
---

# SPEC-0023: Teams and Tenancy

## Graph Edges

- **Implements:** **ADR-0029** — every artifact belongs to a user or a team; the operator owns none
- **Requires:** **SPEC-0009** — link-based access, visibility and retention, which this spec enforces and extends
- **Requires:** **SPEC-0012** — outbound webhooks, whose delivery targets become owned subscriptions; this spec replaces its REQ "Delivery Targets from Configuration"
- **Requires:** **SPEC-0001**, **SPEC-0002**, **SPEC-0007**, **SPEC-0013** — the Bin, the artifact core, MCP OAuth and GitHub login, each of which gains an owner or a team argument
- **Related:** **SPEC-0005**, **SPEC-0006**, **SPEC-0008**, **SPEC-0014** — hooks, annotations, the CLI and metrics, touched by the audit fixes
- **Related:** **SPEC-0016**, **SPEC-0020**, **SPEC-0021**, **SPEC-0022** — events, permanent retention, receipts and search, which take their owner model and `authorizeRead` from this spec

## Overview

Cairn is multi-tenant. This spec, per ADR-0029, makes the owner of every artifact explicit and adds
**teams** as the second kind of owner, realising the workspace ADR-0001 named and left thin. It
defines:

* two **profiles**: the **operator**, who runs the instance, and **users**, who sign in and use it;
* **users as rows**, so ownership keys on something a user cannot claim;
* the **owner model**: every artifact is owned by exactly one user or one team;
* **teams**: creation, roles (`owner`, `admin`, `member`), invitations, and optional OIDC group sync;
* **visibility** `link`, `team` or `private`, enforced on every read surface (closing #182);
* a **team view** in the Bin;
* **owned outbound subscriptions** replacing the instance-wide target list (F-C8, closing #185), and
  the removal of `CAIRN_OUTBOUND_WEBHOOK_URLS` and `CAIRN_OUTBOUND_WEBHOOK_SECRET` in the same change;
* static API tokens mapped to a user, quotas for permanent retention, and fixes for the other
  unscoped surfaces found on `main` at `dea1f0b`.

Cairn is pre-1.0. Nothing this spec replaces gets a deprecation window, a transition warning or a
back-compat path: the old behaviour is removed in the change that ships the new one, and the
CHANGELOG's upgrade notes say what an operator must do (design review, Joe, 2026-09-22).

Records that take their owner model from this spec (front-matter edges where they are specs):
ADR-0022 and SPEC-0016 (annotation and trace events), ADR-0023 (redaction), ADR-0024 (GitHub
allowlist), ADR-0026 and SPEC-0020 (permanent retention), ADR-0027 (receipts), ADR-0028 (search).
Switchboard's SPEC-0033 defines the same profiles, roles, invites and group sync for that product.

Terms:

* **Operator**: a user whose session provenance is listed in `CAIRN_OPERATORS`, or who carries
  `CAIRN_OPERATOR_GROUP`.
* **Workspace**: a user's personal space, or a team. Every artifact is in exactly one.
* **Reader**: whoever is asking to read: a signed-in user, an agent acting for one, or anonymous.

## Requirements

### Requirement: Operator and User Profiles

Cairn MUST distinguish two profiles. A user is an **operator** if and only if their session's
recorded provenance, written `<issuer>|<subject>`, appears in the comma-separated `CAIRN_OPERATORS`,
or `CAIRN_OPERATOR_GROUP` is set and their OIDC session's groups claim contains it. An operator MUST
also be a user and MUST own their personal artifacts exactly as any user does. With neither variable
set the instance MUST have no operator, and operator routes MUST answer `404`. Switchboard's
`SWITCHBOARD_OPERATORS` uses the same format.

#### Scenario: Operator by provenance

- **WHEN** `CAIRN_OPERATORS=https://id.example.com|abc-123` and that identity signs in
- **THEN** the operator console is available to them and their Bin shows only their own and their
  teams' artifacts

#### Scenario: No operator configured

- **WHEN** neither variable is set
- **THEN** every operator route answers `404` and every user surface works normally

### Requirement: Operator Surfaces Bound Tenant Data and Never Read It

Operator surfaces MUST be limited to: a directory of users and teams with counts (artifacts, bytes,
permanent bytes, subscriptions); per-user and per-team quotas; login policy (owned by ADR-0024 and
SPEC-0013's enrollment gate, `CAIRN_ENROLLMENT_MODE`); suspending and unsuspending a user or team;
retention bounds; and the metrics scrape credential SPEC-0014 requires. No operator surface MAY
return an artifact's body, title, link, tags, annotations, captured requests or subscription
targets. Operator status MUST NOT widen `authorizeRead`.

Instance configuration MAY bound tenant data (size, retention, rate, who may sign in) and MUST NOT
route, copy or reveal it: no environment variable or setting MAY name a destination that receives
other owners' artifacts or events. There is no exception (REQ "Removing the Instance-Wide Outbound
Targets").

Every operator action that affects a tenant MUST write an audit row (operator, target, action,
required reason, time) in the same transaction, readable by the affected user or the affected team's
owners.

#### Scenario: Operator opens a private artifact's link

- **WHEN** the operator requests another user's `private` artifact by id on any surface
- **THEN** the response is the uniform `404`

#### Scenario: Suspending a user offboards them

- **WHEN** the operator suspends user U with a reason
- **THEN** U's sessions, personal access tokens, OAuth grants and refresh tokens stop authenticating
  on the next request, U's team memberships stop conferring access, and U sees the audit row after
  reinstatement

### Requirement: Users and Identities

Cairn MUST keep a `users` table and a `user_identities` table with one row per `(issuer, subject)`
that has signed in. A sign-in MUST resolve its identity by `(issuer, subject)`. A new identity MUST
link to an existing user only when the provider asserts a verified email equal (after lower-casing)
to that user's primary email; otherwise it MUST create a new user. The OIDC callback MUST ignore an
email whose `email_verified` claim is not true. GitHub identities MUST key on the numeric account id.

Every owner and actor reference MUST be a user id. Display surfaces MAY show the user's email to
themselves and to members of a shared team, and MUST show a display handle, not an email, to
anonymous readers.

The dev password login MUST be disabled whenever any production provider (OIDC or GitHub) is
configured, and `CAIRN_DEV_INSECURE_BEARER_AUTH` MUST refuse to start when `CAIRN_BASE_URL` is
`https`.

#### Scenario: Pocket ID and GitHub, same verified email

- **WHEN** a user signs in with Pocket ID as `joe@example.com`, and later with a GitHub account whose
  primary verified email is `Joe@Example.com`
- **THEN** both identities belong to the same user and see the same artifacts

#### Scenario: Unverified email cannot claim a user

- **WHEN** an OIDC identity presents `email: victim@example.com` with `email_verified: false`
- **THEN** a new user is created, and the victim's artifacts are not reachable by it

#### Scenario: Dev login on a GitHub-only deployment

- **WHEN** GitHub login is configured, OIDC is not, and `CAIRN_DEV_LOGIN_PASSWORD` is set
- **THEN** the dev login route answers `404`

### Requirement: Owner Model

Every artifact MUST be owned by exactly one user (`owner_user_id`) or one team (`owner_team_id`),
enforced by a database constraint, and MUST record `created_by_user_id`, which MUST NOT confer any
permission. Runs, hooks, bundle members, spans and captured hook requests MUST inherit the owner of
their artifact. Personal access tokens, sessions, OAuth grants and MCP sessions MUST be owned by a
user. Outbound subscriptions MUST be owned by a user or a team. Nothing MAY be owned by the operator
or by the instance.

#### Scenario: Constraint rejects two owners

- **WHEN** a write sets both `owner_user_id` and `owner_team_id` on an artifact
- **THEN** the database rejects it

#### Scenario: A run follows its artifact

- **GIVEN** a trace run created into team T
- **THEN** its spans are readable by exactly the readers of the run's artifact

### Requirement: Teams and Roles

Any user MAY create a team, up to the operator's per-user ceiling (default 10), and MUST become its
first owner. A team MUST have an instance-unique slug (3–40 lowercase letters, digits and single
hyphens) and a display name. A team MUST always have at least one owner.

Roles MUST grant:

| Action | member | admin | owner |
|---|---|---|---|
| Create artifacts, bundles, runs and hooks in the team | yes | yes | yes |
| Read and annotate every team artifact; see the team Bin | yes | yes | yes |
| Change sharing, expiry, rotate or delete a team artifact they created | yes | yes | yes |
| Change sharing, expiry, rotate or delete any team artifact; moderate comments on team artifacts | no | yes | yes |
| Mark team artifacts permanent within the team quota | no | yes | yes |
| Create, edit, rotate and delete team outbound subscriptions | no | yes | yes |
| Invite and remove members and admins; revoke invites | no | yes | yes |
| Grant `owner`; rename or delete the team | no | no | yes |

Team existence MUST NOT be observable by non-members: team routes MUST answer `404` to a user who is
neither a member nor the addressee of a pending invite. Deleting a team MUST require the owner to
retype the slug and MUST delete the team's artifacts and subscriptions.

#### Scenario: Member deletes another member's artifact

- **WHEN** member B deletes a team artifact created by member A
- **THEN** the request is refused with `403` and the artifact is unchanged

#### Scenario: Admin moderates a comment

- **WHEN** an admin of T deletes a comment on a T artifact written by someone else
- **THEN** the comment is removed and the removal is recorded with the admin as actor

### Requirement: Team Invitations

An admin or owner MAY invite an email address with a role no higher than their own. Cairn MUST mint
an unguessable single-use token of at least 128 bits, store only its hash, and expire it after 7
days. Accepting MUST require a signed-in user whose verified email equals the invite address
(case-insensitive); any other caller, and any accepted, revoked or expired token, MUST receive
`404`. Whether an invited email already has an account MUST NOT be revealed to the inviter. These
semantics MUST match Switchboard's SPEC-0033 REQ "Team Invitations".

When the enrollment mode is `invite` (ADR-0024, `CAIRN_ENROLLMENT_MODE`, the default when GitHub
login is configured), a pending invite to a verified email MUST admit that identity as a new user;
this spec supplies the invitations that mode reads and does not otherwise define who may sign in.

#### Scenario: Accepting

- **WHEN** the addressee opens a valid invite and accepts
- **THEN** they become a member with the invited role and the token is consumed

#### Scenario: Invite admits a new user under invite-only enrollment

- **GIVEN** `CAIRN_ENROLLMENT_MODE=invite` and a pending invite to `friend@example.com`
- **WHEN** a GitHub identity whose primary verified email is `friend@example.com` signs in for the
  first time
- **THEN** a user is created and the invite page is offered

#### Scenario: Forwarded invite

- **WHEN** a different signed-in user opens the same link
- **THEN** the page answers `404` and the invite stays pending

### Requirement: Membership Changes Take Effect Immediately

Removal, demotion, suspension or leaving MUST take effect on the reader's next request on every
surface, including open SSE streams, which MUST be closed within one keep-alive interval. A user who
leaves a team MUST lose access to every artifact owned by that team, including artifacts they
created there, except through that artifact's `link` visibility.

#### Scenario: Removed member's open stream

- **GIVEN** member B is watching a live `team`-visibility run
- **WHEN** an admin removes B
- **THEN** B's stream closes and reconnecting answers `404`

### Requirement: Visibility Values

An artifact's visibility MUST be one of `link`, `team` or `private`, constrained by its owner:

| Owner | Allowed | Default at creation |
|---|---|---|
| user | `link`, `private` | `link` |
| team | `link`, `team` | `team` |

A request to set a value not allowed for the owner MUST be refused with `400 invalid_visibility`.
Changing visibility MUST follow the permissions of REQ "Teams and Roles" (the owner for a personal
artifact; the creator or an admin for a team artifact). Agents MUST NOT change visibility (ADR-0007:
agents hold no `sharing:manage`).

#### Scenario: Team artifact defaults to team

- **WHEN** an agent creates an artifact with `team: "stump"`
- **THEN** its visibility is `team`

#### Scenario: Private on a team artifact

- **WHEN** a team admin sets visibility `private` on a team artifact
- **THEN** the request is refused with `400 invalid_visibility`

### Requirement: Read Authorization on Every Surface

A single store-level `authorizeRead(reader, artifact)` MUST decide every read, and MUST allow the
read if and only if:

* visibility is `link` and the reader presents the artifact's current id; or
* visibility is `private` and the reader is the owning user (or an agent acting for them); or
* visibility is `team` and the reader is a current, unsuspended member of the owning team (or an
  agent acting for one).

Every surface that returns an artifact's body, metadata, members, annotations, span outputs,
captured requests or live stream MUST call it, including: the web shell and bundle panes;
downloads; `GET /v1/artifacts/{id}` and its `/body`, `/members/*`, `/annotations`, `/reactions`;
`/v1/runs/{id}*` and `/v1/hooks/{id}*` reads and streams; MCP `artifact_read`, the resources and
their subscriptions, and every A2UI resource; the Bin; produced-artifact links on runs; search
(ADR-0028); and receipts (ADR-0027). A failed check MUST produce the same `404` body, status and
timing class as an unknown or expired id. Commenting and reacting MUST require that the reader pass
`authorizeRead`. This closes #182.

#### Scenario: Anonymous read of a private artifact (the #182 reproduction)

- **GIVEN** an artifact whose owner set visibility `private`
- **WHEN** an anonymous client requests `GET /v1/artifacts/{id}`
- **THEN** the response is `404`, identical to an unknown id

#### Scenario: Agent reads a private artifact for someone else

- **WHEN** an agent authorized by user B calls `artifact_read` on user A's `private` artifact
- **THEN** the call returns `not_found`

#### Scenario: Member reads a team artifact

- **WHEN** a member of T, signed in, opens a `team` artifact of T
- **THEN** it renders, with its annotations

#### Scenario: Comment on an unreadable artifact

- **WHEN** a signed-in user who cannot read a `private` artifact posts a comment to it
- **THEN** the request returns `404` and nothing is stored

#### Scenario: Produced link to a private artifact

- **GIVEN** a public run whose span records producing artifact X, and X is `private`
- **WHEN** an anonymous reader opens the run
- **THEN** the span shows that an artifact was produced and does not show X's id or title

### Requirement: Moving an Artifact Into a Team

A user MAY move a personal artifact they own into a team they belong to. The move MUST set
`owner_team_id`, clear `owner_user_id`, keep `created_by_user_id`, change visibility `private` to
`team` (and leave `link` as `link`), re-charge any permanent retention to the team (REQ "Quotas for
Permanent Retention"), and emit subsequent events to the team's subscriptions. No surface MAY move a
team artifact to a personal workspace or to another team.

#### Scenario: Move in

- **WHEN** U moves their `private` artifact into team T
- **THEN** it is a T artifact with visibility `team`, and U can still change its sharing as its
  creator

#### Scenario: Move out refused

- **WHEN** a team admin asks to move a team artifact to their personal workspace
- **THEN** the request is refused with `400 move_out_of_team`

### Requirement: Team View in the Bin

The Bin MUST offer a workspace switcher listing *Personal* and each team the user belongs to. A team
view MUST list the team's artifacts (keyset-paginated and tag-filterable as the personal Bin is).
`GET /v1/bin` MUST accept `workspace=personal` (default) or `workspace=team:<slug>`. `cairn ls` MUST
accept `--team <slug>`. MCP `artifact_create`, `bundle_create` and `run_create` MUST accept an
optional `team` argument naming a team the authorizing user belongs to; any other value MUST fail
with `not_found`.

#### Scenario: Team Bin

- **WHEN** a member opens the Bin and switches to `stump`
- **THEN** they see every `stump` artifact, including ones other members created

#### Scenario: Agent creates into a team its human is not in

- **WHEN** an agent calls `artifact_create` with `team: "other"` and its user is not a member
- **THEN** the call fails with `not_found` and nothing is created

### Requirement: Owned Outbound Subscriptions

Outbound delivery targets MUST be `outbound_subscriptions` owned by a user or a team. A user MUST be
able to create, list, rotate the secret of, pause and delete their own subscriptions in Settings and
over `/v1`; team admins and owners MUST be able to do the same for the team's. Each subscription MUST
have a target URL, a secret, optional filters (event types, share types, tags), and health: last
attempt, last status, consecutive failures. The secret MUST be either supplied by the creator (at
least 32 bytes, for a receiver that issues its own signing secret, as a Switchboard `cairn` webhook
does) or minted by Cairn; it MUST be shown once and stored encrypted. After 20 consecutive
failed deliveries a subscription MUST be disabled and say so. Per-owner ceilings MUST apply (default
5 per user, 10 per team). Deliveries MUST be signed per SPEC-0012 REQ "Signed Delivery" with the
subscription's own secret.

#### Scenario: A user wires their own Switchboard

- **WHEN** U creates a subscription to their Switchboard webhook URL and then creates an artifact
- **THEN** one signed `artifact.created` delivery reaches that URL, verifiable with the secret U was
  shown

#### Scenario: Receiver-issued secret

- **WHEN** U creates a subscription and supplies the signing secret their Switchboard webhook issued
- **THEN** deliveries verify at that webhook with no change on the Switchboard side

#### Scenario: Member tries to add a team subscription

- **WHEN** a team member (not admin) creates a subscription for team T
- **THEN** the request is refused with `403`

### Requirement: Events Go Only to the Artifact's Workspace

An event about an artifact — `artifact.created` (SPEC-0012), and the annotation and trace events of
ADR-0022 and SPEC-0016 — MUST be delivered only to the subscriptions of the workspace that owns the
artifact at the time of the event, never to the actor's own subscriptions when the actor is someone
else, and never to any subscription of another workspace. Creation of runs and hooks MUST emit
`artifact.created` like any other share type.

#### Scenario: A friend comments on U's artifact

- **GIVEN** user V has a subscription, and V comments on U's `link` artifact
- **WHEN** the annotation event is emitted
- **THEN** it reaches U's subscriptions and not V's

#### Scenario: Team artifact

- **WHEN** a member creates an artifact in team T
- **THEN** `artifact.created` reaches T's subscriptions and none of the member's personal ones

### Requirement: Subscription Target Safety

A subscription target MUST be validated when it is created and again immediately before every dial:
HTTPS only unless the operator sets `CAIRN_OUTBOUND_ALLOW_HTTP`; loopback, private, link-local and
unique-local addresses refused; the resolved address re-checked at dial time to defeat DNS
rebinding. Redirects MUST NOT be followed; a 3xx is a failed delivery. Each attempt MUST time out
after 5 seconds. Delivery MUST NOT block artifact creation.

#### Scenario: Rebinding target

- **WHEN** a target resolves to a public address at creation and to `10.0.0.5` at delivery
- **THEN** the delivery is not dialled and counts as a failure

#### Scenario: Redirect

- **WHEN** a target answers `302` to another host
- **THEN** the redirect is not followed and the delivery counts as failed

### Requirement: Removing the Instance-Wide Outbound Targets

The change that ships REQ "Owned Outbound Subscriptions" MUST remove `CAIRN_OUTBOUND_WEBHOOK_URLS`
and `CAIRN_OUTBOUND_WEBHOOK_SECRET` entirely: their parsing, the delivery path that reads them, and
every reference in `.env.example`, the compose files, `DEPLOY.md` and the website guides. The
CHANGELOG MUST carry an upgrade note naming both variables as removed and pointing to subscriptions.
There MUST NOT be a narrowed stage, a deprecation or transition warning, an import command, or any
release in which env targets and subscriptions both deliver. This replaces SPEC-0012 REQ "Delivery
Targets from Configuration" and supersedes ADR-0017's instance-wide delivery; SPEC-0012's payload,
signing and retry requirements apply to subscription deliveries unchanged.

#### Scenario: A leftover variable delivers nothing

- **GIVEN** a deployment upgraded with `CAIRN_OUTBOUND_WEBHOOK_URLS` still set
- **WHEN** any user, the operator included, creates an artifact tagged `handoff`
- **THEN** no request is made to that URL, and cairnd starts and runs normally

#### Scenario: No code reads the variables

- **WHEN** the Go source is searched for either variable name
- **THEN** there is no match, and a test asserts it

#### Scenario: The operator replaces their firehose

- **WHEN** the operator creates a subscription they own to their Switchboard webhook, supplying that
  webhook's signing secret
- **THEN** their artifacts reach it signed as before, and no other user's artifact does

### Requirement: Static API Tokens Act as an Operator's User

`CAIRN_API_TOKENS` entries MUST have the form `secret:<user>[:agent|:human]`, where `<user>` is an
`<issuer>|<subject>` or verified email that MUST resolve at boot to an existing user who is an
operator. An entry naming any other user MUST fail boot with an error naming the entry's position
(never its secret). Tokens MUST default to agent scopes; `:human` MUST NOT grant `sharing:manage`
unless the user is an operator acting on their own artifacts. Each token MUST appear on its user's
Settings page as operator-provisioned. Legacy free-form `secret:actor` entries MUST fail boot from
the release that ships this requirement, with no grace period; the error MUST name the entry's
position and the new form, and the CHANGELOG MUST say so.

#### Scenario: Token naming a non-operator

- **WHEN** `CAIRN_API_TOKENS` contains an entry naming a user who is not an operator
- **THEN** cairnd refuses to start and logs "CAIRN_API_TOKENS entry 2: user is not an operator"

#### Scenario: Legacy entry refused

- **WHEN** `CAIRN_API_TOKENS` contains a free-form `secret:ci-bot` entry
- **THEN** cairnd refuses to start and names that entry's position and the `secret:<user>[:agent|:human]`
  form, never the secret

#### Scenario: Token cannot impersonate

- **WHEN** a valid operator token creates an artifact
- **THEN** the artifact is owned by the operator's user, never by a string of the operator's choosing

### Requirement: Quotas for Permanent Retention

Permanent retention (ADR-0026, SPEC-0020) is off unless the operator enables it (SPEC-0020 REQ-2),
and MUST be charged to the artifact's owner. The operator MAY set per-user and per-team permanent
quotas, by count and by bytes, with SPEC-0020's variables (`CAIRN_PERMANENT_USER_MAX_COUNT`,
`CAIRN_PERMANENT_USER_MAX_BYTES`, `CAIRN_PERMANENT_TEAM_MAX_COUNT`, `CAIRN_PERMANENT_TEAM_MAX_BYTES`),
and MAY override either for one owner, audited. **A quota that is not set MUST NOT limit anything.**
Only the owner of a personal artifact, or an admin or owner of the team, MAY mark an artifact
permanent. A request that would exceed a configured quota MUST be refused with `409` and reason
`retention_quota_exceeded` (SPEC-0020 REQ-5), and a move into a team that would exceed the team's
configured quota MUST be refused the same way.

#### Scenario: Team over quota

- **GIVEN** the operator set a per-team count quota, and team T has used all of it
- **WHEN** an admin marks another T artifact permanent
- **THEN** the request fails with `409 retention_quota_exceeded` and the artifact keeps its expiry

#### Scenario: No team quota configured

- **GIVEN** retention is enabled and no per-team quota is set
- **WHEN** an admin of T marks a 51st T artifact permanent
- **THEN** it succeeds

#### Scenario: Member cannot mark permanent

- **WHEN** a member of T marks a T artifact permanent
- **THEN** the request is refused with `403`

### Requirement: OIDC Group Sync

Group sync MUST be off unless the operator sets `CAIRN_OIDC_GROUPS_CLAIM`; when set, Cairn MUST
request the claim at OIDC sign-in. A team owner MAY link the team to one group they themselves carry,
conferring `member` or `admin`, never `owner`. At each OIDC sign-in, group-sourced memberships MUST be
added and removed to match the claim. Manual memberships MUST NOT be changed by sync; GitHub sign-ins
MUST NOT change group-sourced memberships; sync MUST NOT remove a team's last owner. These semantics
MUST match Switchboard's SPEC-0033 REQ "OIDC Group Sync".

#### Scenario: Group removed at the IdP

- **GIVEN** team T linked to group `family`
- **WHEN** a user's next OIDC sign-in no longer carries `family`
- **THEN** their group-sourced membership of T is removed and T's `team` artifacts answer `404` to
  them

### Requirement: Closing the Audited Surfaces

Each finding in design.md's Audit Findings table MUST be fixed as stated there and MUST gain a test
that fails against the unfixed code.

#### Scenario: Read-only token writes a run (A7)

- **WHEN** a personal access token without `artifacts:write` calls `POST /v1/runs` or `POST /v1/hooks`
- **THEN** the request is refused with `403`

#### Scenario: A sender cannot read other senders' requests (A8)

- **GIVEN** a hook whose ingress URL was given to two third-party senders
- **WHEN** one sender requests `GET /v1/hooks/{id}/requests` with only the ingress id
- **THEN** the response is `404`; captured requests are readable only through the hook's read id by
  readers who pass `authorizeRead`

#### Scenario: Rotation defeats a stale produced link (A9)

- **WHEN** user V records a span producing U's artifact without owning it
- **THEN** the append is refused with `404`

#### Scenario: Owner removes a comment on their artifact (A10)

- **WHEN** U deletes a comment another user left on U's artifact
- **THEN** the comment is removed

#### Scenario: `on_behalf_of` cannot be asserted over REST (A11)

- **WHEN** a REST client sends `on_behalf_of: "Joe Stump"` when creating a comment
- **THEN** the stored value is derived by the server from the authenticated client, not the request

#### Scenario: Unverified OAuth client on the consent screen (A14)

- **WHEN** a dynamically registered client named "Claude Code" requests consent
- **THEN** the consent screen shows its redirect origin and marks it unverified

#### Scenario: Hook capture respects the owner's ceiling (A15)

- **WHEN** a user creates a hook with `request_cap` above the operator's ceiling
- **THEN** the cap is clamped to the ceiling and the response says so

#### Scenario: Public read hides the creator's email (A18)

- **WHEN** an anonymous reader opens a `link` artifact
- **THEN** the creator is shown by display handle, not email

#### Scenario: MCP session bookkeeping is owner-scoped (A19)

- **WHEN** a session id collides across owners
- **THEN** neither owner's `mcp_sessions` row is overwritten

#### Scenario: Settings needs a browser session (A20)

- **WHEN** an agent bearer token requests `/settings`
- **THEN** the request is refused, and a browser session there lists every PAT, OAuth grant and
  operator-provisioned token that acts as the user

#### Scenario: Stream subscription checks read access (A24)

- **WHEN** an MCP client subscribes to the stream of a run it cannot read
- **THEN** the subscription is refused with `not_found`

#### Scenario: OTLP trace ids are owner-scoped (A23)

- **WHEN** OTLP ingest (ADR-0015) lands and two owners export the same `trace_id`
- **THEN** each owner gets their own run

### Requirement: Migration to Explicit Ownership

The migration MUST create one user per distinct existing `owner_id` and `actor_id` string (recorded
as an unverified legacy email), set `owner_user_id` on every artifact from `owner_id`, and create no
team. A user's first sign-in whose verified email equals a legacy row's string MUST claim that row.
Existing `link` artifacts MUST remain `link`; existing `private` artifacts MUST remain `private` and
become unreadable to everyone but their owner. Every Bin listing for a user with no teams MUST
return the same artifacts it returned before.

The same migration MUST drop the legacy `owner_id` and `actor_id` string columns it backfilled from,
and MUST rebuild every index or uniqueness key that named them (including SPEC-0016 EV-6's reaction
key) on the matching `user_id` column. No release MAY carry both the strings and the user columns.
The migration MUST run as one transaction: if the per-user Bin comparison differs, it MUST abort and
leave the previous schema intact. Wire fields named `actor_id` keep their name and are rendered from
the user row.

#### Scenario: Existing owner signs in

- **GIVEN** artifacts owned by the string `joe@example.com` before the upgrade
- **WHEN** a user signs in with verified email `joe@example.com`
- **THEN** those artifacts are theirs, and their Bin is unchanged

#### Scenario: Legacy owner strings are gone

- **WHEN** the migration to explicit ownership completes
- **THEN** `artifacts.owner_id` and every other backfilled `owner_id` / `actor_id` string column no
  longer exists, and no query or index names them

#### Scenario: A failed comparison leaves the old schema

- **WHEN** the backfill would change any user's Bin listing
- **THEN** the migration aborts, nothing is dropped, and `cairnd` refuses to start with an error
  naming the first mismatched owner

### Requirement: Re-Identifying Existing Private Artifacts

Before this spec, `private` was not enforced on any read path (#182), so every existing `private`
artifact was effectively link-visible. The release that first enforces `private` MUST give every
artifact that is `private` at that moment a new id, exactly as SPEC-0009 REQ "Id Rotation as
Revoke-a-Leaked-Link" rotates one: the old id MUST return the uniform `404`, and MUST NOT be reused
within TTL-plus-grace. The re-identification MUST run once, in the same release as enforcement, and
MUST be recorded in the operator audit log with the count of artifacts it re-identified and no ids.
The owner's Bin MUST list each artifact under its new id.

Opt-in permanent retention (SPEC-0020) MUST NOT ship before this release, so no permanent
artifact is ever re-identified (SPEC-0020 REQ-8 refuses rotation while permanent).

#### Scenario: A link sent before enforcement stops resolving

- **GIVEN** a `private` artifact whose link was shared before the upgrade
- **WHEN** anyone, including its owner, opens the old link after the upgrade
- **THEN** the response is the uniform `404`

#### Scenario: The owner finds it under its new id

- **WHEN** the owner opens their Bin after the upgrade
- **THEN** the artifact is listed under a new id and renders for them

#### Scenario: Link artifacts keep their ids

- **WHEN** the upgrade runs
- **THEN** every `link` artifact keeps its id

### Requirement: Database Operation Standards

Membership changes, team deletion, moves into a team, quota checks with the change they guard, and
every operator action with its audit row MUST each run in one transaction. All queries MUST be
parameterized; `authorizeRead` MUST be expressed as a parameterized predicate, never string-built
SQL.

#### Scenario: Quota check races a second request

- **WHEN** two admins mark two artifacts permanent at once and only one fits the quota
- **THEN** exactly one succeeds

## HTTP Endpoints

| Method | Path | Purpose | Auth |
|---|---|---|---|
| POST | `/v1/teams` | Create a team | Required (session or token with `teams:manage`) |
| GET | `/v1/teams` | List the caller's teams | Required |
| GET | `/v1/teams/{slug}` | Team detail | Required, member |
| PATCH | `/v1/teams/{slug}` | Rename | Required, owner |
| DELETE | `/v1/teams/{slug}` | Delete team | Required, owner, browser session |
| GET | `/v1/teams/{slug}/members` | List members | Required, member |
| PATCH | `/v1/teams/{slug}/members/{user}` | Change role | Required, admin (owner for owner) |
| DELETE | `/v1/teams/{slug}/members/{user}` | Remove or leave | Required, admin, or self |
| POST | `/v1/teams/{slug}/invites` | Invite | Required, admin |
| GET | `/v1/teams/{slug}/invites` | List pending invites | Required, admin |
| DELETE | `/v1/teams/{slug}/invites/{id}` | Revoke invite | Required, admin |
| GET | `/invites/{token}` | Invite page | Required (session), addressee |
| POST | `/invites/{token}/accept` | Accept | Required (session), verified email match |
| PUT | `/v1/teams/{slug}/group-link` | Link or unlink an OIDC group | Required, owner |
| POST | `/v1/artifacts/{id}/move` | Move a personal artifact into a team | Required, owner of the artifact, member of the team |
| GET | `/v1/subscriptions` | List the caller's and their admin teams' subscriptions | Required |
| POST | `/v1/subscriptions` | Create a subscription (secret shown once) | Required; admin for a team owner |
| PATCH | `/v1/subscriptions/{id}` | Pause, resume, change filters | Required, subscription owner scope |
| POST | `/v1/subscriptions/{id}/rotate` | Rotate the secret | Required, subscription owner scope |
| DELETE | `/v1/subscriptions/{id}` | Delete | Required, subscription owner scope |
| GET | `/operator` | Operator console | Required, operator |
| GET | `/v1/operator/directory` | Users and teams with counts | Required, operator |
| POST | `/v1/operator/suspensions` | Suspend or unsuspend | Required, operator |
| PUT | `/v1/operator/quotas/{scope}` | Per-owner quota | Required, operator |

No endpoint in this spec is public. `GET /v1/bin` gains `workspace`; `POST /v1/artifacts`, runs and
hooks gain an optional `team`.

## Security Requirements

This is a web-facing spec. Topics not restated follow SPEC-0007, SPEC-0009 and SPEC-0013.

- **Authentication**: every route above requires a signed-in user or a token acting for one;
  team-destructive and operator routes require a browser session, not a bearer token.
- **Authorization**: `authorizeRead` and the role table are enforced server-side on every request;
  what the UI renders is never the check.
- **Enumeration**: team slugs, invite tokens, subscription ids and private artifacts answer the
  uniform `404` outside reach.
- **Rate limiting**: invite creation (20 per team per hour), invite acceptance (10 per session per
  minute) and subscription creation (10 per owner per hour) use the existing limiter.
- **Security headers**: unchanged app-shell middleware; no new inline script.
- **Request body size limits**: team, invite and subscription bodies are capped at 16 KiB.
- **CSRF protection**: every state-changing browser route uses the existing CSRF middleware; invite
  acceptance is a POST.
- **Redirect validation**: after accepting an invite the redirect target is a same-origin path from
  an allow-list, never a query parameter. Outbound deliveries never follow redirects (REQ
  "Subscription Target Safety").
- **Secrets**: invite tokens and subscription secrets are stored hashed or encrypted at rest and are
  shown once; static tokens never appear in logs.

## Accessibility Requirements

This spec adds UI to the app shell. The following are mandatory per WCAG 2.1 AA.

- **WCAG 2.1 AA compliance**: the workspace switcher, team pages, invite page, subscriptions page and
  operator console MUST meet Level AA.
- **ARIA landmarks**: new pages keep the shell's `banner`, `navigation`, `main` and `contentinfo`
  landmarks.
- **Icon-only controls**: remove-member, revoke-invite, rotate-secret and pause-subscription buttons
  that show only an icon MUST carry an `aria-label` naming what they act on.
- **Dynamic content regions**: subscription health and membership lists that refresh in place MUST
  use `aria-live="polite"`; a disabled-subscription notice MUST use `aria-live="assertive"`.
- **Keyboard navigation**: the workspace switcher MUST work with arrow keys and Enter, and Escape
  MUST close it.
- **Focus management**: the delete-team, move-to-team and one-time-secret dialogs MUST trap focus,
  focus their first control on open, and return focus to the trigger on close.
