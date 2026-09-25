// Package mcpsession is the core for tracking MCP agent sessions/connections
// (issue #76, SPEC-0007, ADR-0004): Joe runs multiple agents over MCP and
// wants to make sense of which agent is doing what. A session is recorded
// when an OAuth-authenticated client completes the MCP `initialize`
// handshake, identified by the connected client's Implementation name/version
// (e.g. "claude-code/1.2.3") and tied to the OAuth grant it authorized with —
// so revoking the grant (oauth.Service.RevokeGrant) ends the session, exactly
// like revoking any other connection in settings.
//
// The package is transport-free, the same shape as pat and oauth: the httpapi
// adapter's MCP transport (mcp.go) calls Record at `initialize` and Touch on
// every tool call; the REST adapter (mcpsessions.go) calls List for the
// owner-scoped GET /v1/mcp/sessions endpoint. Ending a session is not a
// method here — it is oauth.Service.RevokeGrant on the session's GrantID,
// the same "revoke anytime in settings" primitive every other connection
// uses (ADR-0004); this package only tracks, never revokes.
//
// Governing: SPEC-0007 (mcp-server-and-oauth), ADR-0004 (MCP first-class
// surface with OAuth, subject-vs-actor identity mapping).
package mcpsession

import "time"

// Session is one recorded MCP client connection.
type Session struct {
	// ID is the MCP transport's own session id (streamable-HTTP
	// Mcp-Session-Id) — globally unique per connection, so it is also this
	// row's primary key (no separate id minted).
	ID string
	// UserID is the user the OAuth grant is bound to (ADR-0004: agents
	// inherit, never exceed, the human's reach; SPEC-0023 REQ "Owner Model").
	UserID string
	// GrantID ties the session to the OAuth grant it authenticated with;
	// revoking that grant ends the session.
	GrantID string
	// ClientID is the OAuth client_id (RFC 7591 dynamically-registered
	// client) the grant authenticated with.
	ClientID string
	// ClientName and ClientVersion are the connecting MCP client's own
	// self-identification from the `initialize` handshake (mcp.Implementation
	// Name/Version — e.g. "claude-code", "1.2.3"), which is what actually
	// distinguishes "which agent" to a human reading the list; the OAuth
	// client_id is an opaque DCR registration, not a legible name.
	ClientName     string
	ClientVersion  string
	ConnectedAt    time.Time
	LastActivityAt time.Time
	// ToolCalls counts every tools/call this session made, success or
	// failure — "what each agent is doing" includes failed attempts.
	ToolCalls int64
	// ArtifactsCreated counts successful artifact_create/bundle_create/
	// run_create/run_append_spans calls (the create-and-push tools).
	ArtifactsCreated int64
	// AnnotationsPosted counts successful artifact_comment/artifact_react
	// calls.
	AnnotationsPosted int64
	// GrantRevokedAt is non-nil when the tied OAuth grant has been revoked
	// (joined from oauth_grants at read time) — the session is then reported
	// as ended, even though this table carries no revoked flag of its own.
	GrantRevokedAt *time.Time
}

// Ended reports whether the session's grant has been revoked.
func (s *Session) Ended() bool { return s.GrantRevokedAt != nil }

// Activity is the kind of action Touch is recording, selecting which
// lightweight counter increments alongside LastActivityAt.
type Activity int

const (
	// ActivityToolCall increments ToolCalls only — recorded for every
	// tools/call regardless of outcome.
	ActivityToolCall Activity = iota
	// ActivityArtifactCreated additionally increments ArtifactsCreated —
	// recorded once a create-and-push tool call SUCCEEDS.
	ActivityArtifactCreated
	// ActivityAnnotationPosted additionally increments AnnotationsPosted —
	// recorded once a comment/react tool call SUCCEEDS.
	ActivityAnnotationPosted
)

// RecordInput is what Service.Record needs to open a session row at
// `initialize`.
type RecordInput struct {
	// ID is the MCP transport session id (primary key).
	ID string
	// UserID is the grant's user.
	UserID string
	// GrantID is the OAuth grant the connecting token authenticated with.
	GrantID string
	// ClientID is the OAuth client_id the grant belongs to.
	ClientID string
	// ClientName and ClientVersion are the MCP client's self-identification.
	ClientName    string
	ClientVersion string
}
