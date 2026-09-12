#!/usr/bin/env node --test
/**
 * Negative tests for scripts/check-links.mjs. A guard that cannot fail is not a
 * guard, so every check is driven over a fixture that breaks exactly it.
 *
 * Governing: ADR-0014, SPEC-0010 REQ "No Repository Links",
 *            SPEC-0010 REQ "Link and Anchor Integrity"
 *
 * The fixtures are written the way Docusaurus writes its output — minified,
 * attribute values unquoted where they can be — because a scanner tested only
 * against pretty-printed HTML passes its tests and misses the real bundle.
 */

import {test} from 'node:test';
import assert from 'node:assert/strict';
import {mkdirSync, mkdtempSync, writeFileSync} from 'node:fs';
import {join} from 'node:path';
import {tmpdir} from 'node:os';

import {
  assertNoEditUrl,
  assertNoRepositoryLinks,
  externalLinksIn,
  scanBundle,
} from './check-links.mjs';

const SITE_URL = 'https://joestump.github.io';

/** A scratch `build/` directory holding the given `path → html` pages. */
function bundle(pages) {
  const root = mkdtempSync(join(tmpdir(), 'cairn-links-'));
  for (const [name, html] of Object.entries(pages)) {
    const file = join(root, name);
    mkdirSync(join(file, '..'), {recursive: true});
    writeFileSync(file, html);
  }
  return root;
}

function scan(pages) {
  return scanBundle({outDir: bundle(pages), siteUrl: SITE_URL});
}

/* ----------------------------------------------------- the rendered bundle */

test('a bundle that links nowhere external passes', () => {
  const outDir = bundle({
    'index.html': '<a href=/cairn/docs/intro>Docs</a>',
    'docs/decisions/ADR-0009.html':
      '<a class=breadcrumbs__link href=/cairn/docs/decisions>Decisions · 14</a>' +
      '<link rel=canonical href="https://joestump.github.io/cairn/docs/decisions/ADR-0009">',
  });
  const {summary} = assertNoRepositoryLinks({outDir, siteUrl: SITE_URL});
  assert.match(summary, /link guard OK: 2 rendered pages/);
});

test('a link to a repository host fails, naming the page and the URL', () => {
  const outDir = bundle({
    'docs/decisions/ADR-0009.html':
      '<a href="https://github.com/joestump/cairn/blob/main/docs/adrs/ADR-0009.md">Source</a>',
  });
  assert.throws(
    () => assertNoRepositoryLinks({outDir, siteUrl: SITE_URL}),
    (error) => {
      assert.match(error.message, /\/docs\/decisions\/ADR-0009\.html/);
      assert.match(error.message, /https:\/\/github\.com\/joestump\/cairn/);
      assert.match(error.message, /source-repository host/);
      return true;
    },
  );
});

test('the check is an allowlist, so an unanticipated forge fails too', () => {
  // The point of the allowlist: this host is on no blocklist anywhere, and a
  // blocklist of known forges would wave it through.
  const {violations} = scan({
    'index.html': '<a href="https://forge.example.org/cairn/cairn">Repo</a>',
  });
  assert.equal(violations.length, 1);
  assert.equal(violations[0].host, 'forge.example.org');
  assert.equal(violations[0].repository, false);
});

test('an allowlisted host passes', () => {
  const {violations} = scan({
    'index.html': '<a href="https://docusaurus.io/docs/docusaurus.config.js/#baseUrl">baseUrl</a>',
  });
  assert.deepEqual(violations, []);
});

test('an absolute link to the site’s own origin is not external', () => {
  const {violations} = scan({
    'index.html': `<link rel=canonical href="${SITE_URL}/cairn/"><meta property=og:url content="${SITE_URL}/cairn/">`,
  });
  assert.deepEqual(violations, []);
});

test('an unquoted attribute value is scanned, as Docusaurus emits them', () => {
  const {violations} = scan({
    'index.html': '<a class=x href=https://gitlab.com/cairn/cairn rel=noopener>x</a>',
  });
  assert.equal(violations.length, 1);
  assert.equal(violations[0].host, 'gitlab.com');
  assert.equal(violations[0].repository, true);
});

