#!/usr/bin/env node --test
/**
 * Governing: ADR-0014, SPEC-0010 REQ "First-Party Asset Loading"
 *
 * The source-level half of the requirement, driven over fixture site roots. The
 * scenario this exists for is stated verbatim in the spec — *"WHEN site source
 * contains a stylesheet import or link naming a third-party font host, THEN the
 * build MUST fail and MUST name the file and the host"* — so the tests assert
 * on both halves of that: the failure, and the naming.
 *
 * The last test runs the checker over the REAL site, which is what stops this
 * capability regressing to a Google Fonts `@import` while all the synthetic
 * fixtures stay green.
 */

import {test} from 'node:test';
import assert from 'node:assert/strict';
import {mkdtempSync, mkdirSync, writeFileSync} from 'node:fs';
import {join, dirname} from 'node:path';
import {tmpdir} from 'node:os';
import {fileURLToPath} from 'node:url';

import {checkAssets} from './check-assets.mjs';

const WEBSITE = join(fileURLToPath(new URL('.', import.meta.url)), '..');

function site(files) {
  const root = mkdtempSync(join(tmpdir(), 'cairn-assets-'));
  for (const [name, contents] of Object.entries(files)) {
    const file = join(root, name);
    mkdirSync(dirname(file), {recursive: true});
    writeFileSync(file, contents);
  }
  return root;
}

const run = (files) => checkAssets({root: site(files)}).failures;

test('fails on the Google Fonts @import this capability removed', () => {
  const failures = run({
    'src/css/custom.css':
      "@import url('https://fonts.googleapis.com/css2?family=IBM+Plex+Sans&display=swap');\n:root{--x:1}",
  });
  assert.equal(failures.length, 1);
  assert.match(failures[0], /src\/css\/custom\.css:1/);
  assert.match(failures[0], /fonts\.googleapis\.com/);
  assert.match(failures[0], /font\/asset CDN/);
});

test('fails on a remote @font-face src, not only on an @import', () => {
  const failures = run({
    'src/css/fonts.css':
      "@font-face{font-family:'X';src:url('https://fonts.gstatic.com/s/x.woff2')}",
  });
  assert.equal(failures.length, 1);
  assert.match(failures[0], /fonts\.gstatic\.com/);
});

test('fails on a preconnect or stylesheet <link> in a component', () => {
  const failures = run({
    'src/pages/index.tsx':
      'export default () => (<><link rel="preconnect" href="https://fonts.gstatic.com" />' +
      '<link rel="stylesheet" href="https://cdn.example.com/a.css" /></>);',
  });
  assert.equal(failures.length, 2);
});

test('fails on a third-party <script src> in the site config', () => {
  const failures = run({
    'docusaurus.config.ts':
      "export default {scripts: ['<script src=\"https://plausible.io/js/s.js\"></script>']};",
  });
  assert.equal(failures.length, 1);
  assert.match(failures[0], /plausible\.io/);
});

test('fails on a host it has never heard of — the rule is remote, not a blocklist', () => {
  const failures = run({
    'src/css/custom.css': "@import url('https://type.some-new-foundry.example/x.css');",
  });
  assert.equal(failures.length, 1);
  assert.match(failures[0], /type\.some-new-foundry\.example/);
  assert.doesNotMatch(failures[0], /font\/asset CDN/);
});

test('passes a relative font reference', () => {
  const failures = run({
    'src/css/custom.css':
      "@font-face{font-family:'X';src:url('./fonts/x.woff2') format('woff2')}",
  });
  assert.deepEqual(failures, []);
});

test('passes a data: URI', () => {
  const failures = run({
    'src/css/custom.css': "body{background:url('data:image/svg+xml;base64,PHN2Zz4=')}",
  });
  assert.deepEqual(failures, []);
});

test('a URL discussed in a comment is not an asset reference', () => {
  // This checker's own docblock names five font CDNs. So does the record.
  const failures = run({
    'src/pages/index.tsx':
      "// We used to @import url('https://fonts.googleapis.com/css2?family=X').\nexport default () => null;",
    'src/css/custom.css':
      "/* Formerly @import url('https://fonts.googleapis.com/css2'); now self-hosted. */\n:root{--x:1}",
  });
  assert.deepEqual(failures, []);
});

test('the real site loads no third-party asset', () => {
  const result = checkAssets({root: WEBSITE});
  assert.deepEqual(result.failures, []);
  assert.ok(result.scanned > 0, 'the checker scanned nothing, so it proved nothing');
});
