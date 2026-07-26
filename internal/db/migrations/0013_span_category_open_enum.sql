-- Open the span category set (issue #5, ADR-0009, SPEC-0004 REQ "Span Model and
-- Ordered Tree" / "Non-recommended category accepted").
--
-- 0004 shipped `spans_category_chk CHECK (category IN ('reason','exec','read',
-- 'net','write'))`, matching a design that has since been superseded: the record
-- now specifies an OPEN set, where the five (now thirteen) recommended values
-- only drive the waterfall's color legend and anything else is accepted and
-- rendered neutrally. The constraint was rejecting categories the spec says MUST
-- be accepted — a span with category 'search' failed at the database even if
-- service validation let it through.
--
-- Replace it rather than simply dropping it: the set is open, but not unbounded.
-- Empty/whitespace-only is still rejected (SPEC-0004 "Empty or missing category
-- rejected"), and a character-length ceiling keeps unbounded agent text out of a
-- column rendered in every legend (SPEC-0004 "Category length bounded"). The
-- bound mirrors trajectory.MaxCategoryLen; the two are asserted equal by
-- TestSpanCategoryConstraintMatchesDomain.
--
-- No data migration is needed: every persisted category already satisfies the
-- new, strictly weaker predicate.

ALTER TABLE spans DROP CONSTRAINT spans_category_chk;

-- `~ '\S'` (at least one non-whitespace character) rather than btrim(): the
-- one-argument btrim() strips ONLY spaces, so a tab-or-newline-only category
-- would slip past it while the service rejects it. length() counts characters,
-- and trajectory.Category.valid() counts runes, so the ceilings mean the same
-- thing on non-ASCII input.
ALTER TABLE spans ADD CONSTRAINT spans_category_chk
    CHECK (category ~ '\S' AND length(category) <= 64);

COMMENT ON COLUMN spans.category IS
    'Open set. Recommended (color-mapped): reason|exec|read|net|write|search|plan|tool|analyze|test|fix|fail|meta. Any other non-empty string is accepted and renders with the neutral default color.';

COMMENT ON COLUMN spans.tool IS
    'Tool name; NULL for reason spans, permitted on every other category.';
