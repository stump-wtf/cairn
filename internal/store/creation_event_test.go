package store

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/event"
)

// Governing: ADR-0022, SPEC-0016 EV-1 "Event Kind Registry", EV-3 "Payload
// Shape", EV-7 "Recipient Selection and Tenancy"

// TestCreationEventMapsToArtifactCreated pins the adapter: every creation
// field lands in the artifact.created event where the encoder reads it, and
// nothing is invented.
func TestCreationEventMapsToArtifactCreated(t *testing.T) {
	exp := time.Date(2026, 9, 18, 8, 0, 0, 0, time.UTC)
	c := CreationEvent{
		PublicID: "7Kq2mZ", ShareType: artifact.TypeBundle, Title: "handoff", WebPath: "/b/7Kq2mZ",
		ActorID: "actor-1", Model: "claude-opus-5", Channel: string(artifact.ChannelMCP),
		ExpiresAt: exp, OnBehalfOf: "claude-code/2.1.0", Tags: []string{"handoff", "lane:m"},
		ActorKind: event.KindAgent, Auth: event.AuthOAuth, OwnerID: "owner-1",
	}
	ev := c.Event()

	if ev.Kind != event.ArtifactCreated {
		t.Fatalf("kind = %q, want artifact.created", ev.Kind)
	}
	s := ev.Subject
	if s.PublicID != "7Kq2mZ" || s.ShareType != artifact.TypeBundle || s.Title != "handoff" ||
		s.WebPath != "/b/7Kq2mZ" || !s.ExpiresAt.Equal(exp) || s.OwnerID != "owner-1" ||
		!slices.Equal(s.Tags, []string{"handoff", "lane:m"}) {
		t.Fatalf("subject = %+v", s)
	}
	want := event.Actor{
		ID: "actor-1", Channel: artifact.ChannelMCP, OnBehalfOf: "claude-code/2.1.0",
		Kind: event.KindAgent, Auth: event.AuthOAuth,
	}
	if ev.Actor != want {
		t.Fatalf("actor = %+v, want %+v", ev.Actor, want)
	}
	if ev.Model != "claude-opus-5" {
		t.Fatalf("model = %q", ev.Model)
	}
	if ev.Comment != nil || ev.Reaction != nil || ev.Run != nil {
		t.Fatalf("creation carries another kind's payload: %+v", ev)
	}
	if err := ev.Actor.Check(); err != nil {
		t.Fatalf("mapped actor fails Check: %v", err)
	}
}

// TestCreationEventCarriesOwner: both create paths hand the emitter the
// artifact's owner, distinct from its actor, so owned subscriptions can be
// selected from the event alone (EV-7).
func TestCreationEventCarriesOwner(t *testing.T) {
	em := &tagCaptureEmitter{}
	s, _ := newTestStore(t, Options{Emitter: em})
	ctx := context.Background()

	in := input([]byte("owned"))
	in.Provenance.ActorID = "actor-1"
	in.Access.OwnerID = "owner-1"
	if _, err := s.CreateArtifact(ctx, in); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.CreateBundle(ctx, CreateBundleInput{
		Members:    []MemberInput{{Name: "a.md", Body: strings.NewReader("x")}},
		Provenance: artifact.Provenance{ActorID: "actor-2", Channel: artifact.ChannelMCP, CapturedAt: time.Now()},
		Access:     artifact.AccessPolicy{OwnerID: "owner-2", Visibility: artifact.VisibilityLink},
		ExpiresAt:  time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("create bundle: %v", err)
	}

	evs := em.snapshot()
	if len(evs) != 2 {
		t.Fatalf("got %d creation events, want 2", len(evs))
	}
	for i, want := range []struct{ actor, owner string }{{"actor-1", "owner-1"}, {"actor-2", "owner-2"}} {
		if evs[i].ActorID != want.actor || evs[i].OwnerID != want.owner {
			t.Errorf("event %d actor=%q owner=%q, want %q and %q", i, evs[i].ActorID, evs[i].OwnerID, want.actor, want.owner)
		}
		if got := evs[i].Event().Subject.OwnerID; got != want.owner {
			t.Errorf("event %d subject owner = %q, want %q", i, got, want.owner)
		}
	}
}
