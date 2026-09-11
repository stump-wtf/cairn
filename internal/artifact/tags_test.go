package artifact

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/joestump/cairn/internal/errs"
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
