package sharetype

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/joestump/cairn/internal/artifact"
	"github.com/joestump/cairn/internal/errs"
)

// Governing: ADR-0002 (Extensible Share-Type Model; optional capability
// interfaces), SPEC-0002 REQ "Share-Type Registry and Total Resolution", REQ
// "Per-Type Anchor Affordances", REQ "Previewability Detection at Ingest",
// SPEC-0006 REQ "Registry-Gated Anchor Capabilities", REQ "Webhook
// Reaction-Only Asymmetry".

func TestResolveTotalToGenericFile(t *testing.T) {
	r := Default()
	// An unregistered / uninterpretable type resolves to the generic file
	// handler so the artifact stays viewable and downloadable.
	for _, key := range []artifact.ShareType{"", "not-a-real-type", "🙂"} {
		got := r.Resolve(key)
		if got.Key() != artifact.TypeFile {
			t.Fatalf("Resolve(%q).Key() = %q, want %q (generic file fallback)", key, got.Key(), artifact.TypeFile)
		}
	}
	// A registered type resolves to itself.
	if got := r.Resolve(KeyMarkdown); got.Key() != KeyMarkdown {
		t.Fatalf("Resolve(markdown).Key() = %q, want markdown", got.Key())
	}
}

func TestRegisterDuplicatePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("registering a duplicate key should panic")
		}
	}()
	r := NewRegistry(fileType)
	r.Register(markdownType)
	r.Register(markdownType) // duplicate -> panic
}

func TestRegisterEmptyKeyPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("registering an empty key should panic")
		}
	}()
	NewRegistry(fileType).Register(simpleType{key: "", badge: "X"})
}

// TestAddingTypeIsAdditive shows a brand-new type is integrated purely by
// registering it — no edit to the aggregate, storage, or a switch statement.
func TestAddingTypeIsAdditive(t *testing.T) {
	r := NewRegistry(fileType)
	r.Register(fileType)
	custom := simpleType{
		key:     "sbom",
		badge:   "SBOM",
		preview: isText,
		anchors: []AnchorSpec{both(AnchorArtifact), commentOnly(AnchorTextSelection)},
	}
	r.Register(custom)
	if got := r.Resolve("sbom"); got.Badge() != "SBOM" {
		t.Fatalf("custom type badge = %q, want SBOM", got.Badge())
	}
	ty, prev := r.DecidePreview("sbom", "text/plain", 100, 1<<20)
	if ty != "sbom" || !prev {
		t.Fatalf("DecidePreview(sbom, text) = (%q,%v), want (sbom,true)", ty, prev)
	}
}

func TestDecidePreview(t *testing.T) {
	r := Default()
	const previewMax = 1 << 20
	tests := []struct {
		name     string
		declared artifact.ShareType
		media    string
		size     int64
		wantType artifact.ShareType
		wantPrev bool
	}{
		{"markdown small text", KeyMarkdown, "text/markdown", 200, KeyMarkdown, true},
		{"markdown plain sniffed", KeyMarkdown, "text/plain; charset=utf-8", 200, KeyMarkdown, true},
		{"markdown too large downgrades", KeyMarkdown, "text/markdown", previewMax + 1, artifact.TypeFile, false},
		{"code small text", KeyCode, "text/x-go", 500, KeyCode, true},
		{"image png", KeyImage, "image/png", 4096, KeyImage, true},
		{"image wrong media downgrades", KeyImage, "text/plain", 4096, artifact.TypeFile, false},
		{"unknown type downgrades to file", "totally-unknown", "text/plain", 10, artifact.TypeFile, false},
		{"gzip body becomes gz", KeyMarkdown, "application/gzip", previewMax + 5, artifact.TypeGZ, false},
		{"declared file stays file", artifact.TypeFile, "application/octet-stream", 10, artifact.TypeFile, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotType, gotPrev := r.DecidePreview(tc.declared, tc.media, tc.size, previewMax)
			if gotType != tc.wantType || gotPrev != tc.wantPrev {
				t.Fatalf("DecidePreview(%q,%q,%d) = (%q,%v), want (%q,%v)",
					tc.declared, tc.media, tc.size, gotType, gotPrev, tc.wantType, tc.wantPrev)
			}
		})
	}
}

