package httpapi

import (
	"context"
	"embed"
	"encoding/json"
	"html/template"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/joestump/cairn/internal/annotation"
	"github.com/joestump/cairn/internal/artifact"
	"github.com/joestump/cairn/internal/errs"
	"github.com/joestump/cairn/internal/sharetype"
)

// The web app shell (ADR-0011, SPEC-0001): a single server-rendered
// `html/template` layout that presents EVERY share type through one chrome —
// header (logo · type badge · title · one URL control with copy + `◆ mcp` ·
// Share · panel toggle), a registry-resolved body slot, and a collapsible
// right-hand panel (provenance · type-specific metadata · comments). The shell
// never switches on share type: the body partial comes from the ADR-0002
// BodyViewer capability (with the generic-file card as the total-resolution
// floor) and the type-specific panel fields from the MetadataPanel capability,
// so adding a share type is adding a partial, not editing the shell
// (SPEC-0001 REQ "Unified App Shell", REQ "Type-Specific Body Slot").
//
// Read is progressively enhanced: the server renders complete HTML, so opening
// a share and reading its body + comments works with no JavaScript; HTMX and
// Alpine only layer on the local URL/panel affordances (SPEC-0001 REQ
// "Progressive Enhancement"). These HTML routes carry their OWN Content-Security
// -Policy (self + the script/style/font origins the embedded HTMX+Alpine need),
// scoped by route so the strict `default-src 'none'` policy of the /v1 JSON API
// is never loosened (SPEC-0001 REQ "Security Headers").
//
// Governing: ADR-0011, ADR-0002 (registry-resolved viewer/panel; no switch on
// type), ADR-0005 (id URL scheme), ADR-0007 (link-capability read, uniform
// 404), SPEC-0001.

//go:embed web/templates/*.html
var webTemplateFS embed.FS

//go:embed web/assets
var webAssetFS embed.FS

// webCSP is the Content-Security-Policy for the HTML shell routes. It permits
// the app's own scripts (embedded HTMX + Alpine, served from /assets), styles,
// and fonts from the same origin, and inline `data:` images, while forbidding
// framing and inline <script> execution. `script-src` is a bare `'self'`: the
// vendored Alpine is the `@alpinejs/csp` build, whose whole purpose is to avoid
// runtime expression evaluation (directives resolve only to registered
// property access / method calls — it contains no `eval`/`new Function`), and
// HTMX's one `eval` path is gated behind `hx-*` attributes / `js:` prefixes,
// of which the templates use none. So neither library needs `'unsafe-eval'`,
// and omitting both it and `'unsafe-inline'` means no inline handler, inline
// <script>, or eval() vector can execute on the surface that renders untrusted
// user content (titles, comments) — the requirement is "forbid inline
// execution," which this satisfies (SPEC-0001 REQ "Security Headers"). This is
// a per-route policy: the /v1 API keeps its strict `default-src 'none'` (see
// securityHeaders).
const webCSP = "default-src 'none'; " +
	"base-uri 'none'; " +
	"img-src 'self' data:; " +
	"style-src 'self'; " +
	"script-src 'self'; " +
	"font-src 'self'; " +
	"connect-src 'self'; " +
	"form-action 'self'; " +
	"frame-ancestors 'none'"

// parseWebTemplates parses the embedded shell templates, wiring the helper
// functions the layout needs. It is called once from New; a parse failure is a
// programming error (the templates ship in the binary) and panics.
func parseWebTemplates() *template.Template {
	t := template.New("web").Funcs(template.FuncMap{
		"humanizeSince": humanizeSince,
	})
	t = template.Must(t.ParseFS(webTemplateFS, "web/templates/*.html"))
	return t
}

// webSecurityHeaders sets the shell's per-route CSP plus the shared hardening
// headers. It replaces (for the web group only) the /v1 adapter's strict
// `default-src 'none'` CSP with webCSP, without touching the API group's policy
// (SPEC-0001 REQ "Security Headers").
func (s *Server) webSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", webCSP)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		if r.TLS != nil {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

