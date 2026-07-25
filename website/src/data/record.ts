// Governing: ADR-0014, SPEC-0010 REQ "Derived Data Module"
//
// The one accessor for the derived data module. Every generated index, badge,
// count and chip on the site reads from here, and nothing on the site restates a
// fact the record owns.
//
// `src/generated/record.json` is written by the pipeline and is git-ignored, so
// this import resolves only after the generator has run. That is guaranteed for
// `docusaurus build` and `docusaurus start` (both await the generator from the
// config factory) and is what `npm run generate` exists for elsewhere.

import generated from '@site/src/generated/record.json';

import {emptyRelations} from '@site/plugins/record/graph';
import type {
  DecisionEntry,
  RecordData,
  RecordRef,
  RecordRelations,
  SpecEntry,
} from '@site/plugins/record/types';

export type {DecisionEntry, RecordData, RecordRef, RecordRelations, SpecEntry};

export const record = generated as unknown as RecordData;

export const decisions: DecisionEntry[] = record.decisions;
export const specs: SpecEntry[] = record.specs;
export const counts = record.counts;

/** Both directions of every relationship touching one record. */
export function relationsFor(id: string): RecordRelations {
  return record.graph.relations[id] ?? emptyRelations();
}

/** The title and route needed to render a chip for a record id. */
export function refFor(id: string): RecordRef {
  return record.refs[id] ?? {id, title: id, href: '#'};
}

export default record;
