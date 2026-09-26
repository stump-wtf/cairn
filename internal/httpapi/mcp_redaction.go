package httpapi

// Redaction on the MCP Create Tools
//
// artifact_create and bundle_create scan what they store for credentials,
// and an agent should learn that from tools/list, not from a refused call.
// The tool descriptions say so in one sentence; the redaction argument's own
// schema description carries the detail: which share types refuse, that
// "mask" is the only value, and that the output reports the outcome as
// redacted and redactions {count, rules} (SPEC-0017 RD-5, RD-9).
//
// There is deliberately no enum on the argument. The SDK would refuse a
// value outside one with its own schema error before the handler runs, and
// RD-5 requires that refusal to be validation_failed with reason
// unknown_value, which redactionDowngrade produces.
//
// Governing: ADR-0023, SPEC-0017 RD-5, RD-9; SPEC-0007 REQ "Agent-Shaped Tool
// Schemas"
//
// @joestump 09/26/2026 - Added for cairn#295.

// redactionToolDoc is the scanning sentence in the create tools' descriptions.
const redactionToolDoc = " What is stored is scanned for credentials first: a detected value is stored " +
	"as [REDACTED] or refuses the create, depending on the share type (see the redaction argument), " +
	"and the output's redacted and redactions {count, rules} report what was masked."
