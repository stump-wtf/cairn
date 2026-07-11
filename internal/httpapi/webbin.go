package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/joestump/cairn/internal/annotation"
	"github.com/joestump/cairn/internal/errs"
	"github.com/joestump/cairn/internal/sharetype"
)

// The authenticated Bin (SPEC-0001 REQ "The Bin Listing") and the shell's web
// comment post, both realized over the same core the /v1 API projects (ADR-0003)
// — the Bin is the *same* keyset query the JSON /v1/bin and the CLI TUI render,
// and a comment posts through the same annotation service as the JSON API.

// binPageView is the Bin listing view model: the workspace rows, the keyset
// cursor for the next page's "load more", and the signed-in actor for the header.
type binPageView struct {
	Actor      string
	Rows       []binRow
	NextCursor string
	CSRFToken  string
}

// binRow is one Bin listing row: the type badge (with the type label as a
// non-color text cue for WCAG), title, provenance, and the separate reaction /
// comment counts (never summed — SPEC-0006 REQ "Count Aggregation").
type binRow struct {
	ID            string
	Badge         string
	TypeLabel     string
	Title         string
	WebURL        string
	Actor         string
	OnBehalfOf    string
	Channel       string
	Age           string
	ReactionCount int
	CommentCount  int
}

// handleBinPage renders the workspace Bin as HTML, reusing the base layout with a
// listing body. It projects store.ListBin — the identical keyset query the JSON
// /v1/bin and the CLI TUI use — ordered by created_at, keyset-paginated so a
// "load more" neither skips nor duplicates rows under churn (SPEC-0001 REQ "The
// Bin Listing", REQ "Bin Pagination"). It runs under requireWebSession, so an
// unauthenticated browser was already redirected to login.
func (s *Server) handleBinPage(w http.ResponseWriter, r *http.Request) {
	p, ok := principalFrom(r.Context())
	if !ok {
		s.writeError(w, r, errs.ErrUnauthorized, nil)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	page, err := s.store.ListBin(r.Context(), p.ActorID, r.URL.Query().Get("cursor"), limit)
	if err != nil {
		s.renderWebError(w, r, err)
		return
	}
	vm := binPageView{Actor: p.ActorID, NextCursor: page.NextCursor}
	if c, cerr := r.Cookie(csrfCookieName); cerr == nil {
		vm.CSRFToken = c.Value
	}
	for _, a := range page.Artifacts {
		vm.Rows = append(vm.Rows, binRow{
			ID:            a.PublicID,
			Badge:         s.reg.BadgeFor(a),
			TypeLabel:     string(a.ShareType),
			Title:         firstNonEmpty(a.Title, a.PublicID),
			WebURL:        s.webURL(a),
			Actor:         a.Provenance.ActorID,
			OnBehalfOf:    a.Provenance.OnBehalfOf,
			Channel:       string(a.Provenance.Channel),
			Age:           humanizeSince(a.CreatedAt),
			ReactionCount: a.ReactionCount,
			CommentCount:  a.CommentCount,
		})
	}
	// An HTMX "load more" fetch asks for just the appended rows; a full navigation
	// gets the whole page. Both project the same query, so the listing is provably
	// identical either way.
	name := "bin"
	if r.Header.Get("HX-Request") != "" && r.URL.Query().Get("cursor") != "" {
		name = "bin-rows"
	}
	s.renderWeb(w, r, name, vm)
}

// handleWebComment posts a whole-artifact comment from the shell composer and
// returns the server-rendered comment partial for an HTMX append (SPEC-0001 REQ
// "Comment posts through the core"). The actor is the session principal — never a
// client claim — so provenance is server-derived (SPEC-0009); the anchor is the
// whole-artifact anchor, which every commentable type accepts. requireAuth +
// enforceCSRF have already gated it: a session (ambient) caller needed a valid
// CSRF token, a bearer caller was exempt.
func (s *Server) handleWebComment(w http.ResponseWriter, r *http.Request) {
	p, ok := principalFrom(r.Context())
	if !ok {
		s.writeError(w, r, errs.ErrUnauthorized, nil)
		return
	}
	id := chi.URLParam(r, "id")
	r.Body = http.MaxBytesReader(w, r.Body, maxAnnotationRequestBytes)
	if err := r.ParseForm(); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			s.renderWebError(w, r, errs.ErrTooLarge)
			return
		}
		s.renderWebError(w, r, errs.Validationf("comment: malformed form"))
		return
	}
	comment, err := s.annot.AddComment(r.Context(), id, annotation.CommentInput{
		AnchorType: sharetype.AnchorArtifact,
		AnchorRef:  json.RawMessage(`{}`),
		ActorID:    p.ActorID,
		Body:       r.PostFormValue("body"),
	})
	if err != nil {
		s.renderWebError(w, r, err)
		return
	}
	line := toCommentLines([]annotation.Comment{comment})[0]
	s.renderWeb(w, r, "comment-item", line)
}
