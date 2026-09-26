package subscription

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/stump-wtf/cairn/internal/user"
)

// Match is what a subscription's filters are tested against: the event's
// kind, and the subject artifact's share type and tags.
type Match struct {
	Kind      string
	ShareType string
	Tags      []string
}

// Target is one subscription an event is to be delivered to, with its
// opened secret. It is never logged.
type Target struct {
	ID     string
	URL    string
	Secret []byte
}

// Failure reasons recorded in last_error. The vocabulary is fixed so the
// column can never carry a URL, a host or a response body.
const (
	ReasonBlockedAddress = "blocked_address"
	ReasonTargetRefused  = "target_refused"
	ReasonRedirect       = "redirect"
	ReasonHTTPStatus     = "http_status"
	ReasonTimeout        = "timeout"
	ReasonConnection     = "connection_failed"
)

// Result is the outcome of one delivery to one subscription, after retries.
type Result struct {
	OK bool
	// Status is the last HTTP status received, 0 when none was.
	Status int
	// Reason is one of the Reason constants on failure, "" on success.
	Reason string
}

// Targets returns the active subscriptions of the workspace that owns the
// event's subject whose filters admit it, with their secrets opened. It is
// the only way an event finds a target: there is no instance-wide list, and
// the actor's own subscriptions are never consulted unless the actor's
// workspace is the owner (SPEC-0023 REQ "Events Go Only to the Artifact's
// Workspace"). An empty filter admits everything; a tag filter admits an
// event carrying any one of its tags. A suspended owner receives nothing.
//
// A subscription whose secret does not open (the key was changed or
// removed) is skipped and reported in skipped, never delivered unsigned.
func (s *Service) Targets(ctx context.Context, owner Owner, m Match) (targets []Target, skipped int, err error) {
	if s == nil || !owner.Valid() {
		return nil, 0, nil
	}
	tags := m.Tags
	if tags == nil {
		tags = []string{}
	}
	rows, err := s.pool.Query(ctx, `
		SELECT o.id::text, o.url, o.secret_enc
		  FROM outbound_subscriptions o
		  LEFT JOIN users u ON u.id = o.owner_user_id
		 WHERE o.owner_user_id IS NOT DISTINCT FROM $1
		   AND o.owner_team_id IS NOT DISTINCT FROM $2
		   AND NOT o.paused AND o.disabled_reason IS NULL
		   AND (o.owner_user_id IS NULL OR u.suspended_at IS NULL)
		   AND (cardinality(o.event_types) = 0 OR $3 = ANY (o.event_types))
		   AND (cardinality(o.share_types) = 0 OR $4 = ANY (o.share_types))
		   AND (cardinality(o.tags) = 0 OR o.tags && $5::text[])
		 ORDER BY o.created_at, o.id`,
		user.IDParam(owner.UserID), user.IDParam(owner.TeamID), m.Kind, m.ShareType, tags)
	if err != nil {
		return nil, 0, fmt.Errorf("subscription: targets: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			t      Target
			sealed []byte
		)
		if err := rows.Scan(&t.ID, &t.URL, &sealed); err != nil {
			return nil, 0, fmt.Errorf("subscription: targets: %w", err)
		}
		if s.sealer == nil {
			skipped++
			continue
		}
		secret, err := s.sealer.open(sealed, t.ID)
		if err != nil {
			skipped++
			continue
		}
		t.Secret = secret
		targets = append(targets, t)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("subscription: targets: %w", err)
	}
	return targets, skipped, nil
}

// Record stores one delivery's outcome: a success resets the failure count,
// a failure increments it, and the DisableAfter-th consecutive failure
// disables the subscription. disabled reports whether this call disabled it.
func (s *Service) Record(ctx context.Context, id string, r Result) (disabled bool, err error) {
	if !user.ValidID(id) {
		return false, nil
	}
	var status any
	if r.Status != 0 {
		status = r.Status
	}
	var reason any
	if !r.OK && r.Reason != "" {
		reason = r.Reason
	}
	var nowDisabled bool
	err = s.pool.QueryRow(ctx, `
		WITH prev AS (
			SELECT id, disabled_reason FROM outbound_subscriptions WHERE id = $1 FOR UPDATE
		)
		UPDATE outbound_subscriptions o
		   SET last_attempt_at = now(),
		       last_status = $2,
		       last_error = $3,
		       consecutive_failures = CASE WHEN $4 THEN 0 ELSE o.consecutive_failures + 1 END,
		       disabled_reason = CASE
		           WHEN NOT $4 AND o.consecutive_failures + 1 >= $5 THEN COALESCE(o.disabled_reason, $6)
		           ELSE o.disabled_reason END
		  FROM prev
		 WHERE o.id = prev.id
		RETURNING prev.disabled_reason IS NULL AND o.disabled_reason IS NOT NULL`,
		id, status, reason, r.OK, DisableAfter, DisabledReason).Scan(&nowDisabled)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Deleted while the delivery was in flight.
			return false, nil
		}
		return false, fmt.Errorf("subscription: record: %w", err)
	}
	return nowDisabled, nil
}
