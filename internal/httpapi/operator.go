package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/operator"
	"github.com/stump-wtf/cairn/internal/user"
)

// The operator surface (ADR-0029, SPEC-0023): a console at /operator and its
// JSON siblings under /v1/operator. It is deliberately small. The directory
// lists users with counts and never an artifact's body, title, link, tags or
// annotations; suspension offboards a user and writes an audit row in the
// same transaction; and nothing here widens what the operator may read.
// Every other surface is unchanged for an operator: their Bin is their own,
// and another user's private artifact is exactly as unreachable to them as
// to anyone else.
//
// Every operator route needs an operator's BROWSER session. A bearer token —
// even one minted by an operator — an anonymous caller, a signed-in user who
// is not an operator, and every caller on an instance with no operator
// configured all get the uniform 404, so the surface does not reveal that it
// exists.
//
// Deferred, with the stories that give them something to act on: team rows
// and team suspension (#328), per-owner quotas (SPEC-0020 permanent
// retention), retention bounds, and the metrics scrape credential
// (SPEC-0014).
//
// Governing: ADR-0029, SPEC-0023 REQ "Operator and User Profiles", REQ
// "Operator Surfaces Bound Tenant Data and Never Read It", REQ "Database
// Operation Standards".

// maxOperatorRequestBytes caps an operator request body (SPEC-0023 Security
// Requirements: 16 KiB).
const maxOperatorRequestBytes = 16 << 10

// operatorConsolePage is how many users one console page lists.
const operatorConsolePage = 200

// operatorPrincipal returns the request's principal when it is an operator's
// browser session on an instance with an operator configured, and false
// otherwise. sessionPrincipal refuses bearer and agent callers.
func (s *Server) operatorPrincipal(r *http.Request) (*Principal, bool) {
	if s.ops == nil || !s.cfg.Operators.Enabled() {
		return nil, false
	}
	p, ok := s.sessionPrincipal(r)
	if !ok || !p.Operator || !user.ValidID(p.UserID) {
		return nil, false
	}
	return p, true
}

// requireOperator guards the /v1/operator routes: an operator's browser
// session, or the uniform JSON 404.
func (s *Server) requireOperator(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.operatorPrincipal(r)
		if !ok {
			s.writeError(w, r, errs.ErrNotFound, nil)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalCtxKey{}, p)))
	})
}

// requireOperatorPage guards the console: an operator's browser session, or
// the uniform HTML 404 (never a login redirect, which would reveal the
// route).
func (s *Server) requireOperatorPage(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.operatorPrincipal(r)
		if !ok {
			s.renderWebError(w, r, errs.ErrNotFound)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalCtxKey{}, p)))
	})
}

// --- JSON ------------------------------------------------------------------

// directoryUserView is one directory row: identity and counts only.
type directoryUserView struct {
	ID          string     `json:"id"`
	Handle      string     `json:"handle"`
	Actor       string     `json:"actor"`
	CreatedAt   time.Time  `json:"created_at"`
	LastSeenAt  *time.Time `json:"last_seen_at,omitempty"`
	SuspendedAt *time.Time `json:"suspended_at,omitempty"`
	Operator    bool       `json:"operator"`
	Artifacts   int64      `json:"artifacts"`
	Bytes       int64      `json:"bytes"`
}

// directoryResponse is GET /v1/operator/directory. Teams is always present
// and empty until teams exist (#328), so a client can rely on the shape.
type directoryResponse struct {
	Users      []directoryUserView `json:"users"`
	Teams      []struct{}          `json:"teams"`
	NextOffset *int                `json:"next_offset,omitempty"`
}

// handleOperatorDirectory lists users with counts, one page at a time
// (?limit, ?offset).
func (s *Server) handleOperatorDirectory(w http.ResponseWriter, r *http.Request) {
	limit, err := queryInt(r, "limit", operator.MaxDirectoryPage)
	if err != nil || limit < 1 || limit > operator.MaxDirectoryPage {
		s.writeError(w, r, errs.Validationf("operator: limit must be 1..%d", operator.MaxDirectoryPage), map[string]string{"field": "limit"})
		return
	}
	offset, err := queryInt(r, "offset", 0)
	if err != nil || offset < 0 {
		s.writeError(w, r, errs.Validationf("operator: offset must be a non-negative integer"), map[string]string{"field": "offset"})
		return
	}
	users, err := s.ops.Directory(r.Context(), limit, offset)
	if err != nil {
		s.writeError(w, r, err, nil)
		return
	}
	out := directoryResponse{Users: make([]directoryUserView, 0, len(users)), Teams: []struct{}{}}
	for _, u := range users {
		out.Users = append(out.Users, directoryUserView(u))
	}
	if len(users) == limit {
		next := offset + limit
		out.NextOffset = &next
	}
	s.writeJSON(w, http.StatusOK, out)
}

// suspensionRequest is POST /v1/operator/suspensions.
type suspensionRequest struct {
	UserID    string `json:"user_id"`
	Suspended *bool  `json:"suspended"`
	Reason    string `json:"reason"`
}