// mountWeb registers the ADR-0011 web shell routes on the given router. The
// caller has already applied webSecurityHeaders + the shared rate limiter to
// the group. `GET /{id}` and `GET /run/{id}` are the only Public artifact
// routes (link-capability read, ADR-0007); `/` renders the on-brand landing.
func (s *Server) mountWeb(r chi.Router) {
	r.Get("/", s.handleLanding)

	// Minimal web session surface (SPEC-0001, ADR-0004): login/logout/whoami and
	// the authenticated Bin. Static paths are matched ahead of the /{id} wildcard
	// by chi, so they never shadow (or are shadowed by) an artifact id.
	r.Get("/login", s.handleLoginForm)
	r.Post("/login", s.handleLogin)
	r.Post("/logout", s.handleLogout)
	// The OIDC relying-party flow (ADR-0013): "Sign in with Pocket ID" on the
	// login page above links here, and requireWebSession/the consent screen
	// redirect straight here (loginRedirectPath) when OIDC is configured. Both
	// routes 503 when OIDC is unconfigured (see oidc.go). "auth" is a reserved
	// id word (internal/id), so no artifact id can shadow these paths.
	r.Get("/auth/login", s.handleOIDCLogin)
	r.Get("/auth/callback", s.handleOIDCCallback)
	r.With(s.requireWebSession).Get("/whoami", s.handleWhoami)
	r.With(s.requireWebSession).Get("/bin", s.handleBinPage)
	// Settings (issue #75): API tokens, MCP connection, CLI, and Account, all
	// behind auth like the Bin — the logged-out landing only teases the
	// capability, it never explains how to exercise it (SPEC-0001 REQ
	// "Settings & Connect Instructions"). The standalone "Connect an agent"
	// page (#57) is retired: /connect is now a permanent redirect into this
	// page's MCP connection section, so any bookmarked/shared link still
	// resolves.
	r.With(s.requireWebSession).Get("/settings", s.handleSettingsPage)
	r.Get("/connect", s.handleConnectRedirect)

	// The OAuth 2.1 authorization endpoint (SPEC-0007, ADR-0004): the human
	// login + consent screen, riding the web session surface above. Its JSON
	// siblings (discovery, DCR, token, revoke) live on the strict-CSP API group
	// (see mountOAuthJSON). "oauth" is a reserved id word, so no artifact id can
	// shadow the path (ADR-0005).
	s.mountOAuthWeb(r)

	r.Get("/{id}", s.handleArtifactShell)
	r.Get("/run/{id}", s.handleRunShell)
	// A sniff-proof body download served on the web surface itself (#12): the
	// generic-file card links here rather than bouncing to the /v1 API, so the
	// download stays same-origin under the web CSP. It is a link-capability read
	// (ADR-0007) like the shell, canonically bare-scheme (a prefixed type such as
	// a trajectory at /run/ is not reachable, matching handleArtifactShell), and
	// streams as `application/octet-stream` + `Content-Disposition: attachment`
	// with X-Content-Type-Options nosniff so untrusted bytes can never be sniffed
	// into an executable type or rendered inline in Cairn's origin (SPEC-0001 REQ
	// "Security Headers"; supersedes upstream #2).
	r.Get("/{id}/download", s.handleWebDownload)
	// The image viewer's own inline body route (SPEC-0003 REQ "Image Viewer",
	// #69): unlike the generic sniff-proof download above, this serves the raw
	// bytes with their real Content-Type and an `inline` disposition, so the
	// `<img src>` the viewer fragment (internal/imageview) renders actually
	// paints on-page instead of triggering a download prompt. Gated by the
	// registry InlineViewer capability — only the image type implements it —
	// so this stays registry-resolved rather than a type-key switch (ADR-0002).
	r.Get("/{id}/image", s.handleWebImage)
	// The bundle viewer's member surfaces (SPEC-0003, #19), both link-capability
	// reads gated exactly like the shell: `/{id}/pane/<name>` returns one member's
	// pane fragment for an HTMX swap (no full reload), and `/{id}/members/<name>`
	// streams that member's body as a sniff-proof same-origin attachment so the
	// generic-file pane's download stays under the web CSP.
	r.Get("/{id}/pane/*", s.handleBundlePane)
	r.Get("/{id}/members/*", s.handleWebMemberDownload)
	// Web annotation post (SPEC-0001, SPEC-0006): the shell's comment composer
	// posts here. It requires a session and passes the CSRF seam — the guard that
	// only bites ambient (cookie) principals — and returns the server-rendered
	// comment partial for an HTMX swap. A bearer caller is exempt from CSRF as on
	// the /v1 surface, so the same route serves scripted posts too.
	r.With(s.requireAuth, s.requireScope(scopeAnnotationsWrite), s.enforceCSRF).Post("/{id}/comments", s.handleWebComment)

	// The embedded, immutable app assets (HTMX, Alpine, the stylesheet, the
	// shell script). Long-cache: the files are content-stable for a build.
	assetSub, _ := fs.Sub(webAssetFS, "web/assets")
	fileSrv := http.StripPrefix("/assets/", http.FileServer(http.FS(assetSub)))
	r.Get("/assets/*", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		fileSrv.ServeHTTP(w, req)
	})
}

