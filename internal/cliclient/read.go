package cliclient

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// BodyMeta is what a streaming body response says about the bytes it carries:
// the size the server declared and the checksum it re-verifies the stored
// bytes against. A member's figures are the member's own, which is why a
// bundle read cannot take them from the bundle artifact's metadata.
// Size is 0 when the server streamed without a Content-Length.
type BodyMeta struct {
	Size   int64
	SHA256 string
}

// ChecksumHeader carries the stored body's SHA-256 on a body response
// (internal/httpapi/handlers.go serveBody).
const ChecksumHeader = "X-Cairn-Checksum"

// Member is one file of a bundle, as the server lists it. A bundle artifact
// carries no single body, so these names are what OpenMember addresses.
type Member struct {
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	MediaType string `json:"media_type"`
}

// Provenance is the server-derived record of who created an artifact, through
// which channel, and when (SPEC-0009). The CLI displays it and never computes
// any part of it.
type Provenance struct {
	Actor      string    `json:"actor"`
	OnBehalfOf string    `json:"on_behalf_of,omitempty"`
	Channel    string    `json:"channel,omitempty"`
	CapturedAt time.Time `json:"captured_at"`
}

// ArtifactRead is the CLI's view of GET /v1/artifacts/{id}: the metadata a
// reader needs in order to decide what to fetch next. It is deliberately a
// different shape from Artifact (a create response, which the CLI prints
// verbatim): a read also carries the agent handle and, for a bundle, the
// member list, neither of which a create has any reason to repeat.
type ArtifactRead struct {
	ID            string     `json:"id"`
	URL           string     `json:"url"`
	MCP           string     `json:"mcp,omitempty"`
	ShareType     string     `json:"share_type"`
	Title         string     `json:"title,omitempty"`
	Size          int64      `json:"size"`
	MediaType     string     `json:"media_type"`
	Checksum      string     `json:"checksum,omitempty"`
	Visibility    string     `json:"visibility"`
	Provenance    Provenance `json:"provenance"`
	Tags          []string   `json:"tags,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	ExpiresAt     time.Time  `json:"expires_at"`
	CommentCount  int        `json:"comment_count"`
	ReactionCount int        `json:"reaction_count"`
	// Members lists a bundle's files in bundle order; nil for every other
	// share type, which is what tells a script a `--path` is required.
	Members []Member `json:"members,omitempty"`
}

// GetArtifact fetches an artifact's metadata through GET /v1/artifacts/{id}.
// A read is a link capability (ADR-0007), so no credential is required for an
// artifact whose link is shareable; a token sent anyway also reveals the
// owner-only fields the server gates on identity.
func (c *Client) GetArtifact(ctx context.Context, id string) (*ArtifactRead, error) {
	resp, err := c.Do(ctx, http.MethodGet, "/v1/artifacts/"+url.PathEscape(id), nil, "")
	if err != nil {
		return nil, err
	}
	var art ArtifactRead
	if err := DecodeInto(resp, &art); err != nil {
		return nil, err
	}
	return &art, nil
}

// OpenBody streams an artifact's raw bytes from GET /v1/artifacts/{id}/body.
// The caller closes the returned reader.
func (c *Client) OpenBody(ctx context.Context, id string) (io.ReadCloser, BodyMeta, error) {
	return c.open(ctx, "/v1/artifacts/"+url.PathEscape(id)+"/body")
}

// OpenMember streams one bundle member's raw bytes from
// GET /v1/artifacts/{id}/members/<name>. Each path segment of the name is
// escaped separately so a stored name containing a slash still addresses the
// member the server stored rather than resolving to a different path.
func (c *Client) OpenMember(ctx context.Context, id, name string) (io.ReadCloser, BodyMeta, error) {
	segments := strings.Split(name, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	return c.open(ctx, "/v1/artifacts/"+url.PathEscape(id)+"/members/"+strings.Join(segments, "/"))
}

// open performs a streaming GET: on success the body is returned unread for
// the caller to consume, so unlike DecodeInto it must never be drained by the
// status check.
func (c *Client) open(ctx context.Context, path string) (io.ReadCloser, BodyMeta, error) {
	resp, err := c.Do(ctx, http.MethodGet, path, nil, "")
	if err != nil {
		return nil, BodyMeta{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, BodyMeta{}, DecodeError(resp)
	}
	meta := BodyMeta{SHA256: resp.Header.Get(ChecksumHeader)}
	if resp.ContentLength > 0 {
		meta.Size = resp.ContentLength
	}
	return resp.Body, meta, nil
}
