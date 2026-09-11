package db

import (
	"regexp"
	"strconv"
	"testing"

	"github.com/joestump/cairn/internal/artifact"
)

// TestArtifactTagsConstraintMatchesDomain pins the tag-count ceiling to one
// number across the two layers that enforce it: 0015's CHECK and
// artifact.MaxTags. If they drift, one layer rejects what the other accepts.
//
// Governing: ADR-0018 (Client-Asserted Artifact Tags), SPEC-0002 REQ "Artifact Tags"
func TestArtifactTagsConstraintMatchesDomain(t *testing.T) {
	raw, err := migrationsFS.ReadFile("migrations/0015_artifact_tags.sql")
	if err != nil {
		t.Fatalf("read 0015: %v", err)
	}
	m := regexp.MustCompile(`cardinality\(tags\)\s*<=\s*(\d+)`).FindSubmatch(stripSQLComments(raw))
	if m == nil {
		t.Fatal("0015 no longer bounds cardinality(tags); the domain still does, so the two layers have drifted")
	}
	if got, want := string(m[1]), strconv.Itoa(artifact.MaxTags); got != want {
		t.Fatalf("0015 bounds tags at %s, artifact.MaxTags is %s", got, want)
	}
}
