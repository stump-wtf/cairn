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