// handleLanding renders the ADR-0011 web root split (#57): a caller who
// already carries a valid session or bearer credential sees their Bin — the
// SAME store.ListBin projection /bin and the CLI TUI render — never the empty
// "instance is live" placeholder; a caller with no credential sees the on-brand
// landing (what Cairn is, plus the "Sign in with Pocket ID" CTA). Resolution
// goes through optionalPrincipal (never requireWebSession), so an anonymous
// visitor is served the landing directly rather than bounced through a login
// redirect, and the store is touched only once a principal actually resolves —
// an anonymous request never queries or leaks Bin data (SPEC-0001 REQ "Root
// Split", REQ "Connect Instructions").
func (s *Server) handleLanding(w http.ResponseWriter, r *http.Request) {
	if p, ok := s.optionalPrincipal(r); ok {
		s.renderBin(w, r, p)
		return
	}
	s.renderWeb(w, r, "landing", newLandingView(s))
}

// landingView is the logged-out landing's view model. LoginPath is where the
// primary CTA points: OIDC's passwordless /auth/login when configured (ADR-0013
// "no second login"), else the dev-only /login form fallback — the same
// resolution requireWebSession uses (loginRedirectPath), so the landing CTA and
// every other auth gate on the site always agree on where "sign in" goes.
type landingView struct {
	LoginPath   string
	OIDCEnabled bool
}

func newLandingView(s *Server) landingView {
	return landingView{
		LoginPath:   s.loginRedirectPath(),
		OIDCEnabled: s.oidc != nil,
	}
}

// handleArtifactShell renders the app shell for a bare-scheme artifact at
// `GET /{id}` (ADR-0005). It is the canonical path only for types with no URL
// sub-prefix; a type whose registry prefix is non-empty (a trajectory lives at
// /run/{id}) is not reachable here and yields the same uniform 404 as an
// unknown id, so the id space stays canonical and leak-free (ADR-0007).
func (s *Server) handleArtifactShell(w http.ResponseWriter, r *http.Request) {
	s.renderShellFor(w, r, "")
}

// handleRunShell (the trajectory viewer at `GET /run/{id}`) lives in
// trajectory_view.go: unlike a bodied share type it renders from the trajectory
// service's reconstructed span tree, not a single streamed body.

// handleWebDownload streams an artifact's single content-addressed body as a
// safe attachment on the web surface (#12). It enforces the same canonical
// bare-scheme discipline as the shell: the id must resolve, its registry web
// prefix must be empty (a prefixed type such as a trajectory at /run/ is not
// reachable here), and it must have a body — otherwise the request yields the
// same uniform 404 as an unknown id, so the id space stays canonical and leaks
// no signal (ADR-0007). serveBody sets `application/octet-stream` +
// `Content-Disposition: attachment` and the web group has already set nosniff,
// so a browser can only download — never render or sniff — the bytes.
func (s *Server) handleWebDownload(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	a, err := s.store.GetByPublicID(r.Context(), id)
	if err != nil {
		s.renderWebError(w, r, err)
		return
	}
	if s.reg.URLPrefixFor(a.ShareType).Web != "" || a.BodySHA256 == "" {
		s.renderWebError(w, r, errs.ErrNotFound)
		return
	}
	rc, info, err := s.store.OpenBody(r.Context(), id)
	if err != nil {
		s.renderWebError(w, r, err)
		return
	}
	defer rc.Close()
	s.serveBody(w, r, rc, info, firstNonEmpty(a.Title, a.PublicID))
}

// handleWebImage streams an artifact's body inline (real Content-Type,
// `Content-Disposition: inline`) for the image viewer's own `<img src>`
// (SPEC-0003 REQ "Image Viewer", #69). It holds the same canonical
// bare-scheme + bodied discipline as handleWebDownload, plus the registry
// InlineViewer gate: an artifact whose type does not declare inline serving
// (every type except image) yields the same uniform 404 as an unknown id, so
// this route can never be used to bypass the sniff-proof download for an
// arbitrary body (ADR-0007).
func (s *Server) handleWebImage(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	a, err := s.store.GetByPublicID(r.Context(), id)
	if err != nil {
		s.renderWebError(w, r, err)
		return
	}
	if s.reg.URLPrefixFor(a.ShareType).Web != "" || a.BodySHA256 == "" || !s.reg.InlineBodyFor(a) {
		s.renderWebError(w, r, errs.ErrNotFound)
		return
	}
	rc, info, err := s.store.OpenBody(r.Context(), id)
	if err != nil {
		s.renderWebError(w, r, err)
		return
	}
	defer rc.Close()
	s.serveInlineBody(w, r, rc, info, a.MediaType, firstNonEmpty(a.Title, a.PublicID))
}

