package clicmd

import (
	"errors"
	"regexp"
	"strconv"
	"strings"

	"github.com/stump-wtf/cairn/internal/cliclient"
)

// A validation error names wire fields (X-Cairn-Ttl-Seconds, tags[2],
// members[1].content), but a human typed flags and file arguments. This file
// maps the one onto the other, so `cairn add notes.md --ttl 60d` fails with
// "--ttl 60d exceeds the server's maximum of 30d" rather than a bare
// "validation_failed". The server's verdict is shown, never recomputed: the
// CLI rewords only the limits it can state in the user's own units.
//
// Governing: ADR-0025 (Actionable Validation Errors), SPEC-0019 VE-8,
// SPEC-0008 REQ "Machine-Readable Error Mapping and Exit Codes"

// sentRequest is what a create command sent, in the user's own terms, so a
// violation can be pointed back at the flag or argument that carried it.
type sentRequest struct {
	ttl   string   // --ttl as typed ("60d"), not the seconds sent
	tags  []string // tags as sent, after case folding, in order
	files []string // file arguments, in bundle member order
	// redact is --redact as sent: "mask" when the writer already downgraded,
	// so a rejection does not advise sending it again.
	redact string
}

// requestError carries sentRequest to printError alongside the failure. It is
// transparent to errors.Is and errors.As, so exit-code mapping is unchanged.
type requestError struct {
	err  error
	sent sentRequest
}

func (e *requestError) Error() string { return e.err.Error() }
func (e *requestError) Unwrap() error { return e.err }

// withSent attaches sent to err, or returns nil for a nil err.
func withSent(err error, sent sentRequest) error {
	if err == nil {
		return nil
	}
	return &requestError{err: err, sent: sent}
}

// sentOf returns the request context attached to err, if any.
func sentOf(err error) sentRequest {
	var re *requestError
	if errors.As(err, &re) {
		return re.sent
	}
	return sentRequest{}
}

// violationLines renders one line per violation (without the "cairn: "
// prefix), or nil when there is nothing more specific to say than the
// top-level message: an old server sent no violations, or the only one is the
// generic entry an unmigrated validator produces (SPEC-0019 VE-6).
func violationLines(vs []cliclient.Violation, sent sentRequest) []string {
	specific := false
	for _, v := range vs {
		if !isGeneric(v) {
			specific = true
			break
		}
	}
	if !specific {
		return nil
	}
	lines := make([]string, len(vs))
	for i, v := range vs {
		lines[i] = violationLine(v, sent)
	}
	return lines
}

func isGeneric(v cliclient.Violation) bool {
	return v.Reason == cliclient.ReasonInvalid && v.Field == "request"
}

// violationLine renders one violation as "<subject> <predicate>", where the
// subject is the flag or file argument and the value the user gave it.
func violationLine(v cliclient.Violation, sent sentRequest) string {
	subject, showsValue := violationSubject(v, sent)
	limit := limitText(v)
	switch {
	case v.Reason == cliclient.ReasonExceedsMax && limit != "":
		return subject + " exceeds the server's maximum of " + limit
	case v.Reason == cliclient.ReasonTooLong && limit != "":
		return subject + " is too long: the server's maximum is " + limit
	case v.Reason == cliclient.ReasonTooMany && limit != "":
		return subject + " has too many entries: the server's maximum is " + limit
	case v.Reason == cliclient.ReasonTooLarge && limit != "":
		return subject + " is too large: the server's maximum is " + limit
	}

	// Every other reason is the server's own sentence. The server starts it
	// with the quoted value when it echoed one; drop that when the subject
	// already shows the value, so it is not said twice.
	msg := v.Message
	if showsValue && v.Value != nil {
		msg = strings.TrimPrefix(msg, strconv.Quote(*v.Value)+" ")
	}
	if msg == "" {
		msg = "is not valid (" + v.Reason + ")"
	}
	// The server's sentences are predicates ("must be lowercase", "contains a
	// character …"), except a detected secret's, which is a clause of its own.
	if v.Reason == "secret_detected" || isGeneric(v) {
		return subject + ": " + msg
	}
	return subject + " " + msg
}

