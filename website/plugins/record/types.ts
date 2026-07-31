// Governing: ADR-0014, SPEC-0010 REQ "Derived Data Module"
//
// The shape of the single derived data module. Every generated index, badge,
// count and chip rendered inside a page reads from an object of this shape;
// nothing restates a fact the record owns.

/** MADR statuses the validator accepts for an ADR. */
export const ADR_STATUSES = [
  'draft',
  'proposed',
  'rejected',
  'accepted',
  'deprecated',
  'superseded',
] as const;

export type RecordStatus = (typeof ADR_STATUSES)[number];

/** The five forward-only edge kinds authors write in front-matter. */
export type AuthoredEdgeKind =
  | 'extends'
  | 'enables'
  | 'related'
  | 'implements'
  | 'requires';

/**
 * The four *relationship* kinds that survive normalisation. `enables` is not
 * one of them: `A enables B` is the same relationship as `B extends A`, and
 * collapsing the two is what makes de-duplication possible at all.
 */
export type EdgeKind = 'extends' | 'related' | 'implements' | 'requires';

/**
 * A normalised edge. Orientation is canonical per kind:
 *  - `extends`    from = the extending record,   to = the extended record
 *  - `implements` from = the specification,      to = the decision
 *  - `requires`   from = the requiring spec,     to = the required spec
 *  - `related`    symmetric; `from` < `to` lexicographically
 */
export interface RecordEdge {
  kind: EdgeKind;
  from: string;
  to: string;
}

/**
 * Both directions of every relationship touching one record.
 *
 * SPEC-0010 REQ "Derived Cross-Reference Graph" names five chip kinds —
 * `extends`, `enables`, `related`, `implements`, `requires` — and the same
 * requirement mandates de-duplication, which is only possible if `A enables B`
 * and `B extends A` collapse into one stored fact. So `enables` has no bucket
 * here, and the mapping a chip renderer needs is:
 *
 *   authored `extends`    → `extends`      · inverse rendered from `extendedBy`
 *   authored `enables`    → `extendedBy`   · inverse rendered from `extends`
 *   authored `related`    → `related`      · symmetric, one bucket
 *   authored `implements` → `implements`   · inverse rendered from `implementedBy`
 *   authored `requires`   → `requires`     · inverse rendered from `requiredBy`
 *
 * The consequence, stated so it is a decision rather than a discovery: which end
 * *authored* an extends/enables relationship is not recoverable from this shape.
 * A chip renderer therefore labels by direction ("extends" / "extended by"), not
 * by the front-matter key the author happened to use.
 */
export interface RecordRelations {
  extends: string[];
  extendedBy: string[];
  related: string[];
  implements: string[];
  implementedBy: string[];
  requires: string[];
  requiredBy: string[];
}

export interface RecordRef {
  /** `ADR-0009` / `SPEC-0004`. */
  id: string;
  title: string;
  /** Site-relative route, e.g. `/docs/decisions/ADR-0009`. */
  href: string;
}

export interface DecisionEntry {
  id: string;
  number: number;
  title: string;
  status: RecordStatus;
  /** ISO `YYYY-MM-DD`. */
  date: string;
  /** Derived from the transformed document, never from the raw source. */
  summary: string;
  /** Docusaurus doc id, e.g. `decisions/ADR-0009`. */
  docId: string;
  href: string;
  /** Repo-relative source path, for the coverage check's error messages. */
  sourcePath: string;
}

export interface RequirementEntry {
  name: string;
  /** Heading anchor the site generates for this requirement. */
  anchor: string;
  scenarioCount: number;
}

export interface SpecEntry {
  id: string;
  number: number;
  title: string;
  /** Directory name under `docs/openspec/specs/`. */
  capability: string;
  status: RecordStatus;
  date: string;
  summary: string;
  requirementCount: number;
  scenarioCount: number;
  requirements: RequirementEntry[];
  docId: string;
  href: string;
  /** Present whenever the capability ships a paired `design.md`. */
  designHref: string | null;
  designDocId: string | null;
  sourcePath: string;
}

export interface RecordCounts {
  decisions: number;
  specifications: number;
  requirements: number;
  scenarios: number;
}

// Governing: ADR-0014, SPEC-0010 REQ "Derived HTTP Reference Page"
//
// The API surface, as read out of the specifications' own endpoint tables. See
// `endpoints.ts` for why the sections are enumerated and the columns are
// identified by header rather than by position.

