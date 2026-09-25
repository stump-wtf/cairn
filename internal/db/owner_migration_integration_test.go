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

	"github.com/stump-wtf/cairn/internal/user"
)

// Tests for 0018_owner_columns.sql, the move from owner strings to user ids.
//
// Governing: ADR-0029, SPEC-0023 REQ "Owner Model", REQ "Migration to
// Explicit Ownership".

var ownerSchemaSeq atomic.Int64

// preOwnerPool opens a private schema migrated through 0017, the state a
// deployment is in before this story's migration runs.
func preOwnerPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("CAIRN_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set CAIRN_TEST_DATABASE_URL to run db integration tests")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("db_owner_test_%d_%d", time.Now().UnixNano(), ownerSchemaSeq.Add(1))
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
	if err := migrateThrough(ctx, pool, "0017_users"); err != nil {
		t.Fatalf("migrate through 0017: %v", err)
	}
	return pool
}

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

func mustUUID(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&id); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return id
}

var legacyArtifactSeq atomic.Int64

// legacyArtifact inserts an artifact in the 0017 schema, owned and created
// by the given strings, and returns its internal id.
func legacyArtifact(t *testing.T, pool *pgxpool.Pool, owner, actor string, age time.Duration) int64 {
	t.Helper()
	var id int64
	n := legacyArtifactSeq.Add(1)
	err := pool.QueryRow(context.Background(), `
		INSERT INTO artifacts (public_id, share_type, title, media_type, actor_id, channel,
		                       captured_at, owner_id, visibility, expires_at, created_at)
		VALUES ($1, 'trajectory', 'legacy', 'application/json', $2, 'via API',
		        now(), $3, 'link', now() + interval '1 day', now() - $4::interval)
		RETURNING id`,
		fmt.Sprintf("legacy%06d", n), actor, owner, fmt.Sprintf("%d seconds", int(age.Seconds())),
	).Scan(&id)
	if err != nil {
		t.Fatalf("insert legacy artifact for %s: %v", owner, err)
	}
	return id
}

// binBefore lists, per owner string, the artifact ids its Bin returns: newest
// first, as store.ListBin orders them.
func binBefore(t *testing.T, pool *pgxpool.Pool) map[string][]int64 {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT owner_id, id FROM artifacts ORDER BY owner_id, created_at DESC, id DESC`)
	if err != nil {
		t.Fatalf("bin before: %v", err)
	}
	defer rows.Close()
	out := map[string][]int64{}
	for rows.Next() {
		var owner string
		var id int64
		if err := rows.Scan(&owner, &id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[owner] = append(out[owner], id)
	}
	return out
}

// binOf lists a user's Bin after the migration, in the same order.
func binOf(t *testing.T, pool *pgxpool.Pool, userID string) []int64 {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT id FROM artifacts WHERE owner_user_id = $1 ORDER BY created_at DESC, id DESC`, userID)
	if err != nil {
		t.Fatalf("bin of %s: %v", userID, err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, id)
	}
	return out
}

// userRendering is the users row a legacy string now renders as.
func userRendering(t *testing.T, pool *pgxpool.Pool, actor string) string {
	t.Helper()
	return mustUUID(t, pool,
		`SELECT id::text FROM users u WHERE `+user.ActorSQL("u")+` = $1`, actor)
}

