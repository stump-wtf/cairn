package store

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/errs"
)

// Governing: ADR-0018 (Client-Asserted Artifact Tags), SPEC-0002 REQ "Artifact
// Tags", SPEC-0012 REQ "Event Payload"

// tagCaptureEmitter records every creation event the store hands it.
type tagCaptureEmitter struct {
	mu     sync.Mutex
	events []CreationEvent
}

func (c *tagCaptureEmitter) EmitArtifactCreated(ev CreationEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, ev)
}

func (c *tagCaptureEmitter) snapshot() []CreationEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]CreationEvent(nil), c.events...)
}

// TestTagsPersistReadListFilterAndEmit covers the store half of tags end to
// end: normalized and persisted by both create paths, returned by read and
// list, filterable in the Bin, and carried with on_behalf_of on the creation
// event for single-body and bundle alike.
func TestTagsPersistReadListFilterAndEmit(t *testing.T) {
	em := &tagCaptureEmitter{}
	s, _ := newTestStore(t, Options{Emitter: em})
	ctx := context.Background()

	want := []string{"handoff", "lane:m", "issue:stump.wtf/cairn#42"}
	in := input([]byte("do the thing"))
	in.Provenance.OnBehalfOf = "claude-code/2.1.0"
	in.Tags = []string{"handoff", "lane:m", "issue:stump.wtf/cairn#42", "handoff"}
	art, err := s.CreateArtifact(ctx, in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !slices.Equal(art.Tags, want) {
		t.Fatalf("created tags = %q, want deduplicated %q", art.Tags, want)
	}
	// The event must not share a slice with the returned artifact.
	art.Tags[1] = "mutated-after-create"

	got, err := s.GetByPublicID(ctx, art.PublicID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !slices.Equal(got.Tags, want) {
		t.Fatalf("read tags = %q, want %q", got.Tags, want)
	}

	plain, err := s.CreateArtifact(ctx, input([]byte("no tags")))
	if err != nil {
		t.Fatalf("create untagged: %v", err)
	}
	gotPlain, err := s.GetByPublicID(ctx, plain.PublicID)
	if err != nil {
		t.Fatalf("read untagged: %v", err)
	}
	if len(gotPlain.Tags) != 0 {
		t.Fatalf("untagged artifact read back tags %q", gotPlain.Tags)
	}

	bundle, err := s.CreateBundle(ctx, CreateBundleInput{
		Title: "handoff bundle",
		Members: []MemberInput{
			{Name: "prompt.md", Body: strings.NewReader("# task")},
			{Name: "context.log", Body: strings.NewReader("log")},
		},
		Provenance: artifact.Provenance{ActorID: "u1", OnBehalfOf: "crush/0.9.0", Channel: artifact.ChannelMCP, CapturedAt: time.Now()},
		Access:     artifact.AccessPolicy{OwnerID: "u1", Visibility: artifact.VisibilityLink},
		ExpiresAt:  time.Now().Add(time.Hour),
		Tags:       []string{"handoff", "size:s"},
	})
	if err != nil {
		t.Fatalf("create bundle: %v", err)
	}

	bin := func(tags ...string) []string {
		t.Helper()
		page, err := s.ListBin(ctx, "u1", "", 10, tags...)
		if err != nil {
			t.Fatalf("list %q: %v", tags, err)
		}
		ids := make([]string, 0, len(page.Artifacts))
		for _, a := range page.Artifacts {
			ids = append(ids, a.PublicID)
			if a.PublicID == art.PublicID && !slices.Equal(a.Tags, want) {
				t.Errorf("bin tags for %s = %q, want %q", a.PublicID, a.Tags, want)
			}
		}
		slices.Sort(ids)
		return ids
	}
	sorted := func(ids ...string) []string { slices.Sort(ids); return ids }
	if got, want := bin(), sorted(art.PublicID, plain.PublicID, bundle.PublicID); !slices.Equal(got, want) {
		t.Fatalf("unfiltered bin = %q, want %q", got, want)
	}
	if got, want := bin("handoff"), sorted(art.PublicID, bundle.PublicID); !slices.Equal(got, want) {
		t.Fatalf("bin tag=handoff = %q, want %q", got, want)
	}
	if got, want := bin("handoff", "size:s"), sorted(bundle.PublicID); !slices.Equal(got, want) {
		t.Fatalf("bin tag=handoff&tag=size:s (AND) = %q, want %q", got, want)
	}
	if got := bin("nope"); len(got) != 0 {
		t.Fatalf("bin tag=nope = %q, want none", got)
	}
	if _, err := s.ListBin(ctx, "u1", "", 10, "Bad"); errs.CodeOf(err) != errs.CodeValidation {
		t.Fatalf("bin with a malformed tag filter = %v, want a validation error", err)
	}

	evs := em.snapshot()
	if len(evs) != 3 {
		t.Fatalf("got %d creation events, want 3", len(evs))
	}
	if !slices.Equal(evs[0].Tags, want) || evs[0].OnBehalfOf != "claude-code/2.1.0" {
		t.Fatalf("single-body event = tags %q obo %q", evs[0].Tags, evs[0].OnBehalfOf)
	}
	if len(evs[1].Tags) != 0 || evs[1].OnBehalfOf != "" {
		t.Fatalf("untagged event = tags %q obo %q, want neither", evs[1].Tags, evs[1].OnBehalfOf)
	}
	if evs[2].ShareType != artifact.TypeBundle || !slices.Equal(evs[2].Tags, []string{"handoff", "size:s"}) || evs[2].OnBehalfOf != "crush/0.9.0" {
		t.Fatalf("bundle event = %+v", evs[2])
	}
}

// TestInvalidTagsPersistAndEmitNothing: a rejected tag list fails the create
// before anything streams or commits, so there is no row and no doorbell.
func TestInvalidTagsPersistAndEmitNothing(t *testing.T) {
	em := &tagCaptureEmitter{}
	s, _ := newTestStore(t, Options{Emitter: em})
	ctx := context.Background()

	tooMany := make([]string, artifact.MaxTags+1)
	for i := range tooMany {
		tooMany[i] = "t" + strings.Repeat("x", i)
	}
	for _, tags := range [][]string{{"Lane:m"}, tooMany} {
		in := input([]byte("body"))
		in.Tags = tags
		if _, err := s.CreateArtifact(ctx, in); errs.CodeOf(err) != errs.CodeValidation {
			t.Fatalf("create with tags %q = %v, want validation error", tags, err)
		}
	}
	_, err := s.CreateBundle(ctx, CreateBundleInput{
		Members:    []MemberInput{{Name: "a.md", Body: strings.NewReader("x")}},
		Provenance: artifact.Provenance{ActorID: "u1", Channel: artifact.ChannelMCP, CapturedAt: time.Now()},
		Access:     artifact.AccessPolicy{OwnerID: "u1", Visibility: artifact.VisibilityLink},
		ExpiresAt:  time.Now().Add(time.Hour),
		Tags:       []string{"lane auto"},
	})
	if errs.CodeOf(err) != errs.CodeValidation {
		t.Fatalf("bundle with a bad tag = %v, want validation error", err)
	}

	page, err := s.ListBin(ctx, "u1", "", 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page.Artifacts) != 0 {
		t.Fatalf("rejected creates persisted %d artifacts", len(page.Artifacts))
	}
	if n := len(em.snapshot()); n != 0 {
		t.Fatalf("rejected creates emitted %d events", n)
	}
}
