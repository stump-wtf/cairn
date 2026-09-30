package outboundhook

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/event"
)

// Governing: ADR-0022, SPEC-0016 EV-1 "Event Kind Registry", EV-3 "Payload
// Shape", EV-7 "Recipient Selection and Tenancy", EV-8 "Signed, Bounded
// Delivery with Creation Priority", "Error Handling Standards", "Concurrency
// Safety"; ADR-0017, SPEC-0012 REQ "Event Payload".

// ownerMarker is the subject owner in every fixture. EV-7 forbids it on the
// wire, so no encoded body may contain it.
const ownerMarker = "owner-never-on-the-wire"

func kindSubject() event.Subject {
	return event.Subject{
		PublicID:  "7Kq2mZ",
		ShareType: "markdown",
		Title:     "Work order: rotate the runner image",
		WebPath:   "/7Kq2mZ",
		Tags:      []string{"handoff", "lane:m"},
		ExpiresAt: time.Date(2026, 9, 18, 8, 0, 0, 0, time.UTC),
		OwnerID:   ownerMarker,
	}
}

func humanActor() event.Actor {
	return event.Actor{ID: "alice@example.com", Channel: artifact.ChannelWeb, Kind: event.KindHuman, Auth: event.AuthSession}
}

func agentActor() event.Actor {
	return event.Actor{
		ID: "alice@example.com", Channel: artifact.ChannelMCP, OnBehalfOf: "claude-code/2.1.0",
		Kind: event.KindAgent, Auth: event.AuthOAuth,
	}
}

func creationEvent() event.Event {
	return event.Event{Kind: event.ArtifactCreated, Subject: kindSubject(), Actor: agentActor(), Model: "claude-opus-5"}
}

// kindEvents is one event per registered kind other than artifact.created,
// whose goldens predate this file. The keys name the golden fixtures.
func kindEvents() map[string]event.Event {
	parent := int64(40)
	started := time.Date(2026, 9, 11, 11, 58, 0, 0, time.UTC)
	return map[string]event.Event{
		"comment_created": {
			Kind: event.CommentCreated, Subject: kindSubject(), Actor: agentActor(),
			Comment: &event.Comment{
				ID: 41, ParentID: &parent, AnchorType: "line", AnchorKey: `{"line":12}`,
				Body: "LGTM <ship it> & thanks",
			},
		},
		"reaction_added": {
			Kind: event.ReactionAdded, Subject: kindSubject(), Actor: humanActor(),
			Reaction: &event.Reaction{
				ID: 812, AnchorType: "artifact", AnchorKey: "{}", Emoji: "👍",
				ApprovalClass: true, Approval: true,
			},
		},
		"reaction_removed": {
			Kind: event.ReactionRemoved, Subject: kindSubject(), Actor: agentActor(),
			Reaction: &event.Reaction{ID: 813, AnchorType: "artifact", AnchorKey: "{}", Emoji: "👀"},
		},
		"run_closed": {
			Kind: event.RunClosed, Subject: kindSubject(),
			Actor: event.Actor{ID: "alice@example.com", Channel: artifact.ChannelCLI, Kind: event.KindAgent, Auth: event.AuthPAT},
			Run: &event.Run{
				Status: "closed", SpanCount: 17, StartedAt: started,
				EndedAt: started.Add(90 * time.Second), DurationMS: 90000,
			},
		},
		"artifact_retained": {Kind: event.ArtifactRetained, Subject: kindSubject(), Actor: humanActor()},
		"artifact_released": {Kind: event.ArtifactReleased, Subject: kindSubject(), Actor: humanActor()},
		"artifact_deleted":  {Kind: event.ArtifactDeleted, Subject: kindSubject(), Actor: humanActor()},
	}
}

// allKindEvents is kindEvents plus artifact.created: every registered kind.
func allKindEvents() map[string]event.Event {
	evs := kindEvents()
	evs["artifact_created"] = creationEvent()
	return evs
}

// TestKindFixturesCoverRegistry keeps the fixtures honest: a kind added to the
// registry without a golden fails here instead of shipping unpinned.
func TestKindFixturesCoverRegistry(t *testing.T) {
	have := map[event.Kind]bool{}
	for _, ev := range allKindEvents() {
		have[ev.Kind] = true
	}
	for _, k := range event.RegisteredKinds() {
		if !have[k] {
			t.Errorf("registered kind %q has no fixture in kindEvents", k)
		}
	}
}

