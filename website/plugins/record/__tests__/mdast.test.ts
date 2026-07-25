// Governing: ADR-0014, SPEC-0010 REQ "Record Prose to Structured Components"
// Governing: ADR-0014, SPEC-0010 REQ "CommonMark Fidelity"

import assert from 'node:assert/strict';
import {describe, it} from 'node:test';

import {
  collectInventory,
  deriveSummary,
  literaliseRawHtml,
  parseRecordMarkdown,
  structureRequirements,
  wrapRfc2119Keywords,
} from '../mdast.ts';

/* eslint-disable @typescript-eslint/no-explicit-any */

function find(node: any, predicate: (n: any) => boolean): any[] {
  const out: any[] = [];
  const walk = (n: any): void => {
    if (predicate(n)) {
      out.push(n);
    }
    for (const child of n.children ?? []) {
      walk(child);
    }
  };
  walk(node);
  return out;
}

function named(tree: any, name: string): any[] {
  return find(
    tree,
    (n) =>
      (n.type === 'mdxJsxFlowElement' || n.type === 'mdxJsxTextElement') &&
      n.name === name,
  );
}

function attr(node: any, name: string): string | undefined {
  return node.attributes?.find((a: any) => a.name === name)?.value;
}

function textOf(node: any): string {
  return find(node, (n) => n.type === 'text' || n.type === 'inlineCode')
    .map((n) => n.value)
    .join('');
}

describe('literaliseRawHtml', () => {
  it('turns a tag-shaped word into literal text instead of an element', async () => {
    // CommonMark reads `<id>` as an open tag, so it parses to an `html` node and
    // Docusaurus's rehype-raw would turn it into a real `<id>` element.
    const tree = await parseRecordMarkdown('An artifact lives at /<id> today.\n');
    assert.equal(find(tree, (n) => n.type === 'html').length, 1);

    const result = literaliseRawHtml(tree);
    assert.equal(result.literalised, 1);
    assert.equal(find(tree, (n) => n.type === 'html').length, 0);
    assert.match(textOf(tree), /An artifact lives at \/<id> today\./);
  });

  it('drops an HTML comment rather than printing it', async () => {
    const tree = await parseRecordMarkdown('<!-- a note -->\n\nProse.\n');
    const result = literaliseRawHtml(tree);
    assert.equal(result.dropped, 1);
    assert.equal(result.literalised, 0);
    assert.doesNotMatch(textOf(tree), /a note/);
  });

  it('leaves a tag-shaped word inside a code span alone', async () => {
    const tree = await parseRecordMarkdown('The route is `cairn.sh/<id>`.\n');
    assert.equal(find(tree, (n) => n.type === 'html').length, 0);
    const result = literaliseRawHtml(tree);
    assert.equal(result.literalised, 0);
    assert.equal(find(tree, (n) => n.type === 'inlineCode')[0]!.value, 'cairn.sh/<id>');
  });

  it('leaves a bare `<` and a bare `{` as plain text', async () => {
    const tree = await parseRecordMarkdown('If a < b and the shape is {a, b}.\n');
    literaliseRawHtml(tree);
    assert.match(textOf(tree), /a < b/);
    assert.match(textOf(tree), /\{a, b\}/);
  });
});

