package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/db"
	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/objectstore"
)

// newTestStore returns a Store backed by the Postgres DSN in
// CAIRN_TEST_DATABASE_URL (skipping otherwise) and either a real S3-compatible
// object store (CAIRN_TEST_S3_ENDPOINT) or an in-memory one. It migrates and
// truncates so each test starts clean.
// schemaSeq disambiguates test schemas created within the same nanosecond.
var schemaSeq atomic.Int64

func newTestStore(t *testing.T, o Options) (*Store, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("CAIRN_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set CAIRN_TEST_DATABASE_URL to run store integration tests")
	}
	ctx := context.Background()

	// A private, per-test schema dropped on cleanup, matching every other
	// DB-using package here. This package was the last one still truncating
	// shared tables in `public`: `go test ./...` runs packages concurrently
	// (the Makefile passes no -p 1) against one database, so a TRUNCATE ...
	// CASCADE reached outside this package's own rows by construction. A fresh
	// schema starts empty, so no TRUNCATE is needed and none of that is
	// possible.
	//
	// search_path is set as a CONNECTION runtime parameter rather than executed
	// once after connecting: pgxpool opens connections lazily, so a SET run on
	// the first connection would leave later ones pointing at `public` under
	// concurrency — passing locally and failing unpredictably in CI.
	schema := fmt.Sprintf("store_test_%d_%d", time.Now().UnixNano(), schemaSeq.Add(1))

	admin, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := admin.Exec(ctx, fmt.Sprintf(`CREATE SCHEMA %q`, schema)); err != nil {
		admin.Close()
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), fmt.Sprintf(`DROP SCHEMA %q CASCADE`, schema)); err != nil {
			t.Errorf("drop schema: %v", err)
		}
		admin.Close()
	})

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	var obj objectstore.ObjectStore
	if ep := os.Getenv("CAIRN_TEST_S3_ENDPOINT"); ep != "" {
		m, err := objectstore.NewMinIO(ctx, objectstore.MinIOConfig{
			Endpoint:  ep,
			AccessKey: os.Getenv("CAIRN_TEST_S3_ACCESS_KEY"),
			SecretKey: os.Getenv("CAIRN_TEST_S3_SECRET_KEY"),
			Bucket:    envOr("CAIRN_TEST_S3_BUCKET", "cairn"),
			Region:    "us-east-1",
		})
		if err != nil {
			t.Fatalf("minio: %v", err)
		}
		obj = m
	} else {
		obj = objectstore.NewMemory()
	}

	t.Cleanup(pool.Close)
	return New(pool, obj, o), pool
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func input(body []byte) CreateArtifactInput {
	return CreateArtifactInput{
		ShareType:  artifact.TypeFile,
		Title:      "test",
		Body:       bytes.NewReader(body),
		Provenance: artifact.Provenance{ActorID: "u1", Channel: artifact.ChannelCLI, CapturedAt: time.Now()},
		Access:     artifact.AccessPolicy{OwnerID: "u1", Visibility: artifact.VisibilityLink},
		ExpiresAt:  time.Now().Add(time.Hour),
	}
}

