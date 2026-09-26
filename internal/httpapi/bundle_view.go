package httpapi

import (
	"context"
	"encoding/json"
	"html/template"
	"net/http"
	"net/url"
	"path"

	"github.com/go-chi/chi/v5"

	"github.com/stump-wtf/cairn/internal/annotation"
	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/code"
	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/sharetype"
	"github.com/stump-wtf/cairn/internal/store"
)

// The bundle viewer (SPEC-0003 REQ "Bundle Viewer", ADR-0011, issue #19). A
// bundle is bodyless: its render data is the ordered set of member files the
// store lists, not a single streamed body. So — like the trajectory viewer,
// which resolves its run tree through the trajectory service rather than the
// single-body BodyViewer seam — the bundle resolves its members through the
// store the web server already holds. It is still driven by registry DATA, not a
// switch on the type key: buildShellView picks the bundle path off the ADR-0002
// ComposedViewer capability (ComposedViewerFor), which only the bundle type
// declares, and delegates each member back through the registry (ClassifyMember
// → BodyViewerFor) to that member's own viewer.
//
// The whole page is server-rendered so the file rail, the active member's pane,
// and the aggregated COMMENTS panel are all present and readable with no
// JavaScript (SPEC-0003 REQ "Progressive Enhancement"): the rail's items are
// real links to `/{id}?file=<name>`, so a no-JS reader navigates member to
// member with a full render; bundle.js only layers on the roving-tabindex
// keyboard nav and the HTMX pane swap. Per-member reactions and comments anchor
// to bundle_file so they resolve to the correct member, and the panel labels
// each thread with its file ("· on notes.md").
//
// Governing: ADR-0011 (server-rendered viewer fragments), ADR-0002 (registry
// resolution; no switch on type), ADR-0006 (unified annotation layer), ADR-0007
// (link-capability read, uniform 404), SPEC-0003 (Bundle Viewer), SPEC-0002
// (ordered members), SPEC-0006 (registry-gated anchors).

// bundleView is the fully server-computed view model the bundle body slot
// renders: the file rail (its header + the per-member rows) and the active
// member's pane. Every field is derived from the store's member list + the
// annotation service, so the page carries no client data layer (ADR-0011).
type bundleView struct {
	ID         string
	FileCount  int
	TotalSize  string // humanized sum of member sizes
	Files      []bundleFile
	Active     bundleFile
	ActivePane memberPane
}

// bundleFile is one row in the file rail: a member's type badge, name, size, and
// its per-file engagement count (reactions + comments anchored to that member).
// PaneURL is the HTMX pane-swap target; ShellURL is the no-JS full-navigation
// fallback; TabID gives the tab a stable id so the pane can be aria-labelledby it.
type bundleFile struct {
	Name       string
	Badge      string
	MediaType  string
	Size       string
	Reactions  int
	Comments   int
	Engagement int // reactions + comments on this member
	Active     bool
	PaneURL    string
	ShellURL   string
	TabID      string
}

// memberPane is the rendered content of the active (or requested) member's pane.
// Exactly one of Body (a delegated rich viewer fragment, e.g. markdown) or Card
// (the generic-file floor for a non-previewable member) is set, so the pane
// always resolves to some viewer (ADR-0002 total resolution).
type memberPane struct {
	Name  string
	Badge string
	Body  template.HTML
	Card  *fileCardView
}

