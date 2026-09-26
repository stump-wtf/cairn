package webhook

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stump-wtf/cairn/internal/metrics"
	"github.com/stump-wtf/cairn/internal/objectstore"
	"github.com/stump-wtf/cairn/internal/redact"
)

// Integration tests for the capture-time credential scan (SPEC-0017 RD-4):
// each plants runtime-built tokens in a capture and then greps everything the
// capture reaches, the hook_requests row, the object store, the live fan-out,
// the reads and the log, for any part of them.
//
// Governing: ADR-0023, SPEC-0017 RD-1, RD-4, RD-9; ADR-0010, SPEC-0005

// redactionHarness is newHarness's database with a service whose object
// store, log and metrics are in reach.
type redactionHarness struct {
	svc  *Service
	pool *pgxpool.Pool
	obj  *objectstore.Memory
	logs *bytes.Buffer
	reg  *metrics.Registry
}

func newRedactionHarness(t *testing.T) *redactionHarness {
	t.Helper()
	_, pool := newHarness(t)
	h := &redactionHarness{pool: pool, obj: objectstore.NewMemory(), logs: &bytes.Buffer{}, reg: metrics.New()}
	h.svc = NewService(pool, h.obj, Options{Scanner: testScanner(t), Metrics: h.reg, Logger: bufLogger(h.logs)})
	return h
}

// assertNowhere fails if any planted token appears in any stored row, stored
// object or log line.
func (h *redactionHarness) assertNowhere(t *testing.T, tokens ...string) {
	t.Helper()
	all := h.dump(t)
	for _, tok := range tokens {
		if strings.Contains(all, tok) {
			t.Fatalf("a planted token reached storage or the log")
		}
	}
}

// dump is every stored row, stored object and log line, as text.
func (h *redactionHarness) dump(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	var dump strings.Builder
	for _, q := range []string{
		`SELECT coalesce(string_agg(row_to_json(r)::text, E'\n'), '') FROM hook_requests r`,
		// body_inline is bytea, which row_to_json renders as hex.
		`SELECT coalesce(string_agg(convert_from(body_inline, 'UTF8'), E'\n'), '') FROM hook_requests WHERE body_inline IS NOT NULL`,
		`SELECT coalesce(string_agg(row_to_json(b)::text, E'\n'), '') FROM blobs b`,
	} {
		var s string
		if err := h.pool.QueryRow(ctx, q).Scan(&s); err != nil {
			t.Fatalf("dump: %v", err)
		}
		dump.WriteString(s)
	}
	for _, key := range h.obj.KeysWithPrefix("") {
		rc, err := h.obj.Get(ctx, key)
		if err != nil {
			t.Fatalf("get %s: %v", key, err)
		}
		b, _ := io.ReadAll(rc)
		_ = rc.Close()
		dump.Write(b)
	}
	dump.WriteString(h.logs.String())
	return dump.String()
}

