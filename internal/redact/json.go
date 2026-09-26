package redact

// JSON Documents
//
// A span's args are a JSON document, and masking one as raw text breaks it:
// the secret-assignment rule masks a quoted value together with its quotes
// ("password": [REDACTED]), a value class that stops only at whitespace eats a
// closing brace or the backslash of an escape, and a re-scan of
// "password": "[REDACTED]" fires again on the quoted mask. Raw text also misses
// the most common shape of all: the header rules expect `Authorization: Bearer
// …`, and a JSON object spells it `"Authorization": "Bearer …"`.
//
// So JSON scans the document's scalars where they stand. Each object member
// is scanned as the text `name: value`, which gives the label rules the name
// they key on, and only the value part is written back. A member name, and an
// array element or top-level scalar, is scanned on its own. A string that
// changed is re-encoded as a JSON string and spliced over the original
// literal's bytes; a number that changed becomes the JSON string of its masked
// text. Everything between the literals, the structure, key order and
// whitespace, is copied through untouched, and the result is checked with
// json.Valid before it is returned: a masked document that would not parse
// fails the scan closed rather than being stored.
//
// Findings from a JSON scan carry no line or column: each was found in one
// decoded scalar, so its position in that scalar is not a position in the
// document.
//
// Governing: ADR-0023, SPEC-0017 RD-3, RD-4
//
// @joestump 09/26/2026 - Added for cairn#291 (span args).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
)

// memberSep joins a member's name to its value when the pair is scanned
// together, the header shape the label rules match.
const memberSep = ": "

// jsonScalar is one string or number literal in a document.
type jsonScalar struct {
	start, end int    // the literal's bytes in the document
	text       string // the decoded string, or the number's literal text
	number     bool
	name       bool   // an object member's name
	label      string // for a member's value: the member's name
	nameMasked bool   // for a member's value: its name was itself masked
}

// JSON scans the JSON document doc and masks detected values in place, so the
// result is still valid JSON with the same structure. A doc that is not valid
// JSON is scanned as text instead, since there is no structure to keep. The
// cap and the modes apply as for Text; the whole document is one field.
func (s *Scanner) JSON(ctx context.Context, field string, doc []byte, mode Mode) ([]byte, Outcome, error) {
	if err := checkMode(mode); err != nil {
		return nil, Outcome{}, err
	}
	if int64(len(doc)) > s.maxBytes {
		out, o, err := s.oversized(field, string(doc), int64(len(doc)))
		return []byte(out), o, err
	}
	if !json.Valid(doc) {
		out, o, err := s.Text(ctx, field, string(doc), mode)
		if err != nil {
			return nil, o, err
		}
		return []byte(out), o, nil
	}
	scalars, err := jsonScalars(doc)
	if err != nil {
		return nil, Outcome{}, s.failed(field, err)
	}

	total := Outcome{Status: StatusClean, Rules: map[string]int{}}
	replaced := make(map[int][]byte) // scalar index -> its new literal
	nameMasked := false
	for i, sc := range scalars {
		if sc.name {
			nameMasked = false
		}
		masked, o, err := s.jsonScalar(ctx, field, sc, nameMasked, mode)
		if err != nil {
			return nil, withoutPositions(o), withoutPositionsErr(err)
		}
		if sc.name && o.Status == StatusMasked {
			nameMasked = true
		}
		total.Count += o.Count
		for rule, n := range o.Rules {
			total.Rules[rule] += n
		}
		total.Findings = append(total.Findings, o.Findings...)
		if o.Status != StatusMasked {
			continue
		}
		total.Status = StatusMasked
		lit, err := encodeJSONString(masked)
		if err != nil {
			return nil, Outcome{}, s.failed(field, err)
		}
		replaced[i] = lit
	}
	total = withoutPositions(total)
	if total.Status != StatusMasked {
		return doc, total, nil
	}

	var b bytes.Buffer
	b.Grow(len(doc))
	prev := 0
	for i, sc := range scalars {
		lit, ok := replaced[i]
		if !ok {
			continue
		}
		b.Write(doc[prev:sc.start])
		b.Write(lit)
		prev = sc.end
	}
	b.Write(doc[prev:])
	out := b.Bytes()
	if !json.Valid(out) {
		return nil, Outcome{}, s.failed(field, errors.New("masked JSON does not parse"))
	}
	return out, total, nil
}

