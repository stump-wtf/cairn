package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/stump-wtf/cairn/internal/annotation"
	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/sharetype"
)

// maxAnnotationRequestBytes bounds an annotation request body before it is
// buffered, so an oversize comment or reaction payload is rejected with 413
// rather than read into memory (SPEC-0006 REQ "Request Body Size Limits"). It
// sits comfortably above the service-level 16 KiB comment-body invariant so the
// service's own validation, not the transport cap, explains a merely-too-long
// body.
const maxAnnotationRequestBytes = 32 << 10

// reactionRequest is the POST/DELETE reactions body: the anchor to attach to
// and the emoji. anchor_ref is the raw locator object, validated per anchor_type
// by the registry (an absent ref is the whole-artifact `{}`).
type reactionRequest struct {
	AnchorType string          `json:"anchor_type"`
	AnchorRef  json.RawMessage `json:"anchor_ref,omitempty"`
	Emoji      string          `json:"emoji"`
}

// reactionResponse is the JSON view of a stored reaction. ID is the handle the
// DELETE /reactions/{rid} shape removes.
type reactionResponse struct {
	ID         int64           `json:"id"`
	AnchorType string          `json:"anchor_type"`
	AnchorRef  json.RawMessage `json:"anchor_ref"`
	Emoji      string          `json:"emoji"`
	ActorID    string          `json:"actor_id"`
	CreatedAt  time.Time       `json:"created_at"`
}

// tallyResponse is the GET reactions view: per-anchor emoji tallies plus the
// requesting actor's "did I react" flag (empty actor ⇒ all false).
type tallyResponse struct {
	Reactions []tallyView `json:"reactions"`
}

type tallyView struct {
	AnchorType string `json:"anchor_type"`
	AnchorKey  string `json:"anchor_key"`
	Emoji      string `json:"emoji"`
	Count      int    `json:"count"`
	Reacted    bool   `json:"reacted"`
}

// commentRequest is the POST comments body. A nil parent_id is a thread root; a
// reply may omit the anchor to inherit its root's (SPEC-0006 "Threaded
// Comments"). on_behalf_of records an agent acting for the human.
type commentRequest struct {
	AnchorType string          `json:"anchor_type"`
	AnchorRef  json.RawMessage `json:"anchor_ref,omitempty"`
	ParentID   *int64          `json:"parent_id,omitempty"`
	OnBehalfOf string          `json:"on_behalf_of,omitempty"`
	Body       string          `json:"body"`
}

// commentResponse is the JSON view of a comment. A soft-deleted comment renders
// as a tombstone: deleted=true, empty body, structure intact.
type commentResponse struct {
	ID         int64           `json:"id"`
	AnchorType string          `json:"anchor_type"`
	AnchorRef  json.RawMessage `json:"anchor_ref"`
	AnchorKey  string          `json:"anchor_key"`
	ParentID   *int64          `json:"parent_id,omitempty"`
	ActorID    string          `json:"actor_id"`
	OnBehalfOf string          `json:"on_behalf_of,omitempty"`
	Body       string          `json:"body"`
	CreatedAt  time.Time       `json:"created_at"`
	EditedAt   *time.Time      `json:"edited_at,omitempty"`
	Deleted    bool            `json:"deleted"`
}

type commentsResponse struct {
	Comments []commentResponse `json:"comments"`
}

