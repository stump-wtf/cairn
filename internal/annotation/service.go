// The annotation Service is the surface-agnostic core for reactions and
// threaded comments: the REST adapter (SPEC-0006 HTTP endpoints), and later the
// MCP tools and CLI, are thin adapters over exactly these methods, which is how
// cross-surface parity is guaranteed mechanically rather than by convention.
//
// Every write validates its anchor against the share-type registry (anchor.go),
// persists with bound parameters, and updates the artifact's denormalized
// counters — reaction_count, comment_count, pin_count — in the SAME transaction,
// so a committed count can never diverge from its rows.
//
// Governing: ADR-0006 (Unified Annotation Layer), ADR-0012 (backend contract),
// SPEC-0006 REQ "Idempotent Reactions", REQ "Threaded Comments", REQ "Count
// Aggregation", REQ "Cross-Surface Parity", REQ "Database Operation Standards".

package annotation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/joestump/cairn/internal/artifact"
	"github.com/joestump/cairn/internal/errs"
	"github.com/joestump/cairn/internal/sharetype"
)

// Comment body and emoji bounds. The transport additionally enforces its own
// request-size limit (413); these are the service-level invariants that hold on
// every surface.
const (
	// maxCommentBytes bounds a comment body (SPEC-0006 REQ "Request Body Size
	// Limits" is transport-level; this is the core invariant behind it).
	maxCommentBytes = 16 << 10
	// maxEmojiBytes / maxEmojiRunes bound an emoji to a plausible single
	// grapheme cluster (ZWJ sequences run several runes, e.g. 👩‍👩‍👧‍👧).
	maxEmojiBytes = 64
	maxEmojiRunes = 16
)

// Sentinel domain errors for the service's write paths, distinguishable with
// errors.Is and mapped to stable codes by transport adapters via errs.CodeOf
// (SPEC-0006 REQ "Error Handling Standards").
var (
	// ErrEmojiInvalid rejects an emoji that is empty, oversized, or contains
	// whitespace/control characters.
	ErrEmojiInvalid = errs.New(errs.CodeValidation, "emoji must be a single unicode grapheme")
	// ErrBodyInvalid rejects an empty or oversized comment body.
	ErrBodyInvalid = errs.New(errs.CodeValidation, "comment body must be non-empty and within the size bound")
	// ErrParentNotFound rejects a reply whose parent comment does not exist on
	// the annotated artifact (or is soft-deleted).
	ErrParentNotFound = errs.New(errs.CodeValidation, "parent comment not found")
	// ErrThreadTooDeep rejects a reply to a reply: threads nest exactly one
	// level (SPEC-0006 REQ "Threaded Comments").
	ErrThreadTooDeep = errs.New(errs.CodeValidation, "comments nest one level deep")
	// ErrAnchorMismatch rejects a reply whose anchor differs from its root's.
	ErrAnchorMismatch = errs.New(errs.CodeValidation, "reply anchor must match its root comment's anchor")
)

// Service is the annotation core service. It owns the only write paths into
// the reactions and comments tables.
type Service struct {
	pool *pgxpool.Pool
	reg  *sharetype.Registry
	now  func() time.Time
}

// NewService constructs a Service over a Postgres pool. A nil registry falls
// back to the process-wide default.
func NewService(pool *pgxpool.Pool, reg *sharetype.Registry) *Service {
	if reg == nil {
		reg = sharetype.Default()
	}
	return &Service{pool: pool, reg: reg, now: time.Now}
}

// Reaction is one stored reaction row.
type Reaction struct {
	ID        int64
	Anchor    Anchor
	Emoji     string
	ActorID   string
	CreatedAt time.Time
}

// Comment is one stored comment. A soft-deleted comment is returned as a
// tombstone: Deleted is true and Body is empty, but the row (and therefore the
// thread structure under it) remains resolvable.
type Comment struct {
	ID         int64
	Anchor     Anchor
	ParentID   *int64
	ActorID    string
	OnBehalfOf string
	Body       string
	CreatedAt  time.Time
	EditedAt   *time.Time
	Deleted    bool
}