// renderShellFor resolves the id, enforces that the route's URL sub-prefix
// matches the artifact type's registry prefix (canonical-URL discipline, driven
// by registry data — never a switch on the type), builds the view model, and
// renders the shell. Unknown, expired, and wrong-prefix ids all render the same
// uniform 404 (ADR-0007, SPEC-0001 REQ "Uniform 404 for unknown id").
func (s *Server) renderShellFor(w http.ResponseWriter, r *http.Request, wantPrefix string) {
	id := chi.URLParam(r, "id")
	a, err := s.store.GetByPublicID(r.Context(), id)
	if err != nil {
		s.renderWebError(w, r, err)
		return
	}
	if s.reg.URLPrefixFor(a.ShareType).Web != wantPrefix {
		// The id resolves, but not at this path: treat as not-found so the two
		// routes each own exactly their canonical prefix and leak no signal.
		s.renderWebError(w, r, errs.ErrNotFound)
		return
	}
	// A bundle browses its members through the file rail; `?file=` selects the
	// active member for the no-JS full-navigation path (an HTMX pane swap uses the
	// dedicated /{id}/pane route). Non-bundle types ignore it. `?lang=` is the
	// code viewer's language override (SPEC-0003 "overridable"); every other
	// type ignores it too — it rides the generic sharetype.WithBodyHint seam so
	// BodyViewer's signature stays the same for every type (ADR-0002).
	ctx := sharetype.WithBodyHint(r.Context(), r.URL.Query().Get("lang"))
	vm := s.buildShellView(ctx, a, r.URL.Query().Get("file"))
	// A logged-in viewer sees the comment composer; an anonymous link reader sees
	// the read-only thread (posting requires a session + CSRF). Resolution is
	// optional — an absent/invalid credential simply yields the read-only view.
	if p, ok := s.optionalPrincipal(r); ok {
		vm.Authenticated = true
		vm.Actor = p.ActorID
		vm.ShareDialog.IsOwner = p.ActorID == a.Access.OwnerID
	}
	s.renderWeb(w, r, "shell", vm)
}

// optionalPrincipal resolves the caller's principal when the request carries
// valid credentials (a session cookie or a bearer token), or reports false for
// an anonymous link read. Like optionalActor it never rejects: the shell is a
// link-capability read, so an unauthenticated viewer still sees the artifact.
func (s *Server) optionalPrincipal(r *http.Request) (*Principal, bool) {
	if s.auth == nil {
		return nil, false
	}
	p, err := s.auth.Authenticate(r)
	if err != nil || p == nil {
		return nil, false
	}
	return p, true
}

// shellView is the fully-resolved view model the shell template renders. Every
// field is server-computed from the one core (artifact + registry + annotation
// service), so the page needs no client data layer (ADR-0011).
type shellView struct {
	ID            string
	Badge         string
	Title         string
	TypeLabel     string // the share-type key, shown as the breadcrumb crumb
	WebURL        string
	MCPHandle     string
	MetaLine      string // e.g. "1.2 kB · text/markdown"
	Body          template.HTML
	HasRichBody   bool          // a registered BodyViewer produced the body
	Bundle        *bundleView   // the composed member rail + active pane (bundle)
	Hook          *hookView     // the live captured-request inspector (webhook, issue #86)
	FileCard      *fileCardView // the generic-file floor when no viewer resolved
	Provenance    provenanceLine
	PanelFields   []sharetype.PanelField // type-specific (registry MetadataPanel)
	Details       []sharetype.PanelField // shell-owned artifact facts
	Comments      []commentLine
	ReactionCount int
	CommentCount  int
	PinCount      int
	// CommentsSupported gates the whole Comments panel section (heading, thread,
	// composer): registry data (AllowsAnchor against the whole-artifact anchor),
	// never a type-key switch, so a type declaring no comment anchors at all —
	// webhook, SPEC-0006 REQ "Webhook Reaction-Only Asymmetry" — renders no
	// comment affordance whatsoever rather than an empty thread inviting a post
	// the write-time gate would only then refuse.
	CommentsSupported bool
	// Authenticated reports whether the viewer holds a session (or token), which
	// gates the comment composer; Actor is that viewer's id. An anonymous link
	// read leaves both zero and sees the read-only thread.
	Authenticated bool
	Actor         string
	// ShareDialog is the Share button's view model (#46).
	ShareDialog shareDialogView
}

