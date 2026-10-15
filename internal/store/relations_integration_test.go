package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/event"
	"github.com/stump-wtf/cairn/internal/tracectx"
)

// Governing: SPEC-0024 REQ "Relation Targets Must Be Readable by the Creator",
// REQ "Relations on Read", REQ "Relations in artifact.created", REQ "Database
// Operation Standards".

// create seeds one artifact and returns it.
func create(t *testing.T, s *Store, title string, relations ...artifact.Relation) *artifact.Artifact {
	t.Helper()
	in := input([]byte("body of " + title))
	in.Title = title
	in.Relations = relations
	art, err := s.CreateArtifact(context.Background(), in)
	if err != nil {
		t.Fatalf("create %q: %v", title, err)
	}
	return art
}

// captureEmitter records the creation events a store emits post-commit.
type captureEmitter struct {
	events []CreationEvent
}

func (c *captureEmitter) EmitArtifactCreated(ev CreationEvent) {
	c.events = append(c.events, ev)
}

func TestCreateWithRelationsReadsBack(t *testing.T) {
	s, _ := newTestStore(t, Options{})
	ctx := context.Background()

	target := create(t, s, "the one being replied to")
	other := create(t, s, "the one it derives from")
	art := create(t, s, "the reply",
		artifact.Relation{Kind: artifact.RelationReplyTo, ID: target.PublicID},
		artifact.Relation{Kind: artifact.RelationDerivedFrom, ID: other.PublicID},
	)

	got, err := s.Relations(ctx, Reader{UserID: testOwner}, art.PublicID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Relations) != 2 {
		t.Fatalf("got %d relations, want 2: %+v", len(got.Relations), got.Relations)
	}
	// Declaration order is the read order.
	if got.Relations[0].Kind != artifact.RelationReplyTo || got.Relations[0].PublicID != target.PublicID {
		t.Errorf("relations[0] = %+v, want the reply_to target first", got.Relations[0])
	}
	if got.Relations[0].Title != target.Title || !got.Relations[0].Available {
		t.Errorf("relations[0] = %+v, want the target's title and available", got.Relations[0])
	}
	if got.Relations[1].PublicID != other.PublicID || got.Relations[1].Title != other.Title {
		t.Errorf("relations[1] = %+v, want the derived_from target", got.Relations[1])
	}

	// Both ends resolve: each target names the reply.
	for _, target := range []*artifact.Artifact{target, other} {
		back, err := s.Relations(ctx, Reader{UserID: testOwner}, target.PublicID)
		if err != nil {
			t.Fatal(err)
		}
		if len(back.ReferencedBy) != 1 {
			t.Fatalf("referenced_by of %q = %+v, want the reply alone", target.PublicID, back.ReferencedBy)
		}
		if back.ReferencedBy[0].PublicID != art.PublicID || back.ReferencedBy[0].Title != art.Title {
			t.Errorf("referenced_by[0] = %+v, want the replying artifact", back.ReferencedBy[0])
		}
	}
}

// One artifact relating twice to the same target is stored as two edges and
// listed once in referenced_by, in the order it declared them.
func TestReferencedByListsAnArtifactOnce(t *testing.T) {
	s, _ := newTestStore(t, Options{})
	ctx := context.Background()

	target := create(t, s, "target")
	reply := create(t, s, "double",
		artifact.Relation{Kind: artifact.RelationFollows, ID: target.PublicID},
		artifact.Relation{Kind: artifact.RelationReplyTo, ID: target.PublicID},
	)

	got, err := s.Relations(ctx, Reader{UserID: testOwner}, reply.PublicID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Relations) != 2 {
		t.Fatalf("the declaring side lost an edge: %+v", got.Relations)
	}
	back, err := s.Relations(ctx, Reader{UserID: testOwner}, target.PublicID)
	if err != nil {
		t.Fatal(err)
	}
	if len(back.ReferencedBy) != 1 {
		t.Fatalf("referenced_by = %+v, want one entry for the one artifact", back.ReferencedBy)
	}
}

// Duplicate (kind, id) pairs cost nothing: the store keeps one edge, in first-
// occurrence order, exactly as the aggregate does.
func TestCreateDedupsRelations(t *testing.T) {
	s, _ := newTestStore(t, Options{})
	ctx := context.Background()

	target := create(t, s, "target")
	other := create(t, s, "other")
	art := create(t, s, "repeats",
		artifact.Relation{Kind: artifact.RelationReplyTo, ID: target.PublicID},
		artifact.Relation{Kind: artifact.RelationFollows, ID: other.PublicID},
		artifact.Relation{Kind: artifact.RelationReplyTo, ID: target.PublicID},
	)
	got, err := s.Relations(ctx, Reader{UserID: testOwner}, art.PublicID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Relations) != 2 || got.Relations[0].PublicID != target.PublicID {
		t.Fatalf("relations = %+v, want the two declared, deduped in order", got.Relations)
	}
}

