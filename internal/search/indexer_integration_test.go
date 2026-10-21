package search_test

// Integration tests for the lexical index foundation (SPEC-0022 REQ-2, REQ-10,
// REQ-11 lexical part, "Concurrency Safety", "Database Operation Standards").
// They run against the Postgres in CAIRN_TEST_DATABASE_URL and skip without it,
// like every other DB package here: the whole point is that tokenization, the
// weighted tsvector, the cascade and FOR UPDATE SKIP LOCKED are the database's,
// so a fake cannot prove them.
//
// Each test gets a private schema, migrated to head, and creates its artifacts
// through the real store so the enqueue it asserts on is the one a create
// actually performs.
//
// Governing: ADR-0028, SPEC-0022 REQ-2, REQ-8, REQ-10, REQ-11.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stump-wtf/cairn/internal/annotation"
	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/db"
	"github.com/stump-wtf/cairn/internal/event"
	"github.com/stump-wtf/cairn/internal/objectstore"
	"github.com/stump-wtf/cairn/internal/redact"
	"github.com/stump-wtf/cairn/internal/search"
	"github.com/stump-wtf/cairn/internal/sharetype"
	"github.com/stump-wtf/cairn/internal/store"
	"github.com/stump-wtf/cairn/internal/webhook"
)

const ownerID = "00000000-0000-4000-8000-000000000001"

// env is one migrated private schema with the real write paths wired to one
// indexer.
type env struct {
	pool    *pgxpool.Pool
	obj     *objectstore.Memory
	st      *store.Store
	ix      *search.Indexer
	ann     *annotation.Service
	cfg     search.Config
	scanner *redact.Scanner
}

