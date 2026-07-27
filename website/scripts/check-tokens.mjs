#!/usr/bin/env node
/**
 * Token-layer guard.
 *
 * Governing: ADR-0014 (the public site is the design record),
 *            SPEC-0010 REQ "Design Token Source of Truth",
 *            SPEC-0010 REQ "Category Palette Immutability",
 *            SPEC-0010 REQ "Typographic Roles",
 *            SPEC-0010 REQ "Category Accents Are Dark-Ground Text Colours"
 *
 * WHERE THIS RUNS. design.md ("Validation runs where it can actually run")
 * decides that source-level checks run at CONFIG LOAD, so they fire on every
 * `docusaurus build` *and* every `docusaurus start` and an author finds out in
 * the dev server rather than in CI. `assertTokens()` is therefore exported and
 * called from docusaurus.config.ts, which throws before Docusaurus finishes
 * loading the config. `npm run lint:tokens` is the same code as a CLI, for CI.
 *
 * Five checks:
 *
 *   1. Colour literals. No file under website/src/ (nor the site config) other
 *      than the token definition may carry a colour literal, in a stylesheet or
 *      in an inline style. Reports file, line and literal.
 *   2. Category palette. The category NAMES are derived from ADR-0009 — the one
 *      place they are stated — and are not retyped here. Each must be declared
 *      exactly once, at `:root`, as a hex literal equal to the value that
 *      shipped. A `var()` or `color-mix()` redeclaration in any scope is a
 *      recolour and fails. Category-derived CLASS names must cover exactly that
 *      set, plus classes that fall back to the neutral default.
 *   3. Type families. Both families are declared once in the token definition
 *      with fallbacks; no other file may name a family directly.
 *   4. Dark-ground contrast. Every category accent is re-measured as foreground
 *      against the ink ramp and against the chip and badge tints composited over
 *      it, and must clear 4.5:1. Values are RESOLVED through var() chains before
 *      measuring, so an indirection cannot hide a colour. The light-ground
 *      figures are printed for the record — they are why those surfaces pin the
 *      ink ramp.
 *   5. Nothing outside the token definition declares a `--cat-*` property.
 *
 * CLI flags: `--report` prints the contrast table; `--root <dir>` checks a
 * different website directory (scripts/check-tokens.test.mjs drives the checks
 * over deliberately broken fixtures that way).
 */

import {existsSync, readFileSync, readdirSync, statSync} from 'node:fs';
import {basename, join, relative, sep} from 'node:path';
import {fileURLToPath} from 'node:url';

const HERE = fileURLToPath(new URL('.', import.meta.url));
const DEFAULT_WEBSITE = join(HERE, '..');
/* The record is the source of truth for the category NAMES, so the checker
   reads it rather than restating it. Deliberately not affected by `--root`: a
   fixture varies the site, never the record. */
const REPO = join(HERE, '..', '..');

const AA_TEXT = 4.5;

/* ------------------------------------------------------- the shipped values */

/**
 * The values the fourteen accents shipped with, keyed by category name.
 *
 * ADR-0009 states the category NAMES and no hex values, so the values have to
 * be frozen somewhere and this is the only mechanical option. The names are NOT
 * frozen here: they are read from ADR-0009 below and cross-checked against this
 * table, so a decision that changes the category set fails with an error saying
 * exactly that instead of passing silently.
 */
const SHIPPED_VALUES = {
  /* Operation kind. */
  reason: '#8b7cf6',
  exec: '#46c878',
  read: '#4f9cf9',
  net: '#e6b450',
  write: '#f0568f',
  search: '#5ed4e0',
  plan: '#c08bff',
  tool: '#56d9a0',
  analyze: '#e0a060',
  test: '#6cc8e8',
  fix: '#f08550',
  fail: '#f0506a',
  meta: '#808c9a',

  /* Workflow phase. Each repeats the hex of the operation-kind category that
     means the same thing, because ADR-0009 decides they SHARE a hue: roughly
     thirteen accents is the limit of comfortable discrimination, and ten more
     near-duplicates would make a waterfall harder to read, not easier. The
     values are repeated rather than aliased because this table's job is to
     freeze what shipped — an alias here would let a synonym's colour change
     silently when its partner changed. The stylesheet expresses the sharing. */
  research: '#5ed4e0', // = search
  implementation: '#f0568f', // = write
  review: '#e0a060', // = analyze
  testing: '#6cc8e8', // = test
  debug: '#f08550', // = fix
  build: '#46c878', // = exec
  docs: '#c8b18a', // sand — the one phase with no operation-kind synonym
  delivery: '#e6b450', // = net
  deploy: '#e6b450', // = net
  wait: '#808c9a', // = meta
  prompt: '#8b7cf6', // = reason

  /* The ADR-0009 neutral default, for any category the ADR does not name. */
  other: '#aab2bd',
};

