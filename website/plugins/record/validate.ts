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

import fs from 'node:fs/promises';
import path from 'node:path';

import {authoredEdges} from './graph.ts';
import {RecordError} from './parse.ts';
import {repoRelative, type RecordPaths} from './paths.ts';
import type {AuthoredEdgeKind, ParsedRecord, RecordData} from './types.ts';

/** Which kind of record each authored edge is allowed to name. */
export function edgeTargetKind(kind: AuthoredEdgeKind): 'ADR' | 'SPEC' {
  return kind === 'requires' ? 'SPEC' : 'ADR';
}

export interface ValidationInput {
  decisions: ParsedRecord[];
  specs: ParsedRecord[];
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

export interface CoverageInput {
  paths: RecordPaths;
  /** Absolute path of every `docs/adrs/ADR-*.md` found on disk. */
  adrFiles: string[];
  /** Absolute path of every capability directory found on disk. */
  capabilityDirs: string[];
  data: RecordData;
}

/**
 * Coverage, as ADR-0014's Confirmation section defines it: a source file that
 * matches the pattern but produces no page fails the build.
 *
 * This runs *after* staging and stats the staged file, on purpose. Comparing the
 * source inventory against the in-memory record list would compare a list against
 * a mapping over itself and could never fire — the check would read as
 * load-bearing while being a no-op. Ending at the filesystem makes it a real
 * check: it catches a record that parsed and was counted but never reached disk,
 * whether because staging skipped it, two records collided on one output path, or
 * the stale-file prune removed it.
 */
export async function assertStagedCoverage({
  paths,
  adrFiles,
  capabilityDirs,
  data,
}: CoverageInput): Promise<void> {
  for (const absPath of adrFiles) {
    const sourcePath = repoRelative(paths, absPath);
    const decision = data.decisions.find(
      (entry) => entry.sourcePath === sourcePath,
    );
    if (!decision) {
      throw new RecordError(
        sourcePath,
        `matched the ADR-*.md decision pattern but produced no page`,
      );
    }
    await assertStagedFile(
      sourcePath,
      path.join(paths.stagedDecisionsDir, `${decision.id}.md`),
    );
  }

  for (const dir of capabilityDirs) {
    const sourcePath = repoRelative(paths, dir);
    const capability = path.basename(dir);
    const spec = data.specs.find((entry) => entry.capability === capability);
    if (!spec) {
      throw new RecordError(
        sourcePath,
        `matched the capability specification layout but produced no card`,
      );
    }
    await assertStagedFile(
      sourcePath,
      path.join(paths.stagedSpecsDir, capability, 'index.md'),
    );
  }
}

async function assertStagedFile(
  sourcePath: string,
  stagedPath: string,
): Promise<void> {
  try {
    const stat = await fs.stat(stagedPath);
    if (stat.isFile() && stat.size > 0) {
      return;
    }
  } catch {
    // fall through to the failure below
  }
  throw new RecordError(
    sourcePath,
    `produced no page: nothing was staged at ${stagedPath}`,
  );
}
