package cliclient

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
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
	// Tags echo what the server stored (normalized), so `--json` shows them.
	Tags []string `json:"tags,omitempty"`

	// RedactionStatus, Redacted and Redactions are the ingest secret scan's
	// outcome, which a create response carries for its owner (SPEC-0017
	// RD-9). Redacted is what tells a writer that the stored bytes, and so
	// the stored SHA-256, differ from what was sent (RD-10). All three are
	// absent from a server that predates scanning.
	RedactionStatus string      `json:"redaction_status,omitempty"`
	Redacted        *bool       `json:"redacted,omitempty"`
	Redactions      *Redactions `json:"redactions,omitempty"`
}

// Redactions counts the values the server masked, in total and per rule ID.
// It never holds a value (SPEC-0017 RD-9).
type Redactions struct {
	Count int            `json:"count"`
	Rules map[string]int `json:"rules"`
}

// WasRedacted reports whether the server masked anything in the stored copy.
func (a *Artifact) WasRedacted() bool {
	return a != nil && a.Redacted != nil && *a.Redacted
}

// RedactionHeader carries a writer's downgrade of a reject-mode create to
// mask (SPEC-0017 RD-5), and RedactionMask is the only value it takes.
//
// Governing: ADR-0023, SPEC-0017 RD-5
const (
	RedactionHeader = "X-Cairn-Redaction"
	RedactionMask   = "mask"
)

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
	// Tags are client-asserted routing strings (`cairn --tag`, ADR-0018), sent
	// as one comma-separated X-Cairn-Tags header. No tag may contain a comma, so
	// the list survives an intermediary folding repeated headers intact.
	Tags []string
	// Redaction is the writer's downgrade (`--redact=mask`), sent as
	// X-Cairn-Redaction when set. The CLI checks it before sending; the
	// server is still the judge (SPEC-0017 RD-5).
	Redaction string
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
	if len(opts.Tags) > 0 {
		req.Header.Set("X-Cairn-Tags", strings.Join(opts.Tags, ","))
	}
	if opts.Redaction != "" {
		req.Header.Set(RedactionHeader, opts.Redaction)
	}
	return c.send(req)
}
