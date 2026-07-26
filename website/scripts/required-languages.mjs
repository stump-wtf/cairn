/**
 * Governing: ADR-0014, SPEC-0010 REQ "Code Block Fidelity"
 *
 * The highlighter's coverage floor is stated in the specification, so it is
 * READ from the specification rather than retyped — the same discipline
 * `check-tokens.mjs` applies to ADR-0009's category names. Two tests need the
 * floor (`scripts/prism-languages.test.mjs`, which checks the site config
 * against it, and `plugins/record/__tests__/code-blocks.test.ts`, which checks
 * that a fence's `lang` survives the record transforms), and a floor retyped in
 * one of them is a floor that can drift from the other. It lives here so both
 * read the same source.
 */

import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import {join} from 'node:path';
import {fileURLToPath} from 'node:url';

const HERE = fileURLToPath(new URL('.', import.meta.url));
const REPO = join(HERE, '..', '..');
const SPEC = join(
  REPO,
  'docs',
  'openspec',
  'specs',
  'public-website-and-design-record',
  'spec.md',
);

/**
 * The languages REQ "Code Block Fidelity" names, taken from the sentence that
 * names them. The requirement body is sliced out first so that a backticked
 * word elsewhere in the specification cannot leak into the floor.
 *
 * @returns {string[]} the required languages, in the order the requirement
 *   writes them.
 */
export function requiredLanguages() {
  const text = readFileSync(SPEC, 'utf8');
  const start = text.indexOf('### Requirement: Code Block Fidelity');
  assert.notEqual(
    start,
    -1,
    `${SPEC} no longer contains "### Requirement: Code Block Fidelity". The floor is ` +
      `derived from the specification and must not be retyped in a test.`,
  );
  const rest = text.slice(start + 1);
  const end = rest.indexOf('\n### ');
  const body = end === -1 ? rest : rest.slice(0, end);

  const sentence = body
    .replace(/\s+/g, ' ')
    .split(/(?<=\.)\s/)
    .find((s) => s.includes('highlighter') && s.includes('cover'));
  assert.ok(
    sentence,
    `no "the highlighter MUST cover …" sentence found in REQ "Code Block Fidelity". The ` +
      `parser and the specification have drifted; fix the parser rather than hardcoding ` +
      `the languages.`,
  );

  const languages = [...sentence.matchAll(/`([a-z0-9+#-]+)`/g)].map((m) => m[1]);
  assert.ok(
    languages.length >= 2,
    `REQ "Code Block Fidelity" names ${languages.length} language(s); expected the ` +
      `backticked list the requirement is written with.`,
  );
  return languages;
}
