package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/errs"
)

// Governing: SPEC-0024 REQ "Declaring Relations on Create", REQ "Relation Kinds
// and Limits", REQ "Trace Context on Create"; ADR-0025.

// reasons flattens the violations a set reports into "field/reason" pairs.
func reasons(t *testing.T, err error) []string {
	t.Helper()
	if err == nil {
		return nil
	}
	vs := errs.ViolationsOf(err)
	out := make([]string, 0, len(vs))
	for _, v := range vs {
		out = append(out, string(v.Field)+"/"+string(v.Reason))
	}
	return out
}

func TestRelationEntryGrammar(t *testing.T) {
	cases := []struct {
		name  string
		entry string
		want  []artifact.Relation
		bad   []string
	}{
		{
			name:  "equals separator",
			entry: "reply_to=a1b2c3",
			want:  []artifact.Relation{{Kind: artifact.RelationReplyTo, ID: "a1b2c3"}},
		},
		{
			name:  "colon separator is accepted too",
			entry: "derived_from:a1b2c3",
			want:  []artifact.Relation{{Kind: artifact.RelationDerivedFrom, ID: "a1b2c3"}},
		},
		{
			name:  "surrounding space is trimmed",
			entry: "  follows=a1b2c3\t",
			want:  []artifact.Relation{{Kind: artifact.RelationFollows, ID: "a1b2c3"}},
		},
		{name: "empty entry is skipped", entry: "  "},
		{
			name:  "no separator at all",
			entry: "reply_to",
			bad:   []string{"relation/invalid_format"},
		},
		{
			name:  "unknown kind",
			entry: "mentions=a1b2c3",
			bad:   []string{"relation.kind/unknown_value"},
		},
		{
			name:  "empty target",
			entry: "reply_to=",
			bad:   []string{"relation.id/required"},
		},
		{
			name:  "id outside the alphabet",
			entry: "reply_to=n0t_an_i-d",
			bad:   []string{"relation.id/invalid_charset"},
		},
		{
			name:  "entry longer than any valid one",
			entry: "reply_to=" + strings.Repeat("a", 40),
			bad:   []string{"relation/too_long"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var rs relationSet
			rs.addEntry(tc.entry, errs.LocHeader)
			if err := rs.err(); err != nil {
				got := reasons(t, err)
				if len(got) != len(tc.bad) {
					t.Fatalf("violations = %v, want %v", got, tc.bad)
				}
				for i := range got {
					if got[i] != tc.bad[i] {
						t.Errorf("violation %d = %s, want %s", i, got[i], tc.bad[i])
					}
				}
				if len(rs.relations) != 0 {
					t.Errorf("a rejected entry still added %+v", rs.relations)
				}
				return
			}
			if len(tc.bad) > 0 {
				t.Fatalf("err = nil, want %v", tc.bad)
			}
			if len(rs.relations) != len(tc.want) {
				t.Fatalf("relations = %+v, want %+v", rs.relations, tc.want)
			}
			for i := range tc.want {
				if rs.relations[i] != tc.want[i] {
					t.Errorf("relation %d = %+v, want %+v", i, rs.relations[i], tc.want[i])
				}
			}
		})
	}
}

// The three sources on one request merge into one set, first-occurrence order
// wins, and a target named twice is one edge.
func TestRelationSourcesMerge(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost,
		"/v1/artifacts?relation=reply_to=aaa111&relation=derived_from:bbb222", nil)
	req.Header.Add(relationHeader, "reply_to=aaa111")
	req.Header.Add(relationHeader, "follows=ccc333")

	var rs relationSet
	if err := rs.addFromRequest(req); err != nil {
		t.Fatalf("addFromRequest: %v", err)
	}
	want := []artifact.Relation{
		{Kind: artifact.RelationReplyTo, ID: "aaa111"},
		{Kind: artifact.RelationDerivedFrom, ID: "bbb222"},
		{Kind: artifact.RelationFollows, ID: "ccc333"},
	}
	if len(rs.relations) != len(want) {
		t.Fatalf("relations = %+v, want %+v", rs.relations, want)
	}
	for i := range want {
		if rs.relations[i] != want[i] {
			t.Errorf("relation %d = %+v, want %+v", i, rs.relations[i], want[i])
		}
	}
}