func TestDecidePreviewZeroBoundDisablesSizeCheck(t *testing.T) {
	// A non-positive bound means "no size limit" — a large previewable body
	// stays previewable.
	ty, prev := Default().DecidePreview(KeyMarkdown, "text/markdown", 1<<30, 0)
	if ty != KeyMarkdown || !prev {
		t.Fatalf("DecidePreview with 0 bound = (%q,%v), want (markdown,true)", ty, prev)
	}
}

// TestDecidePreviewPromotesGenericFloorByContent pins the #72 badge-drift fix:
// the badge is a function of the CONTENT TYPE, not the push path, so a body
// declared as the generic floor (file/empty — the uploader made no type claim)
// is promoted to the rich type its media type names. A real binary still lands
// on FILE. Bundle and run are declared types (never the floor), so they pass
// through untouched.
func TestDecidePreviewPromotesGenericFloorByContent(t *testing.T) {
	r := Default()
	const previewMax = 1 << 20
	tests := []struct {
		name     string
		declared artifact.ShareType
		media    string
		wantType artifact.ShareType
		wantPrev bool
	}{
		{"markdown declared file", artifact.TypeFile, "text/markdown", KeyMarkdown, true},
		{"markdown declared empty", "", "text/markdown", KeyMarkdown, true},
		{"go declared file", artifact.TypeFile, "text/x-go", KeyCode, true},
		{"go declared empty", "", "text/x-gosrc", KeyCode, true},
		// An image pushed on the generic route badges IMG and previews, exactly
		// as the same body declared `image` does — the promotion is asked of
		// the image type's own PreviewableMedia, so the two routes cannot
		// diverge. SVG lands on IMAGE, not CODE, despite chroma having an XML
		// lexer: the ordering in classifyByMedia is what guarantees it.
		{"png declared file", artifact.TypeFile, "image/png", KeyImage, true},
		{"jpeg declared empty", "", "image/jpeg", KeyImage, true},
		{"svg prefers image over the xml lexer", artifact.TypeFile, "image/svg+xml", KeyImage, true},
		// Plain text is not code: chroma resolves it to its plain-text
		// sentinel, so a .txt body stays on the floor rather than badging CODE.
		{"plain text stays file", artifact.TypeFile, "text/plain", artifact.TypeFile, false},
		{"generic binary stays file", artifact.TypeFile, "application/octet-stream", artifact.TypeFile, false},
		{"gzip floor stays gz", artifact.TypeGZ, "application/gzip", artifact.TypeGZ, false},
		{"declared bundle untouched", artifact.TypeBundle, "application/octet-stream", artifact.TypeBundle, true},
		{"declared markdown kept", KeyMarkdown, "text/markdown", KeyMarkdown, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotType, gotPrev := r.DecidePreview(tc.declared, tc.media, 100, previewMax)
			if gotType != tc.wantType || gotPrev != tc.wantPrev {
				t.Fatalf("DecidePreview(%q,%q) = (%q,%v), want (%q,%v)",
					tc.declared, tc.media, gotType, gotPrev, tc.wantType, tc.wantPrev)
			}
		})
	}
}

