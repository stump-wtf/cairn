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

/** Both directions of every relationship touching one record. */
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

export interface RecordData {
  decisions: DecisionEntry[];
  specs: SpecEntry[];
  counts: RecordCounts;
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
  /** Absolute path of the source file. */
  absPath: string;
  /** Repo-relative source path. */
  sourcePath: string;
  /** The record body, verbatim, with front-matter removed. */
  body: string;
}
