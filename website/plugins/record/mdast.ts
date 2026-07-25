// Governing: ADR-0014, SPEC-0010 REQ "Record Prose to Structured Components"
// Governing: ADR-0014, SPEC-0010 REQ "CommonMark Fidelity"
//
// Every structural transform of the record happens here, on the parsed tree.
//
// Nothing in this module writes component markup into record text. That is the
// whole point: a document that contains literal JSX can no longer contain an
// un-escaped `<` or `{`, and record prose is full of both. Rewriting the mdast
// after parsing means the surrounding prose is never re-parsed, so a bare `<`
// in an ADR paragraph stays a text node and cannot become a tag.
//
// The same functions run twice, deliberately, and that is not duplication:
// the generator calls them to derive counts, anchors and page descriptions from
// the *transformed* document, and the remark plugin calls them again inside the
// MDX compiler to produce the components. One implementation, two callers.

/* eslint-disable @typescript-eslint/no-explicit-any */

import type {RequirementEntry} from './types.ts';

// mdast/unist types are not a declared dependency of this site, and pulling
// them in would buy very little: these trees are walked structurally.
type Node = any;
type Parent = {children: Node[]} & Node;

/** RFC 2119 keywords, longest first so `MUST NOT` wins over `MUST`. */
export const RFC2119_KEYWORDS = [
  'MUST NOT',
  'SHALL NOT',
  'SHOULD NOT',
  'NOT RECOMMENDED',
  'MUST',
  'SHALL',
  'SHOULD',
  'REQUIRED',
  'RECOMMENDED',
  'OPTIONAL',
  'MAY',
] as const;

// Alternation is leftmost-first, so `MUST NOT` must precede `MUST` in the list
// above or every negative keyword would be wrapped as its positive twin. The
// boundary classes keep `MUSTARD`, `MAYBE` and `SHOULDER` out.
const RFC2119_PATTERN = new RegExp(
  `(?<![A-Za-z0-9_])(${RFC2119_KEYWORDS.join('|')})(?![A-Za-z0-9_])`,
  'g',
);

/**
 * mdast node types whose children are phrasing content. A raw-HTML node inside
 * one of these becomes a text node; anywhere else it is in flow position and has
 * to be wrapped in a paragraph, or `mdast-util-to-hast` renders it loose.
 */
const PHRASING_PARENTS = new Set([
  'paragraph',
  'heading',
  'emphasis',
  'strong',
  'delete',
  'link',
  'linkReference',
  'tableCell',
]);

/** The two step keywords the record's scenarios are written with. */
const SCENARIO_STEPS = new Set(['WHEN', 'THEN', 'GIVEN', 'AND', 'AND WHEN']);

const HTML_COMMENT = /^<!--[\s\S]*-->$/;

export const REQUIREMENT_HEADING_DEPTH = 3;
export const SCENARIO_HEADING_DEPTH = 4;

const REQUIREMENT_PREFIX = 'Requirement:';
const SCENARIO_PREFIX = 'Scenario:';

// ---------------------------------------------------------------------------
// Parsing
// ---------------------------------------------------------------------------

/**
 * Parse record CommonMark to mdast. GFM is enabled because the record uses
 * pipe tables throughout; front-matter is *not* an extension here because
 * callers hand us a body with the front-matter already split off.
 */
export async function parseRecordMarkdown(body: string): Promise<Node> {
  const {unified} = await import('unified');
  const remarkParse = (await import('remark-parse')).default;
  const remarkGfm = (await import('remark-gfm')).default;
  return unified().use(remarkParse).use(remarkGfm).parse(body);
}

async function nodeToString(node: Node): Promise<string> {
  const {toString} = await import('mdast-util-to-string');
  return toString(node);
}

// ---------------------------------------------------------------------------
// CommonMark fidelity: raw HTML becomes literal text
// ---------------------------------------------------------------------------

/**
 * A word shaped like an HTML tag — `<id>`, `<hash>`, `<form>` — is a valid
 * CommonMark open tag, so it parses to an `html` node. Docusaurus then runs
 * `rehype-raw` over `format: 'md'` documents
 * (`@docusaurus/mdx-loader/lib/processor.js`), which turns that node into a
 * real `<id>` element in the DOM, and any excerpt taken from the raw source is
 * truncated at it. The record is not going to stop writing `/<id>`, so the
 * pipeline turns those nodes back into the literal text the author meant.
 *
 * HTML comments are dropped rather than literalised — printing `<!-- … -->`
 * into the page would be worse than either alternative.
 *
 * @returns the number of nodes rewritten, and the number dropped.
 */
