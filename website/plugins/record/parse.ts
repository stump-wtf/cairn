// Governing: ADR-0014, SPEC-0010 REQ "Derived Data Module"
// Governing: ADR-0014, SPEC-0010 REQ "CommonMark Fidelity"
//
// Reading the record. Nothing in here writes anything: it turns one source file
// into a `ParsedRecord`, and every field of that record is derived from the file
// rather than restated anywhere.

/* eslint-disable @typescript-eslint/no-explicit-any */

import fs from 'node:fs/promises';
import path from 'node:path';

import {
  collectInventory,
  deriveSummary,
  findTitleHeading,
  headingText,
  literaliseRawHtml,
  parseRecordMarkdown,
} from './mdast.ts';
import {collectBadgeCodes} from './badges.ts';
import {collectEndpointSections} from './endpoints.ts';
import type {AuthoredEdgeKind, ParsedRecord, RecordStatus} from './types.ts';
import {ADR_STATUSES} from './types.ts';
import {repoRelative, type RecordPaths} from './paths.ts';

export const AUTHORED_EDGE_KINDS: readonly AuthoredEdgeKind[] = [
  'extends',
  'enables',
  'related',
  'implements',
  'requires',
];

/** Thrown for anything wrong with the record. Always names the file. */
export class RecordError extends Error {
  constructor(sourcePath: string, message: string) {
    super(`${sourcePath}: ${message}`);
    this.name = 'RecordError';
  }
}

const ADR_ID = /^ADR-(\d{4})$/;
const SPEC_ID = /^SPEC-(\d{4})$/;
const ADR_HEADING = /^(ADR-\d{4})\s*:\s*(.+)$/;
const SPEC_HEADING = /^(SPEC-\d{4})\s*:\s*(.+)$/;
const ISO_DATE = /^\d{4}-\d{2}-\d{2}$/;

/** Files matching this are decisions. Anything else in `docs/adrs/` is not. */
export const ADR_FILENAME = /^ADR-\d{4}.*\.md$/;

export function isAdrId(id: string): boolean {
  return ADR_ID.test(id);
}

export function isSpecId(id: string): boolean {
  return SPEC_ID.test(id);
}

export function idNumber(id: string): number {
  const match = ADR_ID.exec(id) ?? SPEC_ID.exec(id);
  if (!match) {
    throw new Error(`Not a record identifier: ${id}`);
  }
  return Number(match[1]);
}

/**
 * Split front-matter off a record file without a YAML dependency of its own.
 * The record's front-matter carries `#` comments ("Do NOT author inverse
 * edges") which a naive line parser would take for data, so the block is handed
 * to a real YAML parser — but the *body* is returned byte-for-byte, because the
 * staged output must be the record's own CommonMark and nothing else.
 */
export function splitFrontMatter(raw: string): {
  frontMatter: string;
  body: string;
} {
  const normalised = raw.startsWith('﻿') ? raw.slice(1) : raw;
  if (!normalised.startsWith('---')) {
    return {frontMatter: '', body: normalised};
  }
  const end = normalised.indexOf('\n---', 3);
  if (end === -1) {
    return {frontMatter: '', body: normalised};
  }
  const afterFence = normalised.indexOf('\n', end + 1);
  return {
    frontMatter: normalised.slice(normalised.indexOf('\n') + 1, end + 1),
    body: afterFence === -1 ? '' : normalised.slice(afterFence + 1),
  };
}

async function loadYaml(source: string): Promise<Record<string, unknown>> {
  if (source.trim() === '') {
    return {};
  }
  const yaml = await import('js-yaml');
  const parsed = (yaml.load ?? (yaml as any).default.load)(source);
  return (parsed ?? {}) as Record<string, unknown>;
}

/**
 * Normalise a front-matter date to `YYYY-MM-DD`.
 *
 * YAML resolves an unquoted `2026-07-08` to a `Date`, and a quoted one to a
 * string, and the record contains both spellings over time. Both are accepted;
 * anything else is a validation failure that names the field.
 */
export function normaliseDate(value: unknown): string | null {
  if (value instanceof Date && !Number.isNaN(value.getTime())) {
    return value.toISOString().slice(0, 10);
  }
  if (typeof value === 'string' && ISO_DATE.test(value.trim())) {
    return value.trim();
  }
  return null;
}

export function normaliseStatus(value: unknown): RecordStatus | null {
  if (typeof value !== 'string') {
    return null;
  }
  const lower = value.trim().toLowerCase();
  return (ADR_STATUSES as readonly string[]).includes(lower)
    ? (lower as RecordStatus)
    : null;
}

function readIdList(
  sourcePath: string,
  key: string,
  value: unknown,
): string[] | undefined {
  if (value === undefined || value === null) {
    return undefined;
  }
  if (!Array.isArray(value) || value.some((item) => typeof item !== 'string')) {
    throw new RecordError(
      sourcePath,
      `front-matter '${key}' must be a list of record identifiers`,
    );
  }
  return (value as string[]).map((item) => item.trim());
}

interface ParseOptions {
  paths: RecordPaths;
  absPath: string;
  /** `ADR` for decisions, `SPEC` for specifications. */
  kind: 'ADR' | 'SPEC';
}