// suspensionResponse reports the user's state after the action, what
// suspension revoked, and the audit row it wrote.
type suspensionResponse struct {
	UserID      string           `json:"user_id"`
	Suspended   bool             `json:"suspended"`
	SuspendedAt *time.Time       `json:"suspended_at,omitempty"`
	Revoked     operator.Revoked `json:"revoked"`
	AuditID     int64            `json:"audit_id"`
}

// handleOperatorSuspension suspends or reinstates a user.
func (s *Server) handleOperatorSuspension(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	r.Body = http.MaxBytesReader(w, r.Body, maxOperatorRequestBytes)
	var req suspensionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			s.writeError(w, r, errs.ErrTooLarge, nil)
			return
		}
		s.writeError(w, r, errs.Validationf("operator: malformed JSON body"), nil)
		return
	}
	if req.Suspended == nil {
		s.writeError(w, r, errs.Validationf("operator: suspended is required"), map[string]string{"field": "suspended"})
		return
	}
	res, err := s.ops.SetSuspended(r.Context(), operator.SuspensionInput{
		OperatorID: p.UserID,
		UserID:     strings.TrimSpace(req.UserID),
		Suspend:    *req.Suspended,
		Reason:     req.Reason,
	})
	if err != nil {
		// The envelope message is the uniform one per code; details names
		// which refusal it was, from the same fixed vocabulary the console
		// uses.
		var details map[string]string
		if code := consoleErrorCode(err); code != "" {
			details = map[string]string{"problem": code}
		}
		s.writeError(w, r, err, details)
		return
	}
	s.logSuspension(r, p, res, *req.Suspended)
	s.writeJSON(w, http.StatusOK, suspensionResponse{
		UserID:      res.UserID,
		Suspended:   res.SuspendedAt != nil,
		SuspendedAt: res.SuspendedAt,
		Revoked:     res.Revoked,
		AuditID:     res.AuditID,
	})
}

// logSuspension records an operator action in the server log: ids and
// counts, never the reason (it is the affected user's to read) and never a
// credential.
func (s *Server) logSuspension(r *http.Request, p *Principal, res *operator.Suspension, suspend bool) {
	action := operator.ActionUnsuspendUser
	if suspend {
		action = operator.ActionSuspendUser
	}
	s.log.InfoContext(r.Context(), "operator: action",
		"action", action, "operator", p.UserID, "target", res.UserID, "audit_id", res.AuditID,
		"sessions", res.Revoked.Sessions, "tokens", res.Revoked.PersonalTokens, "grants", res.Revoked.OAuthGrants)
}

// queryInt reads an integer query parameter, def when absent.
func queryInt(r *http.Request, key string, def int) (int, error) {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def, nil
	}
	return strconv.Atoi(v)
}

// --- console ---------------------------------------------------------------

// operatorConsoleView is the console's view model.
type operatorConsoleView struct {
	Actor      string
	CSRFToken  string
	Users      []operatorUserRow
	Audit      []auditRowView
	Notice     string
	Error      string
	PrevOffset int
	NextOffset int
	HasPrev    bool
	HasNext    bool
}

// operatorUserRow is one directory row as the console renders it.
type operatorUserRow struct {
	ID        string
	Handle    string
	Actor     string
	Created   string
	LastSeen  string
	Suspended string // humanized; empty when active
	Operator  bool
	Self      bool
	Artifacts int64
	Bytes     string
}

// auditRowView is one audit row as the console and the Settings page render
// it: handles, never emails.
type auditRowView struct {
	Action   string
	Operator string
	Target   string
	Reason   string
	Revoked  string
	When     string
}

// consoleNotices maps the fixed ?done= and ?error= codes the suspension form
// redirects with to their messages. Only these codes render: nothing a
// caller puts in the query string is reflected.
var (
	consoleNotices = map[string]string{
		"suspended":   "User suspended. Their sessions, tokens and OAuth grants no longer authenticate.",
		"unsuspended": "User reinstated. They must sign in again; revoked credentials stay revoked.",
	}
	consoleErrors = map[string]string{
		"reason":    "A reason is required, up to 1000 characters.",
		"self":      "You cannot suspend yourself.",
		"protected": "That user has a CAIRN_OPERATORS identity. Remove it from the list before suspending them.",
		"state":     "That user is already in that state.",
		"notfound":  "No such user.",
		"invalid":   "The request was invalid.",
	}
)

