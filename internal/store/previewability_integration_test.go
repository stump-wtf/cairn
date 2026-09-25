package store

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/sharetype"
)

// Previewability is decided at ingest via the registry and stored on the
// artifact so every surface agrees without re-sniffing (SPEC-0002 REQ
// "Previewability Detection at Ingest"). These exercise the real ingest path.

func typedInput(shareType artifact.ShareType, body []byte) CreateArtifactInput {
	return CreateArtifactInput{
		ShareType:  shareType,
		Title:      "preview test",
		Body:       bytes.NewReader(body),
		Provenance: artifact.Provenance{CreatedByUserID: testOwner, ActorID: "u1", Channel: artifact.ChannelCLI, CapturedAt: time.Now()},
		Access:     artifact.AccessPolicy{OwnerUserID: testOwner, Visibility: artifact.VisibilityLink},
		ExpiresAt:  time.Now().Add(time.Hour),
	}
}

func TestPreviewableTextMarkdown(t *testing.T) {
	s, _ := newTestStore(t, Options{PreviewMaxBytes: 1 << 20})
	ctx := context.Background()

	art, err := s.CreateArtifact(ctx, typedInput(sharetype.KeyMarkdown, []byte("# hello\n\nsome *markdown* body")))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if art.ShareType != sharetype.KeyMarkdown {
		t.Fatalf("share type = %q, want markdown (previewable, kept)", art.ShareType)
	}
	if !art.Previewable {
		t.Fatal("small text markdown should be previewable")
	}
}

func TestNonPreviewableLargeBodyDowngradesToFile(t *testing.T) {
	// A tiny preview bound forces an otherwise-previewable body over the limit.
	s, _ := newTestStore(t, Options{PreviewMaxBytes: 8})
	ctx := context.Background()

	body := []byte(strings.Repeat("x", 4096)) // text, but well over the 8-byte bound
	art, err := s.CreateArtifact(ctx, typedInput(sharetype.KeyMarkdown, body))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if art.ShareType != artifact.TypeFile {
		t.Fatalf("share type = %q, want file (downgraded, non-previewable)", art.ShareType)
	}
	if art.Previewable {
		t.Fatal("body over the preview bound must be non-previewable")
	}
	if art.BodySHA256 != sha256Hex(body) {
		t.Fatalf("checksum wrong after downgrade: got %s", art.BodySHA256)
	}

	// The stored decision must survive a read (no re-sniffing).
	got, err := s.GetByPublicID(ctx, art.PublicID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Previewable || got.ShareType != artifact.TypeFile {
		t.Fatalf("read-back = (type=%q, previewable=%v), want (file, false)", got.ShareType, got.Previewable)
	}
}

func TestUnknownTypeIngestsAsFile(t *testing.T) {
	s, _ := newTestStore(t, Options{PreviewMaxBytes: 1 << 20})
	ctx := context.Background()

	art, err := s.CreateArtifact(ctx, typedInput("some-future-type", []byte("bytes")))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if art.ShareType != artifact.TypeFile || art.Previewable {
		t.Fatalf("unknown type ingested as (type=%q, previewable=%v), want (file, false)", art.ShareType, art.Previewable)
	}
}
