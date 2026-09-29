package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/cliclient"
	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/store"
)

// Governing: ADR-0025 (Actionable Validation Errors), SPEC-0019 VE-1..VE-6

// storelessServer serves the /v1 surface with no store behind it. Every
// request below is rejected by a validator before the store is reached, so
// these run without a database.
func storelessServer(t *testing.T, cfg Config) *httptest.Server {
	t.Helper()
	cfg.DevInsecureBearerAuth = true
	srv := httptest.NewServer(New(nil, nil, nil, cfg, slog.New(slog.NewTextHandler(io.Discard, nil))).Handler())
	t.Cleanup(srv.Close)
	return srv
}

func postCreate(t *testing.T, url string, headers map[string]string, body string) errorEnvelope {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer alice")
	req.Header.Set("Content-Type", "text/plain")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 400: %s", resp.StatusCode, b)
	}
	return decodeError(t, resp)
}

func valueOf(v errs.Violation) string {
	if v.Value == nil {
		return "<none>"
	}
	return *v.Value
}

// VE-1 scenario "TTL over the cap".
func TestViolationTTLOverCap(t *testing.T) {
	cfg := noRateLimit()
	cfg.MaxRequestedTTL = 30 * 24 * time.Hour
	srv := storelessServer(t, cfg)

	env := postCreate(t, srv.URL+"/v1/artifacts", map[string]string{"X-Cairn-Ttl-Seconds": "5184000"}, "hello")
	if env.Error.Code != errs.CodeValidation || len(env.Error.Violations) != 1 {
		t.Fatalf("error = %+v, want one validation violation", env.Error)
	}
	v := env.Error.Violations[0]
	if v.Field != "X-Cairn-Ttl-Seconds" || v.Location != errs.LocHeader || v.Reason != errs.ReasonExceedsMax ||
		v.Limit != float64(2592000) || v.Unit != errs.UnitSeconds || valueOf(v) != "5184000" {
		t.Fatalf("violation = %+v (value %s)", v, valueOf(v))
	}
	if want := "X-Cairn-Ttl-Seconds: " + v.Message; env.Error.Message != want {
		t.Fatalf("message = %q, want %q", env.Error.Message, want)
	}
}

// VE-1 scenario "Malformed TTL", plus the non-positive case.
func TestViolationMalformedTTL(t *testing.T) {
	srv := storelessServer(t, noRateLimit())
	for raw, reason := range map[string]errs.Reason{
		"7d":                    errs.ReasonInvalidFormat,
		"0":                     errs.ReasonNotPositive,
		"-5":                    errs.ReasonNotPositive,
		"-99999999999999999999": errs.ReasonNotPositive, // out of int64 range, still negative
	} {
		env := postCreate(t, srv.URL+"/v1/artifacts", map[string]string{"X-Cairn-Ttl-Seconds": raw}, "hello")
		v := env.Error.Violations[0]
		if v.Reason != reason || valueOf(v) != raw {
			t.Fatalf("%q: violation = %+v, want reason %s", raw, v, reason)
		}
		if !strings.Contains(v.Message, "must be a positive integer number of seconds") {
			t.Fatalf("%q: message = %q, want it to say what a TTL must be", raw, v.Message)
		}
	}
}

// VE-3 scenario "Tag echoed" and VE-4 scenario "Two bad tags".
func TestViolationTwoBadTags(t *testing.T) {
	srv := storelessServer(t, noRateLimit())
	env := postCreate(t, srv.URL+"/v1/artifacts?tag=Size:M", map[string]string{"X-Cairn-Tags": "lane m"}, "hello")
	vs := env.Error.Violations
	if len(vs) != 2 {
		t.Fatalf("violations = %+v, want 2", vs)
	}
	if vs[0].Field != "tag" || vs[0].Reason != errs.ReasonUppercase || valueOf(vs[0]) != "Size:M" || vs[0].Location != errs.LocQuery {
		t.Fatalf("first = %+v", vs[0])
	}
	if vs[1].Field != "tag" || vs[1].Reason != errs.ReasonInvalidCharset || valueOf(vs[1]) != "lane m" || vs[1].Location != errs.LocHeader {
		t.Fatalf("second = %+v", vs[1])
	}
	if !strings.HasPrefix(env.Error.Message, "2 problems: tag: ") {
		t.Fatalf("message = %q, want it to begin with 2 problems:", env.Error.Message)
	}
	if env.Error.Details != nil {
		t.Fatalf("details = %v, want none: violation fields are not mirrored into details (VE-5)", env.Error.Details)
	}
}

