// Package redact finds credentials in text before Cairn stores it, and either
// refuses the write or masks each value as [REDACTED].
//
// # Ingest Redaction
//
// A Scanner wraps one gitleaks v8 detector, built once at startup from
// gitleaks' default ruleset plus the credential shapes Harness's
// internal/redact masks (cairn-gitleaks.toml), and shared by every request.
// Text scans an in-memory field; Staged scans a staged object in overlapping
// windows and, in mask mode, writes a masked copy. gitleaks types never leave
// this package, and Finding has no field that could carry a secret, so a
// detected value cannot reach a log line, an error string or a response by
// construction.
//
// Every failure fails closed. A detector panic or error, a cancelled context,
// or a finding the masker cannot locate in the original text rejects the
// field rather than storing it unscanned. After masking, the masked output is
// scanned again, and anything still detected rejects the field.
//
// Governing: ADR-0023, SPEC-0017 RD-2, RD-3, RD-7, RD-8, RD-9
//
// @joestump 09/23/2026 - Added for cairn#289. No call sites yet; the wiring
// stories use it.
package redact

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/rs/zerolog"
	"github.com/zricethezav/gitleaks/v8/config"
	"github.com/zricethezav/gitleaks/v8/detect"
)

// Mask replaces every detected value. It is Harness's mask string, so masked
// text reads the same in both tools.
const Mask = "[REDACTED]"

// Mode is what a scan does with a hit.
type Mode string

const (
	// ModeReject refuses the field.
	ModeReject Mode = "reject"
	// ModeMask replaces each detected value with Mask and keeps the rest.
	ModeMask Mode = "mask"
)

// Status is a recorded scan outcome (SPEC-0017 RD-9).
type Status string

const (
	StatusClean              Status = "clean"
	StatusMasked             Status = "masked"
	StatusNotScannedBinary   Status = "not_scanned_binary"
	StatusNotScannedOversize Status = "not_scanned_oversize"
	StatusUnscanned          Status = "unscanned"
)

// Violation reasons (SPEC-0017 RD-11).
const (
	ReasonSecretDetected = "secret_detected"
	ReasonTooLargeToScan = "too_large_to_scan"
)

// Sentinel errors. A *Rejection wraps one of the first two together with
// errs.ErrValidation; ErrScanFailed marks an internal failure, which the
// transport renders as internal and never as a validation error.
var (
	ErrSecretDetected = errors.New("secret detected")
	ErrTooLargeToScan = errors.New("too large to scan")
	ErrScanFailed     = errors.New("redaction scan failed")
)

// Finding locates one detected value. There is deliberately no secret, match,
// line-text or fingerprint field (SPEC-0017 RD-9).
type Finding struct {
	Rule   string // gitleaks rule ID
	Line   int    // 1-based
	Column int    // 1-based, in characters; 0 when unknown
}

// Outcome is a scan's recordable result: statuses, counts and rule IDs only.
type Outcome struct {
	Status   Status
	Count    int
	Rules    map[string]int // rule ID -> count
	Findings []Finding      // for rejection errors only; never persist these
}

// Scanner is the shared, concurrency-safe detector.
type Scanner struct {
	det      *detect.Detector
	maxBytes int64
	oversize OversizePolicy

	// window and overlap size the windowed scan. Fixed in production; tests
	// shrink them.
	window  int
	overlap int
}

const (
	defaultWindow  = 1 << 20  // SPEC-0017 RD-7: fields over 1 MiB are windowed
	defaultOverlap = 64 << 10 // at least 4 KiB (RD-7); a token up to half of this is seen whole
)

var silenceOnce sync.Once

// silenceGitleaksLogging disables zerolog, which gitleaks logs through. Its
// trace and debug lines quote matched values and decoded segments, so they
// must never reach any output. Cairn itself logs with slog.
func silenceGitleaksLogging() {
	silenceOnce.Do(func() { zerolog.SetGlobalLevel(zerolog.Disabled) })
}

// New builds the Scanner. Any failure, including an operator allowlist that
// does not parse or names an unsupported entry, is an error, and cairnd must
// not start (SPEC-0017 RD-2).
func New(c Config) (*Scanner, error) {
	silenceGitleaksLogging()

	maxBytes := c.MaxScanBytes
	switch {
	case maxBytes == 0:
		maxBytes = DefaultMaxScanBytes
	case maxBytes < 0:
		return nil, fmt.Errorf("redact: max scan bytes must be positive, got %d", maxBytes)
	}
	oversize := c.Oversize
	switch oversize {
	case "":
		oversize = OversizeReject
	case OversizeReject, OversizeStoreUnscanned:
	default:
		return nil, fmt.Errorf("redact: oversize policy must be %q or %q, got %q", OversizeReject, OversizeStoreUnscanned, oversize)
	}

	al, err := loadAllowlist(c.AllowlistFile)
	if err != nil {
		return nil, err
	}
	cfg, err := buildConfig(al, c.AllowlistFile)
	if err != nil {
		return nil, err
	}
	det, err := newDetector(cfg)
	if err != nil {
		return nil, err
	}
	return &Scanner{
		det:      det,
		maxBytes: maxBytes,
		oversize: oversize,
		window:   defaultWindow,
		overlap:  defaultOverlap,
	}, nil
}

