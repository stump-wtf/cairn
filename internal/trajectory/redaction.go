package trajectory

import (
	"context"
	"fmt"

	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/metrics"
	"github.com/stump-wtf/cairn/internal/redact"
)

// Trace Redaction
//
// A trace is a mask-mode surface (SPEC-0017 RD-4): an agent's transcript is
// where tokens leak most, and refusing a live append would lose the rest of
// the run, so a detected credential is replaced with [REDACTED] and the span
// is kept. The scanned fields are the run's title and prompt and each span's
// name, args and output, inline or spilled.
//
// Every field is scanned before the write's transaction opens and before any
// output is staged, so neither a row, a staged or promoted blob, nor a span
// published to live viewers ever holds the unmasked text (RD-1). Span outputs
// are already in memory (the request body is buffered under its own cap), so
// they are scanned with Text whatever their size, which windows a large one
// exactly as Staged would, without first writing the raw bytes to object
// storage. Args are a JSON document and go through Scanner.JSON, which keeps
// them valid JSON. A scan that fails, or a value that cannot be masked, refuses
// the whole write: a run is never stored partly scanned.
//
// The outcomes of every field fold into one redact.Summary per write. A
// create records it on the run and its artifact envelope; an append folds it
// into both with store.AccumulateRedaction, in the append's transaction.
//
// Governing: ADR-0023, SPEC-0017 RD-1, RD-4, RD-7, RD-9
//
// @joestump 09/26/2026 - Added for cairn#291.

// scanRun masks the run's title, prompt and spans, returning the masked input
// and the folded outcome.
func (s *Service) scanRun(ctx context.Context, in RunInput) (RunInput, redact.Summary, error) {
	if s.scanner == nil {
		return in, redact.Summary{}, nil
	}
	total := redact.Summary{Status: redact.StatusClean}
	var err error
	if in.Title, err = s.scanField(ctx, "title", in.Title, &total); err != nil {
		return RunInput{}, redact.Summary{}, err
	}
	if in.Prompt, err = s.scanField(ctx, "prompt", in.Prompt, &total); err != nil {
		return RunInput{}, redact.Summary{}, err
	}
	spans, sum, err := s.scanSpans(ctx, in.Spans)
	if err != nil {
		return RunInput{}, redact.Summary{}, err
	}
	in.Spans = spans
	return in, total.Merge(sum), nil
}

// scanSpans masks each span's name, args and output. It returns a new slice;
// the caller's spans are not modified.
func (s *Service) scanSpans(ctx context.Context, spans []SpanInput) ([]SpanInput, redact.Summary, error) {
	if s.scanner == nil {
		return spans, redact.Summary{}, nil
	}
	total := redact.Summary{Status: redact.StatusClean}
	if len(spans) == 0 {
		return spans, total, nil
	}
	out := make([]SpanInput, len(spans))
	for i, sp := range spans {
		// An output past the per-span cap is refused as too large by
		// spillOutputs, as before scanning existed; scanning it first would
		// turn a 413 into a 422.
		if int64(len(sp.Output)) > s.maxOutputBytes {
			return nil, redact.Summary{}, fmt.Errorf("trajectory: span %q output: %w", sp.SpanID, errs.ErrTooLarge)
		}
		var err error
		if sp.Name, err = s.scanField(ctx, fmt.Sprintf("spans[%d].name", i), sp.Name, &total); err != nil {
			return nil, redact.Summary{}, err
		}
		if len(sp.Args) > 0 {
			args, o, err := s.scanner.JSON(ctx, fmt.Sprintf("spans[%d].args", i), sp.Args, redact.ModeMask)
			s.metrics.ObserveRedaction(metrics.SurfaceRun, o, err)
			if err != nil {
				return nil, redact.Summary{}, err
			}
			sp.Args = args
			total = total.Merge(o.Summary())
		}
		if len(sp.Output) > 0 {
			text, err := s.scanField(ctx, fmt.Sprintf("spans[%d].output", i), string(sp.Output), &total)
			if err != nil {
				return nil, redact.Summary{}, err
			}
			sp.Output = []byte(text)
		}
		out[i] = sp
	}
	return out, total, nil
}

// scanField masks one text field, counts the scan and folds its outcome into
// total. An empty field holds nothing to find, and is not counted as a scan.
func (s *Service) scanField(ctx context.Context, field, in string, total *redact.Summary) (string, error) {
	if in == "" {
		return in, nil
	}
	out, o, err := s.scanner.Text(ctx, field, in, redact.ModeMask)
	s.metrics.ObserveRedaction(metrics.SurfaceRun, o, err)
	if err != nil {
		return "", err
	}
	*total = total.Merge(o.Summary())
	return out, nil
}
