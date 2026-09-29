package db

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Tests for the advisory lock that serializes migrators. A no-transaction
// migration (0022) is not protected by a wrapping transaction, so two
// processes starting together must take turns rather than interleave its
// statements.

var lockSchemaSeq atomic.Int64

// emptySchemaPool opens a pool on a private, empty schema and returns it with
// the schema's name.
func emptySchemaPool(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	dsn := os.Getenv("CAIRN_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set CAIRN_TEST_DATABASE_URL to run db integration tests")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("db_migrate_lock_test_%d_%d", time.Now().UnixNano(), lockSchemaSeq.Add(1))
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
	return pool, schema
}

func embeddedVersionCount(t *testing.T) int {
	t.Helper()
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			n++
		}
	}
	return n
}

func appliedCount(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	return n
}

// TestMigrateWaitsForTheLock holds the migration lock for a schema from
// another session and proves Migrate waits for it: a caller whose context
// ends while waiting gives up with that error, a patient one touches nothing
// while it waits, and it completes once the lock is released.
func TestMigrateWaitsForTheLock(t *testing.T) {
	pool, _ := emptySchemaPool(t)
	ctx := context.Background()

	holder, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire holder: %v", err)
	}
	defer holder.Release()
	var key int32
	if err := holder.QueryRow(ctx, `SELECT hashtext(current_schema())`).Scan(&key); err != nil {
		t.Fatalf("lock key: %v", err)
	}
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_lock($1, $2)`, migrateLockClass, key); err != nil {
		t.Fatalf("hold lock: %v", err)
	}

	short, cancel := context.WithTimeout(ctx, 2*migrateLockPoll)
	defer cancel()
	if err := Migrate(short, pool); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Migrate with an expiring context while locked out = %v, want DeadlineExceeded", err)
	}

	done := make(chan error, 1)
	go func() { done <- Migrate(ctx, pool) }()

	// Unlocked, Migrate creates schema_migrations first and finishes the
	// whole chain well inside this window; locked out, it must do neither.
	select {
	case err := <-done:
		t.Fatalf("Migrate finished (%v) while another session held the lock", err)
	case <-time.After(4 * migrateLockPoll):
	}
	var table *string
	if err := holder.QueryRow(ctx, `SELECT to_regclass('schema_migrations')::text`).Scan(&table); err != nil {
		t.Fatalf("probe schema_migrations: %v", err)
	}
	if table != nil {
		t.Fatalf("schema_migrations = %q while Migrate waited, want it untouched", *table)
	}

	if _, err := holder.Exec(ctx, `SELECT pg_advisory_unlock($1, $2)`, migrateLockClass, key); err != nil {
		t.Fatalf("release lock: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Migrate after release: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("Migrate did not finish after the lock was released")
	}
	if got, want := appliedCount(t, pool), embeddedVersionCount(t); got != want {
		t.Fatalf("applied %d migrations, want %d", got, want)
	}
}

// TestConcurrentMigratorsAllSucceed starts several migrators on one empty
// schema at once, as overlapping deploys would. Each must succeed, and every
// migration is applied exactly once.
func TestConcurrentMigratorsAllSucceed(t *testing.T) {
	pool, _ := emptySchemaPool(t)
	ctx := context.Background()

	const runners = 3
	var wg sync.WaitGroup
	errs := make([]error, runners)
	start := make(chan struct{})
	for i := range runners {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = Migrate(ctx, pool)
		}()
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("migrator %d: %v", i, err)
		}
	}
	if got, want := appliedCount(t, pool), embeddedVersionCount(t); got != want {
		t.Fatalf("applied %d migrations, want %d", got, want)
	}
	// The lock is released: a later Migrate is a prompt no-op.
	lctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := Migrate(lctx, pool); err != nil {
		t.Fatalf("Migrate after the race: %v", err)
	}
}