// TestCapabilityMatrix exercises the full reconciled SPEC-0006 anchor
// capability matrix for every built-in type — reactions and comments — and
// asserts the declared anchor sets match the annotations design matrix exactly,
// so registry data cannot drift from the spec.
func TestCapabilityMatrix(t *testing.T) {
	r := Default()
	type rc struct{ reactions, comments bool }
	matrix := map[artifact.ShareType]map[Anchor]rc{
		KeyMarkdown: {
			AnchorArtifact:       {true, true},
			AnchorMarkdownBlock:  {true, false},
			AnchorMarkdownBullet: {true, false},
			AnchorTextSelection:  {false, true},
		},
		KeyCode: {
			AnchorArtifact:      {true, true},
			AnchorCodeLine:      {true, true},
			AnchorCodeRange:     {true, false},
			AnchorTextSelection: {false, true},
		},
		KeyImage: {
			AnchorArtifact:    {true, true},
			AnchorImageRegion: {true, true},
		},
		artifact.TypeFile: {
			AnchorArtifact: {true, true},
		},
		artifact.TypeGZ: {
			AnchorArtifact: {true, true},
		},
		// Bundle: per-member reactions + comments anchor to bundle_file; a member's
		// own text selections stay text_selection (comment-only). SPEC-0003 pins
		// this down from the SPEC-0006 matrix's provisional bundle row.
		artifact.TypeBundle: {
			AnchorArtifact:      {true, true},
			AnchorBundleFile:    {true, true},
			AnchorTextSelection: {false, true},
		},
		// Webhook: reactable on artifact + webhook_request, comments on NOTHING
		// (SPEC-0006 REQ "Webhook Reaction-Only Asymmetry").
		KeyWebhook: {
			AnchorArtifact:       {true, false},
			AnchorWebhookRequest: {true, false},
		},
		KeyTrajectory: {
			AnchorArtifact:           {true, true},
			AnchorTrajectoryTurn:     {true, false},
			AnchorTrajectoryToolCall: {true, false},
			AnchorTrajectorySpan:     {false, true},
			AnchorTextSelection:      {false, true},
		},
	}
	for key, anchors := range matrix {
		for anchor, want := range anchors {
			if got := r.AllowsAnchor(key, anchor, KindReaction); got != want.reactions {
				t.Errorf("AllowsAnchor(%q,%q,reaction) = %v, want %v", key, anchor, got, want.reactions)
			}
			if got := r.AllowsAnchor(key, anchor, KindComment); got != want.comments {
				t.Errorf("AllowsAnchor(%q,%q,comment) = %v, want %v", key, anchor, got, want.comments)
			}
		}
		// The declared spec set must match the matrix exactly — no extra anchors.
		declared := r.Resolve(key).Anchors()
		for _, spec := range declared {
			want, ok := anchors[spec.Anchor]
			if !ok {
				t.Errorf("type %q declares anchor %q not in the SPEC-0006 matrix", key, spec.Anchor)
				continue
			}
			if spec.Reactions != want.reactions || spec.Comments != want.comments {
				t.Errorf("type %q anchor %q = {reactions:%v comments:%v}, want {%v %v}",
					key, spec.Anchor, spec.Reactions, spec.Comments, want.reactions, want.comments)
			}
		}
		if len(declared) != len(anchors) {
			t.Errorf("type %q declares %d anchors, matrix has %d", key, len(declared), len(anchors))
		}
	}
}