func TestCreateReadRoundTrip(t *testing.T) {
	s, _ := newTestStore(t, Options{})
	ctx := context.Background()
	body := []byte("round trip through storage and back")

	art, err := s.CreateArtifact(ctx, input(body))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(art.PublicID) != 8 {
		t.Fatalf("public id %q length = %d, want 8", art.PublicID, len(art.PublicID))
	}
	if art.BodySHA256 != sha256Hex(body) {
		t.Fatalf("body sha = %s, want %s", art.BodySHA256, sha256Hex(body))
	}
	// public id must never equal the content hash (ADR-0005 decoupling).
	if art.PublicID == art.BodySHA256 {
		t.Fatal("public id must not equal the content hash")
	}

	got, err := s.GetByPublicID(ctx, art.PublicID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != art.ID || got.Title != "test" || got.Provenance.Channel != artifact.ChannelCLI {
		t.Fatalf("resolved artifact mismatch: %+v", got)
	}

	rc, info, err := s.OpenBody(ctx, art.PublicID)
	if err != nil {
		t.Fatalf("open body: %v", err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !bytes.Equal(data, body) {
		t.Fatal("downloaded body differs from uploaded body")
	}
	if sha256Hex(data) != info.SHA256 {
		t.Fatalf("round-trip hash %s != stored checksum %s", sha256Hex(data), info.SHA256)
	}
}

func TestDedupIdenticalBytes(t *testing.T) {
	s, pool := newTestStore(t, Options{})
	ctx := context.Background()
	body := []byte("identical bytes stored once")

	a1, err := s.CreateArtifact(ctx, input(body))
	if err != nil {
		t.Fatalf("create 1: %v", err)
	}
	a2, err := s.CreateArtifact(ctx, input(body))
	if err != nil {
		t.Fatalf("create 2: %v", err)
	}
	if a1.PublicID == a2.PublicID {
		t.Fatal("two artifacts must have distinct public ids")
	}
	if a1.BodySHA256 != a2.BodySHA256 {
		t.Fatal("identical bytes must share one content hash")
	}

	var blobCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM blobs WHERE sha256 = $1`, a1.BodySHA256).Scan(&blobCount); err != nil {
		t.Fatalf("count blobs: %v", err)
	}
	if blobCount != 1 {
		t.Fatalf("expected exactly 1 blob row, got %d", blobCount)
	}
}

func TestUniformNotFound(t *testing.T) {
	s, _ := newTestStore(t, Options{})
	ctx := context.Background()

	_, err := s.GetByPublicID(ctx, "nonexist")
	if errs.CodeOf(err) != errs.CodeNotFound {
		t.Fatalf("unknown id code = %q, want not_found", errs.CodeOf(err))
	}

	// An expired artifact resolves identically to a never-existed one.
	in := input([]byte("already expired"))
	in.ExpiresAt = time.Now().Add(-time.Minute)
	art, err := s.CreateArtifact(ctx, in)
	if err != nil {
		t.Fatalf("create expired: %v", err)
	}
	_, err = s.GetByPublicID(ctx, art.PublicID)
	if errs.CodeOf(err) != errs.CodeNotFound {
		t.Fatalf("expired id code = %q, want not_found", errs.CodeOf(err))
	}
}

func TestHashMismatchRejected(t *testing.T) {
	s, pool := newTestStore(t, Options{})
	ctx := context.Background()

	in := input([]byte("body with a wrong declared checksum"))
	in.ExpectedSHA256 = "0000000000000000000000000000000000000000000000000000000000000000"
	_, err := s.CreateArtifact(ctx, in)
	if err == nil {
		t.Fatal("expected a checksum mismatch error")
	}
	if errs.CodeOf(err) != errs.CodeValidation {
		t.Fatalf("code = %q, want validation_failed", errs.CodeOf(err))
	}
	if !errors.Is(err, errs.ErrChecksumMismatch) {
		t.Fatalf("err = %v, want it to still match errs.ErrChecksumMismatch", err)
	}
	// The error is logged, so it must not carry the received body's digest.
	if got := fmt.Sprintf("%x", sha256.Sum256([]byte("body with a wrong declared checksum"))); strings.Contains(err.Error(), got) {
		t.Fatalf("checksum error carries the received body's SHA-256: %v", err)
	}
	vs := errs.ViolationsOf(err)
	if len(vs) != 1 || vs[0].Field != ChecksumHeader || vs[0].Location != errs.LocHeader ||
		vs[0].Reason != errs.ReasonChecksum || vs[0].Value == nil || *vs[0].Value != in.ExpectedSHA256 {
		t.Fatalf("violations = %+v, want one checksum_mismatch on %s echoing the sent digest", vs, ChecksumHeader)
	}

	var artifacts int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM artifacts`).Scan(&artifacts); err != nil {
		t.Fatalf("count artifacts: %v", err)
	}
	if artifacts != 0 {
		t.Fatalf("a rejected upload must persist no artifact, found %d", artifacts)
	}
}

// SPEC-0019 VE-4: every member over the upload cap is named, not only the
// first, and nothing is persisted.
func TestBundleOversizeMembersReportedTogether(t *testing.T) {
	s, pool := newTestStore(t, Options{MaxUploadBytes: 8})
	ctx := context.Background()

	in := CreateBundleInput{
		Provenance: artifact.Provenance{ActorID: "u1", Channel: artifact.ChannelCLI, CapturedAt: time.Now()},
		Access:     artifact.AccessPolicy{OwnerID: "u1", Visibility: artifact.VisibilityLink},
		ExpiresAt:  time.Now().Add(time.Hour),
		Members: []MemberInput{
			{Name: "big1", Body: bytes.NewReader(bytes.Repeat([]byte("a"), 64))},
			{Name: "ok", Body: bytes.NewReader([]byte("fine"))},
			{Name: "big2", Body: bytes.NewReader(bytes.Repeat([]byte("b"), 64))},
		},
	}
	_, err := s.CreateBundle(ctx, in)
	if errs.CodeOf(err) != errs.CodePayloadTooLarge || !errors.Is(err, errs.ErrTooLarge) {
		t.Fatalf("err = %v (code %q), want payload_too_large", err, errs.CodeOf(err))
	}
	vs := errs.ViolationsOf(err)
	if len(vs) != 2 || vs[0].Field != "members[0].content" || vs[1].Field != "members[2].content" {
		t.Fatalf("violations = %+v, want members[0] and members[2]", vs)
	}
	for _, v := range vs {
		if v.Reason != errs.ReasonTooLarge || v.Limit != int64(8) || v.Unit != errs.UnitBytes || v.Value != nil {
			t.Fatalf("violation = %+v, want too_large 8 bytes with no echo", v)
		}
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM artifacts`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("a refused bundle persisted %d artifacts", n)
	}
}

// TestAtomicFinalizeRollback drives the multi-step mutation (upsert blob →
// insert artifact) to fail at the artifact step and asserts the whole
// transaction rolls back — no blob row and no artifact survive.
func TestAtomicFinalizeRollback(t *testing.T) {
	// A NewID that returns an empty id makes the aggregate invariant check fail
	// after the blob row has been inserted within the transaction.
	s, pool := newTestStore(t, Options{NewID: func() (string, error) { return "", nil }})
	ctx := context.Background()

	_, err := s.CreateArtifact(ctx, input([]byte("should roll back entirely")))
	if err == nil {
		t.Fatal("expected the create to fail")
	}

	for _, table := range []string{"artifacts", "blobs"} {
		var n int
		if err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s`, table)).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != 0 {
			t.Fatalf("%s should be empty after rollback, found %d", table, n)
		}
	}
}

