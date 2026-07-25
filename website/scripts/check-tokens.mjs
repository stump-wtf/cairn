#!/usr/bin/env node
/**
 * Token-layer guard. Run by `npm run lint:tokens`, which `npm run build` runs
 * first, so every failure below fails the build.
 *
 * Governing: ADR-0014 (the public site is the design record),
 *            SPEC-0010 REQ "Design Token Source of Truth",
 *            SPEC-0010 REQ "Category Palette Immutability",
 *            SPEC-0010 REQ "Typographic Roles",
 *            SPEC-0010 REQ "Category Accents Are Dark-Ground Text Colours"
 *
 * Four checks:
 *
 *   1. Colour literals. No file under website/src/ other than the token
 *      definition may carry a colour literal. Reports file, line and literal.
 *   2. Category palette. The fourteen ADR-0009 tokens must be present, at their
 *      shipped values, with nothing added or removed.
 *   3. Type families. Both families are declared once in the token definition;
 *      no other file under website/src/ may name a family directly.
 *   4. Dark-ground contrast. Every category accent is re-measured as foreground
 *      against the ink ramp and against the chip and badge tints composited
 *      over it, and must clear 4.5:1. The light-ground figures are printed for
 *      the record — they are why those surfaces pin the ink ramp.
 *
 * `--report` prints the contrast table without changing the exit status logic.
 * `--root <dir>` checks a different website directory; scripts/check-tokens.test.mjs
 * uses it to drive the checks over deliberately broken fixtures.
 */

import {readFileSync, readdirSync, statSync} from 'node:fs';
import {join, relative, sep} from 'node:path';
import {fileURLToPath} from 'node:url';

const rootFlag = process.argv.indexOf('--root');
const WEBSITE =
  rootFlag === -1
    ? join(fileURLToPath(new URL('.', import.meta.url)), '..')
    : process.argv[rootFlag + 1];
const SRC = join(WEBSITE, 'src');
const TOKEN_FILE = join(SRC, 'css', 'custom.css');

/* ---------------------------------------------------------------- expected */

// ADR-0009's recommended set plus the neutral default, at their shipped values.
// Changing this table is a decision against ADR-0009, not a website change.
const CATEGORY_TOKENS = {
  '--cat-reason': '#8b7cf6',
  '--cat-exec': '#46c878',
  '--cat-read': '#4f9cf9',
  '--cat-net': '#e6b450',
  '--cat-write': '#f0568f',
  '--cat-search': '#5ed4e0',
  '--cat-plan': '#c08bff',
  '--cat-tool': '#56d9a0',
  '--cat-analyze': '#e0a060',
  '--cat-test': '#6cc8e8',
  '--cat-fix': '#f08550',
  '--cat-fail': '#f0506a',
  '--cat-meta': '#808c9a',
  '--cat-other': '#aab2bd',
};

const AA_TEXT = 4.5;

/* -------------------------------------------------------------- colour maths */

