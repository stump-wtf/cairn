package pat

import (
	"strings"
	"testing"
)

func TestNewSecretIsPrefixedAndUnique(t *testing.T) {
	a, err := newSecret()
	if err != nil {
		t.Fatalf("newSecret: %v", err)
	}
	b, err := newSecret()
	if err != nil {
		t.Fatalf("newSecret: %v", err)
	}
	if !strings.HasPrefix(a, secretPrefix) {
		t.Fatalf("secret %q missing prefix %q", a, secretPrefix)
	}
	if a == b {
		t.Fatal("two consecutive secrets must not collide")
	}
}

func TestHashSecretIsDeterministicAndOneWay(t *testing.T) {
	secret := "cairn_pat_example"
	h1 := hashSecret(secret)
	h2 := hashSecret(secret)
	if h1 != h2 {
		t.Fatal("hashSecret must be deterministic for the same input")
	}
	if h1 == secret {
		t.Fatal("hashSecret must not return the plaintext")
	}
	if len(h1) != 64 {
		t.Fatalf("hash length = %d, want 64 (hex sha256)", len(h1))
	}
}

func TestParseScopeRejectsUnknownScope(t *testing.T) {
	if _, err := ParseScope("artifacts:read sharing:manage"); err == nil {
		t.Fatal("ParseScope must reject sharing:manage — no PAT may ever hold it")
	}
}

func TestParseScopeAcceptsSubset(t *testing.T) {
	scopes, err := ParseScope("artifacts:read")
	if err != nil {
		t.Fatalf("ParseScope: %v", err)
	}
	if len(scopes) != 1 || scopes[0] != ScopeArtifactsRead {
		t.Fatalf("scopes = %v, want exactly [%s]", scopes, ScopeArtifactsRead)
	}
}

func TestAllScopesIsExactlyThree(t *testing.T) {
	if got := len(AllScopes()); got != 3 {
		t.Fatalf("AllScopes() has %d entries, want exactly 3 (ADR-0004)", got)
	}
}
