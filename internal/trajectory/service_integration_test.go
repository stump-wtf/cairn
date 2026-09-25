package trajectory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/db"
	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/event"
	"github.com/stump-wtf/cairn/internal/objectstore"
	"github.com/stump-wtf/cairn/internal/store"
)

var schemaSeq atomic.Int64

// fixedStart is the run's started_at; the fixture's offsets are relative to it,
// so wall time is deterministic (34.2s) regardless of when the test runs.
var fixedStart = time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)

// newHarness connects to CAIRN_TEST_DATABASE_URL (skipping otherwise), migrates
// into a private per-test schema, and returns a trajectory Service and a store
// Store sharing one pool and object store. The per-schema isolation lets this
// run in parallel with the other packages' integration tests against the one CI
// database without truncating their tables.
func newHarness(t *testing.T) (*Service, *store.Store, *pgxpool.Pool, *objectstore.Memory) {
	t.Helper()
	dsn := os.Getenv("CAIRN_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set CAIRN_TEST_DATABASE_URL to run trajectory integration tests")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("trajectory_test_%d_%d", time.Now().UnixNano(), schemaSeq.Add(1))

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

	obj := objectstore.NewMemory()
	svc := NewService(pool, obj, Options{})
	st := store.New(pool, obj, store.Options{})
	return svc, st, pool, obj
}

// bigOutput is a deterministic 20 KiB blob — above the 16 KiB inline threshold —
// so the span carrying it spills to a content-addressed blob.
func bigOutput(seed byte) []byte {
	b := bytes.Repeat([]byte{seed}, 20*1024)
	copy(b, []byte("BEGIN npm ls --all\n"))
	return b
}

// future is a comfortably-future expiry so a run resolves under the link-cap
// read policy (expires_at > now()) regardless of when the suite runs.
func future() time.Time { return time.Now().Add(30 * 24 * time.Hour) }

func prov() artifact.Provenance {
	return artifact.Provenance{ActorID: "joe", Channel: artifact.ChannelMCP, CapturedAt: fixedStart}
}
func access() artifact.AccessPolicy {
	return artifact.AccessPolicy{OwnerID: "joe", Visibility: artifact.VisibilityLink}
}

// checkoutWebAudit is the design's canonical checkout-web-audit run: a human
// prompt, ten top-level spans, a three-span sub-agent excursion under s6, and a
// write span (s9) that produces a markdown artifact. Offsets/durations are the
// waterfall dataset's percentages scaled to the 34.2s wall (1% = 342ms). s2's
// bash output is oversized so it spills. producedID, when non-empty, is linked
// from the write span.
func checkoutWebAudit(producedID string) RunInput {
	arg := func(k, v string) json.RawMessage {
		return json.RawMessage(fmt.Sprintf(`{%q:%q}`, k, v))
	}
	return RunInput{
		Title:      "checkout-web-audit",
		Prompt:     "Audit the checkout web flow for vulnerable dependencies.",
		Model:      "claude-sonnet-4.6",
		TokenCount: 48100,
		StartedAt:  fixedStart,
		Provenance: prov(),
		Access:     access(),
		ExpiresAt:  future(),
		Spans: []SpanInput{
			{SpanID: "s1", Category: CategoryReason, Name: "planned the audit", StartOffsetMS: 0, DurationMS: 2223},
			{SpanID: "s2", Category: CategoryExec, Tool: "bash", Name: "npm ls --all", Args: arg("command", "npm ls --all"), Output: bigOutput('x'), StartOffsetMS: 2223, DurationMS: 2907},
			{SpanID: "s3", Category: CategoryRead, Tool: "read", Name: "package.json", Args: arg("path", "package.json"), Output: []byte("{ name: checkout-web }"), StartOffsetMS: 5130, DurationMS: 1368},
			{SpanID: "s4", Category: CategoryReason, Name: "assessed dependencies", StartOffsetMS: 6498, DurationMS: 2736},
			{SpanID: "s5", Category: CategoryExec, Tool: "grep", Name: "grep resolved", Args: arg("pattern", "resolved"), Output: []byte("5 hits"), StartOffsetMS: 9234, DurationMS: 1368},
			{SpanID: "s6", Category: CategoryNet, Tool: "sub-agent", Name: "advisory lookup", StartOffsetMS: 10602, DurationMS: 10602},
			{SpanID: "s6a", ParentSpanID: "s6", Category: CategoryNet, Tool: "web_search", Name: "CVE search", Args: arg("query", "checkout lib CVE"), StartOffsetMS: 10944, DurationMS: 3762},
			{SpanID: "s6b", ParentSpanID: "s6", Category: CategoryNet, Tool: "web_fetch", Name: "nvd.nist.gov", Args: arg("url", "nvd.nist.gov"), StartOffsetMS: 14877, DurationMS: 4104},
			{SpanID: "s6c", ParentSpanID: "s6", Category: CategoryReason, Name: "summarized advisory", StartOffsetMS: 18981, DurationMS: 2223},
			{SpanID: "s7", Category: CategoryRead, Tool: "read", Name: "confirm resolved 0.9.4", Args: arg("path", "package-lock.json"), StartOffsetMS: 21375, DurationMS: 1197},
			{SpanID: "s8", Category: CategoryReason, Name: "composed findings", StartOffsetMS: 22572, DurationMS: 6156},
			{SpanID: "s9", Category: CategoryWrite, Tool: "write", Name: "checkout-web-audit.md", Args: arg("path", "checkout-web-audit.md"), ProducedArtifactID: producedID, StartOffsetMS: 28728, DurationMS: 2736},
			{SpanID: "s10", Category: CategoryReason, Name: "final summary", StartOffsetMS: 31464, DurationMS: 2736},
		},
	}
}

