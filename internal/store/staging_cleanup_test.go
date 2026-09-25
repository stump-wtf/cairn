package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/objectstore"
)

// memStore rebinds the DB pool from newTestStore to a fresh in-memory object
// store so a test can enumerate object keys directly. newTestStore may pick a
// real S3 backend from the environment; the staging-leak assertions need the
// enumerable Memory store, so we force it here while keeping the (migrated,
// truncated) Postgres pool the success paths require.
func memStore(t *testing.T) (*Store, *objectstore.Memory) {
	t.Helper()
	_, pool := newTestStore(t, Options{}) // skips without CAIRN_TEST_DATABASE_URL
	mem := objectstore.NewMemory()
	return New(pool, mem, Options{}), mem
}

// assertNoStagingLeak fails if any staging/<rand> object survived. After a
// successful ingest the only objects left must be content-addressed blobs; a
// leftover staging key is exactly the orphan this fix removes.
//
// Governing: SPEC-0002 REQ "Content Addressing and Blobs".
func assertNoStagingLeak(t *testing.T, mem *objectstore.Memory) {
	t.Helper()
	if leaked := mem.KeysWithPrefix("staging/"); len(leaked) != 0 {
		t.Fatalf("staging objects leaked after success: %v", leaked)
	}
}

// TestNoStagingLeakOnSuccess asserts that no staging object remains after any
// successful ingest path: (a) a new-blob create, (b) a dedup-hit create of the
// same bytes, and (c) a bundle create. Before this fix the committed-gated
// cleanup left one orphan staging/<rand> object behind on each of these paths.
//
// Governing: SPEC-0002 REQ "Content Addressing and Blobs".
func TestNoStagingLeakOnSuccess(t *testing.T) {
	ctx := context.Background()

	t.Run("new blob create", func(t *testing.T) {
		s, mem := memStore(t)
		if _, err := s.CreateArtifact(ctx, input([]byte("brand new bytes"))); err != nil {
			t.Fatalf("create: %v", err)
		}
		assertNoStagingLeak(t, mem)
	})

	t.Run("dedup-hit create", func(t *testing.T) {
		s, mem := memStore(t)
		body := []byte("identical bytes ingested twice")
		if _, err := s.CreateArtifact(ctx, input(body)); err != nil {
			t.Fatalf("create 1: %v", err)
		}
		// Second create hits object-level dedup (blob object already exists), so
		// its staging object is never promoted — the path that leaked hardest.
		if _, err := s.CreateArtifact(ctx, input(body)); err != nil {
			t.Fatalf("create 2 (dedup): %v", err)
		}
		assertNoStagingLeak(t, mem)
		// Sanity: exactly one content-addressed blob object exists for the bytes.
		if blobs := mem.KeysWithPrefix("blobs/"); len(blobs) != 1 {
			t.Fatalf("dedup: want 1 blob object, got %d: %v", len(blobs), blobs)
		}
	})

	t.Run("bundle create", func(t *testing.T) {
		s, mem := memStore(t)
		in := CreateBundleInput{
			Title: "bundle",
			Members: []MemberInput{
				{Name: "a.txt", Body: strings.NewReader("member a")},
				{Name: "b.txt", Body: strings.NewReader("member b")},
			},
			Provenance: artifact.Provenance{CreatedByUserID: testOwner, ActorID: "u1", Channel: artifact.ChannelCLI, CapturedAt: time.Now()},
			Access:     artifact.AccessPolicy{OwnerUserID: testOwner, Visibility: artifact.VisibilityLink},
			ExpiresAt:  time.Now().Add(time.Hour),
		}
		if _, err := s.CreateBundle(ctx, in); err != nil {
			t.Fatalf("bundle: %v", err)
		}
		assertNoStagingLeak(t, mem)
	})
}
