package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stump-wtf/cairn/internal/event"
)

// withPrincipal stashes p in the request context the way requireAuth does, so
// the CSRF seam can be exercised without a full auth round-trip.
func withPrincipal(r *http.Request, p *Principal) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), principalCtxKey{}, p))
}

// TestEnforceCSRF exercises the CSRF seam directly: token principals are exempt
// (no ambient credential to forge), while an ambient (cookie-session) principal
// is refused unless it presents a double-submit token that matches its cookie
// (SPEC-0006 REQ "CSRF Protection").
func TestEnforceCSRF(t *testing.T) {
	s := New(nil, nil, nil, Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := s.enforceCSRF(next)

	newReq := func(p *Principal, mutate func(*http.Request)) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/v1/artifacts/abc12345/comments", nil)
		if mutate != nil {
			mutate(r)
		}
		return withPrincipal(r, p)
	}

	tokenPrincipal := &Principal{ActorID: "alice", Auth: event.AuthPAT}                      // bearer: ambient=false
	ambientPrincipal := &Principal{ActorID: "alice", Ambient: true, Auth: event.AuthSession} // session cookie

	tests := []struct {
		name string
		req  *http.Request
		want int
	}{
		{
			name: "token principal is exempt",
			req:  newReq(tokenPrincipal, nil),
			want: http.StatusOK,
		},
		{
			name: "ambient without token is refused",
			req:  newReq(ambientPrincipal, nil),
			want: http.StatusForbidden,
		},
		{
			name: "ambient with mismatched token is refused",
			req: newReq(ambientPrincipal, func(r *http.Request) {
				r.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "secret-token"})
				r.Header.Set(csrfHeaderName, "wrong-token")
			}),
			want: http.StatusForbidden,
		},
		{
			name: "ambient with matching double-submit token passes",
			req: newReq(ambientPrincipal, func(r *http.Request) {
				r.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "secret-token"})
				r.Header.Set(csrfHeaderName, "secret-token")
			}),
			want: http.StatusOK,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, tc.req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}
