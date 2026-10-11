package cliclient

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"time"
)

// AnchorArtifact is the whole-artifact anchor `cairn comment` posts on. The
// server requires an anchor_type on every comment (SPEC-0006 "Polymorphic
// Anchor Model") and validates it against the share type, so this is the one
// literal the CLI sends and the server remains the judge of whether it is
// allowed.
const AnchorArtifact = "artifact"

// Comment is one annotation as the server stored it. Actor identity and kind
// are server-derived; the CLI only displays them (SPEC-0016 EV-4).
type Comment struct {
	ID         int64     `json:"id"`
	AnchorType string    `json:"anchor_type"`
	ParentID   *int64    `json:"parent_id,omitempty"`
	ActorID    string    `json:"actor_id"`
	ActorKind  string    `json:"actor_kind"`
	Body       string    `json:"body"`
	CreatedAt  time.Time `json:"created_at"`
	Deleted    bool      `json:"deleted"`
}

// CommentOptions carries one comment write. ParentID is nil for a thread root;
// a reply inherits its root's anchor, so no anchor is sent with it.
type CommentOptions struct {
	Body     string
	ParentID *int64
}

// commentPayload is POST /v1/artifacts/{id}/comments' body. It is kept
// unexported: callers compose a CommentOptions, never wire shape.
type commentPayload struct {
	AnchorType string `json:"anchor_type"`
	ParentID   *int64 `json:"parent_id,omitempty"`
	Body       string `json:"body"`
}

// Comment posts one comment on an artifact through
// POST /v1/artifacts/{id}/comments and returns the stored row. It is a
// mutating command's request, so the caller must have resolved a token before
// calling (SPEC-0008 "Every mutating command carries auth"); the server's
// annotations:write scope decides what the token may do.
func (c *Client) Comment(ctx context.Context, id string, opts CommentOptions) (*Comment, error) {
	payload := commentPayload{AnchorType: AnchorArtifact, ParentID: opts.ParentID, Body: opts.Body}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(ctx, http.MethodPost, "/v1/artifacts/"+url.PathEscape(id)+"/comments", bytes.NewReader(body), "application/json")
	if err != nil {
		return nil, err
	}
	var out Comment
	if err := DecodeInto(resp, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