// shareDialogView is the Share dialog's view model (#46, SPEC-0001 REQ "Share
// Affordance"; design turn 7 share sheet). It is a read-only summary sourced
// from the same server-computed facts as the header and panel: the shareable
// web link and mcp:// handle (duplicated from the header URL control so the
// copy affordance works without closing the dialog), the current link access
// policy line and TTL/expiry countdown (ADR-0007 "🔒 you + anyone with link" /
// "⧗ expires 7d"), and the provenance line. Adjusting the policy itself (rotate
// id, change TTL/visibility via POST /v1/artifacts/{id}/share) is a later
// story; this dialog never re-implements or mutates access rules client-side —
// it only shows a non-owner why the controls are absent.
type shareDialogView struct {
	// ID is the artifact's current public id — the owner controls below (issue
	// #94) address their PATCH/POST requests at /v1/artifacts/{ID}/..., so the
	// dialog carries it as a data attribute for share.js to read.
	ID          string
	WebURL      string
	MCPHandle   string
	Visibility  artifact.Visibility // raw value, drives the owner visibility <select>
	AccessLabel string              // "🔒 you + anyone with link" | "🔒 you only" (ADR-0007)
	ExpiresIn   string              // humanized ("expires in 6d"), empty when no expiry
	Provenance  provenanceLine
	// IsOwner reports whether the resolved viewer is the artifact's owner. It is
	// set by the caller once the request's principal is known (buildShellView /
	// buildTrajectoryView run before authentication is resolved), and gates the
	// dialog's owner-only note (SPEC-0001 REQ "Share Affordance": "A non-owner
	// or unauthenticated viewer MUST NOT be able to change sharing") as well as
	// the owner-only TTL/visibility/rotate controls (issue #94, SPEC-0009 REQ
	// "Owner-Only Policy Changes"). A non-owner or signed-out viewer sees the
	// countdown/access line read-only, with no mutating affordance at all.
	IsOwner bool
}

// shareAccessLabel renders the ADR-0007 access-policy line for the Share
// dialog from the artifact's stored visibility.
func shareAccessLabel(v artifact.Visibility) string {
	if v == artifact.VisibilityPrivate {
		return "🔒 you only"
	}
	return "🔒 you + anyone with link"
}

// fileCardView is the generic-file viewer — the total-resolution floor every
// share type falls back to when no rich BodyViewer is registered (#12). It is a
// server-rendered card carrying the type glyph/badge, the title, the size, the
// media type, and the sha256, plus a sniff-proof `Content-Disposition:
// attachment` download for a bodied artifact (a bodyless type such as a
// trajectory or bundle leaves HasDownload false). The floor guarantees the
// SPEC-0001 REQ "Type-Specific Body Slot" total-resolution property: EVERY
// artifact — including an unknown/unregistered share type — resolves to some
// viewer and stays viewable, checksummed, and downloadable.
type fileCardView struct {
	Badge       string
	Title       string
	Size        string // humanized, empty for a bodyless type
	MediaType   string
	SHA256      string // short display form
	SHA256Full  string // full digest, shown as the title attribute
	HasDownload bool
	DownloadURL string
}

type provenanceLine struct {
	Actor      string
	OnBehalfOf string
	// Model is the model that produced the artifact, empty when the creator
	// reported none — the template omits the row rather than showing it blank.
	Model    string
	Channel  string
	Captured string // humanized
	Expires  string // humanized ("in 6d")
}

type commentLine struct {
	// ID is the comment's own row id: the shell renders it as `id="comment-N"`
	// on the card so the image viewer's pin markers can jump/scroll straight to
	// their thread (SPEC-0003 REQ "Image Viewer": "clicking a marker opens/
	// scrolls to its comment thread", #69).
	ID         int64
	Actor      string
	OnBehalfOf string
	Body       string
	When       string
	IsReply    bool
	Deleted    bool
	// AnchorContext is a short human cue for a non-whole-artifact anchor — e.g.
	// `on span s2` or `on "…quote…"` — shown as a purple prefix on the comment
	// card (design t7a "anchor context"). Empty for a whole-artifact comment.
	AnchorContext string
	// HasPin/PinX/PinY carry an image_region comment's normalized fractional
	// {x,y} (ADR-0006) as the comment-item partial's `data-pin-x`/`data-pin-y`
	// attributes — image.js reads them straight off the already-rendered panel
	// thread to place (and, on a fresh HTMX append, immediately place) the pin
	// marker over the image with no extra round-trip (SPEC-0003 REQ "Image
	// Annotation Anchors", #69).
	HasPin bool
	PinX   string
	PinY   string
}

