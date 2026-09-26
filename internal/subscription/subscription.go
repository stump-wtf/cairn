// Package subscription implements owned outbound subscriptions: the only
// delivery targets for Cairn's outbound events. Each subscription belongs to
// one user or one team, names a target URL and a signing secret, optionally
// filters by event type, share type and tag, and records its delivery health.
// It replaces the instance-wide outbound target list and its shared secret,
// which were removed in the same change rather than retired in stages.
//
// This package owns the rows, the sealed secrets, the per-owner ceilings and
// the target-safety policy. Who may act for an owner is decided by the
// transport (internal/httpapi); delivery is internal/outboundhook's, which
// asks Targets for the owning workspace's matching subscriptions and reports
// each outcome to Record.
//
// Governing: ADR-0029 (Teams and Tenancy, section 6), SPEC-0023 REQ "Owned
// Outbound Subscriptions", REQ "Events Go Only to the Artifact's Workspace",
// REQ "Subscription Target Safety", REQ "Removing the Instance-Wide Outbound
// Targets"
package subscription

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/sharetype"
	"github.com/stump-wtf/cairn/internal/user"
)

// Defaults and bounds.
const (
	// DefaultPerUser and DefaultPerTeam are the per-owner ceilings
	// (CAIRN_SUBSCRIPTIONS_PER_USER / _PER_TEAM).
	DefaultPerUser = 5
	DefaultPerTeam = 10
	// DisableAfter consecutive failed deliveries disable a subscription.
	DisableAfter = 20
	// MinSecretBytes is the shortest secret a creator may supply.
	MinSecretBytes = 32
	// maxSecretBytes bounds a supplied secret.
	maxSecretBytes = 512
	// maxShareTypes bounds the share-type filter.
	maxShareTypes = 32
	// mintedPrefix marks a secret Cairn minted.
	mintedPrefix = "whsec_"
)

// DisabledReason is what a subscription says once the worker disables it.
var DisabledReason = "disabled after " + strconv.Itoa(DisableAfter) + " consecutive failed deliveries"

// EventTypes is the closed vocabulary an event-type filter may name: the
// SPEC-0016 EV-1 registry. Only artifact.created is emitted today; the
// annotation, trace and retention kinds are delivered through this same
// filter once their emitters land (#305, #311, #313). When internal/event
// lands, this list is replaced by event.RegisteredKinds().
var EventTypes = []string{
	"artifact.created",
	"comment.created",
	"reaction.added",
	"reaction.removed",
	"run.closed",
	"artifact.retained",
	"artifact.released",
	"artifact.deleted",
}

// ErrUnavailable is returned by Create and Rotate when the server has no
// CAIRN_ENCRYPTION_KEY, so a secret could not be stored encrypted.
var ErrUnavailable = errs.New(errs.CodeConflict, "subscriptions are unavailable: the server has no "+KeyEnv)

// ErrCeiling is returned by Create when the owner already has as many
// subscriptions as the ceiling allows.
var ErrCeiling = errs.New(errs.CodeConflict, "subscription ceiling reached")

// Owner is the workspace a subscription or an artifact belongs to: exactly
// one of UserID and TeamID is set.
type Owner struct {
	UserID string
	TeamID string
}

// Valid reports whether exactly one owner id is set and well formed.
func (o Owner) Valid() bool {
	switch {
	case o.UserID != "" && o.TeamID == "":
		return user.ValidID(o.UserID)
	case o.TeamID != "" && o.UserID == "":
		return user.ValidID(o.TeamID)
	}
	return false
}

// String renders the owner as it appears on the wire: "user:<id>" or
// "team:<id>".
func (o Owner) String() string {
	if o.TeamID != "" {
		return "team:" + o.TeamID
	}
	return "user:" + o.UserID
}

// Subscription is one owned delivery target. The secret is never part of it.
type Subscription struct {
	ID         string
	Owner      Owner
	CreatedBy  string
	URL        string
	EventTypes []string
	ShareTypes []string
	Tags       []string
	// Paused is the owner's switch; DisabledReason is the worker's.
	Paused              bool
	DisabledReason      string
	ConsecutiveFailures int
	LastAttemptAt       *time.Time
	LastStatus          *int
	LastError           string
	CreatedAt           time.Time
}

