#!/usr/bin/env node
/**
 * Accessibility guard.
 *
 * Governing: ADR-0014 (the public site is the design record),
 *            SPEC-0010 REQ "Contrast",
 *            SPEC-0010 REQ "Keyboard Navigation & Focus Management",
 *            SPEC-0010 REQ "WCAG 2.1 AA & Semantics",
 *            SPEC-0010 REQ "Non-Colour Encoding"
 *
 * WHERE THIS RUNS. Alongside `check-tokens.mjs`, and for the same reason
 * (design.md, "Validation runs where it can actually run"): source-level checks
 * belong at CONFIG LOAD, so they fire on `docusaurus build` and `docusaurus
 * start` alike and no `npx` invocation can route around them. `npm run
 * lint:a11y` is the same code as a CLI, for CI.
 *
 * WHY IT IS A SEPARATE CHECKER. `check-tokens.mjs` asks whether a colour is
 * declared in the right place and whether the fourteen frozen category accents
 * still measure what they measured. This one asks whether the pairs the site
 * actually RENDERS clear WCAG's thresholds, and it has to resolve tokens
 * per colour mode to do it — the token guard resolves only `:root`, because a
 * category accent is the same value in both modes by construction and a role
 * token is deliberately not.
 *
 * Six checks:
 *
 *   1. Text contrast. Every foreground/background pair the site composes from
 *      role tokens is re-measured, in BOTH colour modes, against 4.5:1. Values
 *      are resolved through their var() chains, so an indirection cannot hide a
 *      colour and a comment cannot drift from the token it describes.
 *   2. Non-text contrast. The focus ring and the current-page marker are
 *      measured against every surface they can land on, against 3:1
 *      (WCAG 1.4.11).
 *   3. The banned tertiary. `#565E6E` — the docs mockup's label colour, which
 *      measures 3.02:1 on the page ground and 2.82:1 on the elevated surface —
 *      may not appear anywhere under `src/`, the token definition included.
 *   4. The focus indicator exists. A ring token per colour mode, and a
 *      `:focus-visible` rule that uses it.
 *   5. Reduced motion. A `prefers-reduced-motion: reduce` guard in the token
 *      layer, and — because a blanket rule can damp a transition but cannot
 *      undo a transform — a `transform: none` INSIDE that guard in every
 *      stylesheet that moves an element on hover.
 *   6. The quoted ratios. The token definition documents its measurements as
 *      `@ratio <mode> <fg> on <bg> = N.NN:1` assertions; each is re-derived, so
 *      retuning a token either retunes its documentation or breaks the build.
 *
 * `assertRecordSemantics()` and `assertRenderedSemantics()` are separate
 * because they run later, and against different material.
 * `assertRecordSemantics()` reads the STAGED markdown, which does not exist
 * until the pipeline has run. `assertRenderedSemantics()` reads the BUILT HTML,
 * which does not exist until `postBuild`, and it is the only one of the three
 * that sees the whole page: a heading emitted by a React component — a spec
 * card, a future index widget — is in no markdown file anywhere, so a
 * source-level outline check is structurally blind to it. That blind spot
 * shipped an h1 → h3 jump on `/docs/specs`; the two checks are kept because
 * source-level failures name a line an author can fix and fire in the dev
 * server, while the rendered pass is the one that cannot be fooled.
 *
 * CLI flags: `--report` prints the measured table; `--root <dir>` checks a
 * different website directory (scripts/check-a11y.test.mjs drives the checks
 * over deliberately broken fixtures that way); `--out <dir>` additionally
 * checks built HTML in that directory.
 */

import {existsSync, readFileSync, readdirSync, statSync} from 'node:fs';
import {basename, join, relative, sep} from 'node:path';
import {fileURLToPath} from 'node:url';

import {parseDeclarations} from './check-tokens.mjs';

const HERE = fileURLToPath(new URL('.', import.meta.url));
const DEFAULT_WEBSITE = join(HERE, '..');

/** WCAG 2.1 AA, 1.4.3 — normal text. */
const AA_TEXT = 4.5;
/** WCAG 2.1 AA, 1.4.11 — non-text contrast. */
const AA_NON_TEXT = 3;

/**
 * The docs mockup's tertiary label colour. Barred outright rather than by
 * measurement: on the page ground it would technically clear the 3:1 large-text
 * floor and on the elevated surface it would not, and a colour whose legality
 * depends on which surface it lands on is a trap. SPEC-0010 REQ "Contrast".
 */