// buildBundleView projects a bundle artifact + its ordered members into the
// viewer model. It lists the members, computes the per-member engagement counts
// from the annotation thread + reaction tallies, selects the active member
// (`?file=` when it names a real member, else the first), and renders that
// member's pane by delegating through the registry. A bundle with no members (or
// a store/list failure) returns nil so the shell falls back to the generic-file
// card — the total-resolution floor is never bypassed.
func (s *Server) buildBundleView(ctx context.Context, a *artifact.Artifact, activeFile string, comments []annotation.Comment) *bundleView {
	members, err := s.store.ListMembers(ctx, a.PublicID)
	if err != nil {
		s.log.WarnContext(ctx, "web: list bundle members failed", "id", a.PublicID, "error", err)
		return nil
	}
	if len(members) == 0 {
		return nil
	}

	commentCounts := memberCommentCounts(comments)
	reactionCounts := s.memberReactionCounts(ctx, a.PublicID)

	vm := &bundleView{ID: a.PublicID, FileCount: len(members)}
	activeIdx := activeMemberIndex(members, activeFile)
	var totalSize int64
	for i, m := range members {
		totalSize += m.Size
		memberType := s.reg.ClassifyMember(m.Name, m.MediaType)
		f := bundleFile{
			Name:       m.Name,
			Badge:      s.reg.Resolve(memberType).Badge(),
			MediaType:  m.MediaType,
			Size:       humanizeBytes(m.Size),
			Reactions:  reactionCounts[m.Name],
			Comments:   commentCounts[m.Name],
			Engagement: reactionCounts[m.Name] + commentCounts[m.Name],
			Active:     i == activeIdx,
			PaneURL:    "/" + a.PublicID + "/pane/" + memberPath(m.Name),
			ShellURL:   "/" + a.PublicID + "?file=" + memberPath(m.Name),
			TabID:      "bundle-tab-" + itoa(i),
		}
		vm.Files = append(vm.Files, f)
	}
	vm.TotalSize = humanizeBytes(totalSize)
	vm.Active = vm.Files[activeIdx]
	vm.ActivePane = s.renderMemberPane(ctx, a, members[activeIdx])
	return vm
}

// renderMemberPane resolves one member to a rendered pane by delegating back
// through the registry (SPEC-0003 "delegate the selected member to that file's
// registered viewer"). A member whose classified type registers a BodyViewer
// (markdown in 0.0.2) renders that viewer's fragment from the member body;
// everything else — and any member whose viewer errors, or whose body cannot be
// opened — renders the generic-file card with a same-origin member download, so
// a member is never unviewable (ADR-0002 total resolution; SPEC-0003 "non-md
// members degrade to the file card").
func (s *Server) renderMemberPane(ctx context.Context, a *artifact.Artifact, m store.Member) memberPane {
	memberType := s.reg.ClassifyMember(m.Name, m.MediaType)
	pane := memberPane{Name: m.Name, Badge: s.reg.Resolve(memberType).Badge()}

	if viewer, ok := s.reg.BodyViewerFor(memberType); ok {
		if rc, _, err := s.store.OpenMember(ctx, a.PublicID, m.Name); err == nil {
			defer rc.Close()
			// RenderBody is handed the BUNDLE artifact — a member has no
			// artifact of its own — so a code viewer detecting its language
			// from a.MediaType/a.Title would read the bundle's, and every
			// member rendered as "Plain Text" however obvious its filename.
			// Resolve the language from the MEMBER's own name and media type
			// and pass it through the existing body-hint seam, the same channel
			// the web shell's ?lang= override already uses.
			ctx := sharetype.WithBodyHint(ctx, s.memberLangHint(memberType, m))
			if html, err := viewer.RenderBody(ctx, a, rc); err == nil {
				pane.Body = html
				return pane
			} else {
				s.log.WarnContext(ctx, "web: bundle member viewer failed, falling back to file card", "id", a.PublicID, "member", m.Name, "error", err)
			}
		} else {
			s.log.WarnContext(ctx, "web: open bundle member failed", "id", a.PublicID, "member", m.Name, "error", err)
		}
	}

	pane.Card = &fileCardView{
		Badge:       pane.Badge,
		Title:       m.Name,
		Size:        humanizeBytes(m.Size),
		MediaType:   m.MediaType,
		SHA256:      shortSHA(m.SHA256),
		SHA256Full:  m.SHA256,
		HasDownload: true,
		DownloadURL: "/" + a.PublicID + "/members/" + memberPath(m.Name),
	}
	return pane
}

// memberLangHint resolves the highlighting language for a bundle member from
// its OWN name and media type, returned as the lexer key the body-hint seam
// accepts. Empty for a non-code member, or when nothing is detectable — an
// empty hint leaves the viewer's own detection untouched.
func (s *Server) memberLangHint(memberType artifact.ShareType, m store.Member) string {
	if memberType != sharetype.KeyCode {
		return ""
	}
	if lang := code.Detect("", m.MediaType, m.Name); lang.Key != code.PlainTextKey {
		return lang.Key
	}
	return ""
}

