package operator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/user"
)

// Audit actions. They are the stable vocabulary of operator_audit.action.
const (
	ActionSuspendUser   = "user.suspend"
	ActionUnsuspendUser = "user.unsuspend"
)

// MaxReasonRunes caps an audit reason. The reason is required (the CHECK in
// 0019) and shown to the affected user, so it is prose, not a document.
const MaxReasonRunes = 1000

// MaxDirectoryPage caps one directory page.
const MaxDirectoryPage = 500

// Refusals SetSuspended distinguishes, each a validation failure.
var (
	ErrReason    = errs.New(errs.CodeValidation, "operator: a reason of 1 to 1000 characters is required")
	ErrSelf      = errs.New(errs.CodeValidation, "operator: an operator cannot suspend themselves")
	ErrProtected = errs.New(errs.CodeValidation, "operator: a CAIRN_OPERATORS identity cannot be suspended; remove it from the list first")
)

// Service is the Postgres-backed operator core: the directory, suspension,
// and the audit trail both write and read.
type Service struct {
	pool *pgxpool.Pool
	set  *Set
}

// NewService builds the operator core over the shared pool and the
// configured profile.
func NewService(pool *pgxpool.Pool, set *Set) *Service {
	return &Service{pool: pool, set: set}
}

// DirectoryUser is one user as the operator sees them: identity and counts,
// never content. Actor is the user as rendered on the wire (their verified
// email, else their actor key); the operator needs it to know whom they are
// acting on.
type DirectoryUser struct {
	ID          string
	Handle      string
	Actor       string
	CreatedAt   time.Time
	LastSeenAt  *time.Time
	SuspendedAt *time.Time
	// Operator reports that one of the user's sign-in identities is listed in
	// CAIRN_OPERATORS. (A CAIRN_OPERATOR_GROUP operator is one by session,
	// not by identity, and is not flagged here.)
	Operator  bool
	Artifacts int64
	Bytes     int64
}

