package cliclient

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"time"
)

// Artifact is the CLI's view of the server's artifact response
// (internal/httpapi/api.go artifactResponse), trimmed to the fields the CLI
// displays. The CLI never recomputes any of these — they are server-decided
// (SPEC-0008 "Data the CLI shows but never decides").
type Artifact struct {
	ID         string    `json:"id"`
	URL        string    `json:"url"`
	ShareType  string    `json:"share_type"`
	Title      string    `json:"title,omitempty"`
	Size       int64     `json:"size"`
	MediaType  string    `json:"media_type"`
	Visibility string    `json:"visibility"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// CreateArtifactOptions carries the raw-body single-artifact create request
// (SPEC-0008 "Pipe and Path Ingest"). The server assigns share type,
// provenance, access, and expiry; the CLI supplies only the body and the
// handful of client-known hints (title, declared media type, checksum) it
// already forwards today in ADR-0012's REST surface.
type CreateArtifactOptions struct {
	// Title is an optional display title (X-Cairn-Title).
	Title string
	// MediaType is the client-declared Content-Type; the server may
	// re-sniff it, but a caller with an obvious extension can help.
	MediaType string
	// SHA256 is an optional pre-computed checksum (X-Cairn-Sha256) the
	// server verifies against the streamed body.
	SHA256 string
	// TTLSeconds is an optional explicit expiry request (`cairn --ttl`,
	// X-Cairn-Ttl-Seconds), zero meaning "let the server assign the default
	// TTL" (SPEC-0008 "Data the CLI shows but never decides" — the CLI only
	// forwards what the human explicitly asked for; the server remains
	// authoritative and may reject an out-of-bounds request).
	TTLSeconds int64
}

// CreateArtifact streams body to POST /v1/artifacts and decodes the created
// Artifact. It streams rather than buffers the whole payload (SPEC-0008
// "streaming the body to the server rather than buffering the whole payload
// in memory where feasible"): body is passed straight through to the
// underlying http.Request.
func (c *Client) CreateArtifact(ctx context.Context, body io.Reader, opts CreateArtifactOptions) (*Artifact, error) {
	contentType := opts.MediaType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	resp, err := c.doCreate(ctx, "/v1/artifacts", body, contentType, opts)
	if err != nil {
		return nil, err
	}
	var art Artifact
	if err := DecodeInto(resp, &art); err != nil {
		return nil, err
	}
	return &art, nil
}

func (c *Client) doCreate(ctx context.Context, path string, body io.Reader, contentType string, opts CreateArtifactOptions) (*http.Response, error) {
	req, err := c.newRequest(ctx, http.MethodPost, path, body, contentType)
	if err != nil {
		return nil, err
	}
	if opts.Title != "" {
		req.Header.Set("X-Cairn-Title", opts.Title)
	}
	if opts.SHA256 != "" {
		req.Header.Set("X-Cairn-Sha256", opts.SHA256)
	}
	if opts.TTLSeconds > 0 {
		req.Header.Set("X-Cairn-Ttl-Seconds", strconv.FormatInt(opts.TTLSeconds, 10))
	}
	return c.send(req)
}
