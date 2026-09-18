-- Sessions gain auth provenance (SPEC-0012): which provider established the
-- session and the subject that provider asserted. Both default '' so rows
-- written before this migration (dev-password and first-party OIDC logins)
-- read back as unprovenanced rather than failing the scan.
ALTER TABLE sessions ADD COLUMN issuer TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN subject TEXT NOT NULL DEFAULT '';
