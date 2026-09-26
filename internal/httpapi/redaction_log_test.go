package httpapi

// Redaction Rejection Log Tests
//
// SPEC-0017 RD-9 "Logs carry no value": a logged rejection carries the rule
// ID, the surface and the request id, and no part of the secret. These run
// without a database: each drives writeError or mcpToolErr with a real
// rejection from the scanner over a token assembled at run time.
//
// Governing: ADR-0023, SPEC-0017 RD-9
//
// @joestump 09/26/2026 - Added for cairn#290.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/redact"
)

// rejectionFor returns the scanner's reject-mode error for tok in field.
func rejectionFor(t *testing.T, field, tok string) error {
	t.Helper()
	sc, err := redact.New(redact.Config{})
	if err != nil {
		t.Fatal(err)
	}
	_, _, rej := sc.Text(context.Background(), field, "token: "+tok+"\n", redact.ModeReject)
	if !errors.Is(rej, redact.ErrSecretDetected) {
		t.Fatalf("the scanner did not reject the planted token: %v", rej)
	}
	return rej
}

// TestRedactionRejectionLogNamesSurface: the surface in a REST rejection's
// log line comes from the matched route (and, for the artifact create, from
// whether the rejected field is a bundle member), never from the client.
func TestRedactionRejectionLogNamesSurface(t *testing.T) {
	tok := plantedToken(11)
	cases := []struct {
		name, method, route, target, field, want string
	}{
		{"artifact", http.MethodPost, "/v1/artifacts", "/v1/artifacts", "body", "artifact"},
		{"bundle member", http.MethodPost, "/v1/artifacts", "/v1/artifacts", "members[2].content", "bundle"},
		{"comment", http.MethodPost, "/v1/artifacts/{id}/comments", "/v1/artifacts/abc/comments", "body", "comment"},
		{"web comment", http.MethodPost, "/{id}/comments", "/abc/comments", "body", "comment"},
		{"run create", http.MethodPost, "/v1/runs", "/v1/runs", "prompt", "run"},
		{"run spans", http.MethodPost, "/v1/runs/{id}/spans", "/v1/runs/abc/spans", "spans[12].args", "run"},
		{"webhook", http.MethodPost, "/h/{id}", "/h/abc", "body", "webhook"},
		{"unscanned route", http.MethodPost, "/v1/tokens", "/v1/tokens", "body", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rejectErr := rejectionFor(t, tc.field, tok)
			logs := &syncBuffer{}
			s := New(nil, nil, nil, Config{}, slog.New(slog.NewTextHandler(logs, nil)))
			var reqID string
			r := chi.NewRouter()
			r.Use(middleware.RequestID)
			r.MethodFunc(tc.method, tc.route, func(w http.ResponseWriter, r *http.Request) {
				reqID = middleware.GetReqID(r.Context())
				s.writeError(w, r, rejectErr, nil)
			})
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.target, nil))

			out := logs.String()
			if reqID == "" {
				t.Fatal("no request id was assigned")
			}
			wants := []string{"request_id=" + reqID, "redaction_rules=github-pat", "redaction_reason=secret_detected"}
			if tc.want != "" {
				wants = append(wants, "surface="+tc.want)
			} else if strings.Contains(out, "surface=") {
				t.Errorf("a route with no scanned surface logged one: %s", out)
			}
			for _, want := range wants {
				if !slices.Contains(strings.Fields(out), want) {
					t.Errorf("log line lacks %q: %s", want, out)
				}
			}
			body, _ := io.ReadAll(rec.Body)
			assertNoToken(t, "rejection log", out, []string{tok})
			assertNoToken(t, "rejection response", string(body), []string{tok})
		})
	}
}

// TestRedactionLogAttrsOnlyForRejections: any other error adds nothing, so
// ordinary failures keep their existing log shape.
func TestRedactionLogAttrsOnlyForRejections(t *testing.T) {
	if got := redactionLogAttrs(errs.ErrNotFound, "artifact"); got != nil {
		t.Errorf("a non-rejection gained redaction attributes: %v", got)
	}
	if got := redactionLogAttrs(nil, "artifact"); got != nil {
		t.Errorf("a nil error gained redaction attributes: %v", got)
	}
}

// TestMCPToolErrLogsRejectionSurface: the MCP transport logs the same
// attributes, with the surface from the tool name and the HTTP request id,
// and neither the log nor the error returned to the agent holds the token.
func TestMCPToolErrLogsRejectionSurface(t *testing.T) {
	tok := plantedToken(13)
	for tool, want := range map[string]string{
		"artifact_create":  "artifact",
		"bundle_create":    "bundle",
		"artifact_comment": "comment",
		"run_create":       "run",
		"run_append_spans": "run",
	} {
		t.Run(tool, func(t *testing.T) {
			logs := &syncBuffer{}
			s := New(nil, nil, nil, Config{}, slog.New(slog.NewTextHandler(logs, nil)))
			ctx := context.WithValue(context.Background(), middleware.RequestIDKey, "req-290")
			got := s.mcpToolErr(ctx, tool, rejectionFor(t, "body", tok))

			out := logs.String()
			for _, w := range []string{"tool=" + tool, "request_id=req-290", "surface=" + want, "redaction_rules=github-pat"} {
				if !slices.Contains(strings.Fields(out), w) {
					t.Errorf("log line lacks %q: %s", w, out)
				}
			}
			assertNoToken(t, "mcp rejection log", out, []string{tok})
			assertNoToken(t, "mcp tool error", got.Error(), []string{tok})
		})
	}
}
