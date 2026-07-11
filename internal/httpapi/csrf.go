package httpapi

import (
	"crypto/subtle"
	"net/http"

	"github.com/joestump/cairn/internal/errs"
)

// csrfCookieName / csrfHeaderName are the double-submit pair a cookie-session
// surface uses: the server sets an unpredictable token in a cookie the browser
// echoes on same-site navigations, and a same-origin script copies it into a
// request header a cross-site attacker cannot read or set. A match proves the
// request originated from Cairn's own origin.
const (
	csrfCookieName = "cairn_csrf"
	csrfHeaderName = "X-CSRF-Token"
)

// enforceCSRF is the CSRF seam for state-changing annotation writes. It runs
// after requireAuth, so a principal is present. Token-authenticated callers
// (API/MCP/CLI) carry no ambient credential a cross-site request could forge,
// so they are exempt and pass straight through — this is the common path today.
//
// The guarded branch is the deliberate seam for the future cookie-session
// surface (#11 / SPEC-0008): when a principal is Ambient, the request MUST
// present a CSRF token in the X-CSRF-Token header that matches the cairn_csrf
// cookie (a stateless double-submit check), or it is refused. The web-session
// Authenticator sets Principal.Ambient and issues the cookie; wiring the check
// here means every annotation write is already covered the day that surface
// lands, with no new gate to remember (SPEC-0006 REQ "CSRF Protection").
//
// Governing: SPEC-0006 REQ "CSRF Protection", ADR-0012 (error envelope).
func (s *Server) enforceCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFrom(r.Context())
		if ok && p.Ambient && !validCSRFToken(r) {
			// A leak-free 403: the envelope never says "CSRF" specifically, it
			// is the uniform forbidden shape.
			s.writeError(w, r, errs.ErrForbidden, nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// validCSRFToken reports whether the request carries a header token that
// matches its cairn_csrf cookie under a constant-time compare. An absent or
// empty token on either side fails closed.
func validCSRFToken(r *http.Request) bool {
	header := r.Header.Get(csrfHeaderName)
	if header == "" {
		return false
	}
	cookie, err := r.Cookie(csrfCookieName)
	if err != nil || cookie.Value == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(header), []byte(cookie.Value)) == 1
}
