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
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/event"
	"github.com/stump-wtf/cairn/internal/id"
	"github.com/stump-wtf/cairn/internal/objectstore"
	"github.com/stump-wtf/cairn/internal/sharetype"
)

// idMaxAttempts bounds public-id collision retries; at the targeted keyspace
// occupancy a single retry is already astronomically unlikely.
const idMaxAttempts = 5

// retiredIDGrace bounds how long a retired public id (rotated away by
// Store.RotateID) stays excluded from re-minting (ADR-0005 "a retired id is
// not reused within TTL-plus-grace", SPEC-0009 REQ "Id Rotation as
// Revoke-a-Leaked-Link"). It is set generously longer than the platform's
// maximum permitted artifact TTL (httpapi.Config.MaxRequestedTTL defaults to
// 30 days) with margin, mirroring the same "longer than max TTL" posture the
// object-storage lifecycle backstop uses (SPEC-0009 REQ "Object-Storage
// Lifecycle Backstop") so a rotated id can never be re-minted while any
// plausible artifact created before the rotation could still be alive and
// confusable with it.
const retiredIDGrace = 60 * 24 * time.Hour

// defaultPreviewMaxBytes is the fallback preview size bound when none is
// configured: bodies larger than this are never previewable (SPEC-0002).
const defaultPreviewMaxBytes = 5 << 20

// Store is the Artifact core service.
type Store struct {
	pool       *pgxpool.Pool
	obj        objectstore.ObjectStore
	maxBytes   int64
	previewMax int64
	registry   *sharetype.Registry
	newID      func() (string, error)
	emitter    CreationEmitter
}

// CreationEvent is the transport-agnostic fact that an artifact came into
// existence, handed to a CreationEmitter after the creating transaction
// commits. WebPath is the registry-derived public web path (origin-agnostic:
// the emitter joins its configured base URL onto it).
//
// Governing: ADR-0017 (Outbound Webhooks), SPEC-0012 REQ "Event Payload"
type CreationEvent struct {
	PublicID  string
	ShareType artifact.ShareType
	Title     string
	WebPath   string
	ActorID   string
	Model     string
	Channel   string
	ExpiresAt time.Time
	CreatedAt time.Time
	// OnBehalfOf is the MCP client's self-reported name/version, recorded from
	// the session handshake (empty for REST/CLI). It names the harness, not a
	// principal: ActorID is the authenticated identity.
	OnBehalfOf string
	// Tags are client-asserted (ADR-0018): carried so a consumer can route on
	// them, never so it can trust them.
	Tags []string
	// ActorKind and Auth are the creator's server-derived credential class
	// (ADR-0022, SPEC-0016 EV-4).
	ActorKind event.ActorKind
	Auth      event.AuthMethod
	// OwnerID is the artifact's owner, carried so owned subscriptions can be
	// selected without a lookup (ADR-0029). It is never put on the wire
	// (SPEC-0016 EV-7).
	OwnerID string
}

// Event is the artifact.created lifecycle event this creation announces. It is
// what makes CreationEmitter a thin adapter over event.Emitter: an
// implementation forwards Emit(ev.Event()), so the creation path and every
// other kind share one encoder.
//
// Governing: ADR-0022, SPEC-0016 EV-1 "Event Kind Registry", EV-3 "Payload
// Shape"
func (c CreationEvent) Event() event.Event {
	return event.Event{
		Kind: event.ArtifactCreated,
		Subject: event.Subject{
			PublicID:  c.PublicID,
			ShareType: c.ShareType,
			Title:     c.Title,
			WebPath:   c.WebPath,
			Tags:      c.Tags,
			ExpiresAt: c.ExpiresAt,
			OwnerID:   c.OwnerID,
		},
		Actor: event.Actor{
			ID:         c.ActorID,
			Channel:    artifact.Channel(c.Channel),
			OnBehalfOf: c.OnBehalfOf,
			Kind:       c.ActorKind,
			Auth:       c.Auth,
		},
		Model: c.Model,
	}
}

