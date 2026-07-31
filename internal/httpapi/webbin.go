package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/joestump/cairn/internal/annotation"
	"github.com/joestump/cairn/internal/artifact"
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

// binRow is one Bin listing row (design turn 6c). It carries the type badge
// (with the type label as a non-color text cue for WCAG), title + a content
// subtitle, the provenance line (`claude · via mcp · 2h`), the separate
// reaction / comment / pin counts (never summed — SPEC-0006 REQ "Count
// Aggregation"), and the lifetime cue (a TTL chip; live types show a live-dot
// keyed on TypeLabel in CSS, the same data-driven pattern the type badge uses,
// so the shell never switches on type in Go — ADR-0002). IsAgent / IsShared are
// the client-side tab lenses (Agents = agent-authored; Shared = has a live
// shareable link), applied over the already-rendered rows so keyset pagination
// stays intact and the no-JS view is the full Bin (progressive enhancement).
type binRow struct {
	ID            string
	Badge         string
	TypeLabel     string // the raw share-type key: the data-type/data-search hook
	TypeName      string // the reader-facing type name ("run" for a trajectory — #68)
	Title         string
	Subtitle      string
	WebURL        string
	Lead          string // agent (on-behalf-of) if present, else the human actor
	LeadShort     string // truncated agent version for display; full string in title tooltip
	Channel       string
	Age           string
	TTL           string // humanized time-to-expiry, e.g. "in 6d"
	ReactionCount int
	CommentCount  int
	PinCount      int
	IsAgent       bool // authored by an agent on the owner's behalf
	IsShared      bool // has a live shareable link (visibility != private)
}

// handleBinPage renders the workspace Bin as HTML at its legacy path (#57): `/`
// is now the canonical authenticated entry point (handleLanding), and this
// route stays live purely so existing `/bin` links keep resolving — both call
// the same renderBin projection, so the two paths can never disagree. It runs
// under requireWebSession, so an unauthenticated browser was already redirected
// to login.
func (s *Server) handleBinPage(w http.ResponseWriter, r *http.Request) {
	p, ok := principalFrom(r.Context())
	if !ok {
		s.writeError(w, r, errs.ErrUnauthorized, nil)
		return
	}
	s.renderBin(w, r, p)
}

