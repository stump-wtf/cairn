package httpapi

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/subscription"
)

// The Settings page's Outbound subscriptions section (SPEC-0023 REQ "Owned
// Outbound Subscriptions": "in Settings and over /v1"). It works without
// script: each action is a plain form posted back to /settings/subscriptions
// under the double-submit form check, like the operator console, and calls
// the same subscription.Service the /v1 handlers call. A create or a rotate
// answers with the page itself carrying the new secret, shown this once and
// never again; nothing else ever renders a secret.
//
// Governing: ADR-0029 (section 6), SPEC-0023 REQ "Owned Outbound
// Subscriptions", REQ "Subscription Target Safety"

// maxSubscriptionFormBytes bounds the Settings subscription form.
const maxSubscriptionFormBytes = 16 << 10

// subscriptionsSectionView is the section's view model.
type subscriptionsSectionView struct {
	Rows      []subscriptionRowView
	Limit     int
	Available bool
	AtLimit   bool
	// EventTypes is the event-type filter vocabulary, for the checkboxes.
	EventTypes []string
	// NewSecret is the secret just minted, supplied or rotated, with the id
	// of the subscription it belongs to. Set only on the response to that
	// create or rotate.
	NewSecret   string
	NewSecretID string
	// Error is why the last form post was refused; Done names what it did.
	Error string
	Done  string
}

// subscriptionRowView is one subscription row: never the secret.
type subscriptionRowView struct {
	ID          string
	URL         string
	Filters     string
	Status      string // "active", "paused" or the disabled reason
	Active      bool
	Paused      bool
	Failures    int
	LastAttempt string
	LastResult  string
}

// subscriptionOutcome is the result of a form post, rendered into the page.
type subscriptionOutcome struct {
	secret, secretID, err, done string
}

func toSubscriptionRow(s *subscription.Subscription) subscriptionRowView {
	var filters []string
	if len(s.EventTypes) > 0 {
		filters = append(filters, "events: "+strings.Join(s.EventTypes, ", "))
	}
	if len(s.ShareTypes) > 0 {
		filters = append(filters, "types: "+strings.Join(s.ShareTypes, ", "))
	}
	if len(s.Tags) > 0 {
		filters = append(filters, "tags: "+strings.Join(s.Tags, ", "))
	}
	row := subscriptionRowView{
		ID:          s.ID,
		URL:         s.URL,
		Filters:     strings.Join(filters, " · "),
		Active:      s.Active(),
		Paused:      s.Paused,
		Failures:    s.ConsecutiveFailures,
		LastAttempt: "never",
	}
	if row.Filters == "" {
		row.Filters = "everything"
	}
	switch {
	case s.DisabledReason != "":
		row.Status = s.DisabledReason
	case s.Paused:
		row.Status = "paused"
	default:
		row.Status = "active"
	}
	if s.LastAttemptAt != nil {
		row.LastAttempt = humanizeSince(*s.LastAttemptAt)
		switch {
		case s.LastError != "" && s.LastStatus != nil:
			row.LastResult = s.LastError + " (" + strconv.Itoa(*s.LastStatus) + ")"
		case s.LastError != "":
			row.LastResult = s.LastError
		case s.LastStatus != nil:
			row.LastResult = "delivered (" + strconv.Itoa(*s.LastStatus) + ")"
		}
	}
	return row
}

// subscriptionsSection builds the section for p, or nil without a core.
func (s *Server) subscriptionsSection(r *http.Request, p *Principal, out *subscriptionOutcome) *subscriptionsSectionView {
	if s.subs == nil {
		return nil
	}
	owner := subscription.Owner{UserID: p.UserID}
	v := &subscriptionsSectionView{
		Limit:      s.subs.Ceiling(owner),
		Available:  s.subs.Available(),
		EventTypes: subscription.EventTypes,
	}
	if out != nil {
		v.NewSecret, v.NewSecretID, v.Error, v.Done = out.secret, out.secretID, out.err, out.done
	} else {
		// The redirect after a pause, resume or delete names what it did;
		// only these fixed words are ever shown.
		switch done := r.URL.Query().Get("subscription"); done {
		case "paused", "resumed", "deleted":
			v.Done = done
		}
	}
	subs, err := s.subs.List(r.Context(), owner)
	if err != nil {
		s.log.WarnContext(r.Context(), "settings: list subscriptions failed", "error", err)
		return v
	}
	for _, sub := range subs {
		v.Rows = append(v.Rows, toSubscriptionRow(sub))
	}
	v.AtLimit = len(v.Rows) >= v.Limit
	return v
}

