package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/errs"
)

// The owner policy surface (issue #94, SPEC-0009 REQ "Owner-Only Policy
// Changes", REQ "Id Rotation as Revoke-a-Leaked-Link", REQ "Default 7-Day
// TTL, Owner-Adjustable, Visible Countdown"). All three endpoints below are
// mounted owner + sharing:manage only: requireScope(scopeSharingManage)
// refuses every agent token (agentScopes omits sharing:manage, ADR-0004 /
// SPEC-0007 "sharing:manage is human-only"), and the store's resolveOwned
// then refuses any authenticated-but-non-owning human with a DISTINCT 403
// (SPEC-0009 Security Requirements "Non-owner policy change") rather than the
// uniform 404 DeleteArtifact uses — provenance/ownership are never
// reassigned by any of these.
//
// Endpoint Table (SPEC-0009):
//
//	PATCH /v1/artifacts/{id}/policy  change sharing (visibility)
//	PATCH /v1/artifacts/{id}/ttl     change expiry (extend/shorten)
//	POST  /v1/artifacts/{id}/rotate  rotate the id (revoke a leaked link)

// maxPolicyRequestBytes bounds a policy/ttl request body before it is
// buffered (SPEC-0009 REQ "Request Body Size Limits"); these payloads are a
// single small field so the cap is deliberately tight.
const maxPolicyRequestBytes = 4 << 10

// visibilityRequest is the PATCH .../policy body: the new link-access
// visibility, within the artifact.Visibility set (ADR-0007).
type visibilityRequest struct {
	Visibility artifact.Visibility `json:"visibility"`
}

// ttlRequest is the PATCH .../ttl body: the new TTL in seconds from now
// (extends when longer than the remaining time, shortens when shorter),
// mirroring the create path's X-Cairn-Ttl-Seconds semantics (handlers.go
// requestedTTL) so the two TTL surfaces agree on units and bounds.
type ttlRequest struct {
	TTLSeconds int64 `json:"ttl_seconds"`
}

// handleUpdateVisibility implements PATCH /v1/artifacts/{id}/policy.
func (s *Server) handleUpdateVisibility(w http.ResponseWriter, r *http.Request) {
	p, ok := principalFrom(r.Context())
	if !ok {
		s.writeError(w, r, errs.ErrUnauthorized, nil)
		return
	}
	id := chi.URLParam(r, "id")
	var req visibilityRequest
	if err := s.decodePolicyBody(w, r, &req); err != nil {
		s.writeError(w, r, err, map[string]string{"id": id})
		return
	}
	art, err := s.store.UpdateVisibility(r.Context(), id, p.ActorID, req.Visibility)
	if err != nil {
		s.writeError(w, r, err, map[string]string{"id": id})
		return
	}
	s.writeJSON(w, http.StatusOK, s.toArtifactResponse(art))
}

// handleUpdateTTL implements PATCH /v1/artifacts/{id}/ttl. The server remains
// authoritative over expiry (ADR-0007): a requested TTL outside (0,
// MaxRequestedTTL] is validation_failed rather than silently clamped, the
// same discipline requestedTTL enforces on create.
func (s *Server) handleUpdateTTL(w http.ResponseWriter, r *http.Request) {
	p, ok := principalFrom(r.Context())
	if !ok {
		s.writeError(w, r, errs.ErrUnauthorized, nil)
		return
	}
	id := chi.URLParam(r, "id")
	var req ttlRequest
	if err := s.decodePolicyBody(w, r, &req); err != nil {
		s.writeError(w, r, err, map[string]string{"id": id})
		return
	}
	ttl, inv := checkTTLSeconds("ttl_seconds", errs.LocBody, strconv.FormatInt(req.TTLSeconds, 10), s.cfg.MaxRequestedTTL)
	if inv != nil {
		s.writeError(w, r, inv, map[string]string{"id": id})
		return
	}
	art, err := s.store.UpdateTTL(r.Context(), id, p.ActorID, s.now().Add(ttl))
	if err != nil {
		s.writeError(w, r, err, map[string]string{"id": id})
		return
	}
	s.writeJSON(w, http.StatusOK, s.toArtifactResponse(art))
}

// handleRotateID implements POST /v1/artifacts/{id}/rotate: mints a new
// public id, invalidates the old one, and returns the artifact's fresh
// envelope (new id/url/mcp handle) so a caller — including the web shell's
// owner controls — can redirect to the new location in one round trip.
func (s *Server) handleRotateID(w http.ResponseWriter, r *http.Request) {
	p, ok := principalFrom(r.Context())
	if !ok {
		s.writeError(w, r, errs.ErrUnauthorized, nil)
		return
	}
	id := chi.URLParam(r, "id")
	art, err := s.store.RotateID(r.Context(), id, p.ActorID)
	if err != nil {
		s.writeError(w, r, err, map[string]string{"id": id})
		return
	}
	s.writeJSON(w, http.StatusOK, s.toArtifactResponse(art))
}

// decodePolicyBody caps the request body at maxPolicyRequestBytes and decodes
// it as JSON, mirroring decodeAnnotationBody's transport-level 413/validation
// split (SPEC-0009 REQ "Request Body Size Limits").
func (s *Server) decodePolicyBody(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxPolicyRequestBytes)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return errs.ErrTooLarge
		}
		return errs.Violate("body", errs.LocBody, errs.ReasonInvalidFormat, errs.WithExpect("a JSON object"))
	}
	return nil
}