// TestGoldenPayloadEveryKind pins the exact signed bytes of each kind.
func TestGoldenPayloadEveryKind(t *testing.T) {
	for name, ev := range kindEvents() {
		t.Run(name, func(t *testing.T) {
			raw, err := goldenEmitter().encode(ev, goldenEventID, goldenCreatedAt)
			if err != nil {
				t.Fatal(err)
			}
			assertGolden(t, name, raw)
		})
	}
}

// preADR0022REST is testdata/artifact_created_rest.golden.json as SPEC-0012
// left it, before ADR-0022. Kept verbatim so the regenerated golden's diff is
// checked by a test, not by a reviewer's eye.
const preADR0022REST = `{"source":"cairn","kind":"artifact.created","event_id":"5b0f3c1e-8a3d-4c55-9f0e-2d7c6b1a9e40","created_at":"2026-09-11T12:00:00Z","data":{"id":"7Kq2mZ","share_type":"markdown","title":"incident notes","url":"https://cairn.example/7Kq2mZ","channel":"via API","model":"claude-opus-5","actor_id":"joestump","expires_at":"2026-09-18T08:00:00Z"}}`

// TestRESTGoldenGainsOnlyAppendedKeys: SPEC-0016 EV-3 "artifact.created gains
// only appended keys". The regenerated golden is the pre-ADR-0022 body with
// exactly actor_kind and auth appended to data, and no other byte different.
func TestRESTGoldenGainsOnlyAppendedKeys(t *testing.T) {
	got, err := os.ReadFile("testdata/artifact_created_rest.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	want := strings.TrimSuffix(preADR0022REST, "}}") + `,"actor_kind":"agent","auth":"pat"}}`
	if string(bytes.TrimSuffix(got, []byte("\n"))) != want {
		t.Fatalf("REST golden is not the SPEC-0012 body plus two appended keys\n got: %s\nwant: %s", got, want)
	}
}

// TestUnknownKindNeverEmitted: SPEC-0016 EV-1. An unregistered kind, including
// the reserved ones, is refused with ErrUnknownKind, logged with its kind and
// event id, and nothing is enqueued. The registered creation is the positive
// control: the same emitter does enqueue it.
func TestUnknownKindNeverEmitted(t *testing.T) {
	for _, k := range []event.Kind{"reaction.add", "", "comment.updated", event.CommentEdited, event.CommentDeleted} {
		t.Run(string(k), func(t *testing.T) {
			ev := creationEvent()
			ev.Kind = k
			if _, err := goldenEmitter().encode(ev, goldenEventID, goldenCreatedAt); !errors.Is(err, ErrUnknownKind) {
				t.Fatalf("encode(%q) error = %v, want ErrUnknownKind", k, err)
			}

			var logs bytes.Buffer
			const target = "https://sink.example/capability-secret"
			e := New([]string{target}, "s3cr3t", "https://cairn.example", slog.New(slog.NewJSONHandler(&logs, nil)))
			e.Emit(ev)
			if n := len(e.ch); n != 0 {
				t.Fatalf("unknown kind %q enqueued %d events", k, n)
			}
			line := onlyLogLine(t, &logs)
			if line["level"] != "ERROR" || line["kind"] != string(k) || line["event_id"] == "" || line["event_id"] == nil {
				t.Fatalf("refusal log = %v, want an ERROR naming the kind and event id", line)
			}
			if strings.Contains(logs.String(), "capability-secret") {
				t.Fatal("refusal log contains the target URL")
			}

			e.Emit(creationEvent())
			if n := len(e.ch); n != 1 {
				t.Fatalf("positive control: registered artifact.created enqueued %d events, want 1", n)
			}
		})
	}
}

// onlyLogLine decodes the single JSON log line in buf.
func onlyLogLine(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	sc := bufio.NewScanner(bytes.NewReader(buf.Bytes()))
	var lines []map[string]any
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("log line is not JSON: %q", sc.Text())
		}
		lines = append(lines, m)
	}
	if len(lines) != 1 {
		t.Fatalf("got %d log lines, want 1: %s", len(lines), buf.String())
	}
	return lines[0]
}

