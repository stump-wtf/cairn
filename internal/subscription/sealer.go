package subscription

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// KeyEnv names the variable holding the key that seals subscription secrets.
const KeyEnv = "CAIRN_ENCRYPTION_KEY"

// sealVersion prefixes every sealed secret, so a later key rotation can tell
// which key sealed a row without a schema change.
const sealVersion byte = 1

// errOpen is returned for any secret that does not open under this key: a
// different key, another row's ciphertext, or a corrupted value. It never
// says which, and never carries the value.
var errOpen = errors.New("subscription: sealed secret does not open under the configured key")

// Sealer encrypts subscription secrets at rest with AES-256-GCM (SPEC-0023
// REQ "Owned Outbound Subscriptions": "stored encrypted"). The row id is the
// associated data, so a ciphertext copied onto another row does not open.
// A nil Sealer seals and opens nothing: without a key, subscriptions cannot
// be created, and the rest of the server runs normally.
//
// Governing: ADR-0029 (Teams and Tenancy, section 6), SPEC-0023 REQ "Owned
// Outbound Subscriptions"
type Sealer struct {
	aead cipher.AEAD
}

// ParseKey builds a Sealer from the CAIRN_ENCRYPTION_KEY value: 32 bytes,
// base64 (standard or URL alphabet, padded or not). An empty value returns
// a nil Sealer and no error. A malformed value is an error that names the
// variable and never quotes it.
func ParseKey(raw string) (*Sealer, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var key []byte
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(raw); err == nil {
			key = b
			break
		}
	}
	if key == nil {
		return nil, fmt.Errorf("config %s: not base64", KeyEnv)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("config %s: decodes to %d bytes, want 32 (generate one with: openssl rand -base64 32)", KeyEnv, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", KeyEnv, err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", KeyEnv, err)
	}
	return &Sealer{aead: aead}, nil
}

// seal encrypts secret bound to the row id.
func (s *Sealer) seal(secret []byte, rowID string) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("subscription: nonce: %w", err)
	}
	out := make([]byte, 0, 1+len(nonce)+len(secret)+s.aead.Overhead())
	out = append(out, sealVersion)
	out = append(out, nonce...)
	return s.aead.Seal(out, nonce, secret, []byte(rowID)), nil
}

// open decrypts a sealed secret bound to the row id.
func (s *Sealer) open(sealed []byte, rowID string) ([]byte, error) {
	n := s.aead.NonceSize()
	if len(sealed) < 1+n || sealed[0] != sealVersion {
		return nil, errOpen
	}
	pt, err := s.aead.Open(nil, sealed[1:1+n], sealed[1+n:], []byte(rowID))
	if err != nil {
		return nil, errOpen
	}
	return pt, nil
}
