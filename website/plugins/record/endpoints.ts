// Governing: ADR-0014, SPEC-0010 REQ "Derived HTTP Reference Page"
//
// The API surface, read out of the specifications instead of retyped.
//
// ADR-0014 lists "the API reference surface" among the things derived from "the
// specs' own endpoint tables", and a hand-kept copy of that surface is precisely
// the defect class the whole decision exists to kill: `specifications.md` stated
// a requirement total in prose and was wrong about it before anyone noticed.
//
// Three properties of the record make this harder than "parse a table", and all
// three are handled here rather than by asking the record to change shape:
//
//   1. The sections are named four different things — `## HTTP Endpoints`,
//      `## REST Endpoints`, `## Endpoint Table`, `## Web Routes`. The list is
//      ENUMERATED, not pattern-matched: a heading that merely contains the word
//      "endpoint" is not an endpoint table, and guessing would silently publish
//      whatever prose happened to sit under it.
//   2. The columns are not the same table twice. Three shapes ship today —
//      `Method & Path | Purpose | Auth`, `Method | Path | Purpose | Auth`, and
//      `Endpoint | Method | Purpose | Auth`. Columns are therefore identified by
//      their HEADER, never by position.
//   3. Six of ten specifications carry such a section and four do not, so the
//      derived page is a partial view by construction. That is reported as
//      coverage rather than hidden: SPEC-0010 requires the page not to claim a
//      complete API surface, and it can only avoid claiming it if it knows which
//      specifications contributed nothing.

/* eslint-disable @typescript-eslint/no-explicit-any */

import type {
  EndpointRow,
  EndpointSection,
  EndpointSpecCoverage,
  RawEndpointRow,
  RecordEndpoints,
  SpecEntry,
} from './types.ts';

// mdast/unist types are not a declared dependency of this site; these trees are
// walked structurally, exactly as `mdast.ts` walks them.
type Node = any;

/**
 * The enumerated section names, as SPEC-0010 REQ "Derived HTTP Reference Page"
 * states them. Adding a fifth spelling to the record means adding it here; that
 * is deliberate, because the alternative is a heuristic that decides for itself
 * what an endpoint table is.
 */
export const ENDPOINT_SECTION_NAMES = [
  'HTTP Endpoints',
  'REST Endpoints',
  'Endpoint Table',
  'Web Routes',
] as const;

/**
 * The capability whose own `## Web Routes` table is NOT part of the service API.
 *
 * SPEC-0010's route table describes this website — a static bundle with no
 * server — and its columns (`Route | Content | Source`) would otherwise parse
 * cleanly and publish `/docs/decisions` beside `POST /v1/artifacts`. The
 * specification says so in its own prose; parsing that prose to find out would
 * be a worse dependency than naming the capability, so it is named, and its
 * existence is asserted below so a directory rename fails the build instead of
 * quietly re-admitting the table.
 */
export const WEBSITE_CAPABILITY = 'public-website-and-design-record';

/** Endpoint sections are `##`, like every other top-level section in a spec. */
const SECTION_HEADING_DEPTH = 2;

/**
 * A path always starts with `/` in this record, which is what makes a combined
 * "Method & Path" cell splittable at all: everything before the first `/`-led
 * token is the method (or methods — `POST / GET`), everything after the token is
 * a parenthetical the author added (`(web)`, `(also hook.cairn.sh/{id})`).
 */
const METHOD_AND_PATH = /^((?:[A-Z]{3,7})(?:\s*\/\s*[A-Z]{3,7})*)\s+(\/\S*)\s*(.*)$/;

/** A path cell that carries a trailing parenthetical of its own. */
const PATH_AND_NOTE = /^(\S+)\s*(.*)$/;

/**
 * An auth cell is "posture — justification": `Public — gated by artifact link
 * capability (ADR-0007)`. The posture is the machine-readable half and the half
 * a reader scans a column for; the justification is the sentence the record
 * wrote to defend it, and both are kept. Split only on a SPACED dash, so
 * `Client-authenticated (PKCE)` survives intact.
 */