// buildShellView projects an artifact into the shell view model: the header
// facts (badge/title/URL/MCP from the registry + ADR-0005 scheme), the body
// (the registry BodyViewer capability when present, else the generic-file
// card), the type-specific panel fields (registry MetadataPanel capability),
// the shell-owned detail facts, provenance, and the comment thread.
func (s *Server) buildShellView(ctx context.Context, a *artifact.Artifact, activeFile string) shellView {
	vm := shellView{
		ID:            a.PublicID,
		Badge:         s.reg.BadgeFor(a),
		Title:         firstNonEmpty(a.Title, a.PublicID),
		TypeLabel:     string(a.ShareType),
		WebURL:        s.webURL(a),
		MCPHandle:     s.mcpHandle(a),
		MetaLine:      metaLine(a),
		PanelFields:   s.reg.MetadataPanelFor(a),
		Details:       detailFields(a),
		ReactionCount: a.ReactionCount,
		CommentCount:  a.CommentCount,
		PinCount:      a.PinCount,
		// Registry data, not a type-key switch: a type declaring no comment
		// anchors at all (webhook) hides the whole Comments section rather than
		// showing an always-empty thread inviting a post the write-time gate
		// (annotation.Validate) would only then refuse (SPEC-0006 REQ "Webhook
		// Reaction-Only Asymmetry").
		CommentsSupported: s.reg.AllowsAnchor(a.ShareType, sharetype.AnchorArtifact, sharetype.KindComment),
		Provenance: provenanceLine{
			Actor:      a.Provenance.ActorID,
			OnBehalfOf: a.Provenance.OnBehalfOf,
			Model:      a.Provenance.Model,
			Channel:    string(a.Provenance.Channel),
			Captured:   humanizeSince(a.Provenance.CapturedAt),
			Expires:    humanizeUntil(a.ExpiresAt),
		},
	}
	vm.ShareDialog = shareDialogView{
		ID:          vm.ID,
		WebURL:      vm.WebURL,
		MCPHandle:   vm.MCPHandle,
		Visibility:  a.Access.Visibility,
		AccessLabel: shareAccessLabel(a.Access.Visibility),
		ExpiresIn:   vm.Provenance.Expires,
		Provenance:  vm.Provenance,
	}

	// Comments are read under the same link capability as the artifact; a nil
	// annotation service (unit paths without a store) yields an empty thread. The
	// raw thread is loaded once and reused for the panel rows AND (for a bundle)
	// the per-file engagement counts, so both project one query.
	var rawComments []annotation.Comment
	if s.annot != nil {
		if comments, err := s.annot.ListComments(ctx, a.PublicID); err == nil {
			rawComments = comments
			vm.Comments = toCommentLines(comments)
		} else {
			s.log.WarnContext(ctx, "web: list comments failed", "id", a.PublicID, "error", err)
		}
	}

	// Body slot — registry-driven viewer resolution with the generic-file card as
	// the total-resolution floor (SPEC-0001 REQ "Type-Specific Body Slot",
	// ADR-0002). Resolution order is entirely registry data, never a switch on the
	// type key:
	//   1. a ComposedViewer type (a bundle) renders its member file rail + the
	//      selected member's pane, each member delegated back through the registry
	//      to its own viewer (SPEC-0003 REQ "Bundle Viewer");
	//   2. a StreamViewer type (webhook) renders its live captured-request
	//      inspector by delegating to the webhook service, exactly as the bundle
	//      path delegates to the store (SPEC-0005 REQ "Inspector Viewer", #86);
	//   3. a BodyViewer type renders its own (already-escaped) fragment from the
	//      streamed body (markdown, #39);
	//   4. every other type — and any viewer that errors — resolves to the
	//      generic-file card, so EVERY share type resolves to some viewer (#12).
	if _, ok := s.reg.ComposedViewerFor(a.ShareType); ok && s.store != nil {
		if bv := s.buildBundleView(ctx, a, activeFile, rawComments); bv != nil {
			vm.Bundle = bv
		}
	}
	if vm.Bundle == nil {
		if _, ok := s.reg.StreamViewerFor(a.ShareType); ok && s.hook != nil {
			if hv := s.buildHookView(ctx, a); hv != nil {
				vm.Hook = hv
			}
		}
	}
	if vm.Bundle == nil && vm.Hook == nil {
		if viewer, ok := s.reg.BodyViewerFor(a.ShareType); ok && a.BodySHA256 != "" && s.store != nil {
			if rc, _, err := s.store.OpenBody(ctx, a.PublicID); err == nil {
				defer rc.Close()
				if html, err := viewer.RenderBody(ctx, a, rc); err == nil {
					vm.Body = html
					vm.HasRichBody = true
				} else {
					s.log.WarnContext(ctx, "web: body viewer failed, falling back to generic file card", "id", a.PublicID, "error", err)
				}
			}
		}
	}
	if !vm.HasRichBody && vm.Bundle == nil && vm.Hook == nil {
		vm.FileCard = fileCard(a, vm.Badge)
	}
	return vm
}

// toCommentLines flattens the thread into display rows, marking replies (which
// the template indents) and preserving soft-delete tombstones as structure.
func toCommentLines(cs []annotation.Comment) []commentLine {
	out := make([]commentLine, 0, len(cs))
	for _, c := range cs {
		line := commentLine{
			ID:            c.ID,
			Actor:         c.ActorID,
			OnBehalfOf:    c.OnBehalfOf,
			Body:          c.Body,
			When:          humanizeSince(c.CreatedAt),
			IsReply:       c.ParentID != nil,
			Deleted:       c.Deleted,
			AnchorContext: anchorContext(c.Anchor.Type, c.Anchor.Ref),
		}
		if x, y, ok := imagePinFromRef(c.Anchor.Type, c.Anchor.Ref); ok {
			line.HasPin, line.PinX, line.PinY = true, x, y
		}
		out = append(out, line)
	}
	return out
}