func TestPublicIDCollisionRetry(t *testing.T) {
	ids := []string{"AAAAAAAA", "AAAAAAAA", "BBBBBBBB"}
	i := 0
	next := func() (string, error) {
		v := ids[i]
		i++
		return v, nil
	}
	s, _ := newTestStore(t, Options{NewID: next})
	ctx := context.Background()

	a1, err := s.CreateArtifact(ctx, input([]byte("first")))
	if err != nil {
		t.Fatalf("create 1: %v", err)
	}
	if a1.PublicID != "AAAAAAAA" {
		t.Fatalf("first id = %q, want AAAAAAAA", a1.PublicID)
	}

	a2, err := s.CreateArtifact(ctx, input([]byte("second")))
	if err != nil {
		t.Fatalf("create 2: %v", err)
	}
	if a2.PublicID != "BBBBBBBB" {
		t.Fatalf("second id = %q, want BBBBBBBB after collision retry", a2.PublicID)
	}
}

// TestConcurrentCreate exercises concurrent streaming ingest under the race
// detector; every artifact must land with a distinct public id.
func TestConcurrentCreate(t *testing.T) {
	s, _ := newTestStore(t, Options{})
	ctx := context.Background()

	const n = 24
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		ids  = make(map[string]struct{}, n)
		errc = make(chan error, n)
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			art, err := s.CreateArtifact(ctx, input([]byte(fmt.Sprintf("concurrent body %d", i))))
			if err != nil {
				errc <- err
				return
			}
			mu.Lock()
			ids[art.PublicID] = struct{}{}
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	close(errc)
	for err := range errc {
		t.Fatalf("concurrent create: %v", err)
	}
	if len(ids) != n {
		t.Fatalf("expected %d distinct public ids, got %d", n, len(ids))
	}
}
