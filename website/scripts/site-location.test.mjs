#!/usr/bin/env node --test
/**
 * Tests for scripts/site-location.mjs: what a build bakes into its canonical
 * links, og:url and sitemap when it is, and is not, told where it is served.
 *
 * Governing: ADR-0014, SPEC-0010 REQ "Build and Deployment"
 *
 * The Pages host here is a stand-in with the real shape, `<owner>.pages.<domain>`,
 * so no private host name lands in the repository's published tree.
 */

import {test} from 'node:test';
import assert from 'node:assert/strict';

import {
  PAGES_BASE_URL,
  PUBLIC_ORIGIN,
  isPagesHost,
  resolveSiteLocation,
} from './site-location.mjs';

const PAGES_URL = 'https://stump-wtf.pages.example.org';

test('an unconfigured build advertises the public origin, rooted at /', () => {
  assert.equal(PUBLIC_ORIGIN, 'https://cairn.stump.wtf');
  assert.deepEqual(resolveSiteLocation({}), {url: PUBLIC_ORIGIN, baseUrl: '/'});
});

test('empty strings from CI plumbing fall back like unset variables', () => {
  assert.deepEqual(resolveSiteLocation({DOCS_URL: '', DOCS_BASE_URL: ''}), {
    url: PUBLIC_ORIGIN,
    baseUrl: '/',
  });
});

test('the app-domain build gets exactly what it asks for', () => {
  assert.deepEqual(
    resolveSiteLocation({DOCS_URL: 'https://cairn.stump.wtf', DOCS_BASE_URL: '/'}),
    {url: 'https://cairn.stump.wtf', baseUrl: '/'},
  );
});

test('a Pages build that passes only DOCS_URL is based at the repo path', () => {
  assert.deepEqual(resolveSiteLocation({DOCS_URL: PAGES_URL}), {
    url: PAGES_URL,
    baseUrl: PAGES_BASE_URL,
  });
  assert.equal(PAGES_BASE_URL, '/cairn/');
});

test('an explicit DOCS_BASE_URL always wins', () => {
  assert.deepEqual(resolveSiteLocation({DOCS_URL: PAGES_URL, DOCS_BASE_URL: '/'}), {
    url: PAGES_URL,
    baseUrl: '/',
  });
  assert.deepEqual(resolveSiteLocation({DOCS_BASE_URL: '/cairn/'}), {
    url: PUBLIC_ORIGIN,
    baseUrl: '/cairn/',
  });
});

test('isPagesHost matches the <owner>.pages.<domain> shape only', () => {
  assert.equal(isPagesHost(PAGES_URL), true);
  assert.equal(isPagesHost(`${PAGES_URL}/`), true);
  // Positive control above; everything below must be rejected.
  assert.equal(isPagesHost('https://cairn.stump.wtf'), false);
  assert.equal(isPagesHost('https://pages.example.org'), false);
  // A root-served Pages product (one project per host) is not this shape.
  assert.equal(isPagesHost('https://cairn.pages.dev'), false);
  assert.equal(isPagesHost('https://docs.example.org/pages/'), false);
  assert.equal(isPagesHost('not a url'), false);
  assert.equal(isPagesHost(''), false);
});