// imagePinFromRef extracts an image_region comment's normalized {x,y}
// (ADR-0006) for the comment-item partial's `data-pin-x`/`data-pin-y`
// attributes (SPEC-0003 REQ "Image Annotation Anchors", #69). Any other
// anchor type, or a malformed ref, yields ok=false — the locator schema
// already guarantees a well-formed image_region ref by the time a comment
// commits (sharetype.imageRegionLocator), so this only defends against a
// future anchor shape drifting out from under it.
func imagePinFromRef(anchorType sharetype.Anchor, ref json.RawMessage) (x, y string, ok bool) {
	if anchorType != sharetype.AnchorImageRegion {
		return "", "", false
	}
	var loc struct {
		X *float64 `json:"x"`
		Y *float64 `json:"y"`
	}
	if json.Unmarshal(ref, &loc) != nil || loc.X == nil || loc.Y == nil {
		return "", "", false
	}
	return strconv.FormatFloat(*loc.X, 'f', -1, 64), strconv.FormatFloat(*loc.Y, 'f', -1, 64), true
}

// anchorContext renders a short human cue for a comment's anchor, shown as the
// purple context prefix on the card (design t7a). The whole-artifact anchor has
// no cue; a trajectory_span names its span; a text_selection quotes up to 34
// characters of the selected text (SPEC-0006 text_selection self-describing
// quote). Unknown shapes degrade to naming the anchor type rather than erroring.
func anchorContext(anchorType sharetype.Anchor, ref json.RawMessage) string {
	switch anchorType {
	case sharetype.AnchorArtifact, "":
		return ""
	case sharetype.AnchorTrajectorySpan, sharetype.AnchorTrajectoryTurn, sharetype.AnchorTrajectoryToolCall:
		var loc struct {
			SpanID string `json:"span_id"`
		}
		if json.Unmarshal(ref, &loc) == nil && loc.SpanID != "" {
			return "on span " + loc.SpanID
		}
	case sharetype.AnchorTextSelection:
		var loc struct {
			Quote string `json:"quote"`
		}
		if json.Unmarshal(ref, &loc) == nil && loc.Quote != "" {
			return "on “" + truncateRunes(loc.Quote, 34) + "”"
		}
	case sharetype.AnchorCodeLine:
		// The code viewer's per-line comment (SPEC-0003 REQ "Code Annotation
		// Anchors") — "on line 42" resolves to the same #L42 the viewer's own
		// line anchors use (internal/code/viewer.go's lineID).
		var loc struct {
			Line int `json:"line"`
		}
		if json.Unmarshal(ref, &loc) == nil && loc.Line > 0 {
			return "on line " + itoa(loc.Line)
		}
	case sharetype.AnchorImageRegion:
		if x, y, ok := imagePinFromRef(anchorType, ref); ok {
			fx, _ := strconv.ParseFloat(x, 64)
			fy, _ := strconv.ParseFloat(y, 64)
			return "on a pin at " + itoa(int(fx*100)) + "%, " + itoa(int(fy*100)) + "%"
		}
		return "on a pinned region"
	case sharetype.AnchorBundleFile:
		// A per-file bundle comment is labelled by its member name — the "· on
		// notes.md" context the aggregated COMMENTS panel shows (issue #19).
		var loc struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(ref, &loc) == nil && loc.Name != "" {
			return "on " + loc.Name
		}
	}
	return "on " + string(anchorType)
}

// truncateRunes clamps s to at most n runes, appending an ellipsis when it cut.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// fileCard builds the generic-file floor view model for an artifact whose type
// resolved to no rich viewer (#12). The badge is passed in already-resolved
// through the registry (BadgeFor), so this stays a pure projection. A bodied
// artifact gets its size, sha256, and a same-origin `/{id}/download` link (the
// sniff-proof web download route); a bodyless type gets a card with no download.
func fileCard(a *artifact.Artifact, badge string) *fileCardView {
	fc := &fileCardView{
		Badge:     badge,
		Title:     firstNonEmpty(a.Title, a.PublicID),
		MediaType: a.MediaType,
	}
	if a.BodySHA256 != "" {
		fc.HasDownload = true
		fc.Size = humanizeBytes(a.Size)
		fc.SHA256 = shortSHA(a.BodySHA256)
		fc.SHA256Full = a.BodySHA256
		fc.DownloadURL = "/" + a.PublicID + "/download"
	}
	return fc
}

