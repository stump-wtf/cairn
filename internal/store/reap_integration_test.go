package store

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/errs"
)

// inputTTL is input() with an explicit expiry so a test can place an artifact on
// either side of the reaper's expires_at boundary.
func inputTTL(body []byte, expiresAt time.Time) CreateArtifactInput {
	in := input(body)
	in.ExpiresAt = expiresAt
	return in
}

// blobObjectExists reports whether the content-addressed object for sha is
// present in object storage (independent of the DB row).
func blobObjectExists(t *testing.T, s *Store, sha string) bool {
	t.Helper()
	ok, err := s.obj.Stat(context.Background(), shardedKey(sha))
	if err != nil {
		t.Fatalf("stat object %s: %v", sha, err)
	}
	return ok
}

func blobRowExists(t *testing.T, pool *pgxpool.Pool, sha string) bool {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM blobs WHERE sha256 = $1`, sha).Scan(&n); err != nil {
		t.Fatalf("count blob rows %s: %v", sha, err)
	}
	return n > 0
}

func readBody(t *testing.T, s *Store, publicID string) []byte {
	t.Helper()
	rc, _, err := s.OpenBody(context.Background(), publicID)
	if err != nil {
		t.Fatalf("open body %s: %v", publicID, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read body %s: %v", publicID, err)
	}
	return b
}

// TestReaperExpiredArtifactReaped: a past-expiry artifact is hard-deleted, its
// id thereafter 404s, and its now-unreferenced blob (row + object) is GC'd.
//
// Governing: SPEC-0009 REQ "Hard Delete on Expiry via Reference-Counted Reaper".
func TestReaperExpiredArtifactReaped(t *testing.T) {
	s, pool := newTestStore(t, Options{})
	ctx := context.Background()

	body := []byte("expired body to reap")
	sha := sha256Hex(body)
	a, err := s.CreateArtifact(ctx, inputTTL(body, time.Now().Add(-time.Minute)))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	stats, err := s.ReapOnce(ctx, ReaperConfig{Batch: 100, ObjectGrace: time.Hour})
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if stats.ArtifactsDeleted != 1 {
		t.Fatalf("artifacts deleted = %d, want 1", stats.ArtifactsDeleted)
	}
	if stats.BlobsGCd != 1 {
		t.Fatalf("blobs GC'd = %d, want 1", stats.BlobsGCd)
	}
	if _, err := s.GetByPublicID(ctx, a.PublicID); errs.CodeOf(err) != errs.CodeNotFound {
		t.Fatalf("reaped id: code %q, want not_found", errs.CodeOf(err))
	}
	if blobRowExists(t, pool, sha) {
		t.Fatal("blob row survived after last reference reaped")
	}
	if blobObjectExists(t, s, sha) {
		t.Fatal("blob object survived after last reference reaped")
	}
}

// TestReaperSharedBlobSurvivesRefcount: when an expired artifact's body is still
// referenced by a LIVE artifact, the reaper deletes only the expired artifact —
// the shared blob (row + object) and the live body survive.
//
// Governing: SPEC-0009 REQ "Hard Delete on Expiry via Reference-Counted Reaper"
// (scenario "Shared blob survives refcount").
func TestReaperSharedBlobSurvivesRefcount(t *testing.T) {
	s, pool := newTestStore(t, Options{})
	ctx := context.Background()

	body := []byte("shared body, one live one expired")
	sha := sha256Hex(body)
	expired, err := s.CreateArtifact(ctx, inputTTL(body, time.Now().Add(-time.Minute)))
	if err != nil {
		t.Fatalf("create expired: %v", err)
	}
	live, err := s.CreateArtifact(ctx, inputTTL(body, time.Now().Add(time.Hour)))
	if err != nil {
		t.Fatalf("create live: %v", err)
	}

	if _, err := s.ReapOnce(ctx, ReaperConfig{Batch: 100, ObjectGrace: time.Hour}); err != nil {
		t.Fatalf("reap: %v", err)
	}

	if _, err := s.GetByPublicID(ctx, expired.PublicID); errs.CodeOf(err) != errs.CodeNotFound {
		t.Fatalf("expired id: code %q, want not_found", errs.CodeOf(err))
	}
	if !blobRowExists(t, pool, sha) {
		t.Fatal("shared blob row GC'd while a live artifact still references it")
	}
	if !blobObjectExists(t, s, sha) {
		t.Fatal("shared blob object deleted while a live artifact still references it")
	}
	if got := readBody(t, s, live.PublicID); !bytes.Equal(got, body) {
		t.Fatal("live artifact body vanished after reaping a co-referencing expired artifact")
	}
}

// TestReaperDedupAcrossLifecycleBoundary is the REQUIRED lifecycle-boundary test:
// A is created with content C at a short TTL; B dedups against C with a LONGER
// TTL; A expires and is reaped; B's body MUST survive indefinitely. This proves
// (a) blob deletion is reference-counted, not age-based — reaping A never touches
// C because B still references it — and (b) an object whose LastModified is far
// older than any grace window is NEVER deleted while referenced (no age rule on
// blobs/, issue #93 §1). Runs on the in-memory store so the object clock is
// controllable.
//
// Governing: SPEC-0009 REQ "Object-Storage Lifecycle Backstop", REQ "Hard Delete
// on Expiry via Reference-Counted Reaper"; ADR-0008.
func TestReaperDedupAcrossLifecycleBoundary(t *testing.T) {
	s, mem := memStore(t)
	ctx := context.Background()

	content := []byte("content C shared across a lifecycle boundary")
	sha := sha256Hex(content)

	// A: created "long ago" with a short TTL that is already past.
	old := time.Now().Add(-48 * time.Hour)
	mem.SetClock(func() time.Time { return old }) // C's object gets an OLD LastModified
	a, err := s.CreateArtifact(ctx, inputTTL(content, time.Now().Add(-time.Minute)))
	if err != nil {
		t.Fatalf("create A: %v", err)
	}

	// B: dedups against C near A's expiry but with a much LONGER TTL. Its create
	// happens "now" but reuses C's already-old object (dedup skips re-upload).
	mem.SetClock(nil)
	b, err := s.CreateArtifact(ctx, inputTTL(content, time.Now().Add(24*time.Hour)))
	if err != nil {
		t.Fatalf("create B: %v", err)
	}
	if a.BodySHA256 != b.BodySHA256 {
		t.Fatal("A and B must share one content hash")
	}

	// Reap: A is expired; C's object is 48h old — far older than the 1h grace an
	// age rule would use. Refcounting MUST keep C alive because B references it.
	if _, err := s.ReapOnce(ctx, ReaperConfig{Batch: 100, ObjectGrace: time.Hour}); err != nil {
		t.Fatalf("reap: %v", err)
	}

	if _, err := s.GetByPublicID(ctx, a.PublicID); errs.CodeOf(err) != errs.CodeNotFound {
		t.Fatalf("A after reap: code %q, want not_found", errs.CodeOf(err))
	}
	if !blobObjectExists(t, s, sha) {
		t.Fatal("DATA LOSS: aged, dedup-shared object deleted while B still references it (age-based deletion bug)")
	}
	if got := readBody(t, s, b.PublicID); !bytes.Equal(got, content) {
		t.Fatal("DATA LOSS: B's body did not survive A's expiry+reap")
	}

	// Sanity: a second reap with B still live changes nothing (idempotent) and B
	// still resolves.
	if _, err := s.ReapOnce(ctx, ReaperConfig{Batch: 100, ObjectGrace: time.Hour}); err != nil {
		t.Fatalf("reap 2: %v", err)
	}
	if got := readBody(t, s, b.PublicID); !bytes.Equal(got, content) {
		t.Fatal("B body lost on second reap")
	}
}

// TestReaperConcurrentCreateDedupVsSweep is the REQUIRED truly-concurrent race
// test. For each of many iterations it plants a momentarily-orphaned blob for
// content C (object present, blob row present, refcount zero), then runs a
// create-dedup for C concurrently with the reaper's unreferenced-blob Sweep for
// that exact blob. Whichever wins the blob-row lock, the committed artifact's
// body MUST always be retrievable afterward: either the create committed its
// reference first (Sweep's refcount recheck then skips), or the Sweep deleted the
// object first and the create's fresh-row path re-uploaded it. No interleaving
// may lose the body. Runs under `go test -race`.
//
// Governing: SPEC-0009 REQ "Concurrency Safety (Expiry Reaper)" (scenario
// "Concurrent create races the reaper"); ADR-0008 (create/dedup vs reaper TOCTOU).
func TestReaperConcurrentCreateDedupVsSweep(t *testing.T) {
	s, pool := newTestStore(t, Options{})
	ctx := context.Background()

	const iterations = 60
	for i := 0; i < iterations; i++ {
		content := []byte(fmt.Sprintf("race content %d", i))
		sha := sha256Hex(content)

		// Plant a momentarily-orphaned blob: object + zero-refcount row.
		if err := s.obj.Put(ctx, shardedKey(sha), bytes.NewReader(content), -1, "text/plain"); err != nil {
			t.Fatalf("iter %d: put object: %v", i, err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO blobs (sha256, size_bytes, media_type, storage_key)
			 VALUES ($1, $2, $3, $4) ON CONFLICT (sha256) DO NOTHING`,
			sha, int64(len(content)), "text/plain", shardedKey(sha),
		); err != nil {
			t.Fatalf("iter %d: seed blob row: %v", i, err)
		}

		var (
			wg        sync.WaitGroup
			art       *artifact.Artifact
			createErr error
			sweepErr  error
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			art, createErr = s.CreateArtifact(ctx, inputTTL(content, time.Now().Add(time.Hour)))
		}()
		go func() {
			defer wg.Done()
			// The Sweep for C's momentarily-orphaned blob (reaper phase 2).
			_, _, _, sweepErr = s.sweepUnreferencedBlobs(ctx, 100)
		}()
		wg.Wait()

		if createErr != nil {
			t.Fatalf("iter %d: create raced sweep and failed: %v", i, createErr)
		}
		if sweepErr != nil {
			t.Fatalf("iter %d: sweep failed: %v", i, sweepErr)
		}
		// The committed artifact's body MUST be retrievable regardless of who won.
		if got := readBody(t, s, art.PublicID); !bytes.Equal(got, content) {
			t.Fatalf("iter %d: DATA LOSS: committed artifact body not retrievable (got %q)", i, got)
		}
	}
}

