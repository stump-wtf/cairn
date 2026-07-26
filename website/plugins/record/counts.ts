// Governing: ADR-0014, SPEC-0010 REQ "Referential Integrity and Front-Matter Validation"
// Governing: ADR-0014, SPEC-0010 REQ "Derived Status, Dates, and Counts"
//
// The hardcoded-count gate.
//
// ADR-0014 exists because `website/docs/specifications.md` stated a requirement
// total and a scenario total in prose and both were wrong. Deleting that page
// removes the instance; it does not remove the failure mode, because nothing
// stops the next author writing "14 decisions" into the homepage. SPEC-0010 REQ
// "Derived Status, Dates, and Counts" is explicit: "No count of decisions,
// specifications, requirements, or scenarios MAY be written as a literal
// anywhere in site source."
//
// WHY THIS IS AN AST WALK AND NOT A GREP
// --------------------------------------
// A grep cannot do this job, in both directions.
//
// It cannot decide that a number IS a count. `14` is the decision total today;
// it is also a z-index, a viewBox coordinate, a millisecond duration and an
// array length. A checker keyed on the VALUE fires on all of them, and the first
// false positive gets it deleted.
//
// And it cannot see a count that a grep-visible line does not contain. Markup
// splits the claim:
//
//     <div className={styles.stat}>
//       <strong>14</strong>
//       <span>decisions</span>
//     </div>
//
// No line here matches `\d+\s+decisions`. A reader sees "14 decisions".
//
// So the check is keyed on the SHAPE of the claim rather than on the value: a
// number standing next to a noun the record owns. It is evaluated over the
// *rendered text* of each JSX subtree — children stitched together in source
// order, with element boundaries becoming a space — so intervening markup does
// not hide the adjacency, and over every string and template literal, so a claim
// assembled in an attribute or in the site metadata is caught too.
//
// Being shape-keyed rather than value-keyed also makes it catch the original
// bug. `specifications.md` did not say the *right* number; it said a stale one.
// A value-equality check would have passed it on the day it was written and
// failed it later, at the moment the reader was already misled. This fails it on
// the day it is written, whatever the number is.
//
// WHAT IT DOES NOT CATCH, stated rather than implied:
//
//   * A count written with no record noun anywhere near it — `<Stat n={14} />`
//     where the word "decisions" lives in a CSS `::after`. Rule (C) covers the
//     common component-prop spelling of that; a count hidden in a stylesheet is
//     out of reach of any source check short of rendering the page.
//   * A count computed at runtime from something that is not the data module —
//     `SHARE_TYPES.length` where `SHARE_TYPES` is a hand-written array. That is
//     derived-from-the-wrong-thing rather than a literal, and the honest fix is
//     the reviewer, not this file.
//   * A count spelled as a word. "fourteen decisions" is not matched, on purpose:
//     the false-positive surface of matching number words in English prose on a
//     marketing page is far larger than the bug it would catch.
//
// This runs inside the pipeline, from `generateRecord`, so it fails
// `docusaurus start` as well as `docusaurus build` — REQ "Referential Integrity
// and Front-Matter Validation" requires validation to live there rather than in
// an optional lint step.

/* eslint-disable @typescript-eslint/no-explicit-any */

import fs from 'node:fs/promises';
import path from 'node:path';

import {RecordError} from './parse.ts';
import {DECISIONS_SEGMENT, SPECS_SEGMENT, type RecordPaths} from './paths.ts';
import type {RecordCounts} from './types.ts';

/**
 * The nouns the record owns. A number adjacent to one of these is a claim about
 * the record, and the record is the only thing allowed to make it.
 *
 * `spec`/`specs` is included in its abbreviated form because that is what the
 * navbar and the homepage call them.
 */
const RECORD_NOUN =
  '(?:decisions?|decision records?|ADRs?|specifications?|specs?|SPECs?|requirements?|scenarios?)';

/**
 * Horizontal whitespace only — space and tab, never a line break.
 *
 * A count claim is a thing someone writes on ONE line. Plain `\s` crosses
 * newlines and blank lines, which turns the gap between two unrelated blocks
 * into a separator: a paragraph ending in "…the requirements" followed by a
 * new paragraph opening "3 of them…" is not a claim about 3 requirements, and a
 * heading `## Requirements` followed by any paragraph that starts with a digit
 * is not one either. Both of those made the build unpublishable.
 */
const H = '[^\\S\\r\\n]';

/**
 * `14 decisions`, `14+ decisions`, `14 published decisions`.
 *
 * One optional adjective is allowed between the two, because "14 accepted
 * decisions" is the same claim as "14 decisions".
 */