export function literaliseRawHtml(tree: Node): {
  literalised: number;
  dropped: number;
} {
  let literalised = 0;
  let dropped = 0;

  const walk = (parent: Parent): void => {
    if (!Array.isArray(parent.children)) {
      return;
    }
    const next: Node[] = [];
    for (const child of parent.children) {
      if (child.type === 'html') {
        if (HTML_COMMENT.test(child.value.trim())) {
          dropped += 1;
          continue;
        }
        literalised += 1;
        const text = {
          type: 'text',
          value: child.value,
          position: child.position,
        };
        next.push(
          PHRASING_PARENTS.has(parent.type)
            ? text
            : {type: 'paragraph', children: [text], position: child.position},
        );
        continue;
      }
      // `code` holds verbatim text and must never be walked into.
      if (child.type !== 'code' && child.type !== 'inlineCode') {
        walk(child as Parent);
      }
      next.push(child);
    }
    parent.children = next;
  };

  walk(tree as Parent);
  return {literalised, dropped};
}

// ---------------------------------------------------------------------------
// RFC 2119 keywords
// ---------------------------------------------------------------------------

/**
 * Wrap every RFC 2119 keyword in an `<Rfc2119>` text element so the keyword can
 * be distinguished by weight and carry an accessible name. Code, inline code and
 * headings are skipped: a keyword inside a code span is part of the code.
 *
 * @returns the number of keywords wrapped.
 */
export function wrapRfc2119Keywords(tree: Node): number {
  let wrapped = 0;

  const walk = (parent: Parent): void => {
    if (!Array.isArray(parent.children)) {
      return;
    }
    const next: Node[] = [];
    for (const child of parent.children) {
      if (child.type === 'text') {
        const pieces = splitRfc2119(child.value);
        if (pieces.length === 1 && pieces[0]!.type === 'text') {
          next.push(child);
        } else {
          wrapped += pieces.filter((p) => p.type !== 'text').length;
          next.push(...pieces);
        }
        continue;
      }
      if (
        child.type === 'code' ||
        child.type === 'inlineCode' ||
        child.type === 'heading' ||
        child.type === 'mdxJsxTextElement'
      ) {
        next.push(child);
        continue;
      }
      walk(child as Parent);
      next.push(child);
    }
    parent.children = next;
  };

  walk(tree as Parent);
  return wrapped;
}

function splitRfc2119(value: string): Node[] {
  const out: Node[] = [];
  let last = 0;
  for (const match of value.matchAll(RFC2119_PATTERN)) {
    const start = match.index!;
    if (start > last) {
      out.push({type: 'text', value: value.slice(last, start)});
    }
    out.push(
      jsxTextElement('Rfc2119', [attribute('keyword', match[1]!)], [
        {type: 'text', value: match[1]!},
      ]),
    );
    last = start + match[1]!.length;
  }
  if (out.length === 0) {
    return [{type: 'text', value}];
  }
  if (last < value.length) {
    out.push({type: 'text', value: value.slice(last)});
  }
  return out;
}

// ---------------------------------------------------------------------------
// Requirement / Scenario structure
// ---------------------------------------------------------------------------

/**
 * Rewrite `### Requirement:` and `#### Scenario:` sections into component
 * subtrees, in place.
 *
 * The section's own heading is kept as the first child of the block rather than
 * being replaced. Docusaurus's `headings` and `toc` remark plugins visit *every*
 * node (`@docusaurus/mdx-loader/lib/remark/toc/index.js` uses an untyped
 * `visit`), so a heading nested inside a JSX flow element still gets its anchor
 * and still reaches the on-this-page column. Replacing the headings would
 * silently empty the table of contents of every specification.
 */
export async function structureRequirements(tree: Node): Promise<void> {
  const {toString} = await import('mdast-util-to-string');
  tree.children = await rewriteSections(tree.children ?? [], toString);
}

