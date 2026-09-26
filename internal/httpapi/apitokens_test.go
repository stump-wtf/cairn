package httpapi

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stump-wtf/cairn/internal/errs"
)

// testTokenUserID stands in for a resolved user id where no database is
// wired.
const testTokenUserID = "00000000-0000-4000-8000-000000000001"

// TestParseAPITokens covers the SPEC-0023 CAIRN_API_TOKENS grammar:
// `secret:<user>[:agent|:human]` with <user> an "<issuer>|<subject>" or an
// email, agent by default, positions counted over non-empty entries.
func TestParseAPITokens(t *testing.T) {
	t.Run("empty yields no tokens", func(t *testing.T) {
		got, err := ParseAPITokens("   ")
		if err != nil || got != nil {
			t.Fatalf("ParseAPITokens(empty) = %v, %v; want nil, nil", got, err)
		}
	})

	got, err := ParseAPITokens(" sk_a:https://id.example.com|abc-123 , , sk_b:Joe@Example.com:human," +
		"sk_c:https://id.example.com|auth0|42:agent,sk_d:https://id.example.com:8443|urn:x:y")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []APIToken{
		{Secret: "sk_a", User: "https://id.example.com|abc-123", IsAgent: true, Position: 1},
		{Secret: "sk_b", User: "joe@example.com", IsAgent: false, Position: 2},
		// A subject may hold "|" and ":"; only a trailing role word is a role.
		{Secret: "sk_c", User: "https://id.example.com|auth0|42", IsAgent: true, Position: 3},
		{Secret: "sk_d", User: "https://id.example.com:8443|urn:x:y", IsAgent: true, Position: 4},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d tokens, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("token[%d] = %+v, want %+v", i, got[i], want[i])
		}
		if got[i].UserID != "" {
			t.Errorf("token[%d] has a UserID before boot resolution", i)
		}
	}
}

// TestParseAPITokensRefuses covers SPEC-0023 "Legacy entry refused" and every
// other malformed entry: each fails with the entry's position and the entry
// form, and no error ever carries any part of the entry, so a secret cannot
// reach the boot log.
func TestParseAPITokensRefuses(t *testing.T) {
	const good = "sk_fine_0123456789:ops@example.com,"
	for _, tc := range []struct {
		name, entry, want string
	}{
		{"legacy actor", "sk_legacy_0123456789:ci-bot", "legacy secret:actor"},
		{"legacy actor with role", "sk_legacy_0123456789:ci-bot:agent", "legacy secret:actor"},
		{"legacy actor with human role", "sk_legacy_0123456789:alice:human", "legacy secret:actor"},
		{"legacy user id", "sk_legacy_0123456789:user:00000000-0000-4000-8000-000000000001", "legacy secret:actor"},
		{"no user", "sk_lonely_0123456789", "want secret:<user>"},
		{"blank secret", ":ops@example.com", "want secret:<user>"},
		{"blank user", "sk_blank_0123456789:", "want secret:<user>"},
		{"unknown role on email", "sk_role_0123456789:ops@example.com:root", "role must be agent or human"},
		{"blank issuer", "sk_iss_0123456789: |abc", "issuer and a subject"},
		{"blank subject", "sk_sub_0123456789:https://id.example.com|", "issuer and a subject"},
		{"blank subject before role", "sk_sub_0123456789:https://id.example.com|:agent", "issuer and a subject"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseAPITokens(good + tc.entry)
			if err == nil {
				t.Fatalf("ParseAPITokens(%q) should have errored", tc.entry)
			}
			msg := err.Error()
			if !strings.HasPrefix(msg, "CAIRN_API_TOKENS entry 2: ") {
				t.Errorf("error %q does not name entry 2", msg)
			}
			if !strings.Contains(msg, tc.want) {
				t.Errorf("error %q does not mention %q", msg, tc.want)
			}
			if !strings.Contains(msg, apiTokenForm) {
				t.Errorf("error %q does not name the entry form", msg)
			}
			assertNoEntryText(t, msg, tc.entry)
		})
	}

	t.Run("duplicate secret", func(t *testing.T) {
		_, err := ParseAPITokens("sk_dup_0123456789:a@example.com,sk_dup_0123456789:b@example.com")
		if err == nil || err.Error() != "CAIRN_API_TOKENS entry 2: same secret as entry 1" {
			t.Fatalf("duplicate: %v", err)
		}
	})
}

// assertNoEntryText fails when msg quotes any field of entry longer than a
// few characters: the secret and the user alike.
func assertNoEntryText(t *testing.T, msg, entry string) {
	t.Helper()
	for _, field := range strings.FieldsFunc(entry, func(r rune) bool { return r == ':' || r == '|' }) {
		if len(field) >= 6 && strings.Contains(msg, field) {
			t.Errorf("error %q quotes %q from the entry", msg, field)
		}
	}
}

// An entry that was never resolved to a user (ResolveAPITokens) does not
// authenticate, whatever its secret: the fail-closed default for a server
// that skipped boot resolution.
func TestTokenAuthenticatorUnresolvedFailsClosed(t *testing.T) {
	tokens, err := ParseAPITokens("sk_unresolved_0123456789:ops@example.com:human")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	auth := NewTokenAuthenticator(tokens)
	r := httptest.NewRequest(http.MethodPost, "/v1/artifacts", nil)
	r.Header.Set("Authorization", "Bearer sk_unresolved_0123456789")
	if _, err := auth.Authenticate(r); !errors.Is(err, errs.ErrUnauthorized) {
		t.Fatalf("unresolved token must be unauthorized, got %v", err)
	}
}

// ResolveAPITokens on a storeless server refuses to start rather than
// accepting tokens it cannot bind to a user.
func TestResolveAPITokensNeedsTheDatabase(t *testing.T) {
	tokens, err := ParseAPITokens("sk_storeless_0123456789:ops@example.com")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	s := New(nil, nil, nil, Config{APITokens: tokens}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	err = s.ResolveAPITokens(t.Context())
	if err == nil || !strings.HasPrefix(err.Error(), "CAIRN_API_TOKENS entry 1: ") {
		t.Fatalf("storeless resolve = %v, want an entry 1 error", err)
	}
	assertNoEntryText(t, err.Error(), "sk_storeless_0123456789")
}
