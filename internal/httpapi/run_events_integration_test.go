package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"testing"

	"github.com/stump-wtf/cairn/internal/event"
	"github.com/stump-wtf/cairn/internal/outboundhook"
	"github.com/stump-wtf/cairn/internal/store"
)

// Governing: ADR-0022 (annotation and trace lifecycle events), SPEC-0016 EV-2
// "Batch run emits closed after created", EV-3, EV-4; SPEC-0023 "Creation of
// runs and hooks MUST emit artifact.created".

// lifecycleSpy records every lifecycle event handed to Config.Events.
type lifecycleSpy struct {
	mu  sync.Mutex
	evs []event.Event
}

func (s *lifecycleSpy) Emit(ev event.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evs = append(s.evs, ev)
}

// forSubject returns, in emission order, the events about one public id.
func (s *lifecycleSpy) forSubject(id string) []event.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []event.Event
	for _, ev := range s.evs {
		if ev.Subject.PublicID == id {
			out = append(out, ev)
		}
	}
	return out
}

// only returns the single event of kind k about id, failing otherwise.
func (s *lifecycleSpy) only(t *testing.T, id string, k event.Kind) event.Event {
	t.Helper()
	var found []event.Event
	for _, ev := range s.forSubject(id) {
		if ev.Kind == k {
			found = append(found, ev)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d %s events for %s, want exactly 1", len(found), k, id)
	}
	return found[0]
}

func eventKinds(evs []event.Event) []event.Kind {
	out := make([]event.Kind, len(evs))
	for i, ev := range evs {
		out[i] = ev.Kind
	}
	return out
}

// TestIntegrationRunCloseEmitsOnce is SPEC-0016 EV-2 over REST: POST
// /v1/runs/{id}/close emits exactly one run.closed whose data.run matches the
// stored run, and closing an already-closed run (409) emits nothing. A batch
// POST emits artifact.created and then run.closed for that run.
func TestIntegrationRunCloseEmitsOnce(t *testing.T) {
	spy := &lifecycleSpy{}
	cfg := noRateLimit()
	cfg.Events = spy
	srv := testServer(t, cfg, storeOpts())

	// Batch: created, then closed, in that order.
	resp := do(t, http.MethodPost, srv.URL+"/v1/runs", "joe",
		jsonReader(t, runRequest{Mode: "batch", Title: "batch run", Model: "claude-sonnet-4.6",
			StartedAt: fixedRunStart, Spans: fixtureSpans("")[:3]}), "application/json")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("batch create = %d, want 201", resp.StatusCode)
	}
	batch := decodeRun(t, resp)
	if got := eventKinds(spy.forSubject(batch.ID)); len(got) != 2 ||
		got[0] != event.ArtifactCreated || got[1] != event.RunClosed {
		t.Fatalf("batch events = %v, want [artifact.created run.closed]", got)
	}

	// Open: created only.
	resp = do(t, http.MethodPost, srv.URL+"/v1/runs", "joe",
		jsonReader(t, runRequest{Mode: "open", Title: "live run", StartedAt: fixedRunStart,
			Spans: fixtureSpans("")[:1]}), "application/json")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("open = %d, want 201", resp.StatusCode)
	}
	open := decodeRun(t, resp)
	if got := eventKinds(spy.forSubject(open.ID)); len(got) != 1 || got[0] != event.ArtifactCreated {
		t.Fatalf("open events = %v, want [artifact.created]", got)
	}
	resp = do(t, http.MethodPost, srv.URL+"/v1/runs/"+open.ID+"/spans", "joe",
		jsonReader(t, appendSpansRequest{Spans: fixtureSpans("")[1:4]}), "application/json")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("append = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	// A non-owner's close is refused and announces nothing.
	resp = do(t, http.MethodPost, srv.URL+"/v1/runs/"+open.ID+"/close", "mallory", nil, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-owner close = %d, want 403", resp.StatusCode)
	}

	resp = do(t, http.MethodPost, srv.URL+"/v1/runs/"+open.ID+"/close", "joe", nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("close = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()
	stored := decodeRun(t, do(t, http.MethodGet, srv.URL+"/v1/runs/"+open.ID, "", nil, ""))

	closed := spy.only(t, open.ID, event.RunClosed)
	r := closed.Run
	if r == nil || r.Status != stored.Status || r.SpanCount != stored.Stats.SpanCount ||
		!r.StartedAt.Equal(stored.StartedAt) || stored.EndedAt == nil || !r.EndedAt.Equal(*stored.EndedAt) ||
		r.DurationMS != stored.Stats.WallTimeMS {
		t.Fatalf("run.closed data.run = %+v, stored run = status %s, %d spans, %v..%v, wall %dms",
			r, stored.Status, stored.Stats.SpanCount, stored.StartedAt, stored.EndedAt, stored.Stats.WallTimeMS)
	}
	// The dev bearer is an api_token agent credential; the closer is joe.
	if closed.Actor.ID != "joe" || closed.Actor.Kind != event.KindAgent || closed.Actor.Auth != event.AuthAPIToken {
		t.Errorf("run.closed actor = %+v, want joe as an api_token agent", closed.Actor)
	}

	// Closing again is a 409 and emits nothing.
	resp = do(t, http.MethodPost, srv.URL+"/v1/runs/"+open.ID+"/close", "joe", nil, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("second close = %d, want 409", resp.StatusCode)
	}
	if got := eventKinds(spy.forSubject(open.ID)); len(got) != 2 {
		t.Fatalf("events after a refused and a repeated close = %v, want [artifact.created run.closed]", got)
	}
}

// TestIntegrationRunEventsReachTheEncoder wires the real outbound emitter the
// way cmd/cairnd does and proves both run events pass the ADR-0017 encoder: the
// run's artifact.created is delivered to the env target like any other
// creation, and run.closed is encoded and then held back from env targets
// (EV-7), which the per-kind drop counter records. A refused encode would
// increment neither.
func TestIntegrationRunEventsReachTheEncoder(t *testing.T) {
	recv := newHookReceiver(t)
	em := outboundhook.New([]string{recv.srv.URL}, "", "https://cairn.test",
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); em.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	cfg := noRateLimit()
	cfg.Events = em
	srv := testServer(t, cfg, store.Options{MaxUploadBytes: 1 << 20, Emitter: em})

	resp := do(t, http.MethodPost, srv.URL+"/v1/runs", "joe",
		jsonReader(t, runRequest{Mode: "batch", Title: "wired run", StartedAt: fixedRunStart,
			Spans: fixtureSpans("")[:2]}), "application/json")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("batch create = %d, want 201", resp.StatusCode)
	}
	run := decodeRun(t, resp)

	ev := recv.wait(t)
	var body struct {
		Kind string `json:"kind"`
		Data struct {
			ID        string `json:"id"`
			ShareType string `json:"share_type"`
			URL       string `json:"url"`
			ActorKind string `json:"actor_kind"`
			Auth      string `json:"auth"`
		} `json:"data"`
	}
	if err := json.Unmarshal(ev.body, &body); err != nil {
		t.Fatalf("event body not JSON: %v", err)
	}
	if body.Kind != "artifact.created" || body.Data.ID != run.ID || body.Data.ShareType != "trajectory" ||
		body.Data.URL != "https://cairn.test/run/"+run.ID {
		t.Fatalf("delivered %+v, want the run's artifact.created at https://cairn.test/run/%s", body, run.ID)
	}
	if body.Data.ActorKind != "agent" || body.Data.Auth != "api_token" {
		t.Errorf("actor_kind = %q, auth = %q, want agent and api_token", body.Data.ActorKind, body.Data.Auth)
	}
	if n := em.Dropped(event.RunClosed); n != 1 {
		t.Fatalf("encoded-and-held run.closed = %d, want 1 (0 means the encoder refused it)", n)
	}
}
