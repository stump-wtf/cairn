package webhook

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/stump-wtf/cairn/internal/redact"
)

// Unit tests for the capture-time credential scan (redaction.go). Every
// credential-shaped fixture is assembled at run time from split literals, so
// no contiguous credential exists in the repository.
//
// Governing: ADR-0023, SPEC-0017 RD-4, RD-6

var (
	scannerOnce sync.Once
	scannerVal  *redact.Scanner
	scannerErr  error
)

// testScanner is a real scanner with the production defaults, built once per
// test binary because building the gitleaks detector is not free.
func testScanner(t testing.TB) *redact.Scanner {
	t.Helper()
	scannerOnce.Do(func() { scannerVal, scannerErr = redact.New(redact.Config{}) })
	if scannerErr != nil {
		t.Fatalf("redact.New: %v", scannerErr)
	}
	return scannerVal
}

// plantedToken is a GitHub personal access token shape (ghp_ plus 36
// alphanumerics) the scanner detects, different for each seed.
func plantedToken(seed int) string {
	const alnum = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	var b strings.Builder
	for i := 0; i < 36; i++ {
		b.WriteByte(alnum[(seed*11+i*7)%len(alnum)])
	}
	return "gh" + "p_" + b.String()
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// bufLogger logs to buf; tests read it back to check what a log line names.
func bufLogger(buf *bytes.Buffer) *slog.Logger { return slog.New(slog.NewTextHandler(buf, nil)) }

// failingScanner fails every scan the way a detector panic or a cancelled
// scan does: an internal error wrapping redact.ErrScanFailed.
type failingScanner struct{}

func (failingScanner) Text(_ context.Context, field, _ string, _ redact.Mode) (string, redact.Outcome, error) {
	return "", redact.Outcome{}, fmt.Errorf("redact %s: %w: boom", field, redact.ErrScanFailed)
}

// unmaskableScanner reports a finding it cannot locate, as redact does for a
// decoded secret whose encoded form it cannot find.
type unmaskableScanner struct{}

func (unmaskableScanner) Text(_ context.Context, field, _ string, _ redact.Mode) (string, redact.Outcome, error) {
	o := redact.Outcome{Count: 1, Rules: map[string]int{"github-pat": 1}, Findings: []redact.Finding{{Rule: "github-pat", Line: 1}}}
	return "", o, &redact.Rejection{Field: field, Reason: redact.ReasonSecretDetected, Findings: o.Findings, Unmaskable: true}
}

// TestScrubMasksEveryField runs the real scanner over a capture with a token in
// its query, a header and its body, and checks each comes back masked, the
// Authorization header keeps its name and scheme, and the outcome counts every
// value.
func TestScrubMasksEveryField(t *testing.T) {
	tq, th, tb, ta := plantedToken(1), plantedToken(2), plantedToken(3), plantedToken(4)
	s := &Service{scanner: testScanner(t), log: discardLogger()}
	in := CaptureInput{
		Query:       "event=push&sig=" + tq,
		ContentType: "application/json",
		Body:        []byte(`{"note":"deploy with ` + tb + `"}`),
	}
	headers := sanitizeHeaders(map[string][]string{
		"Authorization": {"Bearer " + ta},
		"X-Forwarded":   {"via " + th},
		"Content-Type":  {"application/json"},
	})
	got := s.scrub(context.Background(), "ep1", in, headers)

	all := got.query + fmt.Sprint(got.headers) + string(got.body)
	for _, tok := range []string{tq, th, tb, ta} {
		if strings.Contains(all, tok) {
			t.Fatalf("a planted token survived scrub: %q", all)
		}
	}
	if !strings.Contains(got.query, redact.Mask) || !strings.Contains(string(got.body), redact.Mask) {
		t.Fatalf("query %q or body %q not masked", got.query, got.body)
	}
	if want := []string{"Bearer " + redact.Mask}; !reflect.DeepEqual(got.headers["authorization"], want) {
		t.Fatalf("authorization = %q, want %q", got.headers["authorization"], want)
	}
	if want := []string{"application/json"}; !reflect.DeepEqual(got.headers["content-type"], want) {
		t.Fatalf("content-type = %q, want it untouched", got.headers["content-type"])
	}
	if got.summary.Status != redact.StatusMasked || got.summary.Count < 4 {
		t.Fatalf("summary = %+v, want masked with at least 4 values", got.summary)
	}
	if len(got.withheld) != 0 {
		t.Fatalf("withheld = %v, want none", got.withheld)
	}
}

// TestScrubCleanCapture: nothing to find is "clean", not "unscanned", and the
// fields are unchanged.
func TestScrubCleanCapture(t *testing.T) {
	s := &Service{scanner: testScanner(t), log: discardLogger()}
	in := CaptureInput{Query: "a=1", ContentType: "text/plain", Body: []byte("hello")}
	got := s.scrub(context.Background(), "ep1", in, map[string][]string{"x-a": {"b"}})
	if got.query != "a=1" || string(got.body) != "hello" || got.headers["x-a"][0] != "b" {
		t.Fatalf("clean capture changed: %+v", got)
	}
	if got.summary.Status != redact.StatusClean || got.summary.Count != 0 || got.summary.Rules == nil {
		t.Fatalf("summary = %+v, want clean, 0, and a non-nil rules map", got.summary)
	}
}

// TestScrubScanFailureWithholdsEveryField: when the scan fails, nothing raw is
// kept, the sender is not refused (scrub has no error to return), and the
// fields are named.
func TestScrubScanFailureWithholdsEveryField(t *testing.T) {
	tok := plantedToken(5)
	logs := &bytes.Buffer{}
	s := &Service{scanner: failingScanner{}, log: bufLogger(logs)}
	in := CaptureInput{Query: "k=" + tok, ContentType: "text/plain", Body: []byte("x " + tok)}
	got := s.scrub(context.Background(), "ep1", in, map[string][]string{"x-key": {tok}, "authorization": {"Basic " + tok}})

	all := got.query + fmt.Sprint(got.headers) + string(got.body) + logs.String()
	if strings.Contains(all, tok) {
		t.Fatalf("a raw value survived a failed scan: %q", all)
	}
	if want := []string{FieldQuery, FieldHeaders, FieldBody}; !reflect.DeepEqual(got.withheld, want) {
		t.Fatalf("withheld = %v, want %v", got.withheld, want)
	}
	if !strings.HasPrefix(string(got.body), redact.Mask) || !strings.Contains(string(got.body), "scan failed") {
		t.Fatalf("body = %q, want the withheld notice", got.body)
	}
	if got.headers["x-key"][0] != redact.Mask || got.headers["authorization"][0] != redact.Mask {
		t.Fatalf("headers = %v, want names kept and values masked", got.headers)
	}
	if got.summary.Status != redact.StatusUnscanned {
		t.Fatalf("status = %q, want unscanned: the withheld fields were never scanned", got.summary.Status)
	}
	if !strings.Contains(logs.String(), "reason=scan_failed") || !strings.Contains(logs.String(), "field=body") {
		t.Fatalf("log = %q, want the field and reason", logs.String())
	}
}

// TestScrubUnmaskableWithholds: a finding the masker cannot locate withholds
// the field and still records the count and rule.
func TestScrubUnmaskableWithholds(t *testing.T) {
	logs := &bytes.Buffer{}
	s := &Service{scanner: unmaskableScanner{}, log: bufLogger(logs)}
	got := s.scrub(context.Background(), "ep1", CaptureInput{ContentType: "text/plain", Body: []byte("opaque")}, nil)
	if !reflect.DeepEqual(got.withheld, []string{FieldBody}) || !strings.Contains(string(got.body), "could not be masked") {
		t.Fatalf("got withheld %v body %q, want the body withheld", got.withheld, got.body)
	}
	if got.summary.Status != redact.StatusMasked || got.summary.Count != 1 || got.summary.Rules["github-pat"] != 1 {
		t.Fatalf("summary = %+v, want masked, 1, github-pat", got.summary)
	}
	if !strings.Contains(logs.String(), "reason=secret_unmaskable") || !strings.Contains(logs.String(), "github-pat") {
		t.Fatalf("log = %q, want the reason and rule", logs.String())
	}
}

// TestScrubOversize: over the scan cap, the default policy withholds the body
// and the store_unscanned policy stores it with a WARN naming the size only.
func TestScrubOversize(t *testing.T) {
	body := []byte(strings.Repeat("plain words ", 20))
	for _, tc := range []struct {
		policy   redact.OversizePolicy
		withheld bool
	}{
		{redact.OversizeReject, true},
		{redact.OversizeStoreUnscanned, false},
	} {
		t.Run(string(tc.policy), func(t *testing.T) {
			sc, err := redact.New(redact.Config{MaxScanBytes: 64, Oversize: tc.policy})
			if err != nil {
				t.Fatal(err)
			}
			logs := &bytes.Buffer{}
			s := &Service{scanner: sc, log: bufLogger(logs)}
			got := s.scrub(context.Background(), "ep1", CaptureInput{ContentType: "text/plain", Body: body}, nil)
			if got.summary.Status != redact.StatusNotScannedOversize {
				t.Fatalf("status = %q, want not_scanned_oversize", got.summary.Status)
			}
			if tc.withheld {
				if !reflect.DeepEqual(got.withheld, []string{FieldBody}) || bytes.Equal(got.body, body) {
					t.Fatalf("withheld %v body %q, want the body withheld", got.withheld, got.body)
				}
				return
			}
			if len(got.withheld) != 0 || !bytes.Equal(got.body, body) {
				t.Fatalf("withheld %v, want the body stored as sent", got.withheld)
			}
			line := logs.String()
			if !strings.Contains(line, "level=WARN") || !strings.Contains(line, "CAIRN_REDACTION_OVERSIZE") ||
				!strings.Contains(line, fmt.Sprintf("size=%d", len(body))) || strings.Contains(line, "plain words") {
				t.Fatalf("log = %q, want a WARN with the size and no content", line)
			}
		})
	}
}

// TestScrubBinaryBodyNotScanned (RD-6 "Real image is not scanned"): a body
// with a binary signature is stored unchanged and flagged, even if it happens
// to contain token-shaped bytes; a body declared as an image that is really
// text is scanned ("Declared image that is text is scanned").
func TestScrubBinaryBodyNotScanned(t *testing.T) {
	s := &Service{scanner: testScanner(t), log: discardLogger()}
	png := append([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"), []byte(plantedToken(6))...)
	got := s.scrub(context.Background(), "ep1", CaptureInput{ContentType: "image/png", Body: png}, nil)
	if !bytes.Equal(got.body, png) || got.summary.Status != redact.StatusNotScannedBinary {
		t.Fatalf("binary body: status %q, changed=%v; want unchanged and not_scanned_binary", got.summary.Status, !bytes.Equal(got.body, png))
	}

	tok := plantedToken(7)
	got = s.scrub(context.Background(), "ep1", CaptureInput{ContentType: "image/png", Body: []byte("text " + tok)}, nil)
	if strings.Contains(string(got.body), tok) || got.summary.Status != redact.StatusMasked {
		t.Fatalf("text declared as image/png: status %q body %q, want it scanned and masked", got.summary.Status, got.body)
	}
}

// TestScrubWithoutScanner: a Service built with no scanner stores fields as
// sent and records "unscanned", but header hygiene still masks Authorization.
func TestScrubWithoutScanner(t *testing.T) {
	s := &Service{log: discardLogger()}
	got := s.scrub(context.Background(), "ep1", CaptureInput{Query: "q=1", Body: []byte("b")},
		map[string][]string{"authorization": {"Bearer abc"}})
	if !reflect.DeepEqual(got.summary, redact.Summary{}) || got.query != "q=1" || string(got.body) != "b" {
		t.Fatalf("got %+v, want fields unchanged and the zero (unscanned) summary", got)
	}
	if got.headers["authorization"][0] != "Bearer "+redact.Mask {
		t.Fatalf("authorization = %q, want it masked without a scanner too", got.headers["authorization"])
	}
}

func TestIsTextBody(t *testing.T) {
	long := bytes.Repeat([]byte("é"), sniffBytes) // 2-byte runes: the 8 KiB cut lands mid-rune or not
	for _, tc := range []struct {
		name string
		ct   string
		body []byte
		want bool
	}{
		{"json type", "application/json; charset=utf-8", []byte{0x89, 'P', 'N', 'G'}, true},
		{"vendor json suffix", "application/vnd.api+json", []byte("{}"), true},
		{"text type", "text/csv", []byte("a,b"), true},
		{"utf8 undeclared", "", []byte("hello"), true},
		{"utf8 cut mid-rune", "application/octet-stream", append([]byte("x"), long...), true},
		{"png", "image/png", []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"), false},
		{"gzip", "", []byte("\x1f\x8b\x08\x00\x00\x00\x00\x00"), false},
		{"unknown bytes are scanned", "application/octet-stream", []byte{0xff, 0x00, 0xfe, 0x01}, true},
	} {
		if got := isTextBody(tc.ct, tc.body); got != tc.want {
			t.Errorf("%s: isTextBody = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestHeadersRoundTrip(t *testing.T) {
	h := map[string][]string{"b": {"2", "3"}, "a": {"1"}}
	text, names := serializeHeaders(h)
	if text != "a: 1\nb: 2\nb: 3" {
		t.Fatalf("serialized = %q", text)
	}
	got, ok := parseHeaders(text, names)
	if !ok || !reflect.DeepEqual(got, h) {
		t.Fatalf("round trip = %v, %v", got, ok)
	}
	// A mask that crossed a line, or ate a label, cannot be parsed back.
	for _, bad := range []string{"a: 1\nb: [REDACTED]", "a: 1\n[REDACTED]\nb: 3"} {
		if _, ok := parseHeaders(bad, names); ok {
			t.Errorf("parseHeaders(%q) ok, want refused", bad)
		}
	}
}

func TestMaskCredentialHeaders(t *testing.T) {
	for in, want := range map[string]string{
		"Bearer abc":                "Bearer [REDACTED]",
		"Bearer [REDACTED]":         "Bearer [REDACTED]",
		"Bearer [REDACTED]xyz":      "Bearer [REDACTED]",
		"AWS4-HMAC-SHA256 Cred=a/b": "AWS4-HMAC-SHA256 [REDACTED]",
		"rawtokenvalue":             "[REDACTED]",
		"1bad scheme":               "[REDACTED]",
		"":                          "[REDACTED]",
	} {
		h := maskCredentialHeaders(map[string][]string{"authorization": {in}, "x-other": {in}})
		if h["authorization"][0] != want || h["x-other"][0] != in {
			t.Errorf("mask(%q) = %q (x-other %q), want %q and x-other untouched", in, h["authorization"][0], h["x-other"][0], want)
		}
	}
}
