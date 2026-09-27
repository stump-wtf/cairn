package httpapi

import (
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/metrics"
	"github.com/stump-wtf/cairn/internal/redact"
)

// Owner-Visible Redaction Outcome
//
// What the ingest secret scanner did to an artifact is the owner's to see, on
// every artifact read surface and in every create response (SPEC-0017 RD-9):
// REST and MCP get redaction_status, redacted and redactions {count, rules};
// the web viewer gets a badge, a keyboard-operable disclosure of the rule IDs
// and a polite live-region notice. A non-owner, or an anonymous link reader,
// sees none of it: the fields are omitted, not zeroed.
//
// Nothing here can carry a secret. The outcome is a status from a closed set,
// a count and rule IDs from the scanner's own config (redact.Summary).
//
// Governing: ADR-0023, SPEC-0017 RD-9, "Accessibility Requirements"
//
// @joestump 09/25/2026 - Added for cairn#290.

// redactionsView is the summary a create response and an owner's read carry:
// the number of values masked and the count per rule ID.
type redactionsView struct {
	Count int            `json:"count"`
	Rules map[string]int `json:"rules"`
}

// OwnerRedaction is the owner-only part of artifactResponse. Every field is
// omitted for a viewer who is not the owner.
type OwnerRedaction struct {
	// RedactionStatus is unscanned, clean, masked, not_scanned_binary or
	// not_scanned_oversize.
	RedactionStatus string `json:"redaction_status,omitempty"`
	// Redacted reports whether any value was replaced with [REDACTED], so a
	// client comparing its own checksum with the stored one can explain the
	// difference (RD-10).
	Redacted   *bool           `json:"redacted,omitempty"`
	Redactions *redactionsView `json:"redactions,omitempty"`
}

// ownerRedactionOf builds the owner-only fields from an artifact's recorded
// outcome. An artifact the store never recorded one for reads "unscanned".
func ownerRedactionOf(s redact.Summary) OwnerRedaction {
	status := s.Status
	if status == "" {
		status = redact.StatusUnscanned
	}
	redacted := s.Redacted()
	rules := maps.Clone(s.Rules)
	if rules == nil {
		rules = map[string]int{}
	}
	return OwnerRedaction{
		RedactionStatus: string(status),
		Redacted:        &redacted,
		Redactions:      &redactionsView{Count: s.Count, Rules: rules},
	}
}

// isOwner reports whether actorID owns a. An empty actor (an anonymous
// reader) never does.
func isOwner(actorID string, a *artifact.Artifact) bool {
	return actorID != "" && actorID == a.Access.OwnerID
}

// toOwnerArtifactResponse is toArtifactResponse plus the owner-only outcome.
// Call it only when the viewer is the owner, which a create's caller always is.
func (s *Server) toOwnerArtifactResponse(a *artifact.Artifact) artifactResponse {
	r := s.toArtifactResponse(a)
	r.OwnerRedaction = ownerRedactionOf(a.Redaction)
	return r
}

// toViewerArtifactResponse shows the outcome to the owner and to nobody else.
func (s *Server) toViewerArtifactResponse(a *artifact.Artifact, actorID string) artifactResponse {
	if isOwner(actorID, a) {
		return s.toOwnerArtifactResponse(a)
	}
	return s.toArtifactResponse(a)
}

// redactionBadgeView is the viewer's owner-only summary.
type redactionBadgeView struct {
	Status string
	Count  int
	// Label is the badge's accessible name and visible text, e.g.
	// "2 values redacted".
	Label string
	// Notice is the post-create sentence announced once in a polite live
	// region; empty when nothing was masked.
	Notice string
	Rules  []redactionRuleLine
}

type redactionRuleLine struct {
	Rule  string
	Count int
}

// redactionBadgeOf builds the viewer's summary, rules sorted by ID so the list
// is stable.
func redactionBadgeOf(s redact.Summary) *redactionBadgeView {
	v := &redactionBadgeView{Status: string(s.Status), Count: s.Count}
	if v.Status == "" {
		v.Status = string(redact.StatusUnscanned)
	}
	noun := "values"
	if s.Count == 1 {
		noun = "value"
	}
	v.Label = fmt.Sprintf("%d %s redacted", s.Count, noun)
	if s.Redacted() {
		v.Notice = fmt.Sprintf("%d secret %s in this artifact were replaced with %s when it was created.", s.Count, noun, redact.Mask)
		if s.Count == 1 {
			v.Notice = fmt.Sprintf("1 secret value in this artifact was replaced with %s when it was created.", redact.Mask)
		}
	}
	for _, rule := range slices.Sorted(maps.Keys(s.Rules)) {
		v.Rules = append(v.Rules, redactionRuleLine{Rule: rule, Count: s.Rules[rule]})
	}
	return v
}

// Rejection log attributes (SPEC-0017 RD-9 "Logs carry no value"): a logged
// redaction rejection names the rule IDs that fired and the surface that was
// scanned, next to the request id writeError and mcpToolErr already log. Both
// come from closed sets, the scanner's rule IDs and metrics.RedactionSurfaces,
// so neither can carry a scanned value; the rejection's own message holds only
// the field, rule, line and column.

// redactionLogAttrs returns the extra log attributes for a redaction
// rejection, and nil for any other error. surface may be empty when the write
// that failed is not one of the scanned surfaces.
func redactionLogAttrs(err error, surface metrics.RedactionSurface) []any {
	var rej *redact.Rejection
	if !errors.As(err, &rej) {
		return nil
	}
	seen := map[string]bool{}
	for _, f := range rej.Findings {
		seen[f.Rule] = true
	}
	attrs := []any{"redaction_reason", rej.Reason, "redaction_rules", strings.Join(slices.Sorted(maps.Keys(seen)), ",")}
	if surface != "" {
		attrs = append(attrs, "surface", string(surface))
	}
	return attrs
}

// restRedactionSurface names the scanned surface of a REST or webhook write
// from its matched route, never from anything the client sent: a comment
// route, a run route, the webhook ingress, or the artifact create, which is a
// bundle when the rejected field is a member. Any other route has no surface.
func restRedactionSurface(r *http.Request, err error) metrics.RedactionSurface {
	pattern := r.URL.Path
	if rc := chi.RouteContext(r.Context()); rc != nil && rc.RoutePattern() != "" {
		pattern = rc.RoutePattern()
	}
	switch {
	case strings.HasPrefix(pattern, hookIngressPathPrefix):
		return metrics.SurfaceWebhook
	case strings.HasSuffix(pattern, "/comments"):
		return metrics.SurfaceComment
	case strings.HasPrefix(pattern, "/v1/runs"):
		return metrics.SurfaceRun
	case pattern == "/v1/artifacts":
		var rej *redact.Rejection
		if errors.As(err, &rej) && strings.HasPrefix(rej.Field, "members[") {
			return metrics.SurfaceBundle
		}
		return metrics.SurfaceArtifact
	}
	return ""
}

// mcpRedactionSurface maps the MCP write tools to their scanned surface.
var mcpRedactionSurface = map[string]metrics.RedactionSurface{
	"artifact_create":  metrics.SurfaceArtifact,
	"bundle_create":    metrics.SurfaceBundle,
	"artifact_comment": metrics.SurfaceComment,
	"run_create":       metrics.SurfaceRun,
	"run_append_spans": metrics.SurfaceRun,
}
