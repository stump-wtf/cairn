package store

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/user"
)

// Bin pagination bounds.
const (
	DefaultBinLimit = 50
	MaxBinLimit     = 100
)

// binColumns is the shared projection for listing artifacts, matching the read
// path so the Bin and a single read agree on shape. It reads artifacts as a
// joined to its creator as cu (artifactFrom): the provenance actor is
// rendered from the creator's user row, never stored (SPEC-0023 REQ
// "Migration to Explicit Ownership").
var binColumns = `
	a.id, a.public_id, a.share_type, a.title, a.body_sha256, a.size_bytes,
	a.media_type, a.previewable, COALESCE(a.created_by_user_id::text, ''),
	COALESCE(` + user.ActorSQL("cu") + `, ''), a.on_behalf_of, a.model, a.channel,
	a.captured_at, COALESCE(a.owner_user_id::text, ''), COALESCE(a.owner_team_id::text, ''),
	a.visibility, a.reaction_count, a.comment_count, a.pin_count,
	a.tags, a.expires_at, a.created_at`

// artifactFrom is the FROM clause binColumns reads.
const artifactFrom = ` FROM artifacts a LEFT JOIN users cu ON cu.id = a.created_by_user_id`

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
//
// ownerUserID is the caller's user id: the Bin is exactly the artifacts that
// user owns (SPEC-0023 REQ "Owner Model").
func (s *Store) ListBin(ctx context.Context, ownerUserID, cursor string, limit int, tags ...string) (BinPage, error) {
	if ownerUserID == "" {
		return BinPage{}, errs.Validationf("bin: owner is required")
	}
	if limit <= 0 {
		limit = DefaultBinLimit
	}
	if limit > MaxBinLimit {
		limit = MaxBinLimit
	}

	// Fetch one extra row to decide whether a further page exists.
	args := []any{user.IDParam(ownerUserID)}
	where := "a.owner_user_id = $1 AND a.expires_at > now()"
	if cursor != "" {
		curCreated, curID, err := decodeCursor(cursor)
		if err != nil {
			return BinPage{}, err
		}
		// Row-value comparison gives a correct, index-friendly keyset step.
		where += " AND (a.created_at, a.id) < ($2, $3)"
		args = append(args, curCreated, curID)
	}
	if len(tags) > 0 {
		filter, err := artifact.NormalizeTags(tags)
		if err != nil {
			return BinPage{}, err
		}
		args = append(args, filter)
		where += fmt.Sprintf(" AND a.tags @> $%d", len(args))
	}
	args = append(args, limit+1)

	query := "SELECT" + binColumns + artifactFrom + " WHERE " + where +
		fmt.Sprintf(" ORDER BY a.created_at DESC, a.id DESC LIMIT $%d", len(args))

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
		&a.MediaType, &a.Previewable, &a.Provenance.CreatedByUserID, &a.Provenance.ActorID,
		&a.Provenance.OnBehalfOf, &a.Provenance.Model, &a.Provenance.Channel, &a.Provenance.CapturedAt,
		&a.Access.OwnerUserID, &a.Access.OwnerTeamID, &a.Access.Visibility,
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
