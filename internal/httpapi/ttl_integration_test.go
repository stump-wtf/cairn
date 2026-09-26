package httpapi

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/cairn/internal/store"
)

// doWithTTLHeader is `do` plus an optional X-Cairn-Ttl-Seconds header, for
// exercising the CLI's `--ttl` request path (SPEC-0008) end to end.
func doWithTTLHeader(t *testing.T, method, url, token, ttlSeconds string, body *bytes.Buffer, contentType string) *http.Response {
	t.Helper()
	var reqBody *bytes.Reader
	if body != nil {
		reqBody = bytes.NewReader(body.Bytes())
	}
	var req *http.Request
	var err error
	if reqBody != nil {
		req, err = http.NewRequest(method, url, reqBody)
	} else {
		req, err = http.NewRequest(method, url, nil)
	}
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if ttlSeconds != "" {
		req.Header.Set("X-Cairn-Ttl-Seconds", ttlSeconds)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do %s %s: %v", method, url, err)
	}
	return resp
}

// TestIntegrationRequestedTTLHonoredWithinCap exercises the CLI's `--ttl`
// path (SPEC-0008 "Data the CLI shows but never decides" — the CLI forwards
// a request, the server decides): a valid, in-bounds X-Cairn-Ttl-Seconds
// overrides Config.DefaultTTL on both the single-artifact and bundle create
// paths.
func TestIntegrationRequestedTTLHonoredWithinCap(t *testing.T) {
	cfg := noRateLimit()
	cfg.MaxRequestedTTL = 48 * time.Hour
	srv := testServer(t, cfg, store.Options{MaxUploadBytes: 1 << 20})

	before := time.Now()
	resp := doWithTTLHeader(t, http.MethodPost, srv.URL+"/v1/artifacts", "alice", "3600",
		bytes.NewBufferString("hello"), "text/plain")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}
	art := decodeArtifact(t, resp)

	wantMin := before.Add(3500 * time.Second)
	wantMax := before.Add(3700 * time.Second)
	if art.ExpiresAt.Before(wantMin) || art.ExpiresAt.After(wantMax) {
		t.Fatalf("expires_at = %s, want ~1h from now (between %s and %s)", art.ExpiresAt, wantMin, wantMax)
	}
}

// TestIntegrationRequestedTTLOverCapRejected verifies the server remains
// authoritative: a request over Config.MaxRequestedTTL is validation_failed,
// never silently clamped (ADR-0007 "owner-adjustable ... subject to any
// workspace cap").
func TestIntegrationRequestedTTLOverCapRejected(t *testing.T) {
	cfg := noRateLimit()
	cfg.MaxRequestedTTL = time.Hour
	srv := testServer(t, cfg, store.Options{MaxUploadBytes: 1 << 20})

	resp := doWithTTLHeader(t, http.MethodPost, srv.URL+"/v1/artifacts", "alice", "7200",
		bytes.NewBufferString("hello"), "text/plain")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if env := decodeError(t, resp); env.Error.Code != "validation_failed" {
		t.Fatalf("code = %q, want validation_failed", env.Error.Code)
	}
}

// TestIntegrationRequestedTTLMalformedRejected verifies a non-numeric or
// non-positive header is rejected before ever reaching the store, rather
// than silently falling back to the default.
func TestIntegrationRequestedTTLMalformedRejected(t *testing.T) {
	srv := testServer(t, noRateLimit(), store.Options{MaxUploadBytes: 1 << 20})

	for _, bad := range []string{"not-a-number", "-1", "0"} {
		resp := doWithTTLHeader(t, http.MethodPost, srv.URL+"/v1/artifacts", "alice", bad,
			bytes.NewBufferString("hello"), "text/plain")
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("X-Cairn-Ttl-Seconds=%q: status = %d, want 400", bad, resp.StatusCode)
		}
		if env := decodeError(t, resp); env.Error.Code != "validation_failed" {
			t.Fatalf("X-Cairn-Ttl-Seconds=%q: code = %q, want validation_failed", bad, env.Error.Code)
		}
	}
}

// TestIntegrationBundleRequestedTTLHonored is the bundle-path counterpart:
// `cairn add --ttl` sends the same header on the multipart create request.
func TestIntegrationBundleRequestedTTLHonored(t *testing.T) {
	cfg := noRateLimit()
	cfg.MaxRequestedTTL = 48 * time.Hour
	srv := testServer(t, cfg, store.Options{MaxUploadBytes: 1 << 20})

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("title", "bundle with ttl")
	for _, f := range []struct{ name, body string }{{"a.txt", "AAA"}, {"b.txt", "BBBB"}} {
		fw, err := mw.CreateFormFile("file", f.name)
		if err != nil {
			t.Fatalf("form file: %v", err)
		}
		fw.Write([]byte(f.body))
	}
	mw.Close()

	before := time.Now()
	resp := doWithTTLHeader(t, http.MethodPost, srv.URL+"/v1/artifacts", "alice", "7200", &buf, mw.FormDataContentType())
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}
	art := decodeArtifact(t, resp)

	wantMin := before.Add(7100 * time.Second)
	wantMax := before.Add(7300 * time.Second)
	if art.ExpiresAt.Before(wantMin) || art.ExpiresAt.After(wantMax) {
		t.Fatalf("expires_at = %s, want ~2h from now", art.ExpiresAt)
	}
}

// TestIntegrationRequestedTTLAbsentUsesDefault confirms the header is
// genuinely optional: no header at all still creates successfully with the
// server's default TTL (unchanged existing behavior).
func TestIntegrationRequestedTTLAbsentUsesDefault(t *testing.T) {
	cfg := noRateLimit()
	srv := testServer(t, cfg, store.Options{MaxUploadBytes: 1 << 20})

	before := time.Now()
	resp := do(t, http.MethodPost, srv.URL+"/v1/artifacts", "alice", strings.NewReader("hello"), "text/plain")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}
	art := decodeArtifact(t, resp)
	want := before.Add(cfg.DefaultTTL)
	if art.ExpiresAt.Before(want.Add(-time.Minute)) || art.ExpiresAt.After(want.Add(time.Minute)) {
		t.Fatalf("expires_at = %s, want ~DefaultTTL from now (%s)", art.ExpiresAt, want)
	}
}
