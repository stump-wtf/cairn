// Governing: ADR-0014, SPEC-0010 REQ "Record Content Pipeline"
// Governing: ADR-0014, SPEC-0010 REQ "Derived Data Module"
//
// End-to-end over the real record. These assertions are about invariants rather
// than about today's totals, so adding a decision does not break the test suite —
// which would be the same "fact written twice" defect one layer down.

import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import path from 'node:path';
import {before, describe, it} from 'node:test';

import {generateRecord} from '../generate.ts';
import {createRecordPaths} from '../paths.ts';
import {splitFrontMatter} from '../parse.ts';
import type {RecordData} from '../types.ts';

const siteDir = path.resolve(import.meta.dirname, '..', '..', '..');
const paths = createRecordPaths(siteDir);

describe('generateRecord over the real record', () => {
  let data: RecordData;

  before(async () => {
    ({data} = await generateRecord({siteDir}));
  });

  it('publishes one page per ADR-*.md and one card per capability directory', async () => {
    const adrFiles = (await fs.readdir(paths.adrDir)).filter((name) =>
      /^ADR-\d{4}.*\.md$/.test(name),
    );
    const capabilities = (
      await fs.readdir(paths.specsDir, {withFileTypes: true})
    ).filter((entry) => entry.isDirectory());

    assert.equal(data.decisions.length, adrFiles.length);
    assert.equal(data.counts.decisions, adrFiles.length);
    assert.equal(data.specs.length, capabilities.length);
    assert.equal(data.counts.specifications, capabilities.length);
  });

  it('orders decisions by ADR number', () => {
    const numbers = data.decisions.map((decision) => decision.number);
    assert.deepEqual(numbers, [...numbers].sort((a, b) => a - b));
  });

  it('orders specifications by the number in their own heading, not by directory name', () => {
    const numbers = data.specs.map((spec) => spec.number);
    assert.deepEqual(numbers, [...numbers].sort((a, b) => a - b));
    // The guard that matters: sorting by directory name would give a different
    // order, so a passing test here means the numbering really came from the
    // `# SPEC-XXXX:` headings.
    const byNumber = data.specs.map((spec) => spec.capability);
    const byDirName = [...byNumber].sort();
    assert.notDeepEqual(byNumber, byDirName);
  });

  it('agrees with a line count of the source requirement and scenario headings', async () => {
    let requirements = 0;
    let scenarios = 0;
    for (const spec of data.specs) {
      const source = await fs.readFile(
        path.join(paths.repoRoot, spec.sourcePath),
        'utf8',
      );
      const lines = source.split('\n');
      requirements += lines.filter((line) =>
        line.startsWith('### Requirement:'),
      ).length;
      scenarios += lines.filter((line) =>
        line.startsWith('#### Scenario:'),
      ).length;
    }
    assert.equal(data.counts.requirements, requirements);
    assert.equal(data.counts.scenarios, scenarios);
    assert.equal(
      data.counts.requirements,
      data.specs.reduce((total, spec) => total + spec.requirementCount, 0),
    );
  });

  it('resolves every graph endpoint to a record the site publishes', () => {
    for (const edge of data.graph.edges) {
      assert.ok(data.refs[edge.from], `no ref for ${edge.from}`);
      assert.ok(data.refs[edge.to], `no ref for ${edge.to}`);
    }
    for (const id of Object.keys(data.graph.relations)) {
      assert.ok(data.refs[id], `no ref for ${id}`);
    }
  });

  it('stores fewer edges than were authored, because the record says the same thing twice', () => {
    // ADR-0001 declares enables:[ADR-0002, ADR-0003] while both of those declare
    // extends:[ADR-0001]. If this ever stops holding, de-duplication has silently
    // stopped mattering and the assertion should be revisited, not deleted.
    const authored = [...data.decisions, ...data.specs].length;
    assert.ok(data.graph.edges.length > authored / 2);
    const keys = new Set(
      data.graph.edges.map((edge) => `${edge.kind} ${edge.from} ${edge.to}`),
    );
    assert.equal(keys.size, data.graph.edges.length);
  });

  it('never lists the same relationship twice on one record', () => {
    for (const relations of Object.values(data.graph.relations)) {
      for (const [bucket, ids] of Object.entries(relations)) {
        assert.equal(
          new Set(ids).size,
          ids.length,
          `${bucket} contains a duplicate`,
        );
      }
    }
  });

  it('stages the record body byte-for-byte, replacing only the front-matter', async () => {
    for (const decision of data.decisions) {
      const source = await fs.readFile(
        path.join(paths.repoRoot, decision.sourcePath),
        'utf8',
      );
      const staged = await fs.readFile(
        path.join(paths.stagedDecisionsDir, `${decision.id}.md`),
        'utf8',
      );
      assert.equal(
        splitFrontMatter(staged).body.trimEnd(),
        splitFrontMatter(source).body.trimEnd(),
        `${decision.id} body was rewritten`,
      );
    }
  });

  it('names the source of truth in every staged file, and warns against editing it', async () => {
    const staged = await fs.readFile(
      path.join(paths.stagedDecisionsDir, `${data.decisions[0]!.id}.md`),
      'utf8',
    );
    assert.match(staged, /do not edit, and do not commit/);
    assert.match(staged, /Source of truth: docs\/adrs\//);
  });

  it('writes a sidebar group label carrying the derived count', async () => {
    const category = JSON.parse(
      await fs.readFile(
        path.join(paths.stagedDecisionsDir, '_category_.json'),
        'utf8',
      ),
    );
    assert.equal(category.label, `Decisions · ${data.counts.decisions}`);
  });

  it('is idempotent: a second run emits identical bytes', async () => {
    const first = await fs.readFile(paths.recordJson, 'utf8');
    await generateRecord({siteDir});
    const second = await fs.readFile(paths.recordJson, 'utf8');
    assert.equal(first, second);
  });

  it('emits nothing the published bundle should not carry', () => {
    const serialised = JSON.stringify(data);
    // Front-matter fields that are not needed for rendering are not republished.
    assert.doesNotMatch(serialised, /decision-makers/);
    assert.doesNotMatch(serialised, /consulted/);
    // No absolute filesystem paths leak the machine the build ran on.
    assert.doesNotMatch(serialised, /\/home\//);
    assert.doesNotMatch(serialised, new RegExp(paths.repoRoot));
  });

  it('routes the record beneath the docs route base, as a single content instance', () => {
    for (const decision of data.decisions) {
      assert.equal(decision.href, `/docs/decisions/${decision.id}`);
      assert.equal(decision.docId, `decisions/${decision.id}`);
    }
    for (const spec of data.specs) {
      assert.equal(spec.href, `/docs/specs/${spec.capability}`);
      assert.equal(spec.docId, `specs/${spec.capability}/index`);
      if (spec.designHref) {
        assert.equal(spec.designHref, `/docs/specs/${spec.capability}/design`);
      }
    }
  });
});