// handleOperatorConsole renders the directory and the recent audit trail.
func (s *Server) handleOperatorConsole(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	offset, err := queryInt(r, "offset", 0)
	if err != nil || offset < 0 {
		offset = 0
	}
	users, err := s.ops.Directory(r.Context(), operatorConsolePage, offset)
	if err != nil {
		s.log.ErrorContext(r.Context(), "operator: directory failed", "error", err)
		s.renderWebError(w, r, err)
		return
	}
	audit, err := s.ops.RecentAudit(r.Context(), 50)
	if err != nil {
		s.log.ErrorContext(r.Context(), "operator: audit failed", "error", err)
		s.renderWebError(w, r, err)
		return
	}
	vm := operatorConsoleView{
		Actor:      p.ActorID,
		Notice:     consoleNotices[r.URL.Query().Get("done")],
		Error:      consoleErrors[r.URL.Query().Get("error")],
		HasPrev:    offset > 0,
		PrevOffset: max(offset-operatorConsolePage, 0),
		HasNext:    len(users) == operatorConsolePage,
		NextOffset: offset + operatorConsolePage,
	}
	if c, cerr := r.Cookie(csrfCookieName); cerr == nil {
		vm.CSRFToken = c.Value
	}
	for _, u := range users {
		row := operatorUserRow{
			ID:        u.ID,
			Handle:    u.Handle,
			Actor:     u.Actor,
			Created:   humanizeSince(u.CreatedAt),
			LastSeen:  "never",
			Operator:  u.Operator,
			Self:      u.ID == p.UserID,
			Artifacts: u.Artifacts,
			Bytes:     humanizeBytes(u.Bytes),
		}
		if u.LastSeenAt != nil {
			row.LastSeen = humanizeSince(*u.LastSeenAt)
		}
		if u.SuspendedAt != nil {
			row.Suspended = humanizeSince(*u.SuspendedAt)
		}
		vm.Users = append(vm.Users, row)
	}
	vm.Audit = toAuditRows(audit)
	s.renderWeb(w, r, "operator", vm)
}

// handleOperatorSuspensionForm is the console's no-script suspension form:
// the same operator.Service call as POST /v1/operator/suspensions, proven
// same-origin by the double-submit form field, answered with a redirect back
// to the console carrying a fixed outcome code.
func (s *Server) handleOperatorSuspensionForm(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	r.Body = http.MaxBytesReader(w, r.Body, maxOperatorRequestBytes)
	if err := r.ParseForm(); err != nil {
		s.renderWebError(w, r, errs.Validationf("operator: malformed form"))
		return
	}
	if !validFormCSRF(r) {
		s.renderWebError(w, r, errs.ErrForbidden)
		return
	}
	var suspend bool
	switch r.PostFormValue("action") {
	case "suspend":
		suspend = true
	case "unsuspend":
	default:
		http.Redirect(w, r, "/operator?error=invalid", http.StatusSeeOther)
		return
	}
	res, err := s.ops.SetSuspended(r.Context(), operator.SuspensionInput{
		OperatorID: p.UserID,
		UserID:     strings.TrimSpace(r.PostFormValue("user_id")),
		Suspend:    suspend,
		Reason:     r.PostFormValue("reason"),
	})
	if err != nil {
		code := consoleErrorCode(err)
		if code == "" {
			s.log.ErrorContext(r.Context(), "operator: suspension failed", "error", err)
			s.renderWebError(w, r, err)
			return
		}
		http.Redirect(w, r, "/operator?error="+url.QueryEscape(code), http.StatusSeeOther)
		return
	}
	s.logSuspension(r, p, res, suspend)
	done := "unsuspended"
	if suspend {
		done = "suspended"
	}
	http.Redirect(w, r, "/operator?done="+done, http.StatusSeeOther)
}

// consoleErrorCode maps a refused suspension to its fixed console message
// code, or "" for an internal failure.
func consoleErrorCode(err error) string {
	switch {
	case errors.Is(err, operator.ErrReason):
		return "reason"
	case errors.Is(err, operator.ErrSelf):
		return "self"
	case errors.Is(err, operator.ErrProtected):
		return "protected"
	}
	switch errs.CodeOf(err) {
	case errs.CodeNotFound:
		return "notfound"
	case errs.CodeConflict:
		return "state"
	case errs.CodeValidation, errs.CodeForbidden:
		return "invalid"
	}
	return ""
}

// auditDetail is the subset of operator_audit.detail the views read.
type auditDetail struct {
	Revoked *operator.Revoked `json:"revoked"`
}

// toAuditRows renders audit entries for the console and the Settings page.
func toAuditRows(entries []operator.AuditEntry) []auditRowView {
	out := make([]auditRowView, 0, len(entries))
	for _, e := range entries {
		row := auditRowView{
			Action:   auditActionLabel(e.Action),
			Operator: firstNonEmpty(e.OperatorHandle, "a former operator"),
			Target:   firstNonEmpty(e.TargetHandle, "a deleted user"),
			Reason:   e.Reason,
			When:     humanizeSince(e.At),
		}
		var d auditDetail
		if json.Unmarshal(e.Detail, &d) == nil && d.Revoked != nil {
			row.Revoked = "ended " + strconv.FormatInt(d.Revoked.Sessions, 10) + " sessions, " +
				strconv.FormatInt(d.Revoked.PersonalTokens, 10) + " API tokens, " +
				strconv.FormatInt(d.Revoked.OAuthGrants, 10) + " OAuth grants"
		}
		out = append(out, row)
	}
	return out
}

// auditActionLabel names an audit action for a reader.
func auditActionLabel(action string) string {
	switch action {
	case operator.ActionSuspendUser:
		return "Suspended"
	case operator.ActionUnsuspendUser:
		return "Reinstated"
	}
	return action
}
