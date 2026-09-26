/**
 * Link guard — the two build-time checks behind "no repository links".
 *
 * Governing: ADR-0014 (the public site is the design record),
 *            SPEC-0010 REQ "No Repository Links",
 *            SPEC-0010 REQ "Link and Anchor Integrity"
 *
 * WHERE EACH CHECK RUNS, and why they cannot both run in the same place.
 *
 * `assertNoEditUrl()` is a check over the *site configuration*, so it runs at
 * config load alongside the token guard — on every `docusaurus build` and every
 * `docusaurus start` — and an author who reintroduces `editUrl` finds out in the
 * dev server. It names the configuration key it found, because the whole point
 * of the check is that the key is easy to add back without noticing.
 *
 * `assertNoRepositoryLinks()` is a check over *rendered output*, and rendered
 * output does not exist until a full build has written it: `docusaurus start`
 * serves from memory and never emits HTML to disk. It therefore runs as a
 * `postBuild` step (see website/plugins/link-guard/) and is a CI check rather
 * than a dev-server one. design.md accepts that latency explicitly, because the
 * alternative — grepping source for link-shaped strings — misses every link the
 * theme assembles at render time and is the weaker check.
 *
 * WHY AN ALLOWLIST RATHER THAN A LIST OF FORGES. A blocklist of repository
 * hostnames stops being true the first time the project moves forge, and it is
 * silently wrong in the interval. An allowlist of *permitted* hosts stays true:
 * a link to a forge nobody anticipated fails by default, which is the direction
 * the failure should point.
 */

import {readdirSync, readFileSync, statSync} from 'node:fs';
import {join, relative, sep} from 'node:path';

/**
 * Every external host the rendered bundle is permitted to link to.
 *
 * Deliberately tiny, and every entry carries its reason. The site's own origin
 * is not listed: it is derived from `siteConfig.url` and skipped as same-origin,
 * so moving the site to a custom domain does not require editing this file.
 *
 * - `docusaurus.io` — theme-classic inlines a "this site did not load properly"
 *   fallback into every page's HTML, and that string contains a link to the
 *   Docusaurus baseUrl documentation. It is emitted by the theme, is not a
 *   repository, and is only ever reachable when the JavaScript bundle has
 *   already failed.
 *
 * - `cairn.stump.wtf` — the PRODUCT. The homepage's "Open your bin" button and
 *   the navbar's "Sign in" item point at the app's auth-gated /bin, and they
 *   use the absolute URL because /bin is an app route, not a page of this
 *   site, and the bundle also serves from Pages where a relative link would
 *   dead-end. On the app-domain build this entry is redundant (same-origin,
 *   skipped before the allowlist is consulted); it exists for the Pages
 *   build, where the product is a foreign host that the record may
 *   legitimately send people to.
 *
 * - `switchboard.stump.wtf` and `stump-wtf.github.io` — the public documentation
 *   sites of Switchboard and Harness, the two sibling products the getting-started
 *   guides explain Cairn alongside (handoffs, outbound webhooks, "how the pieces
 *   fit"). Both are docs sites, not source repositories; the guides link their
 *   docs pages only, never a forge.
 */
export const ALLOWED_HOSTS = new Set([
  'docusaurus.io',
  'cairn.stump.wtf',
  'switchboard.stump.wtf',
  'stump-wtf.github.io',
]);

/**
 * Hosts known to serve source repositories. This list does NOT decide whether a
 * link fails — the allowlist above does that, and a forge missing from here
 * still fails. It exists so the error message can say *why* a link is
 * disqualifying rather than only that it is.
 */
const REPOSITORY_HOSTS = new Set([
  'github.com',
  'www.github.com',
  'gist.github.com',
  'raw.githubusercontent.com',
  'gitlab.com',
  'bitbucket.org',
  'codeberg.org',
  'sr.ht',
  'git.sr.ht',
  'gitea.com',
  'gitea.stump.rocks',
]);