describe('wrapRfc2119Keywords', () => {
  it('wraps each keyword, longest match first', async () => {
    const tree = await parseRecordMarkdown(
      'The build MUST fail and MUST NOT warn, and it MAY log.\n',
    );
    const wrapped = wrapRfc2119Keywords(tree);
    assert.equal(wrapped, 3);
    assert.deepEqual(
      named(tree, 'Rfc2119').map((n) => attr(n, 'keyword')),
      ['MUST', 'MUST NOT', 'MAY'],
    );
  });

  it('wraps a two-word keyword split across a soft line break', async () => {
    // The record hard-wraps at ~85 columns, so `MUST NOT` regularly straddles a
    // line ending, which CommonMark keeps as a `\n` inside one text node. With a
    // literal space in the pattern this matched only `MUST` and the record's
    // prohibition rendered as a positive obligation with a bare "NOT" beside it.
    const tree = await parseRecordMarkdown(
      'Errors MUST be wrapped with context at each layer boundary, MUST\nNOT be silently swallowed.\n',
    );
    const wrapped = wrapRfc2119Keywords(tree);
    assert.equal(wrapped, 2);
    assert.deepEqual(
      named(tree, 'Rfc2119').map((n) => attr(n, 'keyword')),
      ['MUST', 'MUST NOT'],
    );
    // The author's bytes survive in the rendered text; only the attribute is
    // normalised, and no bare "NOT" is left outside a keyword.
    assert.match(textOf(tree), /MUST\nNOT be silently swallowed/);
    assert.doesNotMatch(textOf(tree), /MUST NOT be silently/);
  });

  it('wraps `NOT RECOMMENDED` split across a soft line break', async () => {
    const tree = await parseRecordMarkdown('Doing that is NOT\nRECOMMENDED.\n');
    assert.equal(wrapRfc2119Keywords(tree), 1);
    assert.deepEqual(
      named(tree, 'Rfc2119').map((n) => attr(n, 'keyword')),
      ['NOT RECOMMENDED'],
    );
  });

  it('leaves a keyword inside a code span alone', async () => {
    const tree = await parseRecordMarkdown('Set `status: MUST` on the field.\n');
    assert.equal(wrapRfc2119Keywords(tree), 0);
  });

  it('does not match a keyword embedded in a longer token', async () => {
    const tree = await parseRecordMarkdown('MUSTARD and MAYBE and SHOULDER.\n');
    assert.equal(wrapRfc2119Keywords(tree), 0);
  });

  it('leaves lowercase prose alone', async () => {
    const tree = await parseRecordMarkdown('The reader must may should.\n');
    assert.equal(wrapRfc2119Keywords(tree), 0);
  });

  it('coexists with a bare `<` in the same paragraph', async () => {
    // SPEC-0010: "Keyword adjacent to literal markup".
    const tree = await parseRecordMarkdown('The value MUST be < 10.\n');
    literaliseRawHtml(tree);
    assert.equal(wrapRfc2119Keywords(tree), 1);
    assert.match(textOf(tree), /< 10/);
  });
});

const SPEC_FIXTURE = `# SPEC-9999: Fixture

## Overview

A fixture specification. It exists to exercise the transform.

## Requirements

### Requirement: First Thing

The site MUST do the first thing.

#### Scenario: It happens

- **WHEN** a reader loads the page
- **THEN** the first thing MUST have happened

#### Scenario: It does not happen

- **WHEN** the page fails to load
- **THEN** nothing MUST be claimed

### Requirement: Second Thing

The site MAY do the second thing.

#### Scenario: Optional

- **WHEN** the option is set
- **THEN** the second thing happens

## Notes

Trailing prose that belongs to no requirement.
`;

describe('structureRequirements', () => {
  it('wraps each requirement section in a block carrying its name', async () => {
    const tree = await parseRecordMarkdown(SPEC_FIXTURE);
    await structureRequirements(tree);
    const blocks = named(tree, 'RequirementBlock');
    assert.deepEqual(
      blocks.map((b) => attr(b, 'name')),
      ['First Thing', 'Second Thing'],
    );
  });

  it('keeps the section heading as the block’s first child', async () => {
    // Docusaurus's heading and toc plugins run after this one; removing the
    // heading would silently empty the on-this-page column.
    const tree = await parseRecordMarkdown(SPEC_FIXTURE);
    await structureRequirements(tree);
    const [first] = named(tree, 'RequirementBlock');
    assert.equal(first.children[0].type, 'heading');
    assert.equal(first.children[0].depth, 3);
  });

  it('nests scenarios inside their requirement', async () => {
    const tree = await parseRecordMarkdown(SPEC_FIXTURE);
    await structureRequirements(tree);
    const [first, second] = named(tree, 'RequirementBlock');
    assert.equal(named(first, 'ScenarioBlock').length, 2);
    assert.equal(named(second, 'ScenarioBlock').length, 1);
  });

  it('turns the WHEN/THEN list into steps carrying the keyword as data', async () => {
    const tree = await parseRecordMarkdown(SPEC_FIXTURE);
    await structureRequirements(tree);
    const steps = named(tree, 'ScenarioStep');
    assert.equal(steps.length, 6);
    assert.deepEqual(
      named(named(tree, 'ScenarioBlock')[0], 'ScenarioStep').map((s) =>
        attr(s, 'step'),
      ),
      ['WHEN', 'THEN'],
    );
    assert.match(textOf(steps[0]), /^a reader loads the page/);
  });

  it('stops a requirement at the next heading of the same or lower depth', async () => {
    const tree = await parseRecordMarkdown(SPEC_FIXTURE);
    await structureRequirements(tree);
    const [, second] = named(tree, 'RequirementBlock');
    assert.doesNotMatch(textOf(second), /Trailing prose/);
    // …and that trailing section stays at the top level.
    assert.match(
      tree.children.map((c: any) => textOf(c)).join('\n'),
      /Trailing prose/,
    );
  });

  it('leaves a list that is not a WHEN/THEN pair untouched', async () => {
    const tree = await parseRecordMarkdown(
      '### Requirement: Thing\n\nProse.\n\n#### Scenario: Odd\n\n- just a bullet\n- another\n',
    );
    await structureRequirements(tree);
    assert.equal(named(tree, 'ScenarioStep').length, 0);
    assert.equal(find(tree, (n) => n.type === 'list').length, 1);
  });

  it('ignores a depth-3 heading that is not a requirement', async () => {
    const tree = await parseRecordMarkdown('### Why not the status quo\n\nProse.\n');
    await structureRequirements(tree);
    assert.equal(named(tree, 'RequirementBlock').length, 0);
  });
});

