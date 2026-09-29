package artifact

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/cairn/internal/errs"
)

// Governing: ADR-0018 (Client-Asserted Artifact Tags), SPEC-0002 REQ "Artifact Tags"

func TestNormalizeTags(t *testing.T) {
	distinct := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("t%d", i)
		}
		return out
	}
	convention := []string{
		"handoff", "lane:auto", "size:xl", "repo:stump.wtf/cairn", "issue:stump.wtf/cairn#42",
		"source:crush/2026-09-11t08.00z", "reply:cairn-comment",
	}
	tests := []struct {
		name    string
		raw     []string
		want    []string
		wantErr bool
	}{
		{"nil", nil, nil, false},
		{"empty", []string{}, nil, false},
		{"handoff convention", convention, convention, false},
		{"dedupe keeps first-occurrence order", []string{"lane:m", "handoff", "lane:m", "handoff"}, []string{"lane:m", "handoff"}, false},
		{"32 distinct", distinct(MaxTags), distinct(MaxTags), false},
		{"33 distinct", distinct(MaxTags + 1), nil, true},
		{"many repeats of one tag count once", slices.Repeat([]string{"handoff"}, 100), []string{"handoff"}, false},
		{"64 bytes", []string{strings.Repeat("t", MaxTagBytes)}, []string{strings.Repeat("t", MaxTagBytes)}, false},
		{"65 bytes", []string{strings.Repeat("t", MaxTagBytes+1)}, nil, true},
		{"empty tag", []string{"handoff", ""}, nil, true},
		{"uppercase rejected, not folded", []string{"Handoff"}, nil, true},
		{"space", []string{"lane auto"}, nil, true},
		{"comma", []string{"a,b"}, nil, true},
		{"equals", []string{"lane=auto"}, nil, true},
		{"non-ASCII", []string{"café"}, nil, true},
		{"control character", []string{"a\nb"}, nil, true},
		{"one bad tag rejects the list", []string{"handoff", "Lane:m"}, nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeTags(tc.raw)
			if tc.wantErr {
				if errs.CodeOf(err) != errs.CodeValidation {
					t.Fatalf("NormalizeTags = %v, %v; want a validation error", got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("NormalizeTags = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestNormalizeTagsDoesNotAliasInput: the result is a fresh slice, so a caller
// mutating its request afterwards cannot reach into a stored artifact.
func TestNormalizeTagsDoesNotAliasInput(t *testing.T) {
	raw := []string{"handoff", "lane:s"}
	got, err := NormalizeTags(raw)
	if err != nil {
		t.Fatal(err)
	}
	raw[0] = "mutated"
	if got[0] != "handoff" {
		t.Fatalf("NormalizeTags aliased its input: %q", got)
	}
}

// TestArtifactValidateEnforcesTags: normalized tags are an aggregate invariant,
// not only an adapter check, so no create path can persist a bad or repeated tag.
func TestArtifactValidateEnforcesTags(t *testing.T) {
	a := artifactForTags()
	a.Tags = []string{"handoff", "lane:auto"}
	if err := a.Validate(); err != nil {
		t.Fatalf("valid tags rejected: %v", err)
	}
	for _, bad := range [][]string{{"Bad Tag"}, {"handoff", "handoff"}} {
		a.Tags = bad
		if err := a.Validate(); errs.CodeOf(err) != errs.CodeValidation {
			t.Errorf("Validate() with tags %q = %v, want a validation error", bad, err)
		}
	}
}

func artifactForTags() *Artifact {
	return &Artifact{
		PublicID:   "abc12",
		ShareType:  TypeFile,
		BodySHA256: "deadbeef",
		Provenance: Provenance{ActorID: "u1", Channel: ChannelMCP, CapturedAt: time.Unix(1, 0)},
		Access:     AccessPolicy{OwnerID: "u1", Visibility: VisibilityLink},
		ExpiresAt:  time.Unix(100, 0),
	}
}

// TestNormalizeTagsReportsEveryBadTag: each failing tag is its own violation on
// tags[i], with the reason that names the exact fix (SPEC-0019 VE-1, VE-4).
//
// Governing: ADR-0025, SPEC-0019 VE-1, VE-3, VE-4
func TestNormalizeTagsReportsEveryBadTag(t *testing.T) {
	_, err := NormalizeTags([]string{"handoff", "Size:M", "lane m", "", strings.Repeat("t", MaxTagBytes+1)})
	vs := errs.ViolationsOf(err)
	type want struct {
		field  string
		reason errs.Reason
		value  string
	}
	wants := []want{
		{"tags[1]", errs.ReasonUppercase, "Size:M"},
		{"tags[2]", errs.ReasonInvalidCharset, "lane m"},
		{"tags[3]", errs.ReasonRequired, ""},
		{"tags[4]", errs.ReasonTooLong, strings.Repeat("t", errs.MaxValueBytes)},
	}
	if len(vs) != len(wants) {
		t.Fatalf("got %d violations %+v, want %d", len(vs), vs, len(wants))
	}
	for i, w := range wants {
		v := vs[i]
		if v.Field != w.field || v.Reason != w.reason || v.Location != errs.LocBody {
			t.Errorf("violation %d = %+v, want field %s reason %s", i, v, w.field, w.reason)
		}
		got := ""
		if v.Value != nil {
			got = *v.Value
		}
		if got != w.value {
			t.Errorf("violation %d value = %q, want %q", i, got, w.value)
		}
	}
	if vs[2].Limit != nil || vs[2].Value != nil {
		t.Errorf("an empty tag carries no limit and has nothing to echo: %+v", vs[2])
	}
	if l := vs[len(vs)-1]; l.Limit != MaxTagBytes || l.Unit != errs.UnitBytes {
		t.Errorf("too_long limit = %v %s, want %d bytes", l.Limit, l.Unit, MaxTagBytes)
	}
}

func TestNormalizeTagsTooManyIsOneViolation(t *testing.T) {
	raw := make([]string, MaxTags+5)
	for i := range raw {
		raw[i] = fmt.Sprintf("t%d", i)
	}
	vs := errs.ViolationsOf(func() error { _, err := NormalizeTags(raw); return err }())
	if len(vs) != 1 || vs[0].Reason != errs.ReasonTooMany || vs[0].Field != "tags" ||
		vs[0].Limit != MaxTags || vs[0].Unit != errs.UnitCount {
		t.Fatalf("violations = %+v, want one too_many on tags with limit %d", vs, MaxTags)
	}
}

// An uppercase letter beside a disallowed character is a charset problem:
// lower-casing alone would not fix it.
func TestCheckTagCharsetBeatsUppercase(t *testing.T) {
	if inv := CheckTag("Lane m", "tag", errs.LocHeader); inv == nil || inv.Violations[0].Reason != errs.ReasonInvalidCharset {
		t.Fatalf("CheckTag = %+v, want invalid_charset", inv)
	}
	if inv := CheckTag("lane:m", "tag", errs.LocHeader); inv != nil {
		t.Fatalf("a valid tag reported %+v", inv)
	}
	if err := ValidateTag("handoff"); err != nil {
		t.Fatalf("ValidateTag(valid) = %v, want a nil error (not a typed nil)", err)
	}
}
