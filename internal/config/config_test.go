package config

// Tests for outbound-webhook target validation (SPEC-0012 Security
// Requirements: TLS required for non-localhost targets).
//
// @joestump-agent 09/06/2026 - Added with the validator during review of #175.

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

func TestValidateWebhookURLs(t *testing.T) {
	cases := []struct {
		urls  []string
		valid bool
	}{
		{nil, true},
		{[]string{"https://switchboard.example/webhooks/w/tok"}, true},
		{[]string{"http://localhost:8080/hook", "http://127.0.0.1:9090/hook"}, true},
		{[]string{"http://switchboard.example/webhooks/w/tok"}, false},
		{[]string{"ftp://switchboard.example/hook"}, false},
		{[]string{"://not a url"}, false},
	}
	for _, tc := range cases {
		err := validateWebhookURLs(tc.urls)
		if tc.valid && err != nil {
			t.Errorf("validateWebhookURLs(%q) = %v, want nil", tc.urls, err)
		}
		if !tc.valid && err == nil {
			t.Errorf("validateWebhookURLs(%q) = nil, want error", tc.urls)
		}
	}
}
