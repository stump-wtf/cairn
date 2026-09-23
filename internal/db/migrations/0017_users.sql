-- Users and identities (ADR-0029, SPEC-0023 REQ "Users and Identities").
--
-- The principal stops being whatever string an authenticator produced. A
-- sign-in resolves its identity by (issuer, subject); a NEW identity links to
-- an existing user only when the provider asserts a verified email equal to
-- that user's primary email, and otherwise creates a user. Emails are stored
-- lower-cased (the CHECKs) rather than as citext: extensions are
-- database-wide, and the per-test schemas the suites create and drop CASCADE
-- would take a schema-local extension with them.
--
-- Forward-safe against live data: two new tables and one nullable column. No
-- existing row is read or rewritten here; owner columns and the backfill from
-- the legacy owner strings are a separate migration (#327).

CREATE TABLE users (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    -- NULL until the user signs in with a verified email. Legacy rows the
    -- ownership backfill creates carry email_verified = false until claimed.
    primary_email  text CHECK (primary_email = lower(primary_email)),
    email_verified boolean NOT NULL DEFAULT false,
    -- What anonymous readers see in place of an email (audit A18).
    display_handle text NOT NULL,
    suspended_at   timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now()
);
-- NULLs are distinct, so any number of email-less users coexist.
CREATE UNIQUE INDEX users_primary_email_key ON users (primary_email);

CREATE TABLE user_identities (
    user_id        uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    issuer         text NOT NULL,
    subject        text NOT NULL,
    email          text CHECK (email = lower(email)),
    email_verified boolean NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    last_seen_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (issuer, subject)
);
CREATE INDEX user_identities_user_id_idx ON user_identities (user_id);

-- Sessions record the user they were minted for. Nullable: sessions minted
-- before this migration keep authenticating as their stored actor until they
-- expire.
ALTER TABLE sessions ADD COLUMN user_id uuid REFERENCES users(id) ON DELETE CASCADE;
