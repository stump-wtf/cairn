package main

import (
	"io"
	"log/slog"
	"testing"

	"github.com/joestump/cairn/internal/config"
)

// Governing: ADR-0017 (Outbound Webhooks), SPEC-0012 REQ "Event Emission on
// Artifact Creation"

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// The wiring seam: what main() actually hands to store.New.
//
// This is where cairn#201 lived. The emitter was declared
// `var emitter *outboundhook.Emitter` and assigned into the
// store.Options.Emitter INTERFACE field unconditionally, so an unconfigured
// deployment passed a non-nil interface wrapping a nil pointer. store's
// `s.emitter == nil` guard read false and dispatched to a nil receiver — after
// the commit, so the artifact was written and the client got a 500.
//
// These call newStoreOptions, the same function run() calls. An earlier draft
// re-implemented the `if emitter != nil` assignment inside the test body and
// therefore passed against known-broken production code: it was asserting
// against its own fixture. That is the same defect as the bug it was meant to
// catch, and it is why every existing test in this repo — each of which builds
// its own emitter — stayed green while the binary panicked on every create.
func TestStoreOptionsEmitterIsNilInterfaceWhenUnconfigured(t *testing.T) {
	cfg := &config.Config{}

	emitter := newOutboundEmitter(cfg, quietLogger())
	if emitter != nil {
		t.Fatalf("newOutboundEmitter with no target URLs = %v, want nil", emitter)
	}

	opts := newStoreOptions(cfg, emitter)
	if opts.Emitter != nil {
		t.Fatal("store.Options.Emitter is a non-nil interface for an unconfigured " +
			"deployment: store's nil guard will not fire, and every artifact " +
			"create will panic on a nil receiver after committing the row " +
			"(cairn#201)")
	}
}

// The configured path must still reach store, so the assertion above cannot be
// satisfied by simply never wiring an emitter at all.
func TestStoreOptionsCarriesEmitterWhenConfigured(t *testing.T) {
	cfg := &config.Config{
		OutboundWebhookURLs: []string{"https://example.invalid/sink"},
		BaseURL:             "https://cairn.example",
	}

	emitter := newOutboundEmitter(cfg, quietLogger())
	if emitter == nil {
		t.Fatal("newOutboundEmitter with a target URL = nil, want an emitter")
	}

	if opts := newStoreOptions(cfg, emitter); opts.Emitter == nil {
		t.Fatal("configured emitter did not reach store.Options.Emitter")
	}
}

// The non-emitter options are still populated, so a future refactor cannot
// satisfy the tests above by returning a bare store.Options{}.
func TestStoreOptionsCarriesLimits(t *testing.T) {
	cfg := &config.Config{MaxUploadBytes: 1234, PreviewMaxBytes: 567}

	opts := newStoreOptions(cfg, nil)
	if opts.MaxUploadBytes != 1234 || opts.PreviewMaxBytes != 567 {
		t.Fatalf("newStoreOptions dropped limits: got MaxUploadBytes=%d PreviewMaxBytes=%d",
			opts.MaxUploadBytes, opts.PreviewMaxBytes)
	}
}