// TestEncodeRefusesUnderivedActor: an event must say who caused it with a
// server-derived kind and auth (EV-4). An actor that skipped derivation, or a
// human holding a bearer credential, is refused rather than encoded without
// actor_kind. That includes the creation adapter: a CreationEvent without a
// kind is not delivered.
func TestEncodeRefusesUnderivedActor(t *testing.T) {
	cases := map[string]func(*event.Actor){
		"no kind":        func(a *event.Actor) { a.Kind = "" },
		"no auth":        func(a *event.Actor) { a.Auth = "" },
		"no id":          func(a *event.Actor) { a.ID = "" },
		"human with pat": func(a *event.Actor) { a.Kind, a.Auth = event.KindHuman, event.AuthPAT },
		"invented kind":  func(a *event.Actor) { a.Kind = "admin" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			ev := creationEvent()
			mutate(&ev.Actor)
			if _, err := goldenEmitter().encode(ev, goldenEventID, goldenCreatedAt); !errors.Is(err, ErrInvalidActor) {
				t.Fatalf("encode error = %v, want ErrInvalidActor", err)
			}
		})
	}
	if _, err := goldenEmitter().encode(creationEvent(), goldenEventID, goldenCreatedAt); err != nil {
		t.Fatalf("positive control: valid actor refused: %v", err)
	}

	e := goldenEmitter()
	legacy := restEvent()
	legacy.ActorKind, legacy.Auth = "", ""
	e.EmitArtifactCreated(legacy)
	if n := len(e.ch); n != 0 {
		t.Fatalf("creation without a derived actor kind enqueued %d events", n)
	}
	e.EmitArtifactCreated(restEvent())
	if n := len(e.ch); n != 1 {
		t.Fatalf("positive control: derived creation enqueued %d events, want 1", n)
	}
}

// TestEncodeRefusesPayloadMismatch: each kind carries exactly its own payload
// object (EV-3), so an event built for one kind and named as another fails.
func TestEncodeRefusesPayloadMismatch(t *testing.T) {
	evs := kindEvents()
	cases := map[string]event.Event{}

	ev := evs["comment_created"]
	ev.Comment = nil
	cases["comment without comment"] = ev

	ev = evs["reaction_added"]
	ev.Comment = &event.Comment{ID: 1}
	cases["reaction with a comment too"] = ev

	ev = evs["run_closed"]
	ev.Run = nil
	cases["run without run"] = ev

	ev = creationEvent()
	ev.Reaction = &event.Reaction{ID: 1}
	cases["creation with a reaction"] = ev

	ev = evs["artifact_deleted"]
	ev.Run = &event.Run{Status: "closed"}
	cases["deletion with a run"] = ev

	for name, ev := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := goldenEmitter().encode(ev, goldenEventID, goldenCreatedAt); !errors.Is(err, ErrPayloadMismatch) {
				t.Fatalf("encode error = %v, want ErrPayloadMismatch", err)
			}
		})
	}
}