// A ninth relation is refused, and nothing is persisted by a refused create.
func TestCreateRejectsMoreThanMaxRelations(t *testing.T) {
	s, pool := newTestStore(t, Options{})
	ctx := context.Background()

	target := create(t, s, "target")
	relations := make([]artifact.Relation, 0, artifact.MaxRelations+2)
	for i := 0; i < artifact.MaxRelations+2; i++ {
		relations = append(relations, artifact.Relation{Kind: artifact.RelationFollows, ID: target.PublicID + "x"})
	}
	in := input([]byte("nine relations"))
	in.Relations = relations
	if _, err := s.CreateArtifact(ctx, in); err == nil {
		t.Fatal("more than the cap was accepted")
	} else if errs.CodeOf(err) != errs.CodeValidation {
		t.Fatalf("code = %s, want validation_failed", errs.CodeOf(err))
	}
	assertArtifactCount(t, pool, 1)
}

// An unknown target, an expired target, and a target whose row is gone all
// answer the same way, with nothing persisted. Which one it was is the answer a
// probe is looking for.
func TestRelationTargetNotFoundIsUniform(t *testing.T) {
	s, pool := newTestStore(t, Options{})
	ctx := context.Background()

	create(t, s, "base")

	expired := create(t, s, "expired target")
	if _, err := pool.Exec(ctx, `UPDATE artifacts SET expires_at = now() - interval '1 second' WHERE id = $1`,
		expired.ID); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		id   string
	}{
		{"unknown", "zzzzzzzz"},
		{"expired", expired.PublicID},
		{"deleted", func() string {
			dead := create(t, s, "deleted target")
			if _, err := pool.Exec(ctx, `DELETE FROM artifacts WHERE id = $1`, dead.ID); err != nil {
				t.Fatal(err)
			}
			return dead.PublicID
		}()},
	}
	for _, tc := range cases {
		_, err := s.CreateArtifact(ctx, withRelation(input([]byte(tc.name)), artifact.RelationReplyTo, tc.id))
		if !errors.Is(err, artifact.ErrRelationTargetNotFound) {
			t.Errorf("%s: err = %v, want relation-target-not-found", tc.name, err)
		}
		if code := errs.CodeOf(err); code != errs.CodeValidation {
			t.Errorf("%s: code = %s, want validation_failed", tc.name, code)
		}
		// The message a probe sees carries no id: nothing distinguishes the cases.
		if msg := err.Error(); strings.Contains(msg, tc.id) {
			t.Errorf("%s: error names the target id, which is the probe's answer: %q", tc.name, msg)
		}
	}
	// The base and the expired target remain; the deleted one's row is gone and
	// none of the refused creates persisted anything.
	assertArtifactCount(t, pool, 2)
}

