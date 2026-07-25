// Governing: ADR-0014, SPEC-0010 REQ "Code Block Fidelity"
//
// "Fenced code blocks MUST retain their declared language." The pipeline's
// three tree transforms all walk the whole document, and each of them has a
// reason to want to touch a code block's text — an RFC 2119 keyword in a
// comment, a tag-shaped token in a JSON example, a `### Requirement:` line
// inside a fenced markdown sample. A code block holds verbatim text and none of
// them may. These tests pin that, and pin that the `lang` survives, because a
// dropped `lang` is invisible in a diff and shows up as an unhighlighted block
// on the published site.
//
// The other half of the requirement — that the highlighter actually covers the
// languages the specification names — is checked by
// scripts/prism-languages.test.mjs, which is where the site config lives.

import assert from 'node:assert/strict';
import {describe, it} from 'node:test';

import {
  literaliseRawHtml,
  parseRecordMarkdown,
  structureRequirements,
  wrapRfc2119Keywords,
} from '../mdast.ts';

/* eslint-disable @typescript-eslint/no-explicit-any */

/** Run the record transforms in the order remark-record.ts runs them. */
async function transform(source: string): Promise<any> {
  const tree = await parseRecordMarkdown(source);
  literaliseRawHtml(tree);
  await structureRequirements(tree);
  wrapRfc2119Keywords(tree);
  return tree;
}

function codeNodes(tree: any): any[] {
  const out: any[] = [];
  const walk = (node: any): void => {
    if (node.type === 'code') {
      out.push(node);
    }
    for (const child of node.children ?? []) {
      walk(child);
    }
  };
  walk(tree);
  return out;
}

describe('code block fidelity', () => {
  it('keeps the declared language of every fence', async () => {
    const languages = ['go', 'bash', 'json', 'sql', 'yaml'];
    const source = languages
      .map((lang) => ['```' + lang, `sample ${lang}`, '```'].join('\n'))
      .join('\n\n');

    const blocks = codeNodes(await transform(source));
    assert.deepEqual(
      blocks.map((block) => block.lang),
      languages,
    );
  });

  it('keeps the language of a fence the highlighter does not cover', async () => {
    // The record uses `mermaid` throughout its design documents and nothing
    // highlights it. It must reach the page as a plain preformatted block that
    // still declares its language, not be dropped or relabelled.
    const tree = await transform(
      ['```mermaid', 'flowchart TD', '  A --> B', '```'].join('\n'),
    );
    const [block] = codeNodes(tree);
    assert.equal(block.lang, 'mermaid');
    assert.equal(block.value, 'flowchart TD\n  A --> B');
  });

  it('leaves fenced text verbatim, keywords and tag-shaped tokens included', async () => {
    const value = [
      '// the server MUST NOT rewrite <id>',
      'curl https://cairn.sh/a/<id> | jq .{}',
    ].join('\n');
    const tree = await transform(['```bash', value, '```'].join('\n'));

    const [block] = codeNodes(tree);
    assert.equal(block.lang, 'bash');
    // No <Rfc2119> wrapper, no literalisation pass, no escaping: the bytes the
    // author wrote are the bytes that reach the highlighter.
    assert.equal(block.value, value);
    assert.equal(block.children, undefined);
  });

  it('does not read a fenced heading as a requirement section', async () => {
    // A specification that shows the record's own syntax in a fence would
    // otherwise grow a phantom requirement — and the derived counts on the
    // specs index are the numbers that would be wrong.
    const tree = await transform(
      [
        '### Requirement: Real One',
        '',
        'Prose.',
        '',
        '```md',
        '### Requirement: Fenced Example',
        '```',
      ].join('\n'),
    );

    const blocks: any[] = [];
    const walk = (node: any): void => {
      if (node.type === 'mdxJsxFlowElement' && node.name === 'RequirementBlock') {
        blocks.push(node);
      }
      for (const child of node.children ?? []) {
        walk(child);
      }
    };
    walk(tree);

    assert.equal(blocks.length, 1);
    assert.equal(blocks[0].attributes[0].value, 'Real One');
    assert.equal(codeNodes(tree)[0].lang, 'md');
  });
});