/**
 * URL-bearing attributes in rendered HTML.
 *
 * Docusaurus minifies its output and leaves attribute values unquoted where it
 * can (`href=/cairn/`), so all three quoting forms have to be handled. The
 * scan runs over the whole document including inline `<script>` payloads, which
 * is the conservative choice: a link the theme writes into a script string is
 * still a link the reader can follow.
 */
const URL_ATTR = /\b(href|src|content)\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+))/gi;

/**
 * `srcset` is scanned separately because it is the one URL-bearing attribute
 * whose value is a *list*: comma-separated candidates, each a URL followed by an
 * optional density or width descriptor. Splitting the single-URL attributes on
 * commas instead would corrupt any URL with a legal comma in its path, so the
 * two shapes do not share a pattern.
 */
const SRCSET_ATTR = /\b(srcset)\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+))/gi;

/**
 * Absolute http(s) URLs only; everything else is same-site, an anchor, a
 * `mailto:` or a data URI.
 *
 * The delimiter trim is for links the theme writes into an inline script as a
 * JavaScript string literal, where the attribute quotes arrive backslash-escaped
 * (`href=\"https://…\"`). Those are still links a reader can follow, so they are
 * still in scope; only the escaping has to be undone.
 */
function absoluteUrl(value) {
  const trimmed = value.replace(/^[\\"']+/, '').replace(/[\\"']+$/, '');
  if (!/^https?:\/\//i.test(trimmed)) {
    return null;
  }
  try {
    return new URL(trimmed);
  } catch {
    return null;
  }
}

/** Every external host one rendered page links to, with the URL that did it. */
export function externalLinksIn(html) {
  const found = [];
  const record = (attr, raw) => {
    const url = absoluteUrl(raw);
    if (url) {
      found.push({attr: attr.toLowerCase(), url: url.href, host: url.hostname});
    }
  };

  for (const match of html.matchAll(URL_ATTR)) {
    record(match[1], match[2] ?? match[3] ?? match[4] ?? '');
  }
  for (const match of html.matchAll(SRCSET_ATTR)) {
    const value = match[2] ?? match[3] ?? match[4] ?? '';
    for (const candidate of value.split(',')) {
      // Each candidate is `<url> [descriptor]`; the URL is the first token.
      record(match[1], candidate.trim().split(/\s+/)[0] ?? '');
    }
  }
  return found;
}

function htmlFilesIn(dir, acc = []) {
  for (const entry of readdirSync(dir, {withFileTypes: true})) {
    const abs = join(dir, entry.name);
    if (entry.isDirectory()) {
      htmlFilesIn(abs, acc);
    } else if (entry.isFile() && entry.name.endsWith('.html')) {
      acc.push(abs);
    }
  }
  return acc;
}

/**
 * Scan a built bundle.
 *
 * @param {{outDir: string, siteUrl?: string}} options
 * @returns {{files: number, violations: {page: string, url: string, host: string, repository: boolean}[]}}
 */
export function scanBundle({outDir, siteUrl}) {
  let siteHost = null;
  if (siteUrl) {
    try {
      siteHost = new URL(siteUrl).hostname;
    } catch {
      siteHost = null;
    }
  }

  if (!statSync(outDir, {throwIfNoEntry: false})?.isDirectory()) {
    throw new Error(
      `[cairn-link-guard] no built bundle at ${outDir}. The rendered-output ` +
        `scan only means anything after a full build; run \`npm run build\`.`,
    );
  }

  const violations = [];
  const files = htmlFilesIn(outDir);
  for (const file of files) {
    const page = `/${relative(outDir, file).split(sep).join('/')}`;
    for (const link of externalLinksIn(readFileSync(file, 'utf8'))) {
      if (link.host === siteHost || ALLOWED_HOSTS.has(link.host)) {
        continue;
      }
      violations.push({
        page,
        attr: link.attr,
        url: link.url,
        host: link.host,
        repository: REPOSITORY_HOSTS.has(link.host),
      });
    }
  }
  return {files: files.length, violations};
}

/**
 * Throw unless the bundle links only to allowlisted hosts.
 *
 * @returns {{summary: string}} on success, for the caller to log.
 */
export function assertNoRepositoryLinks({outDir, siteUrl}) {
  const {files, violations} = scanBundle({outDir, siteUrl});
  if (violations.length > 0) {
    const lines = violations.map(
      (violation) =>
        `  ${violation.page}\n    → ${violation.url}` +
        (violation.repository
          ? `\n    ${violation.host} is a source-repository host. ADR-0014 drops the ` +
            `"view source" affordance entirely: the full record renders on the page, so ` +
            `there is nothing a forge link could usefully point at.`
          : `\n    ${violation.host} is not on the permitted-host allowlist in ` +
            `website/scripts/check-links.mjs.`),
    );
    throw new Error(
      `[cairn-link-guard] ${violations.length} disallowed external ` +
        `link${violations.length === 1 ? '' : 's'} in the rendered bundle:\n` +
        `${lines.join('\n')}\n`,
    );
  }
  return {
    summary:
      `link guard OK: ${files} rendered pages, no link to a repository host ` +
      `and none to a host outside the allowlist.`,
  };
}

/**
 * Walk a site configuration and throw if anything configures an edit URL.
 *
 * Generic on purpose. `editUrl` is accepted by the docs, blog and pages content
 * plugins alike, it can be set on a preset or on a standalone plugin entry, and
 * the failure mode being guarded against is somebody adding it back somewhere
 * this check did not think to look. So the walk names the key by its full path
 * — `presets[0][1].docs.editUrl` — rather than probing one known location.
 *
 * @returns {{summary: string}} on success.
 */
export function assertNoEditUrl(config, label = 'docusaurus.config.ts') {
  const found = [];
  const seen = new WeakSet();

  const walk = (node, path) => {
    if (node === null || typeof node !== 'object') {
      return;
    }
    if (seen.has(node)) {
      return;
    }
    seen.add(node);
    if (Array.isArray(node)) {
      node.forEach((item, index) => walk(item, `${path}[${index}]`));
      return;
    }
    for (const [key, value] of Object.entries(node)) {
      const here = path ? `${path}.${key}` : key;
      if (/^edit(Url|CurrentVersion)$/.test(key) && value !== undefined && value !== false) {
        found.push(here);
        continue;
      }
      walk(value, here);
    }
  };

  walk(config, '');

  if (found.length > 0) {
    throw new Error(
      `[cairn-link-guard] ${label} configures an edit URL at ` +
        `${found.join(', ')}. SPEC-0010 REQ "No Repository Links": the ` +
        `documentation content instance must not configure an edit URL, because ` +
        `an "edit this page" link is a link into a repository on every page the ` +
        `site serves.`,
    );
  }
  return {summary: 'link guard OK: no edit URL configured on any content instance.'};
}

/*
 * DELIBERATELY NO CLI, unlike scripts/check-tokens.mjs.
 *
 * The bundle scan needs two inputs: the output directory and the site's own
 * origin, which it must know in order to tell a canonical `<link>` back to the
 * site from a link off it. Both are properties of the site configuration, and a
 * standalone `node scripts/check-links.mjs` cannot read a TypeScript config that
 * awaits the record generator before it resolves. A CLI run without the origin
 * does not fail safe — every page carries a self-referential
 * `<link rel="canonical" href="https://…">` back to itself, so the scan would
 * report one violation per page and bury the one link that matters. The
 * matching `og:url` is a `<meta content=…>`, and `content` IS in URL_ATTR
 * above — a test in this file's suite asserts a forge URL cannot hide in a meta
 * tag — so it would report one violation per page too. Both emitters force
 * this, not canonical alone.
 *
 * `postBuild` receives `outDir` and `siteConfig` already resolved, which is why
 * the plugin in website/plugins/link-guard/ is the only caller. CI needs no
 * separate step: it runs a full build, and a full build runs this.
 */