// A bad TTL and a bad tag on one request are reported together.
func TestViolationTTLAndTagTogether(t *testing.T) {
	srv := storelessServer(t, noRateLimit())
	env := postCreate(t, srv.URL+"/v1/artifacts", map[string]string{"X-Cairn-Ttl-Seconds": "7d", "X-Cairn-Tags": "Handoff"}, "x")
	if len(env.Error.Violations) != 2 || env.Error.Violations[0].Field != "X-Cairn-Ttl-Seconds" || env.Error.Violations[1].Field != "tag" {
		t.Fatalf("violations = %+v, want the TTL then the tag", env.Error.Violations)
	}
}

// Multipart: tags from the header and the form are reported together, before
// any file part is spooled.
func TestViolationMultipartTagsFromEverySource(t *testing.T) {
	srv := storelessServer(t, noRateLimit())
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("tag", "Lane:M,ok")
	_ = mw.WriteField("tag", strings.Repeat("x,", maxTagFieldBytes))
	fw, _ := mw.CreateFormFile("file", "a.md")
	_, _ = fw.Write([]byte("body"))
	_ = mw.Close()

	env := postCreate(t, srv.URL+"/v1/artifacts", map[string]string{"X-Cairn-Tags": "Handoff", "Content-Type": mw.FormDataContentType()}, buf.String())
	vs := env.Error.Violations
	if len(vs) != 3 {
		t.Fatalf("violations = %+v, want header, form and oversize-field violations", vs)
	}
	if vs[0].Location != errs.LocHeader || vs[1].Location != errs.LocForm || vs[1].Reason != errs.ReasonUppercase {
		t.Fatalf("violations = %+v", vs)
	}
	if vs[2].Reason != errs.ReasonTooLong || vs[2].Location != errs.LocForm || vs[2].Value != nil || vs[2].Limit != float64(maxTagFieldBytes) {
		t.Fatalf("oversize field = %+v, want too_long with the field cap and no echo", vs[2])
	}
}

// A multipart body can carry any number of tag fields, and the loop no longer
// stops at the first bad one, so the recorded violations must stay bounded
// however many bad fields arrive — oversize or malformed ones included.
func TestTagSetBoundsRecordedViolations(t *testing.T) {
	var ts tagSet
	oversize := strings.Repeat("x", maxTagFieldBytes+1)
	for i := 0; i < 10*errs.MaxViolations; i++ {
		ts.addFormField(strings.NewReader(oversize))
		ts.addFormField(strings.NewReader("Bad"))
	}
	if len(ts.bad) > errs.MaxViolations {
		t.Fatalf("recorded %d violations, want at most %d", len(ts.bad), errs.MaxViolations)
	}
	if got := len(errs.ViolationsOf(ts.err())); got != errs.MaxViolations {
		t.Fatalf("err carries %d violations, want %d", got, errs.MaxViolations)
	}
}