// detailFields are the shell-owned artifact facts shown under provenance for a
// bodied artifact (size, media type, checksum). Type-specific facts are the
// registry's MetadataPanel; these are the core aggregate's own attributes.
func detailFields(a *artifact.Artifact) []sharetype.PanelField {
	if a.BodySHA256 == "" {
		return nil
	}
	fields := []sharetype.PanelField{{Label: "size", Value: humanizeBytes(a.Size)}}
	if a.MediaType != "" {
		fields = append(fields, sharetype.PanelField{Label: "type", Value: a.MediaType})
	}
	fields = append(fields, sharetype.PanelField{Label: "sha256", Value: shortSHA(a.BodySHA256)})
	return fields
}

// metaLine is the compact header meta ("1.2 kB · text/markdown"). For bodyless
// types (trajectory, bundle) it names the type instead.
func metaLine(a *artifact.Artifact) string {
	if a.BodySHA256 == "" {
		return string(a.ShareType)
	}
	if a.MediaType != "" {
		return humanizeBytes(a.Size) + " · " + a.MediaType
	}
	return humanizeBytes(a.Size)
}

// renderWeb executes a shell template into a buffered response so a mid-render
// error cannot emit a half-written 200. On success it writes text/html.
func (s *Server) renderWeb(w http.ResponseWriter, r *http.Request, name string, data any) {
	var buf strings.Builder
	if err := s.webTmpl.ExecuteTemplate(&buf, name, data); err != nil {
		s.log.ErrorContext(r.Context(), "web: template execute failed", "template", name, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(buf.String()))
}

// renderWebError renders a minimal HTML error page for the web surface, mapping
// the domain error to an aligned status. Not-found (unknown/expired/wrong-path
// ids) renders a uniform page so probing leaks no signal (ADR-0007).
func (s *Server) renderWebError(w http.ResponseWriter, r *http.Request, err error) {
	status := statusFor(errs.CodeOf(err))
	var buf strings.Builder
	if terr := s.webTmpl.ExecuteTemplate(&buf, "error", errPageView{
		Status:  status,
		Message: messageFor(errs.CodeOf(err)),
	}); terr != nil {
		http.Error(w, messageFor(errs.CodeOf(err)), status)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(buf.String()))
}

type errPageView struct {
	Status  int
	Message string
}

// --- humanize helpers ------------------------------------------------------

// humanizeSince renders a past time as a compact relative age ("just now",
// "2h ago", "3d ago"), matching the design's provenance line. The " ago" suffix
// belongs to this helper — templates render the value verbatim — and the
// sub-minute bucket becomes "just now", which already reads as past tense and
// so is returned bare rather than as the ungrammatical "just now ago".
func humanizeSince(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	rel := humanizeDuration(time.Since(t))
	if rel == subMinute {
		return "just now"
	}
	return rel + " ago"
}

// humanizeUntil renders a future time as "in Nd"; a past/zero time renders
// "expired".
func humanizeUntil(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Until(t)
	if d <= 0 {
		return "expired"
	}
	return "in " + humanizeDuration(d)
}

// secondsUntil renders the raw seconds remaining until t for the machine-
// readable half of the TTL countdown (SPEC-0009 REQ "...Visible Countdown"),
// clamped to zero — never negative — once expiry has passed, matching
// humanizeUntil's "expired" floor.
func secondsUntil(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	d := time.Until(t)
	if d < 0 {
		return 0
	}
	return int64(d.Seconds())
}

// subMinute is the bucket humanizeDuration returns below one minute. It is a
// magnitude, not a phrase, so both callers can frame it in their own tense —
// humanizeSince swaps it for "just now", humanizeUntil renders "in <1m".
const subMinute = "<1m"

// humanizeDuration renders a duration as a compact magnitude and nothing else:
// no tense, no framing. The words around it belong to the caller (humanizeSince
// appends " ago", humanizeUntil prepends "in "), which is why the sub-minute
// bucket is "<1m" rather than a past-tense phrase that only ever fit one of
// them and left the other rendering "in just now".
func humanizeDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return subMinute
	case d < time.Hour:
		return itoa(int(d.Minutes())) + "m"
	case d < 24*time.Hour:
		return itoa(int(d.Hours())) + "h"
	default:
		return itoa(int(d.Hours()/24)) + "d"
	}
}

// humanizeBytes renders a byte count as a compact SI-ish size.
func humanizeBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return itoa(int(n)) + " B"
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	val := float64(n) / float64(div)
	suffix := []string{"kB", "MB", "GB", "TB"}[exp]
	// One decimal place without importing fmt for a hot helper.
	whole := int(val)
	frac := int((val - float64(whole)) * 10)
	if frac == 0 {
		return itoa(whole) + " " + suffix
	}
	return itoa(whole) + "." + itoa(frac) + " " + suffix
}

func shortSHA(sha string) string {
	if len(sha) <= 12 {
		return sha
	}
	return sha[:12]
}

// itoa is a tiny non-negative integer formatter (avoids importing strconv/fmt
// in these hot display helpers; negative inputs are clamped to 0).
func itoa(n int) string {
	if n <= 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