const AUTH_POSTURE_SPLIT = /\s+[—–-]\s+/;

/** SSE is how this record spells "streaming", in a method, a purpose or a path. */
const STREAMING_PATH = /\/stream\b/;
const STREAMING_TEXT = /\bSSE\b/;

type Column = 'methodAndPath' | 'method' | 'path' | 'purpose' | 'auth' | 'ignore';

/**
 * Identify a column by its header. Position is never used: the three table
 * shapes in the record put the method first, second, and nowhere respectively.
 */
function classifyColumn(header: string): Column {
  const text = header
    .toLowerCase()
    .replace(/[^a-z]+/g, ' ')
    .trim();
  const hasMethod = /\bmethod\b/.test(text);
  const hasPath = /\b(?:path|route|endpoint|url)\b/.test(text);
  if (hasMethod && hasPath) {
    return 'methodAndPath';
  }
  if (hasMethod) {
    return 'method';
  }
  if (hasPath) {
    return 'path';
  }
  if (/\bauth\b/.test(text)) {
    return 'auth';
  }
  if (/\b(?:purpose|description|content|summary)\b/.test(text)) {
    return 'purpose';
  }
  return 'ignore';
}

function cellText(node: Node, toString: (n: Node) => string): string {
  return toString(node).replace(/\s+/g, ' ').trim();
}

function splitMethodAndPath(value: string): {
  method: string | null;
  path: string;
  note: string | null;
} {
  const match = METHOD_AND_PATH.exec(value);
  if (!match) {
    // No method prefix: treat the whole cell as the path rather than dropping a
    // row the record clearly meant to publish.
    return {method: null, path: value, note: null};
  }
  return {
    method: match[1]!.replace(/\s*\/\s*/g, ' / '),
    path: match[2]!,
    note: match[3]!.trim() || null,
  };
}

function splitPathCell(value: string): {path: string; note: string | null} {
  const match = PATH_AND_NOTE.exec(value);
  if (!match) {
    return {path: value, note: null};
  }
  return {path: match[1]!, note: match[2]!.trim() || null};
}

function parseTable(table: Node, toString: (n: Node) => string): RawEndpointRow[] {
  const [head, ...body] = (table.children ?? []) as Node[];
  if (!head) {
    return [];
  }
  const columns = ((head.children ?? []) as Node[]).map((cell) =>
    classifyColumn(cellText(cell, toString)),
  );
  // A table with no path-bearing column is not an endpoint table, whatever
  // heading it happens to sit under. Contributing nothing is the correct
  // outcome, and it is not an error.
  if (!columns.includes('path') && !columns.includes('methodAndPath')) {
    return [];
  }

  const rows: RawEndpointRow[] = [];
  for (const bodyRow of body) {
    const cells = ((bodyRow.children ?? []) as Node[]).map((cell) =>
      cellText(cell, toString),
    );
    // Accumulated on an object rather than in four locals: the assignments
    // happen inside a callback, where control-flow analysis of a `let` narrowed
    // to its initialiser cannot follow them.
    const row: RawEndpointRow = {
      method: null,
      path: '',
      note: null,
      purpose: '',
      auth: null,
      authPosture: null,
      streaming: false,
    };

    columns.forEach((column, index) => {
      const value = cells[index] ?? '';
      if (column === 'methodAndPath') {
        const split = splitMethodAndPath(value);
        row.method = split.method;
        row.path = split.path;
        row.note = split.note;
      } else if (column === 'method') {
        row.method = value || null;
      } else if (column === 'path') {
        const split = splitPathCell(value);
        row.path = split.path;
        row.note = split.note;
      } else if (column === 'purpose') {
        row.purpose = value;
      } else if (column === 'auth') {
        row.auth = value || null;
      }
    });

    if (!row.path) {
      continue;
    }

    row.authPosture = row.auth
      ? (row.auth.split(AUTH_POSTURE_SPLIT)[0] ?? row.auth).trim()
      : null;
    row.streaming =
      STREAMING_PATH.test(row.path) ||
      STREAMING_TEXT.test(row.purpose) ||
      STREAMING_TEXT.test(row.method ?? '');
    rows.push(row);
  }
  return rows;
}

