package sharetype

import (
	"context"
	"html/template"
	"io"
	"mime"
	"path"
	"strconv"
	"strings"

	"github.com/joestump/cairn/internal/artifact"
	"github.com/joestump/cairn/internal/code"
	"github.com/joestump/cairn/internal/imageview"
	"github.com/joestump/cairn/internal/markdown"
)

// Registry keys for the built-in types beyond the generic file/gz/bundle keys
// declared on the aggregate. Their viewers and capture behavior are delivered by
// later stories (markdown/code/image = SPEC-0003; webhook = SPEC-0004;
// trajectory = SPEC-0005); this registry owns their identity and the anchor +
// previewability affordances that must not drift between viewer and validator.
const (
	KeyMarkdown artifact.ShareType = "markdown"
	KeyCode     artifact.ShareType = "code"
	KeyImage    artifact.ShareType = "image"
	KeyWebhook  artifact.ShareType = "webhook"
	// KeyTrajectory is sourced from the artifact aggregate, which knows the
	// trajectory type is bodyless (like a bundle) so it needs no body blob.
	KeyTrajectory artifact.ShareType = artifact.TypeTrajectory
)

// simpleType is the common ShareType value: an identity, a badge, a
// previewability predicate over media type, and a set of legal anchors —
// including the whole-artifact `artifact` anchor, which is registry data like
// any other (webhook deliberately declares it reaction-only). Capabilities
// beyond the base contract are added by embedding simpleType in a wrapper that
// implements the optional interface (see prefixedType, codeShareType).
type simpleType struct {
	key     artifact.ShareType
	badge   string
	preview func(mediaType string) bool // nil => never previewable
	anchors []AnchorSpec
}

func (t simpleType) Key() artifact.ShareType { return t.key }
func (t simpleType) Badge() string           { return t.badge }
func (t simpleType) Anchors() []AnchorSpec   { return t.anchors }

func (t simpleType) PreviewableMedia(mediaType string) bool {
	return t.preview != nil && t.preview(mediaType)
}

// prefixedType composes simpleType with the URLPrefixer capability for types
// whose short URL / MCP handle carry a legible sub-prefix (ADR-0005).
type prefixedType struct {
	simpleType
	prefix URLPrefix
}

func (t prefixedType) URLPrefix() URLPrefix { return t.prefix }

// markdownShareType composes simpleType with the BodyViewer capability: it
// renders a markdown body as the sanitized, server-side HTML fragment the app
// shell drops into its body slot — the rendered prose, a multi-level CONTENTS
// table of contents, deterministic per-block ids, and the md_block/text_selection
// annotation affordances (SPEC-0003 REQ "Markdown Viewer", REQ "Markdown
// Annotation Anchors"). The viewer stays a registry capability discovered by
// type assertion (ADR-0002): the shell resolves it via BodyViewerFor and never
// switches on the markdown key. Rendering + sanitization live in
// internal/markdown so this wrapper is a thin adapter over the streamed body.
type markdownShareType struct {
	simpleType
}

// RenderBody implements the BodyViewer capability. It streams the whole
// (content-addressed, immutable) body into the markdown renderer, whose output
// is already goldmark-omitted-raw-HTML + bluemonday-sanitized, so the returned
// fragment cannot execute active content in Cairn's origin (SPEC-0003 Security
// REQ "Untrusted markdown body").
func (markdownShareType) RenderBody(_ context.Context, _ *artifact.Artifact, body io.Reader) (template.HTML, error) {
	src, err := io.ReadAll(body)
	if err != nil {
		return "", err
	}
	return markdown.RenderFragment(src)
}

// bundleShareType composes simpleType with the ComposedViewer capability: a
// bundle has no single body but an ordered set of member files (ADR-0008), so
// the shell renders its file rail and delegates each selected member back
// through the registry to that member's own viewer (SPEC-0003 REQ "Bundle
// Viewer"). MemberAnchor names the anchor a whole-member annotation attaches to
// (bundle_file); member-internal anchors (a markdown block, a text selection)
// resolve through the member's own type. The capability stays registry data
// discovered by type assertion (ADR-0002): httpapi resolves the bundle path via
// ComposedViewerFor and never switches on the bundle key.
type bundleShareType struct {
	simpleType
}

func (bundleShareType) MemberAnchor() Anchor { return AnchorBundleFile }