// CommentInput is the surface-agnostic input to AddComment.
type CommentInput struct {
	// AnchorType/AnchorRef locate the comment. For a reply they may be left
	// zero to inherit the parent's anchor; when provided they must match it.
	AnchorType sharetype.Anchor
	AnchorRef  json.RawMessage
	// ParentID threads a reply under a root comment (nil for a root).
	ParentID *int64
	// ActorID is the authenticated author; OnBehalfOf names an agent acting
	// for the human (provenance parity with artifacts, ADR-0007).
	ActorID    string
	OnBehalfOf string
	Body       string
}

// Tally is one per-anchor emoji tally, computed at view time over a single
// artifact's reactions (SPEC-0006 "Per-anchor emoji tally").
type Tally struct {
	AnchorType sharetype.Anchor
	AnchorKey  string
	Emoji      string
	Count      int
	// Reacted reports whether the requesting actor already reacted with this
	// emoji on this anchor ("did I react").
	Reacted bool
}

// React records an idempotent reaction: reacting with the same emoji to the
// same anchor twice by the same actor is a no-op collapsed by the unique
// constraint, never a duplicate row or a double-counted rollup. It returns the
// reaction row and whether it was newly created. The insert and the artifact's
// reaction_count / pin_count bumps commit in one transaction.
//
// Governing: ADR-0006 (idempotency by unique constraint, not
// read-modify-write), SPEC-0006 REQ "Idempotent Reactions", REQ "Count
// Aggregation".
func (s *Service) React(ctx context.Context, publicID string, anchorType sharetype.Anchor, ref json.RawMessage, emoji, actorID string) (Reaction, bool, error) {
	if err := validateEmoji(emoji); err != nil {
		return Reaction{}, false, err
	}
	if actorID == "" {
		return Reaction{}, false, errs.Validationf("annotation: react: actor is required")
	}

	var (
		out     Reaction
		created bool
	)
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		art, err := resolveArtifact(ctx, tx, publicID)
		if err != nil {
			return err
		}
		anchor, err := Validate(s.reg, art.id, art.shareType, sharetype.KindReaction, anchorType, ref)
		if err != nil {
			return err
		}

		// ON CONFLICT DO NOTHING makes two concurrent identical reacts collapse
		// to one row with no application lock; only the transaction that
		// actually inserted bumps the counters.
		var id int64
		var createdAt time.Time
		err = tx.QueryRow(ctx, `
			INSERT INTO reactions (artifact_id, anchor_type, anchor_ref, anchor_key, emoji, actor_id)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (artifact_id, anchor_type, anchor_key, emoji, actor_id) DO NOTHING
			RETURNING id, created_at`,
			anchor.ArtifactID, string(anchor.Type), anchor.Ref, anchor.Key, emoji, actorID,
		).Scan(&id, &createdAt)
		switch {
		case err == nil:
			created = true
			if err := bumpCounts(ctx, tx, anchor.ArtifactID, sharetype.KindReaction, anchor.Type, +1); err != nil {
				return err
			}
		case errors.Is(err, pgx.ErrNoRows):
			// Duplicate react: surface the existing row as the no-op result.
			if err := tx.QueryRow(ctx, `
				SELECT id, created_at FROM reactions
				WHERE artifact_id = $1 AND anchor_type = $2 AND anchor_key = $3 AND emoji = $4 AND actor_id = $5`,
				anchor.ArtifactID, string(anchor.Type), anchor.Key, emoji, actorID,
			).Scan(&id, &createdAt); err != nil {
				return fmt.Errorf("annotation: load existing reaction: %w", err)
			}
		default:
			return fmt.Errorf("annotation: insert reaction: %w", err)
		}
		out = Reaction{ID: id, Anchor: anchor, Emoji: emoji, ActorID: actorID, CreatedAt: createdAt}
		return nil
	})
	if err != nil {
		return Reaction{}, false, err
	}
	return out, created, nil
}

