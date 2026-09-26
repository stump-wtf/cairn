package trajectory

// Trace Redaction Tests
//
// A trace is masked before any of it is stored (SPEC-0017 RD-1, RD-4): the run
// title and prompt, and each span's name, args and output, inline or spilled,
// reach the rows, the object store and live viewers only as [REDACTED]. A live
// append with a token succeeds and reports its redaction; args stay valid JSON
// with the header name and scheme kept (RD-3); the outcome commits with the
// content and accumulates across appends (RD-9); and a scan that fails refuses
// the write. Credentials are assembled at run time from split literals.
//
// Governing: ADR-0023, SPEC-0017 RD-1, RD-3, RD-4, RD-7, RD-9, RD-10,
// REQ "Error Handling Standards"
//
// @joestump 09/25/2026 - Added for cairn#290 (unwired paths record unscanned).
// @joestump 09/26/2026 - cairn#291 wires the scanner in; the tests below it.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/metrics"
	"github.com/stump-wtf/cairn/internal/objectstore"
	"github.com/stump-wtf/cairn/internal/redact"
)

// plantedToken is a GitHub personal access token shape gitleaks detects.
func plantedToken(seed int) string {
	const alnum = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	var b strings.Builder
	for i := 0; i < 36; i++ {
		b.WriteByte(alnum[(seed*11+i*7)%len(alnum)])
	}
	return "gh" + "p_" + b.String()
}

// bearerValue is an opaque bearer credential: only the Authorization label
// rule can find it.
func bearerValue() string { return "zq8" + "Lk2Vx7" + "Pn4Rt9Wm" }

// testScanner is a real scanner with the production defaults.
func testScanner(t *testing.T) *redact.Scanner {
	t.Helper()
	sc, err := redact.New(redact.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

// recordingStore is an in-memory object store that keeps a copy of every byte
// ever Put, staging included, so a test can prove a value never reached object
// storage even transiently.
type recordingStore struct {
	*objectstore.Memory
	mu   sync.Mutex
	puts []string
}

func (r *recordingStore) Put(ctx context.Context, key string, body io.Reader, size int64, ct string) error {
	b, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.puts = append(r.puts, string(b))
	r.mu.Unlock()
	return r.Memory.Put(ctx, key, bytes.NewReader(b), size, ct)
}

func (r *recordingStore) everything() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.puts, "\n")
}

// tablesJSON renders every row of the tables a run writes, so a test can grep
// all of their columns at once.
func tablesJSON(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var out strings.Builder
	for _, table := range []string{"artifacts", "runs", "spans"} {
		var rows string
		if err := pool.QueryRow(context.Background(),
			`SELECT COALESCE(string_agg(row_to_json(t)::text, E'\n'), '') FROM `+table+` t`).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		out.WriteString(rows)
		out.WriteByte('\n')
	}
	return out.String()
}

func assertAbsent(t *testing.T, where, text string, secrets []string) {
	t.Helper()
	for _, s := range secrets {
		if strings.Contains(text, s) {
			t.Errorf("%s holds a planted credential", where)
		}
	}
}

// storedOutcome reads a run's recorded outcome from the run row and from its
// artifact envelope.
func storedOutcome(t *testing.T, pool *pgxpool.Pool, publicID string) (run, art redact.Summary) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), `
		SELECT r.redaction_status, r.redaction_count, r.redaction_rules,
		       a.redaction_status, a.redaction_count, a.redaction_rules
		FROM runs r JOIN artifacts a ON a.id = r.artifact_id
		WHERE a.public_id = $1`, publicID).Scan(
		&run.Status, &run.Count, &run.Rules, &art.Status, &art.Count, &art.Rules); err != nil {
		t.Fatal(err)
	}
	return run, art
}

func redactionCount(t *testing.T, r *metrics.Registry, surface metrics.RedactionSurface, outcome metrics.RedactionOutcome) float64 {
	t.Helper()
	mfs, err := r.Gatherer().Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "cairn_redactions_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["surface"] == string(surface) && labels["outcome"] == string(outcome) {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func openInput(title string) RunInput {
	return RunInput{Title: title, StartedAt: fixedStart, Provenance: prov(), Access: access(), ExpiresAt: future()}
}

// TestUnwiredRunRecordsUnscanned: a Service built without a scanner records
// the outcome "unscanned", never "clean".
func TestUnwiredRunRecordsUnscanned(t *testing.T) {
	_, st, pool, obj := newHarness(t)
	svc := NewService(pool, obj, Options{})
	ctx := context.Background()

	run, err := svc.CreateBatchRun(ctx, checkoutWebAudit(createMarkdown(t, st)))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := storedOutcome(t, pool, run.PublicID)
	if got.Status != redact.StatusUnscanned || got.Count != 0 || got.Rules == nil || len(got.Rules) != 0 {
		t.Errorf("run outcome = %+v; want unscanned, 0, {}", got)
	}
}