async function rewriteSections(
  children: Node[],
  toString: (node: Node) => string,
): Promise<Node[]> {
  const out: Node[] = [];
  let i = 0;
  while (i < children.length) {
    const node = children[i]!;
    const name = sectionName(node, REQUIREMENT_HEADING_DEPTH, REQUIREMENT_PREFIX, toString);
    if (name === null) {
      out.push(node);
      i += 1;
      continue;
    }
    let end = i + 1;
    while (
      end < children.length &&
      !isHeadingAtMost(children[end]!, REQUIREMENT_HEADING_DEPTH)
    ) {
      end += 1;
    }
    const body = rewriteScenarios(children.slice(i + 1, end), toString);
    out.push(
      jsxFlowElement(
        'RequirementBlock',
        [attribute('name', name)],
        [node, ...body],
      ),
    );
    i = end;
  }
  return out;
}

function rewriteScenarios(
  children: Node[],
  toString: (node: Node) => string,
): Node[] {
  const out: Node[] = [];
  let i = 0;
  while (i < children.length) {
    const node = children[i]!;
    const name = sectionName(node, SCENARIO_HEADING_DEPTH, SCENARIO_PREFIX, toString);
    if (name === null) {
      out.push(node);
      i += 1;
      continue;
    }
    let end = i + 1;
    while (
      end < children.length &&
      !isHeadingAtMost(children[end]!, SCENARIO_HEADING_DEPTH)
    ) {
      end += 1;
    }
    const body = children
      .slice(i + 1, end)
      .map((child) => rewriteScenarioSteps(child, toString));
    out.push(
      jsxFlowElement('ScenarioBlock', [attribute('name', name)], [node, ...body]),
    );
    i = end;
  }
  return out;
}

/**
 * `- **WHEN** …` / `- **THEN** …` become `<ScenarioStep step="WHEN">` blocks.
 * A list whose items do not match is passed through untouched, so a scenario
 * written some other way degrades to plain prose instead of breaking.
 */
function rewriteScenarioSteps(
  node: Node,
  toString: (node: Node) => string,
): Node {
  if (node.type !== 'list' || !Array.isArray(node.children)) {
    return node;
  }
  const steps: Node[] = [];
  for (const item of node.children) {
    const step = extractStep(item, toString);
    if (!step) {
      return node; // mixed list: leave the whole thing alone
    }
    steps.push(step);
  }
  return jsxFlowElement('ScenarioSteps', [], steps);
}

function extractStep(
  item: Node,
  toString: (node: Node) => string,
): Node | null {
  if (item.type !== 'listItem' || !Array.isArray(item.children)) {
    return null;
  }
  const [first, ...restBlocks] = item.children;
  if (!first || first.type !== 'paragraph' || !Array.isArray(first.children)) {
    return null;
  }
  const [marker, ...rest] = first.children;
  if (!marker || marker.type !== 'strong') {
    return null;
  }
  const keyword = toString(marker).trim().toUpperCase();
  if (!SCENARIO_STEPS.has(keyword)) {
    return null;
  }
  const body = rest.slice();
  if (body[0]?.type === 'text') {
    body[0] = {...body[0], value: body[0].value.replace(/^\s+/, '')};
  }
  return jsxFlowElement(
    'ScenarioStep',
    [attribute('step', keyword)],
    [{type: 'paragraph', children: body}, ...restBlocks],
  );
}

function sectionName(
  node: Node,
  depth: number,
  prefix: string,
  toString: (node: Node) => string,
): string | null {
  if (node.type !== 'heading' || node.depth !== depth) {
    return null;
  }
  const text = toString(node).trim();
  if (!text.startsWith(prefix)) {
    return null;
  }
  return text.slice(prefix.length).trim();
}

function isHeadingAtMost(node: Node, depth: number): boolean {
  return node.type === 'heading' && node.depth <= depth;
}

// ---------------------------------------------------------------------------
// Inventory: counts and anchors
// ---------------------------------------------------------------------------

/**
 * Count requirements and scenarios and compute each requirement's heading
 * anchor, walking the tree in document order.
 *
 * The anchor has to be the one Docusaurus will emit, so this runs the same
 * slugger over the same headings in the same order as
 * `@docusaurus/mdx-loader/lib/remark/headings`: every heading in the document
 * feeds the slugger, not just the ones we care about, because the slugger
 * de-duplicates and skipping a heading would shift every suffix after it.
 *
 * Call this on the *transformed* tree — after `literaliseRawHtml` — because the
 * remark plugin runs before `headings` does and a literalised `<id>` changes the
 * text the slugger sees.
 */
