-- Redaction Outcome Columns
--
-- Every scanned row records what the ingest secret scanner did to it, and
-- never what it found (SPEC-0017 RD-9). Artifacts, bundle members, comments and
-- runs each gain the same three columns: a status from a closed set, a count,
-- and a JSON object of rule ID to count. None of them can hold a secret, a hash
-- of one or the matched line: the status is a CHECKed enum, the count is an
-- integer, and the rules object's keys are gitleaks rule IDs taken from the
-- scanner's own config, never from the scanned text.
--
-- The status defaults to 'unscanned', so every row written before this
-- migration reads honestly as never scanned (SPEC-0017 "Legacy artifact
-- status"). New rows keep that default until their write path is wired to the
-- scanner: the insert paths pass a redact.Summary whose zero value is
-- 'unscanned', and only a path that actually ran the scanner sets 'clean' or
-- better, in the same transaction as the content. NOT NULL DEFAULT on a
-- constant is a metadata-only change on Postgres 11+, so the backfill is free.
--
-- Governing: ADR-0023, SPEC-0017 RD-9, "Database Operation Standards"
--
-- @joestump 09/25/2026 - Added for cairn#290.

ALTER TABLE artifacts
    ADD COLUMN redaction_status TEXT NOT NULL DEFAULT 'unscanned',
    ADD COLUMN redaction_count  INT  NOT NULL DEFAULT 0,
    ADD COLUMN redaction_rules  JSONB NOT NULL DEFAULT '{}'::jsonb,
    ADD CONSTRAINT artifacts_redaction_status_chk CHECK (redaction_status IN
        ('unscanned', 'clean', 'masked', 'not_scanned_binary', 'not_scanned_oversize')),
    ADD CONSTRAINT artifacts_redaction_count_chk CHECK (redaction_count >= 0),
    ADD CONSTRAINT artifacts_redaction_rules_chk CHECK (jsonb_typeof(redaction_rules) = 'object');

ALTER TABLE bundle_members
    ADD COLUMN redaction_status TEXT NOT NULL DEFAULT 'unscanned',
    ADD COLUMN redaction_count  INT  NOT NULL DEFAULT 0,
    ADD COLUMN redaction_rules  JSONB NOT NULL DEFAULT '{}'::jsonb,
    ADD CONSTRAINT bundle_members_redaction_status_chk CHECK (redaction_status IN
        ('unscanned', 'clean', 'masked', 'not_scanned_binary', 'not_scanned_oversize')),
    ADD CONSTRAINT bundle_members_redaction_count_chk CHECK (redaction_count >= 0),
    ADD CONSTRAINT bundle_members_redaction_rules_chk CHECK (jsonb_typeof(redaction_rules) = 'object');

ALTER TABLE comments
    ADD COLUMN redaction_status TEXT NOT NULL DEFAULT 'unscanned',
    ADD COLUMN redaction_count  INT  NOT NULL DEFAULT 0,
    ADD COLUMN redaction_rules  JSONB NOT NULL DEFAULT '{}'::jsonb,
    ADD CONSTRAINT comments_redaction_status_chk CHECK (redaction_status IN
        ('unscanned', 'clean', 'masked', 'not_scanned_binary', 'not_scanned_oversize')),
    ADD CONSTRAINT comments_redaction_count_chk CHECK (redaction_count >= 0),
    ADD CONSTRAINT comments_redaction_rules_chk CHECK (jsonb_typeof(redaction_rules) = 'object');

ALTER TABLE runs
    ADD COLUMN redaction_status TEXT NOT NULL DEFAULT 'unscanned',
    ADD COLUMN redaction_count  INT  NOT NULL DEFAULT 0,
    ADD COLUMN redaction_rules  JSONB NOT NULL DEFAULT '{}'::jsonb,
    ADD CONSTRAINT runs_redaction_status_chk CHECK (redaction_status IN
        ('unscanned', 'clean', 'masked', 'not_scanned_binary', 'not_scanned_oversize')),
    ADD CONSTRAINT runs_redaction_count_chk CHECK (redaction_count >= 0),
    ADD CONSTRAINT runs_redaction_rules_chk CHECK (jsonb_typeof(redaction_rules) = 'object');
