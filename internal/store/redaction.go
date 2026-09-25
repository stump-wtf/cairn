package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/redact"
)

// Redaction Outcome Writes
//
// Every row that holds scanned content records a redact.Summary in three
// columns (migration 0017). There are two ways to write one, and both take the
// caller's transaction, because SPEC-0017 requires the outcome to commit with
// the content it describes:
//
//   - An insert path passes the Summary with the row. artifact.Artifact carries
//     it as Redaction; the bundle, comment and run inserts take one too. The zero
//     Summary writes "unscanned", so a path the scanner is not wired into yet
//     never claims to be clean.
//   - RecordRedaction and AccumulateRedaction write it onto a row that already
//     exists in the same transaction: RecordRedaction replaces the outcome,
//     AccumulateRedaction folds a new scan into it (a run's appended spans).
//
// Governing: ADR-0023, SPEC-0017 RD-9, "Database Operation Standards"
//
// @joestump 09/25/2026 - Added for cairn#290. The write paths start setting a
// real Summary in #291, #292 and #293.

// RedactionTarget names a table that carries the outcome columns. It is a
// closed set: each value selects one fixed, parameterized statement, and no
// caller-supplied string ever reaches the SQL text.
type RedactionTarget string

const (
	RedactionArtifact RedactionTarget = "artifact"
	RedactionComment  RedactionTarget = "comment"
	RedactionRun      RedactionTarget = "run"
)

// redactionSQL holds each target's read-for-update and write statements. The
// id is each table's own primary key: artifacts.id, comments.id, runs.id.
// Bundle members are not a target: their key is (bundle_id, ordinal), and
// CreateBundle, which scans them, writes each member's outcome with its insert.
var redactionSQL = map[RedactionTarget]struct{ lock, write string }{
	RedactionArtifact: {
		lock:  `SELECT redaction_status, redaction_count, redaction_rules FROM artifacts WHERE id = $1 FOR UPDATE`,
		write: `UPDATE artifacts SET redaction_status = $2, redaction_count = $3, redaction_rules = $4 WHERE id = $1`,
	},
	RedactionComment: {
		lock:  `SELECT redaction_status, redaction_count, redaction_rules FROM comments WHERE id = $1 FOR UPDATE`,
		write: `UPDATE comments SET redaction_status = $2, redaction_count = $3, redaction_rules = $4 WHERE id = $1`,
	},
	RedactionRun: {
		lock:  `SELECT redaction_status, redaction_count, redaction_rules FROM runs WHERE id = $1 FOR UPDATE`,
		write: `UPDATE runs SET redaction_status = $2, redaction_count = $3, redaction_rules = $4 WHERE id = $1`,
	},
}

// redactionColumns returns s as the three column values (status, count,
// rules) a statement binds, normalized so an empty status is "unscanned" and
// the rules are never a JSON null. Insert paths outside this package (comments,
// runs) bind redact.Summary.Normalized's fields the same way.
func redactionColumns(s redact.Summary) (string, int, map[string]int, error) {
	n, err := s.Normalized()
	if err != nil {
		return "", 0, nil, fmt.Errorf("redaction outcome: %w", err)
	}
	return string(n.Status), n.Count, n.Rules, nil
}

// RecordRedaction replaces the outcome on row id of target, inside tx. Use it
// when the content was written earlier in the same transaction and the scan
// result is known only afterwards.
func RecordRedaction(ctx context.Context, tx pgx.Tx, target RedactionTarget, id int64, s redact.Summary) error {
	q, ok := redactionSQL[target]
	if !ok {
		return fmt.Errorf("record redaction: unknown target %q", target)
	}
	status, count, rules, err := redactionColumns(s)
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, q.write, id, status, count, rules)
	if err != nil {
		return fmt.Errorf("record redaction on %s %d: %w", target, id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("record redaction on %s %d: %w", target, id, errs.ErrNotFound)
	}
	return nil
}

// AccumulateRedaction folds s into the outcome already on row id of target,
// inside tx, under a row lock so concurrent appends cannot lose a count. See
// redact.Summary.Merge for how the status combines.
func AccumulateRedaction(ctx context.Context, tx pgx.Tx, target RedactionTarget, id int64, s redact.Summary) error {
	q, ok := redactionSQL[target]
	if !ok {
		return fmt.Errorf("accumulate redaction: unknown target %q", target)
	}
	var prev redact.Summary
	if err := tx.QueryRow(ctx, q.lock, id).Scan(&prev.Status, &prev.Count, &prev.Rules); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("accumulate redaction on %s %d: %w", target, id, errs.ErrNotFound)
		}
		return fmt.Errorf("accumulate redaction on %s %d: %w", target, id, err)
	}
	status, count, rules, err := redactionColumns(prev.Merge(s))
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, q.write, id, status, count, rules); err != nil {
		return fmt.Errorf("accumulate redaction on %s %d: %w", target, id, err)
	}
	return nil
}
