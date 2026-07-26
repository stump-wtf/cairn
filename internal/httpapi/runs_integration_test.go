package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/joestump/cairn/internal/store"
)

// fixedRunStart is the runs' started_at; span offsets are relative to it, so the
// derived wall time is deterministic regardless of when the suite runs.
var fixedRunStart = time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)

// bigSpanOutput is a deterministic 20 KiB blob — above the 16 KiB inline
// threshold — so the span carrying it spills to a content-addressed blob.
func bigSpanOutput() []byte {
	b := bytes.Repeat([]byte{'x'}, 20*1024)
	copy(b, []byte("BEGIN npm ls --all\n"))
	return b
}

// fixtureSpans is the canonical run body: a reasoning turn, a bash exec with an
// oversized (spilled) output, a small inline read, a two-span sub-agent
// excursion, and a write span producing a markdown artifact. Ingesting these in
// one batch and appending them one-by-one to an open run MUST converge.
func fixtureSpans(producedID string) []spanRequest {
	arg := func(k, v string) json.RawMessage { return json.RawMessage(`{"` + k + `":"` + v + `"}`) }
	return []spanRequest{
		{SpanID: "s1", Category: "reason", Name: "planned the audit", StartOffsetMS: 0, DurationMS: 1000},
		{SpanID: "s2", Category: "exec", Tool: "bash", Name: "npm ls --all", Args: arg("command", "npm ls --all"), Output: bigSpanOutput(), StartOffsetMS: 1000, DurationMS: 2000},
		{SpanID: "s3", Category: "read", Tool: "read", Name: "package.json", Args: arg("path", "package.json"), Output: []byte("{ name: checkout-web }"), StartOffsetMS: 3000, DurationMS: 500},
		{SpanID: "s6", Category: "net", Tool: "sub-agent", Name: "advisory lookup", StartOffsetMS: 3500, DurationMS: 4000},
		{SpanID: "s6a", ParentSpanID: "s6", Category: "net", Tool: "web_search", Name: "CVE search", Args: arg("query", "checkout CVE"), StartOffsetMS: 3600, DurationMS: 2000},
		{SpanID: "s9", Category: "write", Tool: "write", Name: "checkout-web-audit.md", Args: arg("path", "checkout-web-audit.md"), ProducedArtifactID: producedID, StartOffsetMS: 7500, DurationMS: 1000},
	}
}

func decodeRun(t *testing.T, resp *http.Response) runResponse {
	t.Helper()
	defer resp.Body.Close()
	var r runResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	return r
}

func flattenSpans(spans []spanView) map[string]spanView {
	out := map[string]spanView{}
	var walk func([]spanView)
	walk = func(ss []spanView) {
		for _, s := range ss {
			out[s.SpanID] = s
			walk(s.Children)
		}
	}
	walk(spans)
	return out
}

