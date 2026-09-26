package mcpsession

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stump-wtf/cairn/internal/errs"
)

// Service is the MCP session core over Postgres, sharing the same pool the
// artifact store and other in-process cores use (ADR-0012: one binary, one
// database). Every query is parameterized (SPEC-0007 REQ "Database Operation
// Standards").
type Service struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// NewService builds the MCP session core over pool.
func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool, now: time.Now}
}

// Record opens (or reopens, on a rare transport session-id collision — see
// below) a session row at the MCP `initialize` handshake. The insert is an
// upsert on the primary key: a streamable-HTTP transport is documented to
// mint a globally-unique session id per connection, but re-initializing an
// already-open id (a client retry before the first initialize's response
// landed) is idempotent rather than a constraint-violation 500 — it resets
// the connected/activity clocks and client identity to this call's view
// rather than erroring or silently keeping stale data.
func (s *Service) Record(ctx context.Context, in RecordInput) (*Session, error) {
	if in.ID == "" || in.OwnerID == "" || in.GrantID == "" {
		return nil, errs.Validationf("mcp session: id, owner, and grant are required")
	}
	now := s.now().UTC()
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO mcp_sessions (id, owner_id, grant_id, client_id, client_name, client_version, connected_at, last_activity_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $7)
		ON CONFLICT (id) DO UPDATE SET
			owner_id = EXCLUDED.owner_id,
			grant_id = EXCLUDED.grant_id,
			client_id = EXCLUDED.client_id,
			client_name = EXCLUDED.client_name,
			client_version = EXCLUDED.client_version,
			connected_at = EXCLUDED.connected_at,
			last_activity_at = EXCLUDED.last_activity_at`,
		in.ID, in.OwnerID, in.GrantID, in.ClientID, in.ClientName, in.ClientVersion, now,
	); err != nil {
		return nil, fmt.Errorf("mcpsession: record session: %w", err)
	}
	return &Session{
		ID: in.ID, OwnerID: in.OwnerID, GrantID: in.GrantID, ClientID: in.ClientID,
		ClientName: in.ClientName, ClientVersion: in.ClientVersion,
		ConnectedAt: now, LastActivityAt: now,
	}, nil
}

// Touch records one unit of activity on sessionID: it always bumps
// last_activity_at and tool_calls (every call, success or failure, is
// activity worth surfacing), and additionally increments artifacts_created
// or annotations_posted when kind names a successful create/annotate call.
// An unknown sessionID (a session this process never recorded, e.g. after a
// restart before initialize) is a silent no-op — activity counters are a
// best-effort presentation aid, not a correctness-critical ledger, so a miss
// here must never fail the tool call it is piggybacked on.
func (s *Service) Touch(ctx context.Context, sessionID string, kind Activity) error {
	if sessionID == "" {
		return nil
	}
	now := s.now().UTC()
	var extra string
	switch kind {
	case ActivityArtifactCreated:
		extra = ", artifacts_created = artifacts_created + 1"
	case ActivityAnnotationPosted:
		extra = ", annotations_posted = annotations_posted + 1"
	}
	if _, err := s.pool.Exec(ctx, `
		UPDATE mcp_sessions
		SET last_activity_at = $1, tool_calls = tool_calls + 1`+extra+`
		WHERE id = $2`,
		now, sessionID,
	); err != nil {
		return fmt.Errorf("mcpsession: touch session %s: %w", sessionID, err)
	}
	return nil
}

// sessionColumns is the column list shared by List and Get, joining
// oauth_grants for its revoked_at so a caller can tell a live connection from
// an ended one without a second query.
const sessionColumns = `
	s.id, s.owner_id, s.grant_id, s.client_id, s.client_name, s.client_version,
	s.connected_at, s.last_activity_at, s.tool_calls, s.artifacts_created, s.annotations_posted,
	g.revoked_at`

// List returns ownerID's MCP sessions (most recently active first),
// owner-scoped (SPEC-0007 acceptance: "owner isolation"). It reports both
// live and ended (grant-revoked) sessions — recent history, not just active
// connections — so limit bounds how far back "recent" reaches.
func (s *Service) List(ctx context.Context, ownerID string, limit int) ([]*Session, error) {
	if ownerID == "" {
		return nil, errs.Validationf("mcp session: owner is required")
	}
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+sessionColumns+`
		FROM mcp_sessions s
		JOIN oauth_grants g ON g.grant_id = s.grant_id
		WHERE s.owner_id = $1
		ORDER BY s.last_activity_at DESC
		LIMIT $2`,
		ownerID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("mcpsession: list sessions: %w", err)
	}
	defer rows.Close()

	var out []*Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("mcpsession: scan session: %w", err)
		}
		out = append(out, sess)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mcpsession: list sessions: %w", err)
	}
	return out, nil
}

// Get returns ownerID's session id, owner-scoped and uniform: a session that
// does not exist or belongs to a different owner is errs.ErrNotFound, so the
// endpoint discloses nothing about another owner's sessions (mirrors
// pat.Service.Revoke's owner-scoped-uniform-404 shape).
func (s *Service) Get(ctx context.Context, ownerID, id string) (*Session, error) {
	if ownerID == "" || id == "" {
		return nil, errs.Validationf("mcp session: owner and id are required")
	}
	row := s.pool.QueryRow(ctx, `
		SELECT `+sessionColumns+`
		FROM mcp_sessions s
		JOIN oauth_grants g ON g.grant_id = s.grant_id
		WHERE s.id = $1 AND s.owner_id = $2`,
		id, ownerID,
	)
	sess, err := scanSession(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("mcp session %s: %w", id, errs.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("mcpsession: get session %s: %w", id, err)
	}
	return sess, nil
}

// rowScanner is the subset of pgx.Rows / pgx.Row Scan needs.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanSession(row rowScanner) (*Session, error) {
	var s Session
	if err := row.Scan(
		&s.ID, &s.OwnerID, &s.GrantID, &s.ClientID, &s.ClientName, &s.ClientVersion,
		&s.ConnectedAt, &s.LastActivityAt, &s.ToolCalls, &s.ArtifactsCreated, &s.AnnotationsPosted,
		&s.GrantRevokedAt,
	); err != nil {
		return nil, err
	}
	return &s, nil
}
