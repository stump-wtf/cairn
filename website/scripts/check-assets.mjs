#!/usr/bin/env node
/**
 * First-party asset guard — source level.
 *
 * Governing: ADR-0014 (the public site is the design record),
 *            SPEC-0010 REQ "First-Party Asset Loading"
 *
 * The requirement has two scenarios and they want two different checks.
 * "Third-party asset request" is about the rendered page and is answered by
 * `scan-bundle.mjs` at postBuild. "Remote font import in source" is about the
 * source and says so: *"WHEN site source contains a stylesheet import or link
 * naming a third-party font host, THEN the build MUST fail and MUST name the
 * file and the host."* That one has to fire on the file, and this is it.
 *
 * It runs at CONFIG LOAD, next to the token guard and for the same reason
 * design.md gives ("Validation runs where it can actually run"): an author who
 * pastes a Google Fonts `@import` back in finds out in the dev server, in the
 * second before they alt-tab, rather than in CI after review. The bundle scan
 * cannot cover them there, because `docusaurus start` renders no HTML to disk.
 *
 * The rule is an ALLOWLIST of nothing: a stylesheet import, a `url()`, a
 * `<link>` or a `<script src>` in site source must be same-origin. A blocklist
 * of font CDNs would be obsolete the first time somebody reached for a CDN this
 * project has not heard of, and the requirement is not "not Google Fonts", it is
 * "every asset the site loads at runtime MUST be served from the site's own
 * origin".
 *
 * `data:` is permitted — a data URI is bytes already in the file, not a request.
 * So is a bare protocol scheme in prose or in a comment: comments are masked
 * before scanning, and prose is not an asset reference because this only looks
 * at the syntactic positions that make the browser fetch something.
 */

import {existsSync, readFileSync, readdirSync, statSync} from 'node:fs';
import {basename, join, relative, sep} from 'node:path';
import {fileURLToPath} from 'node:url';

const HERE = fileURLToPath(new URL('.', import.meta.url));
const DEFAULT_WEBSITE = join(HERE, '..');

const SCANNED_EXTENSIONS = ['.css', '.ts', '.tsx', '.js', '.jsx', '.mjs', '.html'];

/**
 * Font hosts named explicitly, only so the error message can say *"this is a
 * font CDN"* rather than *"this is remote"*. The check does not depend on the
 * list: anything remote fails whether or not it is here.
 */
const KNOWN_FONT_HOSTS = [
  'fonts.googleapis.com',
  'fonts.gstatic.com',
  'use.typekit.net',
  'p.typekit.net',
  'fast.fonts.net',
  'use.fontawesome.com',
  'cdn.jsdelivr.net',
  'unpkg.com',
  'cdnjs.cloudflare.com',
];

function isRemote(url) {
  const value = url.trim();
  if (!value) return false;
  if (value.startsWith('//')) return true;
  return /^[a-z][a-z0-9+.-]*:/i.test(value) && !/^(?:data|blob|about|mailto):/i.test(value);
}