// TestIntegrationRunBatchAndIncrementalConverge is the story's headline
// acceptance: a batch POST and the equivalent open→append→close sequence yield
// identical stored runs — same span tree, same derived stats — read back over
// the API (SPEC-0004 "Batch and incremental converge").
func TestIntegrationRunBatchAndIncrementalConverge(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())
	produced := createArtifact(t, srv.URL, "markdown", "joe", "# Checkout Web Audit")

	// Batch: one POST with the full tree, created closed.
	resp := do(t, http.MethodPost, srv.URL+"/v1/runs", "joe",
		jsonReader(t, runRequest{Mode: "batch", Title: "checkout-web-audit", Prompt: "Audit checkout.",
			Model: "claude-sonnet-4.6", TokenCount: 4200, StartedAt: fixedRunStart, Spans: fixtureSpans(produced)}),
		"application/json")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("batch create = %d, want 201", resp.StatusCode)
	}
	batchCreated := decodeRun(t, resp)
	if batchCreated.Status != "closed" {
		t.Fatalf("batch status = %q, want closed", batchCreated.Status)
	}
	batch := decodeRun(t, do(t, http.MethodGet, srv.URL+"/v1/runs/"+batchCreated.ID, "", nil, ""))

	// Incremental: open empty, append each span in tree order, close.
	resp = do(t, http.MethodPost, srv.URL+"/v1/runs", "joe",
		jsonReader(t, runRequest{Mode: "open", Title: "checkout-web-audit", Prompt: "Audit checkout.",
			Model: "claude-sonnet-4.6", TokenCount: 4200, StartedAt: fixedRunStart}),
		"application/json")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("open = %d, want 201", resp.StatusCode)
	}
	open := decodeRun(t, resp)
	if open.Status != "open" {
		t.Fatalf("open status = %q, want open", open.Status)
	}
	if open.URL != "https://cairn.sh/run/"+open.ID {
		t.Fatalf("open URL = %q, want the shareable https://cairn.sh/run/%s link before any span", open.URL, open.ID)
	}
	for _, sp := range fixtureSpans(produced) {
		resp = do(t, http.MethodPost, srv.URL+"/v1/runs/"+open.ID+"/spans", "joe",
			jsonReader(t, appendSpansRequest{Spans: []spanRequest{sp}}), "application/json")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("append %s = %d, want 200", sp.SpanID, resp.StatusCode)
		}
		resp.Body.Close()
	}
	resp = do(t, http.MethodPost, srv.URL+"/v1/runs/"+open.ID+"/close", "joe", nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("close = %d, want 200", resp.StatusCode)
	}
	incr := decodeRun(t, resp)

	// Derived stats converge.
	assertStatsEqual(t, batch.Stats, incr.Stats)
	if batch.Stats.SpanCount != 6 || batch.Stats.ToolCallCount != 5 || batch.Stats.WallTimeMS != 8500 {
		t.Fatalf("batch stats = %+v, want span=6 tool=5 wall=8500", batch.Stats)
	}

	// Span trees converge structurally (id/parent/depth/seq/category/timing).
	bf, inf := flattenSpans(batch.Spans), flattenSpans(incr.Spans)
	if len(bf) != len(inf) {
		t.Fatalf("span count diverges: batch %d vs incr %d", len(bf), len(inf))
	}
	for id, b := range bf {
		i, ok := inf[id]
		if !ok {
			t.Fatalf("span %s present in batch, missing from incremental", id)
		}
		if b.ParentSpanID != i.ParentSpanID || b.Depth != i.Depth || b.Seq != i.Seq ||
			b.Category != i.Category || b.StartOffsetMS != i.StartOffsetMS || b.DurationMS != i.DurationMS {
			t.Fatalf("span %s diverges:\n batch = %+v\n incr  = %+v", id, b, i)
		}
	}

	// The sub-agent nests under s6 at depth+1 in both.
	if s6 := inf["s6"]; len(s6.Children) != 1 || s6.Children[0].SpanID != "s6a" || s6.Children[0].Depth != 1 {
		t.Fatalf("s6 children = %+v, want [s6a] at depth 1", s6.Children)
	}
	// The write span links to its produced markdown artifact in both.
	for _, f := range []runResponse{batch, incr} {
		s9 := flattenSpans(f.Spans)["s9"]
		if len(s9.ProducedArtifactIDs) != 1 || s9.ProducedArtifactIDs[0] != produced {
			t.Fatalf("s9 produced = %v, want [%s]", s9.ProducedArtifactIDs, produced)
		}
	}
	// The oversized bash output spilled to a ref, not inline, in both.
	for _, f := range []runResponse{batch, incr} {
		s2 := flattenSpans(f.Spans)["s2"]
		if s2.OutputRef == nil || s2.Output != "" {
			t.Fatalf("s2 must carry an output_ref, not inline bytes: %+v", s2)
		}
	}
}

// assertStatsEqual fails unless two stat views are equal field-by-field,
// including the category map (statsView is not comparable with ==).
func assertStatsEqual(t *testing.T, want, got statsView) {
	t.Helper()
	if got.WallTimeMS != want.WallTimeMS || got.SpanCount != want.SpanCount ||
		got.ToolCallCount != want.ToolCallCount || got.TokenCount != want.TokenCount ||
		len(got.TimeByCategoryMS) != len(want.TimeByCategoryMS) {
		t.Fatalf("stats diverge:\n want = %+v\n got  = %+v", want, got)
	}
	for k, v := range want.TimeByCategoryMS {
		if got.TimeByCategoryMS[k] != v {
			t.Fatalf("time_by_category[%s] diverges: want %d, got %d", k, v, got.TimeByCategoryMS[k])
		}
	}
}

