package redact

// Windowed Scanning and Masking
//
// A field is scanned in windows of s.window bytes that overlap by s.overlap,
// read through a bufio.Reader (Peek the window, Discard window-overlap), so a
// staged body is never buffered whole (SPEC-0017 RD-7). The object store has
// no range reads, so this is one streaming Get per pass. Each window OWNS the
// half-open range from the middle of its leading overlap to the middle of its
// trailing one, and a finding counts only in the window that owns the byte
// where the value starts. A token shorter than half the overlap is therefore
// seen whole by exactly one owning window, and a straddling token is counted
// once; the start-truncated tail of a token in the next window starts at that
// window's first byte, which it does not own.
//
// Masking replaces byte ranges, not strings. Each finding contributes the
// range its value was located at, plus every other occurrence of the same
// literal value in the field (Harness masks by value, so a token repeated in a
// log is masked everywhere, not only where a rule fired). Overlapping ranges
// are merged, which is the same as replacing longest-first. A decoded finding
// (gitleaks tags them decoded:*) masks the whole encoded run it came from.
// gitleaks' own line and column fields are 0-based lines and a column that is
// off by one after the first line, and its end column can be zero, so offsets
// are recomputed here from the start column, which is exact once inverted.
//
// Governing: ADR-0023, SPEC-0017 RD-3, RD-7, REQ "Concurrency Safety"
//
// @joestump 09/23/2026 - Added for cairn#289.

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/zricethezav/gitleaks/v8/detect"

	"github.com/stump-wtf/cairn/internal/objectstore"
)

var (
	errTooLarge      = errors.New("field exceeds the scan cap")
	errDetectorPanic = errors.New("detector panicked")
)

// hit is one finding inside this package: one value, which one or more rules
// fired on. It holds the literal value only so the masker can find its other
// occurrences; it is never returned, logged or formatted. Offsets are
// absolute byte offsets into the field.
type hit struct {
	rule       string
	start, end int64  // union of parts; start < 0: reported but not locatable
	parts      []part // one per rule firing; nil when unlocatable
	line, col  int
}

// part is one rule's located value.
type part struct {
	start, end int64
	secret     string // the literal value; "" for a decoded finding
}

// rng is a half-open byte range to replace with Mask.
type rng struct{ start, end int64 }

// source is a field to scan: an in-memory string, or a stream.
type source struct {
	text string
	r    io.Reader
}

// window is one scan window.
type window struct {
	text   string
	base   int64 // absolute offset of text[0]
	lo, hi int   // the owned range of text
	pos    position
}

// position is where a window starts, for reporting lines and columns.
type position struct {
	line int // 1-based line at the window's first byte
	col  int // characters since the last newline before the window
}

// at reports the 1-based line and column of text[off].
func (p position) at(text string, nl []int, off int) (int, int) {
	i := sort.SearchInts(nl, off) // newlines before off
	if i == 0 {
		return p.line, p.col + utf8.RuneCountInString(text[:off]) + 1
	}
	return p.line + i, utf8.RuneCountInString(text[nl[i-1]+1:off]) + 1
}

// advance moves p past seg.
func (p position) advance(seg string) position {
	if i := strings.LastIndexByte(seg, '\n'); i >= 0 {
		return position{line: p.line + strings.Count(seg, "\n"), col: utf8.RuneCountInString(seg[i+1:])}
	}
	return position{line: p.line, col: p.col + utf8.RuneCountInString(seg)}
}

// windows calls fn for each window of src, checking ctx between windows.
func (s *Scanner) windows(ctx context.Context, src source, fn func(w window) error) error {
	if src.r == nil && len(src.text) <= s.window {
		if err := ctx.Err(); err != nil {
			return err
		}
		return fn(window{text: src.text, lo: 0, hi: len(src.text), pos: position{line: 1}})
	}
	r := src.r
	if r == nil {
		r = strings.NewReader(src.text)
	}
	br := bufio.NewReaderSize(r, s.window)
	var base int64
	pos := position{line: 1}
	half := s.overlap / 2
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		buf, err := br.Peek(s.window)
		last := false
		switch {
		case err == nil:
		case errors.Is(err, io.EOF):
			last = true
		default:
			return err
		}
		text := string(buf)
		w := window{text: text, base: base, hi: len(text), pos: pos}
		if base > 0 {
			w.lo = half
		}
		if !last {
			w.hi = s.window - half
		}
		if err := fn(w); err != nil {
			return err
		}
		if last {
			return nil
		}
		adv := s.window - s.overlap
		pos = pos.advance(text[:adv])
		if _, err := br.Discard(adv); err != nil {
			return err
		}
		base += int64(adv)
	}
}

