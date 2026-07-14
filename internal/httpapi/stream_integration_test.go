package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// sseEvent is one parsed Server-Sent Event (or a comment line, with Comment set
// and Event empty).
type sseEvent struct {
	ID      string
	Event   string
	Data    string
	Comment string
}

// streamConn is a live SSE connection whose events are parsed on a background
// goroutine and delivered on events, so a test can await the next event with a
// timeout instead of blocking on a hung read. Cancel tears the connection down.
type streamConn struct {
	events chan sseEvent
	cancel context.CancelFunc
	resp   *http.Response
}

// openStream connects to the run's SSE endpoint (optionally resuming after
// lastEventID, -1 to omit) and starts parsing events in the background.
func openStream(t *testing.T, url, runID string, lastEventID int64) *streamConn {
	t.Helper()
	return openStreamAt(t, url+"/v1/runs/"+runID+"/stream", lastEventID)
}

// openStreamAt connects to an arbitrary SSE endpoint URL (optionally resuming
// after lastEventID, -1 to omit) and starts parsing events in the background
// — the share-type-agnostic core openStream (above, for trajectory runs) and
// openHookStream (hook_stream_integration_test.go, for webhook endpoints)
// both build on.
func openStreamAt(t *testing.T, url string, lastEventID int64) *streamConn {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		cancel()
		t.Fatalf("new stream request: %v", err)
	}
	if lastEventID >= 0 {
		req.Header.Set("Last-Event-ID", strconv.FormatInt(lastEventID, 10))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("open stream: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		cancel()
		t.Fatalf("stream status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		resp.Body.Close()
		cancel()
		t.Fatalf("stream content-type = %q, want text/event-stream", ct)
	}

	sc := &streamConn{events: make(chan sseEvent, 64), cancel: cancel, resp: resp}
	go sc.parse(resp)
	t.Cleanup(func() { cancel(); resp.Body.Close() })
	return sc
}

// parse reads the event stream line-by-line, emitting one sseEvent per frame
// (frames are blank-line separated). Comment lines (": …") emit their own event
// with Comment set.
func (sc *streamConn) parse(resp *http.Response) {
	defer close(sc.events)
	sr := bufio.NewScanner(resp.Body)
	sr.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var cur sseEvent
	dirty := false
	for sr.Scan() {
		line := sr.Text()
		switch {
		case line == "":
			if dirty {
				sc.events <- cur
				cur, dirty = sseEvent{}, false
			}
		case strings.HasPrefix(line, ":"):
			sc.events <- sseEvent{Comment: strings.TrimSpace(line[1:])}
		case strings.HasPrefix(line, "id:"):
			cur.ID = strings.TrimSpace(line[3:])
			dirty = true
		case strings.HasPrefix(line, "event:"):
			cur.Event = strings.TrimSpace(line[6:])
			dirty = true
		case strings.HasPrefix(line, "data:"):
			cur.Data = strings.TrimSpace(line[5:])
			dirty = true
		}
	}
}

// next awaits the next non-comment event, failing on timeout so a missing event
// is a test failure rather than a hang.
func (sc *streamConn) next(t *testing.T) sseEvent {
	t.Helper()
	for {
		select {
		case ev, ok := <-sc.events:
			if !ok {
				t.Fatal("stream closed before the expected event")
			}
			if ev.Comment != "" {
				continue // skip heartbeats / stream-open marker
			}
			return ev
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for a stream event")
		}
	}
}

// nextComment awaits the next comment (heartbeat) event.
func (sc *streamConn) nextComment(t *testing.T) sseEvent {
	t.Helper()
	for {
		select {
		case ev, ok := <-sc.events:
			if !ok {
				t.Fatal("stream closed before a comment")
			}
			if ev.Comment == "" {
				continue
			}
			return ev
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for a heartbeat")
		}
	}
}

// expectEnd asserts the stream closes (server ends the response) within the
// timeout.
func (sc *streamConn) expectEnd(t *testing.T) {
	t.Helper()
	for {
		select {
		case _, ok := <-sc.events:
			if !ok {
				return
			}
		case <-time.After(3 * time.Second):
			t.Fatal("stream did not end when expected")
		}
	}
}

func spanData(t *testing.T, ev sseEvent) streamSpanView {
	t.Helper()
	if ev.Event != "span" {
		t.Fatalf("event = %q, want span", ev.Event)
	}
	var v streamSpanView
	if err := json.Unmarshal([]byte(ev.Data), &v); err != nil {
		t.Fatalf("decode span event: %v", err)
	}
	return v
}

func statusData(t *testing.T, ev sseEvent) string {
	t.Helper()
	if ev.Event != "status" {
		t.Fatalf("event = %q, want status", ev.Event)
	}
	var s struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(ev.Data), &s); err != nil {
		t.Fatalf("decode status event: %v", err)
	}
	return s.Status
}

