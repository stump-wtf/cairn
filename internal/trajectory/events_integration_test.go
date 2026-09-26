package trajectory

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/event"
)

// Governing: ADR-0022 (annotation and trace lifecycle events), SPEC-0016 EV-2
// "Batch run emits closed after created", EV-3 "Payload Shape"; SPEC-0023
// "Creation of runs and hooks MUST emit artifact.created".

// eventSpy records every event the trajectory service hands its emitter.
type eventSpy struct {
	mu  sync.Mutex
	evs []event.Event
}

func (s *eventSpy) Emit(ev event.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evs = append(s.evs, ev)
}

// forRun returns, in emission order, the events whose subject is publicID.
func (s *eventSpy) forRun(publicID string) []event.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []event.Event
	for _, ev := range s.evs {
		if ev.Subject.PublicID == publicID {
			out = append(out, ev)
		}
	}
	return out
}

func (s *eventSpy) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.evs)
}

// newEventHarness is newHarness with the service's emitter replaced by a spy.
func newEventHarness(t *testing.T) (*Service, *eventSpy) {
	t.Helper()
	_, _, pool, obj := newHarness(t)
	spy := &eventSpy{}
	return NewService(pool, obj, Options{Emitter: spy}), spy
}

// agentCreator is the fixture's creator as an MCP OAuth agent would be derived.
func agentCreator(in RunInput) RunInput {
	in.ActorKind = event.KindAgent
	in.Auth = event.AuthOAuth
	in.Provenance.OnBehalfOf = "claude-code/2.1"
	in.Provenance.Model = in.Model
	return in
}

// assertRunSubject checks the subject resolves the run exactly as its
// artifact.created does (EV-3 "Subject fields resolve the artifact for every
// kind").
func assertRunSubject(t *testing.T, ev event.Event, run *Run) {
	t.Helper()
	sub := ev.Subject
	if sub.PublicID != run.PublicID || sub.ShareType != artifact.TypeTrajectory ||
		sub.Title != run.Title || sub.WebPath != "/run/"+run.PublicID || sub.OwnerID != "joe" {
		t.Errorf("%s subject = %+v, want run %s (trajectory, %q, /run/%s, owner joe)",
			ev.Kind, sub, run.PublicID, run.Title, run.PublicID)
	}
	if !sub.ExpiresAt.Round(time.Millisecond).Equal(run.ExpiresAt.Round(time.Millisecond)) {
		t.Errorf("%s subject expires_at = %v, want %v", ev.Kind, sub.ExpiresAt, run.ExpiresAt)
	}
}

// assertClosedMatchesStored checks the run.closed payload against the run as
// read back from the database (EV-3: "the data.run fields match the stored
// run").
func assertClosedMatchesStored(t *testing.T, ev event.Event, run *Run) {
	t.Helper()
	r := ev.Run
	if r == nil {
		t.Fatal("run.closed carries no run payload")
	}
	if r.Status != "closed" || run.Status != StatusClosed {
		t.Errorf("status = %q (stored %q), want closed", r.Status, run.Status)
	}
	if r.SpanCount != run.Stats.SpanCount {
		t.Errorf("span_count = %d, stored run has %d", r.SpanCount, run.Stats.SpanCount)
	}
	if !r.StartedAt.Equal(run.StartedAt) || !r.EndedAt.Equal(run.EndedAt) {
		t.Errorf("started/ended = %v/%v, stored %v/%v", r.StartedAt, r.EndedAt, run.StartedAt, run.EndedAt)
	}
	if r.DurationMS != run.Stats.WallTimeMS {
		t.Errorf("duration_ms = %d, stored wall time %d", r.DurationMS, run.Stats.WallTimeMS)
	}
	if ev.Comment != nil || ev.Reaction != nil || ev.Model != "" {
		t.Errorf("run.closed carries another kind's payload: %+v", ev)
	}
}

