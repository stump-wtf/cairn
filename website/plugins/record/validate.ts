// Governing: ADR-0014, SPEC-0010 REQ "Referential Integrity and Front-Matter Validation"
//
// Validation lives inside the pipeline, not beside it, so that a broken record
// fails `docusaurus start` as well as `docusaurus build`. The generator is
// awaited from the config factory; if this throws, Docusaurus never finishes
// loading the site config and the author sees the failure in the dev server.
//
// Front-matter validity (status enum, parseable date, required fields) is
// enforced in `parse.ts`, at the point each field is read. What is left here is
// everything that needs the whole record in hand: referential integrity and
// coverage.

import {authoredEdges} from './graph.ts';
import {RecordError} from './parse.ts';
import type {AuthoredEdgeKind, ParsedRecord} from './types.ts';

/** Which kind of record each authored edge is allowed to name. */
export function edgeTargetKind(kind: AuthoredEdgeKind): 'ADR' | 'SPEC' {
  return kind === 'requires' ? 'SPEC' : 'ADR';
}

export interface ValidationInput {
  decisions: ParsedRecord[];
  specs: ParsedRecord[];
  /**
   * Every `docs/adrs/ADR-*.md` found on disk, repo-relative — the coverage
   * denominator for decisions.
   */
  adrSourcePaths: string[];
  /**
   * Every capability directory found on disk, repo-relative — the coverage
   * denominator for specifications. Keyed on the directory rather than on
   * `spec.md`, because the requirement is one card per directory.
   */
  capabilityDirs: string[];
  /** Directory of each specification that did produce a page, repo-relative. */
  renderedCapabilityDirs: string[];
}

/**
 * Fail on a broken record rather than degrade.
 *
 * Every check names the referring file and the unresolved identifier, because
 * the author who breaks this may have no idea the website exists.
 */
export function validateRecord(input: ValidationInput): void {
  const {decisions, specs} = input;

  assertUniqueIds(decisions, 'decision');
  assertUniqueIds(specs, 'specification');

  const adrIds = new Set(decisions.map((record) => record.id));
  const specIds = new Set(specs.map((record) => record.id));

  for (const record of [...decisions, ...specs]) {
    for (const edge of authoredEdges(record)) {
      const expected = edgeTargetKind(edge.kind);
      const known = expected === 'ADR' ? adrIds : specIds;
      if (!known.has(edge.target)) {
        throw new RecordError(
          record.sourcePath,
          `front-matter '${edge.kind}' names '${edge.target}', which resolves to no ${
            expected === 'ADR' ? 'decision record' : 'capability specification'
          }`,
        );
      }
    }
  }

  // Every specification must declare what it implements. Decisions have no
  // such requirement: a root decision extends nothing.
  for (const record of specs) {
    if (!record.authored.implements?.length) {
      throw new RecordError(
        record.sourcePath,
        `missing front-matter field 'implements'; every specification must name the decision it realises`,
      );
    }
  }

  // Coverage. A source file that matches the pattern but produced no page is a
  // build failure, because a silently missing decision is exactly the defect
  // ADR-0014 exists to abolish.
  assertCovered(
    input.adrSourcePaths,
    decisions.map((record) => record.sourcePath),
    'ADR-*.md decision pattern',
  );
  assertCovered(
    input.capabilityDirs,
    input.renderedCapabilityDirs,
    'capability specification layout',
  );
}

function assertUniqueIds(records: ParsedRecord[], label: string): void {
  const seen = new Map<string, string>();
  for (const record of records) {
    const previous = seen.get(record.id);
    if (previous) {
      throw new RecordError(
        record.sourcePath,
        `${label} identifier '${record.id}' is already used by ${previous}`,
      );
    }
    seen.set(record.id, record.sourcePath);
  }
}

function assertCovered(found: string[], rendered: string[], label: string): void {
  const renderedSet = new Set(rendered);
  for (const sourcePath of found) {
    if (!renderedSet.has(sourcePath)) {
      throw new RecordError(
        sourcePath,
        `matched the ${label} but produced no page`,
      );
    }
  }
}
