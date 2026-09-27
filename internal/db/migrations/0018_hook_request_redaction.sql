-- Captured Request Redaction Outcome
--
-- A captured webhook request is scanned for credentials before the ring-buffer
-- insert and always masked, never refused: the anonymous sender cannot act on
-- a refusal (SPEC-0017 RD-4). Each hook_requests row records what the scanner
-- did to it, with the same three columns 0017 gave artifacts, bundle members,
-- comments and runs: a status from the closed set, a count, and a JSON object
-- of rule ID to count. None of them can hold a secret, a hash of one or the
-- matched line.
--
-- redaction_withheld names the captured fields Cairn replaced wholesale
-- because it could not store them safely: a field whose scan failed, one over
-- the scan cap under the default oversize policy, or one holding a credential
-- the masker could not locate. The capture is still accepted, and the field is
-- stored as a notice in place of the raw bytes. The array holds field names
-- from a closed set, never content.
--
-- Rows captured before this migration read 'unscanned', which is honest: they
-- were stored before the scanner ran.
--
-- Governing: ADR-0023, SPEC-0017 RD-4, RD-9; ADR-0010, SPEC-0005
--
-- @joestump 09/26/2026 - Added for cairn#293.

ALTER TABLE hook_requests
    ADD COLUMN redaction_status   TEXT   NOT NULL DEFAULT 'unscanned',
    ADD COLUMN redaction_count    INT    NOT NULL DEFAULT 0,
    ADD COLUMN redaction_rules    JSONB  NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN redaction_withheld TEXT[] NOT NULL DEFAULT '{}',
    ADD CONSTRAINT hook_requests_redaction_status_chk CHECK (redaction_status IN
        ('unscanned', 'clean', 'masked', 'not_scanned_binary', 'not_scanned_oversize')),
    ADD CONSTRAINT hook_requests_redaction_count_chk CHECK (redaction_count >= 0),
    ADD CONSTRAINT hook_requests_redaction_rules_chk CHECK (jsonb_typeof(redaction_rules) = 'object'),
    ADD CONSTRAINT hook_requests_redaction_withheld_chk CHECK (redaction_withheld <@ ARRAY['query', 'headers', 'body']::TEXT[]);