// newDetector pins the four settings whose gitleaks defaults are wrong for an
// ingest scanner (ADR-0023). No .gitleaksignore or baseline is ever loaded.
func newDetector(cfg config.Config) (d *detect.Detector, err error) {
	defer func() {
		if r := recover(); r != nil {
			d, err = nil, errors.New("redact: building the detector panicked")
		}
	}()
	d = detect.NewDetector(cfg)
	d.IgnoreGitleaksAllow = true // the uploader must not exempt itself with a comment
	d.MaxTargetMegaBytes = 0     // Cairn's cap applies; gitleaks never skips silently
	d.MaxArchiveDepth = 0        // Cairn does not unpack archives
	d.MaxDecodeDepth = DefaultMaxDecodeDepth
	return d, nil
}

// MaxScanBytes reports the per-field scan cap.
func (s *Scanner) MaxScanBytes() int64 { return s.maxBytes }

// Oversize reports the oversize policy.
func (s *Scanner) Oversize() OversizePolicy { return s.oversize }

// Text scans in. With no hit it returns in unchanged and StatusClean. On a hit
// in mask mode it returns the masked text and StatusMasked; in reject mode it
// returns "" and a *Rejection naming field. A field over the cap is rejected
// (too_large_to_scan) or, under store_unscanned, returned unchanged with
// StatusNotScannedOversize.
func (s *Scanner) Text(ctx context.Context, field string, in string, mode Mode) (string, Outcome, error) {
	if err := checkMode(mode); err != nil {
		return "", Outcome{}, err
	}
	if int64(len(in)) > s.maxBytes {
		return s.oversized(field, in, int64(len(in)))
	}
	hits, err := s.scanStream(ctx, source{text: in})
	if err != nil {
		return "", Outcome{}, s.failed(field, err)
	}
	if len(hits) == 0 {
		return in, Outcome{Status: StatusClean}, nil
	}
	o := outcomeOf(hits)
	if mode == ModeReject {
		return "", o, &Rejection{Field: field, Reason: ReasonSecretDetected, Findings: o.Findings}
	}
	if unlocatable(hits) {
		return "", o, &Rejection{Field: field, Reason: ReasonSecretDetected, Findings: o.Findings, Unmaskable: true}
	}
	out, again, err := s.maskText(ctx, in, hits)
	if err != nil {
		return "", Outcome{}, s.failed(field, err)
	}
	if len(again) > 0 {
		return "", o, &Rejection{Field: field, Reason: ReasonSecretDetected, Findings: outcomeOf(again).Findings, Unmaskable: true}
	}
	o.Status = StatusMasked
	return out, o, nil
}

// maskText masks in, narrow first and then wide (see maskRanges), and
// re-scans each result. It returns the first result the detector finds clean,
// or the findings that survived the wide mask, which the caller rejects:
// anything still detectable after masking is never stored.
func (s *Scanner) maskText(ctx context.Context, in string, hits []hit) (string, []hit, error) {
	var again []hit
	for _, wide := range []bool{false, true} {
		ranges, err := s.maskRanges(ctx, source{text: in}, hits, wide)
		if err != nil {
			return "", nil, err
		}
		var b strings.Builder
		b.Grow(len(in))
		if err := writeMasked(&b, strings.NewReader(in), ranges); err != nil {
			return "", nil, err
		}
		out := b.String()
		if again, err = s.scanStream(ctx, source{text: out}); err != nil {
			return "", nil, err
		}
		if len(again) == 0 {
			return out, nil, nil
		}
	}
	return "", again, nil
}

func (s *Scanner) oversized(field string, in string, size int64) (string, Outcome, error) {
	if s.oversize == OversizeStoreUnscanned {
		return in, Outcome{Status: StatusNotScannedOversize}, nil
	}
	return "", Outcome{}, &Rejection{Field: field, Reason: ReasonTooLargeToScan, Limit: s.maxBytes, Size: size}
}

// failed wraps an internal scan failure. The cause is kept for errors.Is
// (context cancellation) but it never carries content.
func (s *Scanner) failed(field string, err error) error {
	return fmt.Errorf("redact %s: %w: %w", field, ErrScanFailed, err)
}

func checkMode(m Mode) error {
	switch m {
	case ModeReject, ModeMask:
		return nil
	}
	return fmt.Errorf("redact: unknown mode %q: %w", m, ErrScanFailed)
}

func unlocatable(hits []hit) bool {
	for _, h := range hits {
		if h.start < 0 {
			return true
		}
	}
	return false
}

// outcomeOf counts findings per rule and lists their locations, in text order.
func outcomeOf(hits []hit) Outcome {
	o := Outcome{Count: len(hits), Rules: make(map[string]int, len(hits))}
	for _, h := range hits {
		o.Rules[h.rule]++
		o.Findings = append(o.Findings, Finding{Rule: h.rule, Line: h.line, Column: h.col})
	}
	sort.SliceStable(o.Findings, func(i, j int) bool {
		a, b := o.Findings[i], o.Findings[j]
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Column < b.Column
	})
	return o
}
