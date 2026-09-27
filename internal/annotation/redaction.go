package annotation

import (
	"context"

	"github.com/stump-wtf/cairn/internal/metrics"
	"github.com/stump-wtf/cairn/internal/redact"
)

// Comment Redaction
//
// A comment body is a mask-mode surface (SPEC-0017 RD-4): a detected
// credential is replaced with [REDACTED] and the rest of the comment is kept.
// The scan runs before the write's transaction opens, so only the masked body
// is ever inserted, returned or read by anything downstream (RD-1), and a scan
// that fails (a detector error, a panic, a cancelled request) or a value that
// cannot be masked refuses the write rather than storing it unscanned.
//
// Governing: ADR-0023, SPEC-0017 RD-1, RD-4, RD-9
//
// @joestump 09/26/2026 - Added for cairn#291.

// scanBody masks body and counts the scan. With no scanner wired it returns
// body unchanged and the zero Summary, which records "unscanned".
func (s *Service) scanBody(ctx context.Context, body string) (string, redact.Summary, error) {
	if s.scanner == nil {
		return body, redact.Summary{}, nil
	}
	out, o, err := s.scanner.Text(ctx, "body", body, redact.ModeMask)
	s.metrics.ObserveRedaction(metrics.SurfaceComment, o, err)
	if err != nil {
		return "", redact.Summary{}, err
	}
	return out, o.Summary(), nil
}
