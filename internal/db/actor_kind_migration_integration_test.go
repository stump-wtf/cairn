package db

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Tests for the per-kind annotation ownership migration.
//
// Governing: ADR-0022, SPEC-0016 EV-6 "Per-Kind Reaction and Comment
// Ownership", REQ "Database Operation Standards" ("Migration over existing
// rows").

// actorKindMigration is the migration under test, without its .sql suffix.
const actorKindMigration = "0022_annotation_actor_kind"

// oldReactionKey is the 0002 idempotency key the migration replaces.
var oldReactionKey = []string{"artifact_id", "anchor_type", "anchor_key", "emoji", "actor_id"}

var actorKindSchemaSeq atomic.Int64

// versionBefore returns the embedded migration version sorted immediately
// before version, so the pre-migration schema follows renumbering and any
// sibling migration that lands below it.
func versionBefore(t *testing.T, version string) string {
	t.Helper()
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	var versions []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			versions = append(versions, strings.TrimSuffix(e.Name(), ".sql"))
		}
	}
	sort.Strings(versions)
	for i, v := range versions {
		if v == version {
			if i == 0 {
				t.Fatalf("%s is the first migration", version)
			}
			return versions[i-1]
		}
	}
	t.Fatalf("migration %s not found; renumbered without updating the test?", version)
	return ""
}

// preActorKindPool opens a private schema migrated up to the migration just
// before actorKindMigration: the state production is in when it runs.
func preActorKindPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("CAIRN_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set CAIRN_TEST_DATABASE_URL to run db integration tests")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("db_actor_kind_test_%d_%d", time.Now().UnixNano(), actorKindSchemaSeq.Add(1))
	admin, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := admin.Exec(ctx, fmt.Sprintf(`CREATE SCHEMA %q`, schema)); err != nil {
		admin.Close()
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), fmt.Sprintf(`DROP SCHEMA %q CASCADE`, schema)); err != nil {
			t.Errorf("drop schema: %v", err)
		}
		admin.Close()
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	prev := versionBefore(t, actorKindMigration)
	if err := migrateThrough(ctx, pool, prev); err != nil {
		t.Fatalf("migrate through %s: %v", prev, err)
	}
	return pool
}