func openRun(t *testing.T, srvURL string) runResponse {
	t.Helper()
	return decodeRun(t, do(t, http.MethodPost, srvURL+"/v1/runs", "joe",
		jsonReader(t, runRequest{Mode: "open", Prompt: "p", StartedAt: fixedRunStart}), "application/json"))
}

func appendSpan(t *testing.T, srvURL, runID string, sp spanRequest) {
	t.Helper()
	resp := do(t, http.MethodPost, srvURL+"/v1/runs/"+runID+"/spans", "joe",
		jsonReader(t, appendSpansRequest{Spans: []spanRequest{sp}}), "application/json")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("append %s = %d, want 200", sp.SpanID, resp.StatusCode)
	}
	resp.Body.Close()
}

// TestIntegrationRunStreamLateJoinerThenLive is the story's headline SSE
// acceptance: a viewer connecting to a run in flight first receives the spans
// captured so far, then receives subsequent appends live, and finally the closed
// transition — history then live with no gap (SPEC-0004 "Late joiner sees
// history then live", "Live Span Stream Delivery").
func TestIntegrationRunStreamLateJoinerThenLive(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())
	run := openRun(t, srv.URL)
	appendSpan(t, srv.URL, run.ID, spanRequest{SpanID: "s1", Category: "reason", StartOffsetMS: 0, DurationMS: 10})
	appendSpan(t, srv.URL, run.ID, spanRequest{SpanID: "s2", Category: "exec", Tool: "bash", Name: "ls", StartOffsetMS: 10, DurationMS: 10})

	// Connect fresh: replay must deliver s1 then s2, then the open status.
	sc := openStream(t, srv.URL, run.ID, -1)
	if v := spanData(t, sc.next(t)); v.SpanID != "s1" || v.StreamSeq != 1 {
		t.Fatalf("first replay span = %s/seq%d, want s1/1", v.SpanID, v.StreamSeq)
	}
	if v := spanData(t, sc.next(t)); v.SpanID != "s2" || v.StreamSeq != 2 {
		t.Fatalf("second replay span = %s/seq%d, want s2/2", v.SpanID, v.StreamSeq)
	}
	if st := statusData(t, sc.next(t)); st != "open" {
		t.Fatalf("status = %q, want open", st)
	}

	// Live append arrives on the tail with the next cursor.
	appendSpan(t, srv.URL, run.ID, spanRequest{SpanID: "s3", Category: "read", Tool: "read", StartOffsetMS: 20, DurationMS: 10})
	if v := spanData(t, sc.next(t)); v.SpanID != "s3" || v.StreamSeq != 3 {
		t.Fatalf("live span = %s/seq%d, want s3/3", v.SpanID, v.StreamSeq)
	}

	// Closing the run flips the badge and ends the stream.
	resp := do(t, http.MethodPost, srv.URL+"/v1/runs/"+run.ID+"/close", "joe", nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("close = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()
	if st := statusData(t, sc.next(t)); st != "closed" {
		t.Fatalf("final status = %q, want closed", st)
	}
	sc.expectEnd(t)
}

// TestIntegrationRunStreamResume proves a reconnecting client resumes after its
// Last-Event-ID with no loss and no duplication (SPEC-0004 "Resume after a
// dropped connection").
func TestIntegrationRunStreamResume(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())
	run := openRun(t, srv.URL)
	for _, id := range []string{"s1", "s2", "s3"} {
		appendSpan(t, srv.URL, run.ID, spanRequest{SpanID: id, Category: "reason", StartOffsetMS: 0, DurationMS: 10})
	}

	// Resume after seq 1: must replay only s2, s3 — never s1 again.
	sc := openStream(t, srv.URL, run.ID, 1)
	first := spanData(t, sc.next(t))
	if first.SpanID != "s2" || first.StreamSeq != 2 {
		t.Fatalf("resume first span = %s/seq%d, want s2/2 (s1 must not repeat)", first.SpanID, first.StreamSeq)
	}
	if v := spanData(t, sc.next(t)); v.SpanID != "s3" || v.StreamSeq != 3 {
		t.Fatalf("resume second span = %s/seq%d, want s3/3", v.SpanID, v.StreamSeq)
	}
	if st := statusData(t, sc.next(t)); st != "open" {
		t.Fatalf("status = %q, want open", st)
	}
}