// Directory returns one page of users, oldest first, with the count and
// total size of the artifacts each owns. Nothing about an individual
// artifact is read (SPEC-0023 REQ "Operator Surfaces Bound Tenant Data and
// Never Read It").
func (s *Service) Directory(ctx context.Context, limit, offset int) ([]DirectoryUser, error) {
	if limit <= 0 || limit > MaxDirectoryPage {
		limit = MaxDirectoryPage
	}
	if offset < 0 {
		offset = 0
	}
	issuers, subjects := s.set.identities()
	rows, err := s.pool.Query(ctx, `
		SELECT u.id::text, u.display_handle, `+user.ActorSQL("u")+`, u.created_at,
		       (SELECT max(i.last_seen_at) FROM user_identities i WHERE i.user_id = u.id),
		       u.suspended_at,
		       EXISTS (SELECT 1 FROM user_identities i
		                 JOIN unnest($3::text[], $4::text[]) AS o(issuer, subject)
		                   ON o.issuer = i.issuer AND o.subject = i.subject
		                WHERE i.user_id = u.id),
		       COALESCE(a.n, 0), COALESCE(a.bytes, 0)
		  FROM users u
		  LEFT JOIN (SELECT owner_user_id, count(*) AS n, sum(size_bytes)::bigint AS bytes
		               FROM artifacts
		              WHERE owner_user_id IS NOT NULL
		              GROUP BY owner_user_id) a ON a.owner_user_id = u.id
		 ORDER BY u.created_at, u.id
		 LIMIT $1 OFFSET $2`,
		limit, offset, issuers, subjects)
	if err != nil {
		return nil, fmt.Errorf("operator: directory: %w", err)
	}
	defer rows.Close()
	var out []DirectoryUser
	for rows.Next() {
		var d DirectoryUser
		if err := rows.Scan(&d.ID, &d.Handle, &d.Actor, &d.CreatedAt, &d.LastSeenAt,
			&d.SuspendedAt, &d.Operator, &d.Artifacts, &d.Bytes); err != nil {
			return nil, fmt.Errorf("operator: scan directory: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// IsOperatorUser reports whether one of the user's sign-in identities is
// listed in CAIRN_OPERATORS: the operator check for a caller with no session
// to read a groups claim from.
func (s *Service) IsOperatorUser(ctx context.Context, userID string) (bool, error) {
	if !user.ValidID(userID) || s.set.Len() == 0 {
		return false, nil
	}
	return s.isListedUser(ctx, s.pool, userID)
}

type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (s *Service) isListedUser(ctx context.Context, q querier, userID string) (bool, error) {
	issuers, subjects := s.set.identities()
	var listed bool
	err := q.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM user_identities i
		                 JOIN unnest($2::text[], $3::text[]) AS o(issuer, subject)
		                   ON o.issuer = i.issuer AND o.subject = i.subject
		                WHERE i.user_id = $1)`,
		userID, issuers, subjects).Scan(&listed)
	if err != nil {
		return false, fmt.Errorf("operator: match identities: %w", err)
	}
	return listed, nil
}

// SuspensionInput is one suspend or unsuspend action.
type SuspensionInput struct {
	// OperatorID is the acting operator's user id.
	OperatorID string
	// UserID is the user acted on.
	UserID string
	// Suspend is true to suspend, false to reinstate.
	Suspend bool
	// Reason is required and shown to the affected user.
	Reason string
}

// Suspension is the outcome of a suspend or unsuspend: the user's state
// after it, what suspension revoked, and the audit row it wrote.
type Suspension struct {
	UserID      string
	SuspendedAt *time.Time
	Revoked     Revoked
	AuditID     int64
}

// Revoked counts the credentials a suspension ended. All zero on
// reinstatement: nothing is restored, the user signs in again.
type Revoked struct {
	Sessions            int64 `json:"sessions"`
	PersonalTokens      int64 `json:"personal_access_tokens"`
	OAuthGrants         int64 `json:"oauth_grants"`
	OAuthTokens         int64 `json:"oauth_tokens"`
	OAuthAuthorizations int64 `json:"oauth_authorization_codes"`
}

// SetSuspended suspends or reinstates a user, and writes its audit row, in
// one transaction (SPEC-0023 REQ "Database Operation Standards").
//
// Suspension offboards: it deletes the user's sessions and revokes their
// personal access tokens, OAuth grants with every access and refresh token
// in them, and unredeemed authorization codes, so none authenticates on the
// next request (SPEC-0023 "Suspending a user offboards them"). Each
// authenticator also refuses a suspended user outright, so a credential that
// raced the revocation, or a new sign-in, still fails. Reinstating clears
// the flag and restores nothing.
//
// An operator cannot suspend themselves, and a user with a CAIRN_OPERATORS
// identity cannot be suspended from the console: the list is the operator's
// to edit, and suspending a configured operator would only lock them out of
// the instance they run.
func (s *Service) SetSuspended(ctx context.Context, in SuspensionInput) (*Suspension, error) {
	reason := strings.TrimSpace(in.Reason)
	if reason == "" || utf8.RuneCountInString(reason) > MaxReasonRunes {
		return nil, ErrReason
	}
	if !user.ValidID(in.OperatorID) {
		return nil, errs.ErrForbidden
	}
	if !user.ValidID(in.UserID) {
		return nil, fmt.Errorf("user %q: %w", in.UserID, errs.ErrNotFound)
	}
	if in.Suspend && in.UserID == in.OperatorID {
		return nil, ErrSelf
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("operator: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var current *time.Time
	err = tx.QueryRow(ctx, `SELECT suspended_at FROM users WHERE id = $1 FOR UPDATE`, in.UserID).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("user %s: %w", in.UserID, errs.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("operator: lock user: %w", err)
	}

	out := &Suspension{UserID: in.UserID}
	action := ActionUnsuspendUser
	if in.Suspend {
		action = ActionSuspendUser
		if current != nil {
			return nil, fmt.Errorf("user %s is already suspended: %w", in.UserID, errs.ErrConflict)
		}
		listed, err := s.isListedUser(ctx, tx, in.UserID)
		if err != nil {
			return nil, err
		}
		if listed {
			return nil, ErrProtected
		}
		if err := tx.QueryRow(ctx,
			`UPDATE users SET suspended_at = now() WHERE id = $1 RETURNING suspended_at`,
			in.UserID).Scan(&out.SuspendedAt); err != nil {
			return nil, fmt.Errorf("operator: suspend: %w", err)
		}
		if out.Revoked, err = offboard(ctx, tx, in.UserID); err != nil {
			return nil, err
		}
	} else {
		if current == nil {
			return nil, fmt.Errorf("user %s is not suspended: %w", in.UserID, errs.ErrConflict)
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET suspended_at = NULL WHERE id = $1`, in.UserID); err != nil {
			return nil, fmt.Errorf("operator: reinstate: %w", err)
		}
	}

	var detail any = map[string]any{}
	if in.Suspend {
		detail = map[string]any{"revoked": out.Revoked}
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		return nil, fmt.Errorf("operator: encode audit detail: %w", err)
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO operator_audit (operator_id, target_user, action, reason, detail)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id`,
		in.OperatorID, in.UserID, action, reason, raw).Scan(&out.AuditID); err != nil {
		return nil, fmt.Errorf("operator: write audit: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("operator: commit: %w", err)
	}
	return out, nil
}

// offboard ends every credential a user holds, inside the suspension's
// transaction.
func offboard(ctx context.Context, tx pgx.Tx, userID string) (Revoked, error) {
	var r Revoked
	steps := []struct {
		n   *int64
		sql string
	}{
		{&r.Sessions, `DELETE FROM sessions WHERE user_id = $1`},
		{&r.PersonalTokens, `UPDATE personal_access_tokens SET revoked_at = now()
			WHERE user_id = $1 AND revoked_at IS NULL`},
		// Tokens before grants: the subquery must still see the grants.
		{&r.OAuthTokens, `UPDATE oauth_tokens SET revoked_at = now()
			WHERE revoked_at IS NULL
			  AND grant_id IN (SELECT grant_id FROM oauth_grants WHERE user_id = $1)`},
		{&r.OAuthGrants, `UPDATE oauth_grants SET revoked_at = now()
			WHERE user_id = $1 AND revoked_at IS NULL`},
		{&r.OAuthAuthorizations, `DELETE FROM oauth_auth_codes
			WHERE user_id = $1 AND redeemed_at IS NULL`},
	}
	for _, st := range steps {
		tag, err := tx.Exec(ctx, st.sql, userID)
		if err != nil {
			return r, fmt.Errorf("operator: offboard: %w", err)
		}
		*st.n = tag.RowsAffected()
	}
	return r, nil
}

// AuditEntry is one operator_audit row, with the users it names rendered as
// display handles (never emails), so the same row can be shown to the
// affected user.
type AuditEntry struct {
	ID             int64
	OperatorHandle string
	TargetUserID   string
	TargetHandle   string
	Action         string
	Reason         string
	Detail         json.RawMessage
	At             time.Time
}

const auditSelect = `
	SELECT a.id, COALESCE(o.display_handle, ''), COALESCE(a.target_user::text, ''),
	       COALESCE(t.display_handle, ''), a.action, a.reason, a.detail, a.at
	  FROM operator_audit a
	  LEFT JOIN users o ON o.id = a.operator_id
	  LEFT JOIN users t ON t.id = a.target_user`

// AuditFor returns the audit rows that name userID as their target, newest
// first: what the affected user reads (SPEC-0023: "readable by the affected
// user").
func (s *Service) AuditFor(ctx context.Context, userID string, limit int) ([]AuditEntry, error) {
	if !user.ValidID(userID) {
		return nil, nil
	}
	return s.queryAudit(ctx, auditSelect+` WHERE a.target_user = $1 ORDER BY a.at DESC, a.id DESC LIMIT $2`,
		userID, clampLimit(limit))
}

// RecentAudit returns the newest audit rows across the instance, for the
// operator console.
func (s *Service) RecentAudit(ctx context.Context, limit int) ([]AuditEntry, error) {
	return s.queryAudit(ctx, auditSelect+` ORDER BY a.at DESC, a.id DESC LIMIT $1`, clampLimit(limit))
}

func (s *Service) queryAudit(ctx context.Context, sql string, args ...any) ([]AuditEntry, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("operator: audit: %w", err)
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.OperatorHandle, &e.TargetUserID, &e.TargetHandle,
			&e.Action, &e.Reason, &e.Detail, &e.At); err != nil {
			return nil, fmt.Errorf("operator: scan audit: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func clampLimit(limit int) int {
	if limit <= 0 || limit > 200 {
		return 200
	}
	return limit
}
