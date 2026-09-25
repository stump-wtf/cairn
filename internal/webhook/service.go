package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/id"
	"github.com/stump-wtf/cairn/internal/objectstore"
	"github.com/stump-wtf/cairn/internal/store"
	"github.com/stump-wtf/cairn/internal/user"
)

// Default inline-vs-spill threshold and hard per-request body cap. A captured
// body at or below the threshold is stored inline on the row; a larger one
// spills to a content-addressed blob, mirroring internal/trajectory's span
// output split exactly (SPEC-0005 "Request Body Size Limits": "a hard
// body-size cap (e.g. a few MB)").
const (
	defaultInlineThreshold = 16 << 10            // 16 KiB
	defaultMaxBodyBytes    = DefaultMaxBodyBytes // 5 MiB per captured request body
	idMaxAttempts          = 5
	mediaTypeWebhook       = "application/vnd.cairn.webhook"
)

// DefaultMaxBodyBytes is the hard per-captured-request body cap Capture
// enforces by default (SPEC-0005 "Request Body Size Limits"). The open
// ingress transport (internal/httpapi) reads this back via MaxBodyBytes so
// its own pre-buffering 413 cap can never drift from the cap Capture itself
// applies — one number, not two independently maintained ones.
const DefaultMaxBodyBytes = 5 << 20

// Service is the webhook core service. It shares the store's Postgres pool and
// object store so a webhook endpoint is an ordinary artifact in the same
// database and transaction domain (ADR-0012 one binary, one core), exactly
// mirroring internal/trajectory.Service's construction.
type Service struct {
	pool            *pgxpool.Pool
	obj             objectstore.ObjectStore
	inlineThreshold int64
	maxBodyBytes    int64
	newID           func() (string, error)
	now             func() time.Time
	// hub fans each captured request out to live SSE and MCP subscribers of an
	// endpoint. It is process-local: one binary owns the capture, so its
	// in-memory fan-out and the persisted hook_requests rows are the same
	// seq-ordered log both transports read (SPEC-0005 "One Stream, Two
	// Transports (Live Fan-out)"), exactly mirroring internal/trajectory.Service's hub.
	hub *hub
}

// Options configures a Service. Zero values fall back to safe defaults.
type Options struct {
	// InlineThresholdBytes is the size at or below which a captured body is
	// stored inline; above it the body spills to a blob (default 16 KiB).
	InlineThresholdBytes int64
	// MaxBodyBytes caps a single captured request body (default 5 MiB); a body
	// past it is rejected with payload_too_large.
	MaxBodyBytes int64
	// NewID overrides public-id generation; tests inject forced collisions.
	NewID func() (string, error)
	// Now overrides the clock, for deterministic tests.
	Now func() time.Time
}

// MaxBodyBytes reports the hard per-captured-request body cap this Service
// enforces (default DefaultMaxBodyBytes, or Options.MaxBodyBytes when set).
// The open ingress transport reads this to size its own pre-buffering 413
// cap, so the HTTP-layer limit can never silently drift from what Capture
// itself will accept (SPEC-0005 "Request Body Size Limits").
func (s *Service) MaxBodyBytes() int64 { return s.maxBodyBytes }

// NewService constructs a Service over a Postgres pool and an object store.
func NewService(pool *pgxpool.Pool, obj objectstore.ObjectStore, opts Options) *Service {
	s := &Service{
		pool:            pool,
		obj:             obj,
		inlineThreshold: opts.InlineThresholdBytes,
		maxBodyBytes:    opts.MaxBodyBytes,
		newID:           opts.NewID,
		now:             opts.Now,
		hub:             newHub(),
	}
	if s.inlineThreshold <= 0 {
		s.inlineThreshold = defaultInlineThreshold
	}
	if s.maxBodyBytes <= 0 {
		s.maxBodyBytes = defaultMaxBodyBytes
	}
	if s.newID == nil {
		s.newID = id.New
	}
	if s.now == nil {
		s.now = time.Now
	}
	return s
}

