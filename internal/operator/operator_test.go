package operator

import (
	"strings"
	"testing"
)

// SPEC-0023 REQ "Operator and User Profiles": CAIRN_OPERATORS is a
// comma-separated list of "<issuer>|<subject>" entries.
func TestParseOperators(t *testing.T) {
	s, err := Parse(" https://id.example.com|abc-123 , https://auth0.example.com|auth0|42 ,, ", "")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !s.Enabled() || s.Len() != 2 {
		t.Fatalf("Enabled=%v Len=%d, want true 2", s.Enabled(), s.Len())
	}
	if !s.Lists("https://id.example.com", "abc-123") {
		t.Error("listed identity not matched")
	}
	// Split at the FIRST "|": subjects may contain one, issuers never do.
	if !s.Lists("https://auth0.example.com", "auth0|42") {
		t.Error("a subject containing | must survive parsing")
	}
	for _, miss := range [][2]string{
		{"https://id.example.com", "abc-1234"},
		{"https://id.example.com/", "abc-123"},
		{"https://other.example.com", "abc-123"},
		{"", ""},
	} {
		if s.Lists(miss[0], miss[1]) {
			t.Errorf("Lists(%q, %q) = true, want false", miss[0], miss[1])
		}
	}
}

func TestParseOperatorsRejectsMalformedEntriesByPosition(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"https://id.example.com", "entry 1"},
		{"https://a|x, |abc", "entry 2"},
		{"https://a|x, https://b|y, https://c|", "entry 3"},
	} {
		_, err := Parse(tc.raw, "")
		if err == nil || !strings.Contains(err.Error(), "CAIRN_OPERATORS "+tc.want) {
			t.Errorf("Parse(%q) error = %v, want one naming %q", tc.raw, err, tc.want)
		}
	}
}

// SPEC-0023 "No operator configured": neither variable set means no operator,
// and nil behaves the same as an empty profile.
func TestNoOperatorConfigured(t *testing.T) {
	s, err := Parse("", "  ")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	var none *Set
	for name, set := range map[string]*Set{"empty": s, "nil": none} {
		if set.Enabled() {
			t.Errorf("%s: Enabled() = true", name)
		}
		if set.IsOperator("https://id.example.com", "abc", "") || set.IsOperator("", "", "") {
			t.Errorf("%s: IsOperator matched with no operator configured", name)
		}
	}
}

// A session is an operator's by the group only while the group it recorded
// at sign-in is the one configured now, so unsetting or renaming
// CAIRN_OPERATOR_GROUP demotes it on the next request.
func TestIsOperatorByGroup(t *testing.T) {
	s, _ := Parse("", "cairn-ops")
	if !s.Enabled() {
		t.Fatal("a group alone enables the operator profile")
	}
	if !s.IsOperator("https://id.example.com", "abc", "cairn-ops") {
		t.Error("session that recorded the configured group is an operator's")
	}
	if s.IsOperator("https://id.example.com", "abc", "") {
		t.Error("session without the group is not an operator's")
	}
	renamed, _ := Parse("", "platform-ops")
	if renamed.IsOperator("https://id.example.com", "abc", "cairn-ops") {
		t.Error("a session that recorded a group no longer configured must not be an operator's")
	}
	unset, _ := Parse("https://id.example.com|zzz", "")
	if unset.IsOperator("https://id.example.com", "abc", "cairn-ops") {
		t.Error("with the group unset, a recorded group must not make an operator")
	}
}