// Active reports whether the subscription receives deliveries.
func (s *Subscription) Active() bool { return !s.Paused && s.DisabledReason == "" }

// Options configures a Service.
type Options struct {
	// Sealer seals secrets; nil makes Create and Rotate return ErrUnavailable.
	Sealer *Sealer
	// Policy validates targets; nil uses a zero Policy (https only, public
	// addresses only).
	Policy *Policy
	// PerUser and PerTeam are the ceilings; zero or less uses the defaults.
	PerUser int
	PerTeam int
	// Registry validates share-type filters; nil uses sharetype.Default().
	Registry *sharetype.Registry
}

// Service manages subscriptions over the shared Postgres pool.
type Service struct {
	pool    *pgxpool.Pool
	sealer  *Sealer
	policy  *Policy
	perUser int
	perTeam int
	reg     *sharetype.Registry
}

// NewService builds a Service.
func NewService(pool *pgxpool.Pool, opts Options) *Service {
	s := &Service{
		pool:    pool,
		sealer:  opts.Sealer,
		policy:  opts.Policy,
		perUser: opts.PerUser,
		perTeam: opts.PerTeam,
		reg:     opts.Registry,
	}
	if s.policy == nil {
		s.policy = &Policy{}
	}
	if s.perUser <= 0 {
		s.perUser = DefaultPerUser
	}
	if s.perTeam <= 0 {
		s.perTeam = DefaultPerTeam
	}
	if s.reg == nil {
		s.reg = sharetype.Default()
	}
	return s
}

// Available reports whether secrets can be sealed, so subscriptions created.
func (s *Service) Available() bool { return s != nil && s.sealer != nil }

// Policy returns the target policy delivery re-checks against.
func (s *Service) Policy() *Policy { return s.policy }

// Ceiling returns the subscription ceiling for owner.
func (s *Service) Ceiling(owner Owner) int {
	if owner.TeamID != "" {
		return s.perTeam
	}
	return s.perUser
}

// CreateInput is a new subscription. Owner authorization is the caller's.
type CreateInput struct {
	Owner      Owner
	CreatedBy  string
	URL        string
	Secret     string // empty: Cairn mints one
	EventTypes []string
	ShareTypes []string
	Tags       []string
}