func TestAnchorAffordances(t *testing.T) {
	r := Default()
	tests := []struct {
		name   string
		key    artifact.ShareType
		anchor Anchor
		kind   AnnotationKind
		want   bool
	}{
		// Whole-artifact on most types permits both kinds — as declared data.
		{"whole artifact reaction on file", artifact.TypeFile, AnchorArtifact, KindReaction, true},
		{"whole artifact comment on file", artifact.TypeFile, AnchorArtifact, KindComment, true},
		// An unknown type resolves to the file fallback, which keeps it
		// annotatable at the whole-artifact level (ADR-0002 degradation floor).
		{"whole artifact on unknown type", "mystery", AnchorArtifact, KindComment, true},
		// Markdown blocks/bullets are reaction pins; comments ride selections.
		{"markdown block reaction", KeyMarkdown, AnchorMarkdownBlock, KindReaction, true},
		{"markdown block comment rejected", KeyMarkdown, AnchorMarkdownBlock, KindComment, false},
		{"markdown selection comment", KeyMarkdown, AnchorTextSelection, KindComment, true},
		{"markdown selection reaction rejected", KeyMarkdown, AnchorTextSelection, KindReaction, false},
		// Webhook rejects ALL comments — even whole-artifact — but stays reactable.
		{"webhook request reaction", KeyWebhook, AnchorWebhookRequest, KindReaction, true},
		{"webhook request comment rejected", KeyWebhook, AnchorWebhookRequest, KindComment, false},
		{"webhook whole-artifact comment rejected", KeyWebhook, AnchorArtifact, KindComment, false},
		{"webhook whole-artifact reaction", KeyWebhook, AnchorArtifact, KindReaction, true},
		// Trajectory: reactions on turn/toolcall only; comments on span/selection only.
		{"trajectory turn reaction", KeyTrajectory, AnchorTrajectoryTurn, KindReaction, true},
		{"trajectory turn comment rejected", KeyTrajectory, AnchorTrajectoryTurn, KindComment, false},
		{"trajectory span comment", KeyTrajectory, AnchorTrajectorySpan, KindComment, true},
		{"trajectory span reaction rejected", KeyTrajectory, AnchorTrajectorySpan, KindReaction, false},
		// Code: line permits both, range is reaction-only.
		{"code line comment", KeyCode, AnchorCodeLine, KindComment, true},
		{"code range reaction", KeyCode, AnchorCodeRange, KindReaction, true},
		{"code range comment rejected", KeyCode, AnchorCodeRange, KindComment, false},
		// An anchor a type does not declare is rejected.
		{"code line on markdown rejected", KeyMarkdown, AnchorCodeLine, KindComment, false},
		{"image region on code rejected", KeyCode, AnchorImageRegion, KindReaction, false},
		// Generic file permits only whole-artifact, so any specific anchor fails.
		{"selection on file rejected", artifact.TypeFile, AnchorTextSelection, KindComment, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := r.AllowsAnchor(tc.key, tc.anchor, tc.kind); got != tc.want {
				t.Fatalf("AllowsAnchor(%q,%q,%q) = %v, want %v", tc.key, tc.anchor, tc.kind, got, tc.want)
			}
		})
	}
}

func TestValidateAnchorReturnsValidationError(t *testing.T) {
	// A comment anywhere on a webhook artifact must be rejected with a
	// validation code (SPEC-0006 "Comment on a webhook request refused").
	for _, anchor := range []Anchor{AnchorWebhookRequest, AnchorArtifact} {
		err := Default().ValidateAnchor(KeyWebhook, anchor, KindComment)
		if err == nil {
			t.Fatalf("expected a validation error for a comment on webhook %q anchor", anchor)
		}
		if errs.CodeOf(err) != errs.CodeValidation {
			t.Fatalf("code = %q, want validation_failed", errs.CodeOf(err))
		}
		if !errors.Is(err, errs.ErrValidation) {
			t.Fatal("error should wrap errs.ErrValidation")
		}
	}
	// A legal anchor returns nil.
	if err := Default().ValidateAnchor(KeyWebhook, AnchorArtifact, KindReaction); err != nil {
		t.Fatalf("whole-artifact reaction on webhook should be legal, got %v", err)
	}
	if err := Default().ValidateAnchor(KeyMarkdown, AnchorArtifact, KindComment); err != nil {
		t.Fatalf("whole-artifact comment on markdown should be legal, got %v", err)
	}
}

func TestAnchorTypeStringsMatchSpec(t *testing.T) {
	// The Anchor constants are the persisted anchor_type discriminators; they
	// must match the SPEC-0006 annotations design matrix verbatim.
	want := map[Anchor]string{
		AnchorArtifact:           "artifact",
		AnchorMarkdownBlock:      "md_block",
		AnchorMarkdownBullet:     "md_bullet",
		AnchorTextSelection:      "text_selection",
		AnchorCodeLine:           "code_line",
		AnchorCodeRange:          "code_range",
		AnchorImageRegion:        "image_region",
		AnchorWebhookRequest:     "webhook_request",
		AnchorTrajectorySpan:     "trajectory_span",
		AnchorTrajectoryTurn:     "trajectory_turn",
		AnchorTrajectoryToolCall: "trajectory_toolcall",
	}
	for anchor, s := range want {
		if string(anchor) != s {
			t.Errorf("anchor constant = %q, want %q", anchor, s)
		}
	}
}

