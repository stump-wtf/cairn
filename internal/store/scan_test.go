package store

// Ingest Secret Scan Unit Tests
//
// The parts of the create-path scan that need no database: the RD-6 text or
// binary decision, mode resolution, the in-memory keep, and the conversion of
// a scanner rejection into its SPEC-0019 violation.
//
// Governing: ADR-0023, SPEC-0017 RD-5, RD-6, RD-11
//
// @joestump 09/26/2026 - Added for cairn#292.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/objectstore"
	"github.com/stump-wtf/cairn/internal/redact"
)

// TestIsTextBody: SPEC-0017 RD-6. A textual media type or a UTF-8 head is
// text whatever was declared; a known binary signature is not; anything else
// is scanned.
func TestIsTextBody(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR\x00\x00")
	gzip := []byte("\x1f\x8b\x08\x00\x00\x00\x00\x00\x00\xff")
	elf := []byte("\x7fELF\x02\x01\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00\x02\x00")
	tar := append(bytes.Repeat([]byte{0}, 257), []byte("ustar\x0000")...)
	for _, tc := range []struct {
		name  string
		media string
		head  []byte
		want  bool
	}{
		{"text declared image", "image/png", []byte("hello, world\n"), true},
		{"real png", "image/png", png, false},
		{"real png declared nothing", "application/octet-stream", png, false},
		{"png declared text", "text/plain; charset=utf-8", png, true},
		{"json", "application/json", []byte{0xff, 0x00}, true},
		{"vendor json", "application/vnd.api+json", []byte{0xff, 0x00}, true},
		{"yaml", "application/yaml", []byte{0xff}, true},
		{"gzip", "application/gzip", gzip, false},
		{"elf", "application/octet-stream", elf, false},
		{"tar", "application/x-tar", tar, false},
		{"nul in text", "application/octet-stream", []byte("abc\x00def\xff"), true}, // no signature: scanned
		{"unknown bytes", "application/octet-stream", []byte{0xff, 0xfe, 0xfd, 0x01}, true},
		{"empty", "", nil, true},
		{"ascii signature in text", "application/octet-stream", []byte("MZ is a fine name for a text file"), true},
	} {
		if got := isTextBody(tc.media, tc.head, int64(len(tc.head))); got != tc.want {
			t.Errorf("%s: isTextBody = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestUTF8HeadCutMidCharacter: a head cut off inside a multi-byte character
// is still UTF-8 text, but the same bytes as a whole body are not.
func TestUTF8HeadCutMidCharacter(t *testing.T) {
	head := []byte(strings.Repeat("a", 10) + "\xe2\x82") // the first two bytes of "€"
	if !utf8Head(head, int64(len(head))+1) {
		t.Error("a head cut inside a character is not treated as text")
	}
	if utf8Head(head, int64(len(head))) {
		t.Error("a whole body ending in a partial character is treated as text")
	}
	if utf8Head([]byte("text\x00more"), 100) {
		t.Error("a head with a NUL is treated as text")
	}
}

// TestRedactionMode: reject only for a listed type the writer did not
// downgrade; mask otherwise. Nil lists mean the default, code and bundle.
func TestRedactionMode(t *testing.T) {
	s := &Store{rejectTypes: rejectTypesOf(nil)}
	for _, tc := range []struct {
		typ       artifact.ShareType
		downgrade bool
		want      redact.Mode
	}{
		{"code", false, redact.ModeReject},
		{"code", true, redact.ModeMask},
		{artifact.TypeBundle, false, redact.ModeReject},
		{artifact.TypeBundle, true, redact.ModeMask},
		{"markdown", false, redact.ModeMask},
		{artifact.TypeFile, false, redact.ModeMask},
		{"a-type-added-later", false, redact.ModeMask},
	} {
		if got := s.redactionMode(tc.typ, tc.downgrade); got != tc.want {
			t.Errorf("mode(%s, downgrade=%v) = %s, want %s", tc.typ, tc.downgrade, got, tc.want)
		}
	}
	if none := (&Store{rejectTypes: rejectTypesOf([]string{})}); none.redactionMode("code", false) != redact.ModeMask {
		t.Error("an empty reject list still rejects code")
	}
}

// TestStreamBlobKeep: a body at most the keep limit is kept whole, a larger
// one keeps nothing, and the head is the first 8 KiB either way.
func TestStreamBlobKeep(t *testing.T) {
	obj := objectstore.NewMemory()
	for _, tc := range []struct {
		size, keep int
		kept       bool
	}{{0, 16, true}, {16, 16, true}, {17, 16, false}, {20000, 1 << 20, true}, {20000, 0, false}} {
		body := bytes.Repeat([]byte("x"), tc.size)
		sb, err := streamBlobKeep(context.Background(), obj, bytes.NewReader(body), 1<<20, "", int64(tc.keep))
		if err != nil {
			t.Fatal(err)
		}
		if (sb.body != nil) != tc.kept || (tc.kept && !bytes.Equal(sb.body, body)) {
			t.Errorf("size %d keep %d: kept %d bytes, want kept=%v", tc.size, tc.keep, len(sb.body), tc.kept)
		}
		if want := min(tc.size, sniffBytes); len(sb.head) != want {
			t.Errorf("size %d: head is %d bytes, want %d", tc.size, len(sb.head), want)
		}
	}
}

// TestRejectionViolation: SPEC-0017 RD-11. A scanner rejection becomes one
// secret_detected violation per finding, with rule, line and column and no
// value, or one too_large_to_scan with the limit in bytes. The rejection stays
// in the chain.
func TestRejectionViolation(t *testing.T) {
	sc := testScanner(t)
	tok := plantedToken(21)
	_, _, err := sc.Text(context.Background(), "ignored", "a\nkey="+tok+"\n", redact.ModeReject)
	serr := scanErr("members[3].content", err)

	vs := errs.ViolationsOf(serr)
	if len(vs) != 1 {
		t.Fatalf("violations = %+v", vs)
	}
	v := vs[0]
	if v.Field != "members[3].content" || v.Reason != errs.ReasonSecretDetected || v.Value != nil ||
		v.Extra["rule"] != "github-pat" || v.Extra["line"] != 2 || v.Extra["column"] != 5 {
		t.Errorf("violation = %+v", v)
	}
	raw, err2 := json.Marshal(v)
	if err2 != nil {
		t.Fatal(err2)
	}
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if m["rule"] != "github-pat" || m["line"] != float64(2) || m["column"] != float64(5) {
		t.Errorf("rendered violation = %s", raw)
	}
	for _, out := range []string{string(raw), serr.Error()} {
		if strings.Contains(out, tok) || strings.Contains(out, tok[4:24]) {
			t.Error("the violation carries part of the token")
		}
	}
	var rej *redact.Rejection
	if !errors.As(serr, &rej) || rej.Field != "members[3].content" || !errors.Is(serr, redact.ErrSecretDetected) || !errors.Is(serr, errs.ErrValidation) {
		t.Errorf("the rejection is not in the chain: %v", serr)
	}

	big := RejectionViolation(&redact.Rejection{Field: "body", Reason: redact.ReasonTooLargeToScan, Limit: 16 << 20, Size: 20 << 20})
	if bv := big.Violations[0]; bv.Reason != errs.ReasonTooLargeToScan || bv.Limit != int64(16<<20) || bv.Unit != errs.UnitBytes {
		t.Errorf("too large violation = %+v", bv)
	}
	if !errors.Is(big, redact.ErrTooLargeToScan) {
		t.Error("too_large_to_scan does not keep its sentinel")
	}

	internal := scanErr("body", redact.ErrScanFailed)
	if errs.ViolationsOf(internal) != nil || errs.CodeOf(internal) != errs.CodeInternal {
		t.Errorf("a failed scan became %v (%s), want internal", internal, errs.CodeOf(internal))
	}
}
