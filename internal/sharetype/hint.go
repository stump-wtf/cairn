package sharetype

import "context"

// bodyHintKey is the unexported context key WithBodyHint/BodyHint share.
type bodyHintKey struct{}

// WithBodyHint attaches a free-form, per-request override string to ctx that a
// BodyViewer capability MAY consult while rendering — e.g. the code viewer
// reads it as an explicit `?lang=` language override, taking precedence over
// its media_type/title detection (SPEC-0003 REQ "Code Viewer": "Language
// detected from media_type/title/extension, overridable"). It keeps the
// BodyViewer interface's signature — ctx, artifact, body reader — stable
// across every type (a type that has no use for a hint simply never reads it)
// while still letting httpapi pass a query-string override down to whichever
// viewer resolves through the registry, without a switch on the type key. An
// empty hint is a no-op so callers can pass a possibly-absent query value
// unconditionally.
func WithBodyHint(ctx context.Context, hint string) context.Context {
	if hint == "" {
		return ctx
	}
	return context.WithValue(ctx, bodyHintKey{}, hint)
}

// BodyHint returns the hint WithBodyHint attached to ctx, or "" when none was
// set.
func BodyHint(ctx context.Context) string {
	h, _ := ctx.Value(bodyHintKey{}).(string)
	return h
}
