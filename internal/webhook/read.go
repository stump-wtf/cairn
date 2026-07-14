package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/joestump/cairn/internal/errs"
)

// DefaultListLimit / MaxListLimit bound GET /v1/hooks/{id}/requests
// (SPEC-0005 "paginated/bounded").
const (
	DefaultListLimit = 50
	MaxListLimit     = 200
)

// GetEndpoint resolves an endpoint by its public id and returns its metadata.
// It returns ErrEndpointNotFound uniformly for unknown, unauthorized, or
// expired ids (ADR-0007 link-capability).
func (s *Service) GetEndpoint(ctx context.Context, publicID string) (*Endpoint, error) {
	var ep Endpoint
	err := s.pool.QueryRow(ctx, `
		SELECT a.public_id, a.title, h.request_cap,
		       a.actor_id, a.on_behalf_of, a.channel, a.captured_at,
		       a.owner_id, a.visibility, a.expires_at, a.created_at
		FROM hooks h
		JOIN artifacts a ON a.id = h.artifact_id
		WHERE a.public_id = $1 AND a.expires_at > now()`, publicID).Scan(
		&ep.PublicID, &ep.Title, &ep.RequestCap,
		&ep.Provenance.ActorID, &ep.Provenance.OnBehalfOf, &ep.Provenance.Channel, &ep.Provenance.CapturedAt,
		&ep.Access.OwnerID, &ep.Access.Visibility, &ep.ExpiresAt, &ep.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("webhook: endpoint %s: %w", publicID, ErrEndpointNotFound)
		}
		return nil, fmt.Errorf("webhook: load endpoint %s: %w", publicID, err)
	}
	return &ep, nil
}

// RequestPage is one keyset page of a webhook's captured-request buffer,
// newest-first (SPEC-0005 "the live request list (newest prepended)").
// NextBefore is the seq to pass as Before for the next (older) page; zero when
// there is no further page.
type RequestPage struct {
	Requests   []Request
	NextBefore int64
}

// ListRequests returns a keyset page of an endpoint's captured requests in
// descending seq order (newest first). Before, when non-zero, returns only
// requests with seq < Before, so a caller pages strictly backward through
// history; Limit is clamped to (0, MaxListLimit], defaulting to
// DefaultListLimit (SPEC-0005 "GET /v1/hooks/{id}/requests: List captured
// requests (keyset paginated, seq order)"). An unknown/expired endpoint id is
// the uniform ErrEndpointNotFound.
func (s *Service) ListRequests(ctx context.Context, publicID string, before int64, limit int) (RequestPage, error) {
	if limit <= 0 {
		limit = DefaultListLimit
	}
	if limit > MaxListLimit {
		limit = MaxListLimit
	}

	hookID, err := s.resolveHookID(ctx, publicID)
	if err != nil {
		return RequestPage{}, err
	}

	var (
		rows pgx.Rows
	)
	if before > 0 {
		rows, err = s.pool.Query(ctx, `
			SELECT seq, received_at, method, path, query, headers, status,
			       content_type, body_size, body_inline, body_ref_sha256, body_truncated
			FROM hook_requests
			WHERE hook_id = $1 AND seq < $2
			ORDER BY seq DESC
			LIMIT $3`, hookID, before, limit+1)
	} else {
		rows, err = s.pool.Query(ctx, `
			SELECT seq, received_at, method, path, query, headers, status,
			       content_type, body_size, body_inline, body_ref_sha256, body_truncated
			FROM hook_requests
			WHERE hook_id = $1
			ORDER BY seq DESC
			LIMIT $2`, hookID, limit+1)
	}
	if err != nil {
		return RequestPage{}, fmt.Errorf("webhook: list requests for %s: %w", publicID, err)
	}
	defer rows.Close()

	var out []Request
	for rows.Next() {
		req, err := scanRequest(rows)
		if err != nil {
			return RequestPage{}, fmt.Errorf("webhook: scan request: %w", err)
		}
		out = append(out, req)
	}
	if err := rows.Err(); err != nil {
		return RequestPage{}, fmt.Errorf("webhook: iterate requests for %s: %w", publicID, err)
	}

	page := RequestPage{}
	if len(out) > limit {
		out = out[:limit]
		page.NextBefore = out[len(out)-1].Seq
	}
	page.Requests = out
	return page, nil
}

