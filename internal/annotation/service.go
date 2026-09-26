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
//
// Ownership is per actor KIND as well as per actor id: an agent's bearer
// token and its human's browser session share one actor_id, and must never
// share, occupy or withdraw each other's annotations. Every row stores the
// server-derived actor_kind; rows written before it was stored carry '' and
// are never treated as human, and only the same actor_id removes them.
//
// Governing: ADR-0022 (per-kind idempotency), SPEC-0016 EV-6 "Per-Kind
// Reaction and Comment Ownership".
//
// Each committed write is announced on the optional event.Emitter, from here
// and nowhere else, so REST, web and MCP all emit through one choke point:
// comment.created, reaction.added for an inserted row only, and
// reaction.removed for each deleted row. A reaction event carries the EV-5
// approval bit, computed from the kind the row was stored with.
//
// Governing: ADR-0022, SPEC-0016 EV-2 "Emission Points", EV-5 "Approval Class
// and the Approval Bit".

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

	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/event"
	"github.com/stump-wtf/cairn/internal/sharetype"
	"github.com/stump-wtf/cairn/internal/store"
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
	// events receives comment.created, reaction.added and reaction.removed
	// after each write commits. Nil is inert (SPEC-0016 EV-2).
	events event.Emitter
	// approval classifies reaction emoji for the EV-5 approval bit.
	approval ApprovalClass
}

// Options configures a Service. Zero values fall back to safe defaults.
type Options struct {
	// Registry gates anchors per share type; nil is the process-wide default.
	Registry *sharetype.Registry
	// Emitter, when non-nil, receives the service's lifecycle events, each
	// after its transaction commits (ADR-0022, SPEC-0016 EV-2). Nil = inert.
	Emitter event.Emitter
	// Approval is the EV-5 approval class. The zero value is the default class
	// (👍 ✅ ✔️), never the empty one: an unconfigured deployment still
	// classifies approvals as the spec documents.
	Approval ApprovalClass
}

// NewService constructs a Service over a Postgres pool with no emitter. A nil
// registry falls back to the process-wide default.
func NewService(pool *pgxpool.Pool, reg *sharetype.Registry) *Service {
	return New(pool, Options{Registry: reg})
}

// New constructs a Service over a Postgres pool from Options.
func New(pool *pgxpool.Pool, opts Options) *Service {
	s := &Service{pool: pool, reg: opts.Registry, now: time.Now, events: opts.Emitter, approval: opts.Approval}
	if s.reg == nil {
		s.reg = sharetype.Default()
	}
	if s.approval.set == nil {
		s.approval = DefaultApprovalClass()
	}
	return s
}

// Reaction is one stored reaction row. ActorKind is the server-derived kind
// of the credential that wrote it, or "" for a row written before kinds were
// stored (never human). OnBehalfOf is provenance parity with comments.
type Reaction struct {
	ID         int64
	Anchor     Anchor
	Emoji      string
	ActorID    string
	ActorKind  event.ActorKind
	OnBehalfOf string
	CreatedAt  time.Time
}

// Comment is one stored comment. A soft-deleted comment is returned as a
// tombstone: Deleted is true and Body is empty, but the row (and therefore the
// thread structure under it) remains resolvable.
type Comment struct {
	ID         int64
	Anchor     Anchor
	ParentID   *int64
	ActorID    string
	ActorKind  event.ActorKind // "" for a comment written before kinds were stored
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
	// Actor is the authenticated author, derived server-side from the
	// principal (SPEC-0016 EV-4). Actor.OnBehalfOf names an agent acting for
	// the human (provenance parity with artifacts, ADR-0007).
	Actor event.Actor
	Body  string
}

// validateActor refuses a write whose actor has no identity, no derived kind,
// no recorded auth method, or a kind that contradicts its auth method. Every
// adapter derives the actor from the authenticated principal, so a gap means a
// caller skipped that step: fail closed rather than record an annotation nobody
// can classify (SPEC-0016 EV-3, EV-4).
func validateActor(op string, a event.Actor) error {
	if err := a.Check(); err != nil {
		return errs.Validationf("annotation: %s: %v", op, err)
	}
	return nil
}