// jsonScalar scans one scalar and returns its masked text. A member's value is
// scanned behind its name, unless the name itself was masked, and only the
// value part is kept; if the mask reached into the name the whole value is
// replaced, which errs toward hiding more.
func (s *Scanner) jsonScalar(ctx context.Context, field string, sc jsonScalar, nameMasked bool, mode Mode) (string, Outcome, error) {
	if sc.label == "" || nameMasked {
		return s.Text(ctx, field, sc.text, mode)
	}
	prefix := sc.label + memberSep
	out, o, err := s.Text(ctx, field, prefix+sc.text, mode)
	if err != nil || o.Status != StatusMasked {
		return sc.text, o, err
	}
	if len(out) >= len(prefix) && out[:len(prefix)] == prefix {
		return out[len(prefix):], o, nil
	}
	return Mask, o, nil
}

// jsonScalars lists doc's string and number literals in document order, with
// their byte ranges and roles. doc must be valid JSON.
func jsonScalars(doc []byte) ([]jsonScalar, error) {
	type frame struct {
		object  bool
		wantKey bool
		key     string
	}
	var (
		stack []frame
		out   []jsonScalar
		prev  int
	)
	// afterValue records that the innermost container just received a value,
	// so an object's next string is a member name again.
	afterValue := func() {
		if n := len(stack); n > 0 && stack[n-1].object {
			stack[n-1].wantKey = true
		}
	}
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.UseNumber()
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		end := int(dec.InputOffset())
		switch v := tok.(type) {
		case json.Delim:
			switch v {
			case '{':
				stack = append(stack, frame{object: true, wantKey: true})
			case '[':
				stack = append(stack, frame{})
			default: // '}' or ']'
				stack = stack[:len(stack)-1]
				afterValue()
			}
		case string, json.Number:
			sc := jsonScalar{start: literalStart(doc, prev), end: end}
			if s, ok := v.(string); ok {
				sc.text = s
			} else {
				sc.text, sc.number = string(v.(json.Number)), true
			}
			if n := len(stack); n > 0 && stack[n-1].object && stack[n-1].wantKey {
				sc.name = true
				stack[n-1].key, stack[n-1].wantKey = sc.text, false
			} else {
				if n > 0 && stack[n-1].object {
					sc.label = stack[n-1].key
				}
				afterValue()
			}
			out = append(out, sc)
		default: // bool or null: nothing to scan
			afterValue()
		}
		prev = end
	}
}

// literalStart returns where the literal after offset prev begins: past the
// whitespace and the ',' or ':' the decoder consumes between tokens.
func literalStart(doc []byte, prev int) int {
	for prev < len(doc) {
		switch doc[prev] {
		case ' ', '\t', '\r', '\n', ',', ':':
			prev++
		default:
			return prev
		}
	}
	return prev
}

// encodeJSONString encodes s as a JSON string literal, without the HTML
// escaping json.Marshal adds, so a masked value reads as it was written.
func encodeJSONString(s string) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// withoutPositions drops line and column from o's findings (see the file
// comment).
func withoutPositions(o Outcome) Outcome {
	for i := range o.Findings {
		o.Findings[i].Line, o.Findings[i].Column = 0, 0
	}
	return o
}

// withoutPositionsErr does the same for a rejection.
func withoutPositionsErr(err error) error {
	var rej *Rejection
	if errors.As(err, &rej) {
		for i := range rej.Findings {
			rej.Findings[i].Line, rej.Findings[i].Column = 0, 0
		}
	}
	return err
}
