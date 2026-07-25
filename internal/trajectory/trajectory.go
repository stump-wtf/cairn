// Package trajectory is the core service for the trajectory share type: it
// persists an agent run as an ordered, nested span tree, spills oversized span
// outputs to content-addressed object storage, links a run to the artifacts it
// produced, and derives all run statistics from the span rows so they can never
// disagree with the waterfall. Every method takes a context.Context first and
// propagates it to Postgres and object storage, so a cancelled request releases
// its resources.
//
// The REST, MCP, and CLI surfaces are thin adapters over this one package
// (ADR-0003 / ADR-0012); it owns no transport concern. It is OTel-inspired, not
// OTLP-compliant: it borrows the ordered nested-span-tree-with-timing shape and
// leaves behind sampling, trace-context propagation, and the OTLP wire protocol.
//
// Governing: ADR-0009 (Trajectory Capture & OTel-Inspired Span Model),
// ADR-0008 (Storage & Content Model), ADR-0012 (Backend Platform and API Shape),
// SPEC-0004 (Trajectory Share)
package trajectory

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/joestump/cairn/internal/artifact"
	"github.com/joestump/cairn/internal/errs"
)

// Category is the closed set of span categories. Keeping it closed keeps the
// waterfall's color legend and the time-by-category breakdown total
// (SPEC-0004 "Span Model and Ordered Tree").
type Category string

const (
	CategoryReason Category = "reason"
	CategoryExec   Category = "exec"
	CategoryRead   Category = "read"
	CategoryNet    Category = "net"
	CategoryWrite  Category = "write"
)

// valid reports whether c is one of the five allowed categories.
func (c Category) valid() bool {
	switch c {
	case CategoryReason, CategoryExec, CategoryRead, CategoryNet, CategoryWrite:
		return true
	default:
		return false
	}
}

// toolCapable reports whether a span of this category may carry a tool name. A
// reasoning turn runs no tool; exec/read/net/write spans do (SPEC-0004: tool is
// "non-null only for exec/read/net/write spans"). A sub-agent is an ordinary
// span — typically net-categorized — whose tool is `sub-agent`.
func (c Category) toolCapable() bool {
	switch c {
	case CategoryExec, CategoryRead, CategoryNet, CategoryWrite:
		return true
	default:
		return false
	}
}

// Status is a run's lifecycle state.
type Status string

const (
	StatusOpen   Status = "open"
	StatusClosed Status = "closed"
)

// Sentinel domain errors callers distinguish, each mapped to a stable code by a
// transport adapter via errs.CodeOf without string matching (SPEC-0004 "Error
// Handling Standards").
var (
	// ErrRunNotFound is returned uniformly for an unknown, unauthorized, or
	// expired run id so probing leaks no signal (ADR-0007 link-capability).
	ErrRunNotFound = errs.New(errs.CodeNotFound, "trajectory run not found")
	// ErrRunClosed is the distinct append-after-close sentinel an adapter maps
	// to conflict (409), never a generic error.
	ErrRunClosed = errs.New(errs.CodeConflict, "trajectory run is closed")
	// ErrUnknownParent rejects a span whose parent_span_id is absent from the
	// run (missing or cyclic), atomically persisting none of the payload.
	ErrUnknownParent = errs.New(errs.CodeValidation, "trajectory span references an unknown parent span")
	// ErrUnknownCategory rejects a span whose category is outside the closed set.
	ErrUnknownCategory = errs.New(errs.CodeValidation, "trajectory span has an unknown category")
	// ErrNotOwner rejects an append or close by a non-owning principal (403).
	ErrNotOwner = errs.New(errs.CodeForbidden, "only the run owner may modify the run")
)

// OutputRef references a span's oversized output stored as a content-addressed
// blob (SPEC-0004 "Span Output Storage"). It is fetched lazily on expand.
type OutputRef struct {
	SHA256    string
	Size      int64
	Truncated bool
}

// SpanInput is one span as supplied by an ingesting agent. depth and seq are
// NOT supplied — the service derives them from the parent chain and sibling
// order. Output carries the full bytes; the service inlines it or spills it to
// a content-addressed blob depending on size.
type SpanInput struct {
	SpanID          string
	ParentSpanID    string // "" => top-level
	Category        Category
	Name            string
	Tool            string // "" => none; non-empty only for exec/read/net/write
	Args            json.RawMessage
	Output          []byte
	OutputTruncated bool
	StartOffsetMS   int
	DurationMS      int
	// ProducedArtifactID is the public id of an artifact this write span
	// produced. Non-empty only on a write span (SPEC-0004 "Produced-Artifact
	// Link").
	ProducedArtifactID string
}

