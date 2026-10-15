package outboundhook

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stump-wtf/cairn/internal/event"
)

// Governing: SPEC-0024 REQ "Relations in artifact.created", REQ "Trace Id on
// Read and in Events"; SPEC-0016 EV-3 "Payload Shape".

// relatedTracedEvent is an artifact.created for an artifact that declares two
// relations and carries a trace. Both are appended keys: an artifact with
// neither must produce the pre-SPEC-0024 bytes exactly.
func relatedTracedEvent() event.Event {
	subject := kindSubject()
	subject.Relations = []event.Relation{
		{Kind: "reply_to", ID: "9qz1aB2c"},
		{Kind: "derived_from", ID: "7Yh3nQp4"},
	}
	subject.TraceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	return event.Event{Kind: event.ArtifactCreated, Subject: subject, Actor: agentActor(), Model: "claude-opus-5"}
}

func TestGoldenPayloadArtifactCreatedRelatedAndTraced(t *testing.T) {
	raw, err := goldenEmitter().encode(relatedTracedEvent(), goldenEventID, goldenCreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	assertGolden(t, "artifact_created_related_traced", raw)
}

// An unrelated, untraced create gains neither key: SPEC-0024 requires the
// existing golden payload to stay byte-identical, and a consumer that walks the
// keys must not see an empty relations list on every event.
func TestArtifactCreatedWithoutRelationsGainsNoKeys(t *testing.T) {
	for _, ev := range []event.Event{creationEvent(), {Kind: event.ArtifactRetained, Subject: kindSubject(), Actor: humanActor()}} {
		raw, err := goldenEmitter().encode(ev, goldenEventID, goldenCreatedAt)
		if err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{`"relations"`, `"trace_id"`} {
			if bytes.Contains(raw, []byte(key)) {
				t.Errorf("%s appears in a payload for an artifact with neither: %s", key, raw)
			}
		}
	}
}

// The encoder never invents a title, a URL or an owner for a target: relations
// are kind and id only, so an event cannot disclose a target's content to a
// consumer that could not open it. The owner marker stays off the wire as it
// does for every kind (EV-7).
func TestEncodedRelationsCarryOnlyKindAndID(t *testing.T) {
	raw, err := goldenEmitter().encode(relatedTracedEvent(), goldenEventID, goldenCreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"relations":[{"kind":"reply_to","id":"9qz1aB2c"`) {
		t.Fatalf("relations are not the two-key shape SPEC-0024 defines: %s", raw)
	}
	if bytes.Contains(raw, []byte(ownerMarker)) || bytes.Contains(raw, []byte(userMarker)) {
		t.Errorf("internal ids reached the wire: %s", raw)
	}
}

// An empty relations list is not the same as none: a producer that hands over
// nil must not serialize an empty array, because that would key-differ from the
// payload an unrelated artifact produced before this existed.
func TestEmptyRelationsEncodeAsAbsent(t *testing.T) {
	subject := kindSubject()
	subject.Relations = []event.Relation{}
	raw, err := goldenEmitter().encode(event.Event{
		Kind: event.ArtifactCreated, Subject: subject, Actor: humanActor(),
	}, goldenEventID, goldenCreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("relations")) {
		t.Fatalf("an empty relation list was serialized: %s", raw)
	}
}