test('a link inside an inline script payload is scanned', () => {
  // theme-classic inlines fallback HTML into a script string. A link a reader
  // can follow is a link, whichever element it was smuggled through.
  const {violations} = scan({
    'index.html':
      '<script>document.write("<a href=\\"https://bitbucket.org/cairn\\">src</a>")</script>',
  });
  assert.equal(violations.length, 1);
  assert.equal(violations[0].host, 'bitbucket.org');
});

test('`src` is scanned as well as `href`', () => {
  const {violations} = scan({
    'index.html': '<img src="https://raw.githubusercontent.com/joestump/cairn/main/logo.png">',
  });
  assert.equal(violations.length, 1);
  assert.equal(violations[0].attr, 'src');
});

test('non-HTML files are ignored', () => {
  const {files, violations} = scan({
    'index.html': '<a href=/cairn/>Home</a>',
    'sitemap.xml': '<loc>https://github.com/joestump/cairn</loc>',
  });
  assert.equal(files, 1);
  assert.deepEqual(violations, []);
});

test('scanning without a build says so rather than passing vacuously', () => {
  assert.throws(
    () => assertNoRepositoryLinks({outDir: join(tmpdir(), 'cairn-no-such-build'), siteUrl: SITE_URL}),
    /no built bundle/,
  );
});

test('externalLinksIn ignores relative, anchor, data and mailto targets', () => {
  const found = externalLinksIn(
    '<a href=/cairn/docs/intro>a</a><a href=#context>b</a>' +
      '<a href="mailto:hi@example.com">c</a><img src="data:image/svg+xml;utf8,<svg/>">',
  );
  assert.deepEqual(found, []);
});

test('externalLinksIn reads meta content, so a forge URL cannot hide in a meta tag', () => {
  const found = externalLinksIn(
    '<meta property="og:see_also" content="https://github.com/joestump/cairn">',
  );
  assert.deepEqual(
    found.map((link) => [link.attr, link.host]),
    [['content', 'github.com']],
  );
});

test('externalLinksIn splits srcset candidates off their descriptors', () => {
  const found = externalLinksIn(
    '<img srcset="/cairn/a.png 1x, https://raw.githubusercontent.com/o/r/b.png 2x">',
  );
  assert.deepEqual(
    found.map((link) => [link.attr, link.url]),
    [['srcset', 'https://raw.githubusercontent.com/o/r/b.png']],
  );
});

test('externalLinksIn keeps a comma inside a single-URL attribute intact', () => {
  const found = externalLinksIn('<a href="https://example.com/a,b">x</a>');
  assert.deepEqual(
    found.map((link) => link.url),
    ['https://example.com/a,b'],
  );
});

/* ------------------------------------------------------ the configured site */

test('a config with no edit URL passes', () => {
  const {summary} = assertNoEditUrl({
    presets: [['classic', {docs: {sidebarPath: './sidebars.ts'}, blog: false}]],
  });
  assert.match(summary, /no edit URL configured/);
});

test('an editUrl on the docs preset fails, naming the configuration key', () => {
  assert.throws(
    () =>
      assertNoEditUrl({
        presets: [
          [
            'classic',
            {docs: {editUrl: 'https://github.com/joestump/cairn/tree/main/website/'}},
          ],
        ],
      }),
    /presets\[0\]\[1\]\.docs\.editUrl/,
  );
});

test('an editUrl hidden on a standalone plugin entry fails too', () => {
  // The generic walk is the point: guarding only `presets[0][1].docs` would
  // wave this through, and a second docs instance is exactly how it comes back.
  assert.throws(
    () =>
      assertNoEditUrl({
        plugins: [['@docusaurus/plugin-content-docs', {id: 'extra', editUrl: 'https://forge.example.org/'}]],
      }),
    /plugins\[0\]\[1\]\.editUrl/,
  );
});

test('an editUrl function is caught, not just a string', () => {
  assert.throws(
    () => assertNoEditUrl({presets: [['classic', {docs: {editUrl: () => 'https://github.com/x'}}]]}),
    /editUrl/,
  );
});

test('a cyclic config object terminates', () => {
  const config = {presets: []};
  config.self = config;
  assert.match(assertNoEditUrl(config).summary, /no edit URL/);
});
