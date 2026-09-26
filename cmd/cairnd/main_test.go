package main

import (
	"bytes"
	"encoding/base64"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/stump-wtf/cairn/internal/config"
	"github.com/stump-wtf/cairn/internal/outboundhook"
)

// Governing: ADR-0017 (Outbound Webhooks), SPEC-0012 REQ "Event Emission on
// Artifact Creation"; SPEC-0023 REQ "Owned Outbound Subscriptions", REQ
// "Subscription Target Safety"

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
func TestStoreOptionsEmitterIsNilInterfaceForANilEmitter(t *testing.T) {
	cfg := &config.Config{}
	var emitter *outboundhook.Emitter
	opts := newStoreOptions(cfg, emitter)
	if opts.Emitter != nil {
		t.Fatal("store.Options.Emitter is a non-nil interface for a nil emitter: " +
			"store's nil guard will not fire, and every artifact create will panic " +
			"on a nil receiver after committing the row (cairn#201)")
	}
}

// main() always builds an emitter now — targets are owned subscriptions, not
// configuration — and it must reach store, or no subscription ever receives
// anything.
func TestStoreOptionsCarriesTheEmitter(t *testing.T) {
	cfg := &config.Config{BaseURL: "https://cairn.example"}
	subs, err := newSubscriptions(cfg, nil, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	emitter := newOutboundEmitter(cfg, subs, quietLogger())
	if emitter == nil {
		t.Fatal("newOutboundEmitter = nil, want an emitter")
	}
	if opts := newStoreOptions(cfg, emitter); opts.Emitter == nil {
		t.Fatal("the emitter did not reach store.Options.Emitter")
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

// TestProductionPolicyNeverAdmitsPrivateAddresses: the address permit on
// subscription.Policy exists for tests; the policy cairnd builds leaves it
// unset, so loopback, private and link-local targets stay refused.
func TestProductionPolicyNeverAdmitsPrivateAddresses(t *testing.T) {
	for _, allow := range []bool{false, true} {
		p := newSubscriptionPolicy(&config.Config{OutboundAllowHTTP: allow})
		if p.PermitAddr != nil || p.Resolver != nil {
			t.Fatal("cairnd's subscription policy overrides the address check or the resolver")
		}
		if p.AllowHTTP != allow {
			t.Fatalf("AllowHTTP = %v, want %v", p.AllowHTTP, allow)
		}
	}
}

// TestAllowHTTPWarnsAtStartup: CAIRN_OUTBOUND_ALLOW_HTTP is risky, so turning
// it on is loud (REQ "Subscription Target Safety").
func TestAllowHTTPWarnsAtStartup(t *testing.T) {
	for _, allow := range []bool{false, true} {
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, nil))
		if _, err := newSubscriptions(&config.Config{OutboundAllowHTTP: allow}, nil, logger); err != nil {
			t.Fatal(err)
		}
		warned := strings.Contains(buf.String(), "level=WARN") && strings.Contains(buf.String(), "CAIRN_OUTBOUND_ALLOW_HTTP")
		if warned != allow {
			t.Fatalf("allow_http=%v: warned=%v; log:\n%s", allow, warned, buf.String())
		}
	}
}

// TestEncryptionKey: a malformed key fails boot without echoing it; an
// unset key starts, with subscriptions unavailable and said so; a good key
// makes them available.
func TestEncryptionKey(t *testing.T) {
	bad := "this-is-not-a-32-byte-base64-key"
	if _, err := newSubscriptions(&config.Config{EncryptionKeyRaw: bad}, nil, quietLogger()); err == nil ||
		strings.Contains(err.Error(), bad) || !strings.Contains(err.Error(), "CAIRN_ENCRYPTION_KEY") {
		t.Fatalf("malformed key: err = %v; want an error naming the variable, not the value", err)
	}

	var buf bytes.Buffer
	subs, err := newSubscriptions(&config.Config{}, nil, slog.New(slog.NewTextHandler(&buf, nil)))
	if err != nil || subs.Available() {
		t.Fatalf("unset key: available=%v err=%v; want a start with subscriptions unavailable", subs.Available(), err)
	}
	if !strings.Contains(buf.String(), "CAIRN_ENCRYPTION_KEY is unset") {
		t.Fatalf("unset key was not reported; log:\n%s", buf.String())
	}

	good := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{4}, 32))
	buf.Reset()
	subs, err = newSubscriptions(&config.Config{EncryptionKeyRaw: good}, nil, slog.New(slog.NewTextHandler(&buf, nil)))
	if err != nil || !subs.Available() {
		t.Fatalf("good key: available=%v err=%v", subs.Available(), err)
	}
	if strings.Contains(buf.String(), good) {
		t.Fatal("the key was logged")
	}
}