// renderBin renders the Bin listing for an already-resolved principal, reusing
// the base layout with a listing body. It projects store.ListBin — the
// identical keyset query the JSON /v1/bin and the CLI TUI use — ordered by
// created_at, keyset-paginated so a "load more" neither skips nor duplicates
// rows under churn (SPEC-0001 REQ "The Bin Listing", REQ "Bin Pagination").
// Shared by the authenticated root `/` (handleLanding) and the legacy `/bin`
// path (handleBinPage) so both surfaces render the exact same workspace view.
func (s *Server) renderBin(w http.ResponseWriter, r *http.Request, p *Principal) {
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
		lead := firstNonEmpty(a.Provenance.OnBehalfOf, a.Provenance.ActorID)
		vm.Rows = append(vm.Rows, binRow{
			ID:            a.PublicID,
			Badge:         s.reg.BadgeFor(a),
			TypeLabel:     string(a.ShareType),
			TypeName:      s.reg.DisplayNameFor(a.ShareType),
			Title:         firstNonEmpty(a.Title, a.PublicID),
			Subtitle:      s.metaLine(a),
			WebURL:        s.webURL(a),
			Lead:          lead,
			LeadShort:     truncateAgentVersion(lead),
			Channel:       string(a.Provenance.Channel),
			Age:           humanizeSince(a.CreatedAt),
			TTL:           humanizeUntil(a.ExpiresAt),
			ReactionCount: a.ReactionCount,
			CommentCount:  a.CommentCount,
			PinCount:      a.PinCount,
			IsAgent:       a.Provenance.OnBehalfOf != "",
			IsShared:      a.Access.Visibility != artifact.VisibilityPrivate,
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

// handleWebComment posts a comment from the shell composer and returns the
// server-rendered comment partial for an HTMX append (SPEC-0001 REQ "Comment
// posts through the core"). The actor is the session principal — never a client
// claim — so provenance is server-derived; the anchor defaults to whole-artifact
// but the trajectory viewer's composer/selection toolbar may target a
// trajectory_span (a `span_id` form field) or a text_selection (`sel_start`,
// `sel_end`, `quote`), each validated against the share-type registry by the
// annotation service (SPEC-0006). requireAuth + enforceCSRF have already gated
// it: a session (ambient) caller needed a valid CSRF token, a bearer caller was
// exempt.
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
	anchorType, ref, err := webCommentAnchor(r)
	if err != nil {
		s.renderWebError(w, r, err)
		return
	}
	comment, err := s.annot.AddComment(r.Context(), id, annotation.CommentInput{
		AnchorType: anchorType,
		AnchorRef:  ref,
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

// webCommentAnchor reads the composer's optional anchor form fields and builds
// the (anchor_type, anchor_ref) the comment attaches to. With no anchor fields
// it is the whole-artifact anchor `{}` (which every commentable type accepts);
// a `span_id` targets a trajectory_span; `line` (the code viewer's per-line
// comment button, SPEC-0003 REQ "Code Annotation Anchors") targets a
// code_line; `sel_start`/`sel_end`/`quote` target a text_selection. The
// annotation service still validates the anchor against the registry, so an
// anchor the type forbids is rejected there — this only shapes the ref, it
// grants nothing.
func webCommentAnchor(r *http.Request) (sharetype.Anchor, json.RawMessage, error) {
	// A bundle's composer scopes every comment to the active member: the `file`
	// hidden field names the member, so the comment anchors to bundle_file
	// {"name":…} and the aggregated COMMENTS panel resolves it to that file (#19).
	// This precedes the selection path so a member's comment is member-scoped
	// even when a text selection was captured in the member pane.
	if file := r.PostFormValue("file"); file != "" {
		ref, err := json.Marshal(struct {
			Name string `json:"name"`
		}{file})
		if err != nil {
			return "", nil, errs.Validationf("comment: bad bundle file anchor")
		}
		return sharetype.AnchorBundleFile, ref, nil
	}
	// The image viewer's pin-drop composer sets pin_x/pin_y (normalized
	// fractional coordinates, ADR-0006) before submitting, mirroring the
	// bundle/markdown anchors above (SPEC-0003 REQ "Image Annotation Anchors",
	// #69). The annotation service still validates the ref against the
	// image_region locator schema (sharetype.imageRegionLocator), so an
	// out-of-range or malformed pair is rejected there, not trusted here.
	if px, py := r.PostFormValue("pin_x"), r.PostFormValue("pin_y"); px != "" && py != "" {
		x, errX := strconv.ParseFloat(px, 64)
		y, errY := strconv.ParseFloat(py, 64)
		if errX != nil || errY != nil {
			return "", nil, errs.Validationf("comment: pin coordinates must be numbers")
		}
		ref, err := json.Marshal(struct {
			X float64 `json:"x"`
			Y float64 `json:"y"`
		}{x, y})
		if err != nil {
			return "", nil, errs.Validationf("comment: bad pin anchor")
		}
		return sharetype.AnchorImageRegion, ref, nil
	}
	if spanID := r.PostFormValue("span_id"); spanID != "" {
		ref, err := json.Marshal(struct {
			SpanID string `json:"span_id"`
		}{spanID})
		if err != nil {
			return "", nil, errs.Validationf("comment: bad span anchor")
		}
		return sharetype.AnchorTrajectorySpan, ref, nil
	}
	// The code viewer's per-line comment button (code.js) sets a hidden `line`
	// field before submitting. The anchor_ref deliberately omits the optional
	// "hash" locator field (internal/annotation.LineHash) — including it would
	// change the canonical anchor_key (annotation.CanonicalRef sorts and emits
	// exactly the fields present), splitting one line's reactions/comments
	// across two keys depending on whether a hash happened to be sent.
	if lineStr := r.PostFormValue("line"); lineStr != "" {
		line, err := strconv.Atoi(lineStr)
		if err != nil || line < 1 {
			return "", nil, errs.Validationf("comment: line must be a positive integer")
		}
		ref, err := json.Marshal(struct {
			Line int `json:"line"`
		}{line})
		if err != nil {
			return "", nil, errs.Validationf("comment: bad line anchor")
		}
		return sharetype.AnchorCodeLine, ref, nil
	}
	if q := r.PostFormValue("quote"); q != "" {
		start, err1 := strconv.Atoi(r.PostFormValue("sel_start"))
		end, err2 := strconv.Atoi(r.PostFormValue("sel_end"))
		if err1 != nil || err2 != nil {
			return "", nil, errs.Validationf("comment: selection offsets must be integers")
		}
		ref, err := json.Marshal(struct {
			Start int    `json:"start"`
			End   int    `json:"end"`
			Quote string `json:"quote"`
		}{start, end, q})
		if err != nil {
			return "", nil, errs.Validationf("comment: bad selection anchor")
		}
		return sharetype.AnchorTextSelection, ref, nil
	}
	return sharetype.AnchorArtifact, json.RawMessage(`{}`), nil
}