const BANNED_LITERAL = '#565e6e';

/* -------------------------------------------------------------- colour maths */

function parseHex(hex) {
  let h = hex.replace('#', '');
  if (h.length === 3 || h.length === 4) h = [...h.slice(0, 3)].map((c) => c + c).join('');
  return [0, 2, 4].map((i) => parseInt(h.slice(i, i + 2), 16));
}

function relativeLuminance(rgb) {
  const [r, g, b] = rgb.map((v) => {
    const c = v / 255;
    return c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4;
  });
  return 0.2126 * r + 0.7152 * g + 0.0722 * b;
}

function contrast(a, b) {
  const la = relativeLuminance(parseHex(a));
  const lb = relativeLuminance(parseHex(b));
  return (Math.max(la, lb) + 0.05) / (Math.min(la, lb) + 0.05);
}

/* ------------------------------------------------------- token resolution */

/**
 * Resolve a custom property as one colour mode computes it.
 *
 * The token definition declares the role tokens twice: once at `:root` (light,
 * the secondary mode) and once under `[data-theme='dark']`. Declarations are
 * applied in source order and the dark scope is applied on top for `mode ===
 * 'dark'`, which is what the cascade does — the dark selector is more specific
 * and comes later in the same unlayered file.
 *
 * Scoped blocks that are neither (`.footer--dark`, `.chip`) are deliberately
 * NOT folded in: a pair that only exists inside one of those is stated
 * explicitly in the pair table instead, with the tokens it really composes.
 */
