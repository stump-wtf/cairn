#!/usr/bin/env node --test
/**
 * Governing: ADR-0014, SPEC-0010 REQ "First-Party Asset Loading",
 *            SPEC-0010 REQ "Security Headers",
 *            SPEC-0010 REQ "Deployment Least Privilege"
 *
 * Negative tests for the bundle scan, over synthetic bundles that break exactly
 * one thing each — plus the positive cases that keep it usable. A deploy gate
 * that fails on the record's own prose gets bypassed, and a bypassed gate is
 * indistinguishable from no gate, so the false-positive tests here are load
 * bearing rather than decorative. The `outline.button--active` case in
 * particular is a bug this scanner actually had on its first run.
 */

import {test} from 'node:test';
import assert from 'node:assert/strict';
import {mkdtempSync, mkdirSync, writeFileSync} from 'node:fs';
import {join, dirname} from 'node:path';
import {tmpdir} from 'node:os';

import {scanBundle} from './scan-bundle.mjs';

/** A bundle from a `{path: contents}` map. */
function bundle(files) {
  const out = mkdtempSync(join(tmpdir(), 'cairn-bundle-'));
  for (const [name, contents] of Object.entries(files)) {
    const file = join(out, name);
    mkdirSync(dirname(file), {recursive: true});
    writeFileSync(file, contents);
  }
  return out;
}

const page = (head, body = '') =>
  `<!doctype html><html><head>${head}</head><body>${body}</body></html>`;

const ok = (files) => scanBundle({outDir: bundle(files)}).failures;

/* -------------------------------------------------------- third-party assets */

test('fails on a remote stylesheet', () => {
  const failures = ok({
    'index.html': page('<link rel="stylesheet" href="https://cdn.example.com/a.css">'),
  });
  assert.equal(failures.length, 1);
  assert.match(failures[0], /cdn\.example\.com/);
  assert.match(failures[0], /First-Party Asset Loading/);
});

test('fails on a remote font @import that arrived through a dependency', () => {
  // The exact regression this replaces: nothing under src/ names the host, so a
  // source-level check cannot see it. Only the emitted CSS can.
  const failures = ok({
    'index.html': page('<link rel="stylesheet" href="/assets/s.css">'),
    'assets/s.css': "@import url('https://fonts.googleapis.com/css2?family=X');",
  });
  assert.equal(failures.length, 1);
  assert.match(failures[0], /fonts\.googleapis\.com/);
});

test('fails on a remote font in a CSS url()', () => {
  const failures = ok({
    'a.css': "@font-face{src:url(https://fonts.gstatic.com/s/x.woff2) format('woff2')}",
  });
  assert.equal(failures.length, 1);
  assert.match(failures[0], /gstatic/);
});

test('fails on a protocol-relative subresource', () => {
  const failures = ok({'index.html': page('<script src="//cdn.example.com/x.js"></script>')});
  assert.equal(failures.length, 1);
});

test('fails on a remote image and on a remote srcset candidate', () => {
  const failures = ok({
    'index.html': page('', '<img src="https://img.example.com/a.png" srcset="https://img.example.com/b.png 2x">'),
  });
  assert.equal(failures.length, 2);
});

test('fails on a remote asset injected from a script', () => {
  const failures = ok({
    'assets/main.js': 'l.href="https://cdn.example.com/theme.css";document.head.append(l)',
  });
  assert.equal(failures.length, 1);
  assert.match(failures[0], /asset URL in script/);
});

test('fails on a remote url() hidden in an inline style attribute', () => {
  const failures = ok({
    'index.html': page('', '<div style="background:url(https://cdn.example.com/bg.png)"></div>'),
  });
  assert.equal(failures.length, 1);
});