// wantStats is the derived stats the fixture must reproduce, computed by hand
// from the span rows (SPEC-0004 "Derived Run Statistics").
var wantTimeByCategory = map[Category]int64{
	CategoryReason: 16074, // s1+s4+s6c+s8+s10
	CategoryExec:   4275,  // s2+s5
	CategoryRead:   2565,  // s3+s7
	CategoryNet:    18468, // s6+s6a+s6b
	CategoryWrite:  2736,  // s9
}

// createMarkdown mints an ordinary markdown artifact to be the produced target
// of the write span, returning its public id.
func createMarkdown(t *testing.T, st *store.Store) string {
	t.Helper()
	art, err := st.CreateArtifact(context.Background(), store.CreateArtifactInput{
		ShareType:         "markdown",
		Title:             "checkout-web-audit.md",
		Body:              bytes.NewReader([]byte("# Checkout Web Audit\n\nCVSS 9.1 · critical.")),
		DeclaredMediaType: "text/markdown",
		Provenance:        prov(),
		Access:            access(),
		ExpiresAt:         future(),
	})
	if err != nil {
		t.Fatalf("create markdown: %v", err)
	}
	return art.PublicID
}

func assertFixtureRun(t *testing.T, run *Run) {
	t.Helper()
	if run.Status != StatusClosed {
		t.Errorf("status = %q, want closed", run.Status)
	}
	if len(run.Spans) != 10 {
		t.Fatalf("root span count = %d, want 10", len(run.Spans))
	}
	// The sub-agent excursion nests three spans under s6, in seq order.
	var s6 *Span
	for _, r := range run.Spans {
		if r.SpanID == "s6" {
			s6 = r
		}
	}
	if s6 == nil {
		t.Fatal("s6 sub-agent span missing from roots")
	}
	if len(s6.Children) != 3 || s6.Children[0].SpanID != "s6a" || s6.Children[2].SpanID != "s6c" {
		t.Fatalf("s6 children = %v, want [s6a s6b s6c]", spanIDs(s6.Children))
	}
	if s6.Children[0].Depth != 1 {
		t.Errorf("child depth = %d, want 1", s6.Children[0].Depth)
	}
	// Derived stats match the hand-computed fixture totals.
	st := run.Stats
	if st.SpanCount != 13 {
		t.Errorf("span count = %d, want 13", st.SpanCount)
	}
	if st.ToolCallCount != 8 {
		t.Errorf("tool-call count = %d, want 8", st.ToolCallCount)
	}
	if st.WallTimeMS != 34200 {
		t.Errorf("wall = %dms, want 34200", st.WallTimeMS)
	}
	if st.TokenCount != 48100 {
		t.Errorf("tokens = %d, want 48100", st.TokenCount)
	}
	for cat, want := range wantTimeByCategory {
		if st.TimeByCategoryMS[cat] != want {
			t.Errorf("time-by-category[%s] = %d, want %d", cat, st.TimeByCategoryMS[cat], want)
		}
	}
}

