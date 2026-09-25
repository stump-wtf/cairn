package cliclient

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// Governing: ADR-0025, SPEC-0019 VE-1, VE-5, VE-8

// decodeBody runs DecodeError over a canned response.
func decodeBody(t *testing.T, status int, body string) *APIError {
	t.Helper()
	resp := &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}
	var apiErr *APIError
	if err := DecodeError(resp); !errors.As(err, &apiErr) {
		t.Fatalf("DecodeError = %v, want *APIError", err)
	}
	return apiErr
}

func TestDecodeErrorViolations(t *testing.T) {
	apiErr := decodeBody(t, http.StatusBadRequest, `{"error":{
		"code":"validation_failed",
		"message":"X-Cairn-Ttl-Seconds: \"5184000\" exceeds the maximum of 2592000 seconds",
		"violations":[
			{"field":"X-Cairn-Ttl-Seconds","location":"header","reason":"exceeds_max",
			 "limit":2592000,"unit":"seconds","value":"5184000",
			 "message":"\"5184000\" exceeds the maximum of 2592000 seconds"},
			{"field":"tags[1]","location":"body","reason":"too_long","limit":"64","unit":"bytes",
			 "value":"x","message":"\"x\" is too long: the maximum is 64 bytes"},
			{"field":"members[0].content","location":"form","reason":"secret_detected",
			 "rule":"aws-access-key","line":3,"column":"7","message":"a credential was detected"}
		],
		"request_id":"req_1"}}`)

	if apiErr.Code != CodeValidation || apiErr.RequestID != "req_1" {
		t.Fatalf("code/request_id = %q/%q", apiErr.Code, apiErr.RequestID)
	}
	if len(apiErr.Violations) != 3 {
		t.Fatalf("got %d violations, want 3: %+v", len(apiErr.Violations), apiErr.Violations)
	}

	ttl := apiErr.Violations[0]
	if ttl.Field != "X-Cairn-Ttl-Seconds" || ttl.Location != "header" || ttl.Reason != ReasonExceedsMax || ttl.Unit != UnitSeconds {
		t.Errorf("ttl violation = %+v", ttl)
	}
	if n, ok := ttl.Limit.Int(); !ok || n != 2592000 {
		t.Errorf("ttl limit = %v (%v), want 2592000", n, ok)
	}
	if ttl.Value == nil || *ttl.Value != "5184000" {
		t.Errorf("ttl value = %v, want 5184000", ttl.Value)
	}

	// A limit sent as a string decodes to the same number.
	if n, ok := apiErr.Violations[1].Limit.Int(); !ok || n != 64 {
		t.Errorf("string limit = %v (%v), want 64", n, ok)
	}

	secret := apiErr.Violations[2]
	if secret.Rule != "aws-access-key" || secret.Line.String() != "3" || secret.Column.String() != "7" {
		t.Errorf("secret location = rule %q line %q column %q", secret.Rule, secret.Line.String(), secret.Column.String())
	}
	if secret.Value != nil {
		t.Errorf("secret value = %q, want none", *secret.Value)
	}
}

// TestDecodeErrorWithoutViolations is an old server: no violations key, and
// the top-level message is all there is (SPEC-0019 VE-5 "Old client decodes",
// read the other way round).
func TestDecodeErrorWithoutViolations(t *testing.T) {
	apiErr := decodeBody(t, http.StatusBadRequest,
		`{"error":{"code":"validation_failed","message":"the request was invalid","details":{"field":"tag"}}}`)
	if apiErr.Violations != nil {
		t.Errorf("Violations = %+v, want nil", apiErr.Violations)
	}
	if apiErr.Message != "the request was invalid" || apiErr.Details["field"] != "tag" {
		t.Errorf("message/details = %q/%v", apiErr.Message, apiErr.Details)
	}
}

// TestDecodeErrorBadViolationsKeepsEnvelope: a violations array this client
// cannot parse costs the per-field detail, never the code and message.
func TestDecodeErrorBadViolationsKeepsEnvelope(t *testing.T) {
	for name, violations := range map[string]string{
		"not an array":  `{"field":"tag"}`,
		"object limit":  `[{"field":"tag","reason":"too_long","limit":{"n":1},"message":"m"}]`,
		"one bad entry": `[{"field":"tag","reason":"uppercase","message":"m"},{"field":7}]`,
	} {
		t.Run(name, func(t *testing.T) {
			apiErr := decodeBody(t, http.StatusBadRequest,
				`{"error":{"code":"validation_failed","message":"top","violations":`+violations+`}}`)
			if apiErr.Code != CodeValidation || apiErr.Message != "top" {
				t.Errorf("code/message = %q/%q", apiErr.Code, apiErr.Message)
			}
			if apiErr.Violations != nil {
				t.Errorf("Violations = %+v, want nil", apiErr.Violations)
			}
		})
	}
}

// TestScalarRoundTrip: --json re-emits a limit in the form it arrived in.
func TestScalarRoundTrip(t *testing.T) {
	for _, in := range []string{`{"limit":2592000}`, `{"limit":"30d"}`} {
		var v struct {
			Limit *Scalar `json:"limit"`
		}
		if err := json.Unmarshal([]byte(in), &v); err != nil {
			t.Fatalf("Unmarshal(%s): %v", in, err)
		}
		out, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if string(out) != in {
			t.Errorf("round trip %s = %s", in, out)
		}
	}
}
