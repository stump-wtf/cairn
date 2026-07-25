// Governing: ADR-0014, SPEC-0010 REQ "Derived Status, Dates, and Counts"
// Governing: ADR-0014, SPEC-0010 REQ "Referential Integrity and Front-Matter Validation"
//
// A guard that cannot fail is not a guard. Every case below is a claim the
// checker is supposed to catch or a shape it is supposed to leave alone, and the
// two halves matter equally: a hardcoded-count check that fires on a viewBox
// coordinate gets deleted within a week and then catches nothing at all.

import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import {after, before, describe, it} from 'node:test';

import {assertNoLiteralCounts} from '../counts.ts';
import {createRecordPaths} from '../paths.ts';
import type {RecordCounts} from '../types.ts';

/** The live counts the fixture site is checked against. */
const COUNTS: RecordCounts = {
  decisions: 14,
  specifications: 10,
  requirements: 219,
  scenarios: 359,
};

let siteDir: string;

before(async () => {
  siteDir = await fs.mkdtemp(path.join(os.tmpdir(), 'cairn-counts-'));
  await fs.mkdir(path.join(siteDir, 'src', 'pages'), {recursive: true});
  await fs.mkdir(path.join(siteDir, 'docs'), {recursive: true});
});

after(async () => {
  await fs.rm(siteDir, {recursive: true, force: true});
});

/** Write one site source file and run the check over the fixture site. */
async function check(
  relPath: string,
  source: string,
): Promise<string | null> {
  const absPath = path.join(siteDir, relPath);
  await fs.mkdir(path.dirname(absPath), {recursive: true});
  await fs.writeFile(absPath, source, 'utf8');
  try {
    await assertNoLiteralCounts({
      paths: createRecordPaths(siteDir),
      counts: COUNTS,
    });
    await fs.rm(absPath);
    return null;
  } catch (error) {
    await fs.rm(absPath);
    return (error as Error).message;
  }
}

describe('hardcoded counts in JSX', () => {
  it('fails on a count written next to a record noun', async () => {
    const message = await check(
      'src/pages/stats.tsx',
      'export default () => <p>14 decisions, published in the open.</p>;',
    );
    assert.ok(message, 'expected a failure');
    assert.match(message!, /14 decisions/);
    assert.match(message!, /src\/pages\/stats\.tsx:1/);
    assert.match(message!, /counts\.decisions/);
  });

  it('fails on a count split across elements, which no grep can see', async () => {
    // The literal `\d+\s+decisions` does not appear on any line of this file.
    const message = await check(
      'src/pages/split.tsx',
      [
        'export default () => (',
        '  <div className="stat">',
        '    <strong>14</strong>',
        '    <span>decisions</span>',
        '  </div>',
        ');',
      ].join('\n'),
    );
    assert.ok(message, 'expected a failure');
    assert.match(message!, /14 decisions/);
  });

  it('fails on a STALE count, not only on one that matches the record', async () => {
    // This is the bug ADR-0014 was written about: specifications.md stated a
    // total that had been wrong for some time. A value-equality check would
    // have passed this on the day it was written.
    const message = await check(
      'src/pages/stale.tsx',
      'export default () => <p>The record holds 9 specifications.</p>;',
    );
    assert.ok(message, 'expected a failure');
    assert.match(message!, /9 specifications/);
    assert.match(message!, /already stale/);
  });

  it('fails on the noun-first spelling', async () => {
    const message = await check(
      'src/pages/table.tsx',
      'export default () => <dl><dt>Requirements</dt><dd>219</dd></dl>;',
    );
    assert.ok(message, 'expected a failure');
    assert.match(message!, /Requirements.*219|219.*Requirements/s);
  });

  it('fails on a count reaching the page through an expression container', async () => {
    const message = await check(
      'src/pages/expr.tsx',
      'export default () => <p>{359} scenarios</p>;',
    );
    assert.ok(message, 'expected a failure');
    assert.match(message!, /359 scenarios/);
  });

  it('fails on a number and a noun passed as sibling props', async () => {
    const message = await check(
      'src/pages/props.tsx',
      'export default () => <Stat value="14" label="Decisions" />;',
    );
    assert.ok(message, 'expected a failure');
    assert.match(message!, /src\/pages\/props\.tsx/);
  });

  it('fails on a count in the site metadata, which renders no JSX at all', async () => {
    const message = await check(
      'src/pages/meta.ts',
      "export const description = 'Cairn — 14 decisions in public.';",
    );
    assert.ok(message, 'expected a failure');
    assert.match(message!, /14 decisions/);
  });
});

