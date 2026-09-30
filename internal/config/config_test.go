package config

// Tests for the approval class list (SPEC-0016 EV-5).

import (
	"strings"
	"testing"
)

// TestLoadRedaction covers the CAIRN_REDACTION_* defaults and their
// validation (ADR-0023, SPEC-0017 RD-4, RD-7, RD-8).
//
// @joestump 09/25/2026 - Added for cairn#289.
func TestLoadRedaction(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if c.RedactionMaxScanBytes != 16777216 || c.RedactionOversize != "reject" || c.RedactionAllowlistFile != "" {
			t.Errorf("defaults = %d, %q, %q", c.RedactionMaxScanBytes, c.RedactionOversize, c.RedactionAllowlistFile)
		}
		if got := strings.Join(c.RedactionRejectTypes, ","); got != "code,bundle" {
			t.Errorf("reject types = %q, want code,bundle", got)
		}
	})
	t.Run("set", func(t *testing.T) {
		t.Setenv("CAIRN_REDACTION_MAX_SCAN_BYTES", "1024")
		t.Setenv("CAIRN_REDACTION_OVERSIZE", "store_unscanned")
		t.Setenv("CAIRN_REDACTION_REJECT_TYPES", " Code , markdown,,bundle ")
		t.Setenv("CAIRN_REDACTION_ALLOWLIST_FILE", "/etc/cairn/allowlist.toml")
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if c.RedactionMaxScanBytes != 1024 || c.RedactionOversize != "store_unscanned" || c.RedactionAllowlistFile != "/etc/cairn/allowlist.toml" {
			t.Errorf("got %d, %q, %q", c.RedactionMaxScanBytes, c.RedactionOversize, c.RedactionAllowlistFile)
		}
		if got := strings.Join(c.RedactionRejectTypes, ","); got != "code,markdown,bundle" {
			t.Errorf("reject types = %q", got)
		}
	})
	for _, tc := range []struct{ key, val string }{
		{"CAIRN_REDACTION_OVERSIZE", "skip"},
		{"CAIRN_REDACTION_OVERSIZE", "off"},
		{"CAIRN_REDACTION_MAX_SCAN_BYTES", "0"},
		{"CAIRN_REDACTION_MAX_SCAN_BYTES", "-1"},
		{"CAIRN_REDACTION_MAX_SCAN_BYTES", "lots"},
	} {
		t.Run(tc.key+"="+tc.val, func(t *testing.T) {
			t.Setenv(tc.key, tc.val)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), tc.key) {
				t.Errorf("Load() err = %v, want an error naming %s", err, tc.key)
			}
		})
	}
}

// TestLoadApprovalReactions: CAIRN_APPROVAL_REACTIONS is split on commas with
// blanks dropped, and unset means nil, which annotation.NewApprovalClass reads
// as the default class (SPEC-0016 EV-5).
func TestLoadApprovalReactions(t *testing.T) {
	t.Setenv("CAIRN_APPROVAL_REACTIONS", "")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ApprovalReactions != nil {
		t.Fatalf("unset ApprovalReactions = %q, want nil (the default class)", c.ApprovalReactions)
	}

	t.Setenv("CAIRN_APPROVAL_REACTIONS", " \U0001F680 ,,✅️, ")
	if c, err = Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"\U0001F680", "✅️"}
	if len(c.ApprovalReactions) != len(want) || c.ApprovalReactions[0] != want[0] || c.ApprovalReactions[1] != want[1] {
		t.Fatalf("ApprovalReactions = %q, want %q", c.ApprovalReactions, want)
	}
}

// TestDevInsecureBearerRefusesHTTPS pins audit A21: the insecure bearer lets
// any token act as any actor, so an https base URL (a real deployment) fails
// boot, while local http development keeps working (SPEC-0023 REQ "Users and
// Identities").
func TestDevInsecureBearerRefusesHTTPS(t *testing.T) {
	cases := []struct {
		base    string
		wantErr bool
	}{
		{"https://cairn.example.com", true},
		{"HTTPS://cairn.example.com", true},
		{"http://localhost:8080", false},
	}
	for _, tc := range cases {
		t.Run(tc.base, func(t *testing.T) {
			t.Setenv("CAIRN_DEV_INSECURE_BEARER_AUTH", "true")
			t.Setenv("CAIRN_BASE_URL", tc.base)
			_, err := Load()
			if tc.wantErr && err == nil {
				t.Fatal("Load() accepted the insecure bearer with an https base URL")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Load() = %v, want nil for local http", err)
			}
		})
	}
	// The default base URL is https, so the flag alone must also refuse.
	t.Run("default base URL", func(t *testing.T) {
		t.Setenv("CAIRN_DEV_INSECURE_BEARER_AUTH", "true")
		t.Setenv("CAIRN_BASE_URL", "")
		if _, err := Load(); err == nil {
			t.Fatal("Load() accepted the insecure bearer with the default https base URL")
		}
	})
	t.Run("off", func(t *testing.T) {
		t.Setenv("CAIRN_DEV_INSECURE_BEARER_AUTH", "false")
		t.Setenv("CAIRN_BASE_URL", "https://cairn.example.com")
		if _, err := Load(); err != nil {
			t.Fatalf("Load() = %v with the insecure bearer off", err)
		}
	})
}

// CAIRN_OIDC_TRUST_EMAIL is off unless set, and a malformed value fails boot
// rather than silently leaving an operator's IdP untrusted (or trusted).
func TestOIDCTrustEmail(t *testing.T) {
	t.Setenv("CAIRN_OIDC_TRUST_EMAIL", "")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if c.OIDCTrustEmail {
		t.Fatal("OIDCTrustEmail defaulted on")
	}
	t.Setenv("CAIRN_OIDC_TRUST_EMAIL", "true")
	if c, err = Load(); err != nil || !c.OIDCTrustEmail {
		t.Fatalf("Load() = %v, OIDCTrustEmail = %v with the knob set", err, c != nil && c.OIDCTrustEmail)
	}
	t.Setenv("CAIRN_OIDC_TRUST_EMAIL", "yes please")
	if _, err := Load(); err == nil {
		t.Fatal("Load() accepted a malformed CAIRN_OIDC_TRUST_EMAIL")
	}
}