const numberThenNoun = (gap: string) =>
  new RegExp(
    `(?<![\\w.$-])(\\d{1,4})${gap}*\\+?${gap}+(?:[a-z]+${gap}+)?${RECORD_NOUN}(?![\\w-])`,
    'gi',
  );

/**
 * Punctuation that reads as "…and its count is": a colon, a table-cell wall, a
 * dash of either width, a bullet. The cell wall matters more than it looks —
 * `| Requirements | 219 |` is the `specifications.md` defect this gate exists
 * for, written as a table instead of a sentence.
 *
 * The comma and the sentence enders are deliberately absent, because
 * "…meets the requirements. 3 of them failed" is two sentences, not a claim,
 * and a gate that fires on prose gets deleted.
 */
const COUNT_SEPARATOR = '[:|—–•·]';

/**
 * `Decisions: 14`, `decisions 14`, `Requirements — 132`, `| Requirements | 219 |`.
 *
 * The ASCII hyphen is handled apart from the rest and only with whitespace on
 * at least one side, because it is also how the record spells an id: an
 * unguarded `-` turns every mention of `SPEC-0010` into a claim about 10
 * specifications.
 */
const nounThenNumber = (gap: string) =>
  new RegExp(
    `${RECORD_NOUN}(?:${gap}*${COUNT_SEPARATOR}${gap}*|${gap}+-?${gap}*|${gap}*-${gap}+)(\\d{1,4})(?![\\w.%-])`,
    'gi',
  );

/**
 * PROSE. The gap may not cross a line break: in running text a line break ends
 * a claim, so `## Requirements` above a paragraph opening "3 of the surfaces…"
 * is not a claim about 3 requirements.
 */
const PROSE = {
  numberThenNoun: numberThenNoun(H),
  nounThenNumber: nounThenNumber(H),
};

/**
 * MARKUP. The gap MAY cross a line break, because HTML collapses whitespace and
 * the reader sees one space: the `<strong>14</strong>` / `<span>decisions</span>`
 * pair this gate exists to catch is only ever split across lines.
 *
 * The text itself is left untouched rather than collapsed — `toFinding` counts
 * the newlines before a match to place it on the right line, and a fragment's
 * matches have to land on the same lines as the child elements' matches or they
 * stop de-duplicating against each other.
 */
const MARKUP = {
  numberThenNoun: numberThenNoun('\\s'),
  nounThenNumber: nounThenNumber('\\s'),
};

/** A literal that is nothing but an integer, e.g. a `value="14"` prop. */
const BARE_INTEGER = /^\s*(\d{1,4})\s*$/;

/**
 * Rule (C) is about the shape `<Stat value="14" label="Decisions" />` — a number
 * and a noun that are SIBLING PROPS. Recognising that shape needs the prop
 * NAMES, not just their values: without them the rule degenerates into "this
 * element has some integer and some string mentioning a record noun", which is
 * true of an enormous amount of ordinary correct markup.
 *
 * It collided head-on with this same specification's own accessibility
 * requirements. REQ "Icon-Only Controls" mandates `aria-label`, and REQ
 * "Keyboard Navigation & Focus Management" produces `tabIndex` — so
 * `<div role="region" tabIndex={0} aria-label="Specification endpoints">`, which
 * is precisely the fix REQ "Keyboard Navigation" asks for on a scrollable
 * table, failed the build as the hardcoded count `"region 0 Specification
 * endpoints"`. `<svg width="24" height="24" aria-label="Decisions" />` failed
 * the same way, reporting "no count the record derives equals 24".
 *
 * So the carriers are named explicitly on both sides.
 */
const LABEL_PROPS = new Set([
  'label',
  'title',
  'aria-label',
  'name',
  'caption',
  'heading',
  'legend',
]);

/**
 * The number side. A whitelist, not a blacklist: the numeric props that turn up
 * on ordinary markup (`tabIndex`, `width`, `height`, `size`, `span`, `colSpan`,
 * `rowSpan`, `start`, `maxLength`, `viewBox`, …) are open-ended, and a blacklist
 * would have to grow forever to keep up with them.
 */
const VALUE_PROPS = new Set([
  'value',
  'count',
  'n',
  'total',
  'number',
  'num',
  'amount',
]);

/**
 * The noun-carrying value must BE a record noun, not merely contain one.
 * `alt="The decisions index"` and `className="cairn-decisions-icon"` describe a
 * thing; `label="Decisions"` labels a number.
 */
const NOUN_ONLY = new RegExp(`^\\s*${RECORD_NOUN}\\s*$`, 'i');