/** The neutral default is not one of ADR-0009's recommended categories. */
const NEUTRAL = 'other';

/* --------------------------------------------------- ADR-0009 category names */

/**
 * ADR-0009 states its recommended categories as backticked, middot-separated
 * lists ("`reason · exec · read · … · meta`"), in more than one place.
 *
 * It now states TWO vocabularies, not one — operation kind and workflow phase
 * — because agents describing their own runs reach for SDLC phases at least as
 * readily as for operation kinds. Neither is a subset of the other, so the
 * older "longest list wins, everything else must be a subset" rule reported the
 * phase vocabulary as a self-contradiction and failed the build.
 *
 * So this returns the UNION of the maximal lists: every name the record
 * recommends, which is exactly the set the site must ship an accent for.
 * Phases share their synonym's hue (ADR-0009), so several of those accents
 * resolve to one colour — a decision recorded in the ADR, not duplication.
 *
 * Note this no longer catches a mistyped name here, the way the subset rule
 * did: a typo just reads as another vocabulary. That protection did not
 * disappear, it moved downstream — checkTokens cross-checks this union against
 * SHIPPED_VALUES in BOTH directions, so a name the ADR invents with no shipped
 * accent, or an accent no longer named by the ADR, still fails the build, and
 * with a message that says which. Do not re-add a subset rule here: with two
 * independent vocabularies there is no longer a single set to be a subset of.
 */
