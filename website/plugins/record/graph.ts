// Governing: ADR-0014, SPEC-0010 REQ "Derived Cross-Reference Graph"
// Governing: ADR-0014, SPEC-0010 REQ "Derived Data Module"
//
// The cross-reference graph, in both directions, from forward-only sources.
//
// The record's authoring convention is forward-only — the newer ADRs say so in a
// front-matter comment and the older ones follow it silently — so readers only
// get "extended by ADR-0002" if the build computes it.
//
// The de-duplication is not a nicety. `ADR-0001` declares
// `enables: [ADR-0002, ADR-0003]` while `ADR-0002` and `ADR-0003` each declare
// `extends: [ADR-0001]`, so that relationship is *already* authored from both
// ends. A naive inversion makes ADR-0001 list ADR-0002 twice. The fix is to
// notice that `A enables B` and `B extends A` are the same fact and to store it
// once, oriented canonically.

import type {
  AuthoredEdgeKind,
  EdgeKind,
  ParsedRecord,
  RecordEdge,
  RecordRelations,
} from './types.ts';

/** An authored edge as it appears in front-matter, before normalisation. */
export interface AuthoredEdge {
  /** The record whose front-matter declared it. */
  owner: string;
  kind: AuthoredEdgeKind;
  /** The record it names. */
  target: string;
}

export function emptyRelations(): RecordRelations {
  return {
    extends: [],
    extendedBy: [],
    related: [],
    implements: [],
    implementedBy: [],
    requires: [],
    requiredBy: [],
  };
}

/** Flatten every front-matter graph edge in a parsed record. */
export function authoredEdges(record: ParsedRecord): AuthoredEdge[] {
  const out: AuthoredEdge[] = [];
  for (const [kind, targets] of Object.entries(record.authored)) {
    for (const target of targets ?? []) {
      out.push({owner: record.id, kind: kind as AuthoredEdgeKind, target});
    }
  }
  return out;
}

/**
 * Put one authored edge into canonical `{kind, from, to}` form.
 *
 * `enables` is deliberately absent from `EdgeKind`: `A enables B` *is*
 * `B extends A`, and collapsing the two here is the entire mechanism by which
 * the same relationship authored from both ends becomes one chip.
 * `related` is symmetric, so its endpoints are sorted.
 */
export function normaliseEdge(edge: AuthoredEdge): RecordEdge {
  switch (edge.kind) {
    case 'extends':
      return {kind: 'extends', from: edge.owner, to: edge.target};
    case 'enables':
      return {kind: 'extends', from: edge.target, to: edge.owner};
    case 'implements':
      return {kind: 'implements', from: edge.owner, to: edge.target};
    case 'requires':
      return {kind: 'requires', from: edge.owner, to: edge.target};
    case 'related': {
      const [from, to] =
        edge.owner <= edge.target
          ? [edge.owner, edge.target]
          : [edge.target, edge.owner];
      return {kind: 'related', from, to};
    }
    default: {
      const exhaustive: never = edge.kind;
      throw new Error(`Unknown edge kind: ${String(exhaustive)}`);
    }
  }
}

function edgeKey(edge: RecordEdge): string {
  return `${edge.kind}|${edge.from}|${edge.to}`;
}

/**
 * Normalise and de-duplicate every authored edge in the record.
 *
 * A self-edge is dropped rather than rendered: a record that names itself would
 * produce a chip linking to the page the reader is already on.
 */
export function buildEdges(records: ParsedRecord[]): RecordEdge[] {
  const seen = new Map<string, RecordEdge>();
  for (const record of records) {
    for (const authored of authoredEdges(record)) {
      const edge = normaliseEdge(authored);
      if (edge.from === edge.to) {
        continue;
      }
      const key = edgeKey(edge);
      if (!seen.has(key)) {
        seen.set(key, edge);
      }
    }
  }
  return [...seen.values()].sort(
    (a, b) =>
      a.kind.localeCompare(b.kind) ||
      a.from.localeCompare(b.from) ||
      a.to.localeCompare(b.to),
  );
}

const INVERSE_OF: Record<EdgeKind, keyof RecordRelations> = {
  extends: 'extendedBy',
  implements: 'implementedBy',
  requires: 'requiredBy',
  related: 'related',
};

/**
 * Project the normalised edge set onto each record, so a detail page can render
 * its own chips without walking the graph.
 *
 * Both directions appear. `related` lands in the same bucket from either end,
 * because it is symmetric.
 */
export function buildRelations(
  ids: string[],
  edges: RecordEdge[],
): Record<string, RecordRelations> {
  const relations: Record<string, RecordRelations> = {};
  for (const id of ids) {
    relations[id] = emptyRelations();
  }
  const ensure = (id: string): RecordRelations =>
    (relations[id] ??= emptyRelations());

  for (const edge of edges) {
    if (edge.kind === 'related') {
      push(ensure(edge.from).related, edge.to);
      push(ensure(edge.to).related, edge.from);
      continue;
    }
    push(ensure(edge.from)[edge.kind], edge.to);
    push(ensure(edge.to)[INVERSE_OF[edge.kind]], edge.from);
  }

  for (const relation of Object.values(relations)) {
    for (const key of Object.keys(relation) as (keyof RecordRelations)[]) {
      relation[key].sort();
    }
  }
  return relations;
}

function push(list: string[], id: string): void {
  if (!list.includes(id)) {
    list.push(id);
  }
}