// webhookCount reads cairn_redactions_total{surface="webhook",outcome}.
func (h *redactionHarness) webhookCount(t *testing.T, outcome metrics.RedactionOutcome) float64 {
	t.Helper()
	mfs, err := h.reg.Gatherer().Gather()
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
			if labels["surface"] == string(metrics.SurfaceWebhook) && labels["outcome"] == string(outcome) {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// TestIntegrationCaptureMasksAuthorizationHeader is SPEC-0017 RD-4 "Webhook
// capture is masked": a bearer Authorization header is stored with its name
// and scheme kept and its value masked, and a token in the query and in a body
// big enough to spill to a blob is masked too. The live fan-out, the reads and
// the stored bytes carry only the masked form, and the outcome is recorded.
func TestIntegrationCaptureMasksAuthorizationHeader(t *testing.T) {
	h := newRedactionHarness(t)
	ctx := context.Background()
	ep := newEndpoint(t, h.svc, EndpointInput{})
	sub := h.svc.Subscribe(ep.PublicID)
	defer sub.Close()

	ta, tq, tb := plantedToken(11), plantedToken(12), plantedToken(13)
	body := `{"deploy_key":"` + tb + `","pad":"` + strings.Repeat("x", 20*1024) + `"}`
	got, err := h.svc.Capture(ctx, ep.PublicID, CaptureInput{
		Method: "POST", Path: "/h/" + ep.PublicID, Query: "sig=" + tq,
		Headers:     map[string][]string{"Authorization": {"Bearer " + ta}, "Content-Type": {"application/json"}},
		ContentType: "application/json", Body: []byte(body), Status: DefaultResponseStatus,
	})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if want := []string{"Bearer " + redact.Mask}; !reflect.DeepEqual(got.Headers["authorization"], want) {
		t.Fatalf("authorization = %q, want %q", got.Headers["authorization"], want)
	}
	if got.Ref == nil {
		t.Fatal("the body did not spill; the test needs the blob path")
	}
	if got.Redaction.Status != redact.StatusMasked || got.Redaction.Count < 3 || len(got.Withheld) != 0 {
		t.Fatalf("outcome = %+v withheld %v, want masked with at least 3 values", got.Redaction, got.Withheld)
	}

	// Live fan-out (SSE and MCP share this hub) carries the masked form.
	ev := drainEvent(t, sub)
	if fmt.Sprint(ev.Request.Headers, ev.Request.Query) != fmt.Sprint(got.Headers, got.Query) || strings.Contains(ev.Request.Query, tq) {
		t.Fatalf("fanned-out request differs from the stored, masked one: %+v", ev.Request)
	}

	// Every read returns the masked form and the recorded outcome.
	read, err := h.svc.GetRequest(ctx, ep.PublicID, got.Seq)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(read.Headers, got.Headers) || read.Query != got.Query || !reflect.DeepEqual(read.Redaction, got.Redaction) {
		t.Fatalf("read = %+v, want the captured %+v", read, got)
	}
	after, err := h.svc.RequestsAfter(ctx, ep.PublicID, 0)
	if err != nil || len(after) != 1 || !reflect.DeepEqual(after[0].Redaction, got.Redaction) {
		t.Fatalf("RequestsAfter = %+v, %v", after, err)
	}
	rc, info, err := h.svc.OpenRequestBody(ctx, ep.PublicID, got.Seq)
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !strings.Contains(string(stored), `"deploy_key":"`+redact.Mask+`"`) || info.Size != int64(len(stored)) {
		t.Fatalf("stored body is not the masked one (size %d, %d bytes read)", info.Size, len(stored))
	}

	h.assertNowhere(t, ta, tq, tb)
	if n := h.webhookCount(t, metrics.OutcomeMasked); n != 3 {
		t.Errorf("cairn_redactions_total{webhook,masked} = %v, want 3 (query, headers, body)", n)
	}
}

// TestIntegrationCaptureScanFailureStoresNotice: a scan that fails outright
// never stores the raw bytes and never refuses the capture. A notice stands in
// for the body, the withheld fields are recorded and read back, and the live
// fan-out carries the notice.
func TestIntegrationCaptureScanFailureStoresNotice(t *testing.T) {
	h := newRedactionHarness(t)
	h.svc.scanner = failingScanner{}
	ctx := context.Background()
	ep := newEndpoint(t, h.svc, EndpointInput{})
	sub := h.svc.Subscribe(ep.PublicID)
	defer sub.Close()

	tok := plantedToken(14)
	got, err := h.svc.Capture(ctx, ep.PublicID, CaptureInput{
		Method: "POST", Query: "t=" + tok, Headers: map[string][]string{"X-Key": {tok}},
		ContentType: "text/plain", Body: []byte("key " + tok), Status: DefaultResponseStatus,
	})
	if err != nil {
		t.Fatalf("capture must not fail when the scan does: %v", err)
	}
	if !strings.HasPrefix(string(got.Inline), withheldBodyNotice) {
		t.Fatalf("stored body = %q, want the withheld notice", got.Inline)
	}
	if ev := drainEvent(t, sub); !bytes.Equal(ev.Request.Inline, got.Inline) {
		t.Fatalf("fanned-out body = %q, want the notice", ev.Request.Inline)
	}
	read, err := h.svc.GetRequest(ctx, ep.PublicID, got.Seq)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{FieldQuery, FieldHeaders, FieldBody}; !reflect.DeepEqual(read.Withheld, want) {
		t.Fatalf("withheld read back = %v, want %v", read.Withheld, want)
	}
	if read.Redaction.Status != redact.StatusUnscanned {
		t.Fatalf("status read back = %q, want unscanned", read.Redaction.Status)
	}
	h.assertNowhere(t, tok)
	if n := h.webhookCount(t, metrics.OutcomeFailed); n != 3 {
		t.Errorf("cairn_redactions_total{webhook,failed} = %v, want 3", n)
	}
}

// TestIntegrationCaptureBinaryBodyStoredUnchanged (RD-6): a real image is
// stored byte for byte and flagged not_scanned_binary.
func TestIntegrationCaptureBinaryBodyStoredUnchanged(t *testing.T) {
	h := newRedactionHarness(t)
	ctx := context.Background()
	ep := newEndpoint(t, h.svc, EndpointInput{})
	png := append([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"), bytes.Repeat([]byte{0, 1, 2, 3}, 64)...)
	got, err := h.svc.Capture(ctx, ep.PublicID, CaptureInput{Method: "PUT", ContentType: "image/png", Body: png, Status: 200})
	if err != nil {
		t.Fatal(err)
	}
	read, err := h.svc.GetRequest(ctx, ep.PublicID, got.Seq)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(read.Inline, png) || read.Redaction.Status != redact.StatusNotScannedBinary {
		t.Fatalf("binary capture: status %q, unchanged=%v", read.Redaction.Status, bytes.Equal(read.Inline, png))
	}
	if n := h.webhookCount(t, metrics.OutcomeNotScannedBinary); n != 1 {
		t.Errorf("cairn_redactions_total{webhook,not_scanned_binary} = %v, want 1", n)
	}
}

// TestIntegrationCaptureWithoutScannerReadsUnscanned: a Service built without
// a scanner records the honest "unscanned", and Authorization is still masked
// by header hygiene. It is also the positive control for assertNowhere: the
// unscanned tokens, inline and spilled, are found where they were stored.
func TestIntegrationCaptureWithoutScannerReadsUnscanned(t *testing.T) {
	h := newRedactionHarness(t)
	h.svc = NewService(h.pool, h.obj, Options{Logger: bufLogger(h.logs)})
	svc := h.svc
	ctx := context.Background()
	ep := newEndpoint(t, svc, EndpointInput{})
	inline, spilled := plantedToken(15), plantedToken(16)
	got, err := svc.Capture(ctx, ep.PublicID, CaptureInput{
		Method: "POST", Headers: map[string][]string{"Authorization": {"Bearer abc"}}, Body: []byte("hi " + inline), Status: 200,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Capture(ctx, ep.PublicID, CaptureInput{
		Method: "POST", Body: []byte(spilled + strings.Repeat(" ", 20*1024)), Status: 200,
	}); err != nil {
		t.Fatal(err)
	}
	all := h.dump(t)
	if !strings.Contains(all, inline) || !strings.Contains(all, spilled) {
		t.Fatal("positive control: dump cannot see a token stored unscanned, so assertNowhere proves nothing")
	}
	read, err := svc.GetRequest(ctx, ep.PublicID, got.Seq)
	if err != nil {
		t.Fatal(err)
	}
	if read.Redaction.Status != redact.StatusUnscanned || read.Redaction.Rules == nil {
		t.Fatalf("outcome = %+v, want unscanned with an empty rules map", read.Redaction)
	}
	if read.Headers["authorization"][0] != "Bearer "+redact.Mask {
		t.Fatalf("authorization = %q, want masked", read.Headers["authorization"])
	}
}
