#!/usr/bin/env node --test
/**
 * Negative tests for scripts/check-a11y.mjs. A guard that cannot fail is not a
 * guard, so each check is driven over a fixture that breaks exactly it — and
 * every gap this story found in the shipped site keeps a test here, so none of
 * them can come back quietly.
 *
 * Governing: ADR-0014, SPEC-0010 REQ "Contrast",
 *            SPEC-0010 REQ "Keyboard Navigation & Focus Management",
 *            SPEC-0010 REQ "WCAG 2.1 AA & Semantics"
 *
 * Run with `npm run test:a11y`.
 */

import {test} from 'node:test';
import assert from 'node:assert/strict';
import {
  appendFileSync,
  cpSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  writeFileSync,
} from 'node:fs';
import {join} from 'node:path';
import {tmpdir} from 'node:os';
import {spawnSync} from 'node:child_process';
import {fileURLToPath} from 'node:url';

const SCRIPTS = fileURLToPath(new URL('.', import.meta.url));
const WEBSITE = join(SCRIPTS, '..');
const CHECKER = join(SCRIPTS, 'check-a11y.mjs');

/** Copy the real src/ tree into a scratch website root and hand back its paths. */
function fixture({docs = true} = {}) {
  const root = mkdtempSync(join(tmpdir(), 'cairn-a11y-'));
  cpSync(join(WEBSITE, 'src'), join(root, 'src'), {recursive: true});
  if (docs) {
    mkdirSync(join(root, 'docs'), {recursive: true});
    writeFileSync(join(root, 'docs', 'page.md'), '# Title\n\n## Section\n\n### Detail\n');
  }
  return {
    root,
    tokenFile: join(root, 'src', 'css', 'custom.css'),
    tileFile: join(root, 'src', 'components', 'HomepageFeatures', 'styles.module.css'),
    moduleFile: join(root, 'src', 'pages', 'index.module.css'),
    docsDir: join(root, 'docs'),
  };
}

function run(root, ...extra) {
  const r = spawnSync(process.execPath, [CHECKER, '--root', root, ...extra], {encoding: 'utf8'});
  return {status: r.status, out: r.stdout + r.stderr};
}

/** A scratch directory of built HTML — the shape `postBuild` hands the checker. */
function builtSite(pages) {
  const out = mkdtempSync(join(tmpdir(), 'cairn-a11y-html-'));
  for (const [name, body] of Object.entries(pages)) {
    writeFileSync(join(out, name), `<!doctype html><html><body>${body}</body></html>`);
  }
  return out;
}

function edit(file, from, to) {
  const before = readFileSync(file, 'utf8');
  assert.ok(before.includes(from), `fixture precondition: ${from} present in ${file}`);
  writeFileSync(file, before.replace(from, to));
}

test('the shipped site passes', () => {
  const {root} = fixture();
  const {status, out} = run(root);
  assert.equal(status, 0, out);
  assert.match(out, /accessibility check OK/);
});

test('the real docs tree passes, staged record included', () => {
  /* Not a fixture: the point is that the 40 pages actually served are clean. */
  const {status, out} = run(WEBSITE);
  assert.equal(status, 0, out);
  assert.match(out, /one h1 and no skipped heading level/);
});

/* -------------------------------------------------------------- contrast */

test('a role token that drops below 4.5:1 as text fails, naming the pair and the ratio', () => {
  const {root, tokenFile} = fixture();
  /* --cairn-paper-700 is light mode's --cairn-muted. Lightened to a mid grey it
     still reads as "a muted colour" and no longer clears AA on white. */
  edit(tokenFile, '--cairn-paper-700:  #5c626c;', '--cairn-paper-700:  #9aa0a8;');
  const {status, out} = run(root);
  assert.equal(status, 1);
  assert.match(out, /contrast \(light\)/);
  assert.match(out, /--cairn-muted/);
  assert.match(out, /below 4\.5:1/);
});

test('a focus ring that fails 3:1 on a surface it can land on is reported', () => {
  const {root, tokenFile} = fixture();
  /* The footer and the code blocks pin the ink ramp in light mode, so a ring
     tuned only to the paper ramp is exactly the mistake this catches. */
  edit(
    tokenFile,
    '  --cairn-focus-ring: var(--ifm-color-primary);',
    '  --cairn-focus-ring: var(--cairn-paper-900);',
  );
  const {status, out} = run(root);
  assert.equal(status, 1);
  assert.match(out, /the focus indicator on --cairn-(code-bg|ink-deep)/);
  assert.match(out, /below 3:1/);
});