/** One endpoint table row, before it knows which specification it came from. */
export interface RawEndpointRow {
  /** `GET`, `POST / GET (SSE)`, or null where the source states no method. */
  method: string | null;
  path: string;
  /** A trailing parenthetical the author wrote beside the path: `(web)`. */
  note: string | null;
  purpose: string;
  /** The auth cell verbatim: posture and the justification the record gave. */
  auth: string | null;
  /** The posture alone — the half a reader scans a column for. */
  authPosture: string | null;
  /** Derived, never listed: an SSE method, purpose, or a `/stream` path. */
  streaming: boolean;
}

/** One enumerated endpoint section of one specification, and its rows. */
export interface EndpointSection {
  /** The enumerated name matched: one of `ENDPOINT_SECTION_NAMES`. */
  name: string;
  /**
   * The heading exactly as the record wrote it, which is not always the
   * canonical spelling — `trajectory-share` writes `### HTTP endpoints`. Pages
   * quote this, so a reader searching the specification finds the heading.
   */
  title: string;
  /** The anchor Docusaurus will emit for that heading. */
  anchor: string;
  rows: RawEndpointRow[];
}

/** One contributing section of one specification, as a page can link it. */
export interface EndpointSectionRef {
  name: string;
  title: string;
  /** Deep link to that heading, carried per section rather than re-derived. */
  href: string;
}

/** A row on the reference page: what it says, and which spec owns it. */
export interface EndpointRow extends RawEndpointRow {
  specId: string;
  specTitle: string;
  specHref: string;
  section: string;
  /** Deep link to the owning table, not merely to the specification. */
  sectionHref: string;
}

/**
 * Every specification, contributing or not. A specification that carries none of
 * the enumerated sections is not an error — it is the reason the page may not
 * claim to be a complete API surface, so it has to be nameable.
 */
export interface EndpointSpecCoverage {
  specId: string;
  specTitle: string;
  specHref: string;
  sections: EndpointSectionRef[];
  /**
   * Enumerated sections deliberately left OUT of the reference — today, the
   * website specification's own `## Web Routes`, which maps a static site rather
   * than a service API. Kept distinct from "carries no endpoint section":
   * collapsing the two into `rowCount === 0` made the page state the opposite of
   * what the record says.
   */
  excluded: EndpointSectionRef[];
  rowCount: number;
}

export interface RecordEndpoints {
  /** The enumerated section names scanned, so the page can state its own scope. */
  sectionNames: string[];
  rows: EndpointRow[];
  coverage: EndpointSpecCoverage[];
}

// Governing: ADR-0014, SPEC-0010 REQ "Derived Design-Language Page"

/**
 * One share-type badge, and the records that declare it. The record fixes the
 * codes; it assigns them no colour, so nothing here carries one.
 */
export interface BadgeEntry {
  /** The short code the shell renders: `MD`, `RUN`. */
  code: string;
  /** Every record declaring it, in record order, so the page can cite it. */
  sources: RecordRef[];
}

export interface RecordData {
  decisions: DecisionEntry[];
  specs: SpecEntry[];
  counts: RecordCounts;
  /** The share-type badge set, derived from the record's own declarations. */
  badges: BadgeEntry[];
  /** The service's HTTP surface, derived from the specs' endpoint tables. */
  endpoints: RecordEndpoints;
  graph: {
    edges: RecordEdge[];
    /** Keyed by record id; every id in `decisions`/`specs` has an entry. */
    relations: Record<string, RecordRelations>;
  };
  /** Every record id → the ref needed to render a chip for it. */
  refs: Record<string, RecordRef>;
}

/** A parsed record source file, before staging. */
export interface ParsedRecord {
  id: string;
  number: number;
  title: string;
  status: RecordStatus;
  date: string;
  summary: string;
  /** Raw front-matter graph edges, exactly as authored. */
  authored: Partial<Record<AuthoredEdgeKind, string[]>>;
  requirements: RequirementEntry[];
  scenarioCount: number;
  /**
   * The enumerated endpoint sections this file carries, in document order.
   * Collected for every record; which of them reach the reference page is a
   * generator policy, not a parsing one — see `buildEndpointReference`.
   */
  endpointSections: EndpointSection[];
  /**
   * Share-type badge codes this file declares, in document order. Collected for
   * every record; ADR-0002 declares the whole set, and the specifications and
   * ADR-0009/ADR-0010 declare theirs individually.
   */
  badgeCodes: string[];
  /** Absolute path of the source file. */
  absPath: string;
  /** Repo-relative source path. */
  sourcePath: string;
  /** The record body, verbatim, with front-matter removed. */
  body: string;
}
