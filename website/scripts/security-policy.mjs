/**
 * The site's security policy, defined once.
 *
 * Governing: ADR-0014 (the public site is the design record),
 *            SPEC-0010 REQ "Security Headers",
 *            SPEC-0010 REQ "First-Party Asset Loading"
 *
 * Two consumers, one definition: the `<meta http-equiv>` tag the build injects
 * into every page, and the `_headers` file a header-capable host reads. They are
 * not the same policy and that is not an oversight — see `metaPolicy()`.
 *
 * WHY THERE ARE TWO DELIVERY CHANNELS AT ALL
 * ------------------------------------------
 * SPEC-0010 REQ "Security Headers" is unambiguous that these must be RESPONSE
 * headers, and gives the reason: `frame-ancestors` is specified to be ignored
 * when delivered by `<meta>`, and `X-Content-Type-Options`, `Referrer-Policy`
 * and HSTS have no `<meta>` form any browser honours. The site's pinned deploy
 * target is GitHub Pages, which serves static files and cannot set a response
 * header at all. design.md records this as the capability's most urgent open
 * question ("Where should the site be hosted?").
 *
 * This module does not resolve that question and does not pretend to. It ships
 * everything that is deliverable today and leaves the header file in place and
 * correct so that moving to a header-capable host (Cloudflare Pages, Netlify, or
 * a proxy in front of Pages) is a hosting change and not a code change:
 *
 *   1. `<meta http-equiv="Content-Security-Policy">` on every page, carrying the
 *      directives a meta-delivered policy actually enforces. That is a real
 *      restriction: it is what stops a dependency loading a script or a font
 *      from a third-party host in a reader's browser.
 *   2. `_headers` at the root of the built bundle — the Netlify/Cloudflare
 *      Pages format — carrying the full set including `frame-ancestors`,
 *      `nosniff`, `Referrer-Policy` and HSTS. It is written by the security
 *      plugin's `postBuild`, not copied from `static/`, because it names the
 *      hashes of the inline scripts that build actually emitted. Inert on
 *      GitHub Pages; correct the moment the host can read it.
 *   3. A build-time scan of the emitted bundle (`scan-bundle.mjs`), which is
 *      host-independent and is the check the requirement calls out separately:
 *      "the policy MUST additionally be verified against the built bundle".
 *
 * ON `'unsafe-inline'` IN `style-src`
 * -----------------------------------
 * It is there and it is load-bearing. Docusaurus emits inline `style="…"`
 * attributes — the site config's own comment explains one of them, the
 * `--prism-color` pair it puts on every code block — and a CSP3 browser resolves
 * `style-src-attr` from `style-src`, so a policy without it blanks those
 * surfaces. Hashing does not help: `'unsafe-hashes'` plus a hash per distinct
 * attribute value is a list that changes whenever a code block does.
 *
 * `script-src` gets no such concession. Docusaurus emits exactly two inline
 * scripts (the base-URL banner and the colour-mode bootstrap) and both are
 * static, so the build hashes them and the policy names the hashes. That keeps
 * the directive strict where strictness is worth having.
 */

import {createHash} from 'node:crypto';

/** Placeholder the injected meta tag carries until postBuild seals the hashes. */
export const HASH_PLACEHOLDER = '__CAIRN_SCRIPT_HASHES__';

/**
 * Directives common to both channels.
 *
 * `img-src` admits `data:` because the bundler inlines small images and the SVG
 * favicon as data URIs; a data URI is not a third-party origin and discloses
 * nothing about the reader, which is what REQ "First-Party Asset Loading" is
 * protecting.
 */
function baseDirectives(scriptHashes) {
  const script = ["'self'", ...scriptHashes].join(' ');
  return [
    ["default-src", "'self'"],
    ["base-uri", "'self'"],
    ["script-src", script],
    ["style-src", "'self' 'unsafe-inline'"],
    ["font-src", "'self'"],
    ["img-src", "'self' data:"],
    ["connect-src", "'self'"],
    ["media-src", "'self'"],
    ["object-src", "'none'"],
    ["form-action", "'none'"],
  ];
}

/**
 * Directives a `<meta http-equiv>` policy cannot carry.
 *
 * `frame-ancestors` is specified to be ignored in a meta-delivered policy, and
 * `report-uri`/`sandbox` likewise. Emitting them anyway would put a directive in
 * the page that a browser prints a console warning about and does not apply —
 * security theatre with a warning attached — so the meta variant omits them and
 * the header variant carries them.
 */
function headerOnlyDirectives() {
  return [
    ["frame-ancestors", "'none'"],
    ["upgrade-insecure-requests", ""],
  ];
}