export async function collectInventory(tree: Node): Promise<{
  requirements: RequirementEntry[];
  scenarioCount: number;
}> {
  const {toString} = await import('mdast-util-to-string');
  const {createSlugger} = await import('@docusaurus/utils');
  const slugs = createSlugger();

  const requirements: RequirementEntry[] = [];
  let scenarioCount = 0;
  let current: RequirementEntry | null = null;

  const walk = (node: Node): void => {
    if (node.type === 'heading') {
      // Mirror Docusaurus: `html`/`jsx` children are excluded from heading text.
      const textNodes = (node.children ?? []).filter(
        (child: Node) => child.type !== 'html' && child.type !== 'jsx',
      );
      const text = toString(textNodes.length > 0 ? textNodes : node);
      const anchor = slugs.slug(text, {maintainCase: false});

      const requirementName = text.trim().startsWith(REQUIREMENT_PREFIX)
        ? text.trim().slice(REQUIREMENT_PREFIX.length).trim()
        : null;
      if (node.depth === REQUIREMENT_HEADING_DEPTH && requirementName !== null) {
        current = {name: requirementName, anchor, scenarioCount: 0};
        requirements.push(current);
      } else if (
        node.depth === SCENARIO_HEADING_DEPTH &&
        text.trim().startsWith(SCENARIO_PREFIX)
      ) {
        scenarioCount += 1;
        if (current) {
          current.scenarioCount += 1;
        }
      } else if (node.depth <= REQUIREMENT_HEADING_DEPTH) {
        current = null;
      }
      return;
    }
    for (const child of node.children ?? []) {
      walk(child);
    }
  };

  walk(tree);
  return {requirements, scenarioCount};
}

// ---------------------------------------------------------------------------
// Page description
// ---------------------------------------------------------------------------

const MIN_SUMMARY = 80;
const MAX_SUMMARY = 220;

/**
 * Derive a page summary from the transformed tree rather than from the raw
 * source, so a tag-shaped word never truncates it. Takes whole sentences from
 * the first body paragraph until the summary is long enough to be useful.
 */
export async function deriveSummary(tree: Node): Promise<string> {
  const first = (tree.children ?? []).find(
    (node: Node) => node.type === 'paragraph',
  );
  if (!first) {
    return '';
  }
  const text = (await nodeToString(first)).replace(/\s+/g, ' ').trim();
  const sentences = text.split(/(?<=[.!?])\s+/);
  let out = '';
  for (const sentence of sentences) {
    const candidate = out ? `${out} ${sentence}` : sentence;
    if (out.length >= MIN_SUMMARY || candidate.length > MAX_SUMMARY) {
      break;
    }
    out = candidate;
  }
  if (!out) {
    out = sentences[0] ?? text;
  }
  return out.length > MAX_SUMMARY
    ? `${out.slice(0, MAX_SUMMARY).replace(/\s+\S*$/, '')}…`
    : out;
}

/** Plain-text form of a heading, with inline code and emphasis flattened. */
export async function headingText(node: Node): Promise<string> {
  return (await nodeToString(node)).replace(/\s+/g, ' ').trim();
}

/** The document's first depth-1 heading, or null. */
export function findTitleHeading(tree: Node): Node | null {
  for (const child of tree.children ?? []) {
    if (child.type === 'heading' && child.depth === 1) {
      return child;
    }
  }
  return null;
}

// ---------------------------------------------------------------------------
// JSX node construction
// ---------------------------------------------------------------------------

export function attribute(name: string, value: string): Node {
  return {type: 'mdxJsxAttribute', name, value};
}

export function jsxFlowElement(
  name: string,
  attributes: Node[],
  children: Node[],
): Node {
  return {type: 'mdxJsxFlowElement', name, attributes, children};
}

export function jsxTextElement(
  name: string,
  attributes: Node[],
  children: Node[],
): Node {
  return {type: 'mdxJsxTextElement', name, attributes, children};
}

/**
 * The set of components a transformed record document can reference. The remark
 * plugin injects an ESM import for each one it actually used — see
 * `remark-record.ts` for why the imports cannot be written into the text.
 */
export const RECORD_COMPONENTS = [
  'RequirementBlock',
  'ScenarioBlock',
  'ScenarioSteps',
  'ScenarioStep',
  'Rfc2119',
] as const;
