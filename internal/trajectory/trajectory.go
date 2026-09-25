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
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/id"
)

// Category is an OPEN set of span categories: any non-empty string is accepted
// so agents are never forced to remap their natural vocabulary onto ours
// (ADR-0009, SPEC-0004 "Non-recommended category accepted"). RecommendedCategories
// is the subset the waterfall color-maps; everything else renders with a neutral
// default color and its own text label, so the legend and the time-by-category
// breakdown still total correctly over whatever an agent actually sent.
type Category string

// The recommended vocabulary spans two ways agents naturally describe a run,
// because in practice they reach for one or the other and neither is wrong:
//
//   - OPERATION KIND — what the agent did in this span (reason, read, exec…).
//     The original set; it maps onto the OTel-ish span shape ADR-0009 describes.
//   - WORKFLOW PHASE — where in the job the span sat (research, implementation,
//     testing…). Left to itself an agent reaches for SDLC vocabulary far more
//     readily than ours, and the two vocabularies do not overlap at all, so a
//     phase-labelled run used to land entirely on the neutral color.
//
// Both are advertised in the run_create/run_append_spans schema and the
// `run_capture` MCP prompt, and both are color-mapped in trajectory.css. Mixing
// them within one run is allowed but reads poorly — the prompt says to pick one.
const (
	// Operation kind.
	CategoryReason  Category = "reason"
	CategoryExec    Category = "exec"
	CategoryRead    Category = "read"
	CategoryNet     Category = "net"
	CategoryWrite   Category = "write"
	CategorySearch  Category = "search"
	CategoryPlan    Category = "plan"
	CategoryTool    Category = "tool"
	CategoryAnalyze Category = "analyze"
	CategoryTest    Category = "test"
	CategoryFix     Category = "fix"
	CategoryFail    Category = "fail"
	CategoryMeta    Category = "meta"

	// Workflow phase.
	CategoryResearch       Category = "research"
	CategoryImplementation Category = "implementation"
	CategoryReview         Category = "review"
	CategoryTesting        Category = "testing"
	CategoryDebug          Category = "debug"
	CategoryBuild          Category = "build"
	CategoryDocs           Category = "docs"
	CategoryDelivery       Category = "delivery"
	CategoryDeploy         Category = "deploy"
	CategoryWait           Category = "wait"
	CategoryPrompt         Category = "prompt"
)

// OperationCategories names a span by WHAT THE AGENT DID in it. Original order
// preserved, so an existing run's legend renders exactly as it did before the
// phase vocabulary was added.
var OperationCategories = []Category{
	CategoryReason, CategoryNet, CategoryExec, CategoryRead, CategoryWrite,
	CategorySearch, CategoryPlan, CategoryTool, CategoryAnalyze,
	CategoryTest, CategoryFix, CategoryFail, CategoryMeta,
}

// PhaseCategories names a span by WHERE IN THE JOB it sat, in workflow order.
// `wait` and `prompt` are last because they describe elapsed time rather than
// work: `wait` is blocked on something external (CI, a rate limit), `prompt` is
// the human composing their next instruction.
//
// `prompt` is rendered specially — the waterfall draws it as a 💬 marker rather
// than a bar. A human taking four minutes to type is real elapsed time, but it
// is not something the run DID, and as a proportional bar it dwarfed the work
// either side of it and told the reader nothing.
var PhaseCategories = []Category{
	CategoryResearch, CategoryImplementation, CategoryReview, CategoryTesting,
	CategoryDebug, CategoryBuild, CategoryDocs, CategoryDelivery,
	CategoryDeploy, CategoryWait, CategoryPrompt,
}

// RecommendedCategories is the color-mapped set, in the fixed order the legend
// and the time-by-category breakdown render them. It is presentation order, not
// a validation whitelist — prepareSpans accepts categories outside it. The
// viewer derives its ordering from this one slice so the legend can never drift
// from the palette (SPEC-0004 "Span Model and Ordered Tree"), and the MCP
// surface derives the vocabulary it advertises from the same two slices so the
// schema, the run_capture prompt, and the palette cannot disagree.
var RecommendedCategories = append(append([]Category{}, OperationCategories...), PhaseCategories...)

// MaxCategoryLen bounds an accepted category. The set is open, but the value is
// unbounded agent-supplied text that lands in a TEXT column and is rendered in
// every legend, so it needs *some* ceiling; 64 is far above any real vocabulary
// (SPEC-0004 "Category length bounded").
const MaxCategoryLen = 64

