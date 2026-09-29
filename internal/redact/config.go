package redact

// Detector Construction
//
// Builds the gitleaks config.Config the shared detector runs: gitleaks' default
// ruleset, Cairn's Harness-shape rules from the embedded cairn-gitleaks.toml,
// and the operator's value-only allowlist. Each source is translated by its own
// config.ViperConfig.Translate() call on a private viper instance and the
// results are merged here, because gitleaks' own `[extend] useDefault` path is
// not safe to run more than once per process: it counts extend depth in a
// package global that is never reset (so the third build silently drops the
// default rules, and the second skips rule validation) and it reads the default
// config through the global viper instance.
//
// Translate compiles regexes with MustCompile, so every operator regex is
// compiled with regexp.Compile first and the whole build runs under recover: a
// bad pattern is a startup error, never a panic.
//
// Governing: ADR-0023, SPEC-0017 RD-2, RD-8
//
// @joestump 09/23/2026 - Added for cairn#289.

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"regexp"
	"regexp/syntax"
	"slices"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/spf13/viper"
	"github.com/zricethezav/gitleaks/v8/config"
)

//go:embed cairn-gitleaks.toml
var baseConfig []byte

// OversizePolicy is what happens to a text field larger than MaxScanBytes.
type OversizePolicy string

const (
	// OversizeReject refuses the field with reason too_large_to_scan. Default.
	OversizeReject OversizePolicy = "reject"
	// OversizeStoreUnscanned stores the field unscanned, with status
	// not_scanned_oversize. A risky operator opt-in (SPEC-0017 RD-7).
	OversizeStoreUnscanned OversizePolicy = "store_unscanned"
)

// DefaultMaxScanBytes is the per-field scan cap when Config leaves it zero.
const DefaultMaxScanBytes int64 = 16 << 20

// DefaultMaxDecodeDepth is how many decoding passes (base64, hex, percent,
// unicode escapes) the detector makes, so an encoded token is still caught.
const DefaultMaxDecodeDepth = 2

// Config is the operator configuration for a Scanner.
type Config struct {
	// MaxScanBytes caps each scanned field (CAIRN_REDACTION_MAX_SCAN_BYTES).
	// Zero means DefaultMaxScanBytes.
	MaxScanBytes int64
	// Oversize is the policy for a field over the cap
	// (CAIRN_REDACTION_OVERSIZE). Empty means OversizeReject.
	Oversize OversizePolicy
	// AllowlistFile is the operator allowlist TOML
	// (CAIRN_REDACTION_ALLOWLIST_FILE). Empty means none.
	AllowlistFile string
}

// operatorAllowlist is the only shape CAIRN_REDACTION_ALLOWLIST_FILE may take.
// Anything else, including gitleaks' own `paths` and `commits`, is refused by
// name: an ingest scanner's paths are chosen by the uploader (SPEC-0017 RD-8).
type operatorAllowlist struct {
	Regexes       []string `toml:"regexes"`
	StopWords     []string `toml:"stopwords"`
	DisabledRules []string `toml:"disabledRules"`
}

// loadAllowlist reads and validates the operator allowlist file. Every error
// names the file, and the offending entry where there is one.
func loadAllowlist(path string) (*operatorAllowlist, error) {
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("redaction allowlist %s: %w", path, err)
	}
	var al operatorAllowlist
	md, err := toml.Decode(string(raw), &al)
	if err != nil {
		return nil, fmt.Errorf("redaction allowlist %s: does not parse: %w", path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		for _, k := range undecoded {
			if last := k[len(k)-1]; last == "paths" || last == "commits" {
				return nil, fmt.Errorf("redaction allowlist %s: entry %q is unsupported: allowlists match by value only (regexes, stopwords, disabledRules)", path, k.String())
			}
		}
		return nil, fmt.Errorf("redaction allowlist %s: unknown entry %q (supported: regexes, stopwords, disabledRules)", path, undecoded[0].String())
	}
	for i, re := range al.Regexes {
		c, err := regexp.Compile(re)
		if err != nil {
			return nil, fmt.Errorf("redaction allowlist %s: regexes[%d] does not compile: %w", path, i, err)
		}
		if !anchored(re) {
			return nil, fmt.Errorf("redaction allowlist %s: regexes[%d] must be anchored with ^ and $ so it exempts exact values only", path, i)
		}
		if c.MatchString("") {
			return nil, fmt.Errorf("redaction allowlist %s: regexes[%d] matches the empty string, which would exempt everything", path, i)
		}
	}
	for i, w := range al.StopWords {
		if strings.TrimSpace(w) == "" {
			return nil, fmt.Errorf("redaction allowlist %s: stopwords[%d] is empty, which would exempt everything", path, i)
		}
	}
	for i, id := range al.DisabledRules {
		if strings.TrimSpace(id) == "" {
			return nil, fmt.Errorf("redaction allowlist %s: disabledRules[%d] is empty", path, i)
		}
	}
	return &al, nil
}

// anchored reports whether every match of re must span the whole value: each
// alternative starts with ^ and ends with $ (text anchors, not (?m) line
// anchors). It reads the parsed syntax tree rather than the source text, so
// `^fixture|.+$`, whose second branch is unanchored at the start, is refused.
//
// @joestump 09/26/2026 - Review of cairn#382: the textual prefix/suffix check
// accepted `^fixture|.+$`, which exempts every value.
func anchored(re string) bool {
	t, err := syntax.Parse(re, syntax.Perl)
	if err != nil {
		return false
	}
	return anchoredAt(t, 0) && anchoredAt(t, -1)
}