test('a token that cannot be resolved fails rather than being skipped', () => {
  const {root, tokenFile} = fixture();
  edit(tokenFile, '  --cairn-focus-ring: var(--ifm-color-primary);', '');
  const {status, out} = run(root);
  assert.equal(status, 1);
  assert.match(out, /--cairn-focus-ring is not declared at :root/);
});

/* ------------------------------------------------------- the banned colour */

test('the mockup tertiary #565E6E fails wherever it appears, token definition included', () => {
  const {root, tokenFile} = fixture();
  appendFileSync(tokenFile, '\n.breadcrumb { color: #565E6E; }\n');
  const {status, out} = run(root);
  assert.equal(status, 1);
  assert.match(out, /src\/css\/custom\.css:\d+/);
  assert.match(out, /#565E6E/i);
  assert.match(out, /var\(--cairn-muted\)/);
});

/* ---------------------------------------------------- the focus indicator */

test('a ring token with nothing painting it fails', () => {
  const {root, tokenFile} = fixture();
  edit(tokenFile, '  outline: var(--cairn-focus-ring-width) solid var(--cairn-focus-ring);', '');
  const {status, out} = run(root);
  assert.equal(status, 1);
  assert.match(out, /focus-visible/);
});

test('dropping the dark-mode ring fails: the two ramps need different rings', () => {
  const {root, tokenFile} = fixture();
  edit(tokenFile, '  --cairn-focus-ring: var(--cairn-ink-accent);', '');
  const {status, out} = run(root);
  assert.equal(status, 1);
  assert.match(out, /not re-declared for dark mode/);
});

/* ------------------------------------------------------------ reduced motion */

test('removing the global reduced-motion guard fails', () => {
  const {root, tokenFile} = fixture();
  edit(tokenFile, '@media (prefers-reduced-motion: reduce) {', '@media (min-width: 0px) {');
  const {status, out} = run(root);
  assert.equal(status, 1);
  assert.match(out, /prefers-reduced-motion/);
});

test('a new hover transform with no local guard fails, naming the selector', () => {
  const {root, moduleFile} = fixture();
  appendFileSync(moduleFile, '\n.card:hover { transform: translateY(-4px); }\n');
  const {status, out} = run(root);
  assert.equal(status, 1);
  assert.match(out, /src\/pages\/index\.module\.css:\d+/);
  assert.match(out, /moves on hover/);
});

test('the tile guard is what keeps the feature tiles passing', () => {
  const {root, tileFile} = fixture();
  edit(tileFile, '  .tile:hover { transform: none; }', '');
  const {status, out} = run(root);
  assert.equal(status, 1);
  assert.match(out, /HomepageFeatures\/styles\.module\.css/);
  assert.match(out, /moves on hover/);
});

test('a `transform: none` outside the reduced-motion block does not satisfy the guard', () => {
  /* The check has to ask what the file does WHEN reduced motion is requested.
     A neutralising declaration anywhere else — a reset, a base state, another
     media query — answers a different question, and accepting it would let the
     hover lift back in under a rule that reads like a guard. */
  const {root, tileFile} = fixture();
  edit(tileFile, '  .tile:hover { transform: none; }', '');
  appendFileSync(tileFile, '\n.decoy { transform: none; }\n');
  const {status, out} = run(root);
  assert.equal(status, 1);
  assert.match(out, /moves on hover/);
  assert.match(out, /no `transform: none` inside its/);
});

/* --------------------------------------------------- the documented ratios */

test('a quoted @ratio that no longer matches the tokens fails', () => {
  const {root, tokenFile} = fixture();
  edit(
    tokenFile,
    '@ratio light --ifm-color-primary on --cairn-paper-0   = 5.52:1',
    '@ratio light --ifm-color-primary on --cairn-paper-0   = 5.99:1',
  );
  const {status, out} = run(root);
  assert.equal(status, 1);
  assert.match(out, /claims --ifm-color-primary on --cairn-paper-0 measures 5\.99:1/);
  assert.match(out, /measures 5\.52:1/);
});

test('retuning a token drifts its documentation, and that fails the build', () => {
  /* The scenario the assertion syntax exists for: the ring still clears every
     floor, so no measured pair complains, and every ratio written beside it in
     the comment is now wrong. */
  const {root, tokenFile} = fixture();
  edit(tokenFile, '  --ifm-color-primary: #6b4ce6;', '  --ifm-color-primary: #5b3fd0;');
  const {status, out} = run(root);
  assert.equal(status, 1);
  assert.match(out, /the comment claims --ifm-color-primary/);
});

test('an @ratio naming a token that does not resolve fails rather than being skipped', () => {
  const {root, tokenFile} = fixture();
  edit(tokenFile, 'on --cairn-paper-0   = 5.52:1', 'on --cairn-nonexistent = 5.52:1');
  const {status, out} = run(root);
  assert.equal(status, 1);
  assert.match(out, /--cairn-nonexistent, which does not resolve/);
});

test('deleting every @ratio assertion fails: the section claims they are checked', () => {
  const {root, tokenFile} = fixture();
  const stripped = readFileSync(tokenFile, 'utf8').replaceAll('@ratio', 'ratio');
  writeFileSync(tokenFile, stripped);
  const {status, out} = run(root);
  assert.equal(status, 1);
  assert.match(out, /no @ratio assertions found/);
});

/* --------------------------------------------------------- heading order */

test('a page with two h1 headings fails', () => {
  const {root, docsDir} = fixture();
  writeFileSync(join(docsDir, 'page.md'), '# One\n\n## Section\n\n# Two\n');
  const {status, out} = run(root);
  assert.equal(status, 1);
  assert.match(out, /has 2 top-level headings/);
});

test('a page with no h1 fails', () => {
  const {root, docsDir} = fixture();
  writeFileSync(join(docsDir, 'page.md'), '## Section\n\nbody\n');
  const {status, out} = run(root);
  assert.equal(status, 1);
  assert.match(out, /has 0 top-level headings/);
});

test('a skipped heading level fails, naming the jump and the line', () => {
  const {root, docsDir} = fixture();
  writeFileSync(join(docsDir, 'page.md'), '# Title\n\n### Too deep\n');
  const {status, out} = run(root);
  assert.equal(status, 1);
  assert.match(out, /docs\/page\.md:3/);
  assert.match(out, /h1 → h3/);
});

test('a "#" inside a fenced code block is not a heading', () => {
  const {root, docsDir} = fixture();
  writeFileSync(
    join(docsDir, 'page.md'),
    '# Title\n\n```sh\n# a shell comment\n#### not a heading\n```\n\n## Section\n',
  );
  const {status, out} = run(root);
  assert.equal(status, 0, out);
});

test('front matter is stripped before headings are counted', () => {
  const {root, docsDir} = fixture();
  writeFileSync(join(docsDir, 'page.md'), '---\ntitle: x\n---\n\n# Title\n\n## Section\n');
  const {status, out} = run(root);
  assert.equal(status, 0, out);
});

/* ------------------------------------------------- rendered heading order */

test('a heading a component emits is caught in the HTML, where markdown cannot show it', () => {
  /* The shipped defect, reproduced: the page's markdown is one clean h1 and the
     jump only exists once the component has rendered. The source-level pass
     below is given the same page and passes it, which is the whole argument for
     checking the built output as well. */
  const {root, docsDir} = fixture();
  writeFileSync(join(docsDir, 'specs.mdx'), '# Specifications\n\n<SpecIndex />\n');
  const out = builtSite({
    'specs.html': '<h1>Specifications</h1><h3>SPEC-0001 Core</h3><h3>SPEC-0002 CLI</h3>',
  });

  assert.equal(run(root).status, 0, 'the markdown source is clean');

  const rendered = run(root, '--out', out);
  assert.equal(rendered.status, 1);
  assert.match(rendered.out, /specs\.html/);
  assert.match(rendered.out, /rendered heading level jumps h1 → h3/);
  assert.match(rendered.out, /SPEC-0001 Core/);
  assert.match(rendered.out, /came from a component/);
});

test('a rendered page with two h1 headings fails', () => {
  const {root} = fixture();
  const out = builtSite({'page.html': '<h1>One</h1><h2>Section</h2><h1>Two</h1>'});
  const {status, out: text} = run(root, '--out', out);
  assert.equal(status, 1);
  assert.match(text, /renders 2 top-level headings/);
  assert.match(text, /outline: h1 h2 h1/);
});

test('a heading tag quoted inside an inlined script is not a heading', () => {
  /* Docusaurus inlines its hydration payload, and that payload contains the
     page's own serialised markup. Counting those would fail every real page. */
  const {root} = fixture();
  const out = builtSite({
    'page.html':
      '<h1>Title</h1><h2>Section</h2>' +
      '<script>window.__DATA__ = "<h1>Title</h1><h4>deep</h4>";</script>',
  });
  const {status, out: text} = run(root, '--out', out);
  assert.equal(status, 0, text);
  assert.match(text, /1 rendered pages agree/);
});

test('an --out directory that does not exist fails rather than passing vacuously', () => {
  const {root} = fixture();
  const {status, out} = run(root, '--out', join(root, 'no-such-build'));
  assert.equal(status, 1);
  assert.match(out, /no built output to check/);
});
