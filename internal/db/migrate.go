package db

import (
	"context"
	"embed"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// noTxDirective, as a migration's first line, runs that migration outside a
// transaction: statement by statement, split on stmtBreak lines, and recorded
// only after the last one succeeds. It exists for DDL Postgres refuses inside
// a transaction block, chiefly CREATE INDEX CONCURRENTLY, which the SPEC-0016
// "Database Operation Standards" require so a live table keeps taking writes
// while its index builds.
//
// A crash between two statements leaves the migration unrecorded, so the next
// start reruns the whole file. Every statement in such a file MUST therefore
// be idempotent (IF NOT EXISTS, catalog-guarded DO blocks), and it may not
// rely on an earlier statement's effects being rolled back.
const (
	noTxDirective = "-- cairn:no-transaction"
	stmtBreak     = "-- cairn:statement-break"
)

// migrateLockClass is the first key of the session-level advisory lock that
// serializes migrators; the second is hashtext(current_schema()), so two
// processes migrating the same schema take turns while separate schemas (the
// per-test ones) do not wait on each other. The value is "cair" in ASCII.
const migrateLockClass int32 = 0x63616972

// Migrate applies every embedded migration not yet recorded, in lexical
// (version) order, each within its own transaction unless it opts out with
// noTxDirective. It is idempotent: already applied versions are skipped.
// Migrations are bundled into the binary so the single artifact carries its
// own schema (ADR-0012 "Deployment shape").
//
// Concurrent callers are serialized by a session-level advisory lock, and
// each re-reads schema_migrations once it holds the lock. A transactional
// migration was already protected by its transaction and the version primary
// key; a no-transaction one is not, and two runners interleaving its
// statements could each drop the other's in-progress concurrent index build
// (it reads as INVALID until it finishes) and deadlock.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	return migrateThrough(ctx, pool, "")
}

// migrateThrough applies pending migrations up to and including version last
// ("" applies all). Tests use it to seed a schema as an earlier release left
// it before applying the migration under test. Every statement runs on the one
// connection that holds the migration lock.
func migrateThrough(ctx context.Context, pool *pgxpool.Pool, last string) (err error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("db: acquire migration connection: %w", err)
	}
	var lockKey int32
	if err := conn.QueryRow(ctx, `SELECT hashtext(current_schema())`).Scan(&lockKey); err != nil {
		conn.Release()
		return fmt.Errorf("db: derive migration lock key: %w", err)
	}
	if err := takeMigrateLock(ctx, conn, lockKey); err != nil {
		// A failed or cancelled try may still have been granted server-side;
		// never hand a connection that might hold the lock back to the pool.
		_ = conn.Hijack().Close(context.Background())
		return err
	}
	defer func() {
		// Unlock with a fresh context: ctx may be the reason we are leaving.
		// If the unlock fails, close the connection, which releases the lock,
		// rather than pool a connection that still holds it.
		if _, uerr := conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1, $2)`, migrateLockClass, lockKey); uerr != nil {
			_ = conn.Hijack().Close(context.Background())
			if err == nil {
				err = fmt.Errorf("db: release migration lock: %w", uerr)
			}
			return
		}
		conn.Release()
	}()
	return applyPending(ctx, conn, last)
}

// migrateLockPoll is how often a waiting migrator retries the lock.
const migrateLockPoll = 250 * time.Millisecond

// takeMigrateLock takes the migration lock on conn, polling
// pg_try_advisory_lock rather than blocking in pg_advisory_lock. A blocked
// pg_advisory_lock sits inside an open statement transaction, and CREATE
// INDEX CONCURRENTLY in the lock holder waits for every such transaction to
// end: the holder waits on the waiter, the waiter on the holder, and Postgres
// aborts one as a deadlock. Between tries a poller holds no transaction, so
// the build proceeds.
func takeMigrateLock(ctx context.Context, conn *pgxpool.Conn, lockKey int32) error {
	for {
		var got bool
		if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1, $2)`, migrateLockClass, lockKey).Scan(&got); err != nil {
			return fmt.Errorf("db: take migration lock: %w", err)
		}
		if got {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("db: wait for migration lock: %w", ctx.Err())
		case <-time.After(migrateLockPoll):
		}
	}
}

// applyPending is migrateThrough's body, run while conn holds the migration
// lock.
func applyPending(ctx context.Context, conn *pgxpool.Conn, last string) error {
	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("db: ensure schema_migrations: %w", err)
	}

	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("db: read migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		version := strings.TrimSuffix(name, ".sql")
		if last != "" && version > last {
			break
		}

		var exists bool
		if err := conn.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version = $1)`, version,
		).Scan(&exists); err != nil {
			return fmt.Errorf("db: check migration %s: %w", version, err)
		}
		if exists {
			continue
		}

		sqlBytes, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("db: read migration %s: %w", name, err)
		}

		if isNoTx(sqlBytes) {
			if err := applyNoTx(ctx, conn, version, sqlBytes); err != nil {
				return err
			}
			continue
		}

		tx, err := conn.Begin(ctx)
		if err != nil {
			return fmt.Errorf("db: begin migration %s: %w", version, err)
		}
		if _, err := tx.Exec(ctx, string(sqlBytes)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("db: apply migration %s: %w", version, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations (version) VALUES ($1)`, version,
		); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("db: record migration %s: %w", version, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("db: commit migration %s: %w", version, err)
		}
	}
	return nil
}

// isNoTx reports whether a migration opts out of the wrapping transaction:
// its first line is exactly noTxDirective.
func isNoTx(sqlBytes []byte) bool {
	first, _, _ := strings.Cut(string(sqlBytes), "\n")
	return strings.TrimSpace(first) == noTxDirective
}

// splitStatements cuts a no-transaction migration on its stmtBreak lines.
// Postgres runs a multi-statement string as one implicit transaction, which
// CREATE INDEX CONCURRENTLY refuses, so each statement is sent on its own.
// Chunks holding only comments and whitespace are dropped.
func splitStatements(sqlBytes []byte) []string {
	var (
		out []string
		cur strings.Builder
	)
	flush := func() {
		if hasSQL(cur.String()) {
			out = append(out, strings.TrimSpace(cur.String()))
		}
		cur.Reset()
	}
	for _, line := range strings.Split(string(sqlBytes), "\n") {
		if strings.TrimSpace(line) == stmtBreak {
			flush()
			continue
		}
		cur.WriteString(line)
		cur.WriteByte('\n')
	}
	flush()
	return out
}

// hasSQL reports whether chunk holds anything besides `--` comments and
// whitespace.
func hasSQL(chunk string) bool {
	for _, line := range strings.Split(chunk, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "--") {
			return true
		}
	}
	return false
}

// applyNoTx runs a no-transaction migration one statement at a time and
// records it once every statement has succeeded (see noTxDirective).
func applyNoTx(ctx context.Context, conn *pgxpool.Conn, version string, sqlBytes []byte) error {
	stmts := splitStatements(sqlBytes)
	if len(stmts) == 0 {
		return fmt.Errorf("db: migration %s: no statements", version)
	}
	for i, stmt := range stmts {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("db: apply migration %s (statement %d of %d): %w", version, i+1, len(stmts), err)
		}
	}
	if _, err := conn.Exec(ctx,
		`INSERT INTO schema_migrations (version) VALUES ($1)`, version,
	); err != nil {
		return fmt.Errorf("db: record migration %s: %w", version, err)
	}
	return nil
}