func TestBadges(t *testing.T) {
	// Badge codes follow ADR-0002: MD, CODE/lang, IMG, FILE/GZ, HK, RUN (the
	// trajectory type's reader-facing badge, renamed in #68).
	want := map[artifact.ShareType]string{
		artifact.TypeFile:   "FILE",
		artifact.TypeGZ:     "GZ",
		artifact.TypeBundle: "BUNDLE",
		KeyMarkdown:         "MD",
		KeyCode:             "CODE",
		KeyImage:            "IMG",
		KeyWebhook:          "HK",
		KeyTrajectory:       "RUN",
	}
	for key, badge := range want {
		if b := Default().Resolve(key).Badge(); b != badge {
			t.Errorf("type %q badge = %q, want %q", key, b, badge)
		}
	}
}

// TestDisplayNames covers the #68 taxonomy decision: the flagship trajectory
// type reads as "run" to a reader (matching /run/ and the run_create/
// run_append_spans MCP tools) while keeping "trajectory" as its registry key;
// every other type's display name is its key.
func TestDisplayNames(t *testing.T) {
	r := Default()
	if got := r.DisplayNameFor(KeyTrajectory); got != "run" {
		t.Errorf("DisplayNameFor(trajectory) = %q, want %q", got, "run")
	}
	for _, key := range []artifact.ShareType{KeyMarkdown, KeyCode, KeyImage, KeyWebhook, artifact.TypeFile} {
		if got := r.DisplayNameFor(key); got != string(key) {
			t.Errorf("DisplayNameFor(%q) = %q, want its key", key, got)
		}
	}
	// An unknown key resolves to the fallback, which has no DisplayNamer, so
	// the raw key is returned unchanged.
	if got := r.DisplayNameFor("mystery"); got != "mystery" {
		t.Errorf("DisplayNameFor(mystery) = %q, want the raw key", got)
	}
}

func TestBadgeForLangBadges(t *testing.T) {
	r := Default()
	tests := []struct {
		name  string
		a     *artifact.Artifact
		badge string
	}{
		{"go by media type", &artifact.Artifact{ShareType: KeyCode, MediaType: "text/x-go"}, "GO"},
		{"python by media type", &artifact.Artifact{ShareType: KeyCode, MediaType: "text/x-python"}, "PY"},
		{"python with charset param", &artifact.Artifact{ShareType: KeyCode, MediaType: "text/x-python; charset=utf-8"}, "PY"},
		{"ts by title extension", &artifact.Artifact{ShareType: KeyCode, MediaType: "text/plain", Title: "deploy.ts"}, "TS"},
		{"unrecognized lang falls back", &artifact.Artifact{ShareType: KeyCode, MediaType: "text/plain", Title: "notes.xyz"}, "CODE"},
		{"non-code type uses static badge", &artifact.Artifact{ShareType: KeyTrajectory, MediaType: "application/json"}, "RUN"},
		{"unknown type falls back to FILE", &artifact.Artifact{ShareType: "mystery", MediaType: "text/plain"}, "FILE"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := r.BadgeFor(tc.a); got != tc.badge {
				t.Fatalf("BadgeFor = %q, want %q", got, tc.badge)
			}
		})
	}
}

func TestURLPrefixFor(t *testing.T) {
	r := Default()
	tests := []struct {
		key  artifact.ShareType
		want URLPrefix
	}{
		{KeyTrajectory, URLPrefix{Web: "run", MCP: "run"}},
		{KeyWebhook, URLPrefix{Web: "", MCP: "hook"}},
		{KeyMarkdown, URLPrefix{}},
		{artifact.TypeFile, URLPrefix{}},
		{"unknown-type", URLPrefix{}},
	}
	for _, tc := range tests {
		if got := r.URLPrefixFor(tc.key); got != tc.want {
			t.Errorf("URLPrefixFor(%q) = %+v, want %+v", tc.key, got, tc.want)
		}
	}
}

// capType is a test share type composing every optional capability, proving a
// new type registers identity + anchors + previewability + viewer + panel +
// locator schemas + URL prefix + routes with zero edits to store/httpapi core
// (the acceptance bar of the SDK story).
type capType struct {
	simpleType
}