// VE-1 scenario "Oversize body": the 413 names the body and the cap.
func TestIntegrationViolationOversizeBody(t *testing.T) {
	cfg := noRateLimit()
	cfg.MaxUploadBytes = 1024
	srv := testServer(t, cfg, store.Options{MaxUploadBytes: 1024})

	resp := do(t, http.MethodPost, srv.URL+"/v1/artifacts", "alice", strings.NewReader(strings.Repeat("a", 4096)), "text/plain")
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
	env := decodeError(t, resp)
	if env.Error.Code != errs.CodePayloadTooLarge || len(env.Error.Violations) != 1 {
		t.Fatalf("error = %+v", env.Error)
	}
	v := env.Error.Violations[0]
	if v.Field != "body" || v.Location != errs.LocBody || v.Reason != errs.ReasonTooLarge ||
		v.Limit != float64(1024) || v.Unit != errs.UnitBytes || v.Value != nil {
		t.Fatalf("violation = %+v, want body too_large 1024 bytes with no echo", v)
	}
}

// The policy body's ttl_seconds reports the same reasons as the create header.
func TestIntegrationViolationPolicyTTL(t *testing.T) {
	cfg := policyTestConfig()
	cfg.MaxRequestedTTL = time.Hour
	srv := testServer(t, cfg, store.Options{MaxUploadBytes: 1 << 20})
	art := createArtifactAs(t, srv.URL, "owner-token", "x")

	for body, want := range map[string]errs.Reason{
		`{"ttl_seconds": 7200}`: errs.ReasonExceedsMax,
		`{"ttl_seconds": 0}`:    errs.ReasonNotPositive,
		`{"ttl_seconds": "1h"}`: errs.ReasonInvalidFormat,
	} {
		resp := do(t, http.MethodPatch, srv.URL+"/v1/artifacts/"+art.ID+"/ttl", "owner-token", strings.NewReader(body), "application/json")
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400", body, resp.StatusCode)
		}
		env := decodeError(t, resp)
		v := env.Error.Violations[0]
		if v.Reason != want {
			t.Fatalf("%s: violation = %+v, want %s", body, v, want)
		}
		if want == errs.ReasonExceedsMax && (v.Field != "ttl_seconds" || v.Location != errs.LocBody || v.Limit != float64(3600)) {
			t.Fatalf("%s: violation = %+v, want ttl_seconds over a 3600 s cap", body, v)
		}
		if env.Error.Details["id"] != art.ID || len(env.Error.Details) != 1 {
			t.Fatalf("%s: details = %v, want only the handler's id", body, env.Error.Details)
		}
	}
}

// renderError runs writeError against a recorder, with no request id so the
// body is deterministic.
func renderError(t *testing.T, err error, details map[string]string, log io.Writer) *httptest.ResponseRecorder {
	t.Helper()
	if log == nil {
		log = io.Discard
	}
	s := New(nil, nil, nil, Config{}, slog.New(slog.NewJSONHandler(log, nil)))
	rec := httptest.NewRecorder()
	s.writeError(rec, httptest.NewRequest(http.MethodGet, "/v1/artifacts/abc", nil), err, details)
	return rec
}

