package webhook

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
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

var schemaSeq atomic.Int64

// newHarness connects to CAIRN_TEST_DATABASE_URL (skipping otherwise), migrates
// into a private per-test schema, and returns a webhook Service backed by a
// real S3-compatible object store when CAIRN_TEST_S3_ENDPOINT is set (falling
// back to an in-memory one otherwise) — mirroring internal/trajectory's and
// internal/store's integration harnesses exactly, so this package's tests
// parallelize safely against the one CI database and exercise the same
// content-addressed spill path a captured body actually takes.
func newHarness(t *testing.T) (*Service, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("CAIRN_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set CAIRN_TEST_DATABASE_URL to run webhook integration tests")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("webhook_test_%d_%d", time.Now().UnixNano(), schemaSeq.Add(1))

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
	t.Cleanup(pool.Close)

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

	svc := NewService(pool, obj, Options{})
	return svc, pool
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// bigBody is a deterministic 20 KiB payload — above the 16 KiB inline
// threshold — so a request carrying it spills to a content-addressed blob,
// mirroring the trajectory test suite's bigOutput fixture.
func bigBody(seed byte) []byte {
	b := bytes.Repeat([]byte{seed}, 20*1024)
	copy(b, []byte(`{"seed":`))
	return b
}

func newEndpoint(t *testing.T, svc *Service, in EndpointInput) *Endpoint {
	t.Helper()
	if in.Provenance == (artifact.Provenance{}) {
		in.Provenance = validProvenance()
	}
	if in.Access == (artifact.AccessPolicy{}) {
		in.Access = validAccess()
	}
	if in.ExpiresAt.IsZero() {
		in.ExpiresAt = future()
	}
	ep, err := svc.CreateEndpoint(context.Background(), in)
	if err != nil {
		t.Fatalf("create endpoint: %v", err)
	}
	return ep
}

// TestIntegrationCreateEndpoint proves an endpoint's metadata round-trips
// exactly and defaults its ring-buffer cap when unset (SPEC-0005 "Webhook
// Endpoint and Two Addresses", ADR-0010 "Ring-Buffer Retention and Caps").
func TestIntegrationCreateEndpoint(t *testing.T) {
	svc, _ := newHarness(t)
	ep := newEndpoint(t, svc, EndpointInput{Title: "checkout webhook"})

	if ep.PublicID == "" {
		t.Fatal("endpoint has no public id")
	}
	if ep.Title != "checkout webhook" {
		t.Fatalf("title = %q, want %q", ep.Title, "checkout webhook")
	}
	if ep.RequestCap != DefaultRequestCap {
		t.Fatalf("request cap = %d, want default %d", ep.RequestCap, DefaultRequestCap)
	}
	if ep.Provenance.ActorID != "joe" || ep.Access.OwnerID != "joe" {
		t.Fatalf("provenance/access did not round-trip: %+v / %+v", ep.Provenance, ep.Access)
	}

	got, err := svc.GetEndpoint(context.Background(), ep.PublicID)
	if err != nil {
		t.Fatalf("get endpoint: %v", err)
	}
	if got.PublicID != ep.PublicID || got.RequestCap != ep.RequestCap {
		t.Fatalf("get endpoint diverges from create: %+v vs %+v", got, ep)
	}

	// An explicit cap is honored.
	custom := newEndpoint(t, svc, EndpointInput{RequestCap: 3})
	if custom.RequestCap != 3 {
		t.Fatalf("custom request cap = %d, want 3", custom.RequestCap)
	}
}

// TestIntegrationCaptureSimulatesInboundRequests proves Capture — the method
// the future ingress will call — records method/path/query/headers(sanitized)
// /status/content-type/body faithfully, assigns a monotonic seq, and stores a
// small body inline while spilling an oversized one to a content-addressed
// blob fetched byte-exactly (SPEC-0005 "Request Capture and Storage", "Body
// stored verbatim and content-addressed").
func TestIntegrationCaptureSimulatesInboundRequests(t *testing.T) {
	svc, _ := newHarness(t)
	ctx := context.Background()
	ep := newEndpoint(t, svc, EndpointInput{})

	r1, err := svc.Capture(ctx, ep.PublicID, CaptureInput{
		Method: "POST", Path: "/webhook", Query: "source=stripe",
		Headers:     map[string][]string{"Content-Type": {"application/json"}, "Authorization": {"Bearer secret"}},
		ContentType: "application/json",
		Body:        []byte(`{"event":"payment.succeeded"}`),
		Status:      DefaultResponseStatus,
	})
	if err != nil {
		t.Fatalf("capture 1: %v", err)
	}
	if r1.Seq != 1 {
		t.Fatalf("first capture seq = %d, want 1", r1.Seq)
	}
	if r1.Status != DefaultResponseStatus {
		t.Fatalf("status = %d, want %d (the fixed response, never payload-controlled)", r1.Status, DefaultResponseStatus)
	}
	if _, sensitive := r1.Headers["authorization"]; sensitive {
		t.Fatal("sanitized headers must not carry Authorization")
	}
	if got := r1.Headers["content-type"]; len(got) != 1 || got[0] != "application/json" {
		t.Fatalf("content-type header = %v, want [application/json]", got)
	}
	if r1.Ref != nil || string(r1.Inline) != `{"event":"payment.succeeded"}` {
		t.Fatalf("small body must be inline, got inline=%q ref=%v", r1.Inline, r1.Ref)
	}

	// A second, oversized body spills to a content-addressed blob.
	body := bigBody('a')
	r2, err := svc.Capture(ctx, ep.PublicID, CaptureInput{
		Method: "POST", Path: "/webhook", ContentType: "application/json",
		Body: body, Status: DefaultResponseStatus,
	})
	if err != nil {
		t.Fatalf("capture 2: %v", err)
	}
	if r2.Seq != 2 {
		t.Fatalf("second capture seq = %d, want 2", r2.Seq)
	}
	if r2.Ref == nil || r2.Inline != nil {
		t.Fatalf("oversized body must spill to a ref, got inline=%v ref=%v", r2.Inline, r2.Ref)
	}
	if r2.Ref.Size != int64(len(body)) {
		t.Fatalf("ref size = %d, want %d", r2.Ref.Size, len(body))
	}

	// Lazy fetch returns the exact bytes for both dispositions.
	rc, info, err := svc.OpenRequestBody(ctx, ep.PublicID, r1.Seq)
	if err != nil {
		t.Fatalf("open body 1: %v", err)
	}
	got1, _ := readAllClose(rc)
	if string(got1) != `{"event":"payment.succeeded"}` || !info.Inline {
		t.Fatalf("inline body round-trip failed: %q info=%+v", got1, info)
	}

	rc, info, err = svc.OpenRequestBody(ctx, ep.PublicID, r2.Seq)
	if err != nil {
		t.Fatalf("open body 2: %v", err)
	}
	got2, _ := readAllClose(rc)
	if !bytes.Equal(got2, body) {
		t.Fatal("spilled body round-trip diverges from the captured bytes")
	}
	if info.SHA256 == "" {
		t.Fatal("spilled body info missing checksum")
	}

	// Metadata is queryable via GetEndpoint/ListRequests/GetRequest without
	// ever touching the bodies (SPEC-0005 "Metadata queryable without reading
	// the body").
	page, err := svc.ListRequests(ctx, ep.PublicID, 0, 0)
	if err != nil {
		t.Fatalf("list requests: %v", err)
	}
	if len(page.Requests) != 2 || page.Requests[0].Seq != 2 || page.Requests[1].Seq != 1 {
		t.Fatalf("list requests = %+v, want newest-first [2,1]", page.Requests)
	}

	single, err := svc.GetRequest(ctx, ep.PublicID, r1.Seq)
	if err != nil {
		t.Fatalf("get request: %v", err)
	}
	if single.Method != "POST" || single.Path != "/webhook" || single.Query != "source=stripe" {
		t.Fatalf("get request = %+v, want method/path/query to round-trip", single)
	}
}

// TestIntegrationRingBufferEvictsOldest proves a capture past the cap evicts
// the oldest record atomically, keeping the endpoint's footprint bounded
// regardless of how many requests it has received in total (SPEC-0005
// "Ring-Buffer Retention and Caps": "Overflow evicts the oldest",
// "Bounded footprint under flood").
func TestIntegrationRingBufferEvictsOldest(t *testing.T) {
	svc, _ := newHarness(t)
	ctx := context.Background()
	ep := newEndpoint(t, svc, EndpointInput{RequestCap: 3})

	var last *Request
	for i := 0; i < 5; i++ {
		r, err := svc.Capture(ctx, ep.PublicID, CaptureInput{
			Method: "GET", Path: fmt.Sprintf("/r%d", i), Status: 200,
		})
		if err != nil {
			t.Fatalf("capture %d: %v", i, err)
		}
		last = r
	}
	if last.Seq != 5 {
		t.Fatalf("last seq = %d, want 5 (seq keeps counting past the cap)", last.Seq)
	}

	page, err := svc.ListRequests(ctx, ep.PublicID, 0, 0)
	if err != nil {
		t.Fatalf("list requests: %v", err)
	}
	if len(page.Requests) != 3 {
		t.Fatalf("buffer size = %d, want capped at 3", len(page.Requests))
	}
	gotSeqs := []int64{page.Requests[0].Seq, page.Requests[1].Seq, page.Requests[2].Seq}
	if gotSeqs[0] != 5 || gotSeqs[1] != 4 || gotSeqs[2] != 3 {
		t.Fatalf("retained seqs = %v, want newest-first [5,4,3]", gotSeqs)
	}

	// The evicted requests (seq 1, 2) are gone, not just unlisted.
	for _, seq := range []int64{1, 2} {
		if _, err := svc.GetRequest(ctx, ep.PublicID, seq); !errors.Is(err, ErrRequestNotFound) {
			t.Fatalf("get evicted seq %d: err = %v, want ErrRequestNotFound", seq, err)
		}
	}
}

// TestIntegrationCaptureUnknownOrExpiredEndpoint proves Capture returns the
// uniform ErrEndpointNotFound for both an unknown id and an id whose artifact
// has expired, so probing an id leaks no signal (ADR-0007 link-capability,
// SPEC-0005 "Unguessable ID & No Enumeration", "Expired endpoint stops
// capturing").
func TestIntegrationCaptureUnknownOrExpiredEndpoint(t *testing.T) {
	svc, pool := newHarness(t)
	ctx := context.Background()

	_, err := svc.Capture(ctx, "zzzzzzzz", CaptureInput{Method: "GET", Status: 200})
	if !errors.Is(err, ErrEndpointNotFound) {
		t.Fatalf("capture unknown endpoint: err = %v, want ErrEndpointNotFound", err)
	}

	// Create a live endpoint, then age it past its expiry directly (CreateEndpoint
	// itself reads the row back through the same expiry-enforcing GetEndpoint, so
	// an already-past ExpiresAt can never be supplied at creation).
	ep := newEndpoint(t, svc, EndpointInput{})
	if _, err := pool.Exec(ctx,
		`UPDATE artifacts SET expires_at = now() - interval '1 hour' WHERE public_id = $1`, ep.PublicID,
	); err != nil {
		t.Fatalf("expire endpoint: %v", err)
	}

	_, err = svc.Capture(ctx, ep.PublicID, CaptureInput{Method: "GET", Status: 200})
	if !errors.Is(err, ErrEndpointNotFound) {
		t.Fatalf("capture expired endpoint: err = %v, want ErrEndpointNotFound", err)
	}
	if _, err := svc.GetEndpoint(ctx, ep.PublicID); !errors.Is(err, ErrEndpointNotFound) {
		t.Fatalf("get expired endpoint: err = %v, want ErrEndpointNotFound", err)
	}
}

// TestIntegrationManagementReadIsLinkScoped proves GetEndpoint/ListRequests/
// GetRequest resolve purely off the unguessable public id — any caller who
// holds the link reads, an unknown id is a uniform not-found, and no read
// operation here depends on which actor originally created the endpoint
// (SPEC-0005 "Read endpoints MUST enforce the ADR-0007 link-capability
// policy").
func TestIntegrationManagementReadIsLinkScoped(t *testing.T) {
	svc, _ := newHarness(t)
	ctx := context.Background()
	ep := newEndpoint(t, svc, EndpointInput{
		Provenance: artifact.Provenance{ActorID: "owner-a", Channel: artifact.ChannelAPI, CapturedAt: time.Now()},
		Access:     artifact.AccessPolicy{OwnerID: "owner-a", Visibility: artifact.VisibilityLink},
	})
	if _, err := svc.Capture(ctx, ep.PublicID, CaptureInput{Method: "GET", Status: 200}); err != nil {
		t.Fatalf("capture: %v", err)
	}

	// A read carries no actor of its own — any holder of the link resolves the
	// same endpoint and buffer (the REST adapter mounts these with no auth
	// middleware at all, mirroring GetRun).
	if _, err := svc.GetEndpoint(ctx, ep.PublicID); err != nil {
		t.Fatalf("link-scoped GetEndpoint: %v", err)
	}
	if _, err := svc.ListRequests(ctx, ep.PublicID, 0, 0); err != nil {
		t.Fatalf("link-scoped ListRequests: %v", err)
	}
	if _, err := svc.GetRequest(ctx, ep.PublicID, 1); err != nil {
		t.Fatalf("link-scoped GetRequest: %v", err)
	}

	// An unknown id is a uniform not-found on every read path — no enumeration
	// signal distinguishes "wrong owner" from "never existed".
	for name, readErr := range map[string]error{
		"GetEndpoint":  mustErr(svc.GetEndpoint(ctx, "zzzzzzzz")),
		"ListRequests": mustErrPage(svc.ListRequests(ctx, "zzzzzzzz", 0, 0)),
	} {
		if !errors.Is(readErr, ErrEndpointNotFound) {
			t.Fatalf("%s(unknown id) = %v, want ErrEndpointNotFound", name, readErr)
		}
	}
}

func mustErr(_ *Endpoint, err error) error       { return err }
func mustErrPage(_ RequestPage, err error) error { return err }

// TestIntegrationCaptureOversizeBodyRejectedNoPersist proves a captured body
// past the hard cap is rejected with the distinct oversize sentinel (mapping
// to payload_too_large) and persists nothing (SPEC-0005 "Request Body Size
// Limits", "Distinct sentinel for oversize").
func TestIntegrationCaptureOversizeBodyRejectedNoPersist(t *testing.T) {
	_, pool := newHarness(t)
	// A second service over the same migrated pool/object store, configured
	// with a tight MaxBodyBytes so the cap is easy to exceed deterministically.
	svc := NewService(pool, objectstore.NewMemory(), Options{MaxBodyBytes: 8})
	ctx := context.Background()
	ep := newEndpoint(t, svc, EndpointInput{})

	_, err := svc.Capture(ctx, ep.PublicID, CaptureInput{
		Method: "POST", Body: []byte("this body is far larger than eight bytes"), Status: 200,
	})
	if !errors.Is(err, errs.ErrTooLarge) {
		t.Fatalf("oversize capture: err = %v, want errs.ErrTooLarge", err)
	}
	if errs.CodeOf(err) != errs.CodePayloadTooLarge {
		t.Fatalf("oversize capture code = %q, want payload_too_large", errs.CodeOf(err))
	}

	page, err := svc.ListRequests(ctx, ep.PublicID, 0, 0)
	if err != nil {
		t.Fatalf("list requests: %v", err)
	}
	if len(page.Requests) != 0 {
		t.Fatalf("buffer after rejected oversize capture = %d, want 0 (nothing persisted)", len(page.Requests))
	}
}

// TestIntegrationConcurrentCapturesKeepSeqMonotonic proves concurrent captures
// on one endpoint serialize on the hook row lock and assign a distinct,
// gap-free seq to each (SPEC-0005 "Concurrent captures keep seq monotonic",
// "Concurrency Safety"). Run under `go test -race` per the SPEC's CI
// requirement.
func TestIntegrationConcurrentCapturesKeepSeqMonotonic(t *testing.T) {
	svc, _ := newHarness(t)
	ctx := context.Background()
	const n = 20
	ep := newEndpoint(t, svc, EndpointInput{RequestCap: n})

	var wg sync.WaitGroup
	seqs := make([]int64, n)
	captureErrs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := svc.Capture(ctx, ep.PublicID, CaptureInput{Method: "GET", Path: fmt.Sprintf("/c%d", i), Status: 200})
			captureErrs[i] = err
			if r != nil {
				seqs[i] = r.Seq
			}
		}(i)
	}
	wg.Wait()

	seen := make(map[int64]bool, n)
	for i, err := range captureErrs {
		if err != nil {
			t.Fatalf("concurrent capture %d: %v", i, err)
		}
		if seen[seqs[i]] {
			t.Fatalf("seq %d assigned twice among concurrent captures: %v", seqs[i], seqs)
		}
		seen[seqs[i]] = true
	}
	for want := int64(1); want <= n; want++ {
		if !seen[want] {
			t.Fatalf("seq %d missing from concurrent captures (gap): %v", want, seqs)
		}
	}
}

func readAllClose(rc io.ReadCloser) ([]byte, error) {
	defer rc.Close()
	return io.ReadAll(rc)
}
