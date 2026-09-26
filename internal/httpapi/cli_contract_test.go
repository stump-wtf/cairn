package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/cairn/internal/cliclient"
	"github.com/stump-wtf/cairn/internal/errs"
)

// Governing: ADR-0025, SPEC-0019 VE-1, VE-8

// The CLI's violation tests answer with hand-written envelopes, because the
// CLI never imports the server (ADR-0003). This test is the other half of that
// contract: the real server's response, decoded by the CLI's own decoder,
// yields exactly the fields the CLI renders from. A renamed key or a changed
// limit type on either side fails here rather than silently degrading every
// violation line to the bare top-level message.

func cliDecode(t *testing.T, resp *http.Response) *cliclient.APIError {
	t.Helper()
	var ae *cliclient.APIError
	if err := cliclient.DecodeError(resp); !errors.As(err, &ae) {
		t.Fatalf("DecodeError = %v, want an *APIError", err)
	}
	return ae
}

// TestCLIDecodesServerTTLViolation is VE-8's scenario from the wire up: a
// 30-day server rejecting 5184000 seconds decodes into the violation the CLI
// renders as "--ttl 60d exceeds the server's maximum of 30d".
func TestCLIDecodesServerTTLViolation(t *testing.T) {
	cfg := noRateLimit()
	cfg.MaxRequestedTTL = 30 * 24 * time.Hour
	srv := storelessServer(t, cfg)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/artifacts", strings.NewReader("hello"))
	req.Header.Set("Authorization", "Bearer alice")
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("X-Cairn-Ttl-Seconds", "5184000")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	ae := cliDecode(t, resp)
	if len(ae.Violations) != 1 {
		t.Fatalf("violations = %+v, want one", ae.Violations)
	}
	v := ae.Violations[0]
	limit, isInt := v.Limit.Int()
	if v.Field != "X-Cairn-Ttl-Seconds" || v.Reason != cliclient.ReasonExceedsMax ||
		!isInt || limit != 2592000 || v.Unit != cliclient.UnitSeconds ||
		v.Value == nil || *v.Value != "5184000" {
		t.Fatalf("violation = %+v (limit %v), want X-Cairn-Ttl-Seconds exceeds_max 2592000 seconds, value 5184000", v, v.Limit)
	}
}

// TestCLIDecodesServerTagViolations: the tag violations decode with the value
// the CLI shows beside --tag, and each server message starts with that value
// quoted exactly as the CLI strips it (strconv.Quote plus a space), so the
// value is not printed twice.
func TestCLIDecodesServerTagViolations(t *testing.T) {
	srv := storelessServer(t, noRateLimit())

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/artifacts", strings.NewReader("x"))
	req.Header.Set("Authorization", "Bearer alice")
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("X-Cairn-Tags", "Size:M,lane m")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	ae := cliDecode(t, resp)
	if len(ae.Violations) != 2 {
		t.Fatalf("violations = %+v, want two", ae.Violations)
	}
	for i, want := range []struct{ value, reason string }{
		{"Size:M", "uppercase"},
		{"lane m", "invalid_charset"},
	} {
		v := ae.Violations[i]
		if v.Field != "tag" || v.Reason != want.reason || v.Value == nil || *v.Value != want.value {
			t.Fatalf("violation %d = %+v, want tag %s %q", i, v, want.reason, want.value)
		}
		if !strings.HasPrefix(v.Message, strconv.Quote(want.value)+" ") {
			t.Fatalf("message %q does not start with the quoted value the CLI strips", v.Message)
		}
	}
}

// TestCLIDecodesSecretLocation: secret_detected's reason-specific keys, which
// the server flattens beside the fixed ones, reach the CLI's Rule, Line and
// Column, and no value is echoed (VE-3).
func TestCLIDecodesSecretLocation(t *testing.T) {
	inv := errs.Violate("body", errs.LocBody, errs.ReasonSecretDetected,
		errs.WithValue("ghp_notreal"), errs.WithExtra("rule", "github-pat"),
		errs.WithExtra("line", 3), errs.WithExtra("column", 7))
	ae := cliDecode(t, renderError(t, inv, nil, nil).Result())
	if len(ae.Violations) != 1 {
		t.Fatalf("violations = %+v, want one", ae.Violations)
	}
	v := ae.Violations[0]
	line, lok := v.Line.Int()
	col, cok := v.Column.Int()
	if v.Reason != "secret_detected" || v.Rule != "github-pat" || !lok || line != 3 || !cok || col != 7 || v.Value != nil {
		t.Fatalf("violation = %+v, want rule github-pat at 3:7 and no value", v)
	}
}