// Unreact removes the actor's reaction identified by (anchor, emoji) — the
// toggle-off half of React. It reports whether a row was removed (removing a
// reaction that does not exist is a no-op, matching the toggle semantics). The
// delete and the counter decrements commit in one transaction.
func (s *Service) Unreact(ctx context.Context, publicID string, anchorType sharetype.Anchor, ref json.RawMessage, emoji, actorID string) (bool, error) {
	if err := validateEmoji(emoji); err != nil {
		return false, err
	}
	key, err := CanonicalKey(ref)
	if err != nil {
		return false, fmt.Errorf("annotation: unreact: %w", ErrLocatorInvalid)
	}

	removed := false
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		art, err := resolveArtifact(ctx, tx, publicID)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			DELETE FROM reactions
			WHERE artifact_id = $1 AND anchor_type = $2 AND anchor_key = $3 AND emoji = $4 AND actor_id = $5`,
			art.id, string(anchorType), key, emoji, actorID)
		if err != nil {
			return fmt.Errorf("annotation: delete reaction: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		removed = true
		return bumpCounts(ctx, tx, art.id, sharetype.KindReaction, anchorType, -1)
	})
	return removed, err
}

// UnreactByID deletes one reaction row by id — the REST
// `DELETE /v1/artifacts/{id}/reactions/{rid}` shape. Only the reaction's own
// actor may remove it (SPEC-0006 REQ "Authentication & Authorization").
func (s *Service) UnreactByID(ctx context.Context, publicID string, reactionID int64, actorID string) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		art, err := resolveArtifact(ctx, tx, publicID)
		if err != nil {
			return err
		}
		var anchorType string
		err = tx.QueryRow(ctx, `
			DELETE FROM reactions WHERE id = $1 AND artifact_id = $2 AND actor_id = $3
			RETURNING anchor_type`,
			reactionID, art.id, actorID).Scan(&anchorType)
		if errors.Is(err, pgx.ErrNoRows) {
			// Distinguish "someone else's reaction" from "no such reaction"
			// without leaking other artifacts' rows: both checks stay scoped
			// to this artifact.
			var exists bool
			if err := tx.QueryRow(ctx,
				`SELECT EXISTS(SELECT 1 FROM reactions WHERE id = $1 AND artifact_id = $2)`,
				reactionID, art.id).Scan(&exists); err != nil {
				return fmt.Errorf("annotation: check reaction %d: %w", reactionID, err)
			}
			if exists {
				return fmt.Errorf("annotation: reaction %d belongs to another actor: %w", reactionID, errs.ErrForbidden)
			}
			return fmt.Errorf("annotation: reaction %d: %w", reactionID, errs.ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("annotation: delete reaction %d: %w", reactionID, err)
		}
		return bumpCounts(ctx, tx, art.id, sharetype.KindReaction, sharetype.Anchor(anchorType), -1)
	})
}

// ReactionTallies returns the per-anchor emoji tallies for one artifact plus
// the requesting actor's "did I react" flag, computed with a GROUP BY scoped
// to that artifact and backed by the (artifact_id, anchor_type, anchor_key)
// index (SPEC-0006 REQ "Count Aggregation", second tier). actorID may be empty
// for an anonymous link-capability read (every Reacted is then false).
func (s *Service) ReactionTallies(ctx context.Context, publicID, actorID string) ([]Tally, error) {
	art, err := resolveArtifact(ctx, s.pool, publicID)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT anchor_type, anchor_key, emoji, count(*), bool_or(actor_id = $2)
		FROM reactions
		WHERE artifact_id = $1
		GROUP BY anchor_type, anchor_key, emoji
		ORDER BY anchor_type, anchor_key, count(*) DESC, emoji`,
		art.id, actorID)
	if err != nil {
		return nil, fmt.Errorf("annotation: tally reactions for %s: %w", publicID, err)
	}
	defer rows.Close()

	var out []Tally
	for rows.Next() {
		var t Tally
		var anchorType string
		if err := rows.Scan(&anchorType, &t.AnchorKey, &t.Emoji, &t.Count, &t.Reacted); err != nil {
			return nil, fmt.Errorf("annotation: scan tally: %w", err)
		}
		t.AnchorType = sharetype.Anchor(anchorType)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("annotation: iterate tallies: %w", err)
	}
	return out, nil
}