// imageShareType composes simpleType with three capabilities (SPEC-0003 REQ
// "Image Viewer", issue #69): BodyViewer (the image + pin-overlay +
// react-below scaffolding, rendered by internal/imageview — mirroring
// markdownShareType's split of rendering from registry glue), MetadataPaneler
// (the pin and reaction counts, sourced straight off the artifact's own
// rollup counters so they render with no JavaScript or annotation-service
// call), and InlineViewer (httpapi's own non-attachment body route, since
// every other type's body streams through the sniff-proof generic download —
// ADR-0002 "no switch on type" holds because only image implements it, so
// httpapi resolves the route through the registry same as everything else).
type imageShareType struct {
	simpleType
}

// RenderBody implements the BodyViewer capability. The image bytes are never
// read here — the fragment links back to the artifact's own inline body route
// rather than inlining them into the page — so body is accepted only to
// satisfy the shared BodyViewer signature (ADR-0002).
func (imageShareType) RenderBody(_ context.Context, a *artifact.Artifact, _ io.Reader) (template.HTML, error) {
	return imageview.Fragment(a)
}

// MetadataPanel implements the MetadataPaneler capability: the pin count
// (image-region reactions plus pinned comments, ADR-0006) and the total
// reaction count, both read straight off the artifact's denormalized rollups
// — no annotation-service call needed — so the panel (and, per SPEC-0003
// Scenario "Pin overlay unavailable", the reaction total specifically) is
// visible even when image.js never loads.
func (imageShareType) MetadataPanel(a *artifact.Artifact) []PanelField {
	return []PanelField{
		{Label: "pins", Value: strconv.Itoa(a.PinCount)},
		{Label: "reactions", Value: strconv.Itoa(a.ReactionCount)},
	}
}

// InlineBody implements the InlineViewer capability: an image is the one type
// whose body must render in-browser via `<img src>` rather than force a
// download through the generic sniff-proof route. DecidePreview only ever
// classifies an artifact as KeyImage when its media type already passed
// isImage at ingest (SPEC-0002 "Previewability Detection at Ingest"), so this
// re-check is defense in depth, not the authority.
func (imageShareType) InlineBody(a *artifact.Artifact) bool {
	return isImage(a.MediaType)
}

// codeShareType composes simpleType with the ArtifactBadger capability (a code
// artifact's badge is its language, `GO`/`PY`/…, falling back to the static
// CODE badge — ADR-0002 badge list: "`PY`/lang") and the BodyViewer capability:
// it renders a code body as the sanitized, server-side, line-numbered,
// syntax-highlighted HTML fragment the app shell drops into its body slot —
// the highlighted source, a symbol outline, and the code_line/code_range
// annotation affordances (SPEC-0003 REQ "Code Viewer", REQ "Code Annotation
// Anchors"). Highlighting + the outline heuristic live in internal/code (the
// code-viewer analogue of internal/markdown), so this wrapper stays a thin
// adapter over the streamed body, exactly like markdownShareType.RenderBody
// above — the viewer stays a registry capability discovered by type assertion
// (ADR-0002), never a switch on the code key.
type codeShareType struct {
	simpleType
}

func (t codeShareType) BadgeFor(a *artifact.Artifact) string {
	return langBadge(a.MediaType, a.Title)
}

// RenderBody implements the BodyViewer capability. chroma HTML-escapes every
// highlighted token (see internal/code.Render), so the returned fragment
// cannot execute active content in Cairn's origin (SPEC-0003 Security
// Requirements: "source is escaped, never executed") — the code analogue of
// markdownShareType.RenderBody's sanitization guarantee above. The language
// override, when the caller attached one via WithBodyHint (the web shell's
// `?lang=` query param), takes precedence over media_type/title detection.
func (codeShareType) RenderBody(ctx context.Context, a *artifact.Artifact, body io.Reader) (template.HTML, error) {
	src, err := io.ReadAll(body)
	if err != nil {
		return "", err
	}
	return code.RenderFragment(src, a.MediaType, a.Title, BodyHint(ctx))
}