// TestReaperPerTypeRefcount proves the cross-table refcount honors ALL four
// blob-bearing tables: a blob referenced only by a trajectory span output, and a
// blob referenced only by a captured webhook body, each survive the Sweep while
// referenced and are GC'd once the reference is removed. (Artifact-body and
// bundle-member refcounting are covered by the create/delete tests.)
//
// Governing: ADR-0008 (dedup across artifacts, bundle_members, spans,
// hook_requests), SPEC-0009 REQ "Hard Delete on Expiry via Reference-Counted
// Reaper".
func TestReaperPerTypeRefcount(t *testing.T) {
	ctx := context.Background()

	t.Run("span output", func(t *testing.T) {
		s, pool := newTestStore(t, Options{})
		host, err := s.CreateArtifact(ctx, input([]byte("span host artifact")))
		if err != nil {
			t.Fatalf("host: %v", err)
		}
		sha := seedOrphanBlob(t, s, []byte("large span output spilled to a blob"))

		var runID int64
		if err := pool.QueryRow(ctx,
			`INSERT INTO runs (artifact_id, started_at) VALUES ($1, now()) RETURNING id`,
			host.ID,
		).Scan(&runID); err != nil {
			t.Fatalf("insert run: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO spans (run_id, span_id, depth, seq, category, start_offset_ms, duration_ms, output_ref_sha256, output_size, stream_seq)
			 VALUES ($1, 'span-1', 0, 0, 'exec', 0, 1, $2, 10, 1)`,
			runID, sha,
		); err != nil {
			t.Fatalf("insert span: %v", err)
		}

		assertSurvivesThenGCd(t, s, pool, sha, func() {
			if _, err := pool.Exec(ctx, `DELETE FROM spans WHERE run_id = $1`, runID); err != nil {
				t.Fatalf("delete span: %v", err)
			}
		})
	})

	t.Run("webhook body", func(t *testing.T) {
		s, pool := newTestStore(t, Options{})
		host, err := s.CreateArtifact(ctx, input([]byte("hook host artifact")))
		if err != nil {
			t.Fatalf("host: %v", err)
		}
		sha := seedOrphanBlob(t, s, []byte("large captured webhook body spilled to a blob"))

		var hookID int64
		if err := pool.QueryRow(ctx,
			`INSERT INTO hooks (artifact_id) VALUES ($1) RETURNING id`, host.ID,
		).Scan(&hookID); err != nil {
			t.Fatalf("insert hook: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO hook_requests (hook_id, seq, method, status, body_ref_sha256, body_size)
			 VALUES ($1, 0, 'POST', 200, $2, 10)`,
			hookID, sha,
		); err != nil {
			t.Fatalf("insert hook_request: %v", err)
		}

		assertSurvivesThenGCd(t, s, pool, sha, func() {
			if _, err := pool.Exec(ctx, `DELETE FROM hook_requests WHERE hook_id = $1`, hookID); err != nil {
				t.Fatalf("delete hook_request: %v", err)
			}
		})
	})
}

// seedOrphanBlob puts an object and its blob row (no references yet) and returns
// the content hash. The caller then wires a reference from a specific table.
func seedOrphanBlob(t *testing.T, s *Store, content []byte) string {
	t.Helper()
	ctx := context.Background()
	sha := sha256Hex(content)
	if err := s.obj.Put(ctx, shardedKey(sha), bytes.NewReader(content), -1, "application/octet-stream"); err != nil {
		t.Fatalf("put orphan object: %v", err)
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO blobs (sha256, size_bytes, media_type, storage_key)
		 VALUES ($1, $2, $3, $4) ON CONFLICT (sha256) DO NOTHING`,
		sha, int64(len(content)), "application/octet-stream", shardedKey(sha),
	); err != nil {
		t.Fatalf("seed blob row: %v", err)
	}
	return sha
}

// assertSurvivesThenGCd checks that a Sweep leaves the blob alone while it is
// referenced, then, after removeRef drops the reference, a Sweep GCs it (row +
// object).
func assertSurvivesThenGCd(t *testing.T, s *Store, pool *pgxpool.Pool, sha string, removeRef func()) {
	t.Helper()
	ctx := context.Background()

	if _, _, _, err := s.sweepUnreferencedBlobs(ctx, 100); err != nil {
		t.Fatalf("sweep (referenced): %v", err)
	}
	if !blobRowExists(t, pool, sha) || !blobObjectExists(t, s, sha) {
		t.Fatal("blob GC'd while still referenced by its typed table")
	}

	removeRef()

	if _, _, _, err := s.sweepUnreferencedBlobs(ctx, 100); err != nil {
		t.Fatalf("sweep (unreferenced): %v", err)
	}
	if blobRowExists(t, pool, sha) {
		t.Fatal("blob row survived after its last typed reference was removed")
	}
	if blobObjectExists(t, s, sha) {
		t.Fatal("blob object survived after its last typed reference was removed")
	}
}

// TestReaperNotYetExpiredUntouched: a live (future-expiry) artifact and its blob
// are never touched by a sweep.
func TestReaperNotYetExpiredUntouched(t *testing.T) {
	s, pool := newTestStore(t, Options{})
	ctx := context.Background()

	body := []byte("still very much alive")
	sha := sha256Hex(body)
	a, err := s.CreateArtifact(ctx, inputTTL(body, time.Now().Add(time.Hour)))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	stats, err := s.ReapOnce(ctx, ReaperConfig{Batch: 100, ObjectGrace: time.Hour})
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if stats.ArtifactsDeleted != 0 || stats.BlobsGCd != 0 {
		t.Fatalf("reaper touched a live artifact: %+v", stats)
	}
	if _, err := s.GetByPublicID(ctx, a.PublicID); err != nil {
		t.Fatalf("live artifact must resolve after reap: %v", err)
	}
	if !blobRowExists(t, pool, sha) || !blobObjectExists(t, s, sha) {
		t.Fatal("live artifact's blob was reaped")
	}
}

// TestReaperIdempotentAndBatchBounded creates many expired artifacts, reaps them
// with a SMALL batch, and asserts (a) a single ReapOnce drains them all across
// its bounded internal passes, (b) the pass terminates, and (c) a second run is a
// clean no-op (idempotent).
func TestReaperIdempotentAndBatchBounded(t *testing.T) {
	s, pool := newTestStore(t, Options{})
	ctx := context.Background()

	const n = 25
	for i := 0; i < n; i++ {
		if _, err := s.CreateArtifact(ctx,
			inputTTL([]byte(fmt.Sprintf("expired %d", i)), time.Now().Add(-time.Minute)),
		); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}

	// Small batch: several bounded passes must still drain the whole backlog.
	stats, err := s.ReapOnce(ctx, ReaperConfig{Batch: 4, MaxBatches: 50, ObjectGrace: time.Hour})
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if stats.ArtifactsDeleted != n {
		t.Fatalf("artifacts deleted = %d, want %d", stats.ArtifactsDeleted, n)
	}

	var remaining int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM artifacts`).Scan(&remaining); err != nil {
		t.Fatalf("count artifacts: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("%d expired artifacts survived a bounded drain", remaining)
	}

	// Idempotent: nothing left to do.
	stats2, err := s.ReapOnce(ctx, ReaperConfig{Batch: 4, ObjectGrace: time.Hour})
	if err != nil {
		t.Fatalf("reap 2: %v", err)
	}
	if stats2.ArtifactsDeleted != 0 || stats2.BlobsGCd != 0 || stats2.OrphanObjects != 0 {
		t.Fatalf("second reap was not a no-op: %+v", stats2)
	}
}

// TestReaperOrphanObjectScan exercises the object-storage backstop: a blobs/
// object with NO DB row older than the grace window is removed; a fresh no-row
// object is kept (could be an in-flight create); and a no-row object is never
// removed while too young. Runs on the in-memory store for clock control.
func TestReaperOrphanObjectScan(t *testing.T) {
	s, mem := memStore(t)
	ctx := context.Background()

	orphan := []byte("rolled-back create debris")
	orphanSHA := sha256Hex(orphan)
	fresh := []byte("in-flight create object")
	freshSHA := sha256Hex(fresh)

	// Age the orphan object well past the grace window; keep the fresh one at now.
	old := time.Now().Add(-2 * time.Hour)
	mem.SetClock(func() time.Time { return old })
	if err := s.obj.Put(ctx, shardedKey(orphanSHA), bytes.NewReader(orphan), -1, ""); err != nil {
		t.Fatalf("put orphan: %v", err)
	}
	mem.SetClock(nil)
	if err := s.obj.Put(ctx, shardedKey(freshSHA), bytes.NewReader(fresh), -1, ""); err != nil {
		t.Fatalf("put fresh: %v", err)
	}

	stats, err := s.ReapOnce(ctx, ReaperConfig{Batch: 100, ObjectGrace: time.Hour})
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if stats.OrphanObjects != 1 {
		t.Fatalf("orphan objects removed = %d, want 1", stats.OrphanObjects)
	}
	if blobObjectExists(t, s, orphanSHA) {
		t.Fatal("aged no-row orphan object was not swept")
	}
	if !blobObjectExists(t, s, freshSHA) {
		t.Fatal("fresh no-row object was swept despite the grace window (could be an in-flight create)")
	}
}