export function adr0009Categories(repo = REPO) {
  const dir = join(repo, 'docs', 'adrs');
  const file = existsSync(dir)
    ? readdirSync(dir).find((f) => f.startsWith('ADR-0009-') && f.endsWith('.md'))
    : undefined;
  if (!file) {
    throw new Error(
      `cannot find docs/adrs/ADR-0009-*.md under ${repo}. The category names are derived ` +
        `from that decision record and must not be retyped in the checker.`,
    );
  }
  const text = readFileSync(join(dir, file), 'utf8');
  const lists = [];
  for (const m of text.matchAll(/`([^`]+)`/g)) {
    const items = m[1].replace(/\s+/g, ' ').trim().split(' · ');
    if (items.length >= 3 && items.every((i) => /^[a-z]+$/.test(i))) lists.push(items);
  }
  if (!lists.length) {
    throw new Error(
      `no "a · b · c" category list found in docs/adrs/${file}. The parser and the ADR have ` +
        `drifted; fix the parser rather than hardcoding the names.`,
    );
  }
  /* The vocabularies are the MAXIMAL lists — those no longer list contains.
     Everything else the ADR quotes (the original five, in its consequences) is
     a subset of one of them and contributes no new name. */
  const containedByAnother = (list, i) =>
    lists.some(
      (other, j) => j !== i && other.length > list.length && list.every((x) => other.includes(x)),
    );
  const recommended = [...new Set(lists.filter((l, i) => !containedByAnother(l, i)).flat())];

  if (!recommended.length) {
    throw new Error(`docs/adrs/${file} yielded no category names. The parser and the ADR have drifted.`);
  }
  return recommended;
}

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

/** srgb equivalent of `color-mix(in srgb, fg pct%, bg)`. */
function mix(fg, bg, pct) {
  const f = parseHex(fg);
  const b = parseHex(bg);
  const t = pct / 100;
  const out = f.map((v, i) => Math.round(v * t + b[i] * (1 - t)));
  return '#' + out.map((v) => v.toString(16).padStart(2, '0')).join('');
}

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

/**
 * Blank out comment bodies, preserving every byte offset and newline so line
 * numbers still hold. The previous shape of this — "skip any line whose trimmed
 * text starts with `*` or `/*`" — also swallowed the universal selector and any
 * rule sharing a line with a leading comment.
 *
 * Quote tracking is enabled only where line comments are (JS/TS), because a
 * `//` inside a string is not a comment and CSS has no line comments at all:
 * `@import url('https://…')` must survive untouched.
 */
function maskComments(text, {lineComments}) {
  const out = [...text];
  let i = 0;
  let state = 'code';
  let quote = '';
  while (i < text.length) {
    const c = text[i];
    const n = text[i + 1];
    if (state === 'code') {
      if (c === '/' && n === '*') {
        out[i] = out[i + 1] = ' ';
        i += 2;
        state = 'block';
      } else if (lineComments && c === '/' && n === '/') {
        out[i] = out[i + 1] = ' ';
        i += 2;
        state = 'line';
      } else if (lineComments && (c === '"' || c === "'" || c === '`')) {
        quote = c;
        i += 1;
        state = 'string';
      } else {
        i += 1;
      }
    } else if (state === 'block') {
      if (c === '*' && n === '/') {
        out[i] = out[i + 1] = ' ';
        i += 2;
        state = 'code';
      } else {
        if (c !== '\n') out[i] = ' ';
        i += 1;
      }
    } else if (state === 'line') {
      if (c === '\n') state = 'code';
      else out[i] = ' ';
      i += 1;
    } else {
      if (c === '\\') i += 2;
      else {
        if (c === quote) state = 'code';
        i += 1;
      }
    }
  }
  return out.join('');
}

const lineOf = (text, index) => text.slice(0, index).split('\n').length;

/**
 * Every declaration in a stylesheet, with the selector it sits under and its
 * line. Tolerant rather than complete: enough to answer "what is declared, to
 * what, and where" without pulling a CSS parser into the build.
 */
export function parseDeclarations(css) {
  const src = maskComments(css, {lineComments: false});
  const decls = [];
  const stack = [];
  let buf = '';
  const flush = (end) => {
    const raw = buf.trim();
    buf = '';
    if (!raw || !stack.length) return;
    const m = raw.match(/^([-\w]+)\s*:\s*([\s\S]+)$/);
    if (!m) return;
    decls.push({
      selector: stack[stack.length - 1],
      chain: stack.join(' '),
      prop: m[1],
      value: m[2].replace(/\s+/g, ' ').trim(),
      line: lineOf(css, end - raw.length),
    });
  };
  for (let i = 0; i < src.length; i++) {
    const ch = src[i];
    if (ch === '{') {
      stack.push(buf.trim().replace(/\s+/g, ' '));
      buf = '';
    } else if (ch === '}') {
      flush(i);
      stack.pop();
    } else if (ch === ';') {
      flush(i);
    } else {
      buf += ch;
    }
  }
  return decls;
}

/* ------------------------------------------------------- colour literal scan */

/* The full CSS named-colour set. The previous 22-word list let `crimson`,
   `indigo`, `tomato`, `gold`, `slategray` and ~120 others through. */
const NAMED_COLOURS = [
  'aliceblue', 'antiquewhite', 'aqua', 'aquamarine', 'azure', 'beige', 'bisque', 'black',
  'blanchedalmond', 'blue', 'blueviolet', 'brown', 'burlywood', 'cadetblue', 'chartreuse',
  'chocolate', 'coral', 'cornflowerblue', 'cornsilk', 'crimson', 'cyan', 'darkblue',
  'darkcyan', 'darkgoldenrod', 'darkgray', 'darkgreen', 'darkgrey', 'darkkhaki',
  'darkmagenta', 'darkolivegreen', 'darkorange', 'darkorchid', 'darkred', 'darksalmon',
  'darkseagreen', 'darkslateblue', 'darkslategray', 'darkslategrey', 'darkturquoise',
  'darkviolet', 'deeppink', 'deepskyblue', 'dimgray', 'dimgrey', 'dodgerblue', 'firebrick',
  'floralwhite', 'forestgreen', 'fuchsia', 'gainsboro', 'ghostwhite', 'gold', 'goldenrod',
  'gray', 'green', 'greenyellow', 'grey', 'honeydew', 'hotpink', 'indianred', 'indigo',
  'ivory', 'khaki', 'lavender', 'lavenderblush', 'lawngreen', 'lemonchiffon', 'lightblue',
  'lightcoral', 'lightcyan', 'lightgoldenrodyellow', 'lightgray', 'lightgreen', 'lightgrey',
  'lightpink', 'lightsalmon', 'lightseagreen', 'lightskyblue', 'lightslategray',
  'lightslategrey', 'lightsteelblue', 'lightyellow', 'lime', 'limegreen', 'linen', 'magenta',
  'maroon', 'mediumaquamarine', 'mediumblue', 'mediumorchid', 'mediumpurple',
  'mediumseagreen', 'mediumslateblue', 'mediumspringgreen', 'mediumturquoise',
  'mediumvioletred', 'midnightblue', 'mintcream', 'mistyrose', 'moccasin', 'navajowhite',
  'navy', 'oldlace', 'olive', 'olivedrab', 'orange', 'orangered', 'orchid', 'palegoldenrod',
  'palegreen', 'paleturquoise', 'palevioletred', 'papayawhip', 'peachpuff', 'peru', 'pink',
  'plum', 'powderblue', 'purple', 'rebeccapurple', 'red', 'rosybrown', 'royalblue',
  'saddlebrown', 'salmon', 'sandybrown', 'seagreen', 'seashell', 'sienna', 'silver',
  'skyblue', 'slateblue', 'slategray', 'slategrey', 'snow', 'springgreen', 'steelblue',
  'tan', 'teal', 'thistle', 'tomato', 'turquoise', 'violet', 'wheat', 'white', 'whitesmoke',
  'yellow', 'yellowgreen',
];

/* 3, 4, 6 or 8 hex digits and nothing else. The previous `{3}(?:{1,5})?` shape
   also matched five- and seven-digit runs, so a legitimate heading anchor like
   `href="#added"` failed the build. */
const HEX_RE = /#(?:[0-9a-fA-F]{8}|[0-9a-fA-F]{6}|[0-9a-fA-F]{3,4})\b/g;
/* `color(` is CSS's colour-space function; `color-mix(` is how the token layer
   composites and is allowed, hence the negative lookahead. */
const FUNC_RE = /\b(?:rgba?|hsla?|hwb|lab|lch|oklab|oklch|color(?!-mix))\s*\(/g;
const NAMED_RE = new RegExp(`(?<![\\w-])(?:${NAMED_COLOURS.join('|')})(?![\\w-])`, 'gi');

/* An anchor, a path or an id is not a colour. */
const LINK_CONTEXT_RE = /(?:href|xlink:href|to|src|id|name|hash|anchor)\s*[:=]\s*\{?\s*["'`]$/i;

/** Is this hex match an anchor/URL fragment rather than a colour? */
function isFragment(text, index) {
  const from = Math.max(0, index - 200);
  const before = text.slice(from, index);
  if (LINK_CONTEXT_RE.test(before)) return true;
  if (/url\(\s*["']?$/i.test(before)) return true;
  /* Inside a quoted string that also contains a slash: a URL or a path, so the
     `#` is a fragment. `#dead` in `href="/docs/x#dead"` is not a colour. */
  const dq = before.lastIndexOf('"');
  const sq = before.lastIndexOf("'");
  const nearest = Math.max(dq, sq);
  if (nearest !== -1) {
    const quote = dq > sq ? '"' : "'";
    const open = from + nearest;
    const close = text.indexOf(quote, open + 1);
    if (close > index && text.slice(open + 1, close).includes('/')) return true;
  }
  return false;
}

/** Property names whose value is a colour, for inline styles in JS/TS. */
const COLOUR_PROP_RE = new RegExp(
  '^(?:[a-zA-Z]*[cC]olor|background|backgroundImage|background-image|fill|stroke|boxShadow' +
    '|box-shadow|textShadow|text-shadow|outline|border|border(?:Top|Right|Bottom|Left)' +
    '|border-(?:top|right|bottom|left))$',
);

/**
 * Named colours in a declaration value. Hex literals and colour functions are
 * found by the whole-file pass instead — reporting them here too would name the
 * same literal twice. A colour NAME, though, is only a colour in value position:
 * a class called `.tan` or `.snow` is not a finding.
 */
function scanNamed(value) {
  const hits = [];
  NAMED_RE.lastIndex = 0;
  let m;
  while ((m = NAMED_RE.exec(value))) hits.push({name: 'named colour', literal: m[0]});
  return hits;
}

/**
 * Resolve a `:root`-scope token through its var() chain to a hex literal, so
 * the contrast check measures what the browser computes rather than only what
 * happens to be written as a literal.
 */
function makeResolver(decls) {
  const root = new Map();
  for (const d of decls) {
    if (d.prop.startsWith('--') && /(^|,\s*):root$/.test(d.selector)) root.set(d.prop, d.value);
  }
  return function resolve(name, seen = new Set()) {
    if (seen.has(name)) return null;
    seen.add(name);
    const value = root.get(name);
    if (!value) return null;
    if (/^#[0-9a-fA-F]{3,8}$/.test(value)) return value.toLowerCase();
    const m = value.match(/^var\(\s*(--[\w-]+)\s*\)$/);
    return m ? resolve(m[1], seen) : null;
  };
}

/* ------------------------------------------------------------------- checks */

/**
 * Run every token-layer check over a website directory.
 * @returns {{failures: string[], rows: object[], summary: string, scanned: number}}
 */
export function checkTokens({root = DEFAULT_WEBSITE, repo = REPO} = {}) {
  const SRC = join(root, 'src');
  const TOKEN_FILE = join(SRC, 'css', 'custom.css');
  const CONFIG = join(root, 'docusaurus.config.ts');

  const failures = [];
  const fail = (msg) => failures.push(msg);
  const rel = (p) => relative(root, p).split(sep).join('/');

  const categories = adr0009Categories(repo);
  const all = [...categories, NEUTRAL];
  const expected = new Map(); // --cat-name -> shipped hex
  for (const name of all) {
    const value = SHIPPED_VALUES[name];
    if (!value) {
      fail(
        `ADR-0009 names the category "${name}" but the shipped-value table in ` +
          `scripts/check-tokens.mjs has no entry for it. Changing the category set is a ` +
          `decision against ADR-0009: land the ADR and this table together.`,
      );
      continue;
    }
    expected.set(`--cat-${name}`, value);
  }
  for (const name of Object.keys(SHIPPED_VALUES)) {
    if (name !== NEUTRAL && !categories.includes(name)) {
      fail(
        `scripts/check-tokens.mjs freezes a value for "${name}", which ADR-0009 no longer ` +
          `names. Removing a category is a decision against ADR-0009.`,
      );
    }
  }

  const tokenCss = readFileSync(TOKEN_FILE, 'utf8');
  const tokenDecls = parseDeclarations(tokenCss);
  const resolve = makeResolver(tokenDecls);

  /* -------------------------------------------- 1. colour literals in source */

  const SCANNED_EXTENSIONS = ['.css', '.ts', '.tsx', '.js', '.jsx', '.mjs'];
  const scannable = [
    ...walk(SRC).filter((p) => p !== TOKEN_FILE && SCANNED_EXTENSIONS.some((e) => p.endsWith(e))),
    /* The site config is where the prism `plain` pair lives — the one colour
       outside src/ that reaches a rendered surface. */
    ...(existsSync(CONFIG) ? [CONFIG] : []),
  ];

  for (const file of scannable) {
    const name = rel(file);
    const isCss = file.endsWith('.css');
    const raw = readFileSync(file, 'utf8');
    const text = maskComments(raw, {lineComments: !isCss});

    if (isCss) {
      for (const d of parseDeclarations(raw)) {
        for (const hit of scanNamed(d.value)) {
          fail(
            `${name}:${d.line}: ${hit.name} literal \`${hit.literal}\` — ` +
              `use a token from src/css/custom.css`,
          );
        }
      }
    } else {
      /* Inline styles: `{color: 'black'}` is exactly what REQ "Design Token
         Source of Truth" means by "and inline style". A bare English "black" in
         a sentence on the homepage is not, which is why this is keyed on a
         colour-valued property name rather than on the word. */
      for (const m of text.matchAll(/([A-Za-z-]+)\s*:\s*(["'`])((?:\\.|(?!\2).)*)\2/g)) {
        if (!COLOUR_PROP_RE.test(m[1])) continue;
        for (const hit of scanNamed(m[3])) {
          fail(
            `${name}:${lineOf(text, m.index)}: ${hit.name} literal \`${hit.literal}\` in an ` +
              `inline style — use a token from src/css/custom.css`,
          );
        }
      }
    }

    /* Hex and colour functions anywhere in the file, wherever they hide. */
    for (const [label, re] of [
      ['hex colour', HEX_RE],
      ['colour function', FUNC_RE],
    ]) {
      re.lastIndex = 0;
      let m;
      while ((m = re.exec(text))) {
        if (re === HEX_RE && isFragment(text, m.index)) continue;
        fail(
          `${name}:${lineOf(text, m.index)}: ${label} literal \`${m[0]}\` — ` +
            `use a token from src/css/custom.css`,
        );
      }
    }

    /* 5. Only the token definition may DECLARE a category property. */
    for (const m of text.matchAll(/(--cat-[\w-]+)\s*:/g)) {
      fail(
        `${name}:${lineOf(text, m.index)}: declares ${m[1]}. The category tokens are owned ` +
          `by src/css/custom.css — SPEC-0010 REQ "Category Palette Immutability".`,
      );
    }
  }

  /* ----------------------------------------------- 2. category palette fixed */

  const seen = new Map(); // --cat-* -> [declaration]
  for (const d of tokenDecls) {
    if (!d.prop.startsWith('--cat-')) continue;
    if (!seen.has(d.prop)) seen.set(d.prop, []);
    seen.get(d.prop).push(d);
  }

  for (const [token, hex] of expected) {
    const found = seen.get(token) ?? [];
    if (!found.length) {
      fail(`src/css/custom.css: ADR-0009 token ${token} is missing`);
      continue;
    }
    if (found.length > 1) {
      fail(
        `src/css/custom.css:${found.map((d) => d.line).join(', ')}: ${token} is declared ` +
          `${found.length} times. A category token is declared once, at :root, and is ` +
          `constant across colour modes — SPEC-0010 REQ "Category Palette Immutability".`,
      );
    }
    for (const d of found) {
      if (!/(^|,\s*):root$/.test(d.selector)) {
        fail(
          `src/css/custom.css:${d.line}: ${token} is redeclared under \`${d.selector}\`. ` +
            `Re-pointing a category token in a scope is a recolour.`,
        );
      }
      const value = d.value.toLowerCase();
      if (!/^#[0-9a-fA-F]{3,8}$/.test(value)) {
        fail(
          `src/css/custom.css:${d.line}: ${token} is \`${d.value}\`, not a hex literal. A ` +
            `category token names a fixed colour; an indirection can be re-pointed and is a ` +
            `recolour — SPEC-0010 REQ "Category Palette Immutability".`,
        );
      } else if (value !== hex) {
        fail(
          `src/css/custom.css:${d.line}: ${token} is ${value} but shipped as ${hex}. ` +
            `Recolouring a category is a decision against ADR-0009, not a website change.`,
        );
      }
    }
  }

  for (const [token, found] of seen) {
    if (!expected.has(token)) {
      fail(
        `src/css/custom.css:${found[0].line}: ${token} is not an ADR-0009 category token. ` +
          `Adding a category is a decision against ADR-0009.`,
      );
    }
  }

  /* Category-derived CLASS names. The record names the categories once, so
     every place the site turns a category into a class must cover exactly that
     set — plus classes resolving to the neutral default, which is how a string
     ADR-0009 does not define is allowed to exist (REQ "Category Palette
     Immutability", scenario "Unrecognised category class"). */
  const classSets = [
    {file: TOKEN_FILE, re: /^\.chip-([\w-]+)$/, prop: '--chip-accent', what: 'chip'},
    {
      file: join(SRC, 'components', 'HomepageFeatures', 'styles.module.css'),
      re: /^\.cat_([\w-]+)$/,
      prop: '--accent',
      what: 'tile',
    },
    {
      file: join(SRC, 'components', 'homepage', 'styles.module.css'),
      re: /^\.cat_([\w-]+)$/,
      prop: '--accent',
      what: 'section',
    },
  ];
  for (const set of classSets) {
    if (!existsSync(set.file)) continue;
    const found = new Map();
    for (const d of parseDeclarations(readFileSync(set.file, 'utf8'))) {
      if (d.prop !== set.prop) continue;
      for (const sel of d.selector.split(',').map((s) => s.trim())) {
        const m = sel.match(set.re);
        if (m) found.set(m[1], d);
      }
    }
    for (const name of all) {
      if (!found.has(name)) {
        fail(
          `${rel(set.file)}: no ${set.what} rule for the ADR-0009 category "${name}". Every ` +
            `category the record names must have one.`,
        );
      }
    }
    for (const [name, d] of found) {
      const want = all.includes(name) ? `var(--cat-${name})` : `var(--cat-${NEUTRAL})`;
      if (d.value === want) continue;
      fail(
        all.includes(name)
          ? `${rel(set.file)}:${d.line}: ${set.what} "${name}" sets ${set.prop} to ` +
            `\`${d.value}\` rather than ${want}.`
          : `${rel(set.file)}:${d.line}: ${set.what} "${name}" is not an ADR-0009 category, ` +
            `so it must resolve to ${want} rather than \`${d.value}\` — SPEC-0010 REQ ` +
            `"Category Palette Immutability", scenario "Unrecognised category class".`,
      );
    }
  }

  /* -------------------------------------------------- 3. type families once */

  for (const [role, needle] of [
    ['--cairn-font-sans', "'IBM Plex Sans'"],
    ['--cairn-font-mono', "'JetBrains Mono'"],
  ]) {
    const decl = tokenDecls.filter((d) => d.prop === role);
    if (!decl.length) {
      fail(`src/css/custom.css: ${role} is not declared`);
      continue;
    }
    if (decl.length > 1) {
      fail(`src/css/custom.css: ${role} is declared ${decl.length} times; declare it once`);
    }
    for (const d of decl) {
      if (!d.value.includes(needle)) {
        fail(`src/css/custom.css:${d.line}: ${role} no longer names ${needle}`);
      }
      if (d.value.split(',').length < 3) {
        fail(`src/css/custom.css:${d.line}: ${role} needs system fallbacks after ${needle}`);
      }
    }
  }

  /* `font-family: inherit` and the other CSS-wide keywords name no family and
     are legitimate; anything else outside the token definition must go through
     a role token. */
  const FAMILY_KEYWORDS = new Set(['inherit', 'initial', 'unset', 'revert', 'revert-layer']);
  for (const file of scannable) {
    if (!file.endsWith('.css')) continue;
    for (const d of parseDeclarations(readFileSync(file, 'utf8'))) {
      if (d.prop !== 'font-family' && d.prop !== 'font') continue;
      if (d.value.includes('var(')) continue;
      if (FAMILY_KEYWORDS.has(d.value.toLowerCase())) continue;
      fail(
        `${rel(file)}:${d.line}: ${d.prop} names a family directly (\`${d.value}\`) — ` +
          `use var(--cairn-font-sans) or var(--cairn-font-mono)`,
      );
    }
  }

  /* -------------------------- 4. accents are dark-ground text colours */

  const INK = ['--cairn-ink-0', '--cairn-ink-100', '--cairn-ink-200'];
  const ink = INK.map((t) => resolve(t));
  INK.forEach((t, i) => {
    if (!ink[i]) fail(`src/css/custom.css: ${t} does not resolve to a colour`);
  });

  /* The composited grounds an accent is actually measured against: the chips'
     12 % tint (custom.css `.chip`) and the homepage type badge's 13 % tint
     (HomepageFeatures/styles.module.css `.code`), both over --cairn-ink-100. */
  const TINTS = [12, 13];
  const white = resolve('--cairn-paper-0');

  const rows = [];
  if (ink.every(Boolean)) {
    for (const [token, hex] of expected) {
      const declared = seen.get(token);
      /* Measure the RESOLVED value, so the contrast check and the palette check
         cannot disagree about what a token actually is. */
      const actual = resolve(token) ?? (declared ? declared[0].value.toLowerCase() : hex);
      if (!/^#[0-9a-fA-F]{3,8}$/.test(actual)) continue;
      const onInk = ink.map((g) => contrast(actual, g));
      const onTint = TINTS.map((p) => contrast(actual, mix(actual, ink[1], p)));
      const onWhite = white ? contrast(actual, white) : null;
      rows.push({token, hex: actual, onInk, onTint, onWhite});

      INK.forEach((t, i) => {
        if (onInk[i] < AA_TEXT) {
          fail(
            `contrast: ${token} (${actual}) measures ${onInk[i].toFixed(2)}:1 on ${t} ` +
              `(${ink[i]}), below ${AA_TEXT}:1`,
          );
        }
      });
      TINTS.forEach((p, i) => {
        if (onTint[i] < AA_TEXT) {
          fail(
            `contrast: ${token} (${actual}) measures ${onTint[i].toFixed(2)}:1 on its own ` +
              `${p}% tint over --cairn-ink-100, below ${AA_TEXT}:1`,
          );
        }
      });
    }
  }

  const summary =
    `token-layer check OK: ${expected.size} category tokens (${categories.length} ADR-0009 ` +
    `categories + the neutral default) at their shipped values, ${scannable.length} source ` +
    `files free of colour literals, every accent >= ${AA_TEXT}:1 on the ink ramp.`;

  return {failures, rows, summary, scanned: scannable.length, categories};
}

/** Throw on any failure. This is what the site config calls at config load. */
export function assertTokens(options = {}) {
  const result = checkTokens(options);
  if (result.failures.length) {
    throw new Error(
      `token-layer check FAILED (${result.failures.length}):\n\n` +
        result.failures.map((f) => `  ${f}`).join('\n') +
        '\n',
    );
  }
  return result;
}

/* ---------------------------------------------------------------------- CLI */

function reportTable(rows) {
  const w = (s, n) => String(s).padEnd(n);
  const r = (n) => n.toFixed(2).padStart(8);
  console.log(
    `${w('token', 15)}${w('hex', 10)}${'ink-0'.padStart(8)}${'ink-100'.padStart(8)}` +
      `${'ink-200'.padStart(8)}${'12%tint'.padStart(8)}${'13%tint'.padStart(8)}` +
      `${'white'.padStart(8)}`,
  );
  for (const row of rows) {
    console.log(
      w(row.token, 15) +
        w(row.hex, 10) +
        row.onInk.map(r).join('') +
        row.onTint.map(r).join('') +
        (row.onWhite === null ? '       -' : r(row.onWhite)),
    );
  }
  const mins = (pick) => Math.min(...rows.map(pick));
  console.log(
    `\nworst on the ink ramp        ${mins((x) => Math.min(...x.onInk)).toFixed(2)}:1  ` +
      `(floor ${AA_TEXT}:1)`,
  );
  console.log(
    `worst on a composited tint   ${mins((x) => Math.min(...x.onTint)).toFixed(2)}:1  ` +
      `(floor ${AA_TEXT}:1)`,
  );
  const light = rows.filter((x) => x.onWhite !== null).map((x) => x.onWhite);
  if (light.length) {
    console.log(
      `best on a white ground       ${Math.max(...light).toFixed(2)}:1  ` +
        `— ${light.filter((v) => v >= AA_TEXT).length}/${light.length} clear ${AA_TEXT}:1, ` +
        `${light.filter((v) => v >= 3).length}/${light.length} clear 3:1. This is why every ` +
        `accent-as-text surface pins the ink ramp.`,
    );
  }
  console.log('');
}

function main(argv) {
  const rootFlag = argv.indexOf('--root');
  if (rootFlag !== -1 && !argv[rootFlag + 1]) {
    console.error('check-tokens: --root needs a directory');
    return 2;
  }
  const root = rootFlag === -1 ? DEFAULT_WEBSITE : argv[rootFlag + 1];

  let result;
  try {
    result = checkTokens({root});
  } catch (err) {
    console.error(`token-layer check ERROR: ${err.message}`);
    return 2;
  }

  if (argv.includes('--report')) reportTable(result.rows);

  if (result.failures.length) {
    console.error(`token-layer check FAILED (${result.failures.length}):\n`);
    for (const f of result.failures) console.error(`  ${f}`);
    console.error('');
    return 1;
  }
  console.log(result.summary);
  return 0;
}

if (basename(process.argv[1] ?? '') === 'check-tokens.mjs') {
  process.exit(main(process.argv.slice(2)));
}