// anchoredAt reports whether t is pinned at its start (end 0) or end (end -1)
// on every path through it.
func anchoredAt(t *syntax.Regexp, end int) bool {
	switch t.Op {
	case syntax.OpBeginText:
		return end == 0
	case syntax.OpEndText:
		return end == -1
	case syntax.OpCapture:
		return anchoredAt(t.Sub[0], end)
	case syntax.OpConcat:
		if len(t.Sub) == 0 {
			return false
		}
		if end == 0 {
			return anchoredAt(t.Sub[0], end)
		}
		return anchoredAt(t.Sub[len(t.Sub)-1], end)
	case syntax.OpAlternate:
		for _, sub := range t.Sub {
			if !anchoredAt(sub, end) {
				return false
			}
		}
		return true
	}
	return false
}

// buildConfig assembles the detector config. It never lets gitleaks extend,
// and it turns any panic from Translate into an error.
func buildConfig(al *operatorAllowlist, allowlistPath string) (cfg config.Config, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = errors.New("redact: building the detector config panicked")
		}
	}()

	def, err := translate([]byte(config.DefaultConfig))
	if err != nil {
		return config.Config{}, fmt.Errorf("redact: gitleaks default config: %w", err)
	}
	own, err := translate(baseConfig)
	if err != nil {
		return config.Config{}, fmt.Errorf("redact: cairn-gitleaks.toml: %w", err)
	}
	merged := merge(def, own)

	if al != nil {
		if len(al.Regexes) > 0 || len(al.StopWords) > 0 {
			op, err := translateAllowlist(al)
			if err != nil {
				return config.Config{}, fmt.Errorf("redaction allowlist %s: %w", allowlistPath, err)
			}
			merged.Allowlists = append(merged.Allowlists, op.Allowlists...)
		}
		for _, id := range al.DisabledRules {
			if _, ok := merged.Rules[id]; !ok {
				return config.Config{}, fmt.Errorf("redaction allowlist %s: disabledRules entry %q is not a known rule", allowlistPath, id)
			}
			delete(merged.Rules, id)
		}
	}
	finalize(&merged)
	return merged, nil
}

// translate parses one TOML config on a private viper instance and translates
// it with any extend stripped, so Translate never touches gitleaks' global
// extend depth or the global viper.
func translate(raw []byte) (config.Config, error) {
	v := viper.New()
	v.SetConfigType("toml")
	if err := v.ReadConfig(bytes.NewReader(raw)); err != nil {
		return config.Config{}, err
	}
	var vc config.ViperConfig
	if err := v.Unmarshal(&vc); err != nil {
		return config.Config{}, err
	}
	vc.Extend = config.Extend{}
	return vc.Translate()
}

// translateAllowlist renders the validated operator entries as a gitleaks
// global allowlist and translates it. The TOML encoder does the quoting, so no
// operator string is ever spliced into TOML by hand.
func translateAllowlist(al *operatorAllowlist) (config.Config, error) {
	doc := struct {
		Title      string `toml:"title"`
		Allowlists []struct {
			Description string   `toml:"description"`
			Regexes     []string `toml:"regexes,omitempty"`
			StopWords   []string `toml:"stopwords,omitempty"`
		} `toml:"allowlists"`
	}{Title: "cairn operator allowlist"}
	doc.Allowlists = append(doc.Allowlists, struct {
		Description string   `toml:"description"`
		Regexes     []string `toml:"regexes,omitempty"`
		StopWords   []string `toml:"stopwords,omitempty"`
	}{Description: "operator value allowlist", Regexes: al.Regexes, StopWords: al.StopWords})
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(doc); err != nil {
		return config.Config{}, err
	}
	return translate(buf.Bytes())
}

// merge overlays Cairn's rules on the default set, as gitleaks' extend would:
// a Cairn rule with a default rule's ID replaces it, and global allowlists are
// appended.
func merge(base, own config.Config) config.Config {
	out := config.Config{
		Title:       own.Title,
		Description: own.Description,
		Rules:       make(map[string]config.Rule, len(base.Rules)+len(own.Rules)),
		Keywords:    map[string]struct{}{},
	}
	for id, r := range base.Rules {
		out.Rules[id] = r
	}
	for id, r := range own.Rules {
		out.Rules[id] = r
	}
	out.Allowlists = append(out.Allowlists, base.Allowlists...)
	out.Allowlists = append(out.Allowlists, own.Allowlists...)
	return out
}

// finalize rebuilds the keyword prefilter and rule order from the rules that
// survived, and clips each rule's tag slice so gitleaks' per-finding
// append(r.Tags, …) always allocates instead of writing into a backing array
// shared by concurrent scans.
func finalize(c *config.Config) {
	c.Keywords = map[string]struct{}{}
	c.OrderedRules = c.OrderedRules[:0]
	for id, r := range c.Rules {
		r.Tags = slices.Clip(r.Tags)
		for _, k := range r.Keywords {
			c.Keywords[strings.ToLower(k)] = struct{}{}
		}
		c.Rules[id] = r
		c.OrderedRules = append(c.OrderedRules, id)
	}
	sort.Strings(c.OrderedRules)
}
