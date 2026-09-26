package redact

// Scanner Behaviour Tests
//
// Pins the detector settings ADR-0023 overrides, the fail-closed paths, the
// operator allowlist (RD-8), the rejection error's shape (RD-11) and
// concurrent use. Every credential here is assembled at run time from split
// literals, so no source line holds one whole and this repository's gitleaks CI
// stage stays green without a suppression.
//
// Governing: ADR-0023, SPEC-0017 RD-2, RD-3, RD-8, RD-9, RD-11,
// REQ "Error Handling Standards", REQ "Concurrency Safety"
//
// @joestump 09/25/2026 - Added for cairn#289.

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/stump-wtf/cairn/internal/errs"
)

// pat returns a GitHub personal access token shape: ghp_ plus exactly 36
// distinct characters, walked through the alphanumerics with a stride coprime
// to their count, so each seed gives a different value with enough entropy
// for gitleaks' github-pat rule (entropy > 3).
func pat(seed int) string {
	const alnum = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	var b strings.Builder
	for i := 0; i < 36; i++ {
		b.WriteByte(alnum[(seed*11+i*7)%len(alnum)])
	}
	return "gh" + "p_" + b.String()
}

func writeAllowlist(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "allowlist.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestDetectorSettingsPinned: the CLI defaults that are wrong for an ingest
// scanner stay overridden, whatever gitleaks' defaults become (RD-2).
func TestDetectorSettingsPinned(t *testing.T) {
	s := newTestScanner(t)
	if !s.det.IgnoreGitleaksAllow {
		t.Error("IgnoreGitleaksAllow = false: an uploader could exempt itself with a comment")
	}
	if s.det.MaxTargetMegaBytes != 0 {
		t.Errorf("MaxTargetMegaBytes = %d, want 0 (gitleaks must never skip silently)", s.det.MaxTargetMegaBytes)
	}
	if s.det.MaxArchiveDepth != 0 {
		t.Errorf("MaxArchiveDepth = %d, want 0", s.det.MaxArchiveDepth)
	}
	if s.det.MaxDecodeDepth <= 0 {
		t.Errorf("MaxDecodeDepth = %d, want explicit and non-zero", s.det.MaxDecodeDepth)
	}
	if s.MaxScanBytes() != DefaultMaxScanBytes || s.Oversize() != OversizeReject {
		t.Errorf("defaults = %d, %q", s.MaxScanBytes(), s.Oversize())
	}
}

// TestEmbeddedConfigExtendsDefault: the embedded file declares the extend for
// its readers, and the merged ruleset really holds both gitleaks' default
// rules and Cairn's.
func TestEmbeddedConfigExtendsDefault(t *testing.T) {
	if !strings.Contains(string(baseConfig), "useDefault = true") {
		t.Error("cairn-gitleaks.toml no longer declares [extend] useDefault = true")
	}
	s := newTestScanner(t)
	for _, id := range []string{"github-pat", "aws-access-token", "private-key", "generic-api-key",
		"cairn-url-userinfo", "cairn-authorization", "cairn-api-key-header", "cairn-vendor-token",
		"cairn-secret-assignment", "cairn-secret-flag", "cairn-basic-auth-cli", "cairn-private-key"} {
		if _, ok := s.det.Config.Rules[id]; !ok {
			t.Errorf("rule %q missing from the merged config", id)
		}
	}
	if n := len(s.det.Config.Rules); n < 200 {
		t.Errorf("merged config has %d rules, want the ~200 default rules plus Cairn's", n)
	}
}

// TestRepeatedBuildsKeepDefaultRules: gitleaks counts extend depth in a
// package global that is never reset, so a build that let gitleaks extend
// would drop the default rules from the third build on. Build several times
// and check a default-only rule still fires.
func TestRepeatedBuildsKeepDefaultRules(t *testing.T) {
	token := pat(1)
	for i := 0; i < 5; i++ {
		s := newTestScanner(t)
		_, o, err := s.Text(context.Background(), "body", "token "+token, ModeMask)
		if err != nil {
			t.Fatalf("build %d: %v", i, err)
		}
		if o.Rules["github-pat"] != 1 {
			t.Fatalf("build %d: rules = %v, want github-pat from the default ruleset", i, o.Rules)
		}
	}
}

// TestInlineAllowDoesNotExempt: SPEC-0017 RD-2 "Inline allow comment does not
// exempt".
func TestInlineAllowDoesNotExempt(t *testing.T) {
	s := newTestScanner(t)
	in := "const key = \"" + pat(2) + "\" // gitleaks" + ":allow\n"
	got, o, err := s.Text(context.Background(), "body", in, ModeMask)
	if err != nil {
		t.Fatal(err)
	}
	if o.Status != StatusMasked || strings.Contains(got, pat(2)) {
		t.Errorf("gitleaks:allow exempted the token: %q, %+v", got, o)
	}
	if _, _, err := s.Text(context.Background(), "body", in, ModeReject); !errors.Is(err, ErrSecretDetected) {
		t.Errorf("reject mode: err = %v, want ErrSecretDetected", err)
	}
}

// TestEncodedTokenDetected: SPEC-0017 RD-2 "Encoded token is detected", and in
// mask mode the whole encoded segment is masked.
func TestEncodedTokenDetected(t *testing.T) {
	s := newTestScanner(t)
	enc := base64.StdEncoding.EncodeToString([]byte("export GITHUB_TOKEN=" + pat(3) + "\n"))
	in := "config blob: " + enc + " (end)"
	got, o, err := s.Text(context.Background(), "body", in, ModeMask)
	if err != nil {
		t.Fatal(err)
	}
	if o.Status != StatusMasked || o.Count < 1 {
		t.Fatalf("outcome = %+v, want the encoded token detected", o)
	}
	if strings.Contains(got, enc) || strings.Contains(got, enc[20:40]) {
		t.Errorf("encoded segment survived: %q", got)
	}
	if !strings.HasPrefix(got, "config blob: ") || !strings.HasSuffix(got, " (end)") || !strings.Contains(got, Mask) {
		t.Errorf("masked = %q, want only the encoded segment replaced", got)
	}
	if _, _, err := s.Text(context.Background(), "body", in, ModeReject); !errors.Is(err, ErrSecretDetected) {
		t.Errorf("reject mode: err = %v, want ErrSecretDetected", err)
	}
}

// TestLabelKeptValueMasked: SPEC-0017 RD-3 "Label kept, value masked", for a
// trace span's args.
func TestLabelKeptValueMasked(t *testing.T) {
	s := newTestScanner(t)
	jwt := "eyJ" + "hbGciOiJIUzI1NiJ9" + "." + "eyJzdWIiOiJhZ2VudC0xMjMifQ" + "." + "c2lnbmF0dXJlLXZhbHVlLTEyMw"
	in := `{"headers":{"Authorization":"Bearer ` + jwt + `"},"url":"https://api.example.com"}`
	got, o, err := s.Text(context.Background(), "spans[0].args", in, ModeMask)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"headers":{"Authorization":"Bearer ` + Mask + `"},"url":"https://api.example.com"}`
	if got != want || o.Count != 1 {
		t.Errorf("masked = %q (%+v)\nwant %q with one finding", got, o, want)
	}
}

// TestRepeatedValueMaskedEverywhere: a value is masked at every occurrence,
// and each occurrence a rule fired on is counted.
func TestRepeatedValueMaskedEverywhere(t *testing.T) {
	s := newTestScanner(t)
	tok := pat(4)
	in := "first " + tok + "\nsecond " + tok + "\n"
	got, o, err := s.Text(context.Background(), "body", in, ModeMask)
	if err != nil {
		t.Fatal(err)
	}
	if got != "first "+Mask+"\nsecond "+Mask+"\n" {
		t.Errorf("masked = %q", got)
	}
	if o.Count != 2 || o.Rules["github-pat"] != 2 {
		t.Errorf("outcome = %+v, want github-pat x2", o)
	}
}

// TestFindingLocation: lines and columns are 1-based and point at the value,
// not at gitleaks' 0-based, off-by-one location.
func TestFindingLocation(t *testing.T) {
	s := newTestScanner(t)
	in := "line one\nline two\n  x " + pat(5) + "\n"
	_, o, err := s.Text(context.Background(), "body", in, ModeReject)
	var rej *Rejection
	if !errors.As(err, &rej) {
		t.Fatalf("err = %v, want *Rejection", err)
	}
	want := []Finding{{Rule: "github-pat", Line: 3, Column: 5}}
	if !reflect.DeepEqual(rej.Findings, want) || !reflect.DeepEqual(o.Findings, want) {
		t.Errorf("findings = %+v / %+v, want %+v", rej.Findings, o.Findings, want)
	}
	if msg := err.Error(); msg != "body: a credential was detected (rule github-pat, line 3, column 5); remove it or resend with --redact=mask" {
		t.Errorf("message = %q", msg)
	}
}

// TestRejectionShape: a rejection is validation_failed, wraps the right
// sentinel, and never carries the value (RD-11).
func TestRejectionShape(t *testing.T) {
	s := newTestScanner(t)
	tok := pat(6)
	_, _, err := s.Text(context.Background(), "members[3].content", "key: "+tok, ModeReject)
	if !errors.Is(err, ErrSecretDetected) || !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("err = %v, want ErrSecretDetected and errs.ErrValidation", err)
	}
	if errors.Is(err, ErrTooLargeToScan) || errors.Is(err, ErrScanFailed) {
		t.Errorf("err = %v wraps the wrong sentinel", err)
	}
	if got := errs.CodeOf(err); got != errs.CodeValidation {
		t.Errorf("CodeOf = %q, want %q", got, errs.CodeValidation)
	}
	msg := err.Error()
	if strings.Contains(msg, tok) || strings.Contains(msg, tok[4:20]) {
		t.Errorf("message leaks the value: %q", msg)
	}
	if !strings.HasPrefix(msg, "members[3].content: ") || !strings.Contains(msg, "rule github-pat") {
		t.Errorf("message = %q, want the field and the rule", msg)
	}
}

// TestRejectionMessages covers the unmaskable and truncated forms.
func TestRejectionMessages(t *testing.T) {
	var fs []Finding
	for i := 1; i <= 7; i++ {
		fs = append(fs, Finding{Rule: "r", Line: i, Column: 1})
	}
	msg := (&Rejection{Field: "body", Reason: ReasonSecretDetected, Findings: fs}).Error()
	if !strings.Contains(msg, "and 2 more") {
		t.Errorf("message = %q, want the list truncated", msg)
	}
	msg = (&Rejection{Reason: ReasonSecretDetected, Findings: fs[:1], Unmaskable: true}).Error()
	if msg != "content: a credential was detected that could not be masked (rule r, line 1, column 1); remove it" {
		t.Errorf("unmaskable message = %q", msg)
	}
}

// TestFindingCarriesNoValue: RD-9 by construction. Adding a field to Finding
// that could hold a secret must be a deliberate, reviewed change.
func TestFindingCarriesNoValue(t *testing.T) {
	var names []string
	ft := reflect.TypeOf(Finding{})
	for i := 0; i < ft.NumField(); i++ {
		names = append(names, ft.Field(i).Name)
	}
	sort.Strings(names)
	if !reflect.DeepEqual(names, []string{"Column", "Line", "Rule"}) {
		t.Errorf("Finding fields = %v, want exactly Column, Line, Rule", names)
	}
}

// TestOversizeText: RD-7 on the in-memory path.
func TestOversizeText(t *testing.T) {
	in := strings.Repeat("a", 2048)
	s, err := New(Config{MaxScanBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = s.Text(context.Background(), "body", in, ModeMask)
	var rej *Rejection
	if !errors.As(err, &rej) || rej.Reason != ReasonTooLargeToScan || rej.Limit != 1024 {
		t.Fatalf("err = %v, want too_large_to_scan with limit 1024", err)
	}
	if !errors.Is(err, ErrTooLargeToScan) || errs.CodeOf(err) != errs.CodeValidation {
		t.Errorf("err = %v, want ErrTooLargeToScan as validation_failed", err)
	}

	s, err = New(Config{MaxScanBytes: 1024, Oversize: OversizeStoreUnscanned})
	if err != nil {
		t.Fatal(err)
	}
	got, o, err := s.Text(context.Background(), "body", in, ModeMask)
	if err != nil || got != in || o.Status != StatusNotScannedOversize {
		t.Errorf("store_unscanned: %q, %+v, %v", got[:8], o, err)
	}
}

// TestConfigValidation: bad scanner settings are startup errors.
func TestConfigValidation(t *testing.T) {
	if _, err := New(Config{MaxScanBytes: -1}); err == nil {
		t.Error("negative MaxScanBytes accepted")
	}
	if _, err := New(Config{Oversize: "skip"}); err == nil {
		t.Error("unknown oversize policy accepted")
	}
}

// TestBadAllowlistStopsStartup: SPEC-0017 RD-2 "Bad config stops startup" and
// RD-8 "Path allowlist refused". Every error names the file.
func TestBadAllowlistStopsStartup(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"does not parse", "regexes = [", "does not parse"},
		{"paths", "paths = ['''^docs/.*$''']", `"paths" is unsupported`},
		{"commits", "commits = ['''abc123''']", `"commits" is unsupported`},
		{"nested paths", "[allowlist]\npaths = ['''x''']", `"allowlist.paths" is unsupported`},
		{"unknown key", "regexTarget = 'line'", `unknown entry "regexTarget"`},
		{"bad regex", "regexes = ['''^(unclosed$''']", "regexes[0] does not compile"},
		{"unanchored regex", "regexes = ['''fixture''']", "regexes[0] must be anchored"},
		{"matches everything", "regexes = ['''^.*$''']", "matches the empty string"},
		{"alternation escapes the anchors", "regexes = ['''^fixture|.+$''']", "regexes[0] must be anchored"},
		{"one branch unanchored", "regexes = ['''^fixture$|leak$''']", "regexes[0] must be anchored"},
		{"empty stopword", "stopwords = ['']", "stopwords[0] is empty"},
		{"unknown rule", "disabledRules = ['no-such-rule']", `"no-such-rule" is not a known rule`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := writeAllowlist(t, tc.body)
			_, err := New(Config{AllowlistFile: p})
			if err == nil {
				t.Fatal("New accepted the allowlist")
			}
			if !strings.Contains(err.Error(), p) || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want it to name %s and %q", err, p, tc.want)
			}
		})
	}
	if _, err := New(Config{AllowlistFile: filepath.Join(t.TempDir(), "missing.toml")}); err == nil {
		t.Error("a missing allowlist file was accepted")
	}
}

// TestAllowlistExemptsOnlyTheValue: SPEC-0017 RD-8 "Fixture value
// allowlisted": an anchored regex exempts one inert value, and a different
// token in the same body is still caught.
func TestAllowlistExemptsOnlyTheValue(t *testing.T) {
	inert, real := pat(7), pat(8)
	p := writeAllowlist(t, "# inert test fixture\nregexes = ['''^"+regexp.QuoteMeta(inert)+"$''']\n")
	s, err := New(Config{AllowlistFile: p})
	if err != nil {
		t.Fatal(err)
	}
	in := "fixture " + inert + "\nleaked " + real + "\n"
	got, o, err := s.Text(context.Background(), "body", in, ModeMask)
	if err != nil {
		t.Fatal(err)
	}
	if want := "fixture " + inert + "\nleaked " + Mask + "\n"; got != want {
		t.Errorf("masked = %q\nwant %q", got, want)
	}
	if o.Count != 1 {
		t.Errorf("count = %d, want 1", o.Count)
	}
}

// TestAllowlistStopwordsAndDisabledRules: the other two supported keys.
func TestAllowlistStopwordsAndDisabledRules(t *testing.T) {
	p := writeAllowlist(t, "stopwords = ['hunter2']\ndisabledRules = ['cairn-secret-flag']\n")
	s, err := New(Config{AllowlistFile: p})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.det.Config.Rules["cairn-secret-flag"]; ok {
		t.Error("disabled rule still configured")
	}
	in := "mysql --password " + "hunter2" + "-hunter2 -h db"
	if got, o, err := s.Text(context.Background(), "body", in, ModeMask); err != nil || got != in || o.Status != StatusClean {
		t.Errorf("stopword did not exempt: %q, %+v, %v", got, o, err)
	}
}

// TestModeValidated: an unknown mode is an internal error, never a skip.
func TestModeValidated(t *testing.T) {
	s := newTestScanner(t)
	_, _, err := s.Text(context.Background(), "body", "x", Mode("off"))
	if !errors.Is(err, ErrScanFailed) || errs.CodeOf(err) != errs.CodeInternal {
		t.Errorf("err = %v, want an internal ErrScanFailed", err)
	}
}

// TestCancelledContextFailsClosed: DetectContext returns partial findings and
// no error when its context ends, so the scanner must check for itself.
func TestCancelledContextFailsClosed(t *testing.T) {
	s := newTestScanner(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, _, err := s.Text(ctx, "body", "token "+pat(9), ModeMask)
	if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrScanFailed) {
		t.Fatalf("err = %v, want a cancelled internal failure", err)
	}
	if errs.CodeOf(err) != errs.CodeInternal || got != "" {
		t.Errorf("CodeOf = %q, text = %q; want internal and nothing returned", errs.CodeOf(err), got)
	}
}

// TestDetectorPanicFailsClosed: a panic inside the detector is an internal
// error, and nothing is returned as scanned.
func TestDetectorPanicFailsClosed(t *testing.T) {
	s := newTestScanner(t)
	s.det = nil // DetectContext on a nil detector panics
	got, _, err := s.Text(context.Background(), "body", "hello", ModeMask)
	if !errors.Is(err, ErrScanFailed) || errs.CodeOf(err) != errs.CodeInternal || got != "" {
		t.Errorf("got %q, %v; want an internal ErrScanFailed", got, err)
	}
}

// TestUnlocatableRejects: a finding the masker cannot place rejects the field
// even in mask mode.
func TestUnlocatableRejects(t *testing.T) {
	if !unlocatable([]hit{{rule: "r", start: 3, end: 5}, {rule: "r", start: -1, end: -1}}) {
		t.Error("an unlocated hit was not reported")
	}
	if unlocatable([]hit{{rule: "r", start: 3, end: 5}}) {
		t.Error("a located hit was reported unlocatable")
	}
}

// TestConcurrentText: SPEC-0017 REQ "Concurrency Safety". One shared detector,
// 32 concurrent scans, each masked or rejected correctly; run under -race.
func TestConcurrentText(t *testing.T) {
	s := newTestScanner(t)
	var wg sync.WaitGroup
	errc := make(chan string, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tok := pat(100 + i)
			in := "call " + string(rune('0'+i%10)) + ": Authorization: token " + tok + "\n"
			mode := ModeMask
			if i%2 == 1 {
				mode = ModeReject
			}
			got, o, err := s.Text(context.Background(), "body", in, mode)
			switch {
			case mode == ModeMask && (err != nil || strings.Contains(got, tok) || o.Count != 1):
				errc <- "mask: " + got
			case mode == ModeReject && !errors.Is(err, ErrSecretDetected):
				errc <- "reject: no rejection"
			}
		}(i)
	}
	wg.Wait()
	close(errc)
	for e := range errc {
		t.Error(e)
	}
}

// TestAnchored: an allowlist regex must pin every match to the whole value on
// every branch, so an alternation cannot smuggle in an unanchored pattern.
//
// @joestump 09/26/2026 - Added in review of cairn#382.
func TestAnchored(t *testing.T) {
	for re, want := range map[string]bool{
		`^a$`: true, `(?i)^a$`: true, `^a$|^b$`: true, `^(?:a|b)$`: true,
		`^(a)$`: true, `^[a-z]{40}$`: true,
		`^a|.+$`: false, `^a$|b$`: false, `a`: false, `^a`: false, `a$`: false,
		`^a\$`: false, `(?m)^a$`: false, `(^a)|b$`: false,
	} {
		if got := anchored(re); got != want {
			t.Errorf("anchored(%q) = %v, want %v", re, got, want)
		}
	}
}
