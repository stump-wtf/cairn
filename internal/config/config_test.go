package config

// Tests for outbound-webhook target validation (SPEC-0012 Security
// Requirements: TLS required for non-localhost targets).
//
// @joestump-agent 09/06/2026 - Added with the validator during review of #175.

import "testing"

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
