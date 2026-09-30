package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"

	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/objectstore"
)

// stagedBlob is the result of streaming a body into a temporary staging object:
// its content hash, byte size, resolved media type, and the staging key that
// must be promoted (to its content-addressed key) or discarded (on dedup/error).
type stagedBlob struct {
	sha256     string
	size       int64
	mediaType  string
	stagingKey string
	// head is the body's first sniffBytes, for the SPEC-0017 RD-6 text or
	// binary decision. body is the whole body when it is at most the keep
	// limit the caller asked for, so the ingest scan reads a small body from
	// memory instead of back from staging; nil otherwise.
	head []byte
	body []byte
}

// streamBlob streams r into a staging object while computing its SHA-256
// incrementally, enforcing maxBytes as bytes arrive, and sniffing the media
// type from the leading bytes. It does not touch the database, so it is the
// pure, unit-testable core of streaming ingest.
//
// On any error (including an oversize body or a cancelled context) it removes
// the partial staging object and returns a wrapped error whose domain code is
// preserved (e.g. errs.ErrTooLarge -> payload_too_large).
//
// Governing: ADR-0008 (Storage & Content Model),
// SPEC-0002 REQ "Streaming Upload with Checksum Verification",
// SPEC-0002 REQ "Concurrency Safety for Streaming Uploads"
func streamBlob(ctx context.Context, obj objectstore.ObjectStore, r io.Reader, maxBytes int64, declaredMedia string) (stagedBlob, error) {
	return streamBlobKeep(ctx, obj, r, maxBytes, declaredMedia, 0)
}

// streamBlobKeep is streamBlob that also keeps a body of at most keep bytes
// in memory (stagedBlob.body) for the ingest scan: SPEC-0017 scans bodies of
// 1 MiB and under from memory. A longer body keeps nothing.
func streamBlobKeep(ctx context.Context, obj objectstore.ObjectStore, r io.Reader, maxBytes int64, declaredMedia string, keep int64) (stagedBlob, error) {
	key, err := newStagingKey()
	if err != nil {
		return stagedBlob{}, err
	}

	h := sha256.New()
	lim := &limitedReader{r: r, max: maxBytes}
	sniff := &sniffReader{r: lim}
	kept := &keepWriter{max: keep}
	tee := io.TeeReader(sniff, io.MultiWriter(h, kept))

	if err := obj.Put(ctx, key, tee, -1, declaredMedia); err != nil {
		// Abort the in-flight write; leave nothing but GC-collectable debris.
		_ = obj.Remove(context.Background(), key)
		return stagedBlob{}, err
	}

	media := declaredMedia
	if media == "" || media == "application/octet-stream" {
		media = sniff.contentType()
	}
	return stagedBlob{
		sha256:     hex.EncodeToString(h.Sum(nil)),
		size:       lim.read,
		mediaType:  media,
		stagingKey: key,
		head:       sniff.head,
		body:       kept.bytes(),
	}, nil
}

// keepWriter keeps what is written to it while it totals at most max bytes,
// and drops it all the moment it grows past max.
type keepWriter struct {
	max  int64
	buf  []byte
	over bool
}

func (k *keepWriter) Write(p []byte) (int, error) {
	if k.over || k.max <= 0 {
		return len(p), nil
	}
	if int64(len(k.buf))+int64(len(p)) > k.max {
		k.over, k.buf = true, nil
		return len(p), nil
	}
	k.buf = append(k.buf, p...)
	return len(p), nil
}

// bytes is the kept body, nil when nothing was kept. An empty body that was
// kept is a non-nil empty slice, so "kept and empty" and "not kept" differ.
func (k *keepWriter) bytes() []byte {
	if k.over || k.max <= 0 {
		return nil
	}
	if k.buf == nil {
		return []byte{}
	}
	return k.buf
}

// limitedReader enforces a byte cap incrementally, returning errs.ErrTooLarge
// the moment the stream reads past max — so an oversize upload is rejected
// mid-stream, not after buffering the whole body.
type limitedReader struct {
	r    io.Reader
	max  int64
	read int64
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if l.read > l.max {
		return 0, errs.ErrTooLarge
	}
	// Never read more than one byte past the cap, so we detect the first
	// overflow byte precisely (a body of exactly max is allowed).
	allowed := l.max - l.read + 1
	if int64(len(p)) > allowed {
		p = p[:allowed]
	}
	n, err := l.r.Read(p)
	l.read += int64(n)
	if l.read > l.max {
		return n, errs.ErrTooLarge
	}
	return n, err
}

// sniffBytes is how much of a body's head is captured: the media sniff reads
// the first 512 bytes of it, and the SPEC-0017 RD-6 text check all 8 KiB.
const sniffBytes = 8 << 10

// sniffReader captures the leading bytes of a stream so the media type can be
// detected without buffering the whole body.
type sniffReader struct {
	r    io.Reader
	head []byte
}

func (s *sniffReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if n > 0 && len(s.head) < sniffBytes {
		need := sniffBytes - len(s.head)
		if need > n {
			need = n
		}
		s.head = append(s.head, p[:need]...)
	}
	return n, err
}

func (s *sniffReader) contentType() string {
	if len(s.head) == 0 {
		return "application/octet-stream"
	}
	return http.DetectContentType(s.head)
}

// newStagingKey returns a unique, unguessable key under the staging prefix.
func newStagingKey() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("ingest: staging key: %w", err)
	}
	return "staging/" + hex.EncodeToString(b), nil
}

// shardedKey derives a blob's content-addressed object key from its hash,
// sharded on the first two byte-pairs to avoid hot prefixes (ADR-0008).
func shardedKey(sha string) string {
	return fmt.Sprintf("blobs/%s/%s/%s", sha[0:2], sha[2:4], sha)
}