func execOK(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// seedLegacyAnnotations writes what a pre-migration deployment holds: one
// artifact, two actors' 👍 on it, and a comment.
func seedLegacyAnnotations(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var artID int64
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO artifacts (public_id, share_type, actor_id, channel, captured_at, owner_id, expires_at)
		VALUES ('MIGAAAA1', 'markdown', 'alice', 'cli', now(), 'alice', now() + interval '1 hour')
		RETURNING id`).Scan(&artID); err != nil {
		t.Fatalf("seed artifact: %v", err)
	}
	for _, actor := range []string{"alice", "bob"} {
		execOK(t, pool, `
			INSERT INTO reactions (artifact_id, anchor_type, anchor_ref, anchor_key, emoji, actor_id)
			VALUES ($1, 'artifact', '{}', '{}', '👍', $2)`, artID, actor)
	}
	execOK(t, pool, `
		INSERT INTO comments (artifact_id, anchor_type, anchor_ref, anchor_key, actor_id, on_behalf_of, body)
		VALUES ($1, 'artifact', '{}', '{}', 'alice', 'harness/1', 'legacy')`, artID)
	return artID
}

// uniqueConstraintsOn returns the names of reactions' UNIQUE constraints whose
// column list is exactly cols, read from the catalog (the Postgres-generated
// name is never assumed).
func uniqueConstraintsOn(t *testing.T, pool *pgxpool.Pool, cols []string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT con.conname
		  FROM pg_constraint con
		 WHERE con.conrelid = 'reactions'::regclass AND con.contype = 'u'
		   AND ARRAY(SELECT a.attname::text
		               FROM unnest(con.conkey) WITH ORDINALITY AS k(attnum, ord)
		               JOIN pg_attribute a ON a.attrelid = con.conrelid AND a.attnum = k.attnum
		              ORDER BY k.ord) = $1::text[]`, cols)
	if err != nil {
		t.Fatalf("read constraints: %v", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan constraint: %v", err)
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate constraints: %v", err)
	}
	return names
}

// kindIndexDef returns the new index's definition and validity, or "" when it
// does not exist.
func kindIndexDef(t *testing.T, pool *pgxpool.Pool) (def string, valid, unique bool) {
	t.Helper()
	err := pool.QueryRow(context.Background(), `
		SELECT pg_get_indexdef(i.indexrelid), i.indisvalid, i.indisunique
		  FROM pg_index i
		 WHERE i.indexrelid = to_regclass('reactions_idem_kind_uidx')`).Scan(&def, &valid, &unique)
	if err != nil {
		return "", false, false
	}
	return def, valid, unique
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func isCheckViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23514"
}

// TestActorKindMigrationOverExistingRows is SPEC-0016 "Migration over existing
// rows": pre-existing rows gain an empty actor_kind and stay unique under the new
// key, the old key is gone (found by its columns, not an assumed name), and
// the same actor may now hold one row per kind.
func TestActorKindMigrationOverExistingRows(t *testing.T) {
	pool := preActorKindPool(t)
	ctx := context.Background()
	artID := seedLegacyAnnotations(t, pool)

	// Positive control: the catalog probe sees the old key before the
	// migration, so its absence afterwards is a real drop and not a probe
	// that cannot see.
	before := uniqueConstraintsOn(t, pool, oldReactionKey)
	if len(before) != 1 {
		t.Fatalf("old reaction key constraints before migration = %v, want exactly one", before)
	}

	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	var recorded bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version = $1)`,
		actorKindMigration).Scan(&recorded); err != nil || !recorded {
		t.Fatalf("%s recorded = %v (err %v), want true", actorKindMigration, recorded, err)
	}

	if after := uniqueConstraintsOn(t, pool, oldReactionKey); len(after) != 0 {
		t.Fatalf("old reaction key %v survived the migration", after)
	}
	def, valid, unique := kindIndexDef(t, pool)
	if !valid || !unique || !strings.Contains(def, "(artifact_id, anchor_type, anchor_key, emoji, actor_id, actor_kind)") {
		t.Fatalf("per-kind index = %q valid=%v unique=%v", def, valid, unique)
	}

	// Existing rows read back as legacy: empty kind, empty on_behalf_of.
	var n, legacy int
	if err := pool.QueryRow(ctx, `
		SELECT count(*), count(*) FILTER (WHERE actor_kind = '' AND on_behalf_of = '')
		  FROM reactions WHERE artifact_id = $1`, artID).Scan(&n, &legacy); err != nil {
		t.Fatalf("read reactions: %v", err)
	}
	if n != 2 || legacy != 2 {
		t.Fatalf("reactions after migration = %d, legacy = %d; want 2 and 2", n, legacy)
	}
	var commentKind, commentOBO string
	if err := pool.QueryRow(ctx, `SELECT actor_kind, on_behalf_of FROM comments WHERE artifact_id = $1`,
		artID).Scan(&commentKind, &commentOBO); err != nil {
		t.Fatalf("read comment: %v", err)
	}
	if commentKind != "" || commentOBO != "harness/1" {
		t.Fatalf("comment after migration: kind=%q obo=%q, want '' and the original", commentKind, commentOBO)
	}

	insert := func(kind string) error {
		_, err := pool.Exec(ctx, `
			INSERT INTO reactions (artifact_id, anchor_type, anchor_ref, anchor_key, emoji, actor_id, actor_kind)
			VALUES ($1, 'artifact', '{}', '{}', '👍', 'alice', $2)`, artID, kind)
		return err
	}
	// The legacy row still holds its slot under the new key...
	if err := insert(""); !isUniqueViolation(err) {
		t.Fatalf("second legacy row = %v, want a unique violation", err)
	}
	// ...and alice's agent and alice herself each get their own row.
	for _, kind := range []string{"agent", "human"} {
		if err := insert(kind); err != nil {
			t.Fatalf("insert %s row: %v", kind, err)
		}
	}
	if err := insert("agent"); !isUniqueViolation(err) {
		t.Fatalf("duplicate agent row = %v, want a unique violation", err)
	}
	// ON CONFLICT infers the new index, which is what React relies on.
	if _, err := pool.Exec(ctx, `
		INSERT INTO reactions (artifact_id, anchor_type, anchor_ref, anchor_key, emoji, actor_id, actor_kind)
		VALUES ($1, 'artifact', '{}', '{}', '👍', 'alice', 'human')
		ON CONFLICT (artifact_id, anchor_type, anchor_key, emoji, actor_id, actor_kind) DO NOTHING`, artID); err != nil {
		t.Fatalf("upsert on the per-kind key: %v", err)
	}

	// The kind set is closed on both tables.
	if err := insert("admin"); !isCheckViolation(err) {
		t.Fatalf("reaction kind 'admin' = %v, want a check violation", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO comments (artifact_id, anchor_type, anchor_ref, anchor_key, actor_id, actor_kind, body)
		VALUES ($1, 'artifact', '{}', '{}', 'alice', 'Human', 'x')`, artID); !isCheckViolation(err) {
		t.Fatalf("comment kind 'Human' = %v, want a check violation", err)
	}
}

// TestActorKindMigrationResumes proves the file is safe to rerun after a crash
// part-way through (it runs outside a transaction): a partial first run, a
// leftover INVALID index from a failed concurrent build, and a full rerun of
// an already-applied file all converge on the same schema.
func TestActorKindMigrationResumes(t *testing.T) {
	pool := preActorKindPool(t)
	ctx := context.Background()
	seedLegacyAnnotations(t, pool)

	raw, err := migrationsFS.ReadFile("migrations/" + actorKindMigration + ".sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	if !isNoTx(raw) {
		t.Fatal("the migration must run outside a transaction to build its index CONCURRENTLY")
	}
	stmts := splitStatements(raw)

	// Crash after the column statements.
	for _, s := range stmts[:2] {
		execOK(t, pool, s)
	}
	// A failed concurrent build under the index's name: the seeded rows share
	// an artifact, so a unique index on artifact_id alone cannot build and
	// Postgres leaves it behind INVALID.
	if _, err := pool.Exec(ctx,
		`CREATE UNIQUE INDEX CONCURRENTLY reactions_idem_kind_uidx ON reactions (artifact_id)`); !isUniqueViolation(err) {
		t.Fatalf("seeding a failed concurrent build = %v, want a unique violation", err)
	}
	if def, valid, _ := kindIndexDef(t, pool); def == "" || valid {
		t.Fatalf("seeded leftover index = %q valid=%v, want an invalid index", def, valid)
	}

	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate after partial run: %v", err)
	}
	def, valid, unique := kindIndexDef(t, pool)
	if !valid || !unique || !strings.Contains(def, "actor_kind)") {
		t.Fatalf("index after resume = %q valid=%v unique=%v, want the per-kind key", def, valid, unique)
	}
	if left := uniqueConstraintsOn(t, pool, oldReactionKey); len(left) != 0 {
		t.Fatalf("old key %v survived the resumed migration", left)
	}

	// Every statement is idempotent: running the whole file again is a no-op.
	for i, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("rerun statement %d: %v", i+1, err)
		}
	}
}
