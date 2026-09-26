package config

// Tests for outbound-webhook target validation (SPEC-0012 Security
// Requirements: TLS required for non-localhost targets) and the approval
// class list (SPEC-0016 EV-5).
//
// @joestump-agent 09/06/2026 - Added with the validator during review of #175.

import "testing"

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