const render = (directives) =>
  directives
    .map(([name, value]) => (value ? `${name} ${value}` : name))
    .join('; ');

/** The policy for `<meta http-equiv="Content-Security-Policy">`. */
export function metaPolicy(scriptHashes = []) {
  return render(baseDirectives(scriptHashes));
}

/** The policy for a real `Content-Security-Policy` response header. */
export function headerPolicy(scriptHashes = []) {
  return render([...baseDirectives(scriptHashes), ...headerOnlyDirectives()]);
}

/**
 * The response headers REQ "Security Headers" requires, beyond the CSP.
 *
 * HSTS is deliberately `max-age` + `includeSubDomains` and NOT `preload`.
 * Preloading is an irreversible submission for a whole registrable domain, and
 * which domain this site will live on is the open question above; a project site
 * under a shared host must not preload on that host's behalf.
 */
export const RESPONSE_HEADERS = [
  ['X-Content-Type-Options', 'nosniff'],
  ['Referrer-Policy', 'strict-origin-when-cross-origin'],
  ['Strict-Transport-Security', 'max-age=31536000; includeSubDomains'],
  /* Redundant beside `frame-ancestors 'none'` for a modern browser, and kept
     because it is the only framing control an older one honours. */
  ['X-Frame-Options', 'DENY'],
  ['Cross-Origin-Opener-Policy', 'same-origin'],
];

/** sha256-base64 of an inline script body, in the form CSP wants. */
export function scriptHash(body) {
  return `'sha256-${createHash('sha256').update(body, 'utf8').digest('base64')}'`;
}

/** Inline `<script>` — one with no `src`, i.e. one with a body to hash. */
const INLINE_SCRIPT = /<script\b(?![^>]*\ssrc\s*=)([^>]*)>([\s\S]*?)<\/script>/gi;

const TYPE_ATTR = /\btype\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+))/i;

const JS_MIME =
  /^(?:module|(?:application|text)\/(?:x-)?(?:java|ecma)script|application\/x-ecmascript|text\/jsmodule)$/i;

/**
 * Whether a `<script>` element's body is code the browser will run.
 *
 * This distinction is worth its own function because getting it wrong is
 * expensive in an unobvious direction. Docusaurus emits a
 * `<script type="application/ld+json">` BreadcrumbList on every doc page, and
 * its contents differ per page. Hashing those puts ONE HASH PER PAGE into the
 * policy: the `_headers` file measured 3 KB on a 42-page build and would grow
 * with the record until it hit a proxy's header-size limit, at which point the
 * whole policy silently stops being delivered. They also do not need hashing —
 * a `<script>` whose type is neither a JavaScript MIME type nor `module` is a
 * data block, is never executed, and is not subject to `script-src`.
 *
 * Restricting the set to executable scripts leaves exactly two, the base-URL
 * banner and the colour-mode bootstrap, and both are static — so the policy is
 * constant however large the record grows.
 */
export function isExecutableScript(attributes) {
  const match = TYPE_ATTR.exec(attributes);
  if (!match) {
    return true;
  }
  const type = (match[1] ?? match[2] ?? match[3] ?? '').trim();
  return type === '' || JS_MIME.test(type);
}

/** Every hash a page's executable inline scripts need, in document order. */
export function inlineScriptHashes(html) {
  const hashes = new Set();
  INLINE_SCRIPT.lastIndex = 0;
  let match;
  while ((match = INLINE_SCRIPT.exec(html))) {
    if (match[2].trim() === '' || !isExecutableScript(match[1])) {
      continue;
    }
    hashes.add(scriptHash(match[2]));
  }
  return [...hashes];
}

/**
 * The `_headers` file, in the format Netlify and Cloudflare Pages both read.
 * One rule set, applied to every path.
 */
export function renderHeadersFile(scriptHashes = []) {
  const lines = [
    '# Generated by website/scripts/security-policy.mjs at build time. Do not edit.',
    '#',
    '# Governing: ADR-0014, SPEC-0010 REQ "Security Headers".',
    '#',
    '# Read by Netlify and Cloudflare Pages. GitHub Pages — the currently pinned',
    '# deploy target — ignores it, which is exactly the gap design.md records as',
    '# this capability\'s most urgent open question. The file is emitted regardless',
    '# so that answering that question is a hosting change and not a code change.',
    '',
    '/*',
    `  Content-Security-Policy: ${headerPolicy(scriptHashes)}`,
    ...RESPONSE_HEADERS.map(([name, value]) => `  ${name}: ${value}`),
    '',
  ];
  return lines.join('\n');
}
