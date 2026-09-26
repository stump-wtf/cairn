package subscription

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

// Governing: SPEC-0023 REQ "Owned Outbound Subscriptions" (secret supplied
// or minted, shown once, stored encrypted)

func testKey(b byte) string {
	return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{b}, 32))
}

func TestParseKey(t *testing.T) {
	if s, err := ParseKey(""); s != nil || err != nil {
		t.Fatalf("ParseKey(\"\") = %v, %v; want nil, nil", s, err)
	}
	raw := bytes.Repeat([]byte{7}, 32)
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if s, err := ParseKey(enc.EncodeToString(raw)); s == nil || err != nil {
			t.Fatalf("ParseKey(%v) = %v, %v", enc, s, err)
		}
	}
	for _, bad := range []string{"not base64 at all!", base64.StdEncoding.EncodeToString([]byte("sixteen byte key"))} {
		_, err := ParseKey(bad)
		if err == nil {
			t.Fatalf("ParseKey accepted a malformed key")
		}
		if !strings.Contains(err.Error(), KeyEnv) || strings.Contains(err.Error(), bad) {
			t.Fatalf("error %q must name %s and never quote the value", err, KeyEnv)
		}
	}
}

func TestSealOpen(t *testing.T) {
	s, _ := ParseKey(testKey(1))
	other, _ := ParseKey(testKey(2))
	secret := []byte("whsec_the-plaintext-secret")
	sealed, err := s.seal(secret, "row-a")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, secret) {
		t.Fatal("sealed value contains the plaintext")
	}
	if got, err := s.open(sealed, "row-a"); err != nil || !bytes.Equal(got, secret) {
		t.Fatalf("open = %q, %v", got, err)
	}
	if _, err := s.open(sealed, "row-b"); err == nil {
		t.Fatal("a ciphertext opened under another row's id")
	}
	if _, err := other.open(sealed, "row-a"); err == nil {
		t.Fatal("a ciphertext opened under another key")
	}
	again, _ := s.seal(secret, "row-a")
	if bytes.Equal(again, sealed) {
		t.Fatal("sealing twice produced identical ciphertext (nonce reuse)")
	}
}

func TestSecretOrMint(t *testing.T) {
	minted, err := secretOrMint("")
	if err != nil || !strings.HasPrefix(minted, mintedPrefix) || len(minted) < MinSecretBytes {
		t.Fatalf("minted %q, %v", minted, err)
	}
	if again, _ := secretOrMint(""); again == minted {
		t.Fatal("two mints produced the same secret")
	}
	supplied := strings.Repeat("s", MinSecretBytes)
	if got, err := secretOrMint(supplied); err != nil || got != supplied {
		t.Fatalf("a %d-byte supplied secret = %q, %v; want it verbatim", MinSecretBytes, got, err)
	}
	for _, bad := range []string{
		strings.Repeat("s", MinSecretBytes-1),
		strings.Repeat("s", 513),
		strings.Repeat("s", 31) + " x",
		strings.Repeat("s", 40) + "\n",
	} {
		_, err := secretOrMint(bad)
		if err == nil {
			t.Fatalf("accepted a bad supplied secret of %d bytes", len(bad))
		}
		if strings.Contains(err.Error(), bad) {
			t.Fatal("the error quotes the supplied secret")
		}
	}
}
