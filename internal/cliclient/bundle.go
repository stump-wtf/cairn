package cliclient

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
)

// BundleFile is one member of a bundle create request.
type BundleFile struct {
	// Path is the local filesystem path, used only for the multipart
	// filename and error messages — the server assigns everything else.
	Path string
	Body io.Reader
	Size int64
}

// CreateBundleOptions carries the bundle-level create request fields
// (SPEC-0008 "Bundle Creation", `--title`/`--ttl`).
type CreateBundleOptions struct {
	// Title is an optional bundle title.
	Title string
	// TTLSeconds is an optional explicit expiry request
	// (X-Cairn-Ttl-Seconds), zero meaning "let the server assign the
	// default TTL" — see CreateArtifactOptions.TTLSeconds for the rationale.
	TTLSeconds int64
}

// CreateBundle POSTs all files as a single multipart/form-data request to
// /v1/artifacts (SPEC-0008 "Bundle Creation"). Sending every member in one
// request makes the bundle atomic by construction: the server either
// accepts the complete multipart body and returns 201, or rejects the whole
// request and no partial bundle is ever created — satisfying "the CLI MUST
// NOT report success or emit a link unless the server confirms the complete
// bundle was created."
//
// Callers that want bounded-concurrent local file preparation with per-file
// progress (cairn#22, SPEC-0008 "Concurrency Safety") build files with
// PrepareBundleFilesConcurrent first; by the time they land here each
// BundleFile.Body is already a fully-buffered in-memory reader, so encoding
// and sending the one multipart request is fast regardless of source disk
// latency.
func (c *Client) CreateBundle(ctx context.Context, files []BundleFile, opts CreateBundleOptions) (*Artifact, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	if opts.Title != "" {
		if err := w.WriteField("title", opts.Title); err != nil {
			return nil, fmt.Errorf("cliclient: encode bundle title: %w", err)
		}
	}
	for _, f := range files {
		part, err := w.CreateFormFile("file", filepath.Base(f.Path))
		if err != nil {
			return nil, fmt.Errorf("cliclient: encode bundle part %s: %w", f.Path, err)
		}
		if _, err := io.Copy(part, f.Body); err != nil {
			return nil, fmt.Errorf("cliclient: read %s into bundle: %w", f.Path, err)
		}
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("cliclient: close bundle encoder: %w", err)
	}

	req, err := c.newRequest(ctx, http.MethodPost, "/v1/artifacts", &buf, w.FormDataContentType())
	if err != nil {
		return nil, err
	}
	if opts.TTLSeconds > 0 {
		req.Header.Set("X-Cairn-Ttl-Seconds", strconv.FormatInt(opts.TTLSeconds, 10))
	}
	resp, err := c.send(req)
	if err != nil {
		return nil, err
	}
	var art Artifact
	if err := DecodeInto(resp, &art); err != nil {
		return nil, err
	}
	return &art, nil
}

// OpenBundleFiles stats and opens each de-duplicated path, returning
// BundleFile values ready for CreateBundle. Callers must close the returned
// files (via CloseBundleFiles) once the request completes. A missing file,
// a directory, or a duplicate path is a client-side usage error — the CLI
// checks these locally before ever contacting the server (SPEC-0008
// "Oversize and Duplicate File Handling").
func OpenBundleFiles(paths []string) (files []BundleFile, closeAll func(), err error) {
	opened := make([]*os.File, 0, len(paths))
	closeAll = func() {
		for _, f := range opened {
			_ = f.Close()
		}
	}

	seen := make(map[string]bool, len(paths))
	for _, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			closeAll()
			return nil, nil, fmt.Errorf("resolve %s: %w", p, err)
		}
		if seen[abs] {
			continue // de-duplicated, not uploaded twice
		}
		seen[abs] = true

		f, err := os.Open(p)
		if err != nil {
			closeAll()
			return nil, nil, fmt.Errorf("open %s: %w", p, err)
		}
		opened = append(opened, f)

		info, err := f.Stat()
		if err != nil {
			closeAll()
			return nil, nil, fmt.Errorf("stat %s: %w", p, err)
		}
		if info.IsDir() {
			closeAll()
			return nil, nil, fmt.Errorf("%s is a directory; pass files, not directories", p)
		}
		files = append(files, BundleFile{Path: p, Body: f, Size: info.Size()})
	}
	return files, closeAll, nil
}
