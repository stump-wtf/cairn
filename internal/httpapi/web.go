package httpapi

import (
	"context"
	"embed"
	"html/template"
	"io/fs"
	"net/http"
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
	r.Get("/{id}", s.handleArtifactShell)
	r.Get("/run/{id}", s.handleRunShell)
	// The embedded, immutable app assets (HTMX, Alpine, the stylesheet, the
	// shell script). Long-cache: the files are content-stable for a build.
	assetSub, _ := fs.Sub(webAssetFS, "web/assets")
	fileSrv := http.StripPrefix("/assets/", http.FileServer(http.FS(assetSub)))
	r.Get("/assets/*", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		fileSrv.ServeHTTP(w, req)
	})
}

// handleLanding renders the on-brand landing at the web root. It uses the shell
// tokens (dark #0A0B0D, the stone logo) rather than chi's bare 404, confirming
// the instance is live; the Bin (the authenticated workspace listing) arrives
// with web-session auth (#12).
func (s *Server) handleLanding(w http.ResponseWriter, r *http.Request) {
	s.renderWeb(w, r, "landing", nil)
}

// handleArtifactShell renders the app shell for a bare-scheme artifact at
// `GET /{id}` (ADR-0005). It is the canonical path only for types with no URL
// sub-prefix; a type whose registry prefix is non-empty (a trajectory lives at
// /run/{id}) is not reachable here and yields the same uniform 404 as an
// unknown id, so the id space stays canonical and leak-free (ADR-0007).
func (s *Server) handleArtifactShell(w http.ResponseWriter, r *http.Request) {
	s.renderShellFor(w, r, "")
}

// handleRunShell renders the app shell for a trajectory at `GET /run/{id}`,
// whose registry web prefix is "run" (SPEC-0002 URL scheme). Any id whose type
// does not carry that prefix yields a uniform 404.
func (s *Server) handleRunShell(w http.ResponseWriter, r *http.Request) {
	s.renderShellFor(w, r, "run")
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
	vm := s.buildShellView(r.Context(), a)
	s.renderWeb(w, r, "shell", vm)
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
	HasRichBody   bool // a registered BodyViewer produced the body
	HasDownload   bool // a single content-addressed body is downloadable
	DownloadURL   string
	Provenance    provenanceLine
	PanelFields   []sharetype.PanelField // type-specific (registry MetadataPanel)
	Details       []sharetype.PanelField // shell-owned artifact facts
	Comments      []commentLine
	ReactionCount int
	CommentCount  int
	PinCount      int
}

type provenanceLine struct {
	Actor      string
	OnBehalfOf string
	Channel    string
	Captured   string // humanized
	Expires    string // humanized ("in 6d")
}

type commentLine struct {
	Actor      string
	OnBehalfOf string
	Body       string
	When       string
	IsReply    bool
	Deleted    bool
}

// buildShellView projects an artifact into the shell view model: the header
// facts (badge/title/URL/MCP from the registry + ADR-0005 scheme), the body
// (the registry BodyViewer capability when present, else the generic-file
// card), the type-specific panel fields (registry MetadataPanel capability),
// the shell-owned detail facts, provenance, and the comment thread.
func (s *Server) buildShellView(ctx context.Context, a *artifact.Artifact) shellView {
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
		Provenance: provenanceLine{
			Actor:      a.Provenance.ActorID,
			OnBehalfOf: a.Provenance.OnBehalfOf,
			Channel:    string(a.Provenance.Channel),
			Captured:   humanizeSince(a.Provenance.CapturedAt),
			Expires:    humanizeUntil(a.ExpiresAt),
		},
	}

	// Body slot. A type that implements the ADR-0002 BodyViewer capability
	// renders its own (already-escaped) fragment from the streamed body; every
	// other type falls back to the generic-file card the shell template draws
	// (SPEC-0001 REQ "Type-Specific Body Slot" — total resolution).
	if a.BodySHA256 != "" {
		vm.HasDownload = true
		vm.DownloadURL = s.cfg.BaseURL + "/v1/artifacts/" + a.PublicID + "/body"
		if viewer, ok := s.reg.BodyViewerFor(a.ShareType); ok {
			if rc, _, err := s.store.OpenBody(ctx, a.PublicID); err == nil {
				defer rc.Close()
				if html, err := viewer.RenderBody(ctx, a, rc); err == nil {
					vm.Body = html
					vm.HasRichBody = true
				} else {
					s.log.WarnContext(ctx, "web: body viewer failed, falling back to generic card", "id", a.PublicID, "error", err)
				}
			}
		}
	}

	// Comments are read under the same link capability as the artifact; a nil
	// annotation service (unit paths without a store) yields an empty thread.
	if s.annot != nil {
		if comments, err := s.annot.ListComments(ctx, a.PublicID); err == nil {
			vm.Comments = toCommentLines(comments)
		} else {
			s.log.WarnContext(ctx, "web: list comments failed", "id", a.PublicID, "error", err)
		}
	}
	return vm
}

// toCommentLines flattens the thread into display rows, marking replies (which
// the template indents) and preserving soft-delete tombstones as structure.
func toCommentLines(cs []annotation.Comment) []commentLine {
	out := make([]commentLine, 0, len(cs))
	for _, c := range cs {
		out = append(out, commentLine{
			Actor:      c.ActorID,
			OnBehalfOf: c.OnBehalfOf,
			Body:       c.Body,
			When:       humanizeSince(c.CreatedAt),
			IsReply:    c.ParentID != nil,
			Deleted:    c.Deleted,
		})
	}
	return out
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
// "2h ago", "3d ago"), matching the design's provenance line.
func humanizeSince(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	return humanizeDuration(d) + " ago"
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

func humanizeDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
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