/** Files whose source is scanned. JSON (the data module) is excluded by shape. */
const SCANNED_EXTENSIONS = new Set(['.ts', '.tsx', '.js', '.jsx', '.mjs', '.cjs']);

export interface CountCheckInput {
  paths: RecordPaths;
  counts: RecordCounts;
}

interface Finding {
  file: string;
  line: number;
  claim: string;
  number: number;
}

/**
 * Fail the build on any count of decisions, specifications, requirements or
 * scenarios written as a literal in site source.
 */
export async function assertNoLiteralCounts({
  paths,
  counts,
}: CountCheckInput): Promise<void> {
  const findings: Finding[] = [];
  const seen = new Set<string>();

  const record = (finding: Finding) => {
    const key = `${finding.file}:${finding.line}:${finding.claim}`;
    if (seen.has(key)) {
      return;
    }
    seen.add(key);
    findings.push(finding);
  };

  for (const absPath of await sourceFiles(paths)) {
    const rel = siteRelative(paths, absPath);
    const source = await fs.readFile(absPath, 'utf8');
    for (const hit of await scanSource(rel, source)) {
      record(hit);
    }
  }

  for (const absPath of await narrativeDocs(paths)) {
    const rel = siteRelative(paths, absPath);
    const source = await fs.readFile(absPath, 'utf8');
    for (const hit of scanText(rel, source, 0)) {
      record(hit);
    }
  }

  if (findings.length === 0) {
    return;
  }

  const live = liveCounts(counts);
  const lines = findings.map((finding) => {
    const match = live.find((entry) => entry.value === finding.number);
    const note = match
      ? `the record currently has ${match.value} ${match.label} — read \`counts.${match.key}\` from @site/src/data/record`
      : `no count the record derives equals ${finding.number} today, so this is already stale`;
    return `  ${finding.file}:${finding.line}: "${finding.claim}" — ${note}`;
  });

  throw new RecordError(
    'website source',
    `${findings.length} hardcoded record count${findings.length === 1 ? '' : 's'}. ` +
      `SPEC-0010 REQ "Derived Status, Dates, and Counts": no count of decisions, ` +
      `specifications, requirements or scenarios may be written as a literal in site ` +
      `source.\n\n${lines.join('\n')}\n`,
  );
}

function liveCounts(
  counts: RecordCounts,
): {key: keyof RecordCounts; label: string; value: number}[] {
  return [
    {key: 'decisions', label: 'decisions', value: counts.decisions},
    {
      key: 'specifications',
      label: 'specifications',
      value: counts.specifications,
    },
    {key: 'requirements', label: 'requirements', value: counts.requirements},
    {key: 'scenarios', label: 'scenarios', value: counts.scenarios},
  ];
}

/* ------------------------------------------------------------------ scanning */

/**
 * Apply both adjacency rules to a run of text. `offset` is its start line − 1.
 *
 * The number-first rule runs first and claims its spans, and the noun-first rule
 * skips anything overlapping one. Without that, `14 decisions 10 specifications`
 * produces a third, spurious finding: the substring `decisions 10` reads as a
 * noun-then-number claim while being nothing but the seam between two claims
 * already reported.
 */
function scanText(
  file: string,
  text: string,
  offset: number,
  mode: {numberThenNoun: RegExp; nounThenNumber: RegExp} = PROSE,
): Finding[] {
  const findings: Finding[] = [];
  const claimed: [number, number][] = [];

  const NUMBER_THEN_NOUN = mode.numberThenNoun;
  const NOUN_THEN_NUMBER = mode.nounThenNumber;

  NUMBER_THEN_NOUN.lastIndex = 0;
  let match: RegExpExecArray | null;
  while ((match = NUMBER_THEN_NOUN.exec(text))) {
    claimed.push([match.index, match.index + match[0].length]);
    findings.push(toFinding(file, text, offset, match));
  }

  NOUN_THEN_NUMBER.lastIndex = 0;
  while ((match = NOUN_THEN_NUMBER.exec(text))) {
    const start = match.index;
    const end = start + match[0].length;
    if (claimed.some(([from, to]) => start < to && end > from)) {
      continue;
    }
    findings.push(toFinding(file, text, offset, match));
  }

  return findings;
}

function toFinding(
  file: string,
  text: string,
  offset: number,
  match: RegExpExecArray,
): Finding {
  return {
    file,
    line: offset + lineOf(text, match.index),
    claim: match[0].replace(/\s+/g, ' ').trim(),
    number: Number(match[1]),
  };
}

const lineOf = (text: string, index: number) =>
  text.slice(0, index).split('\n').length;