// TestIntegrationRunStreamClosedRun proves connecting to an already-closed run
// (a batch run is created closed) replays its whole history, reports closed, and
// ends without tailing.
func TestIntegrationRunStreamClosedRun(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())
	batch := decodeRun(t, do(t, http.MethodPost, srv.URL+"/v1/runs", "joe",
		jsonReader(t, runRequest{Mode: "batch", Prompt: "done", StartedAt: fixedRunStart,
			Spans: []spanRequest{
				{SpanID: "s1", Category: "reason", StartOffsetMS: 0, DurationMS: 10},
				{SpanID: "s2", Category: "exec", Tool: "bash", StartOffsetMS: 10, DurationMS: 10},
			}}), "application/json"))

	sc := openStream(t, srv.URL, batch.ID, -1)
	if v := spanData(t, sc.next(t)); v.SpanID != "s1" {
		t.Fatalf("first span = %s, want s1", v.SpanID)
	}
	if v := spanData(t, sc.next(t)); v.SpanID != "s2" {
		t.Fatalf("second span = %s, want s2", v.SpanID)
	}
	if st := statusData(t, sc.next(t)); st != "closed" {
		t.Fatalf("status = %q, want closed", st)
	}
	sc.expectEnd(t)
}

// TestIntegrationRunStreamUnknownRun proves the SSE endpoint returns a uniform
// 404 for an unknown/expired run before any event-stream header is sent
// (ADR-0007 link-capability).
func TestIntegrationRunStreamUnknownRun(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())
	resp := do(t, http.MethodGet, srv.URL+"/v1/runs/zzzzzzzz/stream", "", nil, "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("stream unknown run = %d, want 404", resp.StatusCode)
	}
	if env := decodeError(t, resp); env.Error.Code != "not_found" {
		t.Fatalf("code = %q, want not_found", env.Error.Code)
	}
}

// TestIntegrationRunStreamHeartbeat proves the stream emits heartbeat comments
// on an idle open run, keeping the connection warm.
func TestIntegrationRunStreamHeartbeat(t *testing.T) {
	cfg := noRateLimit()
	cfg.StreamHeartbeat = 100 * time.Millisecond
	srv := testServer(t, cfg, storeOpts())
	run := openRun(t, srv.URL)

	sc := openStream(t, srv.URL, run.ID, -1)
	// Drain the open-status frame, then await a heartbeat.
	if st := statusData(t, sc.next(t)); st != "open" {
		t.Fatalf("status = %q, want open", st)
	}
	if hb := sc.nextComment(t); hb.Comment != "heartbeat" {
		t.Fatalf("comment = %q, want heartbeat", hb.Comment)
	}
}
