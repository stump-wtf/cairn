package clicmd

import (
	"encoding/json"
	"fmt"
	"io"
)

// writeJSON emits v as a single machine-readable JSON object to w
// (SPEC-0008 "Output Formats and --json Mode": "stdout MUST contain a
// single valid JSON object ... and MUST contain no non-JSON decoration").
func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("clicmd: encode JSON output: %w", err)
	}
	return nil
}
