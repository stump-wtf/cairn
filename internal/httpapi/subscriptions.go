package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/subscription"
)

// Owned outbound subscriptions over /v1 (SPEC-0023 REQ "Owned Outbound
// Subscriptions"): create, list, read, pause or resume, rotate the secret of,
// and delete a subscription the caller's workspace owns. Every route needs a
// human principal holding sharing:manage — a browser session, or an
// operator's human static token — because a subscription exports its owner's
// events to a URL of the caller's choosing: an agent token must not be able
// to point them somewhere new. An id the caller's workspace does not own is
// the uniform 404.
//
// Teams do not exist yet (#328). The owner field already takes the design's
// "team:<slug>" form, and every such request is refused with 403, because no
// caller can be an admin or owner of a team (scenario "Member tries to add a
// team subscription"). The schema carries owner_team_id so team
// subscriptions need no migration when teams land.
//
// Governing: ADR-0029 (Teams and Tenancy, section 6), SPEC-0023 REQ "Owned
// Outbound Subscriptions", REQ "Subscription Target Safety"

// maxSubscriptionRequestBytes bounds a subscription request body: a URL, a
// secret and three short filter lists.
const maxSubscriptionRequestBytes = 16 << 10

// createSubscriptionRequest is the POST /v1/subscriptions body.
type createSubscriptionRequest struct {
	// Owner is "" or "user" (the caller), "user:<the caller's id>", or
	// "team:<slug>" (refused until teams exist).
	Owner      string   `json:"owner"`
	URL        string   `json:"url"`
	Secret     string   `json:"secret"`
	EventTypes []string `json:"event_types"`
	ShareTypes []string `json:"share_types"`
	Tags       []string `json:"tags"`
}

// updateSubscriptionRequest is the PATCH /v1/subscriptions/{id} body.
type updateSubscriptionRequest struct {
	Paused *bool `json:"paused"`
}

// rotateSubscriptionRequest is the POST /v1/subscriptions/{id}/rotate body:
// an optional receiver-issued secret; empty mints one.
type rotateSubscriptionRequest struct {
	Secret string `json:"secret"`
}

// subscriptionView is the JSON view of a subscription. The secret appears
// only in the create and rotate responses (subscriptionSecretResponse).
type subscriptionView struct {
	ID                  string     `json:"id"`
	Owner               string     `json:"owner"`
	URL                 string     `json:"url"`
	EventTypes          []string   `json:"event_types"`
	ShareTypes          []string   `json:"share_types"`
	Tags                []string   `json:"tags"`
	Active              bool       `json:"active"`
	Paused              bool       `json:"paused"`
	DisabledReason      string     `json:"disabled_reason,omitempty"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
	LastAttemptAt       *time.Time `json:"last_attempt_at,omitempty"`
	LastStatus          *int       `json:"last_status,omitempty"`
	LastError           string     `json:"last_error,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
}

// subscriptionSecretResponse carries the one-time secret.
type subscriptionSecretResponse struct {
	subscriptionView
	Secret string `json:"secret"`
}

type listSubscriptionsResponse struct {
	Subscriptions []subscriptionView `json:"subscriptions"`
	Limit         int                `json:"limit"`
	Available     bool               `json:"available"`
}

func toSubscriptionView(s *subscription.Subscription) subscriptionView {
	return subscriptionView{
		ID:                  s.ID,
		Owner:               s.Owner.String(),
		URL:                 s.URL,
		EventTypes:          s.EventTypes,
		ShareTypes:          s.ShareTypes,
		Tags:                s.Tags,
		Active:              s.Active(),
		Paused:              s.Paused,
		DisabledReason:      s.DisabledReason,
		ConsecutiveFailures: s.ConsecutiveFailures,
		LastAttemptAt:       s.LastAttemptAt,
		LastStatus:          s.LastStatus,
		LastError:           s.LastError,
		CreatedAt:           s.CreatedAt,
	}
}

// errTeamOwner refuses a team-owned subscription request: only a team's
// admins and owners may manage its subscriptions, and until teams exist
// (#328) no caller is either.
var errTeamOwner = errs.New(errs.CodeForbidden, "only a team admin or owner may manage the team's subscriptions")

// subscriptionOwner resolves the owner a request names to a workspace the
// principal may manage: its own user, or a team it administers. Anything
// else is 403, never a silent fallback to the caller's own workspace.
func subscriptionOwner(p *Principal, raw string) (subscription.Owner, error) {
	raw = strings.TrimSpace(raw)
	switch {
	case raw == "" || raw == "user" || raw == "user:"+p.UserID:
		return subscription.Owner{UserID: p.UserID}, nil
	case strings.HasPrefix(raw, "team:"):
		return subscription.Owner{}, errTeamOwner
	}
	return subscription.Owner{}, errs.ErrForbidden
}

// subscriptionErrorDetails turns a refused request into envelope details that
// say why. Messages never carry the target URL or a secret.
func subscriptionErrorDetails(err error, subs *subscription.Service, owner subscription.Owner) map[string]string {
	switch {
	case errors.Is(err, subscription.ErrCeiling):
		return map[string]string{"reason": "subscription_ceiling_reached", "limit": strconv.Itoa(subs.Ceiling(owner))}
	case errors.Is(err, subscription.ErrUnavailable):
		return map[string]string{"reason": "encryption_key_unset"}
	case errors.Is(err, errTeamOwner):
		return map[string]string{"reason": "team_role_required"}
	case errs.CodeOf(err) == errs.CodeValidation:
		return map[string]string{"reason": strings.TrimSuffix(err.Error(), ": "+errs.ErrValidation.Error())}
	}
	return nil
}