func (t capType) URLPrefix() URLPrefix { return URLPrefix{Web: "cap", MCP: "cap"} }

func (t capType) BadgeFor(a *artifact.Artifact) string { return "CAP!" }

func (t capType) RenderBody(ctx context.Context, a *artifact.Artifact, body io.Reader) (template.HTML, error) {
	b, err := io.ReadAll(body)
	if err != nil {
		return "", err
	}
	return template.HTML("<pre>" + template.HTMLEscapeString(string(b)) + "</pre>"), nil
}

func (t capType) MetadataPanel(a *artifact.Artifact) []PanelField {
	return []PanelField{{Label: "Size", Value: fmt.Sprintf("%d bytes", a.Size)}}
}

func (t capType) LocatorSchema(anchor Anchor) LocatorFunc {
	if anchor != AnchorCodeLine {
		return nil
	}
	return func(ref json.RawMessage) error {
		var loc struct {
			Line *int `json:"line"`
		}
		if err := json.Unmarshal(ref, &loc); err != nil || loc.Line == nil || *loc.Line < 1 {
			return errs.Validationf("code_line locator requires a positive line")
		}
		return nil
	}
}

func (t capType) MountRoutes(r chi.Router) {
	r.Get("/caps/ping", func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("pong"))
	})
}

// bareType implements only the base ShareType contract — no optional
// capabilities at all (unlike simpleType, which supplies the built-in locator
// schemas) — proving every capability helper degrades totally.
type bareType struct{ key artifact.ShareType }

func (t bareType) Key() artifact.ShareType      { return t.key }
func (t bareType) Badge() string                { return "BARE" }
func (t bareType) PreviewableMedia(string) bool { return false }
func (t bareType) Anchors() []AnchorSpec        { return []AnchorSpec{both(AnchorArtifact)} }

func newCapRegistry(t *testing.T) *Registry {
	t.Helper()
	r := NewRegistry(fileType)
	r.Register(fileType)
	r.Register(bareType{key: "bare"})
	r.Register(capType{simpleType{
		key:     "cap",
		badge:   "CAP",
		preview: anyMedia,
		anchors: []AnchorSpec{both(AnchorArtifact), both(AnchorCodeLine), commentOnly(AnchorTextSelection)},
	}})
	return r
}

func TestCapabilityBodyViewer(t *testing.T) {
	r := newCapRegistry(t)
	v, ok := r.BodyViewerFor("cap")
	if !ok {
		t.Fatal("cap type should expose a BodyViewer")
	}
	frag, err := v.RenderBody(context.Background(), &artifact.Artifact{ShareType: "cap"}, strings.NewReader("<b>hi</b>"))
	if err != nil {
		t.Fatalf("RenderBody: %v", err)
	}
	if want := template.HTML("<pre>&lt;b&gt;hi&lt;/b&gt;</pre>"); frag != want {
		t.Fatalf("RenderBody = %q, want %q", frag, want)
	}
	// A type without the capability reports (nil, false) so the shell falls
	// back to the generic file card.
	if _, ok := r.BodyViewerFor(artifact.TypeFile); ok {
		t.Fatal("file type should not expose a BodyViewer")
	}
	if _, ok := r.BodyViewerFor("never-registered"); ok {
		t.Fatal("unknown type should resolve to the viewerless file fallback")
	}
}

func TestCapabilityMetadataPanel(t *testing.T) {
	r := newCapRegistry(t)
	fields := r.MetadataPanelFor(&artifact.Artifact{ShareType: "cap", Size: 42})
	if len(fields) != 1 || fields[0].Label != "Size" || fields[0].Value != "42 bytes" {
		t.Fatalf("MetadataPanelFor = %+v, want [{Size 42 bytes}]", fields)
	}
	if got := r.MetadataPanelFor(&artifact.Artifact{ShareType: artifact.TypeFile}); got != nil {
		t.Fatalf("file type panel = %+v, want nil (no capability)", got)
	}
}