function makeResolver(decls, mode) {
  const vars = new Map();
  for (const d of decls) {
    if (!d.prop.startsWith('--')) continue;
    const selectors = d.selector.split(',').map((s) => s.trim());
    const atRoot = selectors.includes(':root');
    const atDark = selectors.some((s) => s === "[data-theme='dark']" || s === '[data-theme="dark"]');
    if (atRoot || (mode === 'dark' && atDark)) vars.set(d.prop, d.value);
  }
  if (mode === 'dark') {
    for (const d of decls) {
      const selectors = d.selector.split(',').map((s) => s.trim());
      if (selectors.some((s) => s === "[data-theme='dark']" || s === '[data-theme="dark"]')) {
        if (d.prop.startsWith('--')) vars.set(d.prop, d.value);
      }
    }
  }
  return function resolve(name, seen = new Set()) {
    if (seen.has(name)) return null;
    seen.add(name);
    const value = vars.get(name);
    if (!value) return null;
    if (/^#[0-9a-fA-F]{3,8}$/.test(value)) return value.toLowerCase();
    const m = value.match(/^var\(\s*(--[\w-]+)\s*\)$/);
    return m ? resolve(m[1], seen) : null;
  };
}

/* ------------------------------------------------------------- the pairs */

/**
 * Foreground/background pairs the site composes from role tokens, measured in
 * both colour modes. `why` names the surface so a failure reads as a design
 * problem rather than as two hex codes.
 */
const TEXT_PAIRS = [
  ['--cairn-text', '--cairn-bg', 'body text on the page ground'],
  ['--cairn-text', '--cairn-elevated', 'body text on a card'],
  ['--cairn-text', '--cairn-surface-2', 'body text on the second surface'],
  ['--cairn-muted', '--cairn-bg', 'muted prose on the page ground'],
  ['--cairn-muted', '--cairn-elevated', 'muted prose on a card'],
  ['--cairn-muted', '--cairn-surface-2', 'muted prose on the second surface'],
  ['--cairn-accent-label', '--cairn-bg', 'the uppercase mono label'],
  ['--cairn-accent-label', '--cairn-elevated', 'a label on a card'],
  ['--ifm-color-emphasis-900', '--cairn-elevated', 'the skip link'],
  ['--ifm-menu-color', '--cairn-bg', 'a navigation rail entry'],
  ['--ifm-menu-color-active', '--cairn-bg', 'the current rail entry'],
  ['--ifm-toc-link-color', '--cairn-bg', 'an on-this-page entry'],
];

/**
 * Pairs that pin the ink ramp in BOTH colour modes and are therefore measured
 * once: the code surface, the terminal transcript and the dark footer keep the
 * dark ramp on a light page (custom.css sections 3, 5 and 7), so following the
 * mode here would measure a composition the browser never renders.
 */
const PINNED_TEXT_PAIRS = [
  ['--cairn-code-fg', '--cairn-code-bg', 'code on the machine surface'],
  ['--cairn-ink-accent', '--cairn-code-bg', 'a link inside code'],
  ['--cairn-ink-700', '--cairn-ink-deep', 'footer text'],
  ['--cairn-ink-700', '--cairn-ink-100', 'the terminal transcript, dimmed'],
  ['--cairn-ink-900', '--cairn-ink-100', 'the terminal transcript'],
];

/**
 * Surfaces a focusable control can sit on. Both ramps appear in BOTH lists: the
 * footer and the code blocks hold the ink ramp in light mode, so a light-mode
 * focus ring has to clear 3:1 there too.
 */
const FOCUSABLE_SURFACES = [
  '--cairn-bg',
  '--cairn-elevated',
  '--cairn-surface-2',
  '--cairn-border',
  '--cairn-code-bg',
  '--cairn-ink-deep',
];

/**
 * Non-text indicators, measured against every surface above. The current-page
 * bar in the rail paints in `currentColor`, which is --ifm-menu-color-active.
 */
const NON_TEXT_INDICATORS = [
  ['--cairn-focus-ring', 'the focus indicator'],
  ['--ifm-menu-color-active', 'the current-page marker in the rail'],
];

/* ------------------------------------------------------------------- source */

function walk(dir) {
  const out = [];
  for (const entry of readdirSync(dir)) {
    const p = join(dir, entry);
    if (statSync(p).isDirectory()) out.push(...walk(p));
    else out.push(p);
  }
  return out;
}

const lineOf = (text, index) => text.slice(0, index).split('\n').length;

/* ------------------------------------------------------------------- checks */

/**
 * Run every accessibility check that reads only the site source.
 * @returns {{failures: string[], rows: object[], summary: string}}
 */
export function checkA11y({root = DEFAULT_WEBSITE} = {}) {
  const SRC = join(root, 'src');
  const TOKEN_FILE = join(SRC, 'css', 'custom.css');

  const failures = [];
  const fail = (msg) => failures.push(msg);
  const rel = (p) => relative(root, p).split(sep).join('/');

  const tokenCss = readFileSync(TOKEN_FILE, 'utf8');
  const tokenDecls = parseDeclarations(tokenCss);

  const rows = [];

  /* --------------------------------------------- 1 & 2. measured contrast */

  const measure = (mode, fgToken, bgToken, why, floor, kind) => {
    const resolve = makeResolver(tokenDecls, mode);
    const fg = resolve(fgToken);
    const bg = resolve(bgToken);
    if (!fg || !bg) {
      fail(
        `src/css/custom.css: ${!fg ? fgToken : bgToken} does not resolve to a colour in ` +
          `${mode} mode, so ${why} cannot be measured. A pair that cannot be measured is ` +
          `not a pair that passes.`,
      );
      return;
    }
    const ratio = contrast(fg, bg);
    rows.push({mode, kind, fgToken, fg, bgToken, bg, why, ratio, floor});
    if (ratio < floor) {
      fail(
        `contrast (${mode}): ${why} — ${fgToken} (${fg}) on ${bgToken} (${bg}) measures ` +
          `${ratio.toFixed(2)}:1, below ${floor}:1. SPEC-0010 REQ "Contrast".`,
      );
    }
  };

  for (const mode of ['light', 'dark']) {
    for (const [fg, bg, why] of TEXT_PAIRS) measure(mode, fg, bg, why, AA_TEXT, 'text');
    for (const [token, why] of NON_TEXT_INDICATORS) {
      for (const surface of FOCUSABLE_SURFACES) {
        measure(mode, token, surface, `${why} on ${surface}`, AA_NON_TEXT, 'non-text');
      }
    }
  }
  for (const [fg, bg, why] of PINNED_TEXT_PAIRS) {
    measure('pinned', fg, bg, why, AA_TEXT, 'text');
  }

  /* --------------------------------------------------- 3. banned tertiary */

  const stylesheetsAndSource = existsSync(SRC)
    ? walk(SRC).filter((p) => /\.(css|tsx?|jsx?|mjs)$/.test(p))
    : [];
  for (const file of stylesheetsAndSource) {
    const text = readFileSync(file, 'utf8');
    const re = new RegExp(BANNED_LITERAL, 'gi');
    let m;
    while ((m = re.exec(text))) {
      fail(
        `${rel(file)}:${lineOf(text, m.index)}: \`${m[0]}\` is the docs mockup's tertiary ` +
          `label colour — 3.02:1 on the page ground, 2.82:1 on the elevated surface. It may ` +
          `not colour text at any size; use var(--cairn-muted). SPEC-0010 REQ "Contrast".`,
      );
    }
  }

  /* ---------------------------------------------- 4. the focus indicator */

  const declaresIn = (prop, selectorTest) =>
    tokenDecls.some(
      (d) =>
        d.prop === prop &&
        d.selector
          .split(',')
          .map((s) => s.trim())
          .some(selectorTest),
    );

  if (!declaresIn('--cairn-focus-ring', (s) => s === ':root')) {
    fail(
      `src/css/custom.css: --cairn-focus-ring is not declared at :root. Every colour mode ` +
        `needs a focus ring — SPEC-0010 REQ "Keyboard Navigation & Focus Management".`,
    );
  }
  if (!declaresIn('--cairn-focus-ring', (s) => s.startsWith('[data-theme='))) {
    fail(
      `src/css/custom.css: --cairn-focus-ring is not re-declared for dark mode. The two ` +
        `ramps need different rings: the dark ring measures 2.07:1 on white.`,
    );
  }
  const focusRule = tokenDecls.some(
    (d) =>
      d.selector.includes(':focus-visible') &&
      d.prop === 'outline' &&
      d.value.includes('var(--cairn-focus-ring)'),
  );
  if (!focusRule) {
    fail(
      `src/css/custom.css: no \`:focus-visible\` rule sets \`outline\` from ` +
        `var(--cairn-focus-ring). A ring token nothing paints is not a focus indicator — ` +
        `SPEC-0010 REQ "Keyboard Navigation & Focus Management", scenario "Focus indicator ` +
        `visible".`,
    );
  }

  /* ------------------------------------------------------ 5. reduced motion */

  const REDUCED_MOTION_RE = /@media[^{]*prefers-reduced-motion\s*:\s*reduce/i;
  if (!REDUCED_MOTION_RE.test(tokenCss)) {
    fail(
      `src/css/custom.css: no \`prefers-reduced-motion: reduce\` guard. SPEC-0010 REQ ` +
        `"WCAG 2.1 AA & Semantics", scenario "Reduced motion honoured".`,
    );
  }

  /*
   * A blanket guard can collapse a transition DURATION but cannot undo the
   * transform it was animating, so a stylesheet that moves an element on hover
   * has to opt out of that movement itself. This is the check that would have
   * caught the feature tiles, which lifted 3px on hover with no guard anywhere
   * in the site.
   */
  for (const file of stylesheetsAndSource.filter((p) => p.endsWith('.css'))) {
    const css = readFileSync(file, 'utf8');
    const hoverTransforms = parseDeclarations(css).filter(
      (d) => d.prop === 'transform' && d.chain.includes(':hover') && d.value !== 'none',
    );
    if (!hoverTransforms.length) continue;
    const guard = REDUCED_MOTION_RE.test(css);
    /*
     * `d.chain` is the whole nesting stack, at-rule preludes included, so this
     * asks the question the error message claims to ask: is the neutralising
     * declaration INSIDE the reduced-motion block? A `transform: none` sitting
     * anywhere else in the file — a reset, a base state, another media query —
     * says nothing about what happens when a reader asks for less motion.
     */
    const neutralised = parseDeclarations(css).some(
      (d) =>
        d.prop === 'transform' &&
        d.value === 'none' &&
        /prefers-reduced-motion\s*:\s*reduce/i.test(d.chain),
    );
    if (!guard || !neutralised) {
      fail(
        `${rel(file)}:${hoverTransforms[0].line}: \`${hoverTransforms[0].selector}\` moves on ` +
          `hover (\`transform: ${hoverTransforms[0].value}\`) but this file has ` +
          `${guard ? 'no `transform: none` inside its' : 'no'} ` +
          `\`prefers-reduced-motion: reduce\` guard. The global guard damps the transition ` +
          `and leaves the movement — SPEC-0010 REQ "WCAG 2.1 AA & Semantics", scenario ` +
          `"Reduced motion honoured".`,
      );
    }
  }

  /* ------------------------------------------------- 6. the quoted ratios */

  /*
   * The token definition documents its own contrast measurements, and a
   * measurement in a comment is a claim that rots the moment a token is
   * retuned: nudge --ifm-color-primary a shade and every ratio written beside
   * it becomes fiction, silently, while the build stays green because the
   * checker only measures the pairs IT knows about.
   *
   * So the claims are written in a form this can read back. Each `@ratio` line
   * names a mode and two tokens, both are resolved exactly as the pair table
   * above resolves them, and the derived ratio has to be the number in the
   * comment. The documentation is then not a description of the token layer —
   * it is a test of it.
   */
  const RATIO_RE =
    /@ratio\s+(light|dark)\s+(--[\w-]+)\s+on\s+(--[\w-]+)\s*=\s*(\d+\.\d{2}):1/g;

  let quotedRatios = 0;
  for (const m of tokenCss.matchAll(RATIO_RE)) {
    const [, mode, fgToken, bgToken, claimed] = m;
    quotedRatios += 1;
    const resolve = makeResolver(tokenDecls, mode);
    const fg = resolve(fgToken);
    const bg = resolve(bgToken);
    if (!fg || !bg) {
      fail(
        `src/css/custom.css:${lineOf(tokenCss, m.index)}: the @ratio assertion names ` +
          `${!fg ? fgToken : bgToken}, which does not resolve to a colour in ${mode} mode. ` +
          `SPEC-0010 REQ "Contrast".`,
      );
      continue;
    }
    const derived = contrast(fg, bg).toFixed(2);
    if (derived !== claimed) {
      fail(
        `src/css/custom.css:${lineOf(tokenCss, m.index)}: the comment claims ${fgToken} on ` +
          `${bgToken} measures ${claimed}:1 in ${mode} mode; ${fg} on ${bg} measures ` +
          `${derived}:1. Either the token moved and the comment did not, or the comment was ` +
          `wrong to begin with. SPEC-0010 REQ "Contrast".`,
      );
    }
  }
  if (!quotedRatios) {
    fail(
      `src/css/custom.css: no @ratio assertions found. The accessibility section claims its ` +
        `measurements are re-derived by this checker; with none to re-derive, that claim is ` +
        `the thing that has drifted. SPEC-0010 REQ "Contrast".`,
    );
  }

  const worstText = Math.min(...rows.filter((r) => r.kind === 'text').map((r) => r.ratio));
  const worstNonText = Math.min(...rows.filter((r) => r.kind === 'non-text').map((r) => r.ratio));
  const summary =
    `accessibility check OK: ${rows.length} rendered colour pairs measured across both ` +
    `colour modes — worst text ${worstText.toFixed(2)}:1 (floor ${AA_TEXT}:1), worst ` +
    `non-text ${worstNonText.toFixed(2)}:1 (floor ${AA_NON_TEXT}:1); ${quotedRatios} @ratio ` +
    `assertions in the token comments re-derived; focus indicator, reduced-motion guards ` +
    `and the ${BANNED_LITERAL.toUpperCase()} ban all in place.`;

  return {failures, rows, summary};
}

/** Throw on any failure. This is what the site config calls at config load. */
export function assertA11y(options = {}) {
  const result = checkA11y(options);
  if (result.failures.length) {
    throw new Error(
      `accessibility check FAILED (${result.failures.length}):\n\n` +
        result.failures.map((f) => `  ${f}`).join('\n') +
        '\n',
    );
  }
  return result;
}

/* ------------------------------------------------------- record semantics */

/**
 * Heading order over the documentation content root — the staged record and the
 * hand-written narrative pages alike.
 *
 * This is a guard, not a rewrite. The pipeline deliberately keeps each source
 * heading where the author put it, because `remark/headings` derives the anchor
 * and `remark/toc` the on-this-page entry from exactly those nodes, so
 * renumbering a heading to satisfy a level rule would silently move an anchor
 * that a cross-reference elsewhere in the record points at. Failing the build
 * instead keeps the record's format authoritative — ADR-0014, "the record's
 * format is the thing being protected; it does not bend toward the renderer" —
 * and matches the Confirmation section's posture that drift fails the build.
 *
 * It reads markdown and only markdown, so it sees the headings an *author*
 * wrote and none of the headings a *component* emits. That is not a shortcut to
 * be fixed here — a source-level check has to be able to name a source line —
 * it is why `checkRenderedSemantics()` below exists.
 */
export function checkRecordSemantics({root = DEFAULT_WEBSITE} = {}) {
  const DOCS = join(root, 'docs');
  const failures = [];
  const rel = (p) => relative(root, p).split(sep).join('/');

  if (!existsSync(DOCS)) return {failures, files: 0};

  const pages = walk(DOCS).filter((p) => /\.mdx?$/.test(p));
  for (const file of pages) {
    const body = readFileSync(file, 'utf8').replace(/^---\r?\n[\s\S]*?\r?\n---\r?\n/, '');
    const headings = [];
    let inFence = false;
    body.split('\n').forEach((line, i) => {
      if (/^\s*(```|~~~)/.test(line)) inFence = !inFence;
      if (inFence) return;
      const m = line.match(/^(#{1,6})\s+\S/);
      if (m) headings.push({depth: m[1].length, line: i + 1, text: line.trim()});
    });

    const h1s = headings.filter((h) => h.depth === 1);
    if (h1s.length !== 1) {
      failures.push(
        `${rel(file)}: has ${h1s.length} top-level headings, expected exactly 1. A page with ` +
          `no h1 or with two is not a document — SPEC-0010 REQ "WCAG 2.1 AA & Semantics", ` +
          `scenario "Generated page heading order".`,
      );
    }
    let previous = 0;
    for (const h of headings) {
      if (previous && h.depth > previous + 1) {
        failures.push(
          `${rel(file)}:${h.line}: heading level jumps h${previous} → h${h.depth} ` +
            `(${h.text}). Skipping a level breaks the document outline — SPEC-0010 REQ ` +
            `"WCAG 2.1 AA & Semantics", scenario "Generated page heading order".`,
        );
      }
      previous = h.depth;
    }
  }

  return {failures, files: pages.length};
}

/** Throw on any failure. Called after the record has been staged. */
export function assertRecordSemantics(options = {}) {
  const result = checkRecordSemantics(options);
  if (result.failures.length) {
    throw new Error(
      `record semantics check FAILED (${result.failures.length}):\n\n` +
        result.failures.map((f) => `  ${f}`).join('\n') +
        '\n',
    );
  }
  return result;
}

/* ----------------------------------------------------- rendered semantics */

/** Heading text with its markup stripped, for an error message. */
function headingText(inner) {
  const text = inner
    .replace(/<[^>]*>/g, '')
    .replace(/&[a-z]+;|&#\d+;/gi, ' ')
    .replace(/\s+/g, ' ')
    .trim();
  return text.length > 60 ? `${text.slice(0, 57)}…` : text;
}

/**
 * Heading order over the BUILT HTML — every page the deploy would publish.
 *
 * Governing: ADR-0014, SPEC-0010 REQ "WCAG 2.1 AA & Semantics", scenario
 * "Generated page heading order".
 *
 * The scenario is about what "a generated record page" *renders*, and a record
 * page is markdown plus components: `docs/specs/index.mdx` is one h1 and an
 * `<SpecIndex />`, and every other heading on that page comes out of a `.tsx`
 * file. `checkRecordSemantics()` cannot see those and never will, so a check
 * that only reads markdown reports a clean outline for a page that ships a
 * broken one. This reads the served HTML instead, where the two sources of
 * headings have already been merged and the question is finally the same
 * question the assistive technology asks.
 *
 * It runs from `postBuild`, which is the earliest point the HTML exists. That
 * means it does not fire in `docusaurus start` — the source-level pass covers
 * the dev loop, and this one is the gate on the thing that actually deploys.
 *
 * Scripts, styles and comments are stripped first: Docusaurus inlines its
 * hydration payload and route manifest into `<script>` tags, and a heading tag
 * quoted inside serialised page data is not a heading in the document.
 */
export function checkRenderedSemantics({outDir}) {
  const failures = [];

  if (!outDir || !existsSync(outDir)) {
    return {
      failures: [
        `${outDir ?? '(no directory given)'}: there is no built output to check. A rendered ` +
          `heading-order check that silently finds nothing to read is not a check — ` +
          `SPEC-0010 REQ "WCAG 2.1 AA & Semantics".`,
      ],
      files: 0,
    };
  }

  const pages = walk(outDir).filter((p) => /\.html$/i.test(p));
  for (const file of pages) {
    const html = readFileSync(file, 'utf8')
      .replace(/<!--[\s\S]*?-->/g, ' ')
      .replace(/<script\b[^>]*>[\s\S]*?<\/script>/gi, ' ')
      .replace(/<style\b[^>]*>[\s\S]*?<\/style>/gi, ' ');

    const headings = [];
    for (const m of html.matchAll(/<h([1-6])\b[^>]*>([\s\S]*?)<\/h\1>/gi)) {
      headings.push({depth: Number(m[1]), text: headingText(m[2])});
    }

    const where = relative(outDir, file).split(sep).join('/');
    const outline = headings.map((h) => `h${h.depth}`).join(' ');

    const h1s = headings.filter((h) => h.depth === 1);
    if (h1s.length !== 1) {
      failures.push(
        `${where}: renders ${h1s.length} top-level headings, expected exactly 1 ` +
          `(outline: ${outline || 'none'}). SPEC-0010 REQ "WCAG 2.1 AA & Semantics", ` +
          `scenario "Generated page heading order".`,
      );
    }

    let previous = 0;
    for (const h of headings) {
      if (previous && h.depth > previous + 1) {
        failures.push(
          `${where}: rendered heading level jumps h${previous} → h${h.depth} ` +
            `("${h.text}"). The whole outline is ${outline}. If this page's markdown ` +
            `source looks fine, the heading came from a component — SPEC-0010 REQ ` +
            `"WCAG 2.1 AA & Semantics", scenario "Generated page heading order".`,
        );
      }
      previous = h.depth;
    }
  }

  return {failures, files: pages.length};
}

/** Throw on any failure. Called from the site's `postBuild` hook. */
export function assertRenderedSemantics(options = {}) {
  const result = checkRenderedSemantics(options);
  if (result.failures.length) {
    throw new Error(
      `rendered semantics check FAILED (${result.failures.length}):\n\n` +
        result.failures.map((f) => `  ${f}`).join('\n') +
        '\n',
    );
  }
  return result;
}

/* ---------------------------------------------------------------------- CLI */

function reportTable(rows) {
  const w = (s, n) => String(s).padEnd(n);
  console.log(`${w('mode', 8)}${w('foreground', 26)}${w('on', 22)}${'ratio'.padStart(8)}  floor`);
  for (const row of rows) {
    console.log(
      w(row.mode, 8) +
        w(`${row.fgToken} ${row.fg}`, 26) +
        w(`${row.bgToken} ${row.bg}`, 22) +
        row.ratio.toFixed(2).padStart(8) +
        `  ${row.floor}:1  ${row.why}`,
    );
  }
  console.log('');
}

function main(argv) {
  const flag = (name) => {
    const at = argv.indexOf(name);
    if (at === -1) return undefined;
    if (!argv[at + 1]) return null;
    return argv[at + 1];
  };

  const rootFlag = flag('--root');
  if (rootFlag === null) {
    console.error('check-a11y: --root needs a directory');
    return 2;
  }
  /*
   * `--out` is opt-in rather than "check `build/` if it happens to exist": a
   * stale bundle from a previous checkout would otherwise report on code that
   * is no longer here. The authoritative run of this pass is the `postBuild`
   * hook, against the directory Docusaurus has just written.
   */
  const outFlag = flag('--out');
  if (outFlag === null) {
    console.error('check-a11y: --out needs a directory');
    return 2;
  }
  const root = rootFlag ?? DEFAULT_WEBSITE;

  let result;
  let record;
  let rendered = {failures: [], files: 0};
  try {
    result = checkA11y({root});
    record = checkRecordSemantics({root});
    if (outFlag) rendered = checkRenderedSemantics({outDir: outFlag});
  } catch (err) {
    console.error(`accessibility check ERROR: ${err.message}`);
    return 2;
  }

  if (argv.includes('--report')) reportTable(result.rows);

  const failures = [...result.failures, ...record.failures, ...rendered.failures];
  if (failures.length) {
    console.error(`accessibility check FAILED (${failures.length}):\n`);
    for (const f of failures) console.error(`  ${f}`);
    console.error('');
    return 1;
  }
  console.log(
    `${result.summary} ${record.files} documentation pages carry one h1 and no skipped ` +
      `heading level.` +
      (outFlag ? ` ${rendered.files} rendered pages agree.` : ''),
  );
  return 0;
}

if (basename(process.argv[1] ?? '') === 'check-a11y.mjs') {
  process.exit(main(process.argv.slice(2)));
}