// TestBatchRunEmitsCreatedThenClosed is EV-2 "Batch run emits closed after
// created": one batch ingest yields artifact.created and then run.closed for
// that run, both after commit, and the run.closed payload matches the stored
// run. The start carries sub-microsecond precision Postgres rounds away, so
// the payload must come from the stored row, not the request.
func TestBatchRunEmitsCreatedThenClosed(t *testing.T) {
	svc, spy := newEventHarness(t)
	ctx := context.Background()

	in := agentCreator(checkoutWebAudit(""))
	in.StartedAt = fixedStart.Add(1234567 * time.Nanosecond)
	created, err := svc.CreateBatchRun(ctx, in)
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	stored, err := svc.GetRun(ctx, created.PublicID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	evs := spy.forRun(created.PublicID)
	if len(evs) != 2 || evs[0].Kind != event.ArtifactCreated || evs[1].Kind != event.RunClosed {
		t.Fatalf("events = %v, want [artifact.created run.closed]", kinds(evs))
	}
	want := event.Actor{ID: "joe", Channel: artifact.ChannelMCP, OnBehalfOf: "claude-code/2.1",
		Kind: event.KindAgent, Auth: event.AuthOAuth}
	for _, ev := range evs {
		if ev.Actor != want {
			t.Errorf("%s actor = %+v, want the creator %+v", ev.Kind, ev.Actor, want)
		}
		assertRunSubject(t, ev, stored)
	}
	if evs[0].Model != "claude-sonnet-4.6" || evs[0].Run != nil {
		t.Errorf("artifact.created model = %q, run = %+v; want the run's model and no run payload",
			evs[0].Model, evs[0].Run)
	}
	assertClosedMatchesStored(t, evs[1], stored)
	if evs[1].Run.SpanCount != 13 || evs[1].Run.DurationMS != 34200 {
		t.Errorf("run.closed = %+v, want the fixture's 13 spans over 34200ms", evs[1].Run)
	}
}

// TestOpenRunEmitsCreatedAndCloseEmitsOnce: opening a run announces only its
// creation; CloseRun then emits exactly one run.closed attributed to the
// closer, and a second close (ErrRunClosed, a 409) or a refused non-owner
// close emits nothing (EV-2).
func TestOpenRunEmitsCreatedAndCloseEmitsOnce(t *testing.T) {
	svc, spy := newEventHarness(t)
	ctx := context.Background()

	in := agentCreator(checkoutWebAudit(""))
	spans := in.Spans
	in.Spans = spans[:1]
	open, err := svc.OpenRun(ctx, in)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if got := kinds(spy.forRun(open.PublicID)); len(got) != 1 || got[0] != event.ArtifactCreated {
		t.Fatalf("events after open = %v, want [artifact.created]", got)
	}
	if _, err := svc.AppendSpans(ctx, open.PublicID, "joe", spans[1:3]); err != nil {
		t.Fatalf("append: %v", err)
	}
	if n := len(spy.forRun(open.PublicID)); n != 1 {
		t.Fatalf("appending spans emitted %d more events, want none", n-1)
	}

	// A non-owner's refused close changes nothing and announces nothing.
	mallory := event.Actor{ID: "mallory", Channel: artifact.ChannelMCP, Kind: event.KindAgent, Auth: event.AuthOAuth}
	if _, err := svc.CloseRun(ctx, open.PublicID, mallory); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("non-owner close err = %v, want ErrNotOwner", err)
	}
	if n := len(spy.forRun(open.PublicID)); n != 1 {
		t.Fatalf("refused close emitted %d events, want none", n-1)
	}

	// The owner closes from the browser: the closer, not the creator, is the actor.
	closer := event.Actor{ID: "joe", Channel: artifact.ChannelWeb, Kind: event.KindHuman, Auth: event.AuthSession}
	if _, err := svc.CloseRun(ctx, open.PublicID, closer); err != nil {
		t.Fatalf("close: %v", err)
	}
	stored, err := svc.GetRun(ctx, open.PublicID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	evs := spy.forRun(open.PublicID)
	if len(evs) != 2 || evs[1].Kind != event.RunClosed {
		t.Fatalf("events after close = %v, want [artifact.created run.closed]", kinds(evs))
	}
	if evs[1].Actor != closer {
		t.Errorf("run.closed actor = %+v, want the closer %+v", evs[1].Actor, closer)
	}
	assertRunSubject(t, evs[1], stored)
	assertClosedMatchesStored(t, evs[1], stored)
	if evs[1].Run.SpanCount != 3 {
		t.Errorf("run.closed span_count = %d, want 3", evs[1].Run.SpanCount)
	}

	// Closing an already-closed run is a conflict and emits nothing.
	if _, err := svc.CloseRun(ctx, open.PublicID, closer); !errors.Is(err, ErrRunClosed) {
		t.Fatalf("second close err = %v, want ErrRunClosed", err)
	}
	if n := len(spy.forRun(open.PublicID)); n != 2 {
		t.Fatalf("second close emitted %d more events, want none", n-2)
	}
}

// TestRolledBackRunEmitsNothing: a batch whose transaction rolls back (a span
// names a produced artifact that does not exist, found only after the run row
// is inserted) emits neither artifact.created nor run.closed (EV-2 "Rolled-back
// write emits nothing").
func TestRolledBackRunEmitsNothing(t *testing.T) {
	svc, spy := newEventHarness(t)
	in := agentCreator(checkoutWebAudit("NOPENOPE"))
	if _, err := svc.CreateBatchRun(context.Background(), in); err == nil {
		t.Fatal("batch naming a missing produced artifact succeeded, want a validation error")
	}
	if n := spy.count(); n != 0 {
		t.Fatalf("rolled-back batch emitted %d events, want none", n)
	}
}

func kinds(evs []event.Event) []event.Kind {
	out := make([]event.Kind, len(evs))
	for i, ev := range evs {
		out[i] = ev.Kind
	}
	return out
}