/**
 * Every enumerated endpoint section in one parsed record, with the rows of every
 * table beneath it.
 *
 * Call this on the *transformed* tree — after `literaliseRawHtml` — for the same
 * reason `collectInventory` insists on it: the anchor recorded here has to be the
 * one Docusaurus will emit, and a literalised `<id>` changes the text the slugger
 * sees. Every heading in the document feeds the slugger, not just the ones this
 * function cares about, because the slugger de-duplicates and skipping a heading
 * would shift every suffix after it.
 */
export async function collectEndpointSections(
  tree: Node,
): Promise<EndpointSection[]> {
  const {toString} = await import('mdast-util-to-string');
  const {createSlugger} = await import('@docusaurus/utils');
  const slugs = createSlugger();

  const names: readonly string[] = ENDPOINT_SECTION_NAMES;
  const sections: EndpointSection[] = [];
  let current: EndpointSection | null = null;

  const walk = (node: Node): void => {
    if (node.type === 'heading') {
      // Mirror Docusaurus: `html`/`jsx` children are excluded from heading text.
      const textNodes = ((node.children ?? []) as Node[]).filter(
        (child) => child.type !== 'html' && child.type !== 'jsx',
      );
      const text = toString(textNodes.length > 0 ? textNodes : node)
        .replace(/\s+/g, ' ')
        .trim();
      const anchor = slugs.slug(text, {maintainCase: false});
      if (node.depth === SECTION_HEADING_DEPTH && names.includes(text)) {
        current = {title: text, anchor, rows: []};
        sections.push(current);
      } else if (node.depth <= SECTION_HEADING_DEPTH) {
        current = null;
      }
      return;
    }
    if (node.type === 'table') {
      if (current) {
        current.rows.push(...parseTable(node, toString));
      }
      return;
    }
    for (const child of (node.children ?? []) as Node[]) {
      walk(child);
    }
  };

  walk(tree);
  return sections.filter((section) => section.rows.length > 0);
}

/**
 * Flatten every specification's endpoint sections into the reference surface.
 *
 * Ordering is specification order, then document order, so the page reads like
 * the record does. `coverage` carries EVERY specification, contributing or not,
 * because the requirement that the page must not claim a complete API surface is
 * only satisfiable if the page can name the specifications that stayed silent.
 */
export function buildEndpointReference(
  specs: SpecEntry[],
  sectionsBySpecId: Map<string, EndpointSection[]>,
): RecordEndpoints {
  if (!specs.some((spec) => spec.capability === WEBSITE_CAPABILITY)) {
    throw new Error(
      `no capability directory named '${WEBSITE_CAPABILITY}' under docs/openspec/specs/. ` +
        `The HTTP reference excludes that specification's own '## Web Routes' table, which ` +
        `describes the static website rather than the service; if the directory was renamed, ` +
        `update WEBSITE_CAPABILITY in website/plugins/record/endpoints.ts rather than letting ` +
        `the site's own route map be published as an API surface.`,
    );
  }

  const rows: EndpointRow[] = [];
  const coverage: EndpointSpecCoverage[] = [];

  for (const spec of specs) {
    const sections = (sectionsBySpecId.get(spec.id) ?? []).filter(
      (section) =>
        !(spec.capability === WEBSITE_CAPABILITY && section.title === 'Web Routes'),
    );
    let rowCount = 0;
    for (const section of sections) {
      for (const raw of section.rows) {
        rows.push({
          ...raw,
          specId: spec.id,
          specTitle: spec.title,
          specHref: spec.href,
          section: section.title,
          sectionHref: `${spec.href}#${section.anchor}`,
        });
        rowCount += 1;
      }
    }
    coverage.push({
      specId: spec.id,
      specTitle: spec.title,
      specHref: spec.href,
      sections: sections.map((section) => section.title),
      rowCount,
    });
  }

  return {
    sectionNames: [...ENDPOINT_SECTION_NAMES],
    rows,
    coverage,
  };
}
