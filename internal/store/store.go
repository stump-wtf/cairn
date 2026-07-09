// Package store is the Artifact core service: it streams bodies into
// content-addressed object storage and persists the Artifact aggregate and its
// blob registry in Postgres, transactionally. Every method takes a
// context.Context as its first argument and propagates it to the database and
// object-store calls, so a cancelled request releases its resources.
//
// The REST, MCP, and CLI surfaces are thin adapters over this one package
// (ADR-0003 / ADR-0012); it owns no transport concern.
//
// Governing: ADR-0001 (Cairn as AI-Native Artifact Store),
// ADR-0008 (Storage & Content Model), ADR-0012 (Backend Platform and API Shape),
// SPEC-0002 REQ "Artifact Lifecycle — Create"
package store

import (
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/joestump/cairn/internal/id"
	"github.com/joestump/cairn/internal/objectstore"
)

// idMaxAttempts bounds public-id collision retries; at the targeted keyspace
// occupancy a single retry is already astronomically unlikely.
const idMaxAttempts = 5

// Store is the Artifact core service.
type Store struct {
	pool     *pgxpool.Pool
	obj      objectstore.ObjectStore
	maxBytes int64
	newID    func() (string, error)
}

// Options configures a Store. Zero values fall back to safe defaults.
type Options struct {
	// MaxUploadBytes caps a single streamed body (default 64 MiB).
	MaxUploadBytes int64
	// NewID overrides public-id generation; tests inject forced collisions.
	// Defaults to id.New.
	NewID func() (string, error)
}

// New constructs a Store over a Postgres pool and an object store.
func New(pool *pgxpool.Pool, obj objectstore.ObjectStore, opts Options) *Store {
	newID := opts.NewID
	if newID == nil {
		newID = id.New
	}
	maxBytes := opts.MaxUploadBytes
	if maxBytes <= 0 {
		maxBytes = 64 << 20
	}
	return &Store{pool: pool, obj: obj, maxBytes: maxBytes, newID: newID}
}
