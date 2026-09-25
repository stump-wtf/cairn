-- Explicit ownership (ADR-0029, SPEC-0023 REQ "Owner Model", REQ "Migration
-- to Explicit Ownership").
--
-- Until now every owner and actor was a free-form string: artifacts.owner_id
-- and actor_id, comments and reactions actor_id, the credential tables'
-- owner_id / actor_id. This migration resolves every distinct string to a
-- users row, points each of those tables at users(id), and drops the strings
-- in the same transaction. No release carries both (design review
-- 2026-09-22).
--
-- One user per distinct string, resolved in this order (first match wins):
--
--   1. "user:<uuid>" naming an existing user: the interim key #326 gave a
--      user with no usable email or subject.
--   2. The primary email of a user who signed in with it verified (#326).
--   3. The actor a #326 session recorded for its user (a subject or a dev
--      login name).
--   4. Otherwise a new, unverified legacy user whose actor_key is the string.
--      A later sign-in whose VERIFIED email equals it claims that user
--      (internal/user.linkOrCreate); an unverified one never does.
--
-- users.actor_key is the string a user without a primary email is known by
-- on the wire (actor_id and provenance keep their names and are rendered
-- from the user row: primary_email, else actor_key, else "user:<id>").
--
-- A pre-#326 session whose actor WAS its OIDC subject gets that
-- (issuer, subject) identity linked to the user its string resolved to, so a
-- raw-subject owner is still recognised when they next sign in (design,
-- Risks: "Legacy strings that were raw OIDC subjects").
--
-- The whole file runs in one transaction (internal/db.Migrate). Before any
-- string is dropped, every owner's Bin is compared before and after: the
-- artifacts owned by the string must be exactly the artifacts owned by its
-- user. Any difference raises, the transaction rolls back, the old schema is
-- untouched, and cairnd refuses to start with the first mismatched owner in
-- the error. Take a database backup before upgrading: after a successful run
-- the strings are gone, and rollback is a restore.
--
-- No extension is created here: test schemas are dropped CASCADE and would
-- take a schema-local extension with them. gen_random_uuid() is core in
-- Postgres 13+.

ALTER TABLE users ADD COLUMN actor_key text;
CREATE UNIQUE INDEX users_actor_key_key ON users (actor_key);
-- The legacy claim looks a verified email up case-insensitively among users
-- that have no primary email yet.
CREATE INDEX users_actor_key_claim_idx ON users (lower(actor_key)) WHERE primary_email IS NULL;

CREATE TEMP TABLE legacy_owner_map (
    owner   text PRIMARY KEY,
    user_id uuid,
    rule    text
) ON COMMIT DROP;

INSERT INTO legacy_owner_map (owner)
SELECT owner_id FROM artifacts
UNION SELECT actor_id FROM artifacts
UNION SELECT actor_id FROM comments
UNION SELECT actor_id FROM reactions
UNION SELECT owner_id FROM personal_access_tokens
UNION SELECT owner_id FROM mcp_sessions
UNION SELECT actor_id FROM oauth_grants
UNION SELECT actor_id FROM oauth_auth_codes
UNION SELECT actor_id FROM sessions WHERE user_id IS NULL;

-- 1. "user:<uuid>" of an existing user. The CASE keeps the cast away from
--    strings that are not uuids.
UPDATE legacy_owner_map m
   SET user_id = u.id, rule = 'user-id'
  FROM users u
 WHERE m.owner ~ '^user:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
   AND u.id = CASE
         WHEN m.owner ~ '^user:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
         THEN substr(m.owner, 6)::uuid
       END;

-- 2. A verified primary email, compared exactly: a mixed-case string was a
--    different owner before and stays one.
UPDATE legacy_owner_map m
   SET user_id = u.id, rule = 'email'
  FROM users u
 WHERE m.user_id IS NULL
   AND u.primary_email = m.owner
   AND u.email_verified;

-- 3. The user a #326 session acting as this string belonged to.
UPDATE legacy_owner_map m
   SET user_id = s.user_id, rule = 'session'
  FROM (SELECT DISTINCT ON (actor_id) actor_id, user_id
          FROM sessions
         WHERE user_id IS NOT NULL
         ORDER BY actor_id, created_at DESC) s
 WHERE m.user_id IS NULL
   AND s.actor_id = m.owner;

-- Those users keep rendering as the string they acted as.
UPDATE users u
   SET actor_key = m.owner
  FROM legacy_owner_map m
 WHERE m.rule = 'session'
   AND m.user_id = u.id
   AND u.primary_email IS NULL
   AND u.actor_key IS NULL;

-- 4. Everything else becomes a legacy user. Its display handle is the local
--    part of an email-shaped string, so a public reader never sees the email.
UPDATE legacy_owner_map
   SET user_id = gen_random_uuid(), rule = 'legacy'
 WHERE user_id IS NULL;

INSERT INTO users (id, actor_key, email_verified, display_handle)
SELECT user_id, owner, false, COALESCE(NULLIF(split_part(owner, '@', 1), ''), 'user')
  FROM legacy_owner_map
 WHERE rule = 'legacy';

-- Raw-subject owners keep their identity: a pre-#326 session that acted as
-- exactly its OIDC subject links that (issuer, subject) to the owner's user.
-- Email-shaped strings are never linked this way; only a verified email
-- claims them.
INSERT INTO user_identities (user_id, issuer, subject, email, email_verified, created_at, last_seen_at)
SELECT DISTINCT ON (s.issuer, s.subject)
       m.user_id, s.issuer, s.subject, NULL, false, s.created_at, s.created_at
  FROM sessions s
  JOIN legacy_owner_map m ON m.owner = s.actor_id
 WHERE s.user_id IS NULL
   AND s.issuer <> ''
   AND s.actor_id = s.subject
   AND position('@' IN s.subject) = 0
 ORDER BY s.issuer, s.subject, s.created_at DESC
ON CONFLICT (issuer, subject) DO NOTHING;

-- Artifacts: exactly one owner, a user or a team. Teams do not exist yet
-- (#328); owner_team_id lands now so the constraint has its final shape, and
-- gains its foreign key when the teams table does. Nothing writes it yet.
ALTER TABLE artifacts
    ADD COLUMN owner_user_id      uuid REFERENCES users(id) ON DELETE CASCADE,
    ADD COLUMN owner_team_id      uuid,
    -- Records who made it. Confers no permission.
    ADD COLUMN created_by_user_id uuid REFERENCES users(id) ON DELETE SET NULL;

UPDATE artifacts a
   SET owner_user_id = o.user_id, created_by_user_id = c.user_id
  FROM legacy_owner_map o, legacy_owner_map c
 WHERE o.owner = a.owner_id
   AND c.owner = a.actor_id;

ALTER TABLE artifacts
    ADD CONSTRAINT artifacts_one_owner CHECK (num_nonnulls(owner_user_id, owner_team_id) = 1) NOT VALID;
ALTER TABLE artifacts VALIDATE CONSTRAINT artifacts_one_owner;

-- The Bin reads one owner's rows newest first; the same index backs the
-- owner foreign key's cascade.
CREATE INDEX artifacts_owner_user_created_idx ON artifacts (owner_user_id, created_at, id);
CREATE INDEX artifacts_owner_team_created_idx ON artifacts (owner_team_id, created_at, id) WHERE owner_team_id IS NOT NULL;
-- Dropped with owner_id below; only here to keep the comparison cheap.
CREATE INDEX artifacts_legacy_owner_idx ON artifacts (owner_id);

-- The per-user Bin comparison (SPEC-0023 "A failed comparison leaves the old
-- schema"). Every artifact is compared, live or expired, so the check is at
-- least as strict as any Bin page.
DO $$
DECLARE
    mismatched text;
BEGIN
    SELECT m.owner INTO mismatched
      FROM legacy_owner_map m
     WHERE EXISTS (
               SELECT id FROM artifacts WHERE owner_id = m.owner
               EXCEPT
               SELECT id FROM artifacts WHERE owner_user_id = m.user_id)
        OR EXISTS (
               SELECT id FROM artifacts WHERE owner_user_id = m.user_id
               EXCEPT
               SELECT id FROM artifacts WHERE owner_id = m.owner)
     ORDER BY m.owner
     LIMIT 1;
    IF mismatched IS NOT NULL THEN
        RAISE EXCEPTION 'ownership backfill would change the Bin of owner %', mismatched
            USING HINT = 'nothing was changed; the previous schema is intact';
    END IF;
END
$$;

ALTER TABLE artifacts DROP COLUMN owner_id, DROP COLUMN actor_id;

-- Comments and reactions: the author is a user. NO ACTION on delete: a user
-- with annotations on someone else's artifact is not silently erased from
-- that thread or its counts.
ALTER TABLE comments ADD COLUMN user_id uuid REFERENCES users(id);
UPDATE comments c SET user_id = m.user_id FROM legacy_owner_map m WHERE m.owner = c.actor_id;
ALTER TABLE comments ALTER COLUMN user_id SET NOT NULL;
ALTER TABLE comments DROP COLUMN actor_id;
CREATE INDEX comments_user_id_idx ON comments (user_id);

ALTER TABLE reactions ADD COLUMN user_id uuid REFERENCES users(id);
UPDATE reactions r SET user_id = m.user_id FROM legacy_owner_map m WHERE m.owner = r.actor_id;
ALTER TABLE reactions ALTER COLUMN user_id SET NOT NULL;
-- Dropping actor_id drops the idempotency key that named it (SPEC-0016 EV-6);
-- it is rebuilt on user_id.
ALTER TABLE reactions DROP COLUMN actor_id;
ALTER TABLE reactions
    ADD CONSTRAINT reactions_idem_key UNIQUE (artifact_id, anchor_type, anchor_key, emoji, user_id);
CREATE INDEX reactions_user_id_idx ON reactions (user_id);

-- Credentials belong to a user and go with them.
ALTER TABLE personal_access_tokens ADD COLUMN user_id uuid REFERENCES users(id) ON DELETE CASCADE;
UPDATE personal_access_tokens t SET user_id = m.user_id FROM legacy_owner_map m WHERE m.owner = t.owner_id;
ALTER TABLE personal_access_tokens ALTER COLUMN user_id SET NOT NULL;
ALTER TABLE personal_access_tokens DROP COLUMN owner_id;
CREATE INDEX personal_access_tokens_user_id_idx ON personal_access_tokens (user_id);

ALTER TABLE mcp_sessions ADD COLUMN user_id uuid REFERENCES users(id) ON DELETE CASCADE;
UPDATE mcp_sessions s SET user_id = m.user_id FROM legacy_owner_map m WHERE m.owner = s.owner_id;
ALTER TABLE mcp_sessions ALTER COLUMN user_id SET NOT NULL;
ALTER TABLE mcp_sessions DROP COLUMN owner_id;
CREATE INDEX mcp_sessions_user_id_idx ON mcp_sessions (user_id);

ALTER TABLE oauth_grants ADD COLUMN user_id uuid REFERENCES users(id) ON DELETE CASCADE;
UPDATE oauth_grants g SET user_id = m.user_id FROM legacy_owner_map m WHERE m.owner = g.actor_id;
ALTER TABLE oauth_grants ALTER COLUMN user_id SET NOT NULL;
ALTER TABLE oauth_grants DROP COLUMN actor_id;
CREATE INDEX oauth_grants_user_id_idx ON oauth_grants (user_id);

ALTER TABLE oauth_auth_codes ADD COLUMN user_id uuid REFERENCES users(id) ON DELETE CASCADE;
UPDATE oauth_auth_codes c SET user_id = m.user_id FROM legacy_owner_map m WHERE m.owner = c.actor_id;
ALTER TABLE oauth_auth_codes ALTER COLUMN user_id SET NOT NULL;
ALTER TABLE oauth_auth_codes DROP COLUMN actor_id;

-- Sessions gained a nullable user_id in 0017; sessions minted before it are
-- resolved the same way, and the column becomes mandatory.
UPDATE sessions s SET user_id = m.user_id FROM legacy_owner_map m WHERE s.user_id IS NULL AND m.owner = s.actor_id;
ALTER TABLE sessions ALTER COLUMN user_id SET NOT NULL;
ALTER TABLE sessions DROP COLUMN actor_id;