// splitList splits a comma- or space-separated form field.
func splitList(raw string) []string {
	return strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' || r == '\r' })
}

// subscriptionFormMessage is the page's wording for a refused form post.
// Validation messages are the server's own wording; one that names a refused
// filter value is escaped by the template. None names the URL or a secret.
func subscriptionFormMessage(err error, subs *subscription.Service, owner subscription.Owner) string {
	switch {
	case errors.Is(err, subscription.ErrCeiling):
		return "You already have " + strconv.Itoa(subs.Ceiling(owner)) + " subscriptions, the most allowed. Delete one first."
	case errors.Is(err, subscription.ErrUnavailable):
		return "This server has no CAIRN_ENCRYPTION_KEY, so subscription secrets cannot be stored. Ask the operator to set one."
	case errs.CodeOf(err) == errs.CodeNotFound:
		return "That subscription no longer exists."
	case errs.CodeOf(err) == errs.CodeValidation:
		msg := strings.TrimSuffix(err.Error(), ": "+errs.ErrValidation.Error())
		return strings.TrimPrefix(msg, "subscriptions: ")
	}
	return ""
}

// handleSubscriptionForm serves the Settings section's form posts: create,
// pause, resume, rotate and delete. Only a human's browser session may use
// it, under the double-submit form check.
func (s *Server) handleSubscriptionForm(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	if s.subs == nil {
		s.renderWebError(w, r, errs.ErrNotFound)
		return
	}
	if p == nil || !p.Ambient || p.IsAgent || !p.HasScope(scopeSharingManage) {
		s.renderWebError(w, r, errs.ErrForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxSubscriptionFormBytes)
	if err := r.ParseForm(); err != nil {
		s.renderWebError(w, r, errs.Validationf("subscriptions: malformed form"))
		return
	}
	if !validFormCSRF(r) {
		s.renderWebError(w, r, errs.ErrForbidden)
		return
	}
	owner := subscription.Owner{UserID: p.UserID}
	id := r.PostFormValue("id")
	ctx := r.Context()
	var (
		out subscriptionOutcome
		err error
	)
	switch r.PostFormValue("action") {
	case "create":
		var (
			secret string
			sub    *subscription.Subscription
		)
		secret, sub, err = s.subs.Create(ctx, subscription.CreateInput{
			Owner:      owner,
			CreatedBy:  p.UserID,
			URL:        strings.TrimSpace(r.PostFormValue("url")),
			Secret:     r.PostFormValue("secret"),
			EventTypes: r.PostForm["event_types"],
			ShareTypes: splitList(r.PostFormValue("share_types")),
			Tags:       splitList(r.PostFormValue("tags")),
		})
		if err == nil {
			out = subscriptionOutcome{secret: secret, secretID: sub.ID, done: "created"}
			s.log.InfoContext(ctx, "subscriptions: created", "id", sub.ID, "owner", sub.Owner.String(),
				"secret_supplied", r.PostFormValue("secret") != "")
		}
	case "rotate":
		var secret string
		secret, _, err = s.subs.Rotate(ctx, owner, id, r.PostFormValue("secret"))
		if err == nil {
			out = subscriptionOutcome{secret: secret, secretID: id, done: "rotated"}
			s.log.InfoContext(ctx, "subscriptions: secret rotated", "id", id)
		}
	case "pause", "resume":
		_, err = s.subs.SetPaused(ctx, owner, id, r.PostFormValue("action") == "pause")
		if err == nil {
			http.Redirect(w, r, "/settings?subscription="+url.QueryEscape(r.PostFormValue("action")+"d")+"#subscriptions", http.StatusSeeOther)
			return
		}
	case "delete":
		if err = s.subs.Delete(ctx, owner, id); err == nil {
			s.log.InfoContext(ctx, "subscriptions: deleted", "id", id)
			http.Redirect(w, r, "/settings?subscription=deleted#subscriptions", http.StatusSeeOther)
			return
		}
	default:
		err = errs.Validationf("subscriptions: unknown action")
	}
	if err != nil {
		msg := subscriptionFormMessage(err, s.subs, owner)
		if msg == "" {
			s.log.ErrorContext(ctx, "settings: subscription form failed", "error", err)
			s.renderWebError(w, r, err)
			return
		}
		out = subscriptionOutcome{err: msg}
	}
	// The page carries a secret on this response only: keep it out of every
	// cache.
	w.Header().Set("Cache-Control", "no-store")
	s.renderWeb(w, r, "settings", s.settingsView(r, p, &out))
}