describe('collectInventory', () => {
  it('counts requirements and scenarios and attributes them correctly', async () => {
    const tree = await parseRecordMarkdown(SPEC_FIXTURE);
    literaliseRawHtml(tree);
    const {requirements, scenarioCount} = await collectInventory(tree);
    assert.equal(requirements.length, 2);
    assert.equal(scenarioCount, 3);
    assert.deepEqual(
      requirements.map((r) => [r.name, r.scenarioCount]),
      [
        ['First Thing', 2],
        ['Second Thing', 1],
      ],
    );
  });

  it('computes the heading anchor Docusaurus will emit', async () => {
    const tree = await parseRecordMarkdown(SPEC_FIXTURE);
    const {requirements} = await collectInventory(tree);
    assert.deepEqual(
      requirements.map((r) => r.anchor),
      ['requirement-first-thing', 'requirement-second-thing'],
    );
  });

  it('de-duplicates repeated anchors the way the slugger does', async () => {
    const tree = await parseRecordMarkdown(
      '### Requirement: Same\n\nA.\n\n### Requirement: Same\n\nB.\n',
    );
    const {requirements} = await collectInventory(tree);
    assert.deepEqual(
      requirements.map((r) => r.anchor),
      ['requirement-same', 'requirement-same-1'],
    );
  });

  it('does not count a scenario that sits outside any requirement', async () => {
    const tree = await parseRecordMarkdown(
      '## Loose\n\n#### Scenario: Orphan\n\n- **WHEN** x\n- **THEN** y\n',
    );
    const {requirements, scenarioCount} = await collectInventory(tree);
    assert.equal(requirements.length, 0);
    assert.equal(scenarioCount, 1);
  });
});

describe('deriveSummary', () => {
  it('takes whole sentences from the first paragraph', async () => {
    const tree = await parseRecordMarkdown(
      '# ADR-9999: Thing\n\nThe first sentence is short. The second sentence carries the rest of the useful detail. A third.\n',
    );
    const summary = await deriveSummary(tree);
    assert.equal(
      summary,
      'The first sentence is short. The second sentence carries the rest of the useful detail.',
    );
  });

  it('is not truncated at a tag-shaped word', async () => {
    // SPEC-0010: "Tag-shaped word in prose" — the description must survive it.
    const source =
      '# ADR-9999: Thing\n\nArtifacts are addressed at /<id>, and that identifier is opaque so that nothing downstream can infer ordering from it.\n';
    const tree = await parseRecordMarkdown(source);
    literaliseRawHtml(tree);
    const summary = await deriveSummary(tree);
    assert.match(summary, /\/<id>/);
    assert.match(summary, /infer ordering from it/);
  });

  it('collapses the record’s hard line wrapping', async () => {
    const tree = await parseRecordMarkdown('Prose wrapped\nacross two lines.\n');
    assert.equal(await deriveSummary(tree), 'Prose wrapped across two lines.');
  });

  it('returns an empty string for a document with no paragraph', async () => {
    const tree = await parseRecordMarkdown('# Only A Heading\n');
    assert.equal(await deriveSummary(tree), '');
  });
});

describe('literaliseRawHtml placement', () => {
  it('wraps a flow-level raw HTML node in a paragraph', async () => {
    const tree: any = await parseRecordMarkdown('<div>\n\nProse.\n');
    literaliseRawHtml(tree);
    assert.equal(tree.children[0].type, 'paragraph');
    assert.equal(tree.children[0].children[0].type, 'text');
  });

  it('keeps a phrasing-level raw HTML node inline', async () => {
    const tree: any = await parseRecordMarkdown('An <id> in a sentence.\n');
    literaliseRawHtml(tree);
    assert.equal(tree.children[0].type, 'paragraph');
    assert.deepEqual(
      tree.children[0].children.map((c: any) => c.type),
      ['text', 'text', 'text'],
    );
  });

  it('handles a raw HTML node nested inside a list item', async () => {
    const tree: any = await parseRecordMarkdown('- an <id> here\n');
    literaliseRawHtml(tree);
    assert.equal(find(tree, (n) => n.type === 'html').length, 0);
    assert.match(textOf(tree), /an <id> here/);
  });
});