func spanIDs(spans []*Span) []string {
	out := make([]string, len(spans))
	for i, s := range spans {
		out[i] = s.SpanID
	}
	return out
}

// TestBatchRoundTrip ingests the whole fixture in one batch and reconstructs the
// ordered tree with correct derived stats, spilled+deduped output, and a
// resolvable produced edge — the story's headline acceptance.
func TestBatchRoundTrip(t *testing.T) {
	svc, st, pool, obj := newHarness(t)
	ctx := context.Background()
	producedID := createMarkdown(t, st)

	run, err := svc.CreateBatchRun(ctx, checkoutWebAudit(producedID))
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	assertFixtureRun(t, run)

	// s2's oversized bash output spilled to a content-addressed blob (not inline).
	var s2 *Span
	for _, r := range run.Spans {
		if r.SpanID == "s2" {
			s2 = r
		}
	}
	if s2.Ref == nil {
		t.Fatal("s2 large output must carry an output_ref, not inline bytes")
	}
	if s2.Inline != "" {
		t.Error("s2 must not inline an oversized output")
	}
	if s2.Ref.Size != int64(len(bigOutput('x'))) {
		t.Errorf("s2 ref size = %d, want %d", s2.Ref.Size, len(bigOutput('x')))
	}
	// A small output stays inline.
	for _, r := range run.Spans {
		if r.SpanID == "s5" && r.Inline != "5 hits" {
			t.Errorf("s5 inline = %q, want '5 hits'", r.Inline)
		}
	}

	// Lazy fetch of the spilled output returns the exact bytes.
	rc, info, err := svc.OpenSpanOutput(ctx, run.PublicID, "s2")
	if err != nil {
		t.Fatalf("open span output: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, bigOutput('x')) {
		t.Error("lazily fetched span output differs from ingested bytes")
	}
	if info.SHA256 != s2.Ref.SHA256 {
		t.Errorf("fetched sha %s != span ref %s", info.SHA256, s2.Ref.SHA256)
	}

	// Produced edge resolves from the run direction...
	var s9 *Span
	for _, r := range run.Spans {
		if r.SpanID == "s9" {
			s9 = r
		}
	}
	if len(s9.ProducedArtifactIDs) != 1 || s9.ProducedArtifactIDs[0] != producedID {
		t.Fatalf("s9 produced = %v, want [%s]", s9.ProducedArtifactIDs, producedID)
	}
	// ...and from the artifact direction.
	var edgeCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM produced_edges pe JOIN artifacts a ON a.id = pe.artifact_id WHERE a.public_id = $1`,
		producedID).Scan(&edgeCount); err != nil {
		t.Fatalf("reverse edge: %v", err)
	}
	if edgeCount != 1 {
		t.Errorf("reverse produced-edge count = %d, want 1", edgeCount)
	}

	// Exactly one blob for the body (markdown) + one for the spilled span output.
	if got := obj.KeysWithPrefix("staging/"); len(got) != 0 {
		t.Errorf("staging objects leaked: %v", got)
	}
	var blobCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM blobs`).Scan(&blobCount); err != nil {
		t.Fatalf("blob count: %v", err)
	}
	if blobCount != 2 {
		t.Errorf("blob rows = %d, want 2 (markdown body + spilled s2 output)", blobCount)
	}
}