/**
 * Parse one record source file.
 *
 * The summary, the requirement anchors and the counts all come from the
 * *transformed* tree — raw HTML is literalised first — so a tag-shaped word like
 * `<id>` can neither truncate a description nor shift a heading anchor.
 */
export async function parseRecordFile({
  paths,
  absPath,
  kind,
}: ParseOptions): Promise<ParsedRecord> {
  const sourcePath = repoRelative(paths, absPath);
  const raw = await fs.readFile(absPath, 'utf8');
  const {frontMatter, body} = splitFrontMatter(raw);

  let data: Record<string, unknown>;
  try {
    data = await loadYaml(frontMatter);
  } catch (error) {
    throw new RecordError(
      sourcePath,
      `front-matter is not parseable YAML: ${(error as Error).message}`,
    );
  }

  const tree = await parseRecordMarkdown(body);
  literaliseRawHtml(tree);

  const heading = findTitleHeading(tree);
  if (!heading) {
    throw new RecordError(
      sourcePath,
      `no '# ${kind}-XXXX: …' heading found; the identifier and title are read from it`,
    );
  }
  const text = await headingText(heading);
  const match = (kind === 'ADR' ? ADR_HEADING : SPEC_HEADING).exec(text);
  if (!match) {
    throw new RecordError(
      sourcePath,
      `title heading '${text}' does not match '# ${kind}-XXXX: <title>'`,
    );
  }
  const id = match[1]!;
  const title = match[2]!.trim();

  const status = normaliseStatus(data.status);
  if (status === null) {
    throw new RecordError(
      sourcePath,
      data.status === undefined
        ? `missing front-matter field 'status'`
        : `front-matter 'status' is '${String(data.status)}', which is not one of: ${ADR_STATUSES.join(', ')}`,
    );
  }

  const date = normaliseDate(data.date);
  if (date === null) {
    throw new RecordError(
      sourcePath,
      data.date === undefined
        ? `missing front-matter field 'date'`
        : `front-matter 'date' is '${String(data.date)}', which is not a parseable YYYY-MM-DD date`,
    );
  }

  const authored: Partial<Record<AuthoredEdgeKind, string[]>> = {};
  for (const edgeKind of AUTHORED_EDGE_KINDS) {
    const list = readIdList(sourcePath, edgeKind, data[edgeKind]);
    if (list) {
      authored[edgeKind] = list;
    }
  }

  const {requirements, scenarioCount, explicitIds} =
    await collectInventory(tree);
  // The requirement anchors in the derived data module are computed by mirroring
  // Docusaurus's slugger. That mirror does not implement the explicit-id
  // syntaxes, so a heading that uses one would be published under an anchor
  // nothing links to. Refusing it is loud; mirroring it silently would not be.
  if (explicitIds.length > 0) {
    throw new RecordError(
      sourcePath,
      `heading '${explicitIds[0]}' sets an explicit anchor with '{#…}'; the record pipeline derives every anchor from the heading text, so remove it`,
    );
  }
  const summary = await deriveSummary(tree);
  // Read from the same transformed tree the anchors came from, so an endpoint
  // section's deep link and the heading Docusaurus emits cannot disagree.
  const endpointSections = await collectEndpointSections(tree);
  // Governing: ADR-0014, SPEC-0010 REQ "Derived Design-Language Page"
  const badgeCodes = await collectBadgeCodes(tree);

  return {
    id,
    number: idNumber(id),
    title,
    status,
    date,
    summary,
    authored,
    requirements,
    scenarioCount,
    endpointSections,
    badgeCodes,
    absPath,
    sourcePath,
    body,
  };
}

/**
 * A capability's paired design document. It carries no front-matter of its own,
 * so only its title and summary are derived.
 */
export interface ParsedDesign {
  title: string;
  summary: string;
  absPath: string;
  sourcePath: string;
  body: string;
}

export async function parseDesignFile(
  paths: RecordPaths,
  absPath: string,
): Promise<ParsedDesign> {
  const sourcePath = repoRelative(paths, absPath);
  const raw = await fs.readFile(absPath, 'utf8');
  const {body} = splitFrontMatter(raw);
  const tree = await parseRecordMarkdown(body);
  literaliseRawHtml(tree);
  const heading = findTitleHeading(tree);
  const title = heading ? await headingText(heading) : 'Design';
  return {
    title,
    summary: await deriveSummary(tree),
    absPath,
    sourcePath,
    body,
  };
}

/** Every `docs/adrs/ADR-*.md`, sorted by filename. */
export async function listAdrFiles(paths: RecordPaths): Promise<string[]> {
  const entries = await fs.readdir(paths.adrDir, {withFileTypes: true});
  return entries
    .filter((entry) => entry.isFile() && ADR_FILENAME.test(entry.name))
    .map((entry) => path.join(paths.adrDir, entry.name))
    .sort();
}

/** Every capability directory under `docs/openspec/specs/`, sorted by name. */
export async function listCapabilityDirs(
  paths: RecordPaths,
): Promise<string[]> {
  const entries = await fs.readdir(paths.specsDir, {withFileTypes: true});
  return entries
    .filter((entry) => entry.isDirectory())
    .map((entry) => path.join(paths.specsDir, entry.name))
    .sort();
}
