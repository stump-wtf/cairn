package oauth

import (
	"strings"
	"testing"
)

// TestParseScopeExactlyThree proves the scope grammar recognizes exactly the
// three consent scopes and nothing else (SPEC-0007 REQ "Exactly Three Consent
// Scopes").
func TestParseScopeExactlyThree(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"empty defaults to all three", "", "artifacts:read artifacts:write annotations:write", false},
		{"single valid", "artifacts:read", "artifacts:read", false},
		{"all three any order", "annotations:write artifacts:read artifacts:write", "artifacts:read artifacts:write annotations:write", false},
		{"duplicates collapse", "artifacts:read artifacts:read", "artifacts:read", false},
		{"delete scope rejected", "artifacts:delete", "", true},
		{"sharing scope rejected", "sharing:manage", "", true},
		{"elevated among valid rejected", "artifacts:read artifacts:delete", "", true},
		{"garbage rejected", "☃", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseScope(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseScope(%q) = %v, want error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseScope(%q): %v", tt.in, err)
			}
			if JoinScope(got) != tt.want {
				t.Fatalf("ParseScope(%q) = %q, want %q", tt.in, JoinScope(got), tt.want)
			}
		})
	}
}

// TestConsentLines pins the exact three consent-screen lines (SPEC-0007 REQ
// "Consent Screen Content").
func TestConsentLines(t *testing.T) {
	want := map[string]string{
		ScopeArtifactsRead:    "Read artifacts you can access",
		ScopeArtifactsWrite:   "Create & push new artifacts",
		ScopeAnnotationsWrite: "Comment & react on your behalf",
	}
	for scope, line := range want {
		if got := ConsentLine(scope); got != line {
			t.Errorf("ConsentLine(%q) = %q, want %q", scope, got, line)
		}
	}
	if got := ConsentLine("artifacts:delete"); got != "" {
		t.Errorf("ConsentLine(artifacts:delete) = %q, want empty", got)
	}
}

// TestPKCE proves S256 verification: a matching verifier passes, everything
// else — wrong verifier, malformed verifier, empty challenge — fails closed
// (SPEC-0007 REQ "OAuth 2.1 Authorization-Code + PKCE").
func TestPKCE(t *testing.T) {
	verifier := strings.Repeat("v", 43)
	challenge := S256Challenge(verifier)
	if !ValidChallenge(challenge) {
		t.Fatalf("derived challenge %q not valid", challenge)
	}
	if !VerifyPKCE(verifier, challenge) {
		t.Fatal("matching verifier rejected")
	}
	if VerifyPKCE(strings.Repeat("x", 43), challenge) {
		t.Fatal("wrong verifier accepted")
	}
	if VerifyPKCE("short", challenge) {
		t.Fatal("too-short verifier accepted")
	}
	if VerifyPKCE(strings.Repeat("v", 129), challenge) {
		t.Fatal("too-long verifier accepted")
	}
	if VerifyPKCE(verifier+"!", challenge) {
		t.Fatal("verifier with illegal char accepted")
	}
	if VerifyPKCE(verifier, "") {
		t.Fatal("empty challenge accepted")
	}
}

func TestValidChallengeShape(t *testing.T) {
	if ValidChallenge("") || ValidChallenge("tooshort") || ValidChallenge(strings.Repeat("A", 44)) {
		t.Fatal("malformed challenge accepted")
	}
	if ValidChallenge(strings.Repeat("+", 43)) {
		t.Fatal("non-base64url challenge accepted")
	}
}

// TestValidateRedirectURI pins the registration rules: https anywhere, http
// only on loopback, nothing else (SPEC-0007 REQ "Redirect & SSRF Validation").
func TestValidateRedirectURI(t *testing.T) {
	valid := []string{
		"https://client.example/cb",
		"https://client.example:8443/cb?x=1",
		"http://localhost/cb",
		"http://localhost:53682/cb",
		"http://127.0.0.1:9000/callback",
		"http://[::1]:8080/cb",
	}
	for _, u := range valid {
		if err := ValidateRedirectURI(u); err != nil {
			t.Errorf("ValidateRedirectURI(%q) = %v, want nil", u, err)
		}
	}
	invalid := []string{
		"",
		"/relative/path",
		"http://evil.example/cb",              // http on a remote host
		"http://169.254.169.254/latest",       // http to a link-local metadata IP
		"https://client.example/cb#frag",      // fragment
		"https://user:pass@client.example/cb", // userinfo
		"myapp://callback",                    // custom scheme
		"javascript:alert(1)",
		"ftp://client.example/cb",
	}
	for _, u := range invalid {
		if err := ValidateRedirectURI(u); err == nil {
			t.Errorf("ValidateRedirectURI(%q) = nil, want error", u)
		}
	}
}

// TestMatchRedirectURI pins authorize-time matching: exact match only, with
// the RFC 8252 loopback-port allowance (SPEC-0007 scenario "Redirect to an
// unregistered URI").
func TestMatchRedirectURI(t *testing.T) {
	registered := []string{"https://client.example/cb", "http://127.0.0.1:1/cb"}
	tests := []struct {
		presented string
		want      bool
	}{
		{"https://client.example/cb", true},
		{"https://client.example/cb2", false},
		{"https://client.example/cb?extra=1", false},
		{"https://evil.example/cb", false},
		{"http://127.0.0.1:49152/cb", true},   // loopback: port may vary
		{"http://127.0.0.1:49152/cb2", false}, // ...but not the path
		{"http://localhost:49152/cb", false},  // ...or the host spelling
		{"http://evil.example/cb", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := MatchRedirectURI(registered, tt.presented); got != tt.want {
			t.Errorf("MatchRedirectURI(%q) = %v, want %v", tt.presented, got, tt.want)
		}
	}
}