// handleBundlePane serves one member's pane fragment for an HTMX swap
// (`GET /{id}/pane/<name>`), so switching members re-renders only the pane, not
// the whole shell (issue #19: "HTMX pane swap, no full reload"). It holds the
// same canonical bare-scheme discipline as the shell (the id must resolve, its
// registry web prefix must be empty, and it must be a composed/bundle type),
// else the same uniform 404 as an unknown id so the id space stays canonical and
// leaks no signal (ADR-0007). An unknown member name also 404s.
func (s *Server) handleBundlePane(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	name := chi.URLParam(r, "*")
	a, err := s.store.GetByPublicID(r.Context(), id)
	if err != nil {
		s.renderWebError(w, r, err)
		return
	}
	if _, ok := s.reg.ComposedViewerFor(a.ShareType); !ok || s.reg.URLPrefixFor(a.ShareType).Web != "" {
		s.renderWebError(w, r, errs.ErrNotFound)
		return
	}
	members, err := s.store.ListMembers(r.Context(), id)
	if err != nil {
		s.renderWebError(w, r, err)
		return
	}
	idx := activeMemberIndex(members, name)
	if len(members) == 0 || members[idx].Name != name {
		s.renderWebError(w, r, errs.ErrNotFound)
		return
	}
	pane := s.renderMemberPane(r.Context(), a, members[idx])
	s.renderWeb(w, r, "bundle-pane", pane)
}

// handleWebMemberDownload streams a bundle member's body as a safe attachment on
// the web surface, so the pane's generic-file card downloads stay same-origin
// under the web CSP (mirroring handleWebDownload for single-body artifacts, #12).
// serveBody sets `application/octet-stream` + `Content-Disposition: attachment`
// and the web group has already set nosniff, so untrusted member bytes can never
// be sniffed into an executable type or rendered inline in Cairn's origin
// (SPEC-0003 Security REQ "Sniff-proof raw body"). Unknown ids/members yield the
// uniform not-found.
func (s *Server) handleWebMemberDownload(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	name := chi.URLParam(r, "*")
	rc, info, err := s.store.OpenMember(r.Context(), id, name)
	if err != nil {
		s.renderWebError(w, r, err)
		return
	}
	defer rc.Close()
	s.serveBody(w, r, rc, info, memberBase(name))
}

// activeMemberIndex returns the index of the member named `want`, or 0 (the
// first member) when `want` is empty or names no member — the bundle always
// opens on some member.
func activeMemberIndex(members []store.Member, want string) int {
	if want == "" {
		return 0
	}
	for i, m := range members {
		if m.Name == want {
			return i
		}
	}
	return 0
}

// memberCommentCounts buckets the artifact's comment thread by member name,
// counting only comments anchored to a bundle_file member (a whole-artifact or
// unscoped comment counts toward no single file). Soft-deleted tombstones are
// excluded from the engagement tally.
func memberCommentCounts(comments []annotation.Comment) map[string]int {
	out := map[string]int{}
	for _, c := range comments {
		if c.Deleted || c.Anchor.Type != sharetype.AnchorBundleFile {
			continue
		}
		if name := memberNameFromRef(c.Anchor.Ref); name != "" {
			out[name]++
		}
	}
	return out
}

// memberReactionCounts buckets the artifact's reaction tallies by member name,
// summing the per-emoji counts of every bundle_file reaction. A nil annotation
// service (unit paths) yields an empty map.
func (s *Server) memberReactionCounts(ctx context.Context, publicID string) map[string]int {
	out := map[string]int{}
	if s.annot == nil {
		return out
	}
	tallies, err := s.annot.ReactionTallies(ctx, publicID, "")
	if err != nil {
		s.log.WarnContext(ctx, "web: bundle reaction tallies failed", "id", publicID, "error", err)
		return out
	}
	for _, t := range tallies {
		if t.AnchorType != sharetype.AnchorBundleFile {
			continue
		}
		if name := memberNameFromRef(json.RawMessage(t.AnchorKey)); name != "" {
			out[name] += t.Count
		}
	}
	return out
}

// memberNameFromRef extracts the member name from a bundle_file anchor_ref
// ({"name":"notes.md"[, "block_id":…]}); a malformed ref yields "".
func memberNameFromRef(ref json.RawMessage) string {
	var loc struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(ref, &loc) != nil {
		return ""
	}
	return loc.Name
}

// memberPath percent-escapes a member name for a URL path segment so a name with
// spaces or reserved characters routes correctly.
func memberPath(name string) string {
	return (&url.URL{Path: name}).EscapedPath()
}

// memberBase returns the last path element of a member name for the download
// filename, matching the /v1 member route's disposition.
func memberBase(name string) string {
	return path.Base(name)
}