// A failed relation check leaves no orphan behind: no artifact row, and no
// relations for a target that was never written.
func TestFailedRelationCreatePersistsNothing(t *testing.T) {
	s, pool := newTestStore(t, Options{})
	ctx := context.Background()

	in := withRelation(input([]byte("doomed")), artifact.RelationReplyTo, "nosuchid1")
	if _, err := s.CreateArtifact(ctx, in); err == nil {
		t.Fatal("an unusable target was accepted")
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM artifacts`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d artifacts persisted by a refused create", n)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM artifact_relations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d relations persisted by a refused create", n)
	}
}

// A target the reader cannot open keeps its kind and its place, and gives up
// neither its id nor its title.
func TestRelationsHideUnreadableTargets(t *testing.T) {
	s, pool := newTestStore(t, Options{})
	ctx := context.Background()

	hidden := create(t, s, "a title no reader should learn")
	visible := create(t, s, "visible target")
	art := create(t, s, "declares both",
		artifact.Relation{Kind: artifact.RelationReplyTo, ID: hidden.PublicID},
		artifact.Relation{Kind: artifact.RelationFollows, ID: visible.PublicID},
	)
	if _, err := pool.Exec(ctx, `UPDATE artifacts SET expires_at = now() - interval '1 second' WHERE id = $1`,
		hidden.ID); err != nil {
		t.Fatal(err)
	}

	got, err := s.Relations(ctx, Reader{UserID: testOwner}, art.PublicID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Relations) != 2 {
		t.Fatalf("the hidden target dropped its entry: %+v", got.Relations)
	}
	first := got.Relations[0]
	if first.Kind != artifact.RelationReplyTo || first.Available || first.PublicID != "" || first.Title != "" {
		t.Errorf("hidden relation = %+v, want kind only with available false", first)
	}
	if got.Relations[1].PublicID != visible.PublicID || !got.Relations[1].Available {
		t.Errorf("visible relation = %+v", got.Relations[1])
	}
}

// referenced_by is bounded, and says when it was.
func TestReferencedByCapAndFlag(t *testing.T) {
	s, _ := newTestStore(t, Options{})
	ctx := context.Background()

	target := create(t, s, "everyone points here")
	for i := 0; i < MaxReferencedBy+1; i++ {
		create(t, s, "referrer", artifact.Relation{Kind: artifact.RelationReplyTo, ID: target.PublicID})
	}
	got, err := s.Relations(ctx, Reader{UserID: testOwner}, target.PublicID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.ReferencedBy) != MaxReferencedBy {
		t.Fatalf("got %d referrers, want the cap %d", len(got.ReferencedBy), MaxReferencedBy)
	}
	if !got.ReferencedByTruncated {
		t.Error("truncated flag not set with more referrers than the cap")
	}

	under := create(t, s, "one referrer", artifact.Relation{Kind: artifact.RelationReplyTo, ID: target.PublicID})
	_ = under
}

// A bundle commits its relations in the same transaction as its members.
func TestCreateBundleWithRelations(t *testing.T) {
	s, _ := newTestStore(t, Options{})
	ctx := context.Background()

	target := create(t, s, "target")
	in := CreateBundleInput{
		Title:      "bundle",
		Members:    []MemberInput{{Name: "a.txt", Body: strings.NewReader("one")}},
		Provenance: artifact.Provenance{CreatedByUserID: testOwner, ActorID: "u1", Channel: artifact.ChannelCLI, CapturedAt: time.Now()},
		Access:     artifact.AccessPolicy{OwnerUserID: testOwner, Visibility: artifact.VisibilityLink},
		ExpiresAt:  time.Now().Add(time.Hour),
		Relations:  []artifact.Relation{{Kind: artifact.RelationDerivedFrom, ID: target.PublicID}},
	}
	art, err := s.CreateBundle(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Relations(ctx, Reader{UserID: testOwner}, art.PublicID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Relations) != 1 || got.Relations[0].Kind != artifact.RelationDerivedFrom {
		t.Fatalf("bundle relations = %+v", got.Relations)
	}
}

// artifact.created carries the relations inside the artifact's own workspace and
// never one outside it. Until teams exist, workspace is the owning user.
func TestEventCarriesSameWorkspaceRelationsOnly(t *testing.T) {
	em := &captureEmitter{}
	s, _ := newTestStore(t, Options{Emitter: em})
	ctx := context.Background()

	// Owned by the other seeded user, created by this one: readable, but not the
	// workspace the event goes to.
	in := input([]byte("elsewhere"))
	in.Title = "elsewhere"
	in.Access = artifact.AccessPolicy{OwnerUserID: testIntruder, Visibility: artifact.VisibilityLink}
	elsewhere, err := s.CreateArtifact(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	own := create(t, s, "own workspace")
	created := create(t, s, "the event",
		artifact.Relation{Kind: artifact.RelationReplyTo, ID: own.PublicID},
		artifact.Relation{Kind: artifact.RelationFollows, ID: elsewhere.PublicID},
	)
	if len(em.events) != 3 {
		t.Fatalf("emitter saw %d events, want 3", len(em.events))
	}
	last := em.events[len(em.events)-1]
	if len(last.Relations) != 1 {
		t.Fatalf("event relations = %+v, want only the in-workspace one", last.Relations)
	}
	if last.Relations[0] != (event.Relation{Kind: "reply_to", ID: own.PublicID}) {
		t.Errorf("event relation = %+v", last.Relations[0])
	}
	// The edge to the other workspace is real and stored; it is only the event
	// that may not carry it.
	got, err := s.Relations(ctx, Reader{UserID: testOwner}, created.PublicID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Relations) != 2 {
		t.Fatalf("stored relations = %+v, want both edges", got.Relations)
	}
}

// A create that carried a traceparent reads back the trace; one that did not
// reads back none.
func TestTraceContextRoundTrip(t *testing.T) {
	s, _ := newTestStore(t, Options{})
	ctx := context.Background()

	traced, ok := tracectx.Parse("00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	if !ok {
		t.Fatal("fixture traceparent invalid")
	}
	in := input([]byte("traced"))
	in.Trace = &traced
	art, err := s.CreateArtifact(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetByPublicID(ctx, art.PublicID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Trace == nil || got.Trace.TraceHex() != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("trace = %+v, want the parsed one back", got.Trace)
	}

	plain, err := s.GetByPublicID(context.Background(), create(t, s, "untraced").PublicID)
	if err != nil {
		t.Fatal(err)
	}
	if plain.Trace != nil {
		t.Errorf("an untraced create read back a trace: %+v", plain.Trace)
	}
}

// The emitter's trace_id is the same 32 hex characters a read returns.
func TestEventCarriesTraceID(t *testing.T) {
	em := &captureEmitter{}
	s, _ := newTestStore(t, Options{Emitter: em})

	traced, _ := tracectx.Parse("00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	in := input([]byte("traced"))
	in.Trace = &traced
	if _, err := s.CreateArtifact(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if got := em.events[len(em.events)-1].TraceID; got != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("event trace_id = %q", got)
	}
}

func withRelation(in CreateArtifactInput, kind artifact.RelationKind, id string) CreateArtifactInput {
	in.Relations = []artifact.Relation{{Kind: kind, ID: id}}
	return in
}

func assertArtifactCount(t *testing.T, pool *pgxpool.Pool, want int) {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM artifacts").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != want {
		t.Fatalf("%d artifacts, want %d", n, want)
	}
}