// One comma-separated value carries several relations, and a trailing separator
// costs nothing.
func TestRelationListSplits(t *testing.T) {
	var rs relationSet
	rs.addList("reply_to=aaa111, follows=bbb222,", errs.LocQuery)
	if err := rs.err(); err != nil {
		t.Fatalf("addList: %v", err)
	}
	if len(rs.relations) != 2 {
		t.Fatalf("relations = %+v, want 2", rs.relations)
	}
}

// The distinct cap holds across every source, so a multipart body cannot buffer
// an unbounded relation list, and the refusal names where the extra one arrived.
func TestRelationCapAcrossSources(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/artifacts", nil)
	var rs relationSet
	for i := 0; i < artifact.MaxRelations; i++ {
		rs.addEntry("follows=targe"+string(rune('a'+i))+"t", errs.LocQuery)
	}
	if err := rs.err(); err != nil {
		t.Fatalf("at the cap: %v", err)
	}
	req.Header.Add(relationHeader, "follows=zzzzzzzz")
	if err := rs.addFromRequest(req); err == nil {
		t.Fatal("one relation past the cap was accepted")
	} else {
		got := reasons(t, err)
		if len(got) != 1 || got[0] != "relation/too_many" {
			t.Fatalf("violations = %v, want relation/too_many", got)
		}
		if rs.tooMany != errs.LocHeader {
			t.Errorf("too_many location = %q, want header", rs.tooMany)
		}
	}
	if len(rs.relations) != artifact.MaxRelations {
		t.Fatalf("set holds %d relations, want the cap %d", len(rs.relations), artifact.MaxRelations)
	}
}

// A multipart form field carries relations the same way, and a field too long to
// hold only valid entries is refused whole rather than cut.
func TestRelationFormField(t *testing.T) {
	var rs relationSet
	rs.addFormField(strings.NewReader("reply_to=aaa111,derived_from=bbb222"))
	if err := rs.err(); err != nil {
		t.Fatalf("addFormField: %v", err)
	}
	if len(rs.relations) != 2 {
		t.Fatalf("relations = %+v, want 2", rs.relations)
	}

	var big relationSet
	big.addFormField(strings.NewReader("reply_to=" + strings.Repeat("a", maxRelationFieldBytes)))
	if err := big.err(); err == nil {
		t.Fatal("an over-long relation field was accepted")
	} else if got := reasons(t, err); len(got) != 1 || got[0] != "relation/too_long" {
		t.Fatalf("violations = %v, want relation/too_long", got)
	}
}

// Every bad entry on a request is reported, from every source, up to the
// violation cap, so one typo does not hide the next.
func TestRelationReportsEveryBadEntry(t *testing.T) {
	var rs relationSet
	rs.addEntry("nonsense", errs.LocQuery)
	rs.addEntry("mentions=aaa111", errs.LocHeader)
	rs.addEntry("follows=n0t_base62", errs.LocForm)
	err := rs.err()
	got := reasons(t, err)
	want := []string{"relation/invalid_format", "relation.kind/unknown_value", "relation.id/invalid_charset"}
	if len(got) != len(want) {
		t.Fatalf("violations = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("violation %d = %s, want %s", i, got[i], want[i])
		}
	}
}

// A valid traceparent is recorded; anything else, including an absent header, is
// the same as no trace at all and never fails the create.
func TestRequestTrace(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  string
	}{
		{"valid", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
			"4bf92f3577b34da6a3ce929d0e0e4736"},
		{"absent", "", ""},
		{"wrong version", "ff-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", ""},
		{"all-zero trace id", "00-00000000000000000000000000000000-00f067aa0ba902b7-01", ""},
		{"uppercase hex", "00-4BF92F3577B34DA6A3CE929D0E0E4736-00f067aa0ba902b7-01", ""},
		{"too short", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7", ""},
		{"tracestate header is ignored", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
			"4bf92f3577b34da6a3ce929d0e0e4736"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/artifacts", nil)
			if tc.value != "" {
				req.Header.Set(traceparentHeader, tc.value)
			}
			if tc.name == "tracestate header is ignored" {
				req.Header.Set("tracestate", "vendor=thing")
			}
			trace := requestTrace(req)
			if tc.want == "" {
				if trace != nil {
					t.Fatalf("trace = %+v, want none", trace)
				}
				return
			}
			if trace == nil {
				t.Fatal("trace = nil, want the parsed context")
			}
			if got := trace.TraceHex(); got != tc.want {
				t.Errorf("trace_id = %s, want %s", got, tc.want)
			}
		})
	}
}