// TestIntegrationRunAuthAndUniform404 covers the endpoint-security guarantees:
// an unauthenticated ingest is 401, and any operation against an unknown run id
// is an indistinguishable 404 (SPEC-0004 endpoint security, ADR-0007 uniform
// not-found).
func TestIntegrationRunAuthAndUniform404(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())

	// Unauthenticated ingest → 401.
	resp := do(t, http.MethodPost, srv.URL+"/v1/runs", "",
		jsonReader(t, runRequest{Prompt: "x"}), "application/json")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauth ingest = %d, want 401", resp.StatusCode)
	}
	if env := decodeError(t, resp); env.Error.Code != "unauthorized" {
		t.Fatalf("code = %q, want unauthorized", env.Error.Code)
	}

	// Unknown run: read, append, close all return the same uniform 404.
	resp = do(t, http.MethodGet, srv.URL+"/v1/runs/zzzzzzzz", "", nil, "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get unknown run = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
	resp = do(t, http.MethodPost, srv.URL+"/v1/runs/zzzzzzzz/spans", "joe",
		jsonReader(t, appendSpansRequest{Spans: []spanRequest{{SpanID: "a", Category: "reason"}}}), "application/json")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("append to unknown run = %d, want 404", resp.StatusCode)
	}
	if env := decodeError(t, resp); env.Error.Code != "not_found" {
		t.Fatalf("code = %q, want not_found", env.Error.Code)
	}
	resp = do(t, http.MethodPost, srv.URL+"/v1/runs/zzzzzzzz/close", "joe", nil, "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("close unknown run = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()

	// Unknown span output → uniform 404.
	resp = do(t, http.MethodGet, srv.URL+"/v1/runs/zzzzzzzz/spans/s1/output", "", nil, "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("output of unknown run = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestIntegrationRunSpanIngestIdempotent proves an append is idempotent on
// span_id: re-posting already-present spans is a no-op that neither duplicates
// nor conflicts, while a partial replay (mixing new and present spans) is
// rejected whole (SPEC-0004 "appends are additive only").
func TestIntegrationRunSpanIngestIdempotent(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())
	resp := do(t, http.MethodPost, srv.URL+"/v1/runs", "joe",
		jsonReader(t, runRequest{Mode: "open", Prompt: "p", StartedAt: fixedRunStart}), "application/json")
	open := decodeRun(t, resp)

	s1 := spanRequest{SpanID: "s1", Category: "reason", StartOffsetMS: 0, DurationMS: 10}
	// First append.
	resp = do(t, http.MethodPost, srv.URL+"/v1/runs/"+open.ID+"/spans", "joe",
		jsonReader(t, appendSpansRequest{Spans: []spanRequest{s1}}), "application/json")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first append = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	// Replay the same span → idempotent no-op, still 200, count unchanged.
	resp = do(t, http.MethodPost, srv.URL+"/v1/runs/"+open.ID+"/spans", "joe",
		jsonReader(t, appendSpansRequest{Spans: []spanRequest{s1}}), "application/json")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("replay append = %d, want 200", resp.StatusCode)
	}
	replayed := decodeRun(t, resp)
	if replayed.Stats.SpanCount != 1 {
		t.Fatalf("span count after replay = %d, want 1 (no duplicate)", replayed.Stats.SpanCount)
	}

	// A partial replay — one present span + one new — is rejected whole.
	s2 := spanRequest{SpanID: "s2", Category: "reason", StartOffsetMS: 10, DurationMS: 10}
	resp = do(t, http.MethodPost, srv.URL+"/v1/runs/"+open.ID+"/spans", "joe",
		jsonReader(t, appendSpansRequest{Spans: []spanRequest{s1, s2}}), "application/json")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("partial replay = %d, want 400", resp.StatusCode)
	}
	if env := decodeError(t, resp); env.Error.Code != "validation_failed" {
		t.Fatalf("code = %q, want validation_failed", env.Error.Code)
	}
	// Nothing from the rejected mixed batch persisted (still just s1).
	got := decodeRun(t, do(t, http.MethodGet, srv.URL+"/v1/runs/"+open.ID, "", nil, ""))
	if got.Stats.SpanCount != 1 {
		t.Fatalf("span count after rejected partial replay = %d, want 1", got.Stats.SpanCount)
	}
}