// CreationEmitter receives post-commit creation events. Implementations MUST
// be safe for concurrent use and MUST NOT block or panic the caller: an emit
// failure is the emitter's problem, never the create request's
// (SPEC-0012 REQ "Event Emission on Artifact Creation"). The outbound emitter
// implements it by forwarding CreationEvent.Event to event.Emitter.
type CreationEmitter interface {
	EmitArtifactCreated(CreationEvent)
}

// Options configures a Store. Zero values fall back to safe defaults.
type Options struct {
	// MaxUploadBytes caps a single streamed body (default 64 MiB).
	MaxUploadBytes int64
	// PreviewMaxBytes is the size bound above which a body is never previewable
	// and is stored as the generic file type (default 5 MiB). SPEC-0002 REQ
	// "Previewability Detection at Ingest".
	PreviewMaxBytes int64
	// Registry resolves share types to their affordances (previewability,
	// anchors). Defaults to the process-wide sharetype.Default() registry.
	Registry *sharetype.Registry
	// NewID overrides public-id generation; tests inject forced collisions.
	// Defaults to id.New.
	NewID func() (string, error)
	// Emitter, when non-nil, receives a CreationEvent after every durable
	// artifact creation (single-body and bundle), covering every surface
	// (REST/web/CLI/MCP) at this single choke point. Nil = inert
	// (SPEC-0012 REQ "Delivery Targets from Configuration").
	Emitter CreationEmitter
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
	previewMax := opts.PreviewMaxBytes
	if previewMax <= 0 {
		previewMax = defaultPreviewMaxBytes
	}
	registry := opts.Registry
	if registry == nil {
		registry = sharetype.Default()
	}
	return &Store{
		pool:       pool,
		obj:        obj,
		maxBytes:   maxBytes,
		previewMax: previewMax,
		registry:   registry,
		newID:      newID,
		emitter:    opts.Emitter,
	}
}

// emitCreated hands a committed artifact to the emitter, if one is installed.
// Best-effort by contract: a nil emitter or a failing one never influences the
// create result (SPEC-0012 REQ "Emitter failure is isolated").
//
// Governing: ADR-0017 (Outbound Webhooks), SPEC-0012 REQ "Event Emission on
// Artifact Creation"
func (s *Store) emitCreated(a *artifact.Artifact, kind event.ActorKind, auth event.AuthMethod) {
	if s.emitter == nil {
		return
	}
	s.emitter.EmitArtifactCreated(CreationEvent{
		PublicID:   a.PublicID,
		ShareType:  a.ShareType,
		Title:      a.Title,
		WebPath:    "/" + joinPath(s.registry.URLPrefixFor(a.ShareType).Web, a.PublicID),
		ActorID:    a.Provenance.ActorID,
		Model:      a.Provenance.Model,
		Channel:    string(a.Provenance.Channel),
		ExpiresAt:  a.ExpiresAt,
		CreatedAt:  a.CreatedAt,
		OnBehalfOf: a.Provenance.OnBehalfOf,
		// Cloned so the emitter never shares a backing array with the
		// artifact the create call hands back to its caller.
		Tags:      slices.Clone(a.Tags),
		ActorKind: kind,
		Auth:      auth,
		OwnerID:   a.Access.OwnerID,
	})
}

// joinPath joins an optional single-segment prefix and an id without a slash
// between empty prefix and id (mirrors httpapi.prefixedPath).
func joinPath(prefix, id string) string {
	if prefix != "" {
		return prefix + "/" + id
	}
	return id
}

// Pool exposes the underlying Postgres pool so a peer core service that shares
// this store's database — notably the annotation service, which the REST/MCP/CLI
// adapters project alongside the artifact core — can be constructed over the
// same connection pool and transaction domain (ADR-0012 one binary, one core).
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Registry returns the share-type registry this store resolves affordances
// through, so a co-constructed peer service gates anchors against the same
// capability matrix (ADR-0002).
func (s *Store) Registry() *sharetype.Registry { return s.registry }

// ObjectStore exposes the content-addressed object store so a peer core service
// that spills its own large payloads to the same blob registry — notably the
// trajectory service, which spills oversized span outputs (SPEC-0004) — writes
// them through the same backend the artifact bodies use (ADR-0008 one content
// store). The REST/MCP/CLI adapters co-construct that peer over this store.
func (s *Store) ObjectStore() objectstore.ObjectStore { return s.obj }