describe('what the check must NOT fire on', () => {
  /*
   * A claim is written on one line. These are the shapes that made the gate
   * unpublishable when the separator was plain `\s`, which crosses newlines:
   * every one of them hard-failed `docusaurus build` on ordinary content.
   */
  it('passes a heading whose following paragraph merely starts with a number', async () => {
    const message = await check(
      'docs/intro.md',
      ['## Requirements', '', '3 of the surfaces are still in design.'].join('\n'),
    );
    assert.equal(message, null);
  });

  it('passes a sentence ending in a noun before a sentence opening with a number', async () => {
    const message = await check(
      'docs/intro.md',
      ['The page lists the requirements.', '', '3 of them are new.'].join('\n'),
    );
    assert.equal(message, null);
  });

  /*
   * Rule (C) must not collide with SPEC-0010's own accessibility requirements.
   * REQ "Icon-Only Controls" mandates `aria-label` and REQ "Keyboard Navigation
   * & Focus Management" produces `tabIndex`; the first of these is the exact
   * markup REQ "Keyboard Navigation" asks for on a scrollable table.
   */
  it('passes an accessible scroll region: tabIndex beside an aria-label', async () => {
    const message = await check(
      'src/pages/a11y.tsx',
      'export default () => (\n  <div role="region" tabIndex={0} aria-label="Specification endpoints">x</div>\n);',
    );
    assert.equal(message, null);
  });

  it('passes an icon carrying a size and a record-noun label', async () => {
    const message = await check(
      'src/pages/icon.tsx',
      'export default () => <svg width="24" height="24" aria-label="Decisions" />;',
    );
    assert.equal(message, null);
  });

  it('passes a noun that merely appears inside a longer descriptive value', async () => {
    const message = await check(
      'src/pages/img.tsx',
      'export default () => <img src="/a.png" alt="The decisions index" width="800" height="600" />;',
    );
    assert.equal(message, null);
  });

  it('passes the correct spelling: the number comes from the data module', async () => {
    const message = await check(
      'src/pages/derived.tsx',
      [
        "import {counts} from '@site/src/data/record';",
        'export default () => (',
        '  <p>',
        '    {counts.decisions} decisions, {counts.specifications} specifications,',
        '    {counts.requirements} requirements.',
        '  </p>',
        ');',
      ].join('\n'),
    );
    assert.equal(message, null);
  });

  it('passes a number that is a magnitude rather than a count', async () => {
    // 14 is the decision total. A value-keyed checker fires on every line here.
    const message = await check(
      'src/pages/geometry.tsx',
      [
        'const COLUMNS = 14;',
        'const DELAY_MS = 219;',
        'export default () => (',
        '  <svg viewBox="0 0 10 359" style={{zIndex: 14}}>',
        '    <rect width={10} height={359} />',
        '    <text>14ms</text>',
        '  </svg>',
        ');',
      ].join('\n'),
    );
    assert.equal(message, null);
  });

  it('passes prose that names a record noun with no number attached', async () => {
    const message = await check(
      'src/pages/prose.tsx',
      'export default () => <p>Every decision and every specification is published.</p>;',
    );
    assert.equal(message, null);
  });

  it('passes a count of something the record does not own', async () => {
    const message = await check(
      'src/pages/types.tsx',
      'export default () => <p>7 share types, 3 surfaces, 30 days of retention.</p>;',
    );
    assert.equal(message, null);
  });
});

describe('narrative documentation pages', () => {
  it('fails on a count stated in prose on a hand-written docs page', async () => {
    // Exactly the defect that produced ADR-0014: docs/specifications.md stated
    // a requirement total in a sentence and nothing checked it.
    const message = await check(
      'docs/overview.md',
      '# Overview\n\nThe record contains 219 requirements across 10 specifications.\n',
    );
    assert.ok(message, 'expected a failure');
    assert.match(message!, /docs\/overview\.md:3/);
  });

  it('fails on a count stated as a markdown table row, not only as a sentence', async () => {
    // The same defect one syntax away: a cell wall between the noun and the
    // number hides the claim from a separator class that only knows about
    // colons and dashes.
    const message = await check(
      'docs/metrics.md',
      [
        '# Metrics',
        '',
        '| Metric | Count |',
        '| --- | --- |',
        '| Requirements | 219 |',
        '| Scenarios | 359 |',
        '',
      ].join('\n'),
    );
    assert.ok(message, 'expected a failure');
    assert.match(message!, /2 hardcoded record counts/);
    assert.match(message!, /docs\/metrics\.md:5/);
    assert.match(message!, /docs\/metrics\.md:6/);
  });

  it('passes a record id, whose hyphen is not a separator', async () => {
    // `SPEC-0010` must not read as "10 specifications". This is the reason the
    // hyphen is admitted only with whitespace around it, and the record's own
    // narrative pages name ids constantly.
    const message = await check(
      'docs/ids.md',
      '# Ids\n\nADR-0014 is implemented by SPEC-0010, which requires SPEC-0004.\n',
    );
    assert.equal(message, null);
  });

  it('leaves the STAGED record alone — a decision may discuss its own numbering', async () => {
    // The staged trees are the record's own text. ADR-0014 itself says "it ends
    // at ADR-0012"; publishing that sentence is the product, not a defect.
    const message = await check(
      'docs/decisions/ADR-0014.md',
      '# ADR-0014\n\nThe table ends at ADR-0012 while the tree holds 14 decisions.\n',
    );
    assert.equal(message, null);
  });
});

describe('error reporting', () => {
  it('names every offending file and both the claim and the derived source', async () => {
    const message = await check(
      'src/pages/many.tsx',
      [
        'export default () => (',
        '  <>',
        '    <p>14 decisions</p>',
        '    <p>10 specifications</p>',
        '  </>',
        ');',
      ].join('\n'),
    );
    assert.ok(message, 'expected a failure');
    assert.match(message!, /2 hardcoded record counts/);
    assert.match(message!, /counts\.decisions/);
    assert.match(message!, /counts\.specifications/);
  });

  it('fails loudly on site source it cannot parse rather than skipping it', async () => {
    const message = await check(
      'src/pages/broken.tsx',
      'export default () => <p>unclosed',
    );
    assert.ok(message, 'expected a failure');
    assert.match(message!, /could not be parsed/);
  });
});