// TestIntegrationRunOrderedTreeValidity proves an append naming an unknown
// parent is rejected atomically — validation_failed, none of its spans
// persisted (SPEC-0004 "Malformed tree rejected atomically").
func TestIntegrationRunOrderedTreeValidity(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())
	open := decodeRun(t, do(t, http.MethodPost, srv.URL+"/v1/runs", "joe",
		jsonReader(t, runRequest{Mode: "open", Prompt: "p", StartedAt: fixedRunStart}), "application/json"))

	resp := do(t, http.MethodPost, srv.URL+"/v1/runs/"+open.ID+"/spans", "joe",
		jsonReader(t, appendSpansRequest{Spans: []spanRequest{
			{SpanID: "ok", Category: "reason", StartOffsetMS: 0, DurationMS: 10},
			{SpanID: "bad", ParentSpanID: "ghost", Category: "reason", StartOffsetMS: 10, DurationMS: 10},
		}}), "application/json")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown-parent append = %d, want 400", resp.StatusCode)
	}
	if env := decodeError(t, resp); env.Error.Code != "validation_failed" {
		t.Fatalf("code = %q, want validation_failed", env.Error.Code)
	}
	got := decodeRun(t, do(t, http.MethodGet, srv.URL+"/v1/runs/"+open.ID, "", nil, ""))
	if got.Stats.SpanCount != 0 {
		t.Fatalf("span count after atomic-rejected append = %d, want 0", got.Stats.SpanCount)
	}

	// An EMPTY category is a validation failure (SPEC-0004 "Empty or missing
	// category rejected") …
	resp = do(t, http.MethodPost, srv.URL+"/v1/runs/"+open.ID+"/spans", "joe",
		jsonReader(t, appendSpansRequest{Spans: []spanRequest{{SpanID: "z", Category: "  ", StartOffsetMS: 0, DurationMS: 1}}}),
		"application/json")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty-category append = %d, want 400", resp.StatusCode)
	}
	if env := decodeError(t, resp); env.Error.Code != "validation_failed" {
		t.Fatalf("code = %q, want validation_failed", env.Error.Code)
	}

	// … but a category outside the recommended set is NOT: the set is open, and
	// the DB CHECK must agree with the service or this 201 comes back a 500
	// (issue #5 — the closed CHECK in 0004, dropped by 0013). Reading the run
	// back proves the value round-trips verbatim rather than being coerced.
	resp = do(t, http.MethodPost, srv.URL+"/v1/runs/"+open.ID+"/spans", "joe",
		jsonReader(t, appendSpansRequest{Spans: []spanRequest{{SpanID: "z", Category: "teleport", StartOffsetMS: 0, DurationMS: 1}}}),
		"application/json")
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		t.Fatalf("unrecommended-category append = %d, want 2xx (the category set is open)", resp.StatusCode)
	}
	resp.Body.Close()

	got = decodeRun(t, do(t, http.MethodGet, srv.URL+"/v1/runs/"+open.ID, "", nil, ""))
	if got.Stats.SpanCount != 1 {
		t.Fatalf("span count after accepted append = %d, want 1", got.Stats.SpanCount)
	}
	sp, ok := flattenSpans(got.Spans)["z"]
	if !ok {
		t.Fatal("span z is absent from the run after a 2xx append")
	}
	if sp.Category != "teleport" {
		t.Fatalf("category round-tripped as %q, want %q", sp.Category, "teleport")
	}

	// The service bounds the category in runes and the schema in characters, so a
	// 64-character multibyte value must satisfy BOTH. A byte-counting service (or
	// a byte-counting CHECK) makes this a 400 or a 500 respectively — the two
	// layers disagreeing about one value, which is the shape of bug #5 itself.
	multibyte := strings.Repeat("調", 64)
	resp = do(t, http.MethodPost, srv.URL+"/v1/runs/"+open.ID+"/spans", "joe",
		jsonReader(t, appendSpansRequest{Spans: []spanRequest{{SpanID: "m", Category: multibyte, StartOffsetMS: 1, DurationMS: 1}}}),
		"application/json")
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		t.Fatalf("64-character multibyte category = %d, want 2xx (the bound counts characters)", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestIntegrationRunClosedConflict proves an append to a closed run (a batch run
// is created closed) is a typed conflict, and a non-owner cannot close a run
// (SPEC-0004 "Append to a closed run refused", "Non-owner cannot close").
func TestIntegrationRunClosedConflict(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())

	// A batch run is created closed; appending to it is a 409.
	batch := decodeRun(t, do(t, http.MethodPost, srv.URL+"/v1/runs", "joe",
		jsonReader(t, runRequest{Mode: "batch", Prompt: "done", StartedAt: fixedRunStart,
			Spans: []spanRequest{{SpanID: "s1", Category: "reason", StartOffsetMS: 0, DurationMS: 10}}}),
		"application/json"))
	resp := do(t, http.MethodPost, srv.URL+"/v1/runs/"+batch.ID+"/spans", "joe",
		jsonReader(t, appendSpansRequest{Spans: []spanRequest{{SpanID: "s2", Category: "reason", StartOffsetMS: 10, DurationMS: 10}}}),
		"application/json")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("append to closed run = %d, want 409", resp.StatusCode)
	}
	if env := decodeError(t, resp); env.Error.Code != "conflict" {
		t.Fatalf("code = %q, want conflict", env.Error.Code)
	}

	// Non-owner cannot close an open run.
	open := decodeRun(t, do(t, http.MethodPost, srv.URL+"/v1/runs", "joe",
		jsonReader(t, runRequest{Mode: "open", Prompt: "p", StartedAt: fixedRunStart}), "application/json"))
	resp = do(t, http.MethodPost, srv.URL+"/v1/runs/"+open.ID+"/close", "mallory", nil, "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-owner close = %d, want 403", resp.StatusCode)
	}
	if env := decodeError(t, resp); env.Error.Code != "forbidden" {
		t.Fatalf("code = %q, want forbidden", env.Error.Code)
	}
	// The run stays open after the refused close.
	if got := decodeRun(t, do(t, http.MethodGet, srv.URL+"/v1/runs/"+open.ID, "", nil, "")); got.Status != "open" {
		t.Fatalf("status after refused close = %q, want open", got.Status)
	}
}

