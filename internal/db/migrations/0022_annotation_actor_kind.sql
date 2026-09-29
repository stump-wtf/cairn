-- cairn:no-transaction
-- Per-kind ownership for reactions and comments (ADR-0022, SPEC-0016 EV-6,
-- REQ "Database Operation Standards"; #159).
--
-- An agent's bearer token and its human's browser session resolve to the same
-- actor_id, so under the old reaction key
--   UNIQUE (artifact_id, anchor_type, anchor_key, emoji, actor_id)
-- the agent's 👍 and the human's 👍 were ONE row: the agent could occupy the
-- human's row, withdraw the human's reaction, and nothing recorded which of
-- the two it was. This migration stores the server-derived actor_kind
-- ('human' for a browser session, 'agent' for every bearer credential) on both
-- annotation tables, and re-keys reaction idempotency on
--   (artifact_id, anchor_type, anchor_key, emoji, actor_id, actor_kind)
-- so idempotency is per (actor_id, actor_kind), as ADR-0022 decided.
-- on_behalf_of is deliberately NOT in the key: it is self-reported and empty
-- for PAT callers, so it cannot tell a human from their own agent.
--
-- reactions also gains on_behalf_of, populated exactly as comments' is
-- (provenance parity, SPEC-0009).
--
-- Rows written before this migration read back actor_kind = ''. They stay
-- unique under the new key (it only adds a column), are never treated as
-- human, and only the same actor_id may remove them (EV-6 "Legacy row is
-- never an approval").
--
-- The file runs outside a transaction (internal/db.Migrate, noTxDirective) so
-- the new unique index builds CONCURRENTLY and the live table keeps taking
-- writes. A crash part-way leaves it unrecorded and the next start reruns it
-- from the top, so every statement below is idempotent. The old constraint's
-- name is Postgres-generated; it is found in the catalog by its column list,
-- not assumed, and dropped only after the new index is valid.
-- cairn:statement-break

-- Constant defaults are metadata-only (no table rewrite).
ALTER TABLE reactions
    ADD COLUMN IF NOT EXISTS actor_kind   TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS on_behalf_of TEXT NOT NULL DEFAULT '';
-- cairn:statement-break

ALTER TABLE comments
    ADD COLUMN IF NOT EXISTS actor_kind TEXT NOT NULL DEFAULT '';
-- cairn:statement-break

-- The kind set is closed. Added NOT VALID (no scan under the ACCESS
-- EXCLUSIVE lock), then validated below under a lock that admits writes.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                   WHERE conrelid = 'reactions'::regclass AND conname = 'reactions_actor_kind_chk') THEN
        ALTER TABLE reactions ADD CONSTRAINT reactions_actor_kind_chk
            CHECK (actor_kind IN ('', 'human', 'agent')) NOT VALID;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                   WHERE conrelid = 'comments'::regclass AND conname = 'comments_actor_kind_chk') THEN
        ALTER TABLE comments ADD CONSTRAINT comments_actor_kind_chk
            CHECK (actor_kind IN ('', 'human', 'agent')) NOT VALID;
    END IF;
END $$;
-- cairn:statement-break

ALTER TABLE reactions VALIDATE CONSTRAINT reactions_actor_kind_chk;
-- cairn:statement-break

ALTER TABLE comments VALIDATE CONSTRAINT comments_actor_kind_chk;
-- cairn:statement-break

-- A concurrent build that failed on an earlier attempt leaves an INVALID
-- index behind, which IF NOT EXISTS below would then skip. Drop it first.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_index
               WHERE indexrelid = to_regclass('reactions_idem_kind_uidx') AND NOT indisvalid) THEN
        DROP INDEX reactions_idem_kind_uidx;
    END IF;
END $$;
-- cairn:statement-break

CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS reactions_idem_kind_uidx
    ON reactions (artifact_id, anchor_type, anchor_key, emoji, actor_id, actor_kind);
-- cairn:statement-break

-- Swap: drop the old per-actor key only once the per-kind one is valid, so a
-- reaction table is never left without an idempotency key.
DO $$
DECLARE
    c record;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_index
                   WHERE indexrelid = to_regclass('reactions_idem_kind_uidx')
                     AND indisvalid AND indisunique) THEN
        RAISE EXCEPTION 'reactions_idem_kind_uidx is missing or invalid; the old reaction key stays';
    END IF;
    FOR c IN
        SELECT con.conname
          FROM pg_constraint con
         WHERE con.conrelid = 'reactions'::regclass
           AND con.contype = 'u'
           AND ARRAY(SELECT a.attname::text
                       FROM unnest(con.conkey) WITH ORDINALITY AS k(attnum, ord)
                       JOIN pg_attribute a ON a.attrelid = con.conrelid AND a.attnum = k.attnum
                      ORDER BY k.ord)
               = ARRAY['artifact_id', 'anchor_type', 'anchor_key', 'emoji', 'actor_id']
    LOOP
        EXECUTE format('ALTER TABLE reactions DROP CONSTRAINT %I', c.conname);
    END LOOP;
END $$;