// RunInput is a run to ingest — batch (with a full Spans tree, created closed)
// or incremental (empty Spans, opened live). Provenance, Access, and ExpiresAt
// are the ordinary artifact envelope this trajectory gets for free (SPEC-0002).
type RunInput struct {
	Title      string
	Prompt     string
	Model      string
	TokenCount int64
	StartedAt  time.Time
	Provenance artifact.Provenance
	Access     artifact.AccessPolicy
	ExpiresAt  time.Time
	Spans      []SpanInput
}

func (in RunInput) validate() error {
	switch {
	case in.Provenance.Channel == "":
		return errs.Validationf("trajectory: provenance channel is required")
	case in.Provenance.ActorID == "":
		return errs.Validationf("trajectory: provenance actor is required")
	case in.Provenance.CapturedAt.IsZero():
		return errs.Validationf("trajectory: provenance capture time is required")
	case in.Access.OwnerID == "":
		return errs.Validationf("trajectory: access owner is required")
	case in.Access.Visibility == "":
		return errs.Validationf("trajectory: access visibility is required")
	case in.ExpiresAt.IsZero():
		return errs.Validationf("trajectory: expiry is required")
	case in.StartedAt.IsZero():
		return errs.Validationf("trajectory: started_at is required")
	}
	return nil
}

// Span is a node in the reconstructed ordered tree. Exactly one of Inline / Ref
// describes its output (both zero => empty output). Children are ordered by seq.
type Span struct {
	SpanID              string
	ParentSpanID        string
	Depth               int
	Seq                 int
	Category            Category
	Name                string
	Tool                string
	Args                json.RawMessage
	Inline              string
	Ref                 *OutputRef
	StartOffsetMS       int
	DurationMS          int
	ProducedArtifactIDs []string
	Children            []*Span
}

// Stats are the RUN-panel figures, all derived from the span rows plus the
// run's token count — never stored as independent authoritative fields
// (SPEC-0004 "Derived Run Statistics").
type Stats struct {
	WallTimeMS       int64
	SpanCount        int
	ToolCallCount    int
	TokenCount       int64
	TimeByCategoryMS map[Category]int64
}

// Run is a run with its ordered span tree and derived stats.
type Run struct {
	PublicID   string
	Title      string
	Prompt     string
	Model      string
	Status     Status
	StartedAt  time.Time
	EndedAt    time.Time // zero while open
	TokenCount int64
	Provenance artifact.Provenance
	Access     artifact.AccessPolicy
	ExpiresAt  time.Time
	Spans      []*Span // ordered roots
	Stats      Stats
}

// preparedSpan is a SpanInput with its service-derived depth and sibling seq.
type preparedSpan struct {
	in    SpanInput
	depth int
	seq   int
}