// MetadataPanel implements the MetadataPaneler capability: the language field
// the panel shows (SPEC-0003 REQ "Code Viewer": "supplies its metadata-panel
// fields (e.g. language, line count) via the registry capability"). Line
// count is NOT supplied here — MetadataPanel receives only the artifact, not
// its body, and line count cannot be derived without reading the body; it is
// instead shown in the code viewer's own in-fragment STATS line (see
// internal/code/viewer.go), the same place markdownShareType's body-derived
// facts (word count, heading count, read time) live rather than the registry
// panel.
func (codeShareType) MetadataPanel(a *artifact.Artifact) []PanelField {
	lang := code.Detect("", a.MediaType, a.Title)
	return []PanelField{{Label: "language", Value: lang.Display}}
}

// langBadge derives a short language badge from the artifact's media type,
// falling back to its title's file extension. Returns "" when unrecognized so
// the caller falls back to the type's static badge.
func langBadge(mediaType, title string) string {
	if mt, _, err := mime.ParseMediaType(mediaType); err == nil {
		sub := mt[strings.Index(mt, "/")+1:]
		sub = strings.TrimPrefix(sub, "x-")
		if b, ok := langBadges[sub]; ok {
			return b
		}
	}
	if ext := strings.TrimPrefix(strings.ToLower(path.Ext(title)), "."); ext != "" {
		if b, ok := langBadges[ext]; ok {
			return b
		}
	}
	return ""
}

// langBadges maps media-type subtypes (sans "x-" prefix) and file extensions to
// their badge codes.
var langBadges = map[string]string{
	"go": "GO", "golang": "GO",
	"python": "PY", "py": "PY",
	"javascript": "JS", "js": "JS", "mjs": "JS",
	"typescript": "TS", "ts": "TS",
	"ruby": "RB", "rb": "RB",
	"rust": "RS", "rs": "RS",
	"java": "JAVA",
	"c":    "C", "h": "C",
	"c++": "CPP", "cpp": "CPP", "cc": "CPP", "hpp": "CPP",
	"csharp": "CS", "cs": "CS",
	"sh": "SH", "shellscript": "SH", "bash": "SH", "zsh": "SH",
	"sql":  "SQL",
	"yaml": "YAML", "yml": "YAML",
	"json":   "JSON",
	"toml":   "TOML",
	"html":   "HTML",
	"css":    "CSS",
	"php":    "PHP",
	"kotlin": "KT", "kt": "KT",
	"swift": "SWIFT",
}

func isText(mediaType string) bool {
	return strings.HasPrefix(strings.ToLower(mediaType), "text/")
}

func isImage(mediaType string) bool {
	return strings.HasPrefix(strings.ToLower(mediaType), "image/")
}

// anyMedia is used by types whose viewer exists independent of the body's media
// type (bundle tabs, webhook stream, trajectory waterfall).
func anyMedia(string) bool { return true }