// indexedField matches tags[n] and members[n].<rest>.
var indexedField = regexp.MustCompile(`^(tags|members)\[(\d+)\](\..*)?$`)

// flagFor maps a wire field onto the CLI flag that sets it (the SPEC-0019
// design table). Header names are matched case-insensitively.
var flagFor = map[string]string{
	"x-cairn-ttl-seconds": "--ttl",
	"ttl_seconds":         "--ttl",
	"tag":                 "--tag",
	"tags":                "--tag",
	"x-cairn-tags":        "--tag",
	"x-cairn-type":        "--type",
	"type":                "--type",
	"title":               "--title",
	"x-cairn-title":       "--title",
	"x-cairn-redaction":   "--redact",
}

// violationSubject names what the violation is about, in CLI terms, and
// reports whether the name includes the offending value.
func violationSubject(v cliclient.Violation, sent sentRequest) (string, bool) {
	value := ""
	if v.Value != nil {
		value = *v.Value
	}

	if m := indexedField.FindStringSubmatch(v.Field); m != nil {
		n, err := strconv.Atoi(m[2])
		if err == nil && m[1] == "members" {
			// A member is named by the file argument that became it.
			if n < len(sent.files) {
				return shellWord(sent.files[n]), false
			}
			return v.Field, false
		}
		if err == nil && m[1] == "tags" && m[3] == "" {
			if value == "" && n < len(sent.tags) {
				value = sent.tags[n]
			}
			return withValue("--tag", value), value != ""
		}
		return v.Field, false
	}

	flag, ok := flagFor[strings.ToLower(v.Field)]
	if !ok {
		return v.Field, false
	}
	if flag == "--ttl" {
		// Show the TTL as the user typed it (60d), not the seconds sent.
		if sent.ttl != "" {
			return withValue(flag, sent.ttl), true
		}
		if n, err := strconv.ParseInt(value, 10, 64); err == nil && n > 0 {
			return withValue(flag, formatTTLSeconds(n)), true
		}
	}
	return withValue(flag, value), value != ""
}

func withValue(flag, value string) string {
	if value == "" {
		return flag
	}
	return flag + " " + shellWord(value)
}

// limitText states a violation's limit in the unit a user would type: TTLs in
// the largest whole unit --ttl accepts (2592000 seconds is 30d), byte caps in
// an exact binary unit where there is one.
func limitText(v cliclient.Violation) string {
	if v.Limit == nil {
		return ""
	}
	n, isInt := v.Limit.Int()
	switch {
	case v.Unit == cliclient.UnitSeconds && isInt && n > 0:
		return formatTTLSeconds(n)
	case v.Unit == cliclient.UnitBytes && isInt:
		return formatByteLimit(n)
	case v.Unit == "" || v.Unit == "count":
		return v.Limit.String()
	default:
		return v.Limit.String() + " " + v.Unit
	}
}

// formatByteLimit renders n bytes in the largest binary unit that divides it
// exactly (10 MiB), or as a plain byte count. A limit is a boundary, so it is
// never rounded: "10.5 MB" could sit either side of the file that broke it.
func formatByteLimit(n int64) string {
	units := []string{"bytes", "KiB", "MiB", "GiB", "TiB"}
	i := 0
	for i < len(units)-1 && n != 0 && n%1024 == 0 {
		n /= 1024
		i++
	}
	return strconv.FormatInt(n, 10) + " " + units[i]
}

// shellWord shows s as a user would type it: bare when it is one plain
// word, quoted when it holds spaces, quotes or anything unusual.
func shellWord(s string) string {
	if s == "" {
		return `""`
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		plain := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			strings.IndexByte("._:/#@%+=,-~", c) >= 0
		if !plain {
			return strconv.Quote(s)
		}
	}
	return s
}