// VE-5: every code other than validation_failed and payload_too_large renders
// byte-identically to its pre-change form. The goldens are the envelopes as
// they were before violations existed; not-found stays uniform.
func TestNonValidationEnvelopesUnchanged(t *testing.T) {
	goldens := []struct {
		err     error
		details map[string]string
		want    string
	}{
		{errs.ErrNotFound, map[string]string{"id": "abc"}, `{"error":{"code":"not_found","message":"not found or expired","details":{"id":"abc"}}}`},
		{fmt.Errorf("expired: %w", errs.ErrNotFound), nil, `{"error":{"code":"not_found","message":"not found or expired"}}`},
		{errs.ErrUnauthorized, nil, `{"error":{"code":"unauthorized","message":"authentication required"}}`},
		{errs.ErrForbidden, map[string]string{"id": "abc"}, `{"error":{"code":"forbidden","message":"forbidden","details":{"id":"abc"}}}`},
		{errs.ErrConflict, nil, `{"error":{"code":"conflict","message":"conflict"}}`},
		{errors.New("db down"), nil, `{"error":{"code":"internal","message":"internal error"}}`},
	}
	for _, g := range goldens {
		rec := renderError(t, g.err, g.details, nil)
		if got := strings.TrimSpace(rec.Body.String()); got != g.want {
			t.Fatalf("%v:\n got  %s\n want %s", g.err, got, g.want)
		}
	}

	// And with a request id, the not-found body differs from the golden only
	// by that id.
	s := New(nil, nil, nil, Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/artifacts/abc", nil)
	req = req.WithContext(context.WithValue(req.Context(), middleware.RequestIDKey, "req-1"))
	s.writeError(rec, req, errs.ErrNotFound, map[string]string{"id": "abc"})
	if got, want := strings.TrimSpace(rec.Body.String()), `{"error":{"code":"not_found","message":"not found or expired","details":{"id":"abc"},"request_id":"req-1"}}`; got != want {
		t.Fatalf("not-found with request id:\n got  %s\n want %s", got, want)
	}
}

// VE-6 scenario "Unmigrated site still renders".
func TestBareValidationRendersGenericViolation(t *testing.T) {
	rec := renderError(t, fmt.Errorf("create: %w", errs.Validationf("multipart: malformed body")), nil, nil)
	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest || env.Error.Message != "the request was invalid" || len(env.Error.Violations) != 1 {
		t.Fatalf("status %d, envelope %+v", rec.Code, env.Error)
	}
	if v := env.Error.Violations[0]; v.Field != "request" || v.Reason != errs.ReasonInvalid || v.Message != "the request was invalid" {
		t.Fatalf("violation = %+v, want the generic one", v)
	}
	if strings.Contains(rec.Body.String(), "multipart") {
		t.Fatalf("the internal message leaked into the response: %s", rec.Body)
	}

	rec = renderError(t, errs.ErrTooLarge, nil, nil)
	if !strings.Contains(rec.Body.String(), `"violations":[{"field":"body","location":"body","reason":"too_large"`) {
		t.Fatalf("a bare payload_too_large must still name the body: %s", rec.Body)
	}
}

// VE-5 scenario "Old client decodes": the CLI's decoder, which reads details
// as map[string]string and ignores unknown keys, decodes a new response and
// shows the new top-level message.
func TestOldClientDecodesViolations(t *testing.T) {
	err := errs.Join(
		artifact.CheckTag("Size:M", "tag", errs.LocHeader),
		artifact.CheckTag("lane m", "tag", errs.LocHeader),
	)
	rec := renderError(t, err, map[string]string{"id": "abc"}, nil)
	apiErr := cliclient.DecodeError(rec.Result())
	var ae *cliclient.APIError
	if !errors.As(apiErr, &ae) {
		t.Fatalf("DecodeError = %v, want an *APIError", apiErr)
	}
	if ae.Code != cliclient.CodeValidation || !strings.HasPrefix(ae.Message, "2 problems: tag: ") || ae.Details["id"] != "abc" {
		t.Fatalf("decoded = %+v", ae)
	}
}

// Error Handling Standards scenario "Log keeps the internal detail".
func TestViolationLogKeepsInternalError(t *testing.T) {
	var log bytes.Buffer
	_, inv := checkTTLSeconds(ttlHeader, errs.LocHeader, "5184000", 30*24*time.Hour)
	rec := renderError(t, fmt.Errorf("create single: %w", inv), nil, &log)
	if !strings.Contains(log.String(), "create single: ") || !strings.Contains(log.String(), "exceeds the maximum") {
		t.Fatalf("log = %s, want the wrapped internal error", log.String())
	}
	if strings.Contains(rec.Body.String(), "create single") {
		t.Fatalf("the internal wrap leaked into the response: %s", rec.Body)
	}
}

// guardSite is one client-reachable validation site. trigger drives it with
// input it must reject.
type guardSite struct {
	name    string
	trigger func() error
}

// guardSiteSources feeds the SPEC-0019 VE-6 migration guard. Each migration
// registers its own list from an init func in its own test file, so stories
// that land in parallel never edit one shared list. A listed site can never
// regress.
var guardSiteSources = []func(t *testing.T) []guardSite{createPathTTLAndTagSites}

// createPathTTLAndTagSites are the first migrated sites (#275): TTL, tags,
// the policy body and the upload cap.
func createPathTTLAndTagSites(t *testing.T) []guardSite {
	ttlReq := func(v string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/v1/artifacts", nil)
		r.Header.Set(ttlHeader, v)
		return r
	}
	cfg := Config{MaxRequestedTTL: time.Hour}
	ttl := func(v string) func() error {
		return func() error { _, err := requestedTTL(ttlReq(v), cfg); return err }
	}
	tagReq := func(query, header string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/v1/artifacts?"+query, nil)
		if header != "" {
			r.Header.Set(tagHeader, header)
		}
		return r
	}
	s := New(nil, nil, nil, Config{MaxRequestedTTL: time.Hour, MaxUploadBytes: 10}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	many := make([]string, artifact.MaxTags+1)
	for i := range many {
		many[i] = fmt.Sprintf("t%d", i)
	}
	return []guardSite{
		{"ttl header: malformed", ttl("7d")},
		{"ttl header: not positive", ttl("0")},
		{"ttl header: over the cap", ttl("7200")},
		{"ttl header: out of int64 range", ttl("99999999999999999999")},
		{"ttl policy body: ttl_seconds", func() error {
			_, inv := checkTTLSeconds("ttl_seconds", errs.LocBody, "7200", time.Hour)
			return inv
		}},
		{"policy body: not JSON", func() error {
			r := httptest.NewRequest(http.MethodPatch, "/", strings.NewReader("{"))
			return s.decodePolicyBody(httptest.NewRecorder(), r, &ttlRequest{})
		}},
		{"tag: query", func() error { var ts tagSet; return ts.addQuery(tagReq("tag=Size:M", "")) }},
		{"tag: header", func() error { var ts tagSet; return ts.addFromRequest(tagReq("", "lane m")) }},
		{"tag: too many", func() error { var ts tagSet; return ts.addFromRequest(tagReq("", strings.Join(many, ","))) }},
		{"tag: form field", func() error { var ts tagSet; ts.addFormField(strings.NewReader("Handoff")); return ts.err() }},
		{"tag: oversize form field", func() error {
			var ts tagSet
			ts.addFormField(strings.NewReader(strings.Repeat("x", maxTagFieldBytes+1)))
			return ts.err()
		}},
		{"tag: artifact.ValidateTag", func() error { return artifact.ValidateTag("") }},
		{"tag: artifact.NormalizeTags (MCP tags[n])", func() error { _, err := artifact.NormalizeTags([]string{"Handoff"}); return err }},
		{"tag: artifact.NormalizeTags too many", func() error { _, err := artifact.NormalizeTags(many); return err }},
		{"upload: body over the cap", func() error { return s.mapUploadErr(&http.MaxBytesError{Limit: 10}) }},
		{"upload: store limit", func() error { return s.mapUploadErr(fmt.Errorf("stream: %w", errs.ErrTooLarge)) }},
	}
}

// TestMigrationGuard is SPEC-0019 VE-6's guard: it fails, naming the site, if
// any listed validator returns a bare validation error.
func TestMigrationGuard(t *testing.T) {
	var sites []guardSite
	for _, src := range guardSiteSources {
		sites = append(sites, src(t)...)
	}
	for _, site := range sites {
		err := site.trigger()
		if err == nil {
			t.Errorf("%s: accepted input it must reject", site.name)
			continue
		}
		vs := errs.ViolationsOf(err)
		if len(vs) == 0 {
			t.Errorf("%s: returns a bare validation error (%v); it must return typed violations", site.name, err)
			continue
		}
		for _, v := range vs {
			if v.Reason == errs.ReasonInvalid || v.Field == "" || v.Location == "" || v.Message == "" {
				t.Errorf("%s: incomplete or unmigrated violation %+v", site.name, v)
			}
		}
	}
}
