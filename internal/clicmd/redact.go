package clicmd

import "fmt"

// redactToken renders tok for --verbose diagnostic output without ever
// exposing the secret itself (SPEC-0008 "Secure Credential Storage": "Tokens
// MUST NOT be written to logs ... and the CLI MUST redact them in any
// verbose/debug output"; cairn#21 "Token never appears in logs/errors ...
// redaction test"). It deliberately reveals nothing but a length — no
// prefix/suffix — so no amount of verbose output ever narrows down the
// secret.
func redactToken(tok string) string {
	if tok == "" {
		return "<empty>"
	}
	return fmt.Sprintf("[REDACTED, len=%d]", len(tok))
}
