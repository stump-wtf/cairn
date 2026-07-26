#!/usr/bin/env node
/**
 * Bundle scan — what the built site actually ships.
 *
 * Governing: ADR-0014 (the public site is the design record),
 *            SPEC-0010 REQ "First-Party Asset Loading",
 *            SPEC-0010 REQ "Security Headers",
 *            SPEC-0010 REQ "Deployment Least Privilege"
 *
 * Source checks cannot answer either question this file answers. "Does every
 * asset come from our own origin?" is a property of the emitted HTML and CSS,
 * where a transitive dependency's `@import` lands without any file in `src/`
 * mentioning it. "Is there a credential in the output?" is a property of the
 * output. Both are therefore `postBuild` checks, and design.md accepts that
 * consequence explicitly: they are CI checks rather than dev-server ones,
 * because `docusaurus start` never renders HTML to disk.
 *
 * Two independent scans:
 *
 *   1. ASSET ORIGINS. Every subresource reference in the bundle must be
 *      same-origin. This is the mechanical form of REQ "Security Headers"'s
 *      "the policy MUST additionally be verified against the built bundle, so
 *      that a dependency introducing a remote asset is caught at build time
 *      rather than by a reader's browser".
 *
 *      Only SUBRESOURCES are in scope — things the browser fetches on its own to
 *      render the page. An `<a href>` is not one: a reader clicking a link is a
 *      navigation they chose, and which links the site may carry is REQ "No
 *      Repository Links", a different check with a different rule (an allowlist
 *      of hosts, not "same origin only").
 *
 *   2. CREDENTIALS AND PRIVATE HOSTS. REQ "Deployment Least Privilege": "No
 *      credential, token, or private host name MAY appear in the built bundle or
 *      in the derived data module. The bundle MUST be scanned for these before
 *      the deploy step runs, and a match MUST fail the deployment."
 *
 * Run as `npm run scan:bundle`, and automatically from the site's `postBuild`
 * hook so a local build gets the same answer CI does.
 */

import {existsSync, readFileSync, readdirSync, statSync} from 'node:fs';
import {basename, extname, join, relative, sep} from 'node:path';
import {fileURLToPath} from 'node:url';

const HERE = fileURLToPath(new URL('.', import.meta.url));
const DEFAULT_BUILD = join(HERE, '..', 'build');

/* ------------------------------------------------------------ asset origins */

/**
 * HTML attributes the browser fetches without being asked.
 *
 * `<link href>` is conditional rather than listed here: a `<link>` is a
 * subresource for `stylesheet`, `preload`, `icon` and friends, and is NOT one
 * for `canonical` or `alternate`, which are metadata naming a URL rather than
 * requests. Treating those as subresources would fail the build on a correct
 * canonical tag.
 */
const SUBRESOURCE_ATTRS = [
  ['script', 'src'],
  ['img', 'src'],
  ['img', 'srcset'],
  ['source', 'src'],
  ['source', 'srcset'],
  ['iframe', 'src'],
  ['frame', 'src'],
  ['embed', 'src'],
  ['object', 'data'],
  ['video', 'src'],
  ['video', 'poster'],
  ['audio', 'src'],
  ['track', 'src'],
  ['input', 'src'],
  ['use', 'href'],
  ['use', 'xlink:href'],
  ['image', 'href'],
];

/** `rel` values that make a `<link>` a fetched subresource. */
const FETCHING_REL = new Set([
  'stylesheet',
  'preload',
  'prefetch',
  'preconnect',
  'dns-prefetch',
  'modulepreload',
  'icon',
  'shortcut icon',
  'apple-touch-icon',
  'apple-touch-icon-precomposed',
  'mask-icon',
  'manifest',
]);

/**
 * Asset extensions, for the JavaScript pass. A bundle is full of URL-shaped
 * strings that are not subresources (documentation prose, example endpoints,
 * schema identifiers), so flagging every absolute URL inside JavaScript would
 * bury a real finding under the record's own text. A remote URL that ENDS IN AN
 * ASSET EXTENSION inside a script is a different matter: that is a font, a
 * stylesheet or an image about to be injected at runtime.
 */