// SPEC-0023 "Existing owner signs in", fixture half: every owner's Bin is the
// same list before and after, for each way a string resolves to a user, and
// every string still renders as itself on the wire.
func TestOwnerMigrationPreservesEveryBin(t *testing.T) {
	pool := preOwnerPool(t)
	ctx := context.Background()

	// A #326 user who signed in with a verified email, and one with no usable
	// email whose interim key was "user:<id>".
	joe := mustUUID(t, pool, `INSERT INTO users (primary_email, email_verified, display_handle)
		VALUES ('joe@example.com', true, 'joe') RETURNING id::text`)
	anon := mustUUID(t, pool, `INSERT INTO users (display_handle) VALUES ('anon') RETURNING id::text`)
	// A #326 dev-login user whose session acted as the typed name.
	dev := mustUUID(t, pool, `INSERT INTO users (display_handle) VALUES ('devjoe') RETURNING id::text`)
	mustExec(t, pool, `INSERT INTO sessions (token_hash, actor_id, user_id, csrf_token, expires_at)
		VALUES (repeat('a', 64), 'devjoe', $1, 'c', now() + interval '1 day')`, dev)
	// A pre-#326 OIDC session whose actor was its raw subject.
	mustExec(t, pool, `INSERT INTO sessions (token_hash, actor_id, issuer, subject, csrf_token, expires_at)
		VALUES (repeat('b', 64), 'abc-sub', 'https://id.example.com', 'abc-sub', 'c', now() + interval '1 day')`)

	owners := []string{
		"joe@example.com", "user:" + anon, "devjoe", "abc-sub",
		"Pat@Example.com", "pat@example.com", "ci-bot",
	}
	for i, o := range owners {
		for j := 0; j <= i%3; j++ {
			legacyArtifact(t, pool, o, o, time.Duration(10*i+j)*time.Second)
		}
	}
	// An artifact created by one string and owned by another.
	mixed := legacyArtifact(t, pool, "ci-bot", "joe@example.com", time.Hour)

	// Annotations and credentials by strings that own nothing.
	mustExec(t, pool, `INSERT INTO comments (artifact_id, anchor_type, anchor_key, actor_id, body)
		VALUES ($1, 'whole', '{}', 'commenter@example.com', 'hi')`, mixed)
	mustExec(t, pool, `INSERT INTO reactions (artifact_id, anchor_type, anchor_key, emoji, actor_id)
		VALUES ($1, 'whole', '{}', '👀', 'reactor')`, mixed)
	mustExec(t, pool, `INSERT INTO personal_access_tokens (id, owner_id, name, token_hash, scope)
		VALUES ('pat-1', 'devjoe', 'laptop', repeat('c', 64), 'artifacts:read')`)
	mustExec(t, pool, `INSERT INTO oauth_clients (client_id, redirect_uris) VALUES ('cl', '{}')`)
	mustExec(t, pool, `INSERT INTO oauth_grants (grant_id, client_id, actor_id, scope)
		VALUES ('g1', 'cl', 'joe@example.com', 'artifacts:read')`)
	mustExec(t, pool, `INSERT INTO mcp_sessions (id, owner_id, grant_id, client_id)
		VALUES ('m1', 'joe@example.com', 'g1', 'cl')`)
	mustExec(t, pool, `INSERT INTO oauth_auth_codes (code_hash, client_id, actor_id, redirect_uri, scope, code_challenge, expires_at)
		VALUES (repeat('d', 64), 'cl', 'granted@example.com', 'http://x', 'artifacts:read', 'ch', now())`)

	before := binBefore(t, pool)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	for _, o := range owners {
		uid := userRendering(t, pool, o)
		if got, want := binOf(t, pool, uid), before[o]; fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("Bin of %s = %v after, %v before", o, got, want)
		}
	}
	for owner, want := range map[string]string{"joe@example.com": joe, "user:" + anon: anon, "devjoe": dev} {
		if got := userRendering(t, pool, owner); got != want {
			t.Errorf("%s resolved to user %s, want the existing user %s", owner, got, want)
		}
	}
	if userRendering(t, pool, "Pat@Example.com") == userRendering(t, pool, "pat@example.com") {
		t.Error("two owner strings that were distinct owners share a user")
	}

	var creator string
	if err := pool.QueryRow(ctx, `SELECT `+user.ActorSQL("u")+` FROM artifacts a
		JOIN users u ON u.id = a.created_by_user_id WHERE a.id = $1`, mixed).Scan(&creator); err != nil {
		t.Fatalf("creator: %v", err)
	}
	if creator != "joe@example.com" {
		t.Errorf("creator renders as %q, want joe@example.com", creator)
	}

	// Every backfilled row points at the user its string resolved to.
	for table, want := range map[string]string{
		"comments": "commenter@example.com", "reactions": "reactor",
		"personal_access_tokens": "devjoe", "oauth_grants": "joe@example.com",
		"mcp_sessions": "joe@example.com", "oauth_auth_codes": "granted@example.com",
	} {
		got := mustUUID(t, pool, `SELECT user_id::text FROM `+table+` LIMIT 1`)
		if uid := userRendering(t, pool, want); got != uid {
			t.Errorf("%s.user_id = %s, want %s's user %s", table, got, want, uid)
		}
	}
	if got := mustUUID(t, pool, `SELECT user_id::text FROM sessions WHERE token_hash = repeat('b', 64)`); got != userRendering(t, pool, "abc-sub") {
		t.Errorf("pre-users session resolved to %s, want abc-sub's user", got)
	}

	// The raw-subject owner is recognised by its (issuer, subject) again.
	if got := mustUUID(t, pool, `SELECT user_id::text FROM user_identities
		WHERE issuer = 'https://id.example.com' AND subject = 'abc-sub'`); got != userRendering(t, pool, "abc-sub") {
		t.Errorf("abc-sub identity links to %s, want its legacy user", got)
	}

	// Legacy users are unverified and hold no primary email until claimed.
	var verified bool
	var primary *string
	if err := pool.QueryRow(ctx, `SELECT email_verified, primary_email FROM users WHERE actor_key = 'pat@example.com'`).
		Scan(&verified, &primary); err != nil {
		t.Fatalf("legacy user: %v", err)
	}
	if verified || primary != nil {
		t.Errorf("legacy user verified=%v primary=%v, want unverified with no primary email", verified, primary)
	}
}

