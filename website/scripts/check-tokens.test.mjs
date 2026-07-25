#!/usr/bin/env node --test
/**
 * Negative tests for scripts/check-tokens.mjs. A guard that cannot fail is not
 * a guard, so each check is driven over a fixture that breaks exactly it.
 *
 * Governing: ADR-0014, SPEC-0010 REQ "Design Token Source of Truth",
 *            SPEC-0010 REQ "Category Palette Immutability",
 *            SPEC-0010 REQ "Typographic Roles",
 *            SPEC-0010 REQ "Category Accents Are Dark-Ground Text Colours"
 *
 * Run with `npm run test:tokens`.
 */

import {test} from 'node:test';
import assert from 'node:assert/strict';
import {cpSync, mkdtempSync, readFileSync, writeFileSync, appendFileSync} from 'node:fs';
import {join} from 'node:path';
import {tmpdir} from 'node:os';
import {spawnSync} from 'node:child_process';
import {fileURLToPath} from 'node:url';

const SCRIPTS = fileURLToPath(new URL('.', import.meta.url));
const WEBSITE = join(SCRIPTS, '..');
const CHECKER = join(SCRIPTS, 'check-tokens.mjs');

/** Copy the real src/ tree into a scratch website root and hand back its paths. */
function fixture() {
  const root = mkdtempSync(join(tmpdir(), 'cairn-tokens-'));
  cpSync(join(WEBSITE, 'src'), join(root, 'src'), {recursive: true});
  return {
    root,
    tokenFile: join(root, 'src', 'css', 'custom.css'),
    moduleFile: join(root, 'src', 'pages', 'index.module.css'),
  };
}

function run(root) {
  const r = spawnSync(process.execPath, [CHECKER, '--root', root], {encoding: 'utf8'});
  return {status: r.status, out: r.stdout + r.stderr};
}

function edit(file, from, to) {
  const before = readFileSync(file, 'utf8');
  assert.ok(before.includes(from), `fixture precondition: ${from} present in ${file}`);
  writeFileSync(file, before.replace(from, to));
}

test('the shipped token layer passes', () => {
  const {root} = fixture();
  const {status, out} = run(root);
  assert.equal(status, 0, out);
  assert.match(out, /token-layer check OK/);
});

test('a hex literal in a component stylesheet fails, naming file, line and literal', () => {
  const {root, moduleFile} = fixture();
  appendFileSync(moduleFile, '\n.leak { color: #ff00ff; }\n');
  const {status, out} = run(root);
  assert.equal(status, 1);
  assert.match(out, /src\/pages\/index\.module\.css:\d+/);
  assert.match(out, /#ff00ff/);
});

test('a named colour in a component stylesheet fails', () => {
  const {root, moduleFile} = fixture();
  appendFileSync(moduleFile, '\n.leak { background: black; }\n');
  const {status, out} = run(root);
  assert.equal(status, 1);
  assert.match(out, /named colour literal `black`/);
});

test('an rgba() literal in a component stylesheet fails', () => {
  const {root, moduleFile} = fixture();
  appendFileSync(moduleFile, '\n.leak { box-shadow: 0 0 1px rgba(0, 0, 0, 0.5); }\n');
  const {status, out} = run(root);
  assert.equal(status, 1);
  assert.match(out, /colour function literal `rgba\(`/);
});

test('color-mix() in a component stylesheet is allowed', () => {
  const {root, moduleFile} = fixture();
  appendFileSync(
    moduleFile,
    '\n.ok { background: color-mix(in srgb, var(--cat-read) 12%, transparent); }\n',
  );
  assert.equal(run(root).status, 0);
});

test('recolouring an ADR-0009 category fails', () => {
  const {root, tokenFile} = fixture();
  edit(tokenFile, '#f0506a;', '#ff0000;');
  const {status, out} = run(root);
  assert.equal(status, 1);
  assert.match(out, /--cat-fail is #ff0000 but ADR-0009 ships #f0506a/);
});

test('removing an ADR-0009 category fails', () => {
  const {root, tokenFile} = fixture();
  edit(tokenFile, '--cat-meta:    #808c9a;   /* slate   */', '');
  const {status, out} = run(root);
  assert.equal(status, 1);
  assert.match(out, /--cat-meta is missing/);
});

test('adding a category ADR-0009 does not define fails', () => {
  const {root, tokenFile} = fixture();
  edit(tokenFile, '--cat-other:   #aab2bd;', '--cat-other:   #aab2bd;\n  --cat-banana: #ffee00;');
  const {status, out} = run(root);
  assert.equal(status, 1);
  assert.match(out, /--cat-banana is not an ADR-0009 category token/);
});

test('naming a font family outside the token definition fails', () => {
  const {root, moduleFile} = fixture();
  appendFileSync(moduleFile, "\n.leak { font-family: 'JetBrains Mono', monospace; }\n");
  const {status, out} = run(root);
  assert.equal(status, 1);
  assert.match(out, /font-family names a family directly/);
});

test('dropping the system fallbacks from a type role fails', () => {
  const {root, tokenFile} = fixture();
  edit(
    tokenFile,
    "--cairn-font-mono: 'JetBrains Mono', ui-monospace, SFMono-Regular, Menlo, monospace;",
    "--cairn-font-mono: 'JetBrains Mono';",
  );
  const {status, out} = run(root);
  assert.equal(status, 1);
  assert.match(out, /needs system fallbacks/);
});

test('an accent that stops clearing 4.5:1 on the ink ramp fails', () => {
  const {root, tokenFile} = fixture();
  // Lighten the ink ground rather than touch a category token, so the failure
  // is unambiguously the contrast check and not the palette check.
  edit(tokenFile, '--cairn-ink-0:    #0a0b0d;', '--cairn-ink-0:    #cccccc;');
  const {status, out} = run(root);
  assert.equal(status, 1);
  assert.match(out, /contrast: --cat-\w+ .* on --cairn-ink-0 .* below 4\.5:1/);
});

test('an accent that stops clearing 4.5:1 on its composited chip tint fails', () => {
  const {root, tokenFile} = fixture();
  edit(tokenFile, '--cairn-ink-100:  #131418;', '--cairn-ink-100:  #8a8a8a;');
  const {status, out} = run(root);
  assert.equal(status, 1);
  assert.match(out, /on its own 12% tint over --cairn-ink-100, below 4\.5:1/);
});
