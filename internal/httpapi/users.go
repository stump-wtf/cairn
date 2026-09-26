package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/user"
)

// Sign-in identities resolve to user rows (SPEC-0023 REQ "Users and
// Identities"), and every owner and actor reference is that user's id (REQ
// "Owner Model"). What a session shows as its actor is rendered from the user
// row (user.ActorSQL), never from what the provider typed into this sign-in:
// a verified primary email, else the user's actor key (a legacy owner string,
// or a subject that is not email-shaped), else "user:<id>". So a Pocket ID and
// a GitHub identity carrying the same verified email are one user with one
// Bin, and no identity reaches an email's Bin without verifying that email.
//
// Governing: ADR-0029, SPEC-0023 REQ "Users and Identities", REQ "Owner
// Model".

// errNoUserStore reports a sign-in on a wiring with no users store.
var errNoUserStore = errors.New("httpapi: no users store configured")

// errUserSuspended reports a sign-in or credential of a suspended user
// (SPEC-0023 "Suspending a user offboards them").
var errUserSuspended = errors.New("httpapi: user is suspended")

// resolveSignIn resolves a provider identity to its user and the actor the
// session renders as. A suspended user's sign-in is refused with
// errUserSuspended: suspension deleted their sessions, and signing in again
// must not mint a new one.
func (s *Server) resolveSignIn(ctx context.Context, id user.Identity) (*user.User, string, error) {
	if s.users == nil {
		return nil, "", errNoUserStore
	}
	u, err := s.users.Resolve(ctx, id)
	if err != nil {
		return nil, "", err
	}
	if u.SuspendedAt != nil {
		return nil, "", errUserSuspended
	}
	return u, u.Actor, nil
}

// refuseSignIn answers a sign-in resolveSignIn refused. A suspended user
// gets the uniform forbidden page, with no session minted; anything else is
// an internal error.
func (s *Server) refuseSignIn(w http.ResponseWriter, r *http.Request, provider string, err error) {
	if errors.Is(err, errUserSuspended) {
		s.log.InfoContext(r.Context(), provider+": sign-in refused", "reason", "user suspended")
		s.renderWebError(w, r, errs.ErrForbidden)
		return
	}
	s.log.ErrorContext(r.Context(), provider+": resolve user failed", "error", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

// displayActors maps each actor string a page is about to show to what this
// viewer may see (SPEC-0023 REQ "Users and Identities", audit A18): the viewer
// sees their own actor string as it is, and everyone else, anonymous readers
// included, sees a display handle rather than an email. Handles come from the
// users table when a user owns the email, else from the email's local part,
// so an email never reaches a reader who is not its owner even for legacy
// strings no user row claims yet.
func (s *Server) displayActors(ctx context.Context, viewer *Principal, actors ...string) map[string]string {
	out := make(map[string]string, len(actors))
	var lookup []string
	for _, a := range actors {
		if _, done := out[a]; done {
			continue
		}
		if viewer != nil && viewer.ActorID != "" && viewer.ActorID == a {
			out[a] = a
			continue
		}
		out[a] = user.HandleFromEmail(a)
		if strings.Contains(a, "@") {
			lookup = append(lookup, a)
		}
	}
	if len(lookup) > 0 && s.users != nil {
		handles, err := s.users.DisplayHandles(ctx, lookup)
		if err != nil {
			// The local-part fallback is already in place; a lookup failure
			// degrades the label, never reveals the email.
			s.log.WarnContext(ctx, "display handles lookup failed", "error", err)
		}
		for _, a := range lookup {
			if h, ok := handles[user.NormalizeEmail(a)]; ok {
				out[a] = h
			}
		}
	}
	return out
}

// displayActor is displayActors for a single actor string.
func (s *Server) displayActor(ctx context.Context, viewer *Principal, actor string) string {
	return s.displayActors(ctx, viewer, actor)[actor]
}

// maskActors applies displayActors to a rendered page's provenance line and
// comment thread in one lookup.
func (s *Server) maskActors(ctx context.Context, viewer *Principal, prov *provenanceLine, comments []commentLine) {
	actors := make([]string, 0, len(comments)+1)
	actors = append(actors, prov.Actor)
	for _, c := range comments {
		actors = append(actors, c.Actor)
	}
	shown := s.displayActors(ctx, viewer, actors...)
	prov.Actor = shown[prov.Actor]
	for i := range comments {
		comments[i].Actor = shown[comments[i].Actor]
	}
}