// valid reports whether c is an acceptable category: any non-empty,
// non-whitespace-only string within MaxCategoryLen.
//
// The ceiling counts RUNES, not bytes, to mean the same thing as the schema's
// `length(category) <= 64` — Postgres length() counts characters, so a byte
// count here would reject a 40-character Japanese category the database would
// happily store. Where the two layers cannot align exactly (Go trims by
// unicode.IsSpace, the schema by a POSIX \S class), this one is deliberately the
// stricter: the service refuses first, and the CHECK is only ever a backstop.
func (c Category) valid() bool {
	return strings.TrimSpace(string(c)) != "" && utf8.RuneCountInString(string(c)) <= MaxCategoryLen
}

// toolCapable reports whether a span of this category may carry a tool name.
// With an open category set the only rule the record still states is that a
// reasoning turn runs no tool (ADR-0009: `tool` is "null for `reason` spans") —
// every other category, recommended or not, may carry one, because we cannot
// know which of an agent's own categories are tool-shaped. A sub-agent is an
// ordinary span — typically net-categorized — whose tool is `sub-agent`.
func (c Category) toolCapable() bool {
	return c != CategoryReason
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
	// ErrEmptyCategory rejects a span whose category is empty, whitespace-only,
	// or longer than MaxCategoryLen. The set is otherwise open: an unrecognised
	// category is accepted and rendered neutrally, never rejected.
	ErrEmptyCategory = errs.New(errs.CodeValidation, "trajectory span has an empty or over-long category")
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
	Tool            string // "" => none; non-empty on any category except reason
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
// An empty, whitespace-only, or over-long category fails with ErrEmptyCategory
// (any other non-empty string is accepted, recommended or not); a parent that
// resolves to neither a persisted nor a same-batch span (missing or cyclic)
// fails with ErrUnknownParent. On any error nothing is prepared, so the caller's
// transaction persists none of the payload (SPEC-0004 "Malformed tree rejected
// atomically").
//
// Every problem in the batch is reported, not only the first: each is a
// violation on spans[n].<field>, where n is the span's index in the payload,
// and the error still satisfies errors.Is against ErrEmptyCategory and
// ErrUnknownParent when either kind is present.
//
// Governing: ADR-0025, SPEC-0019 VE-1, VE-4, VE-6
func prepareSpans(existingDepth map[string]int, existingChildCount map[string]int, existingIDs map[string]bool, news []SpanInput) ([]preparedSpan, error) {
	var b spanProblems
	b.perSpan = make([][]*errs.Invalid, len(news))

	byID := make(map[string]int, len(news)) // span_id -> index of its first occurrence
	for i, s := range news {
		switch _, dup := byID[s.SpanID]; {
		case s.SpanID == "":
			b.add(i, "span_id", errs.ReasonRequired, nil)
		case existingIDs[s.SpanID], dup:
			b.add(i, "span_id", errs.ReasonDuplicate, nil, errs.WithValue(s.SpanID))
		default:
			byID[s.SpanID] = i
		}
		if !s.Category.valid() {
			if strings.TrimSpace(string(s.Category)) == "" {
				b.add(i, "category", errs.ReasonRequired, ErrEmptyCategory)
			} else {
				b.add(i, "category", errs.ReasonTooLong, ErrEmptyCategory,
					errs.WithValue(string(s.Category)), errs.WithLimit(MaxCategoryLen, errs.UnitChars))
			}
		}
		if s.Tool != "" && !s.Category.toolCapable() {
			b.add(i, "tool", errs.ReasonNotAllowed, nil, errs.WithValue(s.Tool),
				errs.WithExpect(fmt.Sprintf("a %q span runs no tool", CategoryReason)))
		}
		if s.ProducedArtifactID != "" && s.Category != CategoryWrite {
			b.add(i, "produced_artifact_id", errs.ReasonNotAllowed, nil, errs.WithValue(s.ProducedArtifactID),
				errs.WithExpect(fmt.Sprintf("only a %q span declares a produced artifact", CategoryWrite)))
		}
		for _, t := range []struct {
			field string
			ms    int
		}{{"start_offset_ms", s.StartOffsetMS}, {"duration_ms", s.DurationMS}} {
			if t.ms < 0 {
				b.add(i, t.field, errs.ReasonNotPositive, nil, errs.WithValue(strconv.Itoa(t.ms)),
					errs.WithExpect("a non-negative number of milliseconds"))
			}
		}
	}

	// Resolve depths by fixpoint so the payload need not be topologically
	// pre-sorted: a span resolves once its parent's depth is known (persisted or
	// resolved earlier in this pass). A pass with no progress means every
	// unresolved span points at a missing or cyclic parent, or descends from one.
	newDepth := make(map[string]int, len(news))
	resolved := make([]bool, len(news))
	for progress := true; progress; {
		progress = false
		for i, s := range news {
			if resolved[i] {
				continue
			}
			var depth int
			p := s.ParentSpanID
			parentDepth, parentResolved := newDepth[p]
			switch {
			case p == "":
				depth = 0
			case p == s.SpanID:
				continue // its own parent: never resolves
			case hasKey(existingDepth, p):
				depth = existingDepth[p] + 1
			case parentResolved:
				depth = parentDepth + 1
			default:
				continue // parent not resolved yet (or unknown)
			}
			if _, seen := newDepth[s.SpanID]; !seen && s.SpanID != "" {
				newDepth[s.SpanID] = depth
			}
			resolved[i], progress = true, true
		}
	}
	// Report each unresolved span that is itself the fault: its own parent, a
	// parent that exists nowhere, or a member of a cycle. A span merely
	// descending from one of those is not reported again.
	for i, s := range news {
		if resolved[i] {
			continue
		}
		p := s.ParentSpanID
		_, inBatch := byID[p]
		switch {
		case p == s.SpanID:
			b.add(i, "parent_span_id", errs.ReasonNotAllowed, ErrUnknownParent, errs.WithValue(p),
				errs.WithExpect("a span cannot be its own parent"))
		case !inBatch:
			b.add(i, "parent_span_id", errs.ReasonNotAllowed, ErrUnknownParent, errs.WithValue(p),
				errs.WithExpect("it names no span in the run or in this batch"))
		case inParentCycle(news, byID, i):
			b.add(i, "parent_span_id", errs.ReasonNotAllowed, ErrUnknownParent, errs.WithValue(p),
				errs.WithExpect("it makes a parent cycle"))
		}
	}
	if err := b.err(); err != nil {
		return nil, fmt.Errorf("trajectory: span batch rejected: %w", err)
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
		// Normalize the produced-artifact handle here, once, so every consumer of
		// a prepared span sees the bare id: the produced-edge lookup, the span
		// projected back to the appending caller, and the span published to live
		// SSE subscribers. Normalizing only at the lookup would leave a live
		// viewer rendering a cross-link to `/mcp://cairn/<id>` until it reloaded.
		s.ProducedArtifactID = normalizeArtifactID(s.ProducedArtifactID)
		out = append(out, preparedSpan{in: s, depth: newDepth[s.SpanID], seq: seq})
	}
	return out, nil
}

func hasKey(m map[string]int, k string) bool {
	_, ok := m[k]
	return ok
}

// spanField is the caller-facing name of a field of the span at index i of a
// batch: spans[i].field, the JSON path of the REST and MCP bodies.
func spanField(i int, field string) string { return fmt.Sprintf("spans[%d].%s", i, field) }

// spanProblems collects a batch's span violations per span, so they read in
// payload order, together with the sentinels they stand for.
type spanProblems struct {
	perSpan [][]*errs.Invalid
	causes  []error
}

func (b *spanProblems) add(i int, field string, r errs.Reason, cause error, opts ...errs.Opt) {
	b.perSpan[i] = append(b.perSpan[i], errs.Violate(spanField(i, field), errs.LocBody, r, opts...))
	if cause != nil && !slices.Contains(b.causes, cause) {
		b.causes = append(b.causes, cause)
	}
}

// err joins every violation (bounded by errs.MaxViolations), or is nil. Each
// sentinel met is kept as a cause, so errors.Is holds for all of them.
func (b *spanProblems) err() error {
	var all []*errs.Invalid
	for _, vs := range b.perSpan {
		all = append(all, vs...)
	}
	inv := errs.Join(all...)
	if inv == nil {
		return nil
	}
	return inv.Because(errors.Join(b.causes...))
}

// inParentCycle reports whether following parent links from the span at index
// i, through spans of the same batch, leads back to it.
func inParentCycle(news []SpanInput, byID map[string]int, i int) bool {
	cur := news[i].ParentSpanID
	for range news {
		j, ok := byID[cur]
		if !ok {
			return false
		}
		if cur = news[j].ParentSpanID; cur == news[i].SpanID {
			return true
		}
	}
	return false
}

// normalizeArtifactID resolves a bare public id or an mcp://cairn/<id> agent
// handle (optionally under a /run/ or /hook/ sub-prefix a caller may copy-paste)
// to the bare public id the produced-edge lookup takes, so a produced artifact
// id supplied as a handle resolves the same as a bare id (issue #50). It is the
// same [id.Normalize] the transport adapters use, not a second copy of the
// rule, so core and transport cannot drift (ADR-0005: "one id, every surface").
func normalizeArtifactID(raw string) string { return id.Normalize(raw) }

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
		TimeByCategoryMS: make(map[Category]int64, len(RecommendedCategories)),
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