// CreateEndpoint provisions a webhook capture endpoint: it mints the artifact
// envelope and the hook row (ring-buffer cap, seq counter starting at zero) in
// one transaction, so a partial endpoint is never visible (SPEC-0005
// "Webhook Endpoint and Two Addresses", "Database Operation Standards").
func (s *Service) CreateEndpoint(ctx context.Context, in EndpointInput) (*Endpoint, error) {
	if err := in.validate(); err != nil {
		return nil, err
	}
	reqCap := in.RequestCap
	if reqCap == 0 {
		reqCap = DefaultRequestCap
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("webhook: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	artID, publicID, err := s.insertEndpointArtifact(ctx, tx, in)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO hooks (artifact_id, request_cap, next_seq) VALUES ($1, $2, 0)`,
		artID, reqCap,
	); err != nil {
		return nil, fmt.Errorf("webhook: insert hook: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("webhook: commit: %w", err)
	}
	return s.GetEndpoint(ctx, publicID)
}

// spilled holds a captured body's disposition after the inline-vs-blob
// decision (mirrors internal/trajectory.spilled). When staged is non-nil the
// body spilled to a content-addressed blob that is streamed to a staging object
// but NOT yet promoted or registered — the caller finalizes it with
// store.CommitBlob under the blob-row lock and Discards the staging object on
// every path (the reaper-safe two-phase blob write).
type spilled struct {
	inline    []byte
	staged    *store.StagedBlob
	size      int64
	truncated bool
}

// refSHA is the captured body's content hash, or "" when the body was inlined or
// empty (nothing spilled to a blob).
func (d spilled) refSHA() string {
	if d.staged == nil {
		return ""
	}
	return d.staged.SHA256
}

// spillBody decides whether a captured body stays inline or spills to a
// content-addressed blob. An oversized body is streamed to a staging object here
// (before the transaction); the caller promotes and registers it via
// store.CommitBlob under the blob-row lock so the write serializes against the
// SPEC-0009 reaper, and Discards the staging object on every path (ADR-0008).
func (s *Service) spillBody(ctx context.Context, body []byte, declaredMedia string) (spilled, error) {
	size := int64(len(body))
	if size > s.maxBodyBytes {
		return spilled{}, fmt.Errorf("webhook: captured body: %w", errs.ErrTooLarge)
	}
	if size == 0 {
		return spilled{}, nil
	}
	if size <= s.inlineThreshold {
		cp := make([]byte, size)
		copy(cp, body)
		return spilled{inline: cp, size: size}, nil
	}
	staged, err := store.StageBlob(ctx, s.obj, bytes.NewReader(body), s.maxBodyBytes, declaredMedia)
	if err != nil {
		return spilled{}, fmt.Errorf("webhook: spill captured body: %w", err)
	}
	return spilled{staged: staged, size: staged.Size, truncated: false}, nil
}

// Capture records one accepted inbound request against an endpoint: it
// assigns the next monotonic seq, inserts the request row (headers sanitized,
// body inlined or spilled), and — atomically in the same transaction — evicts
// whatever now sits past the ring-buffer cap (SPEC-0005 "Ring-Buffer Retention
// and Caps", "Capture-and-evict is atomic", "Concurrent captures keep seq
// monotonic"). It is the core method the public open ingress
// (internal/httpapi's `ANY /h/{id}` route, issue #84) calls for every real
// inbound request; this package's own tests also call it directly to
// simulate captures without going through HTTP.
//
// An unknown or expired endpoint id returns the uniform ErrEndpointNotFound
// (ADR-0007 link-capability) so probing an id leaks no signal (SPEC-0005
// "Unguessable ID & No Enumeration").
func (s *Service) Capture(ctx context.Context, publicID string, in CaptureInput) (*Request, error) {
	if err := in.validate(); err != nil {
		return nil, err
	}
	d, err := s.spillBody(ctx, in.Body, in.ContentType)
	if err != nil {
		return nil, err
	}
	// Reclaim the staging object on every path (committed OR rolled back) so no
	// staging/<rand> orphan survives (ADR-0008 / SPEC-0002 content addressing).
	defer d.staged.Discard(ctx, s.obj)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("webhook: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Lock the hook row and atomically bump its seq counter in one statement:
	// the row lock this UPDATE holds for the rest of the transaction serializes
	// every concurrent Capture against the same endpoint, so seq assignment and
	// the eviction below can never race (SPEC-0005 "Concurrency Safety").
	var (
		hookID  int64
		seq     int64
		reqCap  int
		headers = sanitizeHeaders(in.Headers)
	)
	err = tx.QueryRow(ctx, `
		UPDATE hooks SET next_seq = hooks.next_seq + 1
		FROM artifacts a
		WHERE hooks.artifact_id = a.id AND a.public_id = $1 AND a.expires_at > now()
		RETURNING hooks.id, hooks.next_seq, hooks.request_cap`,
		publicID,
	).Scan(&hookID, &seq, &reqCap)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("webhook: endpoint %s: %w", publicID, ErrEndpointNotFound)
		}
		return nil, fmt.Errorf("webhook: lock endpoint %s: %w", publicID, err)
	}

	if d.staged != nil {
		// Register + promote the captured-body blob under its row lock, so this
		// write serializes against the reaper's orphan sweep (ADR-0008 / SPEC-0009).
		if err := store.CommitBlob(ctx, tx, s.obj, d.staged); err != nil {
			return nil, fmt.Errorf("webhook: %w", err)
		}
	}

	headersJSON := []byte("{}")
	if len(headers) > 0 {
		var err error
		headersJSON, err = json.Marshal(headers)
		if err != nil {
			return nil, fmt.Errorf("webhook: marshal headers: %w", err)
		}
	}
	receivedAt := s.now()
	if _, err := tx.Exec(ctx, `
		INSERT INTO hook_requests
			(hook_id, seq, received_at, method, path, query, headers, status,
			 content_type, body_size, body_inline, body_ref_sha256, body_truncated)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		hookID, seq, receivedAt, in.Method, in.Path, in.Query, headersJSON, in.Status,
		in.ContentType, d.size, d.inline, nullString(d.refSHA()), d.truncated,
	); err != nil {
		return nil, fmt.Errorf("webhook: insert captured request: %w", err)
	}

	// Evict everything past the cap in one shot: keep the newest reqCap rows by
	// seq, delete the rest. The deleted rows' blob references are left for the
	// ADR-0008 refcounted GC reaper (SPEC-0005 "the oldest record MUST be
	// evicted and its body blob dereferenced (subject to ADR-0008 refcounted
	// GC)") — dropping this row IS the dereference; the reaper reclaims the
	// bytes once nothing else points at them.
	if _, err := tx.Exec(ctx, `
		DELETE FROM hook_requests
		WHERE hook_id = $1 AND seq <= (
			SELECT seq FROM hook_requests WHERE hook_id = $1 ORDER BY seq DESC OFFSET $2 LIMIT 1
		)`, hookID, reqCap,
	); err != nil {
		return nil, fmt.Errorf("webhook: evict overflow: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("webhook: commit: %w", err)
	}

	req := &Request{
		Seq: seq, ReceivedAt: receivedAt, Method: in.Method, Path: in.Path, Query: in.Query,
		Headers: headers, Status: in.Status, ContentType: in.ContentType, BodySize: d.size,
	}
	if d.inline != nil {
		req.Inline = d.inline
	}
	if d.refSHA() != "" {
		req.Ref = &BodyRef{SHA256: d.refSHA(), Size: d.size, Truncated: d.truncated}
	}

	// Fan the durably-committed capture out to live subscribers (browsers over
	// SSE, agents over MCP) in the same seq order the rows now carry —
	// published only after commit so a subscriber never sees a request a
	// rolled-back tx would erase (SPEC-0005 "One Stream, Two Transports (Live
	// Fan-out)": "Capture MUST write the record, then fan the new seq out to
	// live subscribers").
	s.hub.publish(publicID, StreamEvent{Type: EventRequest, Seq: seq, Request: req})
	return req, nil
}

// insertEndpointArtifact mints the webhook artifact envelope (body-less, like
// a bundle or trajectory run), retrying id generation on the rare public_id
// collision via a savepoint — verbatim the trajectory insertRunArtifact
// pattern (SPEC-0002 REQ "Short Opaque Public Identifiers").
func (s *Service) insertEndpointArtifact(ctx context.Context, tx pgx.Tx, in EndpointInput) (int64, string, error) {
	art := &artifact.Artifact{
		ShareType:   artifact.TypeWebhook,
		Title:       in.Title,
		MediaType:   mediaTypeWebhook,
		Previewable: false,
		Provenance:  in.Provenance,
		Access:      in.Access,
		ExpiresAt:   in.ExpiresAt,
	}
	const insertSQL = `
		INSERT INTO artifacts
			(public_id, share_type, title, body_sha256, size_bytes, media_type,
			 previewable, created_by_user_id, on_behalf_of, channel, captured_at,
			 owner_user_id, owner_team_id, visibility, expires_at)
		VALUES ($1,$2,$3,NULL,0,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		RETURNING id`

	for attempt := 0; attempt < idMaxAttempts; attempt++ {
		pid, err := s.newID()
		if err != nil {
			return 0, "", fmt.Errorf("webhook: generate id: %w", err)
		}
		art.PublicID = pid
		if err := art.Validate(); err != nil {
			return 0, "", err // invariant violation, identical every attempt
		}
		sp, err := tx.Begin(ctx)
		if err != nil {
			return 0, "", fmt.Errorf("webhook: savepoint: %w", err)
		}
		var artID int64
		err = sp.QueryRow(ctx, insertSQL,
			art.PublicID, string(art.ShareType), art.Title, art.MediaType,
			art.Previewable, user.IDParam(art.Provenance.CreatedByUserID), art.Provenance.OnBehalfOf,
			string(art.Provenance.Channel), art.Provenance.CapturedAt,
			user.IDParam(art.Access.OwnerUserID), user.IDParam(art.Access.OwnerTeamID),
			string(art.Access.Visibility), art.ExpiresAt,
		).Scan(&artID)
		if err != nil {
			_ = sp.Rollback(ctx)
			if isPublicIDConflict(err) {
				continue
			}
			return 0, "", fmt.Errorf("webhook: insert endpoint artifact: %w", err)
		}
		if err := sp.Commit(ctx); err != nil {
			return 0, "", fmt.Errorf("webhook: release savepoint: %w", err)
		}
		return artID, art.PublicID, nil
	}
	return 0, "", fmt.Errorf("webhook: exhausted %d id attempts: %w", idMaxAttempts, errs.ErrConflict)
}

// isPublicIDConflict reports whether err is a unique-violation on public_id.
func isPublicIDConflict(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505" && pgErr.ConstraintName == "artifacts_public_id_key"
	}
	return false
}

func nullString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