// TestBatchRunMasksEveryField: every scanned field of a batch run is masked
// before it is stored. The args keep the Authorization header's name and
// scheme and stay valid JSON (RD-3), the spilled output's stored SHA-256 is of
// the masked bytes (RD-10), and no row, no object ever Put (staging included)
// and no read holds a token.
func TestBatchRunMasksEveryField(t *testing.T) {
	_, _, pool, _ := newHarness(t)
	rec := &recordingStore{Memory: objectstore.NewMemory()}
	reg := metrics.New()
	svc := NewService(pool, rec, Options{Redaction: testScanner(t), Metrics: reg})
	ctx := context.Background()

	title, prompt, name, inline, spilled, bearer := plantedToken(1), plantedToken(2), plantedToken(3), plantedToken(4), plantedToken(5), bearerValue()
	secrets := []string{title, prompt, name, inline, spilled, bearer}
	big := bigOutput('x')
	copy(big[1000:], "export GH_TOKEN="+spilled+"\n")

	in := openInput("deploy with " + title)
	in.Prompt = "use " + prompt + " to push"
	in.Spans = []SpanInput{
		{
			SpanID: "s1", Category: CategoryExec, Tool: "http", Name: "GET as " + name,
			Args:   json.RawMessage(`{"url": "https://api.example.test/v1/me", "headers": {"Authorization": "Bearer ` + bearer + `", "Accept": "application/json"}}`),
			Output: []byte("token is " + inline), StartOffsetMS: 0, DurationMS: 10,
		},
		{SpanID: "s2", Category: CategoryExec, Tool: "bash", Name: "env", Output: big, StartOffsetMS: 10, DurationMS: 10},
	}

	run, err := svc.CreateBatchRun(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	if run.Title != "deploy with [REDACTED]" || run.Prompt != "use [REDACTED] to push" {
		t.Errorf("title, prompt = %q, %q; want each token masked", run.Title, run.Prompt)
	}
	s1, s2 := run.Spans[0], run.Spans[1]
	if s1.Name != "GET as [REDACTED]" || s1.Inline != "token is [REDACTED]" {
		t.Errorf("span name, inline output = %q, %q; want each token masked", s1.Name, s1.Inline)
	}
	var args struct {
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
	}
	if err := json.Unmarshal(s1.Args, &args); err != nil {
		t.Fatalf("stored args do not parse: %v\n%s", err, s1.Args)
	}
	if args.Headers["Authorization"] != "Bearer [REDACTED]" || args.Headers["Accept"] != "application/json" || args.URL != "https://api.example.test/v1/me" {
		t.Errorf("stored args = %+v; want the Authorization value masked and the rest kept", args)
	}

	// The spilled output reads back masked, and its checksum is of what was
	// stored.
	if s2.Ref == nil {
		t.Fatal("s2 output did not spill")
	}
	rc, info, err := svc.OpenSpanOutput(ctx, run.PublicID, "s2")
	if err != nil {
		t.Fatal(err)
	}
	stored, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(stored, []byte("export GH_TOKEN=[REDACTED]\n")) {
		t.Error("spilled output is not masked")
	}
	sum := sha256.Sum256(stored)
	if hex.EncodeToString(sum[:]) != info.SHA256 || s2.Ref.SHA256 != info.SHA256 {
		t.Errorf("stored SHA-256 %s is not of the stored bytes", info.SHA256)
	}

	wantRules := map[string]int{"github-pat": 5, "cairn-authorization": 1}
	runOutcome, artOutcome := storedOutcome(t, pool, run.PublicID)
	for where, got := range map[string]redact.Summary{"run row": runOutcome, "artifact row": artOutcome, "GetRun": run.Redaction} {
		if got.Status != redact.StatusMasked || got.Count != 6 || len(got.Rules) != 2 ||
			got.Rules["github-pat"] != wantRules["github-pat"] || got.Rules["cairn-authorization"] != 1 {
			t.Errorf("%s outcome = %+v; want masked, 6, %v", where, got, wantRules)
		}
	}

	assertAbsent(t, "a stored row", tablesJSON(t, pool), secrets)
	assertAbsent(t, "an object put to the store", rec.everything(), secrets)
	got, err := svc.GetRun(ctx, run.PublicID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(got)
	assertAbsent(t, "the read run", string(encoded), secrets)

	// One scan per non-empty field: title, prompt, two names, one args, two
	// outputs. Six masked, one clean (s2's name).
	if m, c := redactionCount(t, reg, metrics.SurfaceRun, metrics.OutcomeMasked), redactionCount(t, reg, metrics.SurfaceRun, metrics.OutcomeClean); m != 6 || c != 1 {
		t.Errorf("cairn_redactions_total{run} masked %v, clean %v; want 6, 1", m, c)
	}
}

// TestLiveAppendMaskedNotRefused: SPEC-0017 RD-4 "Live trace append is masked,
// not refused". The append succeeds, its stored output is masked, it reports
// one redaction, the live stream carries only the masked span, and the run
// accumulates the outcome across appends.
func TestLiveAppendMaskedNotRefused(t *testing.T) {
	svc, _, pool, _ := newHarness(t)
	ctx := context.Background()
	tok := plantedToken(6)

	run, err := svc.OpenRun(ctx, openInput("live"))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := storedOutcome(t, pool, run.PublicID); got.Status != redact.StatusClean || got.Count != 0 {
		t.Fatalf("opened run outcome = %+v; want clean, 0", got)
	}
	sub := svc.Subscribe(run.PublicID)
	defer sub.Close()

	appended, outcome, err := svc.AppendSpansWithOutcome(ctx, run.PublicID, "joe", []SpanInput{
		{SpanID: "a1", Category: CategoryExec, Tool: "bash", Name: "cat .env", Args: json.RawMessage(`{"command": "cat .env"}`),
			Output: []byte("GITHUB_TOKEN=" + tok + "\nDEBUG=1\n"), StartOffsetMS: 0, DurationMS: 5},
	})
	if err != nil {
		t.Fatalf("append with a token was refused: %v", err)
	}
	if outcome.Status != redact.StatusMasked || outcome.Count != 1 || outcome.Rules["github-pat"] != 1 {
		t.Errorf("append outcome = %+v; want masked, 1, github-pat", outcome)
	}
	if len(appended) != 1 || appended[0].Inline != "GITHUB_TOKEN=[REDACTED]\nDEBUG=1\n" {
		t.Errorf("appended span = %+v; want the output masked", appended)
	}
	select {
	case ev := <-sub.Events():
		if ev.Span == nil || strings.Contains(ev.Span.Inline, tok) || !strings.Contains(ev.Span.Inline, redact.Mask) {
			t.Errorf("live event = %+v; want the masked span", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no live event for the append")
	}

	// A clean append reports no redaction of its own; the run keeps the
	// masked total.
	_, outcome, err = svc.AppendSpansWithOutcome(ctx, run.PublicID, "joe", []SpanInput{
		{SpanID: "a2", Category: CategoryReason, Name: "thought", StartOffsetMS: 5, DurationMS: 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Status != redact.StatusClean || outcome.Count != 0 {
		t.Errorf("clean append outcome = %+v; want clean, 0", outcome)
	}
	runOutcome, artOutcome := storedOutcome(t, pool, run.PublicID)
	for where, got := range map[string]redact.Summary{"run row": runOutcome, "artifact row": artOutcome} {
		if got.Status != redact.StatusMasked || got.Count != 1 || got.Rules["github-pat"] != 1 {
			t.Errorf("%s outcome = %+v; want masked, 1, github-pat", where, got)
		}
	}
	assertAbsent(t, "a stored row", tablesJSON(t, pool), []string{tok})
}

// TestRunScanFailureFailsClosed: SPEC-0017 "Detector error fails closed". A
// scan that cannot finish refuses the write as an internal failure, and
// nothing of it is stored: no run, no span, no object, no outcome change.
func TestRunScanFailureFailsClosed(t *testing.T) {
	_, _, pool, _ := newHarness(t)
	rec := &recordingStore{Memory: objectstore.NewMemory()}
	svc := NewService(pool, rec, Options{Redaction: testScanner(t)})
	tok := plantedToken(7)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	in := openInput("t")
	in.Spans = []SpanInput{{SpanID: "s1", Category: CategoryExec, Tool: "bash", Name: "x", Output: bigOutput('y'), StartOffsetMS: 0, DurationMS: 1}}
	in.Prompt = "key " + tok
	if _, err := svc.CreateBatchRun(cancelled, in); !errors.Is(err, redact.ErrScanFailed) || strings.Contains(err.Error(), tok) {
		t.Fatalf("create err = %v; want ErrScanFailed without the token", err)
	}
	var runs int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM runs`).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if runs != 0 || rec.Len() != 0 || rec.everything() != "" {
		t.Errorf("after a failed scan: %d runs, %d objects; want none", runs, rec.Len())
	}

	// A failed append leaves the open run exactly as it was.
	run, err := svc.OpenRun(context.Background(), openInput("open"))
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = svc.AppendSpansWithOutcome(cancelled, run.PublicID, "joe", []SpanInput{
		{SpanID: "a1", Category: CategoryExec, Tool: "bash", Name: "x", Output: []byte("key " + tok), StartOffsetMS: 0, DurationMS: 1},
	})
	if !errors.Is(err, redact.ErrScanFailed) {
		t.Fatalf("append err = %v; want ErrScanFailed", err)
	}
	var spans int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM spans`).Scan(&spans); err != nil {
		t.Fatal(err)
	}
	if got, _ := storedOutcome(t, pool, run.PublicID); spans != 0 || got.Status != redact.StatusClean {
		t.Errorf("after a failed append: %d spans, outcome %+v; want 0, clean", spans, got)
	}
	assertAbsent(t, "a stored row", tablesJSON(t, pool), []string{tok})
}

// TestOversizeSpanOutputRejected: SPEC-0017 RD-7 on a span. An output over the
// scan cap is refused as too_large_to_scan, naming the span field, and nothing
// is stored.
func TestOversizeSpanOutputRejected(t *testing.T) {
	_, _, pool, obj := newHarness(t)
	sc, err := redact.New(redact.Config{MaxScanBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(pool, obj, Options{Redaction: sc})
	in := openInput("big")
	in.Spans = []SpanInput{{SpanID: "s1", Category: CategoryExec, Tool: "bash", Name: "x", Output: bytes.Repeat([]byte("a"), 2048), StartOffsetMS: 0, DurationMS: 1}}

	_, err = svc.CreateBatchRun(context.Background(), in)
	var rej *redact.Rejection
	if !errors.As(err, &rej) || !errors.Is(err, redact.ErrTooLargeToScan) || !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("err = %v; want a too_large_to_scan rejection", err)
	}
	if rej.Field != "spans[0].output" || rej.Limit != 1024 {
		t.Errorf("rejection = %+v; want field spans[0].output, limit 1024", rej)
	}
	var runs int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM runs`).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if runs != 0 {
		t.Errorf("%d runs stored; want none", runs)
	}
}

// TestOversizeSpanStoredUnscannedWarns: SPEC-0017 RD-7 under
// CAIRN_REDACTION_OVERSIZE=store_unscanned. A span output over the scan cap is
// stored as written with status not_scanned_oversize, and every write that
// stores one, create and append alike, logs a WARN naming the run, the field
// and its size, and none of its content.
func TestOversizeSpanStoredUnscannedWarns(t *testing.T) {
	_, _, pool, obj := newHarness(t)
	sc, err := redact.New(redact.Config{MaxScanBytes: 1024, Oversize: redact.OversizeStoreUnscanned})
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	svc := NewService(pool, obj, Options{Redaction: sc, Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	ctx := context.Background()
	tok := plantedToken(8)
	output := append(bytes.Repeat([]byte("z"), 2048-len(tok)), tok...)

	in := openInput("big")
	in.Spans = []SpanInput{{SpanID: "s1", Category: CategoryExec, Tool: "bash", Name: "x", Output: output, StartOffsetMS: 0, DurationMS: 1}}
	run, err := svc.OpenRun(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := storedOutcome(t, pool, run.PublicID); got.Status != redact.StatusNotScannedOversize {
		t.Errorf("run outcome = %+v; want not_scanned_oversize", got)
	}
	created := logs.String()
	for _, want := range []string{"level=WARN", "artifact=" + run.PublicID, "field=spans[0].output", "size_bytes=2048", "max_scan_bytes=1024"} {
		if !strings.Contains(created, want) {
			t.Errorf("create log lacks %q:\n%s", want, created)
		}
	}

	logs.Reset()
	if _, _, err := svc.AppendSpansWithOutcome(ctx, run.PublicID, "joe", []SpanInput{
		{SpanID: "a1", Category: CategoryExec, Tool: "bash", Name: "y", Output: output, StartOffsetMS: 1, DurationMS: 1},
	}); err != nil {
		t.Fatal(err)
	}
	appended := logs.String()
	if !strings.Contains(appended, "level=WARN") || !strings.Contains(appended, "field=spans[0].output") {
		t.Errorf("append logged no WARN for its unscanned output:\n%s", appended)
	}

	// A clean, in-cap write logs nothing.
	logs.Reset()
	if _, _, err := svc.AppendSpansWithOutcome(ctx, run.PublicID, "joe", []SpanInput{
		{SpanID: "a2", Category: CategoryReason, Name: "thought", StartOffsetMS: 2, DurationMS: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if logs.Len() != 0 {
		t.Errorf("an in-cap append logged:\n%s", logs.String())
	}
	for where, text := range map[string]string{"create log": created, "append log": appended} {
		if strings.Contains(text, tok) || strings.Contains(text, "zzzz") {
			t.Errorf("%s carries the field's content", where)
		}
	}
}