/**
 * Parse one source file and scan three things:
 *
 *   (A) every string and template literal, and every run of JSX text;
 *   (B) the rendered text of every JSX subtree, stitched across elements;
 *   (C) a JSX element whose attributes carry a bare integer and a record noun.
 */
async function scanSource(file: string, source: string): Promise<Finding[]> {
  const {parse} = await import('@babel/parser');
  let ast: any;
  try {
    ast = parse(source, {
      sourceType: 'module',
      errorRecovery: false,
      // `typescript` and `jsx` together are what site source is written in.
      // Import attributes (`with {type: 'json'}`) need no plugin in Babel 8 and
      // parse out of the box.
      plugins: ['typescript', 'jsx', 'decorators-legacy'],
    });
  } catch (error) {
    throw new RecordError(
      file,
      `could not be parsed while checking for hardcoded record counts: ${
        (error as Error).message
      }`,
    );
  }

  const findings: Finding[] = [];
  const at = (node: any) => (node?.loc?.start?.line ?? 1) - 1;

  walk(ast, (node: any) => {
    // (A) literal text, wherever it sits.
    if (node.type === 'StringLiteral') {
      findings.push(...scanText(file, node.value, at(node)));
    } else if (node.type === 'TemplateElement') {
      findings.push(
        ...scanText(file, node.value.cooked ?? node.value.raw, at(node)),
      );
    } else if (node.type === 'JSXText') {
      findings.push(...scanText(file, node.value, at(node), MARKUP));
    }

    // (B) rendered text of a JSX subtree. Reported at the subtree root, which is
    // why (A) and (B) are de-duplicated by claim text at the call site: a claim
    // wholly inside one JSXText node is found by both.
    if (node.type === 'JSXElement' || node.type === 'JSXFragment') {
      findings.push(...scanText(file, renderedText(node), at(node), MARKUP));

      // (C) `<Stat value="14" label="Decisions" />`: the number and the noun are
      // siblings in the props rather than adjacent in the text, so neither
      // adjacency rule can see them. Both sides are matched BY PROP NAME — see
      // LABEL_PROPS/VALUE_PROPS for why "any integer plus any string mentioning
      // a noun" is far too broad to live in a build gate.
      const attributes = attributeLiterals(node);
      const counter = attributes.find(
        (a) => VALUE_PROPS.has(a.name) && BARE_INTEGER.test(a.value),
      );
      const label = attributes.find(
        (a) => LABEL_PROPS.has(a.name) && NOUN_ONLY.test(a.value),
      );
      if (counter && label) {
        findings.push({
          file,
          line: at(node) + 1,
          claim: `${label.value.trim()} ${counter.value.trim()}`,
          number: Number(BARE_INTEGER.exec(counter.value)![1]),
        });
      }
    }
  });

  return findings;
}


/**
 * What a reader sees. Children are concatenated in source order and every
 * element boundary becomes a space, so `<strong>14</strong><span>decisions</span>`
 * reads as `14 decisions` — the adjacency the markup was hiding.
 *
 * An expression the checker cannot evaluate becomes a space rather than nothing.
 * That is deliberate: `{counts.decisions} decisions` must NOT be stitched into
 * something that looks like a literal claim, because it is exactly the correct
 * spelling.
 */
function renderedText(node: any): string {
  const parts: string[] = [];
  for (const child of node.children ?? []) {
    switch (child.type) {
      case 'JSXText':
        parts.push(child.value);
        break;
      case 'JSXElement':
      case 'JSXFragment':
        parts.push(' ', renderedText(child), ' ');
        break;
      case 'JSXExpressionContainer':
        parts.push(' ', literalValue(child.expression), ' ');
        break;
      default:
        parts.push(' ');
    }
  }
  return parts.join('');
}

/** The text of an expression the checker can see through; `''` otherwise. */
function literalValue(expression: any): string {
  if (!expression) {
    return '';
  }
  if (expression.type === 'StringLiteral') {
    return expression.value;
  }
  if (expression.type === 'NumericLiteral') {
    return String(expression.value);
  }
  if (
    expression.type === 'TemplateLiteral' &&
    expression.expressions.length === 0
  ) {
    return expression.quasis
      .map((quasi: any) => quasi.value.cooked ?? quasi.value.raw)
      .join('');
  }
  return '';
}

/**
 * Every literal-valued attribute on a JSX element, WITH its name. Rule (C)
 * needs the name to tell a labelled count from ordinary markup that merely
 * happens to carry a number and a noun.
 */