// AddComment persists a comment or a one-level reply. A reply must reference a
// root comment on the same artifact (ErrParentNotFound / ErrThreadTooDeep) and
// share its anchor: pass a zero AnchorType to inherit it, or the same anchor
// to assert it (ErrAnchorMismatch otherwise). The insert and the artifact's
// comment_count / pin_count bumps commit in one transaction.
//
// Governing: ADR-0006 (shallow threads, soft delete), SPEC-0006 REQ "Threaded
// Comments", REQ "Count Aggregation".
func (s *Service) AddComment(ctx context.Context, publicID string, in CommentInput) (Comment, error) {
	if in.ActorID == "" {
		return Comment{}, errs.Validationf("annotation: comment: actor is required")
	}
	if in.Body == "" || len(in.Body) > maxCommentBytes {
		return Comment{}, fmt.Errorf("annotation: comment body length %d: %w", len(in.Body), ErrBodyInvalid)
	}

	var out Comment
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		art, err := resolveArtifact(ctx, tx, publicID)
		if err != nil {
			return err
		}

		anchorType, ref := in.AnchorType, in.AnchorRef
		if in.ParentID != nil {
			parent, err := loadParent(ctx, tx, art.id, *in.ParentID)
			if err != nil {
				return err
			}
			if anchorType == "" {
				// Inherit the root's anchor (SPEC-0006 "a reply's anchor MUST
				// match its root's anchor").
				anchorType, ref = parent.anchorType, parent.anchorRef
			} else {
				key, err := CanonicalKey(ref)
				if err != nil {
					return fmt.Errorf("annotation: reply anchor_ref: %v: %w", err, ErrLocatorInvalid)
				}
				if anchorType != parent.anchorType || key != parent.anchorKey {
					return fmt.Errorf("annotation: reply anchor %q/%s differs from root %q/%s: %w",
						anchorType, key, parent.anchorType, parent.anchorKey, ErrAnchorMismatch)
				}
			}
		}

		anchor, err := Validate(s.reg, art.id, art.shareType, sharetype.KindComment, anchorType, ref)
		if err != nil {
			return err
		}

		var (
			id        int64
			createdAt time.Time
		)
		if err := tx.QueryRow(ctx, `
			INSERT INTO comments (artifact_id, anchor_type, anchor_ref, anchor_key, parent_id, actor_id, on_behalf_of, body)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			RETURNING id, created_at`,
			anchor.ArtifactID, string(anchor.Type), anchor.Ref, anchor.Key,
			in.ParentID, in.ActorID, in.OnBehalfOf, in.Body,
		).Scan(&id, &createdAt); err != nil {
			return fmt.Errorf("annotation: insert comment: %w", err)
		}
		if err := bumpCounts(ctx, tx, anchor.ArtifactID, sharetype.KindComment, anchor.Type, +1); err != nil {
			return err
		}
		out = Comment{
			ID: id, Anchor: anchor, ParentID: in.ParentID,
			ActorID: in.ActorID, OnBehalfOf: in.OnBehalfOf, Body: in.Body,
			CreatedAt: createdAt,
		}
		return nil
	})
	if err != nil {
		return Comment{}, err
	}
	return out, nil
}