test('passes a bundle whose every subresource is same-origin', () => {
  const failures = ok({
    'index.html': page(
      '<link rel="stylesheet" href="/cairn/assets/s.css"><link rel="canonical" href="https://site.example/cairn/">',
      '<img src="/cairn/img/logo.svg"><script src="/cairn/assets/js/main.js"></script>',
    ),
    'assets/s.css': "@font-face{src:url(./fonts/x.woff2) format('woff2')}",
  });
  assert.deepEqual(failures, []);
});

test('a canonical or alternate <link> is metadata, not a subresource', () => {
  // A correct canonical tag names an absolute URL by definition. Treating it as
  // a fetch would make the site unpublishable.
  const failures = ok({
    'index.html': page(
      '<link rel="canonical" href="https://site.example/cairn/x">' +
        '<link rel="alternate" hreflang="en" href="https://site.example/cairn/x">',
    ),
  });
  assert.deepEqual(failures, []);
});

test('an <a href> to another site is a navigation, not a subresource', () => {
  // Which links the site may carry is REQ "No Repository Links" — a host
  // allowlist, a different rule. This scan must not pre-empt it.
  const failures = ok({'index.html': page('', '<a href="https://example.com/x">x</a>')});
  assert.deepEqual(failures, []);
});

test('a data: URI is bytes already in the file, not a request', () => {
  const failures = ok({
    'index.html': page('<link rel="icon" href="data:image/svg+xml;base64,PHN2Zz48L3N2Zz4=">'),
  });
  assert.deepEqual(failures, []);
});

/* ------------------------------------------------- credentials, private hosts */

test('fails on a credential in the bundle', () => {
  for (const secret of [
    'AKIAIOSFODNN7EXAMPLE',
    'ghp_0123456789abcdef0123456789abcdef0123',
    '-----BEGIN RSA PRIVATE KEY-----',
    'https://user:hunter2@example.com/x',
  ]) {
    const failures = ok({'assets/main.js': `var x=${JSON.stringify(secret)};`});
    assert.equal(failures.length, 1, `${secret} was not caught`);
    assert.match(failures[0], /Deployment Least Privilege/);
  }
});

test('fails on a private infrastructure hostname', () => {
  const failures = ok({'index.html': page('', '<p>gitea.stump.rocks</p>')});
  assert.equal(failures.length, 1);
  assert.match(failures[0], /private forge hostname/);
});

test('fails on an RFC1918 address', () => {
  const failures = ok({'index.html': page('', '<code>10.0.4.17</code>')});
  assert.equal(failures.length, 1);
});

test("the product's own public hostname is not a private host", () => {
  // `cairn.stump.rocks` is where Cairn is meant to live and appears in the
  // record's examples two dozen times. A deny list that swept the whole domain
  // would fail this build on correct prose.
  const failures = ok({'index.html': page('', '<p>https://cairn.stump.rocks/a/x7Kd2</p>')});
  assert.deepEqual(failures, []);
});

test('an identifier that merely contains a dot is not a hostname', () => {
  // The scanner's first run failed the real build on `outline.button--active`,
  // an Infima class name in the emitted stylesheet.
  const failures = ok({
    'assets/s.css': '.outline.button--active{color:red}.gitea.thing{color:blue}',
  });
  assert.deepEqual(failures, []);
});

test('loopback addresses stay publishable — the CLI docs are full of them', () => {
  const failures = ok({
    'index.html': page('', '<code>cairnd --listen 127.0.0.1:8080</code> or localhost:3000'),
  });
  assert.deepEqual(failures, []);
});

test('a content hash is not a secret', () => {
  // The bundle is made of these. A generic entropy heuristic would report every
  // filename in it.
  const failures = ok({
    'index.html': page('<link rel="stylesheet" href="/assets/css/styles.238042b6.css">'),
  });
  assert.deepEqual(failures, []);
});

test('a missing bundle is an error, not a pass', () => {
  assert.throws(
    () => scanBundle({outDir: join(tmpdir(), 'cairn-no-such-bundle-xyz')}),
    /no built bundle/,
  );
});
