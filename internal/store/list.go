package store

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/errs"
)

// Bin pagination bounds.
const (
	DefaultBinLimit = 50
	MaxBinLimit     = 100
)

// binColumns is the shared projection for listing artifacts, matching the read
// path so the Bin and a single read agree on shape.
const binColumns = `
	id, public_id, share_type, title, body_sha256, size_bytes,
	media_type, previewable, actor_id, on_behalf_of, model, channel,
	captured_at, owner_id, visibility, reaction_count, comment_count, pin_count,
	tags, expires_at, created_at`

// BinPage is one page of the Bin plus the cursor to fetch the next page (empty
// when the last page has been reached).
type BinPage struct {
	Artifacts  []*artifact.Artifact
	NextCursor string
}

// ListBin returns a workspace's live artifacts, keyset-paginated over
// (created_at, id) descending so the page neither skips nor duplicates rows
// while artifacts are inserted and expired mid-scroll. Ordering is by
// created_at, never by public id (ids are random, ADR-0005). This one query
// backs both the web Bin (SPEC-0001) and the CLI TUI (ADR-0003).
//
// Any tags narrow the page to artifacts carrying every one of them (a
// containment match), validated like tags at create; none means no filter.
//
// Governing: ADR-0012 (keyset pagination), SPEC-0002 REQ "Artifact Lifecycle —
// List (the Bin)", ADR-0018, SPEC-0002 REQ "Artifact Tags".
func (s *Store) ListBin(ctx context.Context, ownerID, cursor string, limit int, tags ...string) (BinPage, error) {
	if ownerID == "" {
		return BinPage{}, errs.Validationf("bin: owner is required")
	}
	if limit <= 0 {
		limit = DefaultBinLimit
	}
	if limit > MaxBinLimit {
		limit = MaxBinLimit
	}

	// Fetch one extra row to decide whether a further page exists.
	args := []any{ownerID}
	where := "owner_id = $1 AND expires_at > now()"
	if cursor != "" {
		curCreated, curID, err := decodeCursor(cursor)
		if err != nil {
			return BinPage{}, err
		}
		// Row-value comparison gives a correct, index-friendly keyset step.
		where += " AND (created_at, id) < ($2, $3)"
		args = append(args, curCreated, curID)
	}
	if len(tags) > 0 {
		filter, err := artifact.NormalizeTags(tags)
		if err != nil {
			return BinPage{}, err
		}
		args = append(args, filter)
		where += fmt.Sprintf(" AND tags @> $%d", len(args))
	}
	args = append(args, limit+1)

	query := "SELECT" + binColumns + " FROM artifacts WHERE " + where +
		fmt.Sprintf(" ORDER BY created_at DESC, id DESC LIMIT $%d", len(args))

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return BinPage{}, fmt.Errorf("bin: query: %w", err)
	}
	defer rows.Close()

	var out []*artifact.Artifact
	for rows.Next() {
		a, err := scanArtifact(rows)
		if err != nil {
			return BinPage{}, fmt.Errorf("bin: scan: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return BinPage{}, fmt.Errorf("bin: iterate: %w", err)
	}

	page := BinPage{}
	if len(out) > limit {
		last := out[limit-1]
		page.NextCursor = encodeCursor(last.CreatedAt, last.ID)
		out = out[:limit]
	}
	page.Artifacts = out
	return page, nil
}

// rowScanner is satisfied by both pgx.Row and pgx.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanArtifact scans the binColumns projection into an Artifact.
func scanArtifact(row rowScanner) (*artifact.Artifact, error) {
	var (
		a       artifact.Artifact
		bodySHA *string
	)
	if err := row.Scan(
		&a.ID, &a.PublicID, &a.ShareType, &a.Title, &bodySHA, &a.Size,
		&a.MediaType, &a.Previewable, &a.Provenance.ActorID,
		&a.Provenance.OnBehalfOf, &a.Provenance.Model, &a.Provenance.Channel, &a.Provenance.CapturedAt,
		&a.Access.OwnerID, &a.Access.Visibility,
		&a.ReactionCount, &a.CommentCount, &a.PinCount, &a.Tags,
		&a.ExpiresAt, &a.CreatedAt,
	); err != nil {
		return nil, err
	}
	if bodySHA != nil {
		a.BodySHA256 = *bodySHA
	}
	return &a, nil
}

// encodeCursor packs the keyset position (created_at, id) into an opaque token.
func encodeCursor(createdAt time.Time, id int64) string {
	raw := fmt.Sprintf("%s|%d", createdAt.UTC().Format(time.RFC3339Nano), id)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeCursor parses an opaque Bin cursor, rejecting malformed input with a
// validation-coded error.
func decodeCursor(cursor string) (time.Time, int64, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, 0, errs.Validationf("bin: malformed cursor")
	}
	createdStr, idStr, ok := strings.Cut(string(raw), "|")
	if !ok {
		return time.Time{}, 0, errs.Validationf("bin: malformed cursor")
	}
	createdAt, err := time.Parse(time.RFC3339Nano, createdStr)
	if err != nil {
		return time.Time{}, 0, errs.Validationf("bin: malformed cursor timestamp")
	}
	var id int64
	if _, err := fmt.Sscan(idStr, &id); err != nil {
		return time.Time{}, 0, errs.Validationf("bin: malformed cursor id")
	}
	return createdAt, id, nil
}