// handleReact records an idempotent reaction (SPEC-0006 REQ "Idempotent
// Reactions"). A first react returns 201; reacting again with the same
// (anchor, emoji, actor) is a no-op that returns 200 with the existing row, so
// a double-tap never duplicates or double-counts.
func (s *Server) handleReact(w http.ResponseWriter, r *http.Request) {
	p, ok := principalFrom(r.Context())
	if !ok {
		s.writeError(w, r, errs.ErrUnauthorized, nil)
		return
	}
	id := chi.URLParam(r, "id")
	var req reactionRequest
	if err := s.decodeAnnotationBody(w, r, &req); err != nil {
		s.writeError(w, r, err, map[string]string{"id": id})
		return
	}
	reaction, created, err := s.annot.React(
		r.Context(), id, sharetype.Anchor(req.AnchorType), req.AnchorRef, req.Emoji, p.ActorID)
	if err != nil {
		s.writeError(w, r, err, map[string]string{"id": id, "anchor_type": req.AnchorType})
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	s.writeJSON(w, status, toReactionResponse(reaction))
}

// handleUnreact is the toggle-off half: it removes the caller's (anchor, emoji)
// reaction addressed by value rather than id, so a client that reacted can
// un-react without tracking the row id. Removing a reaction that is not there
// is a no-op — the toggle is idempotent — so both cases return 204.
func (s *Server) handleUnreact(w http.ResponseWriter, r *http.Request) {
	p, ok := principalFrom(r.Context())
	if !ok {
		s.writeError(w, r, errs.ErrUnauthorized, nil)
		return
	}
	id := chi.URLParam(r, "id")
	var req reactionRequest
	if err := s.decodeAnnotationBody(w, r, &req); err != nil {
		s.writeError(w, r, err, map[string]string{"id": id})
		return
	}
	if _, err := s.annot.Unreact(
		r.Context(), id, sharetype.Anchor(req.AnchorType), req.AnchorRef, req.Emoji, p.ActorID); err != nil {
		s.writeError(w, r, err, map[string]string{"id": id, "anchor_type": req.AnchorType})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleUnreactByID is the ADR-0012 DELETE /reactions/{rid} shape: it removes
// one reaction row by id, author-only (a foreign row is 403, a missing one is a
// uniform 404, both scoped to this artifact so nothing leaks).
func (s *Server) handleUnreactByID(w http.ResponseWriter, r *http.Request) {
	p, ok := principalFrom(r.Context())
	if !ok {
		s.writeError(w, r, errs.ErrUnauthorized, nil)
		return
	}
	id := chi.URLParam(r, "id")
	rid, err := parseReactionID(chi.URLParam(r, "rid"))
	if err != nil {
		s.writeError(w, r, err, map[string]string{"id": id})
		return
	}
	if err := s.annot.UnreactByID(r.Context(), id, rid, p.ActorID); err != nil {
		s.writeError(w, r, err, map[string]string{"id": id})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// parseReactionID parses the {rid} path segment (SPEC-0019 VE-1, VE-6).
func parseReactionID(raw string) (int64, error) {
	rid, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("reaction id %q: %w", raw, errs.Violate("rid", errs.LocPath, errs.ReasonInvalidFormat,
			errs.WithValue(raw), errs.WithExpect("an integer reaction id")))
	}
	return rid, nil
}

// handleListReactions returns the per-anchor emoji tallies for one artifact
// (SPEC-0006 REQ "Count Aggregation", per-anchor tier). The read follows the
// artifact's link capability: a valid id reads, an unknown/expired id is a
// uniform 404. When the reader is authenticated, each tally carries their "did
// I react" flag; an anonymous link read gets all-false.
func (s *Server) handleListReactions(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tallies, err := s.annot.ReactionTallies(r.Context(), id, s.optionalActor(r))
	if err != nil {
		s.writeError(w, r, err, map[string]string{"id": id})
		return
	}
	out := tallyResponse{Reactions: make([]tallyView, 0, len(tallies))}
	for _, t := range tallies {
		out.Reactions = append(out.Reactions, tallyView{
			AnchorType: string(t.AnchorType),
			AnchorKey:  t.AnchorKey,
			Emoji:      t.Emoji,
			Count:      t.Count,
			Reacted:    t.Reacted,
		})
	}
	s.writeJSON(w, http.StatusOK, out)
}

// handleComment posts a comment or a one-level reply (SPEC-0006 REQ "Threaded
// Comments"). Registry validation gates the anchor — a comment on a
// non-commentable anchor (e.g. any webhook anchor) is validation_failed and
// persists nothing.
func (s *Server) handleComment(w http.ResponseWriter, r *http.Request) {
	p, ok := principalFrom(r.Context())
	if !ok {
		s.writeError(w, r, errs.ErrUnauthorized, nil)
		return
	}
	id := chi.URLParam(r, "id")
	var req commentRequest
	if err := s.decodeAnnotationBody(w, r, &req); err != nil {
		s.writeError(w, r, err, map[string]string{"id": id})
		return
	}
	comment, err := s.annot.AddComment(r.Context(), id, annotation.CommentInput{
		AnchorType: sharetype.Anchor(req.AnchorType),
		AnchorRef:  req.AnchorRef,
		ParentID:   req.ParentID,
		ActorID:    p.ActorID,
		OnBehalfOf: req.OnBehalfOf,
		Body:       req.Body,
	})
	if err != nil {
		s.writeError(w, r, err, map[string]string{"id": id, "anchor_type": req.AnchorType})
		return
	}
	s.writeJSON(w, http.StatusCreated, toCommentResponse(comment))
}

// handleListComments returns one artifact's comments in thread order with
// tombstones preserved (SPEC-0006 "Threaded Comments"). Link-capability read:
// valid id reads, unknown/expired id is a uniform 404.
func (s *Server) handleListComments(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	comments, err := s.annot.ListComments(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err, map[string]string{"id": id})
		return
	}
	out := commentsResponse{Comments: make([]commentResponse, 0, len(comments))}
	for _, c := range comments {
		out.Comments = append(out.Comments, toCommentResponse(c))
	}
	s.writeJSON(w, http.StatusOK, out)
}

// decodeAnnotationBody caps the request body at maxAnnotationRequestBytes and
// decodes it as JSON. An oversize body is 413 (before the whole payload is
// buffered); malformed JSON is validation_failed. Bounding here, not in the
// service, keeps the 413 a transport concern (SPEC-0006 REQ "Request Body Size
// Limits").
func (s *Server) decodeAnnotationBody(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxAnnotationRequestBytes)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		return fmt.Errorf("decode annotation body: %w", jsonBodyViolation(err))
	}
	return nil
}

// jsonBodyViolation maps a JSON body decode failure to its violation: body
// too_large at the MaxBytesReader cap (413), or body invalid_format (400).
// The decoder's own message is kept for the log line only.
//
// Governing: ADR-0025, SPEC-0019 VE-1, VE-6
func jsonBodyViolation(err error) error {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		return errs.Violate("body", errs.LocBody, errs.ReasonTooLarge, errs.WithLimit(maxErr.Limit, errs.UnitBytes)).Because(err)
	}
	return errs.Violate("body", errs.LocBody, errs.ReasonInvalidFormat, errs.WithExpect("a JSON object")).Because(err)
}

