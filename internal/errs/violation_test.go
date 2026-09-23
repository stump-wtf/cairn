package errs

import (
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

// Governing: ADR-0025, SPEC-0019 VE-1..VE-6

func TestViolateUnwrapsToSentinels(t *testing.T) {
	err := fmt.Errorf("create: %w", Violate("X-Cairn-Ttl-Seconds", LocHeader, ReasonExceedsMax, WithLimit(2592000, UnitSeconds)))
	if !errors.Is(err, ErrValidation) {
		t.Fatal("a violation must stay errors.Is(ErrValidation) through a wrap")
	}
	if CodeOf(err) != CodeValidation {
		t.Fatalf("CodeOf = %q, want validation_failed", CodeOf(err))
	}
	if vs := ViolationsOf(err); len(vs) != 1 || vs[0].Field != "X-Cairn-Ttl-Seconds" {
		t.Fatalf("ViolationsOf through a wrap = %+v", vs)
	}

	big := Violate("body", LocBody, ReasonTooLarge, WithLimit(1024, UnitBytes))
	if CodeOf(big) != CodePayloadTooLarge || !errors.Is(big, ErrTooLarge) {
		t.Fatalf("an all-too_large violation must be payload_too_large, got %q", CodeOf(big))
	}
	mixed := Join(big, Violate("members[1].name", LocForm, ReasonDuplicate))
	if CodeOf(mixed) != CodeValidation {
		t.Fatalf("a mixed list must be validation_failed, got %q", CodeOf(mixed))
	}
}

func TestBecauseKeepsTheCause(t *testing.T) {
	sentinel := errors.New("emoji invalid")
	err := fmt.Errorf("react: %w", Violate("emoji", LocBody, ReasonInvalidFormat).Because(sentinel))
	if !errors.Is(err, sentinel) || !errors.Is(err, ErrValidation) {
		t.Fatal("Because must keep both the cause and ErrValidation reachable")
	}
	if CodeOf(err) != CodeValidation {
		t.Fatalf("the sentinel, not the cause, decides the code: got %q", CodeOf(err))
	}
}

func TestErrorStringKeepsInternalDetail(t *testing.T) {
	err := Violate("X-Cairn-Ttl-Seconds", LocHeader, ReasonExceedsMax, WithLimit(3600, UnitSeconds), WithValue("7200"))
	got := err.Error()
	for _, want := range []string{"X-Cairn-Ttl-Seconds", "7200", "3600 seconds", "validation failed"} {
		if !strings.Contains(got, want) {
			t.Fatalf("Error() = %q, missing %q", got, want)
		}
	}
}

// VE-3: the echo is capped on a rune boundary and never carries a secret.
func TestWithValueCapsOnRuneBoundary(t *testing.T) {
	long := strings.Repeat("é", 40) // 80 bytes of two-byte runes
	v := NewViolation("tag", LocHeader, ReasonTooLong, WithValue(long))
	if v.Value == nil {
		t.Fatal("value dropped")
	}
	if len(*v.Value) > MaxValueBytes || !utf8.ValidString(*v.Value) {
		t.Fatalf("value = %q (%d bytes), want valid UTF-8 of at most %d bytes", *v.Value, len(*v.Value), MaxValueBytes)
	}
	if len(*v.Value) != 64 {
		t.Fatalf("value is %d bytes, want exactly 64 (32 whole runes)", len(*v.Value))
	}
	odd := strings.Repeat("a", 63) + "é" // the rune straddles byte 64
	if got := *NewViolation("tag", LocHeader, ReasonTooLong, WithValue(odd)).Value; got != strings.Repeat("a", 63) {
		t.Fatalf("value = %q, want the rune dropped rather than split", got)
	}
}

func TestSecretDetectedNeverEchoes(t *testing.T) {
	v := NewViolation("body", LocBody, ReasonSecretDetected,
		WithValue("would-be-secret"), WithExtra("rule", "github-pat"), WithExtra("line", 42), WithExtra("column", 17),
		WithExtra("value", "smuggled"))
	if v.Value != nil {
		t.Fatal("secret_detected must never carry a value")
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["value"]; ok {
		t.Fatalf("encoded violation carries a value: %s", b)
	}
	if m["rule"] != "github-pat" || m["line"] != float64(42) || m["column"] != float64(17) {
		t.Fatalf("extra keys not flattened: %s", b)
	}
	if strings.Contains(string(b), "would-be-secret") || strings.Contains(string(b), "smuggled") {
		t.Fatalf("encoded violation leaks the offered value: %s", b)
	}
	if !strings.Contains(v.Message, "rule github-pat, line 42") || !strings.Contains(v.Message, "--redact=mask") {
		t.Fatalf("message = %q, want the rule, line and the downgrade hint", v.Message)
	}
}

func TestExtraCannotOverrideFixedKeys(t *testing.T) {
	v := NewViolation("tag", LocHeader, ReasonUppercase, WithValue("Size:M"), WithExtra("field", "other"), WithExtra("reason", "x"))
	b, _ := json.Marshal(v)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if m["field"] != "tag" || m["reason"] != "uppercase" {
		t.Fatalf("Extra overrode a fixed key: %s", b)
	}
}

func TestViolationJSONShape(t *testing.T) {
	v := NewViolation("X-Cairn-Ttl-Seconds", LocHeader, ReasonExceedsMax, WithLimit(2592000, UnitSeconds), WithValue("5184000"))
	b, _ := json.Marshal(v)
	want := `{"field":"X-Cairn-Ttl-Seconds","location":"header","reason":"exceeds_max","limit":2592000,"unit":"seconds","value":"5184000","message":"\"5184000\" exceeds the maximum of 2592000 seconds"}`
	if string(b) != want {
		t.Fatalf("encoded =\n%s\nwant\n%s", b, want)
	}
}

func TestSummary(t *testing.T) {
	one := []Violation{NewViolation("tag", LocHeader, ReasonUppercase, WithValue("Size:M"))}
	if got, want := Summary(one), `tag: "Size:M" must be lowercase`; got != want {
		t.Fatalf("Summary(1) = %q, want %q", got, want)
	}
	two := append(one, NewViolation("tag", LocHeader, ReasonInvalidCharset, WithValue("lane m")))
	if got := Summary(two); !strings.HasPrefix(got, "2 problems: tag: ") || !strings.Contains(got, `; tag: "lane m"`) {
		t.Fatalf("Summary(2) = %q", got)
	}
	if got := Summary([]Violation{Generic()}); got != MessageInvalid {
		t.Fatalf("Summary(generic) = %q, want the bare generic message", got)
	}
}

func TestJoinIsBoundedAndSkipsNil(t *testing.T) {
	if Join(nil, nil) != nil {
		t.Fatal("joining nothing must be nil")
	}
	var parts []*Invalid
	for i := range MaxViolations + 10 {
		parts = append(parts, Violate(fmt.Sprintf("tags[%d]", i), LocBody, ReasonUppercase))
	}
	if got := len(Join(parts...).Violations); got != MaxViolations {
		t.Fatalf("joined %d violations, want the cap %d", got, MaxViolations)
	}
}

// Every reason has a sentence of its own: none falls through to the generic
// message except ReasonInvalid itself.
func TestEveryReasonHasAMessage(t *testing.T) {
	for _, r := range Reasons {
		v := NewViolation("f", LocBody, r, WithLimit(1, UnitBytes))
		if v.Message == "" {
			t.Fatalf("reason %q has an empty message", r)
		}
		if r != ReasonInvalid && v.Message == MessageInvalid {
			t.Fatalf("reason %q falls through to the generic message", r)
		}
	}
}

// VE-2: every Reason constant declared in this package is in the registry
// slice, and the registry is exactly the list SPEC-0019 VE-2 publishes.
func TestReasonRegistryIsClosed(t *testing.T) {
	declared := declaredReasons(t)
	inSlice := map[Reason]bool{}
	for _, r := range Reasons {
		if inSlice[r] {
			t.Fatalf("reason %q is listed twice", r)
		}
		inSlice[r] = true
	}
	for _, r := range declared {
		if !inSlice[r] {
			t.Errorf("reason constant %q is declared but not in Reasons", r)
		}
	}
	if len(declared) != len(Reasons) {
		t.Errorf("%d reason constants declared, %d registered", len(declared), len(Reasons))
	}

	spec := specRegistry(t)
	if strings.Join(spec, ",") != strings.Join(reasonStrings(Reasons), ",") {
		t.Fatalf("registry drifted from SPEC-0019 VE-2:\n code: %v\n spec: %v", reasonStrings(Reasons), spec)
	}
}

func declaredReasons(t *testing.T) []Reason {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "violation.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []Reason
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, s := range gd.Specs {
			vs := s.(*ast.ValueSpec)
			if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "Reason" {
				continue
			}
			for _, val := range vs.Values {
				if lit, ok := val.(*ast.BasicLit); ok {
					out = append(out, Reason(strings.Trim(lit.Value, `"`)))
				}
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("found no Reason constants: the parser probe is broken")
	}
	return out
}

// specRegistry reads the reason list out of SPEC-0019's VE-2 requirement, so
// the code and the published spec cannot drift apart silently.
func specRegistry(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile("../../docs/openspec/specs/validation-errors/spec.md")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	start := strings.Index(s, "### Requirement: VE-2")
	if start < 0 {
		t.Fatal("VE-2 heading not found in SPEC-0019")
	}
	s = s[start:]
	from := strings.Index(s, "MUST be one of:")
	to := strings.Index(s[from:], ".\n")
	if from < 0 || to < 0 {
		t.Fatal("VE-2 registry sentence not found")
	}
	var out []string
	for _, m := range regexp.MustCompile("`([a-z_]+)`").FindAllStringSubmatch(s[from:from+to], -1) {
		out = append(out, m[1])
	}
	if len(out) < 10 {
		t.Fatalf("parsed only %d reasons from the spec: %v", len(out), out)
	}
	return out
}

func reasonStrings(rs []Reason) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = string(r)
	}
	return out
}
