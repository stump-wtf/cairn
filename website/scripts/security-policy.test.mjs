#!/usr/bin/env node --test
/**
 * Governing: ADR-0014, SPEC-0010 REQ "Security Headers"
 *
 * The policy is data, and data that is wrong in a subtle way is worse here than
 * almost anywhere else on the site: a CSP with a typo'd directive name is still
 * a valid-looking header that enforces nothing, and a CSP with a missing hash
 * silently blanks the colour-mode bootstrap on every page. Both fail here
 * instead.
 */

import {test} from 'node:test';
import assert from 'node:assert/strict';
import {createHash} from 'node:crypto';

import {
  RESPONSE_HEADERS,
  headerPolicy,
  inlineScriptHashes,
  isExecutableScript,
  metaPolicy,
  renderHeadersFile,
  scriptHash,
} from './security-policy.mjs';

const REQUIRED = ['default-src', 'script-src', 'style-src', 'font-src', 'img-src'];

test('the policy carries every directive the requirement names, restricted to self', () => {
  for (const channel of [metaPolicy(), headerPolicy()]) {
    for (const directive of REQUIRED) {
      const m = new RegExp(`(?:^|; )${directive} ([^;]+)`).exec(channel);
      assert.ok(m, `${directive} missing from: ${channel}`);
      assert.match(m[1], /'self'/, `${directive} does not admit the site's own origin`);
      assert.doesNotMatch(
        m[1],
        /https?:|\*|'unsafe-eval'/,
        `${directive} admits an origin beyond the site's own`,
      );
    }
  }
});

test("script-src does not admit 'unsafe-inline'", () => {
  // The whole reason the build hashes the two inline scripts.
  const script = /(?:^|; )script-src ([^;]+)/.exec(headerPolicy())[1];
  assert.doesNotMatch(script, /'unsafe-inline'/);
});

test('frame-ancestors is on the header channel and absent from the meta channel', () => {
  // CSP specifies frame-ancestors to be IGNORED when delivered by <meta>, so
  // emitting it there would be a directive that warns and does nothing.
  assert.match(headerPolicy(), /frame-ancestors 'none'/);
  assert.doesNotMatch(metaPolicy(), /frame-ancestors/);
});

test('the response headers the requirement names are all present', () => {
  const names = RESPONSE_HEADERS.map(([name]) => name.toLowerCase());
  for (const required of [
    'x-content-type-options',
    'referrer-policy',
    'strict-transport-security',
  ]) {
    assert.ok(names.includes(required), `${required} missing`);
  }
  const [, nosniff] = RESPONSE_HEADERS.find(([n]) => n === 'X-Content-Type-Options');
  assert.equal(nosniff, 'nosniff');
});

test('HSTS does not preload — the site has no settled domain to commit', () => {
  const [, hsts] = RESPONSE_HEADERS.find(([n]) => n === 'Strict-Transport-Security');
  assert.match(hsts, /max-age=\d+/);
  assert.doesNotMatch(hsts, /preload/);
});

test('hashes are sha256-base64 of the exact script body', () => {
  const body = 'console.log("hi")';
  const expected = createHash('sha256').update(body, 'utf8').digest('base64');
  assert.equal(scriptHash(body), `'sha256-${expected}'`);
});

test('a hashed policy names every hash it was given', () => {
  const hashes = [scriptHash('a'), scriptHash('b')];
  const policy = metaPolicy(hashes);
  for (const hash of hashes) assert.ok(policy.includes(hash), policy);
});

test('inline script classification: executable versus data block', () => {
  assert.equal(isExecutableScript(''), true);
  assert.equal(isExecutableScript(' data-rh=true'), true);
  assert.equal(isExecutableScript(' type="module"'), true);
  assert.equal(isExecutableScript(' type="text/javascript"'), true);
  // The one that matters: Docusaurus emits a per-page JSON-LD breadcrumb, and
  // hashing it would put one hash per page into the policy.
  assert.equal(isExecutableScript(' type="application/ld+json"'), false);
  assert.equal(isExecutableScript(' type="text/template"'), false);
});

test('hashes cover the executable inline scripts and nothing else', () => {
  const html = [
    '<html><head>',
    '<script src="/cairn/assets/js/main.js"></script>',
    '<script type="application/ld+json">{"@type":"BreadcrumbList"}</script>',
    '<script>document.documentElement.dataset.theme="dark"</script>',
    '</head></html>',
  ].join('\n');

  const hashes = inlineScriptHashes(html);
  assert.equal(hashes.length, 1);
  assert.equal(hashes[0], scriptHash('document.documentElement.dataset.theme="dark"'));
});

test('the policy is safe inside a double-quoted meta attribute', () => {
  // The build asserts this too; here it is checked against the policy itself so
  // a future directive with a quoted value fails in the test rather than in the
  // build of whoever pulls next.
  assert.doesNotMatch(metaPolicy([scriptHash('x')]), /"/);
});

test('_headers renders one rule set applying to every path', () => {
  const file = renderHeadersFile([scriptHash('x')]);
  assert.match(file, /^\/\*$/m);
  assert.match(file, /^ {2}Content-Security-Policy: /m);
  for (const [name] of RESPONSE_HEADERS) {
    assert.match(file, new RegExp(`^ {2}${name}: `, 'm'));
  }
});
