package store

// Ingest Secret Scan
//
// Every artifact and bundle create runs its title and each body through the
// redact.Scanner before anything is stored (SPEC-0017 RD-1): after the body
// has streamed to staging and any declared checksum has been verified against
// the received bytes (RD-10), and before CommitBlob promotes it. A hit is
// masked or rejected according to the create's share type (RD-4, RD-5):
//
//   - mask: the masked copy replaces the raw staging object, which is removed,
//     and the stored SHA-256, size and dedup key become the masked bytes'.
//   - reject: the create returns a secret_detected violation naming the field,
//     rule, line and column, never the value (RD-11). The deferred Discard
//     removes the staging object, and nothing is committed or emitted.
//
// A binary body is not scanned (RD-6). A text body over the scan cap is
// rejected, or, under CAIRN_REDACTION_OVERSIZE=store_unscanned, stored with a
// WARN (RD-7). A scan that fails, or a Store built without a scanner, fails the
// create closed as an internal error: nothing is ever stored unscanned because
// the scan could not run.
//
// Governing: ADR-0023, SPEC-0017 RD-1, RD-4, RD-5, RD-6, RD-7, RD-10, RD-11,
// REQ "Error Handling Standards"; ADR-0008
//
// @joestump 09/26/2026 - Added for cairn#292.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/metrics"
	"github.com/stump-wtf/cairn/internal/objectstore"
	"github.com/stump-wtf/cairn/internal/redact"
	"github.com/stump-wtf/cairn/internal/sharetype"
)

// DefaultRedactionRejectTypes are the share types whose creates refuse a
// detected secret by default (SPEC-0017 RD-4). Every other type masks.
var DefaultRedactionRejectTypes = []string{string(sharetype.KeyCode), string(artifact.TypeBundle)}

// scanInMemoryBytes is the largest body kept in memory as it streams and
// scanned from there. A larger body is scanned back from staging in windows.
const scanInMemoryBytes = 1 << 20

// The caller-facing names of the scanned fields (SPEC-0019 VE-1).
const (
	titleField = "title"
	bodyField  = "body"
)

// memberContentField names bundle member i's content, as SPEC-0017 RD-4
// requires a bundle rejection to.
func memberContentField(i int) string { return fmt.Sprintf("members[%d].content", i) }

// errNoScanner fails a create on a Store built without a scanner.
var errNoScanner = fmt.Errorf("no ingest secret scanner is configured: %w", redact.ErrScanFailed)

func rejectTypesOf(types []string) []artifact.ShareType {
	if types == nil {
		types = DefaultRedactionRejectTypes
	}
	out := make([]artifact.ShareType, 0, len(types))
	for _, t := range types {
		out = append(out, artifact.ShareType(t))
	}
	return out
}

// redactionMode resolves a create's mode (ADR-0023): reject when the share
// type is listed in CAIRN_REDACTION_REJECT_TYPES and the writer did not
// downgrade, mask otherwise. A writer can only turn reject into mask; nothing a
// request carries turns scanning off (RD-5).
func (s *Store) redactionMode(t artifact.ShareType, downgrade bool) redact.Mode {
	if !downgrade && slices.Contains(s.rejectTypes, t) {
		return redact.ModeReject
	}
	return redact.ModeMask
}

// scanReport collects what a create's scans leave to do after it commits.
type scanReport struct {
	unscanned []unscannedField
}

// unscannedField is a field stored unscanned under store_unscanned.
type unscannedField struct {
	field string
	size  int64
}

func (r *scanReport) note(field string, size int64, o redact.Outcome) {
	if o.Status == redact.StatusNotScannedOversize {
		r.unscanned = append(r.unscanned, unscannedField{field: field, size: size})
	}
}

// warnUnscanned logs one WARN per field stored unscanned, with the artifact's
// id and the field's size, never its content (SPEC-0017 RD-7). It runs after
// the commit, when the artifact exists.
func (s *Store) warnUnscanned(publicID string, r *scanReport) {
	for _, f := range r.unscanned {
		s.log.Warn("redaction: stored a field WITHOUT a credential scan because it is over the scan cap (CAIRN_REDACTION_OVERSIZE=store_unscanned)",
			"artifact", publicID, "field", f.field, "size_bytes", f.size, "max_scan_bytes", s.scanner.MaxScanBytes())
	}
}

// scanTitle scans a create's title with the create's mode, and returns the
// title to store. An empty title has nothing to scan and is clean.
func (s *Store) scanTitle(ctx context.Context, surface metrics.RedactionSurface, title string, mode redact.Mode, rep *scanReport) (string, redact.Summary, error) {
	if s.scanner == nil {
		s.metrics.ObserveRedaction(surface, redact.Outcome{}, errNoScanner)
		return "", redact.Summary{}, errNoScanner
	}
	if title == "" {
		return "", redact.Summary{Status: redact.StatusClean}, nil
	}
	out, o, err := s.scanner.Text(ctx, titleField, title, mode)
	s.metrics.ObserveRedaction(surface, o, err)
	if err != nil {
		return "", redact.Summary{}, scanErr(titleField, err)
	}
	rep.note(titleField, int64(len(title)), o)
	return out, o.Summary(), nil
}