// SPEC-0023 "Legacy owner strings are gone": no backfilled string column is
// left, and no index or constraint names one; the reaction idempotency key
// (SPEC-0016 EV-6) is rebuilt on user_id.
func TestOwnerMigrationDropsLegacyStrings(t *testing.T) {
	pool := preOwnerPool(t)
	ctx := context.Background()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	legacy := map[string][]string{
		"artifacts": {"owner_id", "actor_id"}, "comments": {"actor_id"}, "reactions": {"actor_id"},
		"personal_access_tokens": {"owner_id"}, "mcp_sessions": {"owner_id"},
		"oauth_grants": {"actor_id"}, "oauth_auth_codes": {"actor_id"}, "sessions": {"actor_id"},
	}
	for table, cols := range legacy {
		for _, col := range cols {
			var n int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
				WHERE table_schema = current_schema() AND table_name = $1 AND column_name = $2`, table, col).Scan(&n); err != nil {
				t.Fatalf("columns: %v", err)
			}
			if n != 0 {
				t.Errorf("%s.%s still exists", table, col)
			}
		}
		var nullable string
		if err := pool.QueryRow(ctx, `SELECT is_nullable FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = $1
			  AND column_name = CASE WHEN $1 = 'artifacts' THEN 'owner_user_id' ELSE 'user_id' END`, table).Scan(&nullable); err != nil {
			t.Fatalf("%s user column: %v", table, err)
		}
		if table != "artifacts" && nullable != "NO" {
			t.Errorf("%s.user_id is nullable", table)
		}
	}
	rows, err := pool.Query(ctx, `SELECT indexname, indexdef FROM pg_indexes WHERE schemaname = current_schema()`)
	if err != nil {
		t.Fatalf("indexes: %v", err)
	}
	defer rows.Close()
	var reactionKey string
	for rows.Next() {
		var name, def string
		if err := rows.Scan(&name, &def); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if strings.Contains(def, "owner_id") || strings.Contains(def, "actor_id") {
			t.Errorf("index %s still names a legacy string: %s", name, def)
		}
		if name == "reactions_idem_key" {
			reactionKey = def
		}
	}
	if !strings.Contains(reactionKey, "(artifact_id, anchor_type, anchor_key, emoji, user_id)") {
		t.Errorf("reaction key = %q, want it rebuilt on user_id", reactionKey)
	}
}

// SPEC-0023 "A failed comparison leaves the old schema": two strings that were
// two owners but resolve to one user would merge their Bins, so the migration
// aborts, names the first such owner, and leaves every legacy column and the
// migration record as they were.
func TestOwnerMigrationAbortsOnBinMismatch(t *testing.T) {
	pool := preOwnerPool(t)
	ctx := context.Background()
	u := mustUUID(t, pool, `INSERT INTO users (display_handle) VALUES ('twice') RETURNING id::text`)
	mustExec(t, pool, `INSERT INTO sessions (token_hash, actor_id, user_id, csrf_token, expires_at) VALUES
		(repeat('a', 64), 'alpha', $1, 'c', now() + interval '1 day'),
		(repeat('b', 64), 'bravo', $1, 'c', now() + interval '1 day')`, u)
	legacyArtifact(t, pool, "alpha", "alpha", 0)
	legacyArtifact(t, pool, "bravo", "bravo", 0)

	err := Migrate(ctx, pool)
	if err == nil {
		t.Fatal("migrate succeeded, want it to abort on the merged Bins")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || !strings.Contains(pgErr.Message, "would change the Bin of owner alpha") {
		t.Fatalf("migrate error = %v, want one naming owner alpha", err)
	}
	if !strings.Contains(err.Error(), "0018_owner_columns") {
		t.Errorf("error %q does not name the migration", err)
	}

	var cols []string
	rows, qerr := pool.Query(ctx, `SELECT column_name FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'artifacts'`)
	if qerr != nil {
		t.Fatalf("columns: %v", qerr)
	}
	for rows.Next() {
		var c string
		_ = rows.Scan(&c)
		cols = append(cols, c)
	}
	rows.Close()
	sort.Strings(cols)
	joined := strings.Join(cols, ",")
	if !strings.Contains(joined, "owner_id") || !strings.Contains(joined, "actor_id") || strings.Contains(joined, "owner_user_id") {
		t.Errorf("artifacts columns after the abort = %s, want the 0017 schema", joined)
	}
	var applied bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version = '0018_owner_columns')`).Scan(&applied); err != nil {
		t.Fatalf("schema_migrations: %v", err)
	}
	if applied {
		t.Error("the aborted migration was recorded as applied")
	}
	var users int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&users); err != nil {
		t.Fatalf("users: %v", err)
	}
	if users != 1 {
		t.Errorf("users = %d after the abort, want the 1 that existed", users)
	}
}