// optionalActor resolves the caller's actor id when the read carries valid
// credentials, or "" for an anonymous link read. It never rejects: annotation
// reads are gated by the artifact's link capability, not by authentication, so
// an absent or invalid credential simply yields the anonymous "did I react"
// view (all false).
func (s *Server) optionalActor(r *http.Request) string {
	if s.auth == nil {
		return ""
	}
	p, err := s.auth.Authenticate(r)
	if err != nil || p == nil {
		return ""
	}
	return p.ActorID
}

func toReactionResponse(r annotation.Reaction) reactionResponse {
	return reactionResponse{
		ID:         r.ID,
		AnchorType: string(r.Anchor.Type),
		AnchorRef:  r.Anchor.Ref,
		Emoji:      r.Emoji,
		ActorID:    r.ActorID,
		CreatedAt:  r.CreatedAt,
	}
}

func toCommentResponse(c annotation.Comment) commentResponse {
	return commentResponse{
		ID:         c.ID,
		AnchorType: string(c.Anchor.Type),
		AnchorRef:  c.Anchor.Ref,
		AnchorKey:  c.Anchor.Key,
		ParentID:   c.ParentID,
		ActorID:    c.ActorID,
		OnBehalfOf: c.OnBehalfOf,
		Body:       c.Body,
		CreatedAt:  c.CreatedAt,
		EditedAt:   c.EditedAt,
		Deleted:    c.Deleted,
	}
}