// scanBody scans a staged body in place. On a mask, b is repointed at the
// masked copy (replaceStaged). A rejection is returned as a *errs.Invalid
// naming field; any other error is a failed scan.
func (s *Store) scanBody(ctx context.Context, surface metrics.RedactionSurface, field string, b *StagedBlob, mode redact.Mode, rep *scanReport) (redact.Summary, error) {
	if s.scanner == nil {
		s.metrics.ObserveRedaction(surface, redact.Outcome{}, errNoScanner)
		return redact.Summary{}, errNoScanner
	}
	var (
		o   redact.Outcome
		err error
	)
	switch {
	case !isTextBody(b.MediaType, b.head, b.Size):
		o = redact.Outcome{Status: redact.StatusNotScannedBinary}
	case b.body != nil:
		o, err = s.scanKept(ctx, field, b, mode)
	default:
		o, err = s.scanStaged(ctx, b, mode)
	}
	s.metrics.ObserveRedaction(surface, o, err)
	if err != nil {
		return redact.Summary{}, scanErr(field, err)
	}
	rep.note(field, b.Size, o)
	return o.Summary(), nil
}

// scanKept scans a body kept in memory, and stages its masked copy on a mask.
func (s *Store) scanKept(ctx context.Context, field string, b *StagedBlob, mode redact.Mode) (redact.Outcome, error) {
	out, o, err := s.scanner.Text(ctx, field, string(b.body), mode)
	if err != nil || o.Status != redact.StatusMasked {
		return o, err
	}
	key, err := newStagingKey()
	if err != nil {
		return o, err
	}
	if err := s.obj.Put(ctx, key, strings.NewReader(out), int64(len(out)), b.MediaType); err != nil {
		_ = s.obj.Remove(context.WithoutCancel(ctx), key)
		return o, fmt.Errorf("stage the masked body: %w", err)
	}
	sum := sha256.Sum256([]byte(out))
	s.useMasked(ctx, b, key, hex.EncodeToString(sum[:]), int64(len(out)))
	return o, nil
}

// scanStaged scans a body back from staging in windows, and adopts the masked
// copy the scanner staged on a mask.
func (s *Store) scanStaged(ctx context.Context, b *StagedBlob, mode redact.Mode) (redact.Outcome, error) {
	key, o, err := s.scanner.Staged(ctx, s.obj, b.stagingKey, b.Size, mode)
	if err != nil || key == "" {
		return o, err
	}
	sha, size, err := hashObject(ctx, s.obj, key)
	if err != nil {
		_ = s.obj.Remove(context.WithoutCancel(ctx), key)
		return o, fmt.Errorf("hash the masked body: %w", err)
	}
	s.useMasked(ctx, b, key, sha, size)
	return o, nil
}

// useMasked repoints b at its masked copy. A raw staging object that cannot be
// removed is never promoted and is swept by the staging lifecycle rule, so the
// failure is logged, without the key's content, rather than failing the create.
func (s *Store) useMasked(ctx context.Context, b *StagedBlob, key, sha string, size int64) {
	if err := b.replaceStaged(ctx, s.obj, key, sha, size); err != nil {
		s.log.Warn("redaction: could not remove an unmasked staging object; the staging lifecycle rule will", "error", err)
	}
}