// Create validates and stores a subscription, returning its secret (the
// supplied one, or one Cairn minted) exactly once.
func (s *Service) Create(ctx context.Context, in CreateInput) (string, *Subscription, error) {
	if !in.Owner.Valid() {
		return "", nil, errs.Validationf("subscriptions: owner is required")
	}
	if !s.Available() {
		return "", nil, ErrUnavailable
	}
	f, err := s.filters(in.EventTypes, in.ShareTypes, in.Tags)
	if err != nil {
		return "", nil, err
	}
	secret, err := secretOrMint(in.Secret)
	if err != nil {
		return "", nil, err
	}
	if err := s.policy.CheckURL(ctx, in.URL); err != nil {
		return "", nil, err
	}
	id := uuid.NewString()
	sealed, err := s.sealer.seal([]byte(secret), id)
	if err != nil {
		return "", nil, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("subscription: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Serialize creates per owner so two concurrent requests cannot both pass
	// the ceiling count.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		"outbound_subscriptions:"+in.Owner.String()); err != nil {
		return "", nil, fmt.Errorf("subscription: lock owner: %w", err)
	}
	var n int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM outbound_subscriptions
		 WHERE owner_user_id IS NOT DISTINCT FROM $1 AND owner_team_id IS NOT DISTINCT FROM $2`,
		user.IDParam(in.Owner.UserID), user.IDParam(in.Owner.TeamID)).Scan(&n); err != nil {
		return "", nil, fmt.Errorf("subscription: count: %w", err)
	}
	if limit := s.Ceiling(in.Owner); n >= limit {
		return "", nil, fmt.Errorf("%w: %d of %d", ErrCeiling, n, limit)
	}
	row := tx.QueryRow(ctx, `
		INSERT INTO outbound_subscriptions
			(id, owner_user_id, owner_team_id, created_by_user_id, url, secret_enc,
			 event_types, share_types, tags)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING `+columns,
		id, user.IDParam(in.Owner.UserID), user.IDParam(in.Owner.TeamID), user.IDParam(in.CreatedBy),
		in.URL, sealed, f.eventTypes, f.shareTypes, f.tags)
	sub, err := scan(row)
	if err != nil {
		return "", nil, fmt.Errorf("subscription: insert: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", nil, fmt.Errorf("subscription: commit: %w", err)
	}
	return secret, sub, nil
}

// List returns owner's subscriptions, oldest first.
func (s *Service) List(ctx context.Context, owner Owner) ([]*Subscription, error) {
	if !owner.Valid() {
		return nil, errs.Validationf("subscriptions: owner is required")
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+columns+` FROM outbound_subscriptions
		 WHERE owner_user_id IS NOT DISTINCT FROM $1 AND owner_team_id IS NOT DISTINCT FROM $2
		 ORDER BY created_at, id`,
		user.IDParam(owner.UserID), user.IDParam(owner.TeamID))
	if err != nil {
		return nil, fmt.Errorf("subscription: list: %w", err)
	}
	defer rows.Close()
	var out []*Subscription
	for rows.Next() {
		sub, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("subscription: list: %w", err)
		}
		out = append(out, sub)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("subscription: list: %w", err)
	}
	return out, nil
}

// Get returns one of owner's subscriptions. Any other id, including one
// another owner holds, is errs.ErrNotFound.
func (s *Service) Get(ctx context.Context, owner Owner, id string) (*Subscription, error) {
	if !owner.Valid() || !user.ValidID(id) {
		return nil, errs.ErrNotFound
	}
	sub, err := scan(s.pool.QueryRow(ctx, `
		SELECT `+columns+` FROM outbound_subscriptions
		 WHERE id = $1 AND owner_user_id IS NOT DISTINCT FROM $2 AND owner_team_id IS NOT DISTINCT FROM $3`,
		id, user.IDParam(owner.UserID), user.IDParam(owner.TeamID)))
	return found(sub, err)
}

// SetPaused pauses or resumes one of owner's subscriptions. Resuming also
// clears a disable and its failure count: resuming is how an owner says the
// target is fixed.
func (s *Service) SetPaused(ctx context.Context, owner Owner, id string, paused bool) (*Subscription, error) {
	if !owner.Valid() || !user.ValidID(id) {
		return nil, errs.ErrNotFound
	}
	sub, err := scan(s.pool.QueryRow(ctx, `
		UPDATE outbound_subscriptions
		   SET paused = $4,
		       disabled_reason = CASE WHEN $4 THEN disabled_reason END,
		       consecutive_failures = CASE WHEN $4 THEN consecutive_failures ELSE 0 END
		 WHERE id = $1 AND owner_user_id IS NOT DISTINCT FROM $2 AND owner_team_id IS NOT DISTINCT FROM $3
		 RETURNING `+columns,
		id, user.IDParam(owner.UserID), user.IDParam(owner.TeamID), paused))
	return found(sub, err)
}

// Rotate replaces one of owner's subscription secrets with the supplied one
// or a freshly minted one, returned exactly once.
func (s *Service) Rotate(ctx context.Context, owner Owner, id, supplied string) (string, *Subscription, error) {
	if !owner.Valid() || !user.ValidID(id) {
		return "", nil, errs.ErrNotFound
	}
	if !s.Available() {
		return "", nil, ErrUnavailable
	}
	secret, err := secretOrMint(supplied)
	if err != nil {
		return "", nil, err
	}
	sealed, err := s.sealer.seal([]byte(secret), id)
	if err != nil {
		return "", nil, err
	}
	sub, err := found(scan(s.pool.QueryRow(ctx, `
		UPDATE outbound_subscriptions SET secret_enc = $4
		 WHERE id = $1 AND owner_user_id IS NOT DISTINCT FROM $2 AND owner_team_id IS NOT DISTINCT FROM $3
		 RETURNING `+columns,
		id, user.IDParam(owner.UserID), user.IDParam(owner.TeamID), sealed)))
	if err != nil {
		return "", nil, err
	}
	return secret, sub, nil
}

// Delete removes one of owner's subscriptions.
func (s *Service) Delete(ctx context.Context, owner Owner, id string) error {
	if !owner.Valid() || !user.ValidID(id) {
		return errs.ErrNotFound
	}
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM outbound_subscriptions
		 WHERE id = $1 AND owner_user_id IS NOT DISTINCT FROM $2 AND owner_team_id IS NOT DISTINCT FROM $3`,
		id, user.IDParam(owner.UserID), user.IDParam(owner.TeamID))
	if err != nil {
		return fmt.Errorf("subscription: delete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return errs.ErrNotFound
	}
	return nil
}

// filterSet is a validated, deduplicated filter triple.
type filterSet struct {
	eventTypes, shareTypes, tags []string
}

func (s *Service) filters(eventTypes, shareTypes, tags []string) (filterSet, error) {
	var f filterSet
	for _, k := range eventTypes {
		if !slices.Contains(EventTypes, k) {
			return f, errs.Validationf("subscriptions: unknown event type %q", truncate(k))
		}
		if !slices.Contains(f.eventTypes, k) {
			f.eventTypes = append(f.eventTypes, k)
		}
	}
	if len(shareTypes) > maxShareTypes {
		return f, errs.Validationf("subscriptions: at most %d share types", maxShareTypes)
	}
	for _, st := range shareTypes {
		if !s.reg.Registered(artifact.ShareType(st)) {
			return f, errs.Validationf("subscriptions: unknown share type %q", truncate(st))
		}
		if !slices.Contains(f.shareTypes, st) {
			f.shareTypes = append(f.shareTypes, st)
		}
	}
	norm, err := artifact.NormalizeTags(tags)
	if err != nil {
		return f, err
	}
	f.tags = norm
	if f.eventTypes == nil {
		f.eventTypes = []string{}
	}
	if f.shareTypes == nil {
		f.shareTypes = []string{}
	}
	if f.tags == nil {
		f.tags = []string{}
	}
	return f, nil
}

// truncate keeps a refused filter value short in an error message.
func truncate(v string) string {
	if len(v) > 64 {
		return v[:64]
	}
	return v
}

// secretOrMint validates a supplied secret, or mints one when none is given.
// A supplied secret is used byte for byte as the HMAC key, so it must be
// what the receiver holds: at least MinSecretBytes of printable, non-space
// ASCII (a receiver-issued secret such as a Switchboard webhook's
// signing_secret). The error never quotes it.
func secretOrMint(supplied string) (string, error) {
	if supplied == "" {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return "", fmt.Errorf("subscription: mint secret: %w", err)
		}
		return mintedPrefix + base64.RawURLEncoding.EncodeToString(b), nil
	}
	if len(supplied) < MinSecretBytes || len(supplied) > maxSecretBytes {
		return "", errs.Validationf("subscriptions: a supplied secret must be %d to %d bytes", MinSecretBytes, maxSecretBytes)
	}
	for i := 0; i < len(supplied); i++ {
		if c := supplied[i]; c < 0x21 || c > 0x7e {
			return "", errs.Validationf("subscriptions: a supplied secret must be printable ASCII without spaces")
		}
	}
	return supplied, nil
}

// columns is every column scan reads, in order.
const columns = `id::text, COALESCE(owner_user_id::text, ''), COALESCE(owner_team_id::text, ''),
	COALESCE(created_by_user_id::text, ''), url, event_types, share_types, tags, paused,
	COALESCE(disabled_reason, ''), consecutive_failures, last_attempt_at, last_status,
	COALESCE(last_error, ''), created_at`

func scan(row pgx.Row) (*Subscription, error) {
	var s Subscription
	if err := row.Scan(&s.ID, &s.Owner.UserID, &s.Owner.TeamID, &s.CreatedBy, &s.URL,
		&s.EventTypes, &s.ShareTypes, &s.Tags, &s.Paused, &s.DisabledReason,
		&s.ConsecutiveFailures, &s.LastAttemptAt, &s.LastStatus, &s.LastError, &s.CreatedAt); err != nil {
		return nil, err
	}
	return &s, nil
}

func found(sub *Subscription, err error) (*Subscription, error) {
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errs.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("subscription: %w", err)
	}
	return sub, nil
}