// TestBatchRunResolvesProducedArtifactHandle covers issue #50: a write span's
// produced_artifact_id may be supplied as an mcp://cairn/<id> handle rather
// than a bare public id, and must resolve to the same produced edge. The bare-id
// path is covered by TestBatchRoundTrip; this pins the handle form.
func TestBatchRunResolvesProducedArtifactHandle(t *testing.T) {
	svc, st, _, _ := newHarness(t)
	ctx := context.Background()
	producedID := createMarkdown(t, st)

	in := checkoutWebAudit("mcp://cairn/" + producedID)
	run, err := svc.CreateBatchRun(ctx, in)
	if err != nil {
		t.Fatalf("batch with handle-form produced id: %v", err)
	}
	var s9 *Span
	for _, r := range run.Spans {
		if r.SpanID == "s9" {
			s9 = r
		}
	}
	if s9 == nil {
		t.Fatal("s9 not found in run")
	}
	if len(s9.ProducedArtifactIDs) != 1 || s9.ProducedArtifactIDs[0] != producedID {
		t.Fatalf("s9 produced = %v, want [%s] (handle normalized to bare id)", s9.ProducedArtifactIDs, producedID)
	}
}

// TestAppendSpansNormalizesProducedHandle pins the append path's *returned*
// span, which is byte-for-byte what the live SSE stream publishes: a handle-form
// produced_artifact_id must come back as the bare id, or a viewer watching the
// run live renders the produced-artifact cross-link as `/mcp://cairn/<id>` and
// only self-corrects on reload, when the card is re-rendered from the database.
func TestAppendSpansNormalizesProducedHandle(t *testing.T) {
	svc, st, _, _ := newHarness(t)
	ctx := context.Background()
	producedID := createMarkdown(t, st)

	in := checkoutWebAudit("")
	in.Spans = nil
	run, err := svc.OpenRun(ctx, in)
	if err != nil {
		t.Fatalf("open run: %v", err)
	}
	appended, err := svc.AppendSpans(ctx, run.PublicID, in.Access.OwnerID, []SpanInput{{
		SpanID: "w1", Category: CategoryWrite, Tool: "write", Name: "wrote the report",
		ProducedArtifactID: "mcp://cairn/" + producedID,
		StartOffsetMS:      0, DurationMS: 10,
	}})
	if err != nil {
		t.Fatalf("append with handle-form produced id: %v", err)
	}
	if len(appended) != 1 {
		t.Fatalf("appended %d spans, want 1", len(appended))
	}
	got := appended[0].ProducedArtifactIDs
	if len(got) != 1 || got[0] != producedID {
		t.Fatalf("appended span produced = %v, want [%s] (bare id, not the handle)", got, producedID)
	}
}

// TestBatchAndIncrementalConverge proves a batch ingest and the equivalent
// open→append→close sequence yield identical span trees and identical derived
// stats (SPEC-0004 "Batch and incremental converge").
func TestBatchAndIncrementalConverge(t *testing.T) {
	svc, st, _, _ := newHarness(t)
	ctx := context.Background()

	batch, err := svc.CreateBatchRun(ctx, checkoutWebAudit(createMarkdown(t, st)))
	if err != nil {
		t.Fatalf("batch: %v", err)
	}

	// Incremental: open empty, append the same spans in tree order, close.
	fix := checkoutWebAudit(createMarkdown(t, st))
	spans := fix.Spans
	fix.Spans = nil
	open, err := svc.OpenRun(ctx, fix)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if open.Status != StatusOpen {
		t.Fatalf("opened run status = %q, want open", open.Status)
	}
	for _, sp := range spans {
		if _, err := svc.AppendSpans(ctx, open.PublicID, "joe", []SpanInput{sp}); err != nil {
			t.Fatalf("append %s: %v", sp.SpanID, err)
		}
	}
	closed, err := svc.CloseRun(ctx, open.PublicID, event.Actor{ID: "joe", Channel: artifact.ChannelMCP, Kind: event.KindAgent, Auth: event.AuthOAuth})
	if err != nil {
		t.Fatalf("close: %v", err)
	}

	if batch.Stats.SpanCount != closed.Stats.SpanCount ||
		batch.Stats.ToolCallCount != closed.Stats.ToolCallCount ||
		batch.Stats.WallTimeMS != closed.Stats.WallTimeMS ||
		batch.Stats.TokenCount != closed.Stats.TokenCount {
		t.Errorf("stats diverge: batch %+v vs incremental %+v", batch.Stats, closed.Stats)
	}
	for cat := range wantTimeByCategory {
		if batch.Stats.TimeByCategoryMS[cat] != closed.Stats.TimeByCategoryMS[cat] {
			t.Errorf("time-by-category[%s] diverges: %d vs %d", cat,
				batch.Stats.TimeByCategoryMS[cat], closed.Stats.TimeByCategoryMS[cat])
		}
	}
	assertFixtureRun(t, closed)

	// The trees are structurally identical (same ids, depths, seqs).
	if !treesEqual(batch.Spans, closed.Spans) {
		t.Error("batch and incremental span trees differ")
	}
}