// Viewer identifies who is reading reaction tallies, for the "did I react"
// flag: the actor id and the kind it authenticated as now. The zero Viewer is
// an anonymous link-capability read.
type Viewer struct {
	ID   string
	Kind event.ActorKind
}

// Tally is one per-anchor emoji tally, computed at view time over a single
// artifact's reactions (SPEC-0006 "Per-anchor emoji tally").
type Tally struct {
	AnchorType sharetype.Anchor
	AnchorKey  string
	Emoji      string
	Count      int
	// HumanCount and AgentCount split Count by the stored actor kind. Rows
	// written before kinds were stored are in neither: they are never human.
	HumanCount int
	AgentCount int
	// Reacted reports whether the requesting actor, as the kind it is now,
	// holds a row it could remove with this emoji on this anchor ("did I
	// react"): its own kind's row or a legacy one.
	Reacted bool
}

// React records an idempotent reaction: reacting with the same emoji to the
// same anchor twice by the same actor of the same kind is a no-op collapsed by
// the unique index, never a duplicate row or a double-counted rollup. The same
// actor_id reacting as the other kind (a human after their agent, or the
// reverse) gets its own row, so an agent can never occupy the human's. It
// returns the reaction row and whether it was newly created. The insert and
// the artifact's reaction_count / pin_count bumps commit in one transaction.
//
// Governing: ADR-0006 (idempotency by unique constraint, not
// read-modify-write), ADR-0022 (per-kind key), SPEC-0006 REQ "Idempotent
// Reactions", REQ "Count Aggregation", SPEC-0016 EV-6 "Agent cannot occupy the
// human's row".
func (s *Service) React(ctx context.Context, publicID string, anchorType sharetype.Anchor, ref json.RawMessage, emoji string, actor event.Actor) (Reaction, bool, error) {
	if err := validateEmoji(emoji); err != nil {
		return Reaction{}, false, err
	}
	if err := validateActor("react", actor); err != nil {
		return Reaction{}, false, err
	}

	var (
		out     Reaction
		created bool
		art     resolvedArtifact
	)
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		art, err = resolveArtifact(ctx, tx, publicID)
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
		var (
			id         int64
			createdAt  time.Time
			onBehalfOf = actor.OnBehalfOf
		)
		err = tx.QueryRow(ctx, `
			INSERT INTO reactions (artifact_id, anchor_type, anchor_ref, anchor_key, emoji, actor_id, actor_kind, on_behalf_of)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (artifact_id, anchor_type, anchor_key, emoji, actor_id, actor_kind) DO NOTHING
			RETURNING id, created_at`,
			anchor.ArtifactID, string(anchor.Type), anchor.Ref, anchor.Key, emoji,
			actor.ID, string(actor.Kind), actor.OnBehalfOf,
		).Scan(&id, &createdAt)
		switch {
		case err == nil:
			created = true
			if err := bumpCounts(ctx, tx, anchor.ArtifactID, sharetype.KindReaction, anchor.Type, +1); err != nil {
				return err
			}
		case errors.Is(err, pgx.ErrNoRows):
			// Duplicate react: surface the existing row, with the provenance it
			// was first recorded with, as the no-op result.
			if err := tx.QueryRow(ctx, `
				SELECT id, created_at, on_behalf_of FROM reactions
				WHERE artifact_id = $1 AND anchor_type = $2 AND anchor_key = $3 AND emoji = $4
				  AND actor_id = $5 AND actor_kind = $6`,
				anchor.ArtifactID, string(anchor.Type), anchor.Key, emoji, actor.ID, string(actor.Kind),
			).Scan(&id, &createdAt, &onBehalfOf); err != nil {
				return fmt.Errorf("annotation: load existing reaction: %w", err)
			}
		default:
			return fmt.Errorf("annotation: insert reaction: %w", err)
		}
		out = Reaction{
			ID: id, Anchor: anchor, Emoji: emoji,
			ActorID: actor.ID, ActorKind: actor.Kind, OnBehalfOf: onBehalfOf,
			CreatedAt: createdAt,
		}
		return nil
	})
	if err != nil {
		return Reaction{}, false, err
	}
	// Only the transaction that inserted announces the row: a duplicate is
	// silent (SPEC-0016 EV-2 "Duplicate reaction is silent").
	if created {
		s.emitReaction(event.ReactionAdded, art, actor, out.ID, out.Anchor.Type, out.Anchor.Key, emoji, actor.Kind)
	}
	return out, created, nil
}

