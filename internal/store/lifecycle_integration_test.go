package store

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/joestump/cairn/internal/artifact"
	"github.com/joestump/cairn/internal/errs"
)

func TestIntegrationListBinKeyset(t *testing.T) {
	s, _ := newTestStore(t, Options{})
	ctx := context.Background()

	const n = 5
	want := map[string]bool{}
	for i := 0; i < n; i++ {
		a, err := s.CreateArtifact(ctx, input([]byte(fmt.Sprintf("bin-body-%d", i))))
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		want[a.PublicID] = true
	}

	seen := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		page, err := s.ListBin(ctx, "u1", cursor, 2)
		if err != nil {
			t.Fatalf("list bin: %v", err)
		}
		for _, a := range page.Artifacts {
			if seen[a.PublicID] {
				t.Fatalf("keyset duplicated %s", a.PublicID)
			}
			seen[a.PublicID] = true
		}
		pages++
		if pages > n+2 {
			t.Fatal("pagination did not terminate")
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(seen) != n {
		t.Fatalf("saw %d artifacts across pages, want %d (skipped or lost rows)", len(seen), n)
	}
	for id := range want {
		if !seen[id] {
			t.Fatalf("artifact %s missing from paginated Bin", id)
		}
	}

	// Scoping: another owner sees an empty Bin.
	other, err := s.ListBin(ctx, "someone-else", "", 10)
	if err != nil {
		t.Fatalf("list other bin: %v", err)
	}
	if len(other.Artifacts) != 0 {
		t.Fatalf("cross-owner Bin leaked %d artifacts", len(other.Artifacts))
	}
}

func TestIntegrationDeleteAndBlobGC(t *testing.T) {
	s, pool := newTestStore(t, Options{})
	ctx := context.Background()

	sharedBody := []byte("shared body referenced twice")
	sha := sha256Hex(sharedBody)

	a1, err := s.CreateArtifact(ctx, input(sharedBody))
	if err != nil {
		t.Fatalf("create a1: %v", err)
	}
	a2, err := s.CreateArtifact(ctx, input(sharedBody))
	if err != nil {
		t.Fatalf("create a2: %v", err)
	}

	blobCount := func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM blobs WHERE sha256 = $1`, sha).Scan(&n); err != nil {
			t.Fatalf("count blobs: %v", err)
		}
		return n
	}

	// Delete a1: the shared blob must survive (a2 still references it).
	if err := s.DeleteArtifact(ctx, a1.PublicID, "u1"); err != nil {
		t.Fatalf("delete a1: %v", err)
	}
	if _, err := s.GetByPublicID(ctx, a1.PublicID); errs.CodeOf(err) != errs.CodeNotFound {
		t.Fatalf("a1 after delete: code %q, want not_found", errs.CodeOf(err))
	}
	if blobCount() != 1 {
		t.Fatalf("shared blob GC'd while still referenced by a2")
	}

	// Delete a2: now the blob's refcount hits zero and it is GC'd.
	if err := s.DeleteArtifact(ctx, a2.PublicID, "u1"); err != nil {
		t.Fatalf("delete a2: %v", err)
	}
	if blobCount() != 0 {
		t.Fatalf("blob not GC'd after last reference removed")
	}

	// A non-owner delete is a uniform not-found and changes nothing.
	a3, err := s.CreateArtifact(ctx, input([]byte("owned by u1")))
	if err != nil {
		t.Fatalf("create a3: %v", err)
	}
	if err := s.DeleteArtifact(ctx, a3.PublicID, "intruder"); errs.CodeOf(err) != errs.CodeNotFound {
		t.Fatalf("non-owner delete: code %q, want not_found", errs.CodeOf(err))
	}
	if _, err := s.GetByPublicID(ctx, a3.PublicID); err != nil {
		t.Fatalf("a3 must survive a non-owner delete attempt: %v", err)
	}
}

func bundleInput(members ...MemberInput) CreateBundleInput {
	return CreateBundleInput{
		Title:      "bundle",
		Members:    members,
		Provenance: artifact.Provenance{ActorID: "u1", Channel: artifact.ChannelCLI, CapturedAt: time.Now()},
		Access:     artifact.AccessPolicy{OwnerID: "u1", Visibility: artifact.VisibilityLink},
		ExpiresAt:  time.Now().Add(time.Hour),
	}
}

func TestIntegrationCreateBundleAndMemberRead(t *testing.T) {
	s, pool := newTestStore(t, Options{})
	ctx := context.Background()

	art, err := s.CreateBundle(ctx, bundleInput(
		MemberInput{Name: "a.txt", Body: bytes.NewReader([]byte("AAA")), DeclaredMediaType: "text/plain"},
		MemberInput{Name: "b.txt", Body: bytes.NewReader([]byte("BBBB")), DeclaredMediaType: "text/plain"},
	))
	if err != nil {
		t.Fatalf("create bundle: %v", err)
	}
	if art.ShareType != artifact.TypeBundle {
		t.Fatalf("share type = %q, want bundle", art.ShareType)
	}
	if art.BodySHA256 != "" {
		t.Fatalf("bundle must have no single body, got %q", art.BodySHA256)
	}

	var members int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM bundle_members WHERE bundle_id = $1`, art.ID).Scan(&members); err != nil {
		t.Fatalf("count members: %v", err)
	}
	if members != 2 {
		t.Fatalf("members = %d, want 2", members)
	}

	// Member addressable as <id>/<name>, re-verifiable against its checksum.
	rc, info, err := s.OpenMember(ctx, art.PublicID, "a.txt")
	if err != nil {
		t.Fatalf("open member: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "AAA" {
		t.Fatalf("member bytes = %q, want AAA", got)
	}
	if info.SHA256 != sha256Hex([]byte("AAA")) {
		t.Fatalf("member checksum mismatch")
	}

	// Unknown member name and members of non-bundles are uniform not-found.
	if _, _, err := s.OpenMember(ctx, art.PublicID, "missing.txt"); errs.CodeOf(err) != errs.CodeNotFound {
		t.Fatalf("missing member: code %q, want not_found", errs.CodeOf(err))
	}
	single, err := s.CreateArtifact(ctx, input([]byte("not a bundle")))
	if err != nil {
		t.Fatalf("create single: %v", err)
	}
	if _, _, err := s.OpenMember(ctx, single.PublicID, "a.txt"); errs.CodeOf(err) != errs.CodeNotFound {
		t.Fatalf("member of non-bundle: code %q, want not_found", errs.CodeOf(err))
	}
}