func newEnv(t *testing.T, cfg search.Config) *env {
	t.Helper()
	dsn := os.Getenv("CAIRN_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set CAIRN_TEST_DATABASE_URL to run search integration tests")
	}
	ctx := context.Background()

	schema := fmt.Sprintf("search_test_%d", time.Now().UnixNano())
	admin, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := admin.Exec(ctx, fmt.Sprintf(`CREATE SCHEMA %q`, schema)); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), fmt.Sprintf(`DROP SCHEMA %q CASCADE`, schema))
		admin.Close()
	})

	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	// search_path is a connection parameter, not a one-off SET: the pool opens
	// connections lazily, and a worker's later connection must land in this
	// schema too.
	poolCfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, actor_key, display_handle) VALUES ($1, 'u1', 'u1')`, ownerID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(pool.Close)

	scanner, err := redact.New(redact.Config{})
	if err != nil {
		t.Fatalf("scanner: %v", err)
	}
	obj := objectstore.NewMemory()

	cfg.Logger = discardLogger()
	if cfg.MaxBodyBytes == 0 {
		cfg.MaxBodyBytes = 256 << 10
	}
	if cfg.Workers == 0 {
		cfg.Workers = 2
	}
	ix := search.New(pool, obj, cfg)

	st := store.New(pool, obj, store.Options{Scanner: scanner, Index: ix, Logger: discardLogger()})
	ann := annotation.NewService(pool, sharetype.Default(),
		annotation.WithRedaction(scanner, nil),
		annotation.WithIndexEnqueuer(ix))
	return &env{pool: pool, obj: obj, st: st, ix: ix, ann: ann, cfg: cfg, scanner: scanner}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func humanActor() event.Actor {
	return event.Actor{ID: "u1", UserID: ownerID, Channel: artifact.ChannelCLI, Kind: event.KindHuman, Auth: event.AuthSession}
}

func provenance() artifact.Provenance {
	return artifact.Provenance{CreatedByUserID: ownerID, ActorID: "u1", Channel: artifact.ChannelCLI, CapturedAt: time.Now()}
}

func access() artifact.AccessPolicy {
	return artifact.AccessPolicy{OwnerUserID: ownerID, Visibility: artifact.VisibilityLink}
}

// createMarkdown creates a live markdown artifact through the store, which is
// what enqueues it: the tests never write search_queue themselves for a create.
func (e *env) createMarkdown(t *testing.T, title, body string, tags ...string) *artifact.Artifact {
	t.Helper()
	art, err := e.st.CreateArtifact(context.Background(), store.CreateArtifactInput{
		ShareType:         sharetype.KeyMarkdown,
		Title:             title,
		Body:              bytes.NewReader([]byte(body)),
		DeclaredMediaType: "text/markdown",
		Tags:              tags,
		Provenance:        provenance(),
		Access:            access(),
		ExpiresAt:         time.Now().Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("create markdown: %v", err)
	}
	return art
}

func (e *env) drain(t *testing.T) search.DrainStats {
	t.Helper()
	stats, err := e.ix.Drain(context.Background(), 0)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	return stats
}

// find returns the public ids whose document matches a plain query, the way the
// next story's search will. artifacts is the authoritative expiry: search_
// documents.expires_at is only a copy the TTL enqueue refreshes, so the join
// filter is what makes an expired artifact invisible the instant it expires
// (SPEC-0022 REQ-10), before the reaper has touched anything.
func (e *env) find(t *testing.T, query string) []string {
	t.Helper()
	rows, err := e.pool.Query(context.Background(), `
		SELECT a.public_id
		  FROM search_documents d JOIN artifacts a ON a.id = d.artifact_id
		 WHERE d.doc @@ websearch_to_tsquery($1, $2)
		   AND a.expires_at > now()
		   AND d.expires_at > now()
		 ORDER BY a.public_id`, e.language(), query)
	if err != nil {
		t.Fatalf("find %q: %v", query, err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan find: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("find rows: %v", err)
	}
	return ids
}

func (e *env) language() string {
	if e.cfg.Language == "" {
		return "english"
	}
	return e.cfg.Language
}

func (e *env) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

func (e *env) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(), query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

func (e *env) comment(t *testing.T, publicID, body string) annotation.Comment {
	t.Helper()
	c, err := e.ann.AddComment(context.Background(), publicID, annotation.CommentInput{
		AnchorType: sharetype.AnchorArtifact,
		Actor:      humanActor(),
		Body:       body,
	})
	if err != nil {
		t.Fatalf("add comment: %v", err)
	}
	return c
}

// REQ-2, scenario "Markdown body is searchable": a markdown artifact containing
// the term is findable once the indexer has run, and is not findable before it
// has — which is the same fact as "the create did not wait for the index".
func TestMarkdownBodyBecomesSearchable(t *testing.T) {
	e := newEnv(t, search.Config{})
	art := e.createMarkdown(t, "Slack thread replies", "The parent event carries thread_ts only on the parent.")

	if got := len(e.find(t, "thread_ts")); got != 0 {
		t.Fatalf("found %d before the indexer ran, want 0: the create must not index inline", got)
	}
	if n := e.count(t, `SELECT count(*) FROM search_queue`); n != 1 {
		t.Fatalf("queue holds %d rows after one create, want 1", n)
	}

	stats := e.drain(t)
	if stats.Indexed != 1 || stats.Failed != 0 {
		t.Fatalf("drain = %+v, want 1 indexed", stats)
	}
	got := e.find(t, "thread_ts")
	if len(got) != 1 || got[0] != art.PublicID {
		t.Fatalf("find thread_ts = %v, want the created artifact", got)
	}
	if n := e.count(t, `SELECT count(*) FROM search_queue`); n != 0 {
		t.Fatalf("queue holds %d rows after a drain, want 0", n)
	}
}

// REQ-2: the title and tags outrank-and-stand-alone, so an artifact with no
// useful body is still found by its tags.
func TestTitleAndTagsAreIndexed(t *testing.T) {
	e := newEnv(t, search.Config{})
	e.createMarkdown(t, "Quarterly receipt", "nothing notable here", "receipt", "repo:example/app")
	e.drain(t)

	for _, term := range []string{"receipt", "quarterly", "repo:example/app"} {
		if got := len(e.find(t, term)); got != 1 {
			t.Errorf("find %q = %d artifacts, want 1", term, got)
		}
	}
}

// REQ-2, scenario "Webhook captures are not indexed": the captured request is
// never read, so a term that exists only inside a capture matches nothing.
func TestWebhookCaptureIsNotIndexed(t *testing.T) {
	e := newEnv(t, search.Config{})
	svc := webhook.NewService(e.pool, e.obj, webhook.Options{Scanner: e.scanner, Logger: discardLogger()})
	ep, err := svc.CreateEndpoint(context.Background(), webhook.EndpointInput{
		Title:      "Billing inbox",
		Provenance: provenance(),
		Access:     access(),
		ExpiresAt:  time.Now().Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("create webhook endpoint: %v", err)
	}
	if _, err := svc.Capture(context.Background(), ep.PublicID, webhook.CaptureInput{
		Method: "POST", Path: "/", ContentType: "application/json",
		Status: 200,
		Body:   []byte(`{"note":"invoice number 4471"}`),
	}); err != nil {
		t.Fatalf("capture: %v", err)
	}

	// Index it explicitly: a webhook artifact is enqueued by nothing today, and
	// the skip list must hold even when something asks for the document.
	if err := e.ix.EnqueueArtifact(context.Background(), endpointID(t, e.pool, ep.PublicID), search.ReasonCreated); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	e.drain(t)

	if got := len(e.find(t, "invoice")); got != 0 {
		t.Fatalf("find invoice = %d, want 0: a captured request is never indexed", got)
	}
	if n := e.count(t, `SELECT count(*) FROM search_documents d JOIN artifacts a ON a.id = d.artifact_id
	                      WHERE a.share_type = 'webhook' AND d.content LIKE '%invoice%'`); n != 0 {
		t.Fatalf("%d webhook documents contain the capture body", n)
	}
}

func endpointID(t *testing.T, pool *pgxpool.Pool, publicID string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `SELECT id FROM artifacts WHERE public_id = $1`, publicID).Scan(&id); err != nil {
		t.Fatalf("endpoint artifact id: %v", err)
	}
	return id
}

// REQ-2, scenario "Create unaffected by the indexer": with no worker running, a
// create still succeeds and the artifact is indexed on resume.
func TestCreateSucceedsWhileIndexerIsStopped(t *testing.T) {
	e := newEnv(t, search.Config{})
	art := e.createMarkdown(t, "Written while the indexer is down", "deferred indexing works")

	if n := e.count(t, `SELECT count(*) FROM search_documents`); n != 0 {
		t.Fatalf("%d documents while the indexer is stopped, want 0", n)
	}
	if n := e.count(t, `SELECT count(*) FROM search_queue`); n != 1 {
		t.Fatalf("queue = %d, want the create to have queued its artifact", n)
	}

	e.drain(t)
	if hits := e.find(t, "deferred"); len(hits) != 1 || hits[0] != art.PublicID {
		t.Fatalf("after resume find deferred = %v, want the artifact", hits)
	}
}

// REQ-2: comments are indexed, an edit re-indexes, and a soft-delete takes the
// text back out. All three are comment-path enqueues, so the test asserts on the
// queue as well as the document.
func TestCommentsAreIndexedLexically(t *testing.T) {
	e := newEnv(t, search.Config{})
	art := e.createMarkdown(t, "Design record", "the body says nothing about schedulers")

	c := e.comment(t, art.PublicID, "we run this on kubernetes, not nomad")
	e.drain(t)
	if got := len(e.find(t, "kubernetes")); got != 1 {
		t.Fatalf("find kubernetes = %v after a comment, want the artifact", got)
	}
	if n := e.count(t, `SELECT count(*) FROM search_queue`); n != 0 {
		t.Fatalf("queue = %d after drain, want 0", n)
	}

	if err := e.ann.EditComment(context.Background(), art.PublicID, c.ID, humanActor(), "actually we run it on nomad"); err != nil {
		t.Fatalf("edit comment: %v", err)
	}
	e.drain(t)
	if got := len(e.find(t, "kubernetes")); got != 0 {
		t.Fatalf("find kubernetes = %v after the edit, want nothing", got)
	}
	if got := len(e.find(t, "nomad")); got != 1 {
		t.Fatalf("find nomad = %v after the edit, want the artifact", got)
	}

	if err := e.ann.DeleteComment(context.Background(), art.PublicID, c.ID, humanActor()); err != nil {
		t.Fatalf("delete comment: %v", err)
	}
	e.drain(t)
	if got := len(e.find(t, "nomad")); got != 0 {
		t.Fatalf("find nomad = %v after a soft delete, want nothing: a tombstone is not live text", got)
	}
}

// Two comments in a row cost one index pass: the queue holds one row per
// artifact, so a busy thread cannot multiply work.
func TestEnqueuesFoldPerArtifact(t *testing.T) {
	e := newEnv(t, search.Config{})
	art := e.createMarkdown(t, "Busy thread", "body")
	e.comment(t, art.PublicID, "first")
	e.comment(t, art.PublicID, "second")

	if n := e.count(t, `SELECT count(*) FROM search_queue`); n != 1 {
		t.Fatalf("queue = %d after three writes to one artifact, want 1", n)
	}
	e.drain(t)
	var content string
	if err := e.pool.QueryRow(context.Background(),
		`SELECT content FROM search_documents d JOIN artifacts a ON a.id = d.artifact_id WHERE a.public_id = $1`,
		art.PublicID).Scan(&content); err != nil {
		t.Fatalf("read content: %v", err)
	}
	if !strings.Contains(content, "first") || !strings.Contains(content, "second") {
		t.Fatalf("content %q holds neither or both comments", content)
	}
}

// REQ-2 skip list: an image body contributes nothing, while its title and tags
// still index.
func TestImageBodyIsSkipped(t *testing.T) {
	e := newEnv(t, search.Config{})
	art, err := e.st.CreateArtifact(context.Background(), store.CreateArtifactInput{
		ShareType:         sharetype.KeyImage,
		Title:             "Chart",
		Body:              bytes.NewReader([]byte("PNGCONTENTS pngbytes keywordz")),
		DeclaredMediaType: "image/png",
		Tags:              []string{"chart"},
		Provenance:        provenance(),
		Access:            access(),
		ExpiresAt:         time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("create image: %v", err)
	}
	e.drain(t)

	if got := len(e.find(t, "pngbytes")); got != 0 {
		t.Fatalf("find pngbytes = %v, want nothing: an image body is never indexed", got)
	}
	if hits := e.find(t, "chart"); len(hits) != 1 || hits[0] != art.PublicID {
		t.Fatalf("find chart = %v, want the image by its tag", hits)
	}
}

// REQ-2's cap: text past CAIRN_SEARCH_MAX_BODY_BYTES is not indexed, so the
// index copy of a body stays bounded.
func TestBodyIsCapped(t *testing.T) {
	e := newEnv(t, search.Config{MaxBodyBytes: 64})
	e.createMarkdown(t, "Long note", strings.Repeat("filler ", 50)+"strawberry")
	e.drain(t)

	if got := len(e.find(t, "strawberry")); got != 0 {
		t.Fatalf("find strawberry = %v, want nothing: it sits past the 64-byte cap", got)
	}
	if got := len(e.find(t, "filler")); got != 1 {
		t.Fatalf("find filler = %v, want the artifact", got)
	}
}

// REQ-2: a bundle indexes its text members and ignores its binary ones.
func TestBundleIndexesTextMembers(t *testing.T) {
	e := newEnv(t, search.Config{})
	art, err := e.st.CreateBundle(context.Background(), store.CreateBundleInput{
		Title: "Research bundle",
		Members: []store.MemberInput{
			{Name: "notes.md", Body: strings.NewReader("the notes mention postgres"), DeclaredMediaType: "text/markdown"},
			{Name: "diagram.png", Body: bytes.NewReader([]byte("PNGDATA hiddenmemberbytes")), DeclaredMediaType: "image/png"},
		},
		Provenance: provenance(),
		Access:     access(),
		ExpiresAt:  time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("create bundle: %v", err)
	}
	e.drain(t)

	if hits := e.find(t, "postgres"); len(hits) != 1 || hits[0] != art.PublicID {
		t.Fatalf("find postgres = %v, want the bundle", hits)
	}
	if got := len(e.find(t, "hiddenmemberbytes")); got != 0 {
		t.Fatalf("find hiddenmemberbytes = %v, want nothing: the image member is skipped", got)
	}
}

// REQ-10: the reaper's delete removes the document in the same transaction, by
// foreign key — no cleanup job, no window where a document outlives its
// artifact.
func TestReaperRemovesTheDocument(t *testing.T) {
	e := newEnv(t, search.Config{})
	art := e.createMarkdown(t, "Expiring soon", "retention sweep finds this text")
	e.drain(t)
	if n := e.count(t, `SELECT count(*) FROM search_documents`); n != 1 {
		t.Fatalf("documents = %d, want 1 indexed", n)
	}

	e.exec(t, `UPDATE artifacts SET expires_at = now() - interval '1 second' WHERE public_id = $1`, art.PublicID)
	// The artifact is invisible to search the instant it expires, before the
	// reaper has touched anything (REQ-10 scenario "Expired but not yet reaped").
	if got := len(e.find(t, "retention")); got != 0 {
		t.Fatalf("find retention = %v after expiry but before the sweep, want nothing", got)
	}

	if _, err := e.st.ReapOnce(context.Background(), store.ReaperConfig{Batch: 10}); err != nil {
		t.Fatalf("reap: %v", err)
	}
	if n := e.count(t, `SELECT count(*) FROM search_documents`); n != 0 {
		t.Fatalf("%d documents remain after the reaper, want 0 (REQ-10)", n)
	}
	if n := e.count(t, `SELECT count(*) FROM search_queue`); n != 0 {
		t.Fatalf("%d queue rows remain after the reaper, want 0", n)
	}
}

// The metadata path is an owner's TTL change. The document carries its own copy
// of expires_at so a query can filter without joining artifacts, so shortening
// the TTL has to move that copy too (SPEC-0022 REQ-10).
func TestTTLChangeRefreshesTheDocument(t *testing.T) {
	e := newEnv(t, search.Config{})
	art := e.createMarkdown(t, "Shorter lease", "text that stays searchable meanwhile")
	e.drain(t)

	expires := func() time.Time {
		var at time.Time
		err := e.pool.QueryRow(context.Background(), `
			SELECT d.expires_at
			  FROM search_documents d JOIN artifacts a ON a.id = d.artifact_id
			 WHERE a.public_id = $1`, art.PublicID).Scan(&at)
		if err != nil {
			t.Fatalf("read document expiry: %v", err)
		}
		return at
	}
	before := expires()

	if _, err := e.st.UpdateTTL(context.Background(), art.PublicID, ownerID, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("update ttl: %v", err)
	}
	e.drain(t)

	if after := expires(); !after.Before(before) {
		t.Fatalf("document expiry = %s, want it moved earlier than %s", after, before)
	}
	if got := len(e.find(t, "searchable")); got != 1 {
		t.Fatalf("find searchable = %v after a TTL change, want the artifact", got)
	}
}

// An owner delete removes the document the same way, through the same key.
func TestOwnerDeleteRemovesTheDocument(t *testing.T) {
	e := newEnv(t, search.Config{})
	art := e.createMarkdown(t, "Deleted by hand", "delete me and the index follows")
	e.drain(t)

	if err := e.st.DeleteArtifact(context.Background(), art.PublicID, ownerID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n := e.count(t, `SELECT count(*) FROM search_documents`); n != 0 {
		t.Fatalf("%d documents after an owner delete, want 0", n)
	}
}

// REQ-10 / "Concurrency Safety": a delete that lands while a document is being
// built must be a no-op. The artifact is removed after its work was claimed, so
// the write has nothing to insert and must not resurrect a row.
func TestDeleteDuringIndexIsNoOp(t *testing.T) {
	e := newEnv(t, search.Config{})
	art := e.createMarkdown(t, "Racing a delete", "indexed while it still exists")
	e.drain(t)
	if got := len(e.find(t, "resurrect")); got != 0 {
		t.Fatalf("find resurrect = %v, want nothing", got)
	}

	artID := endpointID(t, e.pool, art.PublicID)
	if err := e.st.DeleteArtifact(context.Background(), art.PublicID, ownerID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// Index by id now: the artifact row is gone, so this must not write, fail, or
	// leave a queue row behind.
	err := e.ix.IndexOne(context.Background(), artID)
	if err == nil {
		t.Fatal("indexing a deleted artifact reported success; it must report the no-op")
	}
	if n := e.count(t, `SELECT count(*) FROM search_documents`); n != 0 {
		t.Fatalf("%d documents after indexing a deleted artifact, want 0", n)
	}
	if n := e.count(t, `SELECT count(*) FROM search_queue`); n != 0 {
		t.Fatalf("%d queue rows after indexing a deleted artifact, want 0", n)
	}
}

// REQ-11 (lexical): the reindex backfill enqueues live artifacts only, so an
// expired artifact's text never enters the index.
func TestReindexEnqueuesLiveArtifactsOnly(t *testing.T) {
	e := newEnv(t, search.Config{})
	live := e.createMarkdown(t, "Live note", "the live text")
	e.createMarkdown(t, "Another live note", "more live text")
	expired := e.createMarkdown(t, "Expired note", "the expired text")
	e.exec(t, `UPDATE artifacts SET expires_at = now() - interval '1 second' WHERE public_id = $1`, expired.PublicID)

	queued, err := search.EnqueueLive(context.Background(), e.pool, "")
	if err != nil {
		t.Fatalf("enqueue live: %v", err)
	}
	if queued != 2 {
		t.Fatalf("reindex queued %d artifacts, want the 2 live ones", queued)
	}
	e.drain(t)
	if n := e.count(t, `SELECT count(*) FROM search_documents`); n != 2 {
		t.Fatalf("documents = %d after reindex, want 2", n)
	}
	if got := len(e.find(t, "expired")); got != 0 {
		t.Fatalf("find expired = %v, want nothing", got)
	}
	if got := len(e.find(t, "live")); got == 0 {
		t.Fatal("reindex did not backfill the live artifacts")
	}
	_ = live

	// The owner filter narrows the same backfill to one workspace.
	e.exec(t, `DELETE FROM search_documents`)
	e.exec(t, `DELETE FROM search_queue`)
	queued, err = search.EnqueueLive(context.Background(), e.pool, ownerID)
	if err != nil {
		t.Fatalf("enqueue live owner: %v", err)
	}
	if queued != 2 {
		t.Fatalf("owner reindex queued %d, want 2", queued)
	}
	if _, err := search.EnqueueLive(context.Background(), e.pool, "00000000-0000-4000-8000-00000000dead"); err != nil {
		t.Fatalf("enqueue other owner: %v", err)
	}
	if n := e.count(t, `SELECT count(*) FROM search_queue`); n != 2 {
		t.Fatalf("queue = %d, want the unknown owner to add nothing", n)
	}
}

// "Concurrency Safety": workers stop cleanly on cancellation, and a create that
// lands while they run is indexed by one of them. Run under -race in CI.
func TestWorkerPoolShutsDownGracefully(t *testing.T) {
	e := newEnv(t, search.Config{Workers: 3, PollInterval: 5 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		e.ix.Run(ctx)
	}()

	const n = 12
	for i := 0; i < n; i++ {
		e.createMarkdown(t, fmt.Sprintf("Concurrent note %d", i), fmt.Sprintf("marker number %d", i))
	}

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if e.count(t, `SELECT count(*) FROM search_documents`) == n {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := e.count(t, `SELECT count(*) FROM search_documents`); got != n {
		t.Fatalf("indexed %d of %d while the pool ran", got, n)
	}

	cancel()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}

	// Stopped: work queues and nothing indexes it, which is the same contract as
	// the reaper's and the resume case above.
	e.createMarkdown(t, "After shutdown", "queued while stopped")
	if n := e.count(t, `SELECT count(*) FROM search_documents`); n != 12 {
		t.Fatalf("documents = %d after shutdown, want the create to have left the index alone", n)
	}
}

// A document that fails to build backs off instead of spinning, and stops being
// retried once it has used its attempts. The failure is injected by an object
// store that refuses to open bodies.
func TestFailuresBackOffAndGiveUp(t *testing.T) {
	e := newEnv(t, search.Config{MaxAttempts: 2, Backoff: time.Hour, BackoffMax: time.Hour})
	art := e.createMarkdown(t, "Unreadable body", "text that cannot be read back")
	e.exec(t, `UPDATE blobs SET storage_key = 'gone/' || sha256`)
	_ = art

	failing := &failingBlobs{inner: e.obj}
	ix := search.New(e.pool, failing, search.Config{MaxAttempts: 2, Backoff: time.Hour, BackoffMax: time.Hour, Logger: discardLogger()})

	stats, err := ix.Drain(context.Background(), 1)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if stats.Failed != 1 {
		t.Fatalf("drain = %+v, want one failure", stats)
	}
	var attempts int
	var notBefore time.Time
	if err := e.pool.QueryRow(context.Background(), `SELECT attempts, not_before FROM search_queue`).Scan(&attempts, &notBefore); err != nil {
		t.Fatalf("read queue row: %v", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d after one failure, want 1", attempts)
	}
	if !notAfter(time.Now(), notBefore) {
		t.Fatalf("not_before = %s is not in the future: a failed document must back off", notBefore)
	}

	// Past MaxAttempts the row is dropped rather than retried forever.
	e.exec(t, `UPDATE search_queue SET not_before = now()`)
	if _, err := ix.Drain(context.Background(), 1); err != nil {
		t.Fatalf("second drain: %v", err)
	}
	if n := e.count(t, `SELECT count(*) FROM search_queue`); n != 0 {
		t.Fatalf("queue = %d after exhausting MaxAttempts, want the row dropped", n)
	}
}

func notAfter(now, then time.Time) bool { return then.After(now) }

// failingBlobs fails every read, which is how the tests reach the indexer's
// error path without corrupting storage.
type failingBlobs struct{ inner *objectstore.Memory }

func (f *failingBlobs) Get(context.Context, string) (io.ReadCloser, error) {
	return nil, fmt.Errorf("injected object-store failure")
}

// Two indexers in two pools must drain one queue without doing the same work
// twice — the FOR UPDATE SKIP LOCKED claim, which is what lets Cairn scale
// workers across processes.
func TestConcurrentIndexersDoNotDuplicateWork(t *testing.T) {
	e := newEnv(t, search.Config{})
	for i := 0; i < 8; i++ {
		e.createMarkdown(t, fmt.Sprintf("Shared queue %d", i), "indexed by whichever worker claims it")
	}

	var wg sync.WaitGroup
	results := make([]search.DrainStats, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			stats, err := e.ix.Drain(context.Background(), 0)
			if err != nil {
				t.Errorf("drain %d: %v", i, err)
			}
			results[i] = stats
		}(i)
	}
	wg.Wait()

	total := results[0].Indexed + results[1].Indexed
	if total != 8 {
		t.Fatalf("two indexers indexed %d of 8 artifacts, want exactly 8 total", total)
	}
	if n := e.count(t, `SELECT count(*) FROM search_documents`); n != 8 {
		t.Fatalf("documents = %d, want 8", n)
	}
}
