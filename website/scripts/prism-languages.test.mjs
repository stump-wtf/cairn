/**
 * Governing: ADR-0014, SPEC-0010 REQ "Code Block Fidelity"
 *
 * The highlighter's coverage floor is stated in the specification, so it is
 * READ from the specification rather than retyped here — see
 * `required-languages.mjs`, the same discipline `check-tokens.mjs` applies to
 * ADR-0009's category names. A language added to the requirement therefore
 * fails this test until `docusaurus.config.ts` loads it, instead of quietly
 * rendering as plain text on the published site.
 *
 * The covered set is the union of two things, because that is how Docusaurus
 * assembles it: `@docusaurus/theme-classic/src/prism-include-languages.ts`
 * seeds the highlighter with prism-react-renderer's bundled grammars, then
 * `theme/prism-include-languages.ts` `require`s
 * `prismjs/components/prism-<lang>` once per `additionalLanguages` entry.
 */

import assert from 'node:assert/strict';
import {createRequire} from 'node:module';
import {readFileSync} from 'node:fs';
import {join} from 'node:path';
import {describe, it} from 'node:test';
import {fileURLToPath} from 'node:url';

import {Prism} from 'prism-react-renderer';

import {requiredLanguages} from './required-languages.mjs';

const HERE = fileURLToPath(new URL('.', import.meta.url));
const WEBSITE = join(HERE, '..');
const CONFIG = join(WEBSITE, 'docusaurus.config.ts');

const require = createRequire(import.meta.url);

/** The `additionalLanguages` array as the site config declares it. */
function additionalLanguages() {
  const source = readFileSync(CONFIG, 'utf8');
  const match = source.match(/additionalLanguages:\s*\[([^\]]*)\]/);
  assert.ok(
    match,
    `docusaurus.config.ts declares no additionalLanguages array. Without one the ` +
      `highlighter covers only prism-react-renderer's bundled grammars.`,
  );
  return [...match[1].matchAll(/['"]([^'"]+)['"]/g)].map((m) => m[1]);
}

describe('prism language coverage', () => {
  const required = requiredLanguages();
  const additional = additionalLanguages();
  const bundled = new Set(Object.keys(Prism.languages));
  const covered = new Set([...bundled, ...additional]);

  it('covers every language REQ "Code Block Fidelity" names', () => {
    const missing = required.filter((lang) => !covered.has(lang));
    assert.deepEqual(
      missing,
      [],
      `the specification requires ${required.join(', ')}; ` +
        `${missing.join(', ')} is covered neither by prism-react-renderer's bundle nor by ` +
        `additionalLanguages in docusaurus.config.ts.`,
    );
  });

  it('names the whole floor in additionalLanguages, not just the gaps', () => {
    // A grammar that only reaches the page via prism-react-renderer's bundle
    // is covered by a dependency's internal choice. Listing all of them makes
    // the floor survive that bundle being trimmed.
    const delegated = required.filter((lang) => !additional.includes(lang));
    assert.deepEqual(
      delegated,
      [],
      `${delegated.join(', ')} is required by the specification but left to ` +
        `prism-react-renderer's bundled grammars. Name it in additionalLanguages.`,
    );
  });

  it('loads a real prismjs grammar for every additionalLanguages entry', () => {
    // A typo here does not degrade — theme-classic `require`s the path
    // unconditionally, so it throws at runtime on every page with a code block.
    for (const lang of additional) {
      assert.doesNotThrow(
        () => require.resolve(`prismjs/components/prism-${lang}.js`),
        `additionalLanguages names "${lang}", but prismjs ships no ` +
          `components/prism-${lang}.js`,
      );
    }
  });
});
