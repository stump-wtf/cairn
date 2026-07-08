// Package db owns the Postgres connection lifecycle and schema migrations.
// Metadata is authoritative here (ADR-0008); bodies live in object storage.
//
// Governing: ADR-0012 (Backend Platform and API Shape),
// SPEC-0002 REQ "Database Operation Standards"
package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Connect opens a context-aware connection pool and verifies reachability.
func Connect(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	if dsn == "" {
		return nil, fmt.Errorf("db: empty database URL")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("db: open pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}
	return pool, nil
}