// prepareSpans validates a batch of new spans against the run's existing state
// and derives each span's depth (from its parent chain) and gap-free sibling
// seq (continuing any already-persisted siblings). It is pure — no database, no
// context — so tree validation and ordering are unit-testable in isolation.
//
//   - existingDepth maps a persisted span_id to its depth (empty for a fresh run).
//   - existingChildCount maps a parent_span_id ("" for the top level) to the
//     number of already-persisted children, so appended siblings continue the seq.
//   - existingIDs is the set of persisted span_ids, for duplicate detection.
//
// Categories outside the closed set fail with ErrUnknownCategory; a parent that
// resolves to neither a persisted nor a same-batch span (missing or cyclic)
// fails with ErrUnknownParent. On any error nothing is prepared, so the caller's
// transaction persists none of the payload (SPEC-0004 "Malformed tree rejected
// atomically").
func prepareSpans(existingDepth map[string]int, existingChildCount map[string]int, existingIDs map[string]bool, news []SpanInput) ([]preparedSpan, error) {
	byID := make(map[string]SpanInput, len(news))
	for _, s := range news {
		if s.SpanID == "" {
			return nil, errs.Validationf("trajectory: span is missing a span_id")
		}
		if existingIDs[s.SpanID] {
			return nil, errs.Validationf("trajectory: span %q already exists in the run", s.SpanID)
		}
		if _, dup := byID[s.SpanID]; dup {
			return nil, errs.Validationf("trajectory: duplicate span_id %q in payload", s.SpanID)
		}
		byID[s.SpanID] = s
		if !s.Category.valid() {
			return nil, fmt.Errorf("trajectory: span %q category %q: %w", s.SpanID, s.Category, ErrUnknownCategory)
		}
		if s.Tool != "" && !s.Category.toolCapable() {
			return nil, errs.Validationf("trajectory: span %q carries tool %q but category %q takes no tool", s.SpanID, s.Tool, s.Category)
		}
		if s.ProducedArtifactID != "" && s.Category != CategoryWrite {
			return nil, errs.Validationf("trajectory: span %q declares a produced artifact but is not a write span", s.SpanID)
		}
		if s.StartOffsetMS < 0 || s.DurationMS < 0 {
			return nil, errs.Validationf("trajectory: span %q has negative timing", s.SpanID)
		}
	}

	// Resolve depths by fixpoint so the payload need not be topologically
	// pre-sorted: a span resolves once its parent's depth is known (persisted or
	// resolved earlier in this pass). A pass with no progress means every
	// unresolved span points at a missing or cyclic parent.
	newDepth := make(map[string]int, len(news))
	resolved := make(map[string]bool, len(news))
	remaining := len(news)
	for remaining > 0 {
		progress := false
		for _, s := range news {
			if resolved[s.SpanID] {
				continue
			}
			p := s.ParentSpanID
			switch {
			case p == "":
				newDepth[s.SpanID] = 0
			case p == s.SpanID:
				return nil, fmt.Errorf("trajectory: span %q is its own parent: %w", s.SpanID, ErrUnknownParent)
			case hasKey(existingDepth, p):
				newDepth[s.SpanID] = existingDepth[p] + 1
			case resolved[p]:
				newDepth[s.SpanID] = newDepth[p] + 1
			default:
				continue // parent not resolved yet (or unknown)
			}
			resolved[s.SpanID] = true
			progress = true
			remaining--
		}
		if !progress {
			for _, s := range news {
				if !resolved[s.SpanID] {
					return nil, fmt.Errorf("trajectory: span %q parent %q: %w", s.SpanID, s.ParentSpanID, ErrUnknownParent)
				}
			}
		}
	}

	// Assign seq per parent in input order, continuing persisted siblings.
	childCount := make(map[string]int, len(existingChildCount)+len(news))
	for k, v := range existingChildCount {
		childCount[k] = v
	}
	out := make([]preparedSpan, 0, len(news))
	for _, s := range news {
		key := s.ParentSpanID // "" for the top level
		seq := childCount[key]
		childCount[key] = seq + 1
		out = append(out, preparedSpan{in: s, depth: newDepth[s.SpanID], seq: seq})
	}
	return out, nil
}

func hasKey(m map[string]int, k string) bool {
	_, ok := m[k]
	return ok
}

// buildTree assembles a flat, per-parent-seq-ordered span slice into an ordered
// forest, nesting each span under its parent and returning the top-level roots.
// It relies on the persisted depth/seq rather than re-deriving the tree, so the
// same run renders identically every time (SPEC-0004 "Deterministic layout").
func buildTree(flat []*Span) []*Span {
	byID := make(map[string]*Span, len(flat))
	for _, s := range flat {
		byID[s.SpanID] = s
	}
	var roots []*Span
	for _, s := range flat {
		if s.ParentSpanID == "" {
			roots = append(roots, s)
			continue
		}
		if p, ok := byID[s.ParentSpanID]; ok {
			p.Children = append(p.Children, s)
		} else {
			// Defensive: an orphan (should not happen given FK-free integrity)
			// surfaces at the root rather than vanishing.
			roots = append(roots, s)
		}
	}
	sortBySeq(roots)
	for _, s := range flat {
		sortBySeq(s.Children)
	}
	return roots
}

func sortBySeq(spans []*Span) {
	sort.SliceStable(spans, func(i, j int) bool { return spans[i].Seq < spans[j].Seq })
}

// computeStats derives the RUN-panel figures from the flat span rows plus the
// run's token count and its wall time. Span count and tool-call count are row
// counts (tool calls = spans with a non-null tool); time-by-category sums
// duration per category over the same rows the waterfall draws, so the panel
// and the waterfall can never disagree (SPEC-0004 "Derived Run Statistics").
func computeStats(flat []*Span, tokenCount, wallMS int64) Stats {
	st := Stats{
		WallTimeMS:       wallMS,
		SpanCount:        len(flat),
		TokenCount:       tokenCount,
		TimeByCategoryMS: make(map[Category]int64, 5),
	}
	for _, s := range flat {
		if s.Tool != "" {
			st.ToolCallCount++
		}
		st.TimeByCategoryMS[s.Category] += int64(s.DurationMS)
	}
	return st
}

// maxSpanEndMS returns the largest span end offset (start + duration) over the
// spans, which is the run's wall time — used to stamp ended_at deterministically
// on close so a batch run and the equivalent open→append→close converge to the
// identical wall time (SPEC-0004 "Batch and incremental converge").
func maxSpanEndMS(flat []*Span) int64 {
	var max int64
	for _, s := range flat {
		if end := int64(s.StartOffsetMS) + int64(s.DurationMS); end > max {
			max = end
		}
	}
	return max
}