func treesEqual(a, b []*Span) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].SpanID != b[i].SpanID || a[i].Depth != b[i].Depth ||
			a[i].Seq != b[i].Seq || a[i].Category != b[i].Category ||
			a[i].StartOffsetMS != b[i].StartOffsetMS || a[i].DurationMS != b[i].DurationMS {
			return false
		}
		if !treesEqual(a[i].Children, b[i].Children) {
			return false
		}
	}
	return true
}

// TestIdenticalOutputsDedup proves two spans with byte-identical oversized
// outputs share a single stored blob (SPEC-0004 "Identical outputs dedup").
func TestIdenticalOutputsDedup(t *testing.T) {
	svc, _, pool, _ := newHarness(t)
	ctx := context.Background()

	in := RunInput{
		Title: "dedup", Prompt: "p", StartedAt: fixedStart,
		Provenance: prov(), Access: access(), ExpiresAt: future(),
		Spans: []SpanInput{
			{SpanID: "a", Category: CategoryExec, Tool: "bash", Output: bigOutput('z'), StartOffsetMS: 0, DurationMS: 100},
			{SpanID: "b", Category: CategoryExec, Tool: "bash", Output: bigOutput('z'), StartOffsetMS: 100, DurationMS: 100},
		},
	}
	run, err := svc.CreateBatchRun(ctx, in)
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	var refs []string
	for _, s := range run.Spans {
		if s.Ref == nil {
			t.Fatalf("span %s should have spilled", s.SpanID)
		}
		refs = append(refs, s.Ref.SHA256)
	}
	if refs[0] != refs[1] {
		t.Errorf("identical outputs got distinct refs %s vs %s", refs[0], refs[1])
	}
	var blobCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM blobs`).Scan(&blobCount); err != nil {
		t.Fatalf("blob count: %v", err)
	}
	if blobCount != 1 {
		t.Errorf("dedup blob rows = %d, want 1", blobCount)
	}
}

// TestAppendAfterCloseRefused proves the distinct append-after-close sentinel
// and that no span is added (SPEC-0004 "Append to a closed run refused").
func TestAppendAfterCloseRefused(t *testing.T) {
	svc, st, _, _ := newHarness(t)
	ctx := context.Background()
	run, err := svc.CreateBatchRun(ctx, checkoutWebAudit(createMarkdown(t, st)))
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	_, err = svc.AppendSpans(ctx, run.PublicID, "joe",
		[]SpanInput{{SpanID: "extra", Category: CategoryReason, StartOffsetMS: 40000, DurationMS: 100}})
	if !errors.Is(err, ErrRunClosed) {
		t.Fatalf("append after close err = %v, want ErrRunClosed", err)
	}
	if errs.CodeOf(err) != errs.CodeConflict {
		t.Errorf("code = %v, want conflict", errs.CodeOf(err))
	}
	// No span was added.
	after, _ := svc.GetRun(ctx, run.PublicID)
	if after.Stats.SpanCount != 13 {
		t.Errorf("span count after refused append = %d, want 13", after.Stats.SpanCount)
	}
}

// TestNonOwnerCannotClose proves only the owning principal may close a run
// (SPEC-0004 "Non-owner cannot close a run").
func TestNonOwnerCannotClose(t *testing.T) {
	svc, _, _, _ := newHarness(t)
	ctx := context.Background()
	fix := checkoutWebAudit("")
	fix.Spans = nil
	open, err := svc.OpenRun(ctx, fix)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_, err = svc.CloseRun(ctx, open.PublicID, event.Actor{ID: "mallory", Channel: artifact.ChannelMCP, Kind: event.KindAgent, Auth: event.AuthOAuth})
	if !errors.Is(err, ErrNotOwner) {
		t.Fatalf("close by non-owner err = %v, want ErrNotOwner", err)
	}
	if errs.CodeOf(err) != errs.CodeForbidden {
		t.Errorf("code = %v, want forbidden", errs.CodeOf(err))
	}
	got, _ := svc.GetRun(ctx, open.PublicID)
	if got.Status != StatusOpen {
		t.Errorf("run status = %q after refused close, want open", got.Status)
	}
}

// TestMalformedTreeAtomic proves an append naming an unknown parent persists
// none of its spans (SPEC-0004 "Malformed tree rejected atomically").
func TestMalformedTreeAtomic(t *testing.T) {
	svc, _, _, _ := newHarness(t)
	ctx := context.Background()
	fix := checkoutWebAudit("")
	fix.Spans = nil
	open, err := svc.OpenRun(ctx, fix)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_, err = svc.AppendSpans(ctx, open.PublicID, "joe", []SpanInput{
		{SpanID: "ok", Category: CategoryReason, StartOffsetMS: 0, DurationMS: 100},
		{SpanID: "bad", ParentSpanID: "ghost", Category: CategoryReason, StartOffsetMS: 100, DurationMS: 100},
	})
	if !errors.Is(err, ErrUnknownParent) {
		t.Fatalf("err = %v, want ErrUnknownParent", err)
	}
	got, _ := svc.GetRun(ctx, open.PublicID)
	if got.Stats.SpanCount != 0 {
		t.Errorf("span count = %d after atomic-rejected append, want 0", got.Stats.SpanCount)
	}
}

// TestUnknownRunNotFound proves a uniform not-found for an unknown id.
func TestUnknownRunNotFound(t *testing.T) {
	svc, _, _, _ := newHarness(t)
	_, err := svc.GetRun(context.Background(), "ZZ99zz99")
	if !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("err = %v, want ErrRunNotFound", err)
	}
}

// TestConcurrentAppendsKeepSeqMonotonic proves racing appends to one open run
// each receive a distinct, gap-free top-level seq (SPEC-0004 "Concurrency
// Safety"). Run under -race in CI.
func TestConcurrentAppendsKeepSeqMonotonic(t *testing.T) {
	svc, _, pool, _ := newHarness(t)
	ctx := context.Background()
	fix := checkoutWebAudit("")
	fix.Spans = nil
	open, err := svc.OpenRun(ctx, fix)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	const n = 12
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			_, err := svc.AppendSpans(ctx, open.PublicID, "joe", []SpanInput{
				{SpanID: fmt.Sprintf("c%d", i), Category: CategoryReason, StartOffsetMS: i * 10, DurationMS: 5},
			})
			errCh <- err
		}(i)
	}
	for i := 0; i < n; i++ {
		if err := <-errCh; err != nil {
			t.Fatalf("concurrent append: %v", err)
		}
	}

	// Seqs are exactly 0..n-1 with no gaps or collisions among top-level spans.
	rows, err := pool.Query(ctx,
		`SELECT seq FROM spans WHERE parent_span_id IS NULL ORDER BY seq`)
	if err != nil {
		t.Fatalf("query seqs: %v", err)
	}
	defer rows.Close()
	var seqs []int
	for rows.Next() {
		var s int
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		seqs = append(seqs, s)
	}
	if len(seqs) != n {
		t.Fatalf("got %d top-level spans, want %d", len(seqs), n)
	}
	for i, s := range seqs {
		if s != i {
			t.Fatalf("seq[%d] = %d, want %d (gap or collision)", i, s, i)
		}
	}
}