// GetRequest fetches one captured request's full detail (metadata + body ref)
// by its endpoint id and seq. An unknown/expired endpoint is
// ErrEndpointNotFound; a well-formed but unknown or evicted seq is
// ErrRequestNotFound (SPEC-0005 "GET /v1/hooks/{id}/requests/{seq}").
func (s *Service) GetRequest(ctx context.Context, publicID string, seq int64) (*Request, error) {
	hookID, err := s.resolveHookID(ctx, publicID)
	if err != nil {
		return nil, err
	}
	row := s.pool.QueryRow(ctx, `
		SELECT seq, received_at, method, path, query, headers, status,
		       content_type, body_size, body_inline, body_ref_sha256, body_truncated
		FROM hook_requests
		WHERE hook_id = $1 AND seq = $2`, hookID, seq)
	req, err := scanRequest(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("webhook: request %s/%d: %w", publicID, seq, ErrRequestNotFound)
		}
		return nil, fmt.Errorf("webhook: load request %s/%d: %w", publicID, seq, err)
	}
	return &req, nil
}

// BodyInfo describes a captured request body opened for streaming download.
type BodyInfo struct {
	SHA256    string
	Size      int64
	Truncated bool
	Inline    bool
}

// OpenRequestBody opens a captured request's raw body for lazy streaming — the
// deferred fetch the inspector performs only when a request is expanded
// (SPEC-0005 "Bodies MUST be fetched lazily when a request is expanded"). The
// caller must Close the returned reader.
func (s *Service) OpenRequestBody(ctx context.Context, publicID string, seq int64) (io.ReadCloser, BodyInfo, error) {
	hookID, err := s.resolveHookID(ctx, publicID)
	if err != nil {
		return nil, BodyInfo{}, err
	}

	var (
		inline    []byte
		refSHA    *string
		size      int64
		truncated bool
	)
	err = s.pool.QueryRow(ctx, `
		SELECT body_inline, body_ref_sha256, body_size, body_truncated
		FROM hook_requests WHERE hook_id = $1 AND seq = $2`, hookID, seq,
	).Scan(&inline, &refSHA, &size, &truncated)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, BodyInfo{}, fmt.Errorf("webhook: request %s/%d: %w", publicID, seq, ErrRequestNotFound)
		}
		return nil, BodyInfo{}, fmt.Errorf("webhook: load request body %s/%d: %w", publicID, seq, err)
	}

	if refSHA != nil {
		var storageKey string
		if err := s.pool.QueryRow(ctx,
			`SELECT storage_key FROM blobs WHERE sha256 = $1`, *refSHA,
		).Scan(&storageKey); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, BodyInfo{}, fmt.Errorf("webhook: body blob %s: %w", *refSHA, errs.ErrNotFound)
			}
			return nil, BodyInfo{}, fmt.Errorf("webhook: load body blob %s: %w", *refSHA, err)
		}
		rc, err := s.obj.Get(ctx, storageKey)
		if err != nil {
			return nil, BodyInfo{}, fmt.Errorf("webhook: open body blob %s: %w", *refSHA, err)
		}
		return rc, BodyInfo{SHA256: *refSHA, Size: size, Truncated: truncated}, nil
	}

	return io.NopCloser(bytes.NewReader(inline)), BodyInfo{Size: size, Truncated: truncated, Inline: true}, nil
}

// resolveHookID resolves a public endpoint id to its internal hook id,
// enforcing the expiry (hard non-existence) link-capability policy.
func (s *Service) resolveHookID(ctx context.Context, publicID string) (int64, error) {
	var hookID int64
	err := s.pool.QueryRow(ctx,
		`SELECT h.id FROM hooks h JOIN artifacts a ON a.id = h.artifact_id
		 WHERE a.public_id = $1 AND a.expires_at > now()`, publicID,
	).Scan(&hookID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, fmt.Errorf("webhook: endpoint %s: %w", publicID, ErrEndpointNotFound)
		}
		return 0, fmt.Errorf("webhook: resolve endpoint %s: %w", publicID, err)
	}
	return hookID, nil
}

// rowScanner is the pgx.Row / pgx.Rows subset scanRequest needs, so it works
// for both a single QueryRow result and a Rows iteration cursor.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanRequest scans one hook_requests row into a Request.
func scanRequest(row rowScanner) (Request, error) {
	var (
		req         Request
		receivedAt  time.Time
		headersJSON []byte
		inline      []byte
		refSHA      *string
		size        int64
		truncated   bool
	)
	if err := row.Scan(
		&req.Seq, &receivedAt, &req.Method, &req.Path, &req.Query, &headersJSON, &req.Status,
		&req.ContentType, &size, &inline, &refSHA, &truncated,
	); err != nil {
		return Request{}, err
	}
	req.ReceivedAt = receivedAt
	req.BodySize = size
	if len(headersJSON) > 0 {
		var headers map[string][]string
		if err := json.Unmarshal(headersJSON, &headers); err != nil {
			return Request{}, fmt.Errorf("webhook: decode headers: %w", err)
		}
		if len(headers) > 0 {
			req.Headers = headers
		}
	}
	if inline != nil {
		req.Inline = inline
	}
	if refSHA != nil {
		req.Ref = &BodyRef{SHA256: *refSHA, Size: size, Truncated: truncated}
	}
	return req, nil
}