// TestSubjectFieldsResolveForEveryKind: SPEC-0016 EV-3. data's subject fields
// identify the artifact exactly as its artifact.created does, for every kind,
// and the subject's owner never reaches the wire (EV-7).
func TestSubjectFieldsResolveForEveryKind(t *testing.T) {
	subjectOf := func(t *testing.T, ev event.Event) map[string]any {
		t.Helper()
		raw, err := goldenEmitter().encode(ev, goldenEventID, goldenCreatedAt)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(raw, []byte(ownerMarker)) {
			t.Fatalf("%s body carries the subject owner: %s", ev.Kind, raw)
		}
		var body struct {
			Kind string         `json:"kind"`
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		if body.Kind != string(ev.Kind) {
			t.Fatalf("body kind = %q, want %q", body.Kind, ev.Kind)
		}
		out := map[string]any{}
		for _, k := range []string{"id", "url", "share_type", "title", "tags", "expires_at"} {
			out[k] = body.Data[k]
		}
		return out
	}
	want := subjectOf(t, creationEvent())
	if want["id"] != "7Kq2mZ" || want["url"] != "https://cairn.example/7Kq2mZ" {
		t.Fatalf("positive control: creation subject = %v", want)
	}
	for name, ev := range kindEvents() {
		t.Run(name, func(t *testing.T) {
			got := subjectOf(t, ev)
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(want)
			if !bytes.Equal(gotJSON, wantJSON) {
				t.Fatalf("subject fields = %s, want the creation's %s", gotJSON, wantJSON)
			}
		})
	}
}

// TestLongCommentTruncatedInEventOnly: SPEC-0016 EV-3. A 10 KiB comment is cut
// to at most 4096 bytes in the event, with body_truncated, and the event the
// producer handed over (the stored comment) is unchanged.
func TestLongCommentTruncatedInEventOnly(t *testing.T) {
	ev := kindEvents()["comment_created"]
	stored := strings.Repeat("a", 10*1024)
	ev.Comment.Body = stored

	c := encodedComment(t, ev)
	if len(c.Body) > commentBodyMax || len(c.Body) != 4096 || !c.BodyTruncated {
		t.Fatalf("event body %d bytes, truncated=%v; want 4096 and true", len(c.Body), c.BodyTruncated)
	}
	if ev.Comment.Body != stored {
		t.Fatal("encode mutated the producer's comment body")
	}
}

type decodedComment struct {
	Body          string `json:"body"`
	BodyTruncated bool   `json:"body_truncated"`
	hasTruncated  bool
}

func encodedComment(t *testing.T, ev event.Event) decodedComment {
	t.Helper()
	raw, err := goldenEmitter().encode(ev, goldenEventID, goldenCreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Data struct {
			Comment json.RawMessage `json:"comment"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	var c decodedComment
	if err := json.Unmarshal(body.Data.Comment, &c); err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(body.Data.Comment, &keys); err != nil {
		t.Fatal(err)
	}
	_, c.hasTruncated = keys["body_truncated"]
	return c
}

// TestCommentTruncationBoundaries: the cut never splits a rune and never
// exceeds the cap after encoding, even for invalid UTF-8, and body_truncated
// is omitted when nothing was cut (EV-3's omit rule).
func TestCommentTruncationBoundaries(t *testing.T) {
	a := strings.Repeat
	cases := []struct {
		name    string
		body    string
		wantLen int
		cut     bool
	}{
		{"short", "hello", 5, false},
		{"exactly the cap", a("a", 4096), 4096, false},
		{"one over the cap", a("a", 4097), 4096, true},
		{"two-byte rune straddles the cap", a("a", 4095) + "é" + "tail", 4095, true},
		{"four-byte rune straddles the cap", a("a", 4094) + "👍", 4094, true},
		{"four-byte rune ends at the cap", a("a", 4092) + "👍", 4096, false},
		{"invalid utf-8 at the cap", a("a", 4095) + "\xff\xfe" + "tail", 4095, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := kindEvents()["comment_created"]
			ev.Comment.Body = tc.body
			c := encodedComment(t, ev)
			if len(c.Body) != tc.wantLen || c.BodyTruncated != tc.cut {
				t.Fatalf("body %d bytes, truncated=%v; want %d and %v", len(c.Body), c.BodyTruncated, tc.wantLen, tc.cut)
			}
			if !utf8.ValidString(c.Body) {
				t.Fatal("event body is not valid UTF-8")
			}
			if c.hasTruncated != tc.cut {
				t.Fatalf("body_truncated key present=%v, want %v", c.hasTruncated, tc.cut)
			}
		})
	}
}

// TestReactionApprovalAlwaysPresent: approval_class and approval are the one
// exception to the omit rule, because false is meaningful (EV-3, EV-5).
func TestReactionApprovalAlwaysPresent(t *testing.T) {
	raw, err := goldenEmitter().encode(kindEvents()["reaction_removed"], goldenEventID, goldenCreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"approval_class":false,"approval":false`)) {
		t.Fatalf("false approval bits omitted: %s", raw)
	}
}

// capture is a target that records every request it receives.
type capture struct {
	mu   sync.Mutex
	reqs []captured
	n    atomic.Int32
}

type captured struct {
	hdr  http.Header
	body []byte
}

func (c *capture) handler(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	c.reqs = append(c.reqs, captured{hdr: r.Header.Clone(), body: b})
	c.mu.Unlock()
	c.n.Add(1)
	w.WriteHeader(http.StatusAccepted)
}

// snapshot copies the recorded requests under the lock.
func (c *capture) snapshot() []captured {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]captured(nil), c.reqs...)
}

// TestNoEnvTargetReceivesNewKinds: SPEC-0016 EV-7. Every kind ADR-0022 adds is
// encoded, counted and dropped, never delivered to an env target. The creation
// emitted last is the positive control: it is delivered, so the worker ran
// past every earlier event before the assertion.
func TestNoEnvTargetReceivesNewKinds(t *testing.T) {
	var sink capture
	srv := httptest.NewServer(http.HandlerFunc(sink.handler))
	defer srv.Close()

	e := newEmitter(t, "s3cr3t", srv.URL)
	for _, ev := range kindEvents() {
		e.Emit(ev)
	}
	e.Emit(creationEvent())
	runUntil(t, e, &sink.n, 1)

	reqs := sink.snapshot()
	if len(reqs) != 1 {
		t.Fatalf("env target received %d requests, want only the creation", len(reqs))
	}
	if got := reqs[0].hdr.Get("X-Cairn-Event"); got != string(event.ArtifactCreated) {
		t.Fatalf("env target received %q", got)
	}
	for _, ev := range kindEvents() {
		if got := e.Dropped(ev.Kind); got != 1 {
			t.Errorf("Dropped(%q) = %d, want 1", ev.Kind, got)
		}
	}
	if got := e.Dropped(event.ArtifactCreated); got != 0 {
		t.Errorf("Dropped(artifact.created) = %d, want 0", got)
	}
}