// Unreact removes the actor's reaction identified by (anchor, emoji) — the
// toggle-off half of React. Only rows of the caller's own kind are removed,
// plus a legacy row (kind "") of the same actor_id: an agent can never
// withdraw its human's reaction, nor the human its agent's. It reports whether
// a row was removed (removing a reaction that does not exist is a no-op,
// matching the toggle semantics). The delete and the counter decrements
// commit in one transaction.
//
// Governing: SPEC-0016 EV-6 "Agent cannot withdraw the human's approval".
func (s *Service) Unreact(ctx context.Context, publicID string, anchorType sharetype.Anchor, ref json.RawMessage, emoji string, actor event.Actor) (bool, error) {
	if err := validateEmoji(emoji); err != nil {
		return false, err
	}
	if err := validateActor("unreact", actor); err != nil {
		return false, err
	}
	key, err := CanonicalKey(ref)
	if err != nil {
		return false, fmt.Errorf("annotation: unreact: %w", ErrLocatorInvalid)
	}

	var (
		art  resolvedArtifact
		gone []removedReaction
	)
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		art, err = resolveArtifact(ctx, tx, publicID)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			DELETE FROM reactions
			WHERE artifact_id = $1 AND anchor_type = $2 AND anchor_key = $3 AND emoji = $4
			  AND actor_id = $5 AND actor_kind IN ($6, '')
			RETURNING id, actor_kind`,
			art.id, string(anchorType), key, emoji, actor.ID, string(actor.Kind))
		if err != nil {
			return fmt.Errorf("annotation: delete reaction: %w", err)
		}
		gone, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (removedReaction, error) {
			var (
				r    removedReaction
				kind string
			)
			err := row.Scan(&r.id, &kind)
			r.kind = event.ActorKind(kind)
			return r, err
		})
		if err != nil {
			return fmt.Errorf("annotation: delete reaction: %w", err)
		}
		if len(gone) == 0 {
			return nil
		}
		// A legacy row and the caller's own-kind row can both go at once.
		return bumpCounts(ctx, tx, art.id, sharetype.KindReaction, anchorType, -len(gone))
	})
	if err != nil {
		return false, err
	}
	// One reaction.removed per deleted row, each classified by the kind that
	// row was STORED with: a legacy row is never an approval (SPEC-0016 EV-2,
	// EV-5 "Withdrawn approval is announced", EV-6 "Legacy row is never an
	// approval"). Removing nothing announces nothing.
	for _, r := range gone {
		s.emitReaction(event.ReactionRemoved, art, actor, r.id, anchorType, key, emoji, r.kind)
	}
	return len(gone) > 0, nil
}

// removedReaction is one row a Unreact deleted: its id and the actor kind it
// was stored with, which is what decides its approval bit.
type removedReaction struct {
	id   int64
	kind event.ActorKind
}

// UnreactByID deletes one reaction row by id — the REST
// `DELETE /v1/artifacts/{id}/reactions/{rid}` shape. Only the reaction's own
// actor, as the same kind, may remove it; a legacy row (kind "") only needs
// the same actor_id. Any other row is forbidden, including the caller's own
// actor_id under the other kind (SPEC-0006 REQ "Authentication &
// Authorization", SPEC-0016 EV-6 "Agent cannot remove by id").
func (s *Service) UnreactByID(ctx context.Context, publicID string, reactionID int64, actor event.Actor) error {
	if err := validateActor("unreact", actor); err != nil {
		return err
	}
	var (
		art                                resolvedArtifact
		anchorType, anchorKey, emoji, kind string
	)
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		art, err = resolveArtifact(ctx, tx, publicID)
		if err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `
			DELETE FROM reactions
			WHERE id = $1 AND artifact_id = $2 AND actor_id = $3 AND actor_kind IN ($4, '')
			RETURNING anchor_type, anchor_key, emoji, actor_kind`,
			reactionID, art.id, actor.ID, string(actor.Kind)).Scan(&anchorType, &anchorKey, &emoji, &kind)
		if errors.Is(err, pgx.ErrNoRows) {
			// Distinguish "someone else's reaction" (another actor, or the
			// same actor as the other kind) from "no such reaction" without
			// leaking other artifacts' rows: both checks stay scoped to this
			// artifact.
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
	if err != nil {
		return err
	}
	s.emitReaction(event.ReactionRemoved, art, actor, reactionID,
		sharetype.Anchor(anchorType), anchorKey, emoji, event.ActorKind(kind))
	return nil
}

// ReactionTallies returns the per-anchor emoji tallies for one artifact plus
// the requesting viewer's "did I react" flag, computed with a GROUP BY scoped
// to that artifact and backed by the (artifact_id, anchor_type, anchor_key)
// index (SPEC-0006 REQ "Count Aggregation", second tier). The viewer is
// matched by actor id AND kind, so a human never sees their agent's reaction
// as their own toggle (and the reverse). A zero viewer is an anonymous
// link-capability read: every Reacted is then false.
func (s *Service) ReactionTallies(ctx context.Context, publicID string, viewer Viewer) ([]Tally, error) {
	art, err := resolveArtifact(ctx, s.pool, publicID)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT anchor_type, anchor_key, emoji, count(*),
		       count(*) FILTER (WHERE actor_kind = 'human'),
		       count(*) FILTER (WHERE actor_kind = 'agent'),
		       bool_or($2 <> '' AND actor_id = $2 AND actor_kind IN ($3, ''))
		FROM reactions
		WHERE artifact_id = $1
		GROUP BY anchor_type, anchor_key, emoji
		ORDER BY anchor_type, anchor_key, count(*) DESC, emoji`,
		art.id, viewer.ID, string(viewer.Kind))
	if err != nil {
		return nil, fmt.Errorf("annotation: tally reactions for %s: %w", publicID, err)
	}
	defer rows.Close()

	var out []Tally
	for rows.Next() {
		var t Tally
		var anchorType string
		if err := rows.Scan(&anchorType, &t.AnchorKey, &t.Emoji, &t.Count,
			&t.HumanCount, &t.AgentCount, &t.Reacted); err != nil {
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

// ListReactions returns one artifact's reaction rows with their provenance
// (actor id, stored kind, on_behalf_of), grouped in the same (anchor_type,
// anchor_key) order ReactionTallies uses and oldest first within each emoji.
// It is the per-row counterpart of the tallies, and reads exactly what
// ListComments exposes for a comment, so a viewer can show an agent's
// reaction the way it shows an agent's comment. Same link-capability read:
// an unknown or expired id is the uniform not-found.
//
// Governing: SPEC-0016 EV-6 ("reactions MUST also store on_behalf_of,
// populated exactly as for comments"), SPEC-0009 REQ "Actor Captures Human and
// On-Behalf-Of Model".
func (s *Service) ListReactions(ctx context.Context, publicID string) ([]Reaction, error) {
	art, err := resolveArtifact(ctx, s.pool, publicID)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, anchor_type, anchor_ref, anchor_key, emoji, actor_id,
		       actor_kind, on_behalf_of, created_at
		FROM reactions
		WHERE artifact_id = $1
		ORDER BY anchor_type, anchor_key, emoji, created_at, id`,
		art.id)
	if err != nil {
		return nil, fmt.Errorf("annotation: list reactions for %s: %w", publicID, err)
	}
	defer rows.Close()

	var out []Reaction
	for rows.Next() {
		var (
			r          Reaction
			anchorType string
			actorKind  string
		)
		r.Anchor.ArtifactID = art.id
		if err := rows.Scan(&r.ID, &anchorType, &r.Anchor.Ref, &r.Anchor.Key,
			&r.Emoji, &r.ActorID, &actorKind, &r.OnBehalfOf, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("annotation: scan reaction: %w", err)
		}
		r.Anchor.Type = sharetype.Anchor(anchorType)
		r.ActorKind = event.ActorKind(actorKind)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("annotation: iterate reactions: %w", err)
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
	if err := validateActor("comment", in.Actor); err != nil {
		return Comment{}, err
	}
	if in.Body == "" || len(in.Body) > maxCommentBytes {
		return Comment{}, fmt.Errorf("annotation: comment body length %d: %w", len(in.Body), ErrBodyInvalid)
	}

	var (
		out Comment
		art resolvedArtifact
	)
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		art, err = resolveArtifact(ctx, tx, publicID)
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
			INSERT INTO comments (artifact_id, anchor_type, anchor_ref, anchor_key, parent_id, actor_id, actor_kind, on_behalf_of, body)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			RETURNING id, created_at`,
			anchor.ArtifactID, string(anchor.Type), anchor.Ref, anchor.Key,
			in.ParentID, in.Actor.ID, string(in.Actor.Kind), in.Actor.OnBehalfOf, in.Body,
		).Scan(&id, &createdAt); err != nil {
			return fmt.Errorf("annotation: insert comment: %w", err)
		}
		if err := bumpCounts(ctx, tx, anchor.ArtifactID, sharetype.KindComment, anchor.Type, +1); err != nil {
			return err
		}
		out = Comment{
			ID: id, Anchor: anchor, ParentID: in.ParentID,
			ActorID: in.Actor.ID, ActorKind: in.Actor.Kind, OnBehalfOf: in.Actor.OnBehalfOf, Body: in.Body,
			CreatedAt: createdAt,
		}
		return nil
	})
	if err != nil {
		return Comment{}, err
	}
	// Emitted only for a committed insert: a rolled-back write returned above
	// (SPEC-0016 EV-2 "Rolled-back write emits nothing"). The encoder, not the
	// service, truncates the body for the wire (EV-3).
	s.emit(event.Event{
		Kind:    event.CommentCreated,
		Subject: art.subject(s.reg),
		Actor:   in.Actor,
		Comment: &event.Comment{
			ID:         out.ID,
			ParentID:   out.ParentID,
			AnchorType: string(out.Anchor.Type),
			AnchorKey:  out.Anchor.Key,
			Body:       out.Body,
		},
	})
	return out, nil
}