const ASSET_EXTENSIONS =
  /\.(?:css|m?js|woff2?|ttf|otf|eot|png|jpe?g|gif|svg|webp|avif|ico|mp4|webm|wasm)(?:[?#]|$)/i;

/** A URL that leaves our origin. Protocol-relative `//host/…` counts. */
function isRemote(url) {
  const value = url.trim();
  if (!value) return false;
  if (value.startsWith('//')) return true;
  return /^[a-z][a-z0-9+.-]*:/i.test(value) && !/^(?:data|blob|about|mailto):/i.test(value);
}

/** Every candidate URL in a `srcset`, which is a comma-separated list. */
function splitSrcset(value) {
  return value
    .split(',')
    .map((candidate) => candidate.trim().split(/\s+/)[0])
    .filter(Boolean);
}

function attrValue(tag, name) {
  const re = new RegExp(`(?:^|\\s)${name.replace(':', '\\:')}\\s*=\\s*("([^"]*)"|'([^']*)'|([^\\s>]+))`, 'i');
  const m = re.exec(tag);
  if (!m) return null;
  return m[2] ?? m[3] ?? m[4] ?? '';
}

function scanHtml(text, file, findings) {
  for (const m of text.matchAll(/<([a-zA-Z][\w:-]*)\b([^>]*)>/g)) {
    const tag = m[1].toLowerCase();
    const attrs = m[2];

    if (tag === 'link') {
      const rel = (attrValue(attrs, 'rel') ?? '').toLowerCase().trim();
      if (!FETCHING_REL.has(rel)) continue;
      const href = attrValue(attrs, 'href');
      if (href && isRemote(href)) {
        findings.push({file, kind: `<link rel="${rel}">`, url: href});
      }
      const imagesrcset = attrValue(attrs, 'imagesrcset');
      if (imagesrcset) {
        for (const url of splitSrcset(imagesrcset)) {
          if (isRemote(url)) findings.push({file, kind: '<link imagesrcset>', url});
        }
      }
      continue;
    }

    for (const [element, attr] of SUBRESOURCE_ATTRS) {
      if (element !== tag) continue;
      const value = attrValue(attrs, attr);
      if (!value) continue;
      const urls = attr === 'srcset' ? splitSrcset(value) : [value];
      for (const url of urls) {
        if (isRemote(url)) findings.push({file, kind: `<${tag} ${attr}>`, url});
      }
    }

    /* `style="background: url(https://…)"` is a subresource wearing an
       attribute. The CSS pass never sees it, because it is not in a stylesheet. */
    const style = attrValue(attrs, 'style');
    if (style) scanCss(style, file, findings, 'inline style attribute');
  }

  /* Inline <style> blocks are stylesheets that happen to live in the HTML. */
  for (const m of text.matchAll(/<style\b[^>]*>([\s\S]*?)<\/style>/gi)) {
    scanCss(m[1], file, findings, 'inline <style>');
  }
}

/**
 * `@import` is matched first and claims its span, because `@import
 * url('https://…')` satisfies both patterns and would otherwise be reported
 * twice — once as an import and once as the `url()` inside it. One literal, one
 * finding, and the more specific description wins.
 */
function scanCss(text, file, findings, kind = 'stylesheet') {
  const claimed = [];
  for (const m of text.matchAll(/@import\s+(?:url\(\s*)?(['"]?)([^'")\s]+)\1/gi)) {
    claimed.push([m.index, m.index + m[0].length]);
    if (isRemote(m[2])) findings.push({file, kind: `${kind} @import`, url: m[2].trim()});
  }
  for (const m of text.matchAll(/url\(\s*(['"]?)([^'")]+)\1\s*\)/gi)) {
    const end = m.index + m[0].length;
    if (claimed.some(([from, to]) => m.index < to && end > from)) continue;
    if (isRemote(m[2])) findings.push({file, kind: `${kind} url()`, url: m[2].trim()});
  }
}

function scanJs(text, file, findings) {
  for (const m of text.matchAll(/(?:https?:)?\/\/[^\s'"`)\\]+/g)) {
    const url = m[0];
    if (!isRemote(url)) continue;
    if (!ASSET_EXTENSIONS.test(url)) continue;
    findings.push({file, kind: 'asset URL in script', url});
  }
}

/* ------------------------------------------------ credentials, private hosts */

/**
 * Credential shapes, chosen for precision over coverage. Every one of these is
 * a prefix or a structure that does not occur in prose, so a match is a finding
 * rather than a conversation. A generic "high entropy string" heuristic is
 * deliberately absent: the bundle is full of content hashes and minified
 * identifiers, and a scanner that cries wolf on those is a scanner someone
 * removes from the deploy.
 */
const CREDENTIAL_PATTERNS = [
  [/\bAKIA[0-9A-Z]{16}\b/, 'AWS access key id'],
  [/\bASIA[0-9A-Z]{16}\b/, 'AWS temporary access key id'],
  [/\bgh[pousr]_[A-Za-z0-9]{36,}\b/, 'GitHub token'],
  [/\bgithub_pat_[A-Za-z0-9_]{20,}\b/, 'GitHub fine-grained token'],
  [/\bxox[abporst]-[A-Za-z0-9-]{10,}\b/, 'Slack token'],
  [/\bsk-[A-Za-z0-9]{32,}\b/, 'OpenAI-style secret key'],
  [/\bsk-ant-[A-Za-z0-9_-]{20,}\b/, 'Anthropic API key'],
  [/-----BEGIN (?:RSA |EC |DSA |OPENSSH |PGP )?PRIVATE KEY-----/, 'private key block'],
  [/\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b/, 'JSON Web Token'],
  [/\bglpat-[A-Za-z0-9_-]{20,}\b/, 'GitLab personal access token'],
  [/\bAIza[0-9A-Za-z_-]{35}\b/, 'Google API key'],
  [/\b[a-z][a-z0-9+.-]*:\/\/[^\s/:@]+:[^\s/@]+@[^\s/]+/, 'URL with embedded credentials'],
];

/**
 * Hosts that are infrastructure rather than product.
 *
 * This list is short and it is short on purpose. `cairn.stump.rocks` is NOT on
 * it and must not be: that is the product's own intended public hostname, it
 * appears in the record's examples deliberately, and the bundle ships it
 * twenty-four times. A deny list that swept up the whole `stump.rocks` domain
 * would fail this build today on the record's own correct prose, which is how a
 * secret scanner earns a `--force` flag and then means nothing.
 *
 * What belongs here is the private infrastructure a reader of the public site
 * has no business learning about: the forge, the wiki, the hosts.
 */
/*
 * Each service prefix must be followed by at least TWO more dot-separated
 * labels, i.e. a real `service.domain.tld`. Without that the first run of this
 * scanner failed the build on `outline.button--active` — an Infima class name in
 * the stylesheet. That is the deny-list failure mode in miniature, and the shape
 * below is the fix: match hostnames, not identifiers that contain a dot.
 */
const HOST_TAIL = String.raw`(?:\.[a-z0-9](?:[a-z0-9-]*[a-z0-9])?){2,}(?![\w-])`;
const privateHost = (prefix, label) => [
  new RegExp(String.raw`(?<![\w.-])${prefix}${HOST_TAIL}`, 'i'),
  label,
];

const PRIVATE_HOST_PATTERNS = [
  privateHost('gitea', 'private forge hostname'),
  privateHost('outline', 'private wiki hostname'),
  privateHost('openbao', 'secret-store hostname'),
  privateHost(String.raw`(?:cloud|db|vault)\d{2}`, 'internal host name'),
  [/(?<![\w.-])10\.\d{1,3}\.\d{1,3}\.\d{1,3}(?![\w.-])/, 'RFC1918 address'],
  [/(?<![\w.-])192\.168\.\d{1,3}\.\d{1,3}(?![\w.-])/, 'RFC1918 address'],
  [
    /(?<![\w.-])172\.(?:1[6-9]|2\d|3[01])\.\d{1,3}\.\d{1,3}(?![\w.-])/,
    'RFC1918 address',
  ],
];
/*
 * `localhost` and `127.0.0.1` are deliberately absent. The record and the
 * narrative docs show local development commands, a loopback address discloses
 * nothing about anyone's infrastructure, and banning it would make the CLI
 * documentation unpublishable.
 */

/**
 * Every credential or private host name in a string, as `{label, match}`.
 *
 * Exported because REQ "Deployment Least Privilege" names two artifacts, not
 * one: *"No credential, token, or private host name MAY appear in the built
 * bundle OR IN THE DERIVED DATA MODULE."* The record pipeline calls this on the
 * data module it emits, which puts that half of the requirement inside the
 * pipeline where it fails the dev server too, rather than waiting for a build.
 */
export function findSecrets(text) {
  const hits = [];
  for (const [pattern, label] of [...CREDENTIAL_PATTERNS, ...PRIVATE_HOST_PATTERNS]) {
    const m = pattern.exec(text);
    if (m) hits.push({label, match: m[0], index: m.index});
  }
  return hits;
}

/* ------------------------------------------------------------------- driver */

const TEXT_EXTENSIONS = new Set([
  '.html', '.htm', '.css', '.js', '.mjs', '.cjs', '.json', '.map',
  '.txt', '.xml', '.svg', '.webmanifest', '.md',
]);

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

/**
 * Scan a built bundle.
 * @returns {{failures: string[], summary: string, files: number}}
 */
export function scanBundle({outDir = DEFAULT_BUILD} = {}) {
  if (!existsSync(outDir)) {
    throw new Error(
      `no built bundle at ${outDir}. Run \`npm run build\` before scanning it.`,
    );
  }

  const rel = (p) => relative(outDir, p).split(sep).join('/');
  const files = walk(outDir);
  const assetFindings = [];
  const secretFailures = [];

  for (const file of files) {
    const ext = extname(file).toLowerCase();
    if (!TEXT_EXTENSIONS.has(ext)) continue;
    const text = readFileSync(file, 'utf8');
    const name = rel(file);

    if (ext === '.html' || ext === '.htm') scanHtml(text, name, assetFindings);
    else if (ext === '.css') scanCss(text, name, assetFindings);
    else if (ext === '.js' || ext === '.mjs' || ext === '.cjs') scanJs(text, name, assetFindings);
    else if (ext === '.svg') scanHtml(text, name, assetFindings);

    for (const hit of findSecrets(text)) {
      secretFailures.push(
        `${name}:${lineOf(text, hit.index)}: ${hit.label} — \`${hit.match.slice(0, 48)}\``,
      );
    }
  }

  const failures = [
    ...assetFindings.map(
      (f) =>
        `${f.file}: ${f.kind} loads \`${f.url}\` from a third-party origin. ` +
        `Every asset the site loads at runtime must be served from the site's own ` +
        `origin — SPEC-0010 REQ "First-Party Asset Loading".`,
    ),
    ...secretFailures.map(
      (f) =>
        `${f} — nothing secret and no private host name may reach the published ` +
        `bundle: SPEC-0010 REQ "Deployment Least Privilege".`,
    ),
  ];

  const summary =
    `bundle scan OK: ${files.length} files in ${rel(outDir) || basename(outDir)}, ` +
    `every subresource same-origin, no credential or private host name in the output.`;

  return {failures, summary, files: files.length};
}

/** Throw on any failure. This is what the site's `postBuild` hook calls. */
export function assertBundle(options = {}) {
  const result = scanBundle(options);
  if (result.failures.length) {
    throw new Error(
      `bundle scan FAILED (${result.failures.length}):\n\n` +
        result.failures.map((f) => `  ${f}`).join('\n') +
        '\n',
    );
  }
  return result;
}

/* ---------------------------------------------------------------------- CLI */

function main(argv) {
  const flag = argv.indexOf('--out');
  if (flag !== -1 && !argv[flag + 1]) {
    console.error('scan-bundle: --out needs a directory');
    return 2;
  }
  const outDir = flag === -1 ? DEFAULT_BUILD : argv[flag + 1];

  let result;
  try {
    result = scanBundle({outDir});
  } catch (err) {
    console.error(`bundle scan ERROR: ${err.message}`);
    return 2;
  }

  if (result.failures.length) {
    console.error(`bundle scan FAILED (${result.failures.length}):\n`);
    for (const f of result.failures) console.error(`  ${f}`);
    console.error('');
    return 1;
  }
  console.log(result.summary);
  return 0;
}

if (basename(process.argv[1] ?? '') === 'scan-bundle.mjs') {
  process.exit(main(process.argv.slice(2)));
}
