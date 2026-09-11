package outboundhook

import (
	"bytes"
	"flag"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/joestump/cairn/internal/store"
)

// Governing: ADR-0017 (Outbound Webhooks), SPEC-0012 REQ "Event Payload"

var updateGolden = flag.Bool("update", false, "rewrite the testdata/*.golden.json payload files")

// goldenEmitter builds an emitter for encode-only use: no targets, no worker.
func goldenEmitter() *Emitter {
	return New(nil, "", "https://cairn.example", slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// restEvent is a creation over REST/CLI: no on-behalf-of, no labels. Its golden
// was generated before the payload grew on_behalf_of and labels, so a match
// proves that growth is byte-for-byte additive for every existing consumer.
func restEvent() store.CreationEvent {
	return store.CreationEvent{
		PublicID:  "7Kq2mZ",
		ShareType: "markdown",
		Title:     "incident notes",
		WebPath:   "/7Kq2mZ",
		ActorID:   "joestump",
		Model:     "claude-opus-5",
		Channel:   "via API",
		ExpiresAt: time.Date(2026, 9, 18, 8, 0, 0, 0, time.UTC),
	}
}

// assertGolden compares got against testdata/<name>.golden.json, rewriting the
// file instead when -update is set.
func assertGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden.json")
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(got, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run with -update to create it): %v", err)
	}
	if want = bytes.TrimSuffix(want, []byte("\n")); !bytes.Equal(got, want) {
		t.Fatalf("payload drifted from %s\n got: %s\nwant: %s", path, got, want)
	}
}

var (
	goldenEventID   = "5b0f3c1e-8a3d-4c55-9f0e-2d7c6b1a9e40"
	goldenCreatedAt = time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
)

func TestGoldenPayloadREST(t *testing.T) {
	raw, err := goldenEmitter().encode(restEvent(), goldenEventID, goldenCreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	assertGolden(t, "artifact_created_rest", raw)
}