function parseHex(hex) {
  let h = hex.replace('#', '');
  if (h.length === 3) h = [...h].map((c) => c + c).join('');
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

/* ------------------------------------------------------------------- walking */

function walk(dir) {
  const out = [];
  for (const entry of readdirSync(dir)) {
    const p = join(dir, entry);
    if (statSync(p).isDirectory()) out.push(...walk(p));
    else out.push(p);
  }
  return out;
}

/* --------------------------------------------------------------------- parse */

const tokenCss = readFileSync(TOKEN_FILE, 'utf8');

/** Literal-valued custom properties declared in the token definition. */
function literalTokens(css) {
  const found = new Map();
  const re = /(--[a-z0-9-]+)\s*:\s*(#[0-9a-fA-F]{3,8})\s*;/g;
  let m;
  while ((m = re.exec(css))) found.set(m[1], m[2].toLowerCase());
  return found;
}

const declared = literalTokens(tokenCss);

const failures = [];
const fail = (msg) => failures.push(msg);

/* -------------------------------------------- 1. colour literals outside the
                                                  token definition            */

// `transparent` and `currentColor` carry no palette information, so they are
// not literals for this purpose. Everything that names or numbers a colour is.
const LITERAL_PATTERNS = [
  {name: 'hex colour', re: /#[0-9a-fA-F]{3}(?:[0-9a-fA-F]{1,5})?\b/g},
  // `color(` is CSS's colour-space function; `color-mix(` is how the token
  // layer composites and is allowed, hence the negative lookahead.
  {name: 'colour function', re: /\b(?:rgba?|hsla?|hwb|lab|lch|oklab|oklch|color(?!-mix))\s*\(/g},
  {
    name: 'named colour',
    // CSS only — in a .tsx file these are ordinary English words, and a
    // sentence on the homepage containing "black" must not fail the build.
    cssOnly: true,
    re: /(?<![\w-])(?:black|white|red|green|blue|yellow|orange|purple|pink|grey|gray|cyan|magenta|silver|maroon|navy|olive|teal|lime|aqua|fuchsia)(?![\w-])/g,
  },
];

// The mask data-URI SVGs in the token definition and any icon markup use
// attribute syntax that is not a colour declaration; the scan is deliberately
// restricted to source under src/ that is not the token definition, and skips
// lines that are entirely a comment.
const SCANNED_EXTENSIONS = ['.css', '.ts', '.tsx', '.js', '.jsx', '.mjs'];

const scannable = walk(SRC).filter(
  (p) => p !== TOKEN_FILE && SCANNED_EXTENSIONS.some((e) => p.endsWith(e)),
);

for (const file of scannable) {
  const rel = relative(WEBSITE, file).split(sep).join('/');
  const lines = readFileSync(file, 'utf8').split('\n');
  lines.forEach((line, i) => {
    const trimmed = line.trim();
    if (trimmed.startsWith('*') || trimmed.startsWith('//') || trimmed.startsWith('/*')) return;
    for (const {name, re, cssOnly} of LITERAL_PATTERNS) {
      if (cssOnly && !file.endsWith('.css')) continue;
      re.lastIndex = 0;
      let m;
      while ((m = re.exec(line))) {
        fail(`${rel}:${i + 1}: ${name} literal \`${m[0]}\` — use a token from src/css/custom.css`);
      }
    }
  });
}

/* ------------------------------------------------- 2. category palette fixed */

for (const [token, expected] of Object.entries(CATEGORY_TOKENS)) {
  const actual = declared.get(token);
  if (!actual) {
    fail(`src/css/custom.css: ADR-0009 token ${token} is missing`);
  } else if (actual !== expected) {
    fail(
      `src/css/custom.css: ${token} is ${actual} but ADR-0009 ships ${expected}. ` +
        `Recolouring a category is a decision against ADR-0009, not a website change.`,
    );
  }
}

for (const token of declared.keys()) {
  if (token.startsWith('--cat-') && !(token in CATEGORY_TOKENS)) {
    fail(`src/css/custom.css: ${token} is not an ADR-0009 category token`);
  }
}

/* -------------------------------------------------- 3. type families once */

for (const family of ['--cairn-font-sans', '--cairn-font-mono']) {
  if (!new RegExp(`${family}\\s*:`).test(tokenCss)) {
    fail(`src/css/custom.css: ${family} is not declared`);
  }
}
for (const [role, needle] of [
  ['--cairn-font-sans', "'IBM Plex Sans'"],
  ['--cairn-font-mono', "'JetBrains Mono'"],
]) {
  const decl = tokenCss.match(new RegExp(`${role}\\s*:([^;]+);`));
  if (decl && !decl[1].includes(needle)) {
    fail(`src/css/custom.css: ${role} no longer names ${needle}`);
  }
  if (decl && decl[1].split(',').length < 3) {
    fail(`src/css/custom.css: ${role} needs system fallbacks after ${needle}`);
  }
}
for (const file of scannable) {
  const rel = relative(WEBSITE, file).split(sep).join('/');
  readFileSync(file, 'utf8')
    .split('\n')
    .forEach((line, i) => {
      const m = line.match(/font-family\s*:\s*([^;]+)/);
      if (m && !m[1].includes('var(')) {
        fail(
          `${rel}:${i + 1}: font-family names a family directly (\`${m[1].trim()}\`) — ` +
            `use var(--cairn-font-sans) or var(--cairn-font-mono)`,
        );
      }
    });
}

/* ------------------------------- 4. accents are dark-ground text colours */

const INK = ['--cairn-ink-0', '--cairn-ink-100', '--cairn-ink-200'];
for (const t of INK) {
  if (!declared.has(t)) fail(`src/css/custom.css: ${t} is not declared as a literal`);
}

// The composited grounds an accent is actually measured against: the chips'
// 12 % tint (custom.css `.chip`) and the homepage type badge's 13 % tint
// (HomepageFeatures/styles.module.css `.code`), both over --cairn-ink-100.
const TINTS = [12, 13];

const rows = [];
if (INK.every((t) => declared.has(t))) {
  for (const [token, hex] of Object.entries(CATEGORY_TOKENS)) {
    const onInk = INK.map((t) => contrast(hex, declared.get(t)));
    const onTint = TINTS.map((p) => contrast(hex, mix(hex, declared.get('--cairn-ink-100'), p)));
    const onWhite = declared.has('--cairn-paper-0')
      ? contrast(hex, declared.get('--cairn-paper-0'))
      : null;
    rows.push({token, hex, onInk, onTint, onWhite});

    INK.forEach((t, i) => {
      if (onInk[i] < AA_TEXT) {
        fail(
          `contrast: ${token} (${hex}) measures ${onInk[i].toFixed(2)}:1 on ${t} ` +
            `(${declared.get(t)}), below ${AA_TEXT}:1`,
        );
      }
    });
    TINTS.forEach((p, i) => {
      if (onTint[i] < AA_TEXT) {
        fail(
          `contrast: ${token} (${hex}) measures ${onTint[i].toFixed(2)}:1 on its own ` +
            `${p}% tint over --cairn-ink-100, below ${AA_TEXT}:1`,
        );
      }
    });
  }
}

/* -------------------------------------------------------------------- report */

if (process.argv.includes('--report')) {
  const w = (s, n) => String(s).padEnd(n);
  const r = (n, d = 2) => n.toFixed(d).padStart(8);
  console.log(
    `${w('token', 15)}${w('hex', 10)}${'ink-0'.padStart(8)}${'ink-100'.padStart(8)}` +
      `${'ink-200'.padStart(8)}${'12%tint'.padStart(8)}${'13%tint'.padStart(8)}${'white'.padStart(8)}`,
  );
  for (const row of rows) {
    console.log(
      w(row.token, 15) +
        w(row.hex, 10) +
        row.onInk.map((v) => r(v)).join('') +
        row.onTint.map((v) => r(v)).join('') +
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
  const white = rows.filter((x) => x.onWhite !== null).map((x) => x.onWhite);
  if (white.length) {
    console.log(
      `best on a white ground       ${Math.max(...white).toFixed(2)}:1  ` +
        `— ${white.filter((v) => v >= AA_TEXT).length}/${white.length} clear ${AA_TEXT}:1, ` +
        `${white.filter((v) => v >= 3).length}/${white.length} clear 3:1. This is why every ` +
        `accent-as-text surface pins the ink ramp.`,
    );
  }
  console.log('');
}

if (failures.length) {
  console.error(`token-layer check FAILED (${failures.length}):\n`);
  for (const f of failures) console.error(`  ${f}`);
  console.error('');
  process.exit(1);
}

console.log(
  `token-layer check OK: ${Object.keys(CATEGORY_TOKENS).length} ADR-0009 tokens at their ` +
    `shipped values, ${scannable.length} source files free of colour literals, ` +
    `every accent >= ${AA_TEXT}:1 on the ink ramp.`,
);