// hashObject streams key's bytes through SHA-256, never holding them whole.
func hashObject(ctx context.Context, obj objectstore.ObjectStore, key string) (string, int64, error) {
	rc, err := obj.Get(ctx, key)
	if err != nil {
		return "", 0, err
	}
	defer rc.Close()
	h := sha256.New()
	n, err := io.Copy(h, rc)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// scanErr turns a scanner rejection into the SPEC-0019 violation the
// transports render, naming field (the scanner leaves a staged body's field
// empty). Any other error is a failed scan and stays internal, so the create
// fails closed.
func scanErr(field string, err error) error {
	var rej *redact.Rejection
	if !errors.As(err, &rej) {
		return fmt.Errorf("scan %s: %w", field, err)
	}
	rej.Field = field
	return RejectionViolation(rej)
}

// RejectionViolation converts a scanner rejection to its SPEC-0019 violation:
// too_large_to_scan with the cap in bytes, or one secret_detected per finding
// with its rule, line and column (SPEC-0017 RD-11). A Finding holds no value,
// so neither can the violation. The rejection stays in the chain, so
// errors.Is(err, redact.ErrSecretDetected) and errors.As to *redact.Rejection
// still hold.
//
// Governing: ADR-0023, SPEC-0017 RD-11; ADR-0025, SPEC-0019 VE-1, VE-2
func RejectionViolation(rej *redact.Rejection) *errs.Invalid {
	if rej.Reason == redact.ReasonTooLargeToScan {
		return errs.Violate(rej.Field, errs.LocBody, errs.ReasonTooLargeToScan,
			errs.WithLimit(rej.Limit, errs.UnitBytes)).Because(rej)
	}
	vs := make([]*errs.Invalid, 0, len(rej.Findings)+1)
	for _, f := range rej.Findings {
		opts := []errs.Opt{errs.WithExtra("rule", f.Rule)}
		if f.Line > 0 {
			opts = append(opts, errs.WithExtra("line", f.Line))
		}
		if f.Column > 0 {
			opts = append(opts, errs.WithExtra("column", f.Column))
		}
		vs = append(vs, errs.Violate(rej.Field, errs.LocBody, errs.ReasonSecretDetected, opts...))
	}
	if len(vs) == 0 {
		vs = append(vs, errs.Violate(rej.Field, errs.LocBody, errs.ReasonSecretDetected))
	}
	return errs.Join(vs...).Because(rej)
}

// Binary Content (SPEC-0017 RD-6)
//
// A body is text, and scanned, when its resolved media type is textual or its
// first 8 KiB are valid UTF-8 with no NUL, whatever was declared. Otherwise a
// body whose leading bytes match a known binary signature is not scanned and
// is recorded not_scanned_binary. A body that is neither is scanned anyway:
// an unrecognised format is not evidence that it holds no text.

// textualMedia are the non-text/* media types RD-6 treats as text.
var textualMedia = []string{
	"application/json", "application/xml", "application/yaml", "application/toml",
	"application/javascript", "application/x-sh",
}

func isTextBody(mediaType string, head []byte, size int64) bool {
	if isTextualMedia(mediaType) || utf8Head(head, size) {
		return true
	}
	return !binarySignature(head)
}

func isTextualMedia(mediaType string) bool {
	mt, _, err := mime.ParseMediaType(mediaType)
	if err != nil {
		return false
	}
	return strings.HasPrefix(mt, "text/") || slices.Contains(textualMedia, mt) ||
		strings.HasSuffix(mt, "+json") || strings.HasSuffix(mt, "+xml")
}

// utf8Head reports whether head, the first bytes of a body of size bytes, is
// valid UTF-8 with no NUL. A head cut short of the body may end inside a
// multi-byte character; that partial character is not held against it.
func utf8Head(head []byte, size int64) bool {
	if bytes.IndexByte(head, 0) >= 0 {
		return false
	}
	if int64(len(head)) < size {
		for i := 1; i <= utf8.UTFMax && i <= len(head); i++ {
			if utf8.RuneStart(head[len(head)-i]) {
				if !utf8.FullRune(head[len(head)-i:]) {
					head = head[:len(head)-i]
				}
				break
			}
		}
	}
	return utf8.Valid(head)
}

// binaryMedia are the formats http.DetectContentType recognises, outside the
// image, audio, video and font families, that are never scanned.
var binaryMedia = []string{
	"application/pdf", "application/zip", "application/x-gzip", "application/wasm",
	"application/vnd.ms-fontobject", "application/x-rar-compressed", "application/ogg",
}

// binaryMagic are further signatures http.DetectContentType does not know.
var binaryMagic = [][]byte{
	[]byte("7z\xbc\xaf\x27\x1c"),       // 7-Zip
	[]byte("\xfd7zXZ\x00"),             // xz
	[]byte("BZh"),                      // bzip2
	[]byte("\x28\xb5\x2f\xfd"),         // zstd
	[]byte("\x7fELF"),                  // ELF
	[]byte("\xfe\xed\xfa\xce"),         // Mach-O
	[]byte("\xfe\xed\xfa\xcf"),         // Mach-O 64
	[]byte("\xce\xfa\xed\xfe"),         // Mach-O, little-endian
	[]byte("\xcf\xfa\xed\xfe"),         // Mach-O 64, little-endian
	[]byte("\xca\xfe\xba\xbe"),         // Mach-O universal, Java class
	[]byte("MZ"),                       // PE / DOS executable
	[]byte("SQLite format 3\x00"),      // SQLite
	[]byte("PAR1"),                     // Parquet
	[]byte("\xd0\xcf\x11\xe0\xa1\xb1"), // OLE2 (legacy Office)
}

// binarySignature reports whether head starts like a known binary format. It
// is only consulted for a head that is not UTF-8 text, so an ASCII signature
// ("MZ", "BZh") cannot claim a text file.
func binarySignature(head []byte) bool {
	if len(head) == 0 {
		return false
	}
	ct := http.DetectContentType(head)
	for _, family := range []string{"image/", "audio/", "video/", "font/"} {
		if strings.HasPrefix(ct, family) {
			return true
		}
	}
	if slices.Contains(binaryMedia, ct) {
		return true
	}
	for _, sig := range binaryMagic {
		if bytes.HasPrefix(head, sig) {
			return true
		}
	}
	// A tar archive's magic sits at offset 257.
	return len(head) >= 262 && string(head[257:262]) == "ustar"
}
