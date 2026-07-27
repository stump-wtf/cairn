#!/usr/bin/env node --test
/**
 * Negative tests for scripts/check-tokens.mjs. A guard that cannot fail is not
 * a guard, so each check is driven over a fixture that breaks exactly it — and
 * every bypass that has been found in this checker keeps a test here, so none
 * of them can be reintroduced quietly.
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
import {cpSync, mkdirSync, mkdtempSync, readFileSync, writeFileSync, appendFileSync} from 'node:fs';
import {join} from 'node:path';
import {tmpdir} from 'node:os';
import {spawnSync} from 'node:child_process';
import {fileURLToPath} from 'node:url';
import {adr0009Categories} from './check-tokens.mjs';

const SCRIPTS = fileURLToPath(new URL('.', import.meta.url));
const WEBSITE = join(SCRIPTS, '..');
const CHECKER = join(SCRIPTS, 'check-tokens.mjs');
const REPO_ROOT = join(WEBSITE, '..');

/** Copy the real src/ tree into a scratch website root and hand back its paths. */
function fixture() {
  const root = mkdtempSync(join(tmpdir(), 'cairn-tokens-'));
  cpSync(join(WEBSITE, 'src'), join(root, 'src'), {recursive: true});
  return {
    root,
    tokenFile: join(root, 'src', 'css', 'custom.css'),
    moduleFile: join(root, 'src', 'pages', 'index.module.css'),
    tileFile: join(root, 'src', 'components', 'HomepageFeatures', 'styles.module.css'),
    pageFile: join(root, 'src', 'pages', 'index.tsx'),
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

/* ---------------------------------------------------------- colour literals */

test('a hex literal in a component stylesheet fails, naming file, line and literal', () => {
  const {root, moduleFile} = fixture();
  appendFileSync(moduleFile, '\n.leak { color: #ff00ff; }\n');
  const {status, out} = run(root);
  assert.equal(status, 1);
  assert.match(out, /src\/pages\/index\.module\.css:\d+/);
  assert.match(out, /#ff00ff/);
  /* Reported once, not once per scanning pass. */
  assert.equal(out.match(/#ff00ff/g).length, 1);
});

test('a hex literal under the universal selector fails', () => {
  const {root, moduleFile} = fixture();
  appendFileSync(moduleFile, '\n* { color: #ff00ff; }\n');
  const {status, out} = run(root);
  assert.equal(status, 1, out);
  assert.match(out, /#ff00ff/);
});

test('a hex literal on a line that starts with a comment fails', () => {
  const {root, moduleFile} = fixture();
  appendFileSync(moduleFile, '\n/* a note */ .leak { color: #ff00ff; }\n');
  const {status, out} = run(root);
  assert.equal(status, 1, out);
  assert.match(out, /#ff00ff/);
});

test('a hex literal inside a block comment does not fail', () => {
  const {root, moduleFile} = fixture();
  appendFileSync(moduleFile, '\n/*\n * historically #ff00ff\n */\n');
  assert.equal(run(root).status, 0);
});

test('a named colour outside the old 22-word shortlist fails', () => {
  const {root, moduleFile} = fixture();
  appendFileSync(moduleFile, '\n.leak { color: crimson; }\n');
  const {status, out} = run(root);
  assert.equal(status, 1, out);
  assert.match(out, /named colour literal `crimson`/);
});

test('a named colour fails whatever its case', () => {
  const {root, moduleFile} = fixture();
  appendFileSync(moduleFile, '\n.leak { background: WhiteSmoke; }\n');
  const {status, out} = run(root);
  assert.equal(status, 1, out);
  assert.match(out, /named colour literal `WhiteSmoke`/);
});

test('a colour name used as a class name is not a colour literal', () => {
  const {root, moduleFile} = fixture();
  appendFileSync(moduleFile, '\n.tan { padding: 1px; }\n.snow { padding: 1px; }\n');
  assert.equal(run(root).status, 0);
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

test('a named colour in an inline style fails', () => {
  const {root, pageFile} = fixture();
  appendFileSync(pageFile, "\nexport const leak = {color: 'black'};\n");
  const {status, out} = run(root);
  assert.equal(status, 1, out);
  assert.match(out, /named colour literal `black` in an inline style/);
});

test('a hex literal in a .tsx fails', () => {
  const {root, pageFile} = fixture();
  appendFileSync(pageFile, "\nexport const leak = {backgroundColor: '#ff00ff'};\n");
  const {status, out} = run(root);
  assert.equal(status, 1, out);
  assert.match(out, /#ff00ff/);
});

test('the word "black" in prose is not a colour literal', () => {
  const {root, pageFile} = fixture();
  appendFileSync(pageFile, '\nexport const copy = <p>A black box no longer.</p>;\n');
  assert.equal(run(root).status, 0);
});

test('heading anchors and URL fragments are not colour literals', () => {
  const {root, pageFile} = fixture();
  appendFileSync(
    pageFile,
    '\nexport const links = (\n' +
      '  <>\n' +
      '    <a href="#added">a</a>\n' +
      '    <a href="#dead">b</a>\n' +
      '    <a href="/docs/x#facade">c</a>\n' +
      '  </>\n' +
      ');\n',
  );
  const {status, out} = run(root);
  assert.equal(status, 0, out);
});

/* -------------------------------------------------------- category palette */

test('recolouring an ADR-0009 category fails', () => {
  const {root, tokenFile} = fixture();
  edit(tokenFile, '#f0506a;', '#ff0000;');
  const {status, out} = run(root);
  assert.equal(status, 1);
  assert.match(out, /--cat-fail is #ff0000 but shipped as #f0506a/);
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

test('adding a category through a var() indirection fails', () => {
  const {root, tokenFile} = fixture();
  appendFileSync(tokenFile, '\n:root { --cat-banana: var(--cat-read); }\n');
  const {status, out} = run(root);
  assert.equal(status, 1, out);
  assert.match(out, /--cat-banana is not an ADR-0009 category token/);
});

test('re-pointing a category in another colour mode fails', () => {
  const {root, tokenFile} = fixture();
  appendFileSync(tokenFile, '\n[data-theme="dark"] { --cat-fail: var(--cat-exec); }\n');
  const {status, out} = run(root);
  assert.equal(status, 1, out);
  assert.match(out, /--cat-fail is declared 2 times/);
  assert.match(out, /redeclared under `\[data-theme="dark"\]`/);
});

test('a category expressed as color-mix() rather than a literal fails', () => {
  const {root, tokenFile} = fixture();
  edit(
    tokenFile,
    '--cat-fail:    #f0506a;',
    '--cat-fail: color-mix(in srgb, #f0506a 90%, #ffffff);',
  );
  const {status, out} = run(root);
  assert.equal(status, 1, out);
  assert.match(out, /--cat-fail is `color-mix\(.*\)`, not a hex literal/);
});

test('declaring a category token outside the token definition fails', () => {
  const {root, moduleFile} = fixture();
  appendFileSync(moduleFile, '\n.leak { --cat-fail: var(--cat-exec); }\n');
  const {status, out} = run(root);
  assert.equal(status, 1, out);
  assert.match(out, /declares --cat-fail/);
});

test('dropping a category chip rule fails', () => {
  const {root, tokenFile} = fixture();
  edit(tokenFile, '.chip-meta    { --chip-accent: var(--cat-meta); }', '');
  const {status, out} = run(root);
  assert.equal(status, 1, out);
  assert.match(out, /no chip rule for the ADR-0009 category "meta"/);
});

test('a tile class ADR-0009 does not define must resolve to the neutral default', () => {
  const {root, tileFile} = fixture();
  edit(
    tileFile,
    '.cat_mono    { --accent: var(--cat-other); }',
    '.cat_mono    { --accent: var(--cat-meta); }',
  );
  const {status, out} = run(root);
  assert.equal(status, 1, out);
  assert.match(out, /tile "mono" is not an ADR-0009 category/);
});

/* ------------------------------------------------------------ type families */

test('naming a font family outside the token definition fails', () => {
  const {root, moduleFile} = fixture();
  appendFileSync(moduleFile, "\n.leak { font-family: 'JetBrains Mono', monospace; }\n");
  const {status, out} = run(root);
  assert.equal(status, 1);
  assert.match(out, /font-family names a family directly/);
});

test('font-family: inherit names no family and is allowed', () => {
  const {root, moduleFile} = fixture();
  appendFileSync(moduleFile, '\n.ok { font-family: inherit; }\n');
  const {status, out} = run(root);
  assert.equal(status, 0, out);
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

/* ----------------------------------------------------------------- contrast */

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

test('contrast is measured through a var() chain, not only literals', () => {
  const {root, tokenFile} = fixture();
  /* --cairn-ink-0 becomes an indirection to a ground no accent can clear. The
     old checker measured only literal-valued tokens and skipped exactly this. */
  edit(
    tokenFile,
    '--cairn-ink-0:    #0a0b0d;',
    '--cairn-ink-pale: #cccccc;\n  --cairn-ink-0: var(--cairn-ink-pale);',
  );
  const {status, out} = run(root);
  assert.equal(status, 1, out);
  assert.match(out, /contrast: --cat-\w+ .* on --cairn-ink-0 .* below 4\.5:1/);
});

/* ---------------------------------------------------------------------- CLI */

test('--root without a directory reports an error rather than crashing', () => {
  const r = spawnSync(process.execPath, [CHECKER, '--root'], {encoding: 'utf8'});
  assert.equal(r.status, 2);
  assert.match(r.stdout + r.stderr, /--root needs a directory/);
  assert.doesNotMatch(r.stdout + r.stderr, /TypeError/);
});

/* ------------------------------------------------- ADR-0009 category parsing */

/** Write a throwaway repo holding just an ADR-0009 with the given body. */
function adrRepo(body) {
  const repo = mkdtempSync(join(tmpdir(), 'cairn-adr-'));
  mkdirSync(join(repo, 'docs', 'adrs'), {recursive: true});
  writeFileSync(join(repo, 'docs', 'adrs', 'ADR-0009-span-model.md'), body);
  return repo;
}

const OPS = '`reason · exec · read · net · write · search · plan · tool · analyze · test · fix · fail · meta`';
const PHASES = '`research · implementation · review · testing · debug · build · docs · delivery · deploy · wait`';

test('ADR-0009 parsing returns the union of both recommended vocabularies', () => {
  const names = adr0009Categories(adrRepo(`# ADR-0009\n\nBy operation kind, ${OPS}, or by workflow\nphase, ${PHASES}.\n`));
  // Neither vocabulary is a subset of the other, so both must survive: taking
  // only the longest list is what broke the build when phases were added.
  assert.ok(names.includes('reason'), 'operation-kind names missing');
  assert.ok(names.includes('research'), 'workflow-phase names missing');
  assert.equal(names.length, 23);
  assert.equal(new Set(names).size, names.length, 'names must be de-duplicated');
});

test('ADR-0009 parsing ignores a quoted subset of a vocabulary', () => {
  // The ADR quotes the original five in its consequences. That list adds no
  // name and must not be mistaken for a third vocabulary.
  const withSubset = adr0009Categories(
    adrRepo(`# ADR-0009\n\n${OPS} and ${PHASES}.\n\nThe original five were \`reason · exec · read · net · write\`.\n`),
  );
  const without = adr0009Categories(adrRepo(`# ADR-0009\n\n${OPS} and ${PHASES}.\n`));
  assert.deepEqual(withSubset.sort(), without.sort());
});

test('ADR-0009 parsing tolerates the same vocabulary stated twice', () => {
  // The real ADR states the operation-kind set in both its decision drivers
  // and its span-schema section. Equal-length lists are both maximal, so the
  // union must still de-duplicate rather than double-count.
  const names = adr0009Categories(adrRepo(`# ADR-0009\n\n${OPS}\n\nrestated: ${OPS}\n`));
  assert.equal(names.length, 13);
});

test('an ADR with no category list fails loudly rather than yielding nothing', () => {
  assert.throws(() => adr0009Categories(adrRepo('# ADR-0009\n\nNo lists here.\n')), /drifted/);
});

test('every category the real ADR-0009 names has a shipped accent', () => {
  // The cross-check that replaced the old subset rule: this is what now catches
  // a mistyped category name, so it gets a test of its own.
  const names = adr0009Categories(REPO_ROOT);
  const css = readFileSync(join(WEBSITE, 'src', 'css', 'custom.css'), 'utf8');
  for (const name of names) {
    assert.match(css, new RegExp(`--cat-${name}\\s*:`), `no --cat-${name} token for ADR-0009 category "${name}"`);
  }
});