// Built-in types. Badges follow ADR-0002 (`MD`, lang/`CODE`, `IMG`, `FILE`/`GZ`,
// `HK`, `TRJ`); anchor capability sets follow the SPEC-0006 annotations design
// matrix verbatim:
//
//	markdown    reactions: artifact, md_block, md_bullet     comments: artifact, text_selection
//	code        reactions: artifact, code_line, code_range   comments: artifact, code_line, text_selection
//	image       reactions: artifact, image_region            comments: artifact, image_region
//	file/gz     reactions: artifact                          comments: artifact
//	webhook     reactions: artifact, webhook_request         comments: — (none)
//	trajectory  reactions: artifact, trajectory_turn,        comments: artifact, trajectory_span,
//	                       trajectory_toolcall                         text_selection
//
// (bundle is not in the matrix; it mirrors file plus comment-only text
// selection across its composed members until SPEC-0003 pins it down.)
//
// Governing: ADR-0002, ADR-0006, SPEC-0006 REQ "Registry-Gated Anchor
// Capabilities", REQ "Webhook Reaction-Only Asymmetry".
var (
	// The generic file handler is the total-resolution floor: never previewable,
	// whole-artifact annotations only. GZ is the same shape for gzip.
	fileType = simpleType{
		key:     artifact.TypeFile,
		badge:   "FILE",
		anchors: []AnchorSpec{both(AnchorArtifact)},
	}
	gzType = simpleType{
		key:     artifact.TypeGZ,
		badge:   "GZ",
		anchors: []AnchorSpec{both(AnchorArtifact)},
	}

	markdownType = markdownShareType{simpleType{
		key:     KeyMarkdown,
		badge:   "MD",
		preview: isText,
		anchors: []AnchorSpec{
			both(AnchorArtifact),
			reactionOnly(AnchorMarkdownBlock),
			reactionOnly(AnchorMarkdownBullet),
			commentOnly(AnchorTextSelection),
		},
	}}
	codeType = codeShareType{simpleType{
		key:     KeyCode,
		badge:   "CODE",
		preview: isText,
		anchors: []AnchorSpec{
			both(AnchorArtifact),
			both(AnchorCodeLine),
			reactionOnly(AnchorCodeRange),
			commentOnly(AnchorTextSelection),
		},
	}}
	imageType = imageShareType{simpleType{
		key:     KeyImage,
		badge:   "IMG",
		preview: isImage,
		anchors: []AnchorSpec{
			both(AnchorArtifact),
			both(AnchorImageRegion),
		},
	}}
	// A bundle browses its ordered members through the file rail, each rendered by
	// its own registered viewer (SPEC-0003). Per-member reactions and comments
	// anchor to bundle_file (name-scoped); a member's own text selections stay
	// text_selection, so the aggregated COMMENTS panel resolves each thread to its
	// file (SPEC-0006 matrix, pinned down by SPEC-0003).
	bundleType = bundleShareType{simpleType{
		key:     artifact.TypeBundle,
		badge:   "BUNDLE",
		preview: anyMedia,
		anchors: []AnchorSpec{
			both(AnchorArtifact),
			both(AnchorBundleFile),
			commentOnly(AnchorTextSelection),
		},
	}}
	// Webhook artifacts accept comments on NO anchor — not even whole-artifact —
	// while staying reactable; the asymmetry is registry data, not special-cased
	// code (SPEC-0006 REQ "Webhook Reaction-Only Asymmetry"). The MCP handle
	// carries the legible hook/ prefix; the web URL stays bare (ADR-0005).
	webhookType = prefixedType{
		simpleType{
			key:     KeyWebhook,
			badge:   "HK",
			preview: anyMedia,
			anchors: []AnchorSpec{
				reactionOnly(AnchorArtifact),
				reactionOnly(AnchorWebhookRequest),
			},
		},
		URLPrefix{MCP: "hook"},
	}
	// Trajectory: reactions pin moments (turns, tool calls); comment threads
	// attach to spans and text selections (SPEC-0006 matrix). Both the web URL
	// and the MCP handle carry the run/ prefix (ADR-0005).
	trajectoryType = prefixedType{
		simpleType{
			key:     KeyTrajectory,
			badge:   "TRJ",
			preview: anyMedia,
			anchors: []AnchorSpec{
				both(AnchorArtifact),
				reactionOnly(AnchorTrajectoryTurn),
				reactionOnly(AnchorTrajectoryToolCall),
				commentOnly(AnchorTrajectorySpan),
				commentOnly(AnchorTextSelection),
			},
		},
		URLPrefix{Web: "run", MCP: "run"},
	}
)

// defaultRegistry is the process-wide registry, seeded with the built-in types.
// The generic file handler is both a registered type and the total-resolution
// fallback.
var defaultRegistry = newDefault()

func newDefault() *Registry {
	r := NewRegistry(fileType)
	for _, t := range []ShareType{
		fileType, gzType, markdownType, codeType, imageType,
		bundleType, webhookType, trajectoryType,
	} {
		r.Register(t)
	}
	return r
}

// Default returns the process-wide share-type registry.
func Default() *Registry { return defaultRegistry }

// Package-level conveniences over the default registry.

// Resolve resolves a key to its handler via the default registry (total).
func Resolve(key artifact.ShareType) ShareType { return defaultRegistry.Resolve(key) }

// DecidePreview decides effective type + previewability via the default registry.
func DecidePreview(declared artifact.ShareType, mediaType string, size, previewMax int64) (artifact.ShareType, bool) {
	return defaultRegistry.DecidePreview(declared, mediaType, size, previewMax)
}

// ValidateAnchor validates an anchor/kind pair via the default registry.
func ValidateAnchor(key artifact.ShareType, anchor Anchor, kind AnnotationKind) error {
	return defaultRegistry.ValidateAnchor(key, anchor, kind)
}

// AllowsAnchor reports anchor/kind legality via the default registry.
func AllowsAnchor(key artifact.ShareType, anchor Anchor, kind AnnotationKind) bool {
	return defaultRegistry.AllowsAnchor(key, anchor, kind)
}