// EditComment replaces the body of the actor's own live comment and stamps
// edited_at (SPEC-0006 "edits MUST record edited_at"). Author-only.
func (s *Service) EditComment(ctx context.Context, publicID string, commentID int64, actorID, body string) error {
	if body == "" || len(body) > maxCommentBytes {
		return fmt.Errorf("annotation: comment body length %d: %w", len(body), ErrBodyInvalid)
	}
	return s.inTx(ctx, func(tx pgx.Tx) error {
		art, err := resolveArtifact(ctx, tx, publicID)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			UPDATE comments SET body = $1, edited_at = now()
			WHERE id = $2 AND artifact_id = $3 AND actor_id = $4 AND deleted_at IS NULL`,
			body, commentID, art.id, actorID)
		if err != nil {
			return fmt.Errorf("annotation: edit comment %d: %w", commentID, err)
		}
		if tag.RowsAffected() == 0 {
			return commentWriteRefusal(ctx, tx, art.id, commentID)
		}
		return nil
	})
}

// DeleteComment soft-deletes the actor's own comment — the tombstone keeps the
// thread structure and its replies resolvable (SPEC-0006 "Soft-deleted comment
// keeps the thread") — and decrements the rollups in the same transaction.
// Author-only.
func (s *Service) DeleteComment(ctx context.Context, publicID string, commentID int64, actorID string) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		art, err := resolveArtifact(ctx, tx, publicID)
		if err != nil {
			return err
		}
		var anchorType string
		err = tx.QueryRow(ctx, `
			UPDATE comments SET deleted_at = now()
			WHERE id = $1 AND artifact_id = $2 AND actor_id = $3 AND deleted_at IS NULL
			RETURNING anchor_type`,
			commentID, art.id, actorID).Scan(&anchorType)
		if errors.Is(err, pgx.ErrNoRows) {
			return commentWriteRefusal(ctx, tx, art.id, commentID)
		}
		if err != nil {
			return fmt.Errorf("annotation: delete comment %d: %w", commentID, err)
		}
		return bumpCounts(ctx, tx, art.id, sharetype.KindComment, sharetype.Anchor(anchorType), -1)
	})
}

// ListComments returns one artifact's comments in thread order: threads by
// their root's creation, each root immediately followed by its replies oldest
// first. Soft-deleted comments come back as tombstones (Deleted=true, empty
// body) so clients keep the structure without seeing removed content.
func (s *Service) ListComments(ctx context.Context, publicID string) ([]Comment, error) {
	art, err := resolveArtifact(ctx, s.pool, publicID)
	if err != nil {
		return nil, err
	}
	// COALESCE(parent_id, id) groups every thread under its root id; within a
	// thread the root sorts first because replies are created (and numbered)
	// after it.
	rows, err := s.pool.Query(ctx, `
		SELECT id, anchor_type, anchor_ref, anchor_key, parent_id, actor_id,
		       on_behalf_of, body, created_at, edited_at, deleted_at
		FROM comments
		WHERE artifact_id = $1
		ORDER BY COALESCE(parent_id, id), created_at, id`,
		art.id)
	if err != nil {
		return nil, fmt.Errorf("annotation: list comments for %s: %w", publicID, err)
	}
	defer rows.Close()

	var out []Comment
	for rows.Next() {
		var (
			c          Comment
			anchorType string
			deletedAt  *time.Time
		)
		c.Anchor.ArtifactID = art.id
		if err := rows.Scan(&c.ID, &anchorType, &c.Anchor.Ref, &c.Anchor.Key,
			&c.ParentID, &c.ActorID, &c.OnBehalfOf, &c.Body,
			&c.CreatedAt, &c.EditedAt, &deletedAt); err != nil {
			return nil, fmt.Errorf("annotation: scan comment: %w", err)
		}
		c.Anchor.Type = sharetype.Anchor(anchorType)
		if deletedAt != nil {
			c.Deleted = true
			c.Body = "" // tombstone: structure without content
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("annotation: iterate comments: %w", err)
	}
	return out, nil
}

// --- internals -------------------------------------------------------------

// resolvedArtifact is the slice of the artifact the service needs: the internal
// id every annotation query scopes to, and the share type the registry gates
// anchors against.
type resolvedArtifact struct {
	id        int64
	shareType artifact.ShareType
}

// queryRower is satisfied by both *pgxpool.Pool and pgx.Tx.
type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// resolveArtifact maps a public id to the internal id + share type, treating
// unknown and expired ids as a uniform not-found (ADR-0005 / ADR-0007: probing
// leaks no signal; expiry is hard non-existence).
func resolveArtifact(ctx context.Context, q queryRower, publicID string) (resolvedArtifact, error) {
	var art resolvedArtifact
	err := q.QueryRow(ctx,
		`SELECT id, share_type FROM artifacts WHERE public_id = $1 AND expires_at > now()`,
		publicID).Scan(&art.id, &art.shareType)
	if errors.Is(err, pgx.ErrNoRows) {
		return art, fmt.Errorf("annotation: resolve artifact %s: %w", publicID, errs.ErrNotFound)
	}
	if err != nil {
		return art, fmt.Errorf("annotation: resolve artifact %s: %w", publicID, err)
	}
	return art, nil
}

// parentComment is the slice of a root comment a reply is validated against.
type parentComment struct {
	anchorType sharetype.Anchor
	anchorRef  json.RawMessage
	anchorKey  string
}

// loadParent fetches a reply's parent, rejecting a missing/deleted parent
// (ErrParentNotFound) and a parent that is itself a reply (ErrThreadTooDeep).
func loadParent(ctx context.Context, tx pgx.Tx, artifactID, parentID int64) (parentComment, error) {
	var (
		p            parentComment
		anchorType   string
		parentParent *int64
	)
	err := tx.QueryRow(ctx, `
		SELECT anchor_type, anchor_ref, anchor_key, parent_id
		FROM comments
		WHERE id = $1 AND artifact_id = $2 AND deleted_at IS NULL`,
		parentID, artifactID).Scan(&anchorType, &p.anchorRef, &p.anchorKey, &parentParent)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, fmt.Errorf("annotation: parent comment %d: %w", parentID, ErrParentNotFound)
	}
	if err != nil {
		return p, fmt.Errorf("annotation: load parent comment %d: %w", parentID, err)
	}
	if parentParent != nil {
		return p, fmt.Errorf("annotation: comment %d is itself a reply: %w", parentID, ErrThreadTooDeep)
	}
	p.anchorType = sharetype.Anchor(anchorType)
	return p, nil
}

// commentWriteRefusal explains why an author-scoped comment mutation matched
// nothing: forbidden when the live comment exists under another actor,
// not-found otherwise (missing or already soft-deleted).
func commentWriteRefusal(ctx context.Context, tx pgx.Tx, artifactID, commentID int64) error {
	var exists bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM comments WHERE id = $1 AND artifact_id = $2 AND deleted_at IS NULL)`,
		commentID, artifactID).Scan(&exists); err != nil {
		return fmt.Errorf("annotation: check comment %d: %w", commentID, err)
	}
	if exists {
		return fmt.Errorf("annotation: comment %d belongs to another actor: %w", commentID, errs.ErrForbidden)
	}
	return fmt.Errorf("annotation: comment %d: %w", commentID, errs.ErrNotFound)
}

// bumpCounts applies the artifact-level rollup delta for one annotation write
// inside that write's transaction (SPEC-0006 "Atomic write + counter update").
// pin_count counts image-region annotations of either kind — image-region
// reactions plus pinned comments (ADR-0006). GREATEST floors the counters so a
// reconciliation bug can never drive them negative.
func bumpCounts(ctx context.Context, tx pgx.Tx, artifactID int64, kind sharetype.AnnotationKind, anchorType sharetype.Anchor, delta int) error {
	reactionDelta, commentDelta, pinDelta := 0, 0, 0
	switch kind {
	case sharetype.KindReaction:
		reactionDelta = delta
	case sharetype.KindComment:
		commentDelta = delta
	default:
		return fmt.Errorf("annotation: unknown annotation kind %q", kind)
	}
	if anchorType == sharetype.AnchorImageRegion {
		pinDelta = delta
	}
	_, err := tx.Exec(ctx, `
		UPDATE artifacts SET
			reaction_count = GREATEST(reaction_count + $2, 0),
			comment_count  = GREATEST(comment_count  + $3, 0),
			pin_count      = GREATEST(pin_count      + $4, 0)
		WHERE id = $1`,
		artifactID, reactionDelta, commentDelta, pinDelta)
	if err != nil {
		return fmt.Errorf("annotation: roll up counts for artifact %d: %w", artifactID, err)
	}
	return nil
}

// inTx runs fn inside one transaction, rolling back on any error so an
// annotation row and its counter bump commit or vanish together (SPEC-0006 REQ
// "Database Operation Standards").
func (s *Service) inTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("annotation: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("annotation: commit tx: %w", err)
	}
	return nil
}

// validateEmoji bounds a reaction emoji to a plausible single grapheme
// cluster: non-empty valid UTF-8 with no whitespace or control characters,
// within byte/rune bounds sized for ZWJ sequences. Arbitrary emoji are allowed
// (the issue explicitly permits any emoji); strict single-grapheme
// segmentation would need a Unicode segmentation library and is deliberately
// deferred.
func validateEmoji(emoji string) error {
	if emoji == "" || len(emoji) > maxEmojiBytes || !utf8.ValidString(emoji) {
		return fmt.Errorf("annotation: emoji %q: %w", emoji, ErrEmojiInvalid)
	}
	runes := 0
	for _, r := range emoji {
		runes++
		if runes > maxEmojiRunes || unicode.IsSpace(r) || unicode.IsControl(r) {
			return fmt.Errorf("annotation: emoji %q: %w", emoji, ErrEmojiInvalid)
		}
	}
	return nil
}
