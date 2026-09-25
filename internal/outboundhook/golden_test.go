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

	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/event"
	"github.com/stump-wtf/cairn/internal/store"
)

// Governing: ADR-0017 (Outbound Webhooks), SPEC-0012 REQ "Event Payload";
// ADR-0022, SPEC-0016 EV-3 "Payload Shape"

var updateGolden = flag.Bool("update", false, "rewrite the testdata/*.golden.json payload files")

// goldenEmitter builds an emitter for encode-only use: no targets, no worker.
func goldenEmitter() *Emitter {
	return New(nil, "", "https://cairn.example", slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// restEvent is a creation over REST/CLI with a personal access token: no
// on-behalf-of, no tags. Its golden was generated before the payload grew
// on_behalf_of and tags, and regenerated exactly once for ADR-0022, whose only
// change is the appended actor_kind and auth keys. A match proves every other
// byte is unchanged for every existing consumer (SPEC-0016 EV-3
// "artifact.created gains only appended keys").
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
		ActorKind: event.KindAgent,
		Auth:      event.AuthPAT,
		// Routing only: never on the wire, so the golden cannot contain it.
		OwnerID: "owner-never-on-the-wire",
	}
}

// handoffEvent is an agent's handoff bundle over MCP: server-derived
// on_behalf_of plus the full handoff tag convention.
func handoffEvent() store.CreationEvent {
	ev := restEvent()
	ev.ShareType = artifact.TypeBundle
	ev.Title = "handoff: fix flaky reaper test"
	ev.Channel = "via MCP"
	ev.Auth = event.AuthOAuth
	ev.OnBehalfOf = "claude-code/2.1.0"
	ev.Tags = []string{
		"handoff",
		"lane:m",
		"size:m",
		"repo:stump.wtf/cairn",
		"issue:stump.wtf/cairn#42",
		"source:claude-code/morning-brief-2026-09-11",
		"reply:cairn-comment",
	}
	return ev
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
	raw, err := goldenEmitter().encode(restEvent().Event(), goldenEventID, goldenCreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	assertGolden(t, "artifact_created_rest", raw)
}

func TestGoldenPayloadHandoff(t *testing.T) {
	raw, err := goldenEmitter().encode(handoffEvent().Event(), goldenEventID, goldenCreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	assertGolden(t, "artifact_created_handoff", raw)
}

// TestEmptyTagsOmitted: an empty (not just nil) tag slice must not emit
// "tags":[] — that would break the byte-compat the REST golden pins for any
// adapter that happens to pass a non-nil empty slice.
func TestEmptyTagsOmitted(t *testing.T) {
	ev := restEvent()
	ev.Tags = []string{}
	raw, err := goldenEmitter().encode(ev.Event(), goldenEventID, goldenCreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	assertGolden(t, "artifact_created_rest", raw)
}