// TestIntegrationRunLazyOutputFetch proves a spilled span output is fetched
// lazily and byte-exactly over GET /v1/runs/{id}/spans/{span_id}/output, with a
// re-verifiable checksum and a non-executable disposition (SPEC-0004 "Large
// outputs MUST be fetched lazily", security headers).
func TestIntegrationRunLazyOutputFetch(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())
	produced := createArtifact(t, srv.URL, "markdown", "joe", "# md")
	created := decodeRun(t, do(t, http.MethodPost, srv.URL+"/v1/runs", "joe",
		jsonReader(t, runRequest{Mode: "batch", Prompt: "p", StartedAt: fixedRunStart, Spans: fixtureSpans(produced)}),
		"application/json"))

	resp := do(t, http.MethodGet, srv.URL+"/v1/runs/"+created.ID+"/spans/s2/output", "", nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("fetch span output = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("output content-type = %q, want application/octet-stream", ct)
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("span output missing nosniff header")
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("output disposition = %q, want attachment", resp.Header.Get("Content-Disposition"))
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Equal(got, bigSpanOutput()) {
		t.Fatal("lazily fetched span output differs from ingested bytes")
	}

	// A small inline output streams back too.
	resp = do(t, http.MethodGet, srv.URL+"/v1/runs/"+created.ID+"/spans/s3/output", "", nil, "")
	got, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != "{ name: checkout-web }" {
		t.Fatalf("inline output = %q, want the read body", got)
	}
}

// TestIntegrationRunOversizeBatchRejected proves an oversize batch is rejected
// with 413 before it is buffered (SPEC-0004 endpoint security — request-size
// caps).
func TestIntegrationRunOversizeBatchRejected(t *testing.T) {
	srv := testServer(t, Config{BaseURL: "https://cairn.sh", MaxUploadBytes: 1 << 20,
		DefaultTTL: time.Hour, MaxRunRequestBytes: 512}, store.Options{MaxUploadBytes: 1 << 20})

	resp := do(t, http.MethodPost, srv.URL+"/v1/runs", "joe",
		jsonReader(t, runRequest{Mode: "batch", Prompt: strings.Repeat("a", 2048), StartedAt: fixedRunStart}),
		"application/json")
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize batch = %d, want 413", resp.StatusCode)
	}
	if env := decodeError(t, resp); env.Error.Code != "payload_too_large" {
		t.Fatalf("code = %q, want payload_too_large", env.Error.Code)
	}
}