// decodeSubscriptionBody decodes a bounded JSON body into v. An empty body
// is allowed when allowEmpty (rotate).
func (s *Server) decodeSubscriptionBody(w http.ResponseWriter, r *http.Request, v any, allowEmpty bool) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxSubscriptionRequestBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if allowEmpty && errors.Is(err, io.EOF) {
			return true
		}
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			s.writeError(w, r, errs.ErrTooLarge, nil)
			return false
		}
		s.writeError(w, r, errs.Validationf("subscriptions: malformed JSON body"), nil)
		return false
	}
	return true
}

// handleCreateSubscription creates a subscription for the caller's
// workspace and returns its secret exactly once.
func (s *Server) handleCreateSubscription(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	var req createSubscriptionRequest
	if !s.decodeSubscriptionBody(w, r, &req, false) {
		return
	}
	owner, err := subscriptionOwner(p, req.Owner)
	if err != nil {
		s.writeError(w, r, err, subscriptionErrorDetails(err, s.subs, owner))
		return
	}
	secret, sub, err := s.subs.Create(r.Context(), subscription.CreateInput{
		Owner:      owner,
		CreatedBy:  p.UserID,
		URL:        strings.TrimSpace(req.URL),
		Secret:     req.Secret,
		EventTypes: req.EventTypes,
		ShareTypes: req.ShareTypes,
		Tags:       req.Tags,
	})
	if err != nil {
		s.writeError(w, r, err, subscriptionErrorDetails(err, s.subs, owner))
		return
	}
	s.log.InfoContext(r.Context(), "subscriptions: created",
		"id", sub.ID, "owner", sub.Owner.String(), "secret_supplied", req.Secret != "")
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusCreated, subscriptionSecretResponse{subscriptionView: toSubscriptionView(sub), Secret: secret})
}

// handleListSubscriptions lists the caller's workspace's subscriptions.
func (s *Server) handleListSubscriptions(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	owner, err := subscriptionOwner(p, r.URL.Query().Get("owner"))
	if err != nil {
		s.writeError(w, r, err, subscriptionErrorDetails(err, s.subs, owner))
		return
	}
	subs, err := s.subs.List(r.Context(), owner)
	if err != nil {
		s.writeError(w, r, err, nil)
		return
	}
	out := listSubscriptionsResponse{
		Subscriptions: make([]subscriptionView, 0, len(subs)),
		Limit:         s.subs.Ceiling(owner),
		Available:     s.subs.Available(),
	}
	for _, sub := range subs {
		out.Subscriptions = append(out.Subscriptions, toSubscriptionView(sub))
	}
	s.writeJSON(w, http.StatusOK, out)
}

// handleGetSubscription reads one of the caller's workspace's subscriptions.
func (s *Server) handleGetSubscription(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	id := chi.URLParam(r, "id")
	sub, err := s.subs.Get(r.Context(), subscription.Owner{UserID: p.UserID}, id)
	if err != nil {
		s.writeError(w, r, err, map[string]string{"id": id})
		return
	}
	s.writeJSON(w, http.StatusOK, toSubscriptionView(sub))
}

// handleUpdateSubscription pauses or resumes a subscription. Resuming also
// re-enables one the worker disabled after consecutive failures.
func (s *Server) handleUpdateSubscription(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	id := chi.URLParam(r, "id")
	var req updateSubscriptionRequest
	if !s.decodeSubscriptionBody(w, r, &req, false) {
		return
	}
	if req.Paused == nil {
		s.writeError(w, r, errs.Validationf("subscriptions: paused is required"), nil)
		return
	}
	sub, err := s.subs.SetPaused(r.Context(), subscription.Owner{UserID: p.UserID}, id, *req.Paused)
	if err != nil {
		s.writeError(w, r, err, map[string]string{"id": id})
		return
	}
	s.log.InfoContext(r.Context(), "subscriptions: updated", "id", sub.ID, "paused", sub.Paused)
	s.writeJSON(w, http.StatusOK, toSubscriptionView(sub))
}

// handleRotateSubscription replaces a subscription's secret, returning the
// new one exactly once.
func (s *Server) handleRotateSubscription(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	id := chi.URLParam(r, "id")
	var req rotateSubscriptionRequest
	if !s.decodeSubscriptionBody(w, r, &req, true) {
		return
	}
	owner := subscription.Owner{UserID: p.UserID}
	secret, sub, err := s.subs.Rotate(r.Context(), owner, id, req.Secret)
	if err != nil {
		details := subscriptionErrorDetails(err, s.subs, owner)
		if details == nil {
			details = map[string]string{"id": id}
		}
		s.writeError(w, r, err, details)
		return
	}
	s.log.InfoContext(r.Context(), "subscriptions: secret rotated", "id", sub.ID, "secret_supplied", req.Secret != "")
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, subscriptionSecretResponse{subscriptionView: toSubscriptionView(sub), Secret: secret})
}

// handleDeleteSubscription deletes a subscription.
func (s *Server) handleDeleteSubscription(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	id := chi.URLParam(r, "id")
	if err := s.subs.Delete(r.Context(), subscription.Owner{UserID: p.UserID}, id); err != nil {
		s.writeError(w, r, err, map[string]string{"id": id})
		return
	}
	s.log.InfoContext(r.Context(), "subscriptions: deleted", "id", id)
	w.WriteHeader(http.StatusNoContent)
}

// requireSubscriptions answers 404 when the server has no subscription core
// (storeless unit wirings), so the routes behave as absent.
func (s *Server) requireSubscriptions(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.subs == nil {
			s.writeError(w, r, errs.ErrNotFound, nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}