// EditComment replaces the body of the actor's own live comment and stamps
// edited_at (SPEC-0006 "edits MUST record edited_at"). Author-only, and
// kind-scoped like reaction removal: the author's other kind is forbidden,
// and a legacy comment (kind "") needs only the same actor_id (SPEC-0016
// EV-6).
func (s *Service) EditComment(ctx context.Context, publicID string, commentID int64, actor event.Actor, body string) error {
	if err := validateActor("edit comment", actor); err != nil {
		return err
	}
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
			WHERE id = $2 AND artifact_id = $3 AND actor_id = $4 AND actor_kind IN ($5, '')
			  AND deleted_at IS NULL`,
			body, commentID, art.id, actor.ID, string(actor.Kind))
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
// Author-only and kind-scoped, as EditComment.
func (s *Service) DeleteComment(ctx context.Context, publicID string, commentID int64, actor event.Actor) error {
	if err := validateActor("delete comment", actor); err != nil {
		return err
	}
	return s.inTx(ctx, func(tx pgx.Tx) error {
		art, err := resolveArtifact(ctx, tx, publicID)
		if err != nil {
			return err
		}
		var anchorType string
		err = tx.QueryRow(ctx, `
			UPDATE comments SET deleted_at = now()
			WHERE id = $1 AND artifact_id = $2 AND actor_id = $3 AND actor_kind IN ($4, '')
			  AND deleted_at IS NULL
			RETURNING anchor_type`,
			commentID, art.id, actor.ID, string(actor.Kind)).Scan(&anchorType)
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
		       actor_kind, on_behalf_of, body, created_at, edited_at, deleted_at
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
			actorKind  string
			deletedAt  *time.Time
		)
		c.Anchor.ArtifactID = art.id
		if err := rows.Scan(&c.ID, &anchorType, &c.Anchor.Ref, &c.Anchor.Key,
			&c.ParentID, &c.ActorID, &actorKind, &c.OnBehalfOf, &c.Body,
			&c.CreatedAt, &c.EditedAt, &deletedAt); err != nil {
			return nil, fmt.Errorf("annotation: scan comment: %w", err)
		}
		c.Anchor.Type = sharetype.Anchor(anchorType)
		c.ActorKind = event.ActorKind(actorKind)
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
// id every annotation query scopes to, the share type the registry gates
// anchors against, and the fields that describe it as the subject of a
// lifecycle event, read in the same statement so the event names the artifact
// the write resolved.
type resolvedArtifact struct {
	id        int64
	shareType artifact.ShareType
	publicID  string
	title     string
	tags      []string
	expiresAt time.Time
	ownerID   string
}

// subject describes the artifact as the subject of a lifecycle event, exactly
// as its artifact.created did (SPEC-0016 EV-3 "Subject fields resolve the
// artifact for every kind"). OwnerID routes it and never reaches the wire
// (EV-7).
func (a resolvedArtifact) subject(reg *sharetype.Registry) event.Subject {
	return event.Subject{
		PublicID:  a.publicID,
		ShareType: a.shareType,
		Title:     a.title,
		WebPath:   store.WebPath(reg, a.shareType, a.publicID),
		Tags:      a.tags,
		ExpiresAt: a.expiresAt,
		OwnerID:   a.ownerID,
	}
}

// emit hands a committed event to the emitter, if one is installed. It is
// best-effort by contract: the emitter never blocks or fails the request that
// caused the event (SPEC-0016 EV-2).
func (s *Service) emit(ev event.Event) {
	if s.events == nil {
		return
	}
	s.events.Emit(ev)
}

// emitReaction announces one committed reaction row. stored is the kind the
// row was written with — for an add, the caller's; for a removal, the deleted
// row's — and it alone decides the approval bit, so a legacy row (kind "")
// never announces an approval however it is removed (SPEC-0016 EV-5, EV-6).
// The actor is always the caller, derived from its credential (EV-4).
func (s *Service) emitReaction(kind event.Kind, art resolvedArtifact, actor event.Actor, id int64, anchorType sharetype.Anchor, anchorKey, emoji string, stored event.ActorKind) {
	class, approval := s.approval.Approval(emoji, stored)
	s.emit(event.Event{
		Kind:    kind,
		Subject: art.subject(s.reg),
		Actor:   actor,
		Reaction: &event.Reaction{
			ID:            id,
			AnchorType:    string(anchorType),
			AnchorKey:     anchorKey,
			Emoji:         emoji,
			ApprovalClass: class,
			Approval:      approval,
		},
	})
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
	err := q.QueryRow(ctx, `
		SELECT id, share_type, public_id, title, tags, expires_at, owner_id
		FROM artifacts WHERE public_id = $1 AND expires_at > now()`,
		publicID).Scan(&art.id, &art.shareType, &art.publicID, &art.title, &art.tags, &art.expiresAt, &art.ownerID)
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
// nothing: forbidden when the live comment exists under another actor (or the
// same actor as the other kind), not-found otherwise (missing or already
// soft-deleted).
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
