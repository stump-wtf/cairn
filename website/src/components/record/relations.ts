// Governing: ADR-0014, SPEC-0010 REQ "Derived Cross-Reference Graph"
//
// Turning one record's relation buckets into the chip groups a metadata bar
// renders. Kept out of the `.tsx` beside it on purpose: this is the part with
// decisions in it — which buckets render, in what order, under what label, and
// which of them point *at* the page rather than away from it — and it is worth
// testing without a React renderer or a `@site` alias in the way.
//
// The bucket names come from the pipeline (`plugins/record/graph.ts`), which has
// already normalised and de-duplicated the edges. Nothing here re-derives a
// relationship; it only decides how one is presented.

import type {RecordRelations} from '@site/plugins/record/types';

/**
 * Which way the relationship points relative to the page being read.
 *
 * `outbound` is what this record says about others; `inbound` is what others say
 * about it — the direction the record's forward-only authoring convention means
 * nobody writes by hand, and which the pipeline computes.
 *
 * `mutual` is the third case and it is not a hedge. `related` is a *symmetric*
 * edge: `graph.ts` normalises its endpoints into sorted order and pushes each
 * record into the other's `related` bucket, so a record's bucket mixes the ids
 * it named with the ids that named it and the two are indistinguishable
 * afterwards. `ADR-0009` authors `related: [ADR-0006, ADR-0008]` and `ADR-0014`
 * authors `related: [ADR-0009, …]`, which puts `ADR-0014` in ADR-0009's bucket
 * from the far end. Calling that group `outbound` would claim a direction the
 * edge does not have and would hand every one of those chips the cue that marks
 * an author-written edge, so it gets its own value instead.
 *
 * Deliberately *not* called "authored" and "derived". A record's `extendedBy`
 * bucket can be filled either by another record's `extends:` or by this record's
 * own `enables:`, and normalisation collapses the two, so which end typed it is
 * not recoverable — see the note on `RecordRelations`. Direction is recoverable,
 * so direction is what the reader is told.
 */
export type ChipDirection = 'outbound' | 'inbound' | 'mutual';

export interface ChipGroup {
  /** The relation bucket, also the group's React key and its data attribute. */
  kind: keyof RecordRelations;
  /** Human-readable, and the only thing carrying the meaning if colour is gone. */
  label: string;
  direction: ChipDirection;
  ids: string[];
}

/**
 * Presentation order. Outbound first within each pairing, so a reader sees what
 * the record claims before what claims it, and `related` last because a
 * symmetric edge has no direction to explain.
 */
const GROUPS: {kind: keyof RecordRelations; label: string; direction: ChipDirection}[] = [
  {kind: 'extends', label: 'Extends', direction: 'outbound'},
  {kind: 'extendedBy', label: 'Extended by', direction: 'inbound'},
  {kind: 'implements', label: 'Implements', direction: 'outbound'},
  {kind: 'implementedBy', label: 'Implemented by', direction: 'inbound'},
  {kind: 'requires', label: 'Requires', direction: 'outbound'},
  {kind: 'requiredBy', label: 'Required by', direction: 'inbound'},
  {kind: 'related', label: 'Related', direction: 'mutual'},
];

/**
 * The non-empty chip groups for one record, in presentation order.
 *
 * Empty buckets are dropped rather than rendered as an empty row: a record with
 * no graph edges renders no graph at all, and a record with one renders exactly
 * one label.
 */
export function chipGroups(relations: RecordRelations): ChipGroup[] {
  const out: ChipGroup[] = [];
  for (const group of GROUPS) {
    const ids = relations[group.kind] ?? [];
    if (ids.length > 0) {
      out.push({...group, ids});
    }
  }
  return out;
}
