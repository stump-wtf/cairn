package redact

// Recorded Outcome Tests
//
// Pins the shape of Summary, the value Cairn stores and shows the owner, so no
// field of it can ever hold a secret, and checks the zero value reads as
// unscanned, the normalization the store relies on and the merge a growing run
// uses. Credentials are assembled at run time from split literals (see pat).
//
// Governing: ADR-0023, SPEC-0017 RD-9
//
// @joestump 09/25/2026 - Added for cairn#290.

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// TestSummaryHoldsNoSecret: Summary has exactly a status, a count and a map of
// rule ID to count. A new field, or a string where a number was, would be a
// place a secret could land, so the test fails until someone looks.
func TestSummaryHoldsNoSecret(t *testing.T) {
	want := map[string]reflect.Type{
		"Status": reflect.TypeOf(Status("")),
		"Count":  reflect.TypeOf(0),
		"Rules":  reflect.TypeOf(map[string]int{}),
	}
	st := reflect.TypeOf(Summary{})
	if st.NumField() != len(want) {
		t.Fatalf("Summary has %d fields, want exactly %d (Status, Count, Rules)", st.NumField(), len(want))
	}
	for i := 0; i < st.NumField(); i++ {
		f := st.Field(i)
		if w, ok := want[f.Name]; !ok || f.Type != w {
			t.Errorf("Summary.%s is %v; only Status Status, Count int and Rules map[string]int are allowed", f.Name, f.Type)
		}
	}
}

// TestZeroSummaryIsUnscanned: an insert path that was never wired to the
// scanner records "unscanned", never "clean".
func TestZeroSummaryIsUnscanned(t *testing.T) {
	got, err := Summary{}.Normalized()
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusUnscanned || got.Count != 0 || got.Rules == nil || len(got.Rules) != 0 {
		t.Errorf("zero Summary normalizes to %+v, want unscanned, 0, empty non-nil rules", got)
	}
	if (Summary{}).Redacted() {
		t.Error("zero Summary reports redacted")
	}
}

// TestOutcomeSummaryFromScan: a real masked scan becomes a Summary with the
// rule counted per value, and nothing of the value itself.
func TestOutcomeSummaryFromScan(t *testing.T) {
	s := newTestScanner(t)
	a, b := pat(3), pat(4)
	masked, o, err := s.Text(context.Background(), "body", "first "+a+"\nsecond "+b+"\n", ModeMask)
	if err != nil {
		t.Fatalf("Text: %v", err)
	}
	sum := o.Summary()
	if sum.Status != StatusMasked || sum.Count != 2 || sum.Rules["github-pat"] != 2 || !sum.Redacted() {
		t.Fatalf("Summary = %+v, want masked, 2, github-pat x2", sum)
	}
	o.Rules["github-pat"] = 99
	if sum.Rules["github-pat"] != 2 {
		t.Error("Summary shares its rules map with the Outcome")
	}
	norm, err := sum.Normalized()
	if err != nil {
		t.Fatalf("Normalized: %v", err)
	}
	dump := strings.Join([]string{string(norm.Status), strings.Join(keys(norm.Rules), ","), masked}, "|")
	for _, tok := range []string{a, b} {
		if strings.Contains(dump, tok) || strings.Contains(dump, tok[4:20]) {
			t.Error("the recorded outcome or masked text holds a planted token")
		}
	}
}

// TestSummaryNormalizedRefusesNonRuleIDs: a caller that puts scanned text
// where a rule ID belongs is refused, and the refusal does not repeat it.
func TestSummaryNormalizedRefusesNonRuleIDs(t *testing.T) {
	tok := pat(5)
	for name, s := range map[string]Summary{
		"token as rule":   {Status: StatusMasked, Count: 1, Rules: map[string]int{tok: 1}},
		"line as rule":    {Status: StatusMasked, Count: 1, Rules: map[string]int{"token = " + tok: 1}},
		"unknown status":  {Status: "off"},
		"negative count":  {Status: StatusClean, Count: -1},
		"negative per id": {Status: StatusMasked, Count: 1, Rules: map[string]int{"github-pat": -1}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := s.Normalized()
			if err == nil {
				t.Fatal("Normalized accepted it")
			}
			if strings.Contains(err.Error(), tok) || strings.Contains(err.Error(), tok[4:20]) {
				t.Error("the error repeats the planted token")
			}
		})
	}
}

// TestEveryRuleIDIsRecordable: every rule the merged config can report passes
// the rule-ID guard, so a real finding is never refused at write time.
func TestEveryRuleIDIsRecordable(t *testing.T) {
	s := newTestScanner(t)
	for id := range s.det.Config.Rules {
		if !ruleIDPattern.MatchString(id) {
			t.Errorf("rule %q does not match the recorded rule-ID pattern", id)
		}
	}
}

// TestSummaryMerge: counts add, and the status only moves toward the less
// reassuring end.
func TestSummaryMerge(t *testing.T) {
	cases := []struct {
		name       string
		prev, next Summary
		want       Status
	}{
		{"clean then masked", Summary{Status: StatusClean}, Summary{Status: StatusMasked}, StatusMasked},
		{"masked then clean", Summary{Status: StatusMasked}, Summary{Status: StatusClean}, StatusMasked},
		{"oversize then masked", Summary{Status: StatusNotScannedOversize}, Summary{Status: StatusMasked}, StatusNotScannedOversize},
		{"legacy row stays unscanned", Summary{Status: StatusUnscanned}, Summary{Status: StatusClean}, StatusUnscanned},
		{"zero row reads unscanned", Summary{}, Summary{Status: StatusClean}, StatusUnscanned},
		{"unwired append records unscanned", Summary{Status: StatusClean}, Summary{}, StatusUnscanned},
		{"unwired append after a mask", Summary{Status: StatusMasked}, Summary{}, StatusUnscanned},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.prev.Merge(c.next).Status; got != c.want {
				t.Errorf("Merge status = %s, want %s", got, c.want)
			}
		})
	}
	// An unknown status is not dropped by the merge, so the write refuses it.
	if _, err := (Summary{Status: StatusClean}).Merge(Summary{Status: "bogus"}).Normalized(); err == nil {
		t.Error("Merge dropped an unknown status, so Normalized accepted it")
	}
	got := Summary{Status: StatusMasked, Count: 2, Rules: map[string]int{"github-pat": 2}}.
		Merge(Summary{Status: StatusMasked, Count: 1, Rules: map[string]int{"github-pat": 1, "cairn-authorization": 0}})
	if got.Count != 3 || got.Rules["github-pat"] != 3 {
		t.Errorf("Merge = %+v, want count 3 and github-pat 3", got)
	}
}

// TestStatusesAreKnown: every listed status is one Normalized accepts.
func TestStatusesAreKnown(t *testing.T) {
	for _, st := range Statuses {
		if _, err := (Summary{Status: st}).Normalized(); err != nil {
			t.Errorf("status %s: %v", st, err)
		}
	}
}

func keys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