function hostOf(url) {
  const m = /^(?:[a-z][a-z0-9+.-]*:)?\/\/([^/?#]+)/i.exec(url.trim());
  return m ? m[1] : url.trim();
}

/**
 * Blank comment bodies while preserving byte offsets, so line numbers hold and a
 * URL discussed in a comment — this file's own docblock, for one — is not a
 * finding. Quote tracking is enabled only for languages that have line comments;
 * `//` inside a CSS `url('https://…')` is not a comment.
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
 * Every asset-reference position this checker understands, as
 * `[regex, url capture group, description]`.
 */
const REFERENCES = [
  [/@import\s+(?:url\(\s*)?(['"])([^'"]+)\1/gi, 2, 'stylesheet @import'],
  [/@import\s+url\(\s*([^'")\s]+)\s*\)/gi, 1, 'stylesheet @import'],
  [/\burl\(\s*(['"]?)([^'")]+)\1\s*\)/gi, 2, 'url() reference'],
  [/<link\b[^>]*?\bhref\s*=\s*(['"])([^'"]+)\1/gi, 2, '<link href>'],
  [/<script\b[^>]*?\bsrc\s*=\s*(['"])([^'"]+)\1/gi, 2, '<script src>'],
];

/**
 * Check a website directory for remote asset references in source.
 * @returns {{failures: string[], summary: string, scanned: number}}
 */
export function checkAssets({root = DEFAULT_WEBSITE} = {}) {
  const src = join(root, 'src');
  const config = join(root, 'docusaurus.config.ts');
  const staticDir = join(root, 'static');

  const failures = [];
  const rel = (p) => relative(root, p).split(sep).join('/');

  const scannable = [
    ...(existsSync(src) ? walk(src) : []),
    ...(existsSync(staticDir) ? walk(staticDir) : []),
    ...(existsSync(config) ? [config] : []),
  ].filter((p) => SCANNED_EXTENSIONS.some((e) => p.endsWith(e)));

  for (const file of scannable) {
    const name = rel(file);
    const raw = readFileSync(file, 'utf8');
    const text = maskComments(raw, {lineComments: !file.endsWith('.css') && !file.endsWith('.html')});

    /* Patterns are applied in order and each claims the span it matched, so a
       later, more general pattern cannot report the same literal a second time.
       `@import url('https://…')` satisfies both the import rules and the url()
       rule; without this it is one mistake reported twice, and a checker that
       inflates its own count is a checker nobody reads carefully. */
    const claimed = [];

    for (const [re, group, what] of REFERENCES) {
      re.lastIndex = 0;
      let m;
      while ((m = re.exec(text))) {
        const end = m.index + m[0].length;
        if (claimed.some(([from, to]) => m.index < to && end > from)) continue;
        claimed.push([m.index, end]);
        const url = m[group];
        if (!url || !isRemote(url)) continue;
        const host = hostOf(url);
        const isFontHost = KNOWN_FONT_HOSTS.includes(host.toLowerCase());
        failures.push(
          `${name}:${lineOf(text, m.index)}: ${what} names the third-party host ` +
            `\`${host}\`${isFontHost ? ' (a font/asset CDN)' : ''}. ` +
            `Every font, stylesheet, script and image must be served from the site's own ` +
            `origin — SPEC-0010 REQ "First-Party Asset Loading". Vendor the asset under ` +
            `src/ and reference it with a relative path.`,
        );
      }
    }
  }

  return {
    failures,
    scanned: scannable.length,
    summary: `first-party asset check OK: ${scannable.length} source files reference no third-party host.`,
  };
}

/** Throw on any failure. This is what the site config calls at config load. */
export function assertFirstPartyAssets(options = {}) {
  const result = checkAssets(options);
  if (result.failures.length) {
    throw new Error(
      `first-party asset check FAILED (${result.failures.length}):\n\n` +
        result.failures.map((f) => `  ${f}`).join('\n') +
        '\n',
    );
  }
  return result;
}

/* ---------------------------------------------------------------------- CLI */

function main(argv) {
  const flag = argv.indexOf('--root');
  if (flag !== -1 && !argv[flag + 1]) {
    console.error('check-assets: --root needs a directory');
    return 2;
  }
  const root = flag === -1 ? DEFAULT_WEBSITE : argv[flag + 1];

  let result;
  try {
    result = checkAssets({root});
  } catch (err) {
    console.error(`first-party asset check ERROR: ${err.message}`);
    return 2;
  }

  if (result.failures.length) {
    console.error(`first-party asset check FAILED (${result.failures.length}):\n`);
    for (const f of result.failures) console.error(`  ${f}`);
    console.error('');
    return 1;
  }
  console.log(result.summary);
  return 0;
}

if (basename(process.argv[1] ?? '') === 'check-assets.mjs') {
  process.exit(main(process.argv.slice(2)));
}