func TestCapabilityLocatorSchema(t *testing.T) {
	r := newCapRegistry(t)
	// The whole-artifact anchor_ref must be the empty object (SPEC-0006
	// "Whole-artifact anchor") — enforced by the registry for every type.
	for _, ok := range []string{"", "{}", " { } "} {
		if err := r.ValidateLocator("cap", AnchorArtifact, json.RawMessage(ok)); err != nil {
			t.Errorf("ValidateLocator(artifact, %q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{`{"x":1}`, `[1]`, `"s"`} {
		err := r.ValidateLocator("cap", AnchorArtifact, json.RawMessage(bad))
		if err == nil {
			t.Errorf("ValidateLocator(artifact, %q) = nil, want validation error", bad)
		} else if errs.CodeOf(err) != errs.CodeValidation {
			t.Errorf("ValidateLocator(artifact, %q) code = %q, want validation_failed", bad, errs.CodeOf(err))
		}
	}
	// A type-declared schema validates its anchor_ref.
	if err := r.ValidateLocator("cap", AnchorCodeLine, json.RawMessage(`{"line":3}`)); err != nil {
		t.Fatalf("valid code_line locator rejected: %v", err)
	}
	if err := r.ValidateLocator("cap", AnchorCodeLine, json.RawMessage(`{"line":0}`)); err == nil {
		t.Fatal("line 0 locator should be rejected by the type's schema")
	}
	if err := r.ValidateLocator("cap", AnchorCodeLine, json.RawMessage(`{"nope":true}`)); err == nil {
		t.Fatal("shapeless locator should be rejected by the type's schema")
	}
	// An anchor with no declared schema accepts any payload (tightened later
	// as each viewer story lands its locator shapes).
	if err := r.ValidateLocator("cap", AnchorTextSelection, json.RawMessage(`{"whatever":1}`)); err != nil {
		t.Fatalf("schemaless anchor should accept any payload, got %v", err)
	}
	// A type without the LocatorSchemer capability accepts any non-artifact ref.
	if err := r.ValidateLocator("bare", AnchorCodeLine, json.RawMessage(`{"line":-9}`)); err != nil {
		t.Fatalf("capability-less type should not validate locators, got %v", err)
	}
}

func TestCapabilityRouteMounting(t *testing.T) {
	r := newCapRegistry(t)
	router := chi.NewRouter()
	router.Route("/v1", func(v1 chi.Router) {
		r.MountRoutes(v1)
	})
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/caps/ping")
	if err != nil {
		t.Fatalf("GET /v1/caps/ping: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "pong" {
		t.Fatalf("body = %q, want pong", body)
	}
}

func TestCapabilityBadgeAndPrefixViaRegistry(t *testing.T) {
	r := newCapRegistry(t)
	if got := r.URLPrefixFor("cap"); got != (URLPrefix{Web: "cap", MCP: "cap"}) {
		t.Fatalf("URLPrefixFor(cap) = %+v", got)
	}
	if got := r.BadgeFor(&artifact.Artifact{ShareType: "cap"}); got != "CAP!" {
		t.Fatalf("BadgeFor(cap) = %q, want dynamic CAP!", got)
	}
}

func TestTypesIsSortedAndComplete(t *testing.T) {
	types := Default().Types()
	if len(types) != 8 {
		t.Fatalf("Types() returned %d types, want 8 built-ins", len(types))
	}
	for i := 1; i < len(types); i++ {
		if types[i-1].Key() >= types[i].Key() {
			t.Fatalf("Types() not sorted: %q before %q", types[i-1].Key(), types[i].Key())
		}
	}
}

func TestBadgesPresent(t *testing.T) {
	for _, key := range []artifact.ShareType{
		artifact.TypeFile, artifact.TypeGZ, artifact.TypeBundle,
		KeyMarkdown, KeyCode, KeyImage, KeyWebhook, KeyTrajectory,
	} {
		if b := Default().Resolve(key).Badge(); b == "" {
			t.Errorf("type %q has an empty badge", key)
		}
	}
}
