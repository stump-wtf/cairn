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

const AUTHORED_EDGE_KEYS = [
  'extends',
  'enables',
  'related',
  'implements',
  'requires',
] as const;

/**
 * Count graph edges straight out of the record's front-matter, deliberately
 * without importing anything from `graph.ts`. This is the denominator the
 * de-duplication assertion needs: any figure derived from the generator's own
 * output would make that assertion self-referential.
 */
async function countAuthoredEdges(): Promise<number> {
  const yaml = await import('js-yaml');
  /* eslint-disable-next-line @typescript-eslint/no-explicit-any */
  const load = yaml.load ?? (yaml as any).default.load;

  const sources: string[] = [];
  for (const name of await fs.readdir(paths.adrDir)) {
    if (/^ADR-\d{4}.*\.md$/.test(name)) {
      sources.push(path.join(paths.adrDir, name));
    }
  }
  for (const entry of await fs.readdir(paths.specsDir, {withFileTypes: true})) {
    if (entry.isDirectory()) {
      sources.push(path.join(paths.specsDir, entry.name, 'spec.md'));
    }
  }

  let total = 0;
  for (const source of sources) {
    const {frontMatter} = splitFrontMatter(await fs.readFile(source, 'utf8'));
    const parsed = (load(frontMatter) ?? {}) as Record<string, unknown>;
    for (const key of AUTHORED_EDGE_KEYS) {
      const value = parsed[key];
      if (Array.isArray(value)) {
        total += value.length;
      }
    }
  }
  return total;
}

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

  it('stores fewer edges than were authored, because the record says the same thing twice', async () => {
    // The denominator is counted out of the source front-matter by this test,
    // with no help from graph.ts. Deriving it from anything the generator
    // produced would be checking the de-duplicator against itself — which is
    // what this assertion used to do: it compared the edge count to the number
    // of *records*, and `52 > 24 / 2` says nothing about de-duplication.
    const authored = await countAuthoredEdges();
    assert.ok(authored > 0, 'the record authors no graph edges at all');
    assert.ok(
      data.graph.edges.length < authored,
      `expected fewer stored edges than the ${authored} authored, got ${data.graph.edges.length}`,
    );

    // And the specific relationship SPEC-0010 names: ADR-0001 declares
    // `enables: [ADR-0002, ADR-0003]` while ADR-0002 and ADR-0003 each
    // independently declare `extends: [ADR-0001]`, so that fact is authored from
    // both ends and must be stored, and rendered, exactly once.
    const bothEnds = data.graph.edges.filter(
      (edge) =>
        edge.kind === 'extends' &&
        edge.from === 'ADR-0002' &&
        edge.to === 'ADR-0001',
    );
    assert.equal(bothEnds.length, 1, 'ADR-0002 extends ADR-0001 is not stored once');
    assert.deepEqual(
      data.graph.relations['ADR-0001']?.extendedBy.filter(
        (id) => id === 'ADR-0002',
      ),
      ['ADR-0002'],
    );
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

  // Governing: ADR-0014, SPEC-0010 REQ "Record Navigation"
  // "One sidebar spans the record" is a claim about what a reader on a
  // specification page can see, and theme-classic omits a collapsed category's
  // children from the DOM entirely rather than hiding them. If either top-level
  // group is ever staged collapsed the scenario silently stops holding, so
  // assert the flag here rather than trust the rendered output to be spot-checked.
  it('stages the two record groups expanded, so each lists its children everywhere', async () => {
    for (const dir of [paths.stagedDecisionsDir, paths.stagedSpecsDir]) {
      const category = JSON.parse(
        await fs.readFile(path.join(dir, '_category_.json'), 'utf8'),
      );
      assert.equal(category.collapsed, false);
    }
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