// SPEC-0023 "Constraint rejects two owners": an artifact with both a user and
// a team owner, or neither, is refused by the database itself.
func TestOwnerConstraintRejectsTwoOwners(t *testing.T) {
	pool := preOwnerPool(t)
	ctx := context.Background()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	uid := mustUUID(t, pool, `INSERT INTO users (display_handle) VALUES ('o') RETURNING id::text`)
	insert := func(pid string, userID, teamID any) error {
		_, err := pool.Exec(ctx, `
			INSERT INTO artifacts (public_id, share_type, media_type, channel, captured_at,
			                       owner_user_id, owner_team_id, visibility, expires_at)
			VALUES ($1, 'trajectory', 'application/json', 'via API', now(), $2, $3, 'link', now() + interval '1 day')`,
			pid, userID, teamID)
		return err
	}
	for name, tc := range map[string][2]any{
		"both":    {uid, "00000000-0000-0000-0000-000000000001"},
		"neither": {nil, nil},
	} {
		err := insert("two"+name, tc[0], tc[1])
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.ConstraintName != "artifacts_one_owner" {
			t.Errorf("%s owners: err = %v, want artifacts_one_owner violation", name, err)
		}
	}
	if err := insert("oneowner", uid, nil); err != nil {
		t.Errorf("a single user owner was refused: %v", err)
	}
}

// SPEC-0023 "Existing owner signs in": artifacts owned by the string
// joe@example.com before the upgrade belong to whoever later signs in with
// that email VERIFIED, and their Bin is unchanged. A mixed-case legacy string
// is claimed the same way; an unverified sign-in claims nothing.
func TestOwnerMigrationLegacyRowClaimedByVerifiedSignIn(t *testing.T) {
	pool := preOwnerPool(t)
	ctx := context.Background()
	legacyArtifact(t, pool, "joe@example.com", "joe@example.com", 2*time.Second)
	legacyArtifact(t, pool, "joe@example.com", "joe@example.com", time.Second)
	legacyArtifact(t, pool, "Kim@Example.com", "Kim@Example.com", 0)
	before := binBefore(t, pool)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	users := user.NewStore(pool)

	squatter, err := users.Resolve(ctx, user.Identity{Issuer: "https://evil.example.com", Subject: "x", Email: "joe@example.com"})
	if err != nil {
		t.Fatalf("unverified sign-in: %v", err)
	}
	if got := binOf(t, pool, squatter.ID); len(got) != 0 {
		t.Fatalf("an unverified email reached the legacy Bin: %v", got)
	}

	joe, err := users.Resolve(ctx, user.Identity{Issuer: "https://id.example.com", Subject: "joe", Email: "Joe@Example.com", EmailVerified: true})
	if err != nil {
		t.Fatalf("verified sign-in: %v", err)
	}
	if got, want := binOf(t, pool, joe.ID), before["joe@example.com"]; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("Bin after claim = %v, want %v", got, want)
	}
	if !joe.EmailVerified || joe.PrimaryEmail != "joe@example.com" || joe.Actor != "joe@example.com" {
		t.Errorf("claimed user = %+v, want verified joe@example.com", joe)
	}

	kim, err := users.Resolve(ctx, user.Identity{Issuer: "https://id.example.com", Subject: "kim", Email: "kim@example.com", EmailVerified: true})
	if err != nil {
		t.Fatalf("kim sign-in: %v", err)
	}
	if got, want := binOf(t, pool, kim.ID), before["Kim@Example.com"]; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("mixed-case legacy Bin after claim = %v, want %v", got, want)
	}
}
