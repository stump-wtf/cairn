// Governing: ADR-0014, SPEC-0010 REQ "Derived Design-Language Page"
//
// The assertions here are deliberately about the RELATIONSHIP between the token
// definition and the derived inventory, never about the values themselves. A
// test that asserted `--cat-reason` is a particular hex would be a third copy of
// the palette — after `custom.css` and `check-tokens.mjs` — and the whole point
// of the requirement is that the page reads the value rather than restating it.

import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import path from 'node:path';
import {before, describe, it} from 'node:test';

import {createRecordPaths} from '../paths.ts';
import {collectDesignTokens, type DesignTokens} from '../tokens.ts';

const siteDir = path.resolve(import.meta.dirname, '..', '..', '..');
const paths = createRecordPaths(siteDir);

describe('collectDesignTokens', () => {
  let tokens: DesignTokens;
  let css: string;

  before(async () => {
    tokens = await collectDesignTokens(paths);
    css = await fs.readFile(paths.tokenCss, 'utf8');
  });

  it('reads every category token declared in the token definition', () => {
    const declared = [...css.matchAll(/(--cat-[\w-]+)\s*:/g)].map((m) => m[1]);
    assert.deepEqual(
      tokens.categories.map((category) => category.token),
      declared,
    );
  });

  it('takes each value from the definition rather than restating it', () => {
    for (const category of tokens.categories) {
      const declaration = new RegExp(
        `${category.token}\\s*:\\s*${category.value}\\s*;`,
        'i',
      );
      assert.match(
        css,
        declaration,
        `${category.token} was reported as ${category.value}, which is not what the token file declares`,
      );
    }
  });

  it('marks exactly one category as the neutral default', () => {
    const neutral = tokens.categories.filter((category) => !category.recommended);
    assert.equal(neutral.length, 1);
    // Which one is derived from ADR-0009's own recommended set, so this is the
    // fallback the record decided on and not one this test picked.
    assert.ok(neutral[0]!.token.startsWith('--cat-'));
  });

  it('resolves a ramp token that is declared as an indirection', () => {
    // `--cairn-paper-1000: var(--cairn-ink-0)` — a swatch that printed the
    // `var()` instead of the colour would be showing nothing useful.
    const flat = tokens.ramps.flatMap((ramp) => ramp.tokens);
    assert.ok(flat.length > 0);
    for (const swatch of flat) {
      assert.match(swatch.value, /^#[0-9a-f]{3,8}$/);
    }
    const indirect = [...css.matchAll(/(--cairn-\w+-\w+)\s*:\s*var\((--[\w-]+)\)/g)];
    const resolved = indirect
      .map(([, name]) => flat.find((swatch) => swatch.token === name))
      .filter(Boolean);
    assert.ok(
      resolved.length > 0,
      'the token file declares no ramp indirection, so this test no longer proves anything',
    );
  });

  it('reports both type families with their fallbacks intact', () => {
    assert.equal(tokens.families.length, 2);
    for (const family of tokens.families) {
      assert.ok(css.includes(`${family.token}:`));
      assert.ok(family.stack.includes(family.primary));
      assert.ok(
        family.stack.split(',').length >= 3,
        `${family.token} lost its system fallbacks on the way out`,
      );
    }
  });

  it('names the file every value came from, repo-relative', () => {
    assert.equal(tokens.sourcePath, 'website/src/css/custom.css');
  });
});
