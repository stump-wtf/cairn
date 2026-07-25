-- Retired public ids (issue #94, SPEC-0009 REQ "Id Rotation as
-- Revoke-a-Leaked-Link", ADR-0005 "a retired id is not reused within
-- TTL-plus-grace"). Id rotation (Store.RotateID) records the OLD public_id
-- here in the same transaction that renames the artifact row to a freshly
-- minted id, and the generator (Store.freshPublicID) excludes any id still
-- listed here within the grace window, so a stale/rotated link can never
-- later resolve to a different, newer artifact. A row is only ever consulted
-- while fresh (retired_at within the grace window); older rows are simply
-- never matched again and may be pruned opportunistically — there is no
-- correctness requirement to actively reap this table.
CREATE TABLE retired_ids (
    public_id  TEXT        PRIMARY KEY,
    retired_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
