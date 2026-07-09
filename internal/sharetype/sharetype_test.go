package sharetype

import (
	"errors"
	"testing"

	"github.com/joestump/cairn/internal/artifact"
	"github.com/joestump/cairn/internal/errs"
)

// Governing: ADR-0002 (Extensible Share-Type Model), SPEC-0002 REQ "Share-Type
// Registry and Total Resolution", REQ "Per-Type Anchor Affordances", REQ
// "Previewability Detection at Ingest".

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
		anchors: []AnchorSpec{both(AnchorSelection)},
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

func TestAnchorAffordances(t *testing.T) {
	r := Default()
	tests := []struct {
		name   string
		key    artifact.ShareType
		anchor Anchor
		kind   AnnotationKind
		want   bool
	}{
		// Whole-artifact is always legal for every type and kind.
		{"whole artifact reaction on file", artifact.TypeFile, AnchorWholeArtifact, KindReaction, true},
		{"whole artifact comment on file", artifact.TypeFile, AnchorWholeArtifact, KindComment, true},
		{"whole artifact on unknown type", "mystery", AnchorWholeArtifact, KindComment, true},
		// Markdown block permits both.
		{"markdown block comment", KeyMarkdown, AnchorMarkdownBlock, KindComment, true},
		{"markdown block reaction", KeyMarkdown, AnchorMarkdownBlock, KindReaction, true},
		// Webhook request is reaction-only: reactable but NOT comment-threaded.
		{"webhook request reaction", KeyWebhook, AnchorWebhookRequest, KindReaction, true},
		{"webhook request comment rejected", KeyWebhook, AnchorWebhookRequest, KindComment, false},
		// An anchor a type does not declare is rejected.
		{"code line on markdown rejected", KeyMarkdown, AnchorCodeLine, KindComment, false},
		{"image region on code rejected", KeyCode, AnchorImageRegion, KindReaction, false},
		// Generic file permits only whole-artifact, so any specific anchor fails.
		{"selection on file rejected", artifact.TypeFile, AnchorSelection, KindComment, false},
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
	// A comment thread on a webhook request must be rejected with a validation
	// code (SPEC-0002 "Illegal anchor rejected").
	err := Default().ValidateAnchor(KeyWebhook, AnchorWebhookRequest, KindComment)
	if err == nil {
		t.Fatal("expected a validation error for a comment on a webhook request")
	}
	if errs.CodeOf(err) != errs.CodeValidation {
		t.Fatalf("code = %q, want validation_failed", errs.CodeOf(err))
	}
	if !errors.Is(err, errs.ErrValidation) {
		t.Fatal("error should wrap errs.ErrValidation")
	}
	// A legal anchor returns nil.
	if err := Default().ValidateAnchor(KeyWebhook, AnchorWholeArtifact, KindComment); err != nil {
		t.Fatalf("whole-artifact comment should be legal, got %v", err)
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
