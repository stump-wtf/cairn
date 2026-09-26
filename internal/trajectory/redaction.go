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
// A field over the scan cap under CAIRN_REDACTION_OVERSIZE=store_unscanned is
// stored as written, and the write logs a WARN for it once it commits, with
// the run's id and the field's name and size, never its content (RD-7).
//
// Governing: ADR-0023, SPEC-0017 RD-1, RD-4, RD-7, RD-9
//
// @joestump 09/26/2026 - Added for cairn#291.
// @joestump 09/26/2026 - Review: WARN for each field stored unscanned (RD-7).

// runScan is what one write's scans produced: the folded outcome, and the
// fields stored unscanned, which are logged after the write commits.
type runScan struct {
	summary   redact.Summary
	unscanned []unscannedField
}

// unscannedField is a field stored without a scan under store_unscanned.
type unscannedField struct {
	field string
	size  int
}

// add folds one field's outcome in, noting the field if it went unscanned.
func (r *runScan) add(field string, size int, o redact.Outcome) {
	r.summary = r.summary.Merge(o.Summary())
	if o.Status == redact.StatusNotScannedOversize {
		r.unscanned = append(r.unscanned, unscannedField{field: field, size: size})
	}
}

// warnUnscanned logs one WARN per field a committed write stored unscanned
// (SPEC-0017 RD-7): loud each time, with the size and never the content.
func (s *Service) warnUnscanned(ctx context.Context, publicID string, fields []unscannedField) {
	for _, f := range fields {
		s.log.WarnContext(ctx, "trajectory: stored a field WITHOUT a credential scan because it is over the scan cap (CAIRN_REDACTION_OVERSIZE=store_unscanned)",
			"artifact", publicID, "field", f.field, "size_bytes", f.size, "max_scan_bytes", s.scanner.MaxScanBytes())
	}
}

// scanRun masks the run's title, prompt and spans, returning the masked input
// and what the scans produced.
func (s *Service) scanRun(ctx context.Context, in RunInput) (RunInput, runScan, error) {
	if s.scanner == nil {
		return in, runScan{}, nil
	}
	total := runScan{summary: redact.Summary{Status: redact.StatusClean}}
	var err error
	if in.Title, err = s.scanField(ctx, "title", in.Title, &total); err != nil {
		return RunInput{}, runScan{}, err
	}
	if in.Prompt, err = s.scanField(ctx, "prompt", in.Prompt, &total); err != nil {
		return RunInput{}, runScan{}, err
	}
	spans, sc, err := s.scanSpans(ctx, in.Spans)
	if err != nil {
		return RunInput{}, runScan{}, err
	}
	in.Spans = spans
	total.summary = total.summary.Merge(sc.summary)
	total.unscanned = append(total.unscanned, sc.unscanned...)
	return in, total, nil
}

// scanSpans masks each span's name, args and output. It returns a new slice;
// the caller's spans are not modified.
func (s *Service) scanSpans(ctx context.Context, spans []SpanInput) ([]SpanInput, runScan, error) {
	if s.scanner == nil {
		return spans, runScan{}, nil
	}
	total := runScan{summary: redact.Summary{Status: redact.StatusClean}}
	if len(spans) == 0 {
		return spans, total, nil
	}
	out := make([]SpanInput, len(spans))
	for i, sp := range spans {
		// An output past the per-span cap is refused as too large by
		// spillOutputs, as before scanning existed; scanning it first would
		// turn a 413 into a 422.
		if int64(len(sp.Output)) > s.maxOutputBytes {
			return nil, runScan{}, fmt.Errorf("trajectory: span %q output: %w", sp.SpanID, errs.ErrTooLarge)
		}
		var err error
		if sp.Name, err = s.scanField(ctx, fmt.Sprintf("spans[%d].name", i), sp.Name, &total); err != nil {
			return nil, runScan{}, err
		}
		if len(sp.Args) > 0 {
			field := fmt.Sprintf("spans[%d].args", i)
			args, o, err := s.scanner.JSON(ctx, field, sp.Args, redact.ModeMask)
			s.metrics.ObserveRedaction(metrics.SurfaceRun, o, err)
			if err != nil {
				return nil, runScan{}, err
			}
			total.add(field, len(sp.Args), o)
			sp.Args = args
		}
		if len(sp.Output) > 0 {
			text, err := s.scanField(ctx, fmt.Sprintf("spans[%d].output", i), string(sp.Output), &total)
			if err != nil {
				return nil, runScan{}, err
			}
			sp.Output = []byte(text)
		}
		out[i] = sp
	}
	return out, total, nil
}

// scanField masks one text field, counts the scan and folds its outcome into
// total. An empty field holds nothing to find, and is not counted as a scan.
func (s *Service) scanField(ctx context.Context, field, in string, total *runScan) (string, error) {
	if in == "" {
		return in, nil
	}
	out, o, err := s.scanner.Text(ctx, field, in, redact.ModeMask)
	s.metrics.ObserveRedaction(metrics.SurfaceRun, o, err)
	if err != nil {
		return "", err
	}
	total.add(field, len(in), o)
	return out, nil
}