// scanStream runs the detector over every window of src and returns the
// deduplicated findings.
func (s *Scanner) scanStream(ctx context.Context, src source) ([]hit, error) {
	var all []hit
	err := s.windows(ctx, src, func(w window) error {
		hs, nl, err := s.findHits(ctx, w.text)
		if err != nil {
			return err
		}
		for _, h := range hs {
			if h.start >= 0 {
				if int(h.start) < w.lo || int(h.start) >= w.hi {
					continue // another window owns it
				}
				h.line, h.col = w.pos.at(w.text, nl, int(h.start))
				h.start += w.base
				h.end += w.base
				for i := range h.parts {
					h.parts[i].start += w.base
					h.parts[i].end += w.base
				}
			}
			all = append(all, h)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return dedupe(all), nil
}

// findHits runs the detector over one window and locates each finding in it.
// Offsets are window-relative. A panic or a cancelled context is an error:
// DetectContext returns partial findings with no error when ctx ends.
func (s *Scanner) findHits(ctx context.Context, text string) (hs []hit, nl []int, err error) {
	defer func() {
		if r := recover(); r != nil {
			hs, nl, err = nil, nil, errDetectorPanic
		}
	}()
	fs := s.det.DetectContext(ctx, detect.Fragment{Raw: text})
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	nl = newlines(text)
	for _, f := range fs {
		h := hit{rule: f.RuleID, start: -1, end: -1}
		start, ok := offsetOf(nl, f.StartLine, f.StartColumn)
		switch {
		case !ok || start >= len(text):
		case isDecoded(f.Tags):
			end := start + encodedRun(text[start:])
			if e, ok := offsetOf(nl, f.EndLine, f.EndColumn+1); ok && f.EndColumn > 0 && e > end && e <= len(text) {
				end = e
			}
			if end > start {
				h.start, h.end = int64(start), int64(end)
				h.parts = []part{{start: h.start, end: h.end}}
			}
		case f.Secret != "":
			if i := locateSecret(text, start, f.Match, f.Secret); i >= 0 {
				h.start, h.end = int64(i), int64(i+len(f.Secret))
				h.parts = []part{{start: h.start, end: h.end, secret: f.Secret}}
			}
		}
		hs = append(hs, h)
	}
	return hs, nl, nil
}

// offsetOf inverts gitleaks' location(): line is the 0-based index into the
// fragment's newline list, and column is counted from the preceding newline
// byte itself (or from 0 on the first line), plus one.
func offsetOf(nl []int, line, col int) (int, bool) {
	if line < 0 || col < 1 || line > len(nl) {
		return 0, false
	}
	base := 0
	if line > 0 {
		base = nl[line-1]
	}
	return base + col - 1, true
}

// locateSecret finds the secret inside the match that starts at start, falling
// back to its first occurrence anywhere (every occurrence is masked anyway).
// It returns -1 when the value is not in the text at all.
func locateSecret(text string, start int, match, secret string) int {
	end := start + len(match) + 2
	if end > len(text) {
		end = len(text)
	}
	if i := strings.Index(text[start:end], secret); i >= 0 {
		return start + i
	}
	return strings.Index(text, secret)
}

func isDecoded(tags []string) bool {
	for _, t := range tags {
		if strings.HasPrefix(t, "decoded:") {
			return true
		}
	}
	return false
}

// encodedRun is the length of the run of base64, hex, percent or escape
// characters at the start of s: the extent of an encoded segment.
func encodedRun(s string) int {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '+', c == '/', c == '=', c == '_', c == '-', c == '%', c == '\\':
		default:
			return i
		}
	}
	return len(s)
}

func newlines(text string) []int {
	var nl []int
	for i := 0; i < len(text); i++ {
		if text[i] == '\n' {
			nl = append(nl, i)
		}
	}
	return nl
}

// dedupe merges findings whose ranges overlap into one: two rules firing on
// one value are one finding. The merged finding keeps every member's range,
// takes the location of its earliest member, and reports the rule of its
// narrowest (preferring gitleaks' vendor-specific rule over Cairn's broader
// shape when they fire on the same value).
func dedupe(hs []hit) []hit {
	var located, out []hit
	for _, h := range hs {
		if h.start < 0 {
			out = append(out, h)
		} else {
			located = append(located, h)
		}
	}
	sort.Slice(located, func(i, j int) bool {
		if located[i].start != located[j].start {
			return located[i].start < located[j].start
		}
		return located[i].end > located[j].end
	})
	for i := 0; i < len(located); {
		g := located[i]
		g.parts = append([]part(nil), g.parts...)
		best := g
		for i++; i < len(located) && located[i].start < g.end; i++ {
			h := located[i]
			if narrower(h, best) {
				best = h
			}
			if h.end > g.end {
				g.end = h.end
			}
			g.parts = append(g.parts, h.parts...)
		}
		g.rule = best.rule
		out = append(out, g)
	}
	return out
}

// narrower orders findings on one value for reporting: the narrowest range
// wins, then a gitleaks rule over a cairn-* one, then the smaller rule ID.
func narrower(a, b hit) bool {
	if wa, wb := a.end-a.start, b.end-b.start; wa != wb {
		return wa < wb
	}
	if ca, cb := strings.HasPrefix(a.rule, "cairn-"), strings.HasPrefix(b.rule, "cairn-"); ca != cb {
		return cb
	}
	return a.rule < b.rule
}

// maskRanges lists the byte ranges to mask, merged. Each finding contributes
// its value's range and every other occurrence of the literal value.
//
// Narrow (wide false) masks only the innermost value where rules nest: when
// Cairn's curl -u rule masks the password and gitleaks' curl-auth-user masks
// "user:password", or a secret-named assignment runs on past the URL whose
// userinfo password another rule already isolated, the narrow mask keeps the
// label as Harness does (Harness applies its rules in sequence, so a masked
// value is never re-matched; gitleaks runs every rule on the original). The
// caller re-scans a narrow result and falls back to wide, the union of every
// range, when anything is still detectable.
func (s *Scanner) maskRanges(ctx context.Context, src source, hits []hit, wide bool) ([]rng, error) {
	var rs []rng
	seen := map[string]bool{}
	var secrets []string
	for _, h := range hits {
		parts := h.parts
		if wide {
			rs = append(rs, rng{h.start, h.end})
		} else {
			parts = innermost(parts)
			for _, p := range parts {
				rs = append(rs, rng{p.start, p.end})
			}
		}
		for _, p := range parts {
			if v := p.secret; v != "" && !seen[v] {
				seen[v] = true
				secrets = append(secrets, v)
			}
		}
	}
	if len(secrets) > 0 {
		err := s.windows(ctx, src, func(w window) error {
			for _, v := range secrets {
				for from := 0; ; {
					i := strings.Index(w.text[from:], v)
					if i < 0 {
						break
					}
					at := from + i
					if at >= w.lo && at < w.hi {
						rs = append(rs, rng{w.base + int64(at), w.base + int64(at+len(v))})
					}
					from = at + 1
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].start < rs[j].start })
	var merged []rng
	for _, r := range rs {
		if n := len(merged); n > 0 && r.start <= merged[n-1].end {
			if r.end > merged[n-1].end {
				merged[n-1].end = r.end
			}
			continue
		}
		merged = append(merged, r)
	}
	return merged, nil
}

// innermost drops every part that strictly contains another part.
func innermost(ps []part) []part {
	var out []part
	for i, p := range ps {
		outer := false
		for j, q := range ps {
			if i != j && p.start <= q.start && q.end <= p.end && (p.start != q.start || p.end != q.end) {
				outer = true
				break
			}
		}
		if !outer {
			out = append(out, p)
		}
	}
	return out
}

// writeMasked copies r to w with each range replaced by Mask.
func writeMasked(w io.Writer, r io.Reader, ranges []rng) error {
	var pos int64
	for _, rg := range ranges {
		if _, err := io.CopyN(w, r, rg.start-pos); err != nil {
			return err
		}
		if _, err := io.WriteString(w, Mask); err != nil {
			return err
		}
		if _, err := io.CopyN(io.Discard, r, rg.end-rg.start); err != nil {
			return err
		}
		pos = rg.end
	}
	_, err := io.Copy(w, r)
	return err
}

// capReader fails with errTooLarge once more than left bytes are read, so a
// stream longer than its declared size cannot slip past the cap.
type capReader struct {
	r    io.Reader
	left int64
}

func (c *capReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.left -= int64(n)
	if c.left < 0 {
		return n, errTooLarge
	}
	return n, err
}

// scanObject scans a stored object in windows.
func (s *Scanner) scanObject(ctx context.Context, obj objectstore.ObjectStore, key string) ([]hit, error) {
	rc, err := obj.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return s.scanStream(ctx, source{r: &capReader{r: rc, left: s.maxBytes}})
}

// Staged scans the staged object at key, of the given size, in windows. With
// no hit it returns "" and StatusClean. On a hit in mask mode it writes a
// masked copy to a new staging key and returns that key with StatusMasked; the
// caller owns both objects, promotes the masked one and removes the original.
// In reject mode it returns a *Rejection whose Field is empty: the caller
// names the field (for example with errors.As). A body over the cap is
// rejected, or under store_unscanned returned with StatusNotScannedOversize.
func (s *Scanner) Staged(ctx context.Context, obj objectstore.ObjectStore, key string, size int64, mode Mode) (maskedKey string, o Outcome, err error) {
	if err := checkMode(mode); err != nil {
		return "", Outcome{}, err
	}
	if size > s.maxBytes {
		_, o, err := s.oversized("", "", size)
		return "", o, err
	}
	hits, err := s.scanObject(ctx, obj, key)
	if errors.Is(err, errTooLarge) {
		_, o, err := s.oversized("", "", s.maxBytes+1)
		return "", o, err
	}
	if err != nil {
		return "", Outcome{}, s.failed("staged body", err)
	}
	if len(hits) == 0 {
		return "", Outcome{Status: StatusClean}, nil
	}
	o = outcomeOf(hits)
	if mode == ModeReject {
		return "", o, &Rejection{Reason: ReasonSecretDetected, Findings: o.Findings}
	}
	if unlocatable(hits) {
		return "", o, &Rejection{Reason: ReasonSecretDetected, Findings: o.Findings, Unmaskable: true}
	}

	// Narrow first, then wide, re-scanning each masked copy: anything still
	// detectable after the wide mask rejects (see maskRanges).
	var again []hit
	for _, wide := range []bool{false, true} {
		dst, dirty, err := s.stageMasked(ctx, obj, key, hits, wide)
		if err != nil {
			return "", Outcome{}, s.failed("staged body", err)
		}
		if len(dirty) == 0 {
			o.Status = StatusMasked
			return dst, o, nil
		}
		again = dirty
	}
	return "", o, &Rejection{Reason: ReasonSecretDetected, Findings: outcomeOf(again).Findings, Unmaskable: true}
}

// stageMasked writes one masked copy of key to a fresh staging key and
// re-scans it. A copy that is still dirty, or that failed, is removed.
func (s *Scanner) stageMasked(ctx context.Context, obj objectstore.ObjectStore, key string, hits []hit, wide bool) (string, []hit, error) {
	rc, err := obj.Get(ctx, key)
	if err != nil {
		return "", nil, err
	}
	ranges, err := s.maskRanges(ctx, source{r: rc}, hits, wide)
	rc.Close()
	if err != nil {
		return "", nil, err
	}
	dst, err := newStagingKey()
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = obj.Remove(context.WithoutCancel(ctx), dst) }
	if err := putMasked(ctx, obj, key, dst, ranges); err != nil {
		cleanup()
		return "", nil, err
	}
	again, err := s.scanObject(ctx, obj, dst)
	if err != nil {
		cleanup()
		return "", nil, err
	}
	if len(again) > 0 {
		cleanup()
		return "", again, nil
	}
	return dst, nil, nil
}

// putMasked streams src to dst with ranges masked, never holding it whole.
func putMasked(ctx context.Context, obj objectstore.ObjectStore, src, dst string, ranges []rng) error {
	rc, err := obj.Get(ctx, src)
	if err != nil {
		return err
	}
	defer rc.Close()
	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		pw.CloseWithError(writeMasked(pw, rc, ranges))
	}()
	err = obj.Put(ctx, dst, pr, -1, "application/octet-stream")
	pr.CloseWithError(io.ErrClosedPipe) // unblock the writer if Put stopped early
	<-done
	return err
}

// newStagingKey returns a fresh key under staging/, the shape store uses.
func newStagingKey() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("staging key: %w", err)
	}
	return "staging/" + hex.EncodeToString(b), nil
}