// TestHeaderMatchesKindForEveryKind: SPEC-0016 EV-8 "Header matches kind".
// Each kind is sealed and handed to the real delivery path: X-Cairn-Event
// equals the body's kind, X-Cairn-Event-Id its event_id, and the signature
// verifies over the raw body. Delivery is driven directly because EV-7 keeps
// the new kinds out of the queue until owned subscriptions exist.
func TestHeaderMatchesKindForEveryKind(t *testing.T) {
	const secret = "s3cr3t"
	for name, ev := range allKindEvents() {
		t.Run(name, func(t *testing.T) {
			var sink capture
			srv := httptest.NewServer(http.HandlerFunc(sink.handler))
			defer srv.Close()

			e := newEmitter(t, secret, srv.URL)
			env, err := e.seal(ev)
			if err != nil {
				t.Fatal(err)
			}
			e.deliver(context.Background(), env)
			reqs := sink.snapshot()
			if len(reqs) != 1 {
				t.Fatalf("got %d requests, want 1", len(reqs))
			}
			req := reqs[0]
			var body struct {
				Kind    string `json:"kind"`
				EventID string `json:"event_id"`
			}
			if err := json.Unmarshal(req.body, &body); err != nil {
				t.Fatal(err)
			}
			if got := req.hdr.Get("X-Cairn-Event"); got != body.Kind || got != string(ev.Kind) {
				t.Fatalf("X-Cairn-Event = %q, body kind = %q, want both %q", got, body.Kind, ev.Kind)
			}
			if got := req.hdr.Get("X-Cairn-Event-Id"); got != body.EventID || got == "" {
				t.Fatalf("X-Cairn-Event-Id = %q, body event_id = %q", got, body.EventID)
			}
			mac := hmac.New(sha256.New, []byte(secret))
			mac.Write(req.body)
			if got, want := req.hdr.Get("X-Cairn-Signature"), "sha256="+hex.EncodeToString(mac.Sum(nil)); got != want {
				t.Fatalf("signature does not verify over the raw body: got %q want %q", got, want)
			}
		})
	}
}

// TestQueueFullCountsDroppedCreations: a creation that meets a full queue is
// counted under its kind, so the dropped series covers every undelivered event.
func TestQueueFullCountsDroppedCreations(t *testing.T) {
	e := newEmitter(t, "", "http://127.0.0.1:0/not-running")
	for i := 0; i < queueCap+50; i++ {
		e.Emit(creationEvent())
	}
	if got := e.Dropped(event.ArtifactCreated); got != 50 {
		t.Fatalf("Dropped(artifact.created) = %d, want 50", got)
	}
}

// TestConcurrentEmitCountsExactly: the drop accounting is race-free under
// concurrent producers (SPEC-0016 "Concurrency Safety"; run with -race).
func TestConcurrentEmitCountsExactly(t *testing.T) {
	e := newEmitter(t, "", "http://127.0.0.1:0/not-running")
	const workers, per = 50, 20
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < per; i++ {
				e.Emit(kindEvents()["reaction_added"])
				e.Emit(creationEvent())
			}
		}()
	}
	wg.Wait()
	if got := e.Dropped(event.ReactionAdded); got != workers*per {
		t.Fatalf("Dropped(reaction.added) = %d, want %d", got, workers*per)
	}
	enqueued := uint64(len(e.ch))
	if got := enqueued + e.Dropped(event.ArtifactCreated); got != workers*per {
		t.Fatalf("creations enqueued+dropped = %d, want %d", got, workers*per)
	}
}

// TestNilEmitterEmitIsInert: the cairn#201 guard holds for the new entry
// points too.
func TestNilEmitterEmitIsInert(t *testing.T) {
	var e *Emitter
	e.Emit(creationEvent())
	if got := e.Dropped(event.ArtifactCreated); got != 0 {
		t.Fatalf("nil Dropped = %d", got)
	}
}