function attributeLiterals(node: any): {name: string; value: string}[] {
  const opening = node.openingElement;
  if (!opening) {
    return [];
  }
  const attributes: {name: string; value: string}[] = [];
  for (const attribute of opening.attributes ?? []) {
    if (attribute.type !== 'JSXAttribute' || !attribute.value) {
      continue;
    }
    // `aria-label` parses as a JSXNamespacedName-free JSXIdentifier with a
    // dash, so `.name` is the string either way.
    const name =
      typeof attribute.name?.name === 'string' ? attribute.name.name : '';
    if (attribute.value.type === 'StringLiteral') {
      attributes.push({name, value: attribute.value.value});
    } else if (attribute.value.type === 'JSXExpressionContainer') {
      const text = literalValue(attribute.value.expression);
      if (text) {
        attributes.push({name, value: text});
      }
    }
  }
  return attributes;
}

/** Depth-first over every AST node, without a visitor dependency. */
function walk(node: any, visit: (node: any) => void): void {
  if (!node || typeof node !== 'object') {
    return;
  }
  if (Array.isArray(node)) {
    for (const child of node) {
      walk(child, visit);
    }
    return;
  }
  if (typeof node.type === 'string') {
    visit(node);
  }
  for (const key of Object.keys(node)) {
    if (key === 'loc' || key === 'leadingComments' || key === 'trailingComments') {
      continue;
    }
    const value = (node as any)[key];
    if (value && typeof value === 'object') {
      walk(value, visit);
    }
  }
}

/* --------------------------------------------------------------- file lists */

/** Site source: everything under `src/`, plus the site config. */
async function sourceFiles(paths: RecordPaths): Promise<string[]> {
  const srcDir = path.join(paths.siteDir, 'src');
  const generatedDir = paths.generatedDir;
  const files = (await walkDir(srcDir)).filter(
    (file) =>
      !file.startsWith(generatedDir + path.sep) &&
      SCANNED_EXTENSIONS.has(path.extname(file)),
  );
  // `sidebars.ts` sits beside the config rather than under `src/`, and a
  // category `label: "14 decisions"` renders on every docs page — as reachable
  // a place to strand a stale count as any component.
  for (const name of ['docusaurus.config.ts', 'sidebars.ts']) {
    const file = path.join(paths.siteDir, name);
    if (await exists(file)) {
      files.push(file);
    }
  }
  return files.sort();
}

/**
 * The hand-written narrative pages. The STAGED trees are excluded: they are the
 * record's own text, and a decision record is allowed to say "ADR-0012" or count
 * its own requirements — that prose is the thing being published, not a claim
 * the site made up.
 */
async function narrativeDocs(paths: RecordPaths): Promise<string[]> {
  const staged = [
    path.join(paths.docsDir, DECISIONS_SEGMENT) + path.sep,
    path.join(paths.docsDir, SPECS_SEGMENT) + path.sep,
  ];
  const isMarkdown = (file: string) =>
    file.endsWith('.md') || file.endsWith('.mdx');

  const docs = (await walkDir(paths.docsDir)).filter(
    (file) => isMarkdown(file) && !staged.some((p) => file.startsWith(p)),
  );

  /*
   * `src/pages/*.md` and `*.mdx` are first-class Docusaurus pages — a file at
   * `src/pages/stats.md` publishes at `/stats` exactly like a `.tsx` page. They
   * belong to NEITHER list otherwise: `sourceFiles()` filters to
   * SCANNED_EXTENSIONS because it hands everything to the Babel parser, and
   * this function only walked `docs/`. That gap is the deleted
   * `specifications.md` failure mode one file extension away, so markdown under
   * `src/` is scanned here, on the text path, where it can actually be read.
   */
  const pages = (await walkDir(path.join(paths.siteDir, 'src'))).filter(
    (file) =>
      isMarkdown(file) && !file.startsWith(paths.generatedDir + path.sep),
  );

  return [...docs, ...pages].sort();
}

async function walkDir(dir: string): Promise<string[]> {
  let entries;
  try {
    entries = await fs.readdir(dir, {withFileTypes: true});
  } catch {
    return [];
  }
  const out: string[] = [];
  for (const entry of entries) {
    const absPath = path.join(dir, entry.name);
    if (entry.isDirectory()) {
      out.push(...(await walkDir(absPath)));
    } else if (entry.isFile()) {
      out.push(absPath);
    }
  }
  return out;
}

async function exists(absPath: string): Promise<boolean> {
  try {
    await fs.access(absPath);
    return true;
  } catch {
    return false;
  }
}

function siteRelative(paths: RecordPaths, absPath: string): string {
  return path.relative(paths.siteDir, absPath).split(path.sep).join('/');
}
