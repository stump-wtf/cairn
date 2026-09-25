// Package event defines the transport-agnostic lifecycle facts Cairn announces:
// what happened (Kind), which artifact it happened to (Subject), and who caused
// it (Actor). Producers — the artifact store, the annotation service and the
// trajectory service — hand an Event to an Emitter after their transaction
// commits; the outbound webhook emitter (internal/outboundhook) is the only
// encoder, so the wire shape lives in exactly one place.
//
// The package is a leaf: it imports only the artifact vocabulary, so every
// service can depend on it without a cycle.
//
// Governing: ADR-0022 (annotation and trace lifecycle events), SPEC-0016 EV-1
// "Event Kind Registry", EV-4 "Server-Derived Actor Kind"; ADR-0017.
package event

import (
	"time"

	"github.com/stump-wtf/cairn/internal/artifact"
)

// Kind names one event type, `<noun>.<past-tense verb>` (SPEC-0016 EV-1).
type Kind string

// The EV-1 registry. A kind MUST be listed here, and be Registered, before it
// is emitted.
const (
	ArtifactCreated Kind = "artifact.created"
	CommentCreated  Kind = "comment.created"
	ReactionAdded   Kind = "reaction.added"
	ReactionRemoved Kind = "reaction.removed"
	RunClosed       Kind = "run.closed"

	// Registered by SPEC-0020 REQ-14 (opt-in permanent retention) and emitted
	// only once that capability ships.
	ArtifactRetained Kind = "artifact.retained"
	ArtifactReleased Kind = "artifact.released"
	ArtifactDeleted  Kind = "artifact.deleted"

	// Reserved for the comment edit and delete routes (#158). They MUST NOT be
	// emitted for any other meaning, and the encoder refuses them until then.
	CommentEdited  Kind = "comment.edited"
	CommentDeleted Kind = "comment.deleted"
)

// registered is the set of kinds an encoder may put on the wire. The reserved
// kinds are deliberately absent.
var registered = map[Kind]bool{
	ArtifactCreated:  true,
	CommentCreated:   true,
	ReactionAdded:    true,
	ReactionRemoved:  true,
	RunClosed:        true,
	ArtifactRetained: true,
	ArtifactReleased: true,
	ArtifactDeleted:  true,
}

// Registered reports whether k may be emitted. Unknown and reserved kinds are
// not (SPEC-0016 EV-1 "Unknown kind never emitted").
func (k Kind) Registered() bool { return registered[k] }

// Reserved reports whether k is a name held back for a future route (EV-1).
func (k Kind) Reserved() bool { return k == CommentEdited || k == CommentDeleted }

// ActorKind is the server-derived class of the principal behind an event:
// human iff it authenticated with an ambient browser session, agent for every
// bearer credential (SPEC-0016 EV-4).
type ActorKind string

const (
	KindHuman ActorKind = "human"
	KindAgent ActorKind = "agent"
)

// Valid reports whether k is one of the two derivable kinds. The empty kind is
// not valid: a caller that did not derive one must not act.
func (k ActorKind) Valid() bool { return k == KindHuman || k == KindAgent }

// AuthMethod records how the principal authenticated (SPEC-0016 EV-4).
type AuthMethod string

const (
	// AuthSession is a browser session cookie (Pocket ID, GitHub, dev login).
	AuthSession AuthMethod = "session"
	// AuthOAuth is an MCP OAuth access token.
	AuthOAuth AuthMethod = "oauth"
	// AuthPAT is a personal access token, whatever its is_agent flag.
	AuthPAT AuthMethod = "pat"
	// AuthAPIToken is a CAIRN_API_TOKENS entry or the insecure dev bearer.
	AuthAPIToken AuthMethod = "api_token"
)

// Actor is who caused an event, derived from the authenticated principal and
// never from request content. OnBehalfOf is the one asserted field: it names a
// harness or agent for display, and consumers MUST NOT gate trust on it.
type Actor struct {
	ID         string
	Channel    artifact.Channel
	OnBehalfOf string
	Kind       ActorKind
	Auth       AuthMethod
}

// Subject is the artifact an event is about.
type Subject struct {
	PublicID  string
	ShareType artifact.ShareType
	Title     string
	// WebPath is the registry-derived, origin-agnostic web path; the encoder
	// joins its configured base URL onto it.
	WebPath   string
	Tags      []string
	ExpiresAt time.Time
	// OwnerID routes the event to the owner's subscriptions (ADR-0029). It is
	// never encoded on the wire (SPEC-0016 EV-7).
	OwnerID string
}

// Comment is the comment.* payload.
type Comment struct {
	ID         int64
	ParentID   *int64
	AnchorType string
	AnchorKey  string
	// Body is the full stored body; the encoder truncates it (EV-3).
	Body string
}

// Reaction is the reaction.* payload. ApprovalClass and Approval are computed
// by the annotation service at write time (EV-5).
type Reaction struct {
	ID            int64
	AnchorType    string
	AnchorKey     string
	Emoji         string
	ApprovalClass bool
	Approval      bool
}

// Run is the run.closed payload.
type Run struct {
	Status     string
	SpanCount  int
	StartedAt  time.Time
	EndedAt    time.Time
	DurationMS int64
}

// Event is one lifecycle fact. Exactly one of Comment, Reaction and Run is set
// for the kinds that carry one; Model is set only for artifact.created.
type Event struct {
	Kind     Kind
	Subject  Subject
	Actor    Actor
	Model    string
	Comment  *Comment
	Reaction *Reaction
	Run      *Run
}

// Emitter receives post-commit events. Implementations MUST be safe for
// concurrent use and MUST NOT block or panic the caller: an emit failure is the
// emitter's problem, never the originating request's (SPEC-0016 EV-2).
type Emitter interface {
	Emit(Event)
}
