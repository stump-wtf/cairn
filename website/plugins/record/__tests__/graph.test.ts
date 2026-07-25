// Governing: ADR-0014, SPEC-0010 REQ "Derived Data Module"
//
// The graph tests are the ones that matter most, because inversion and
// de-duplication are the two places where the pipeline can be quietly,
// plausibly wrong: a naive inversion still renders, it just renders the same
// relationship twice.

import assert from 'node:assert/strict';
import {describe, it} from 'node:test';

import {
  authoredEdges,
  buildEdges,
  buildRelations,
  normaliseEdge,
} from '../graph.ts';
import type {ParsedRecord} from '../types.ts';

function record(
  id: string,
  authored: ParsedRecord['authored'] = {},
): ParsedRecord {
  return {
    id,
    number: Number(id.slice(-4)),
    title: id,
    status: 'accepted',
    date: '2026-07-08',
    summary: '',
    authored,
    requirements: [],
    scenarioCount: 0,
    endpointSections: [],
    badgeCodes: [],
    absPath: `/tmp/${id}.md`,
    sourcePath: `docs/adrs/${id}.md`,
    body: '',
  };
}

describe('normaliseEdge', () => {
  it('folds enables into extends with the orientation reversed', () => {
    assert.deepEqual(
      normaliseEdge({owner: 'ADR-0001', kind: 'enables', target: 'ADR-0002'}),
      {kind: 'extends', from: 'ADR-0002', to: 'ADR-0001'},
    );
    assert.deepEqual(
      normaliseEdge({owner: 'ADR-0002', kind: 'extends', target: 'ADR-0001'}),
      {kind: 'extends', from: 'ADR-0002', to: 'ADR-0001'},
    );
  });

  it('orients related deterministically, since it is symmetric', () => {
    const forward = normaliseEdge({
      owner: 'ADR-0008',
      kind: 'related',
      target: 'ADR-0005',
    });
    const backward = normaliseEdge({
      owner: 'ADR-0005',
      kind: 'related',
      target: 'ADR-0008',
    });
    assert.deepEqual(forward, backward);
    assert.deepEqual(forward, {
      kind: 'related',
      from: 'ADR-0005',
      to: 'ADR-0008',
    });
  });

  it('keeps implements and requires pointing away from the specification', () => {
    assert.deepEqual(
      normaliseEdge({owner: 'SPEC-0004', kind: 'implements', target: 'ADR-0009'}),
      {kind: 'implements', from: 'SPEC-0004', to: 'ADR-0009'},
    );
    assert.deepEqual(
      normaliseEdge({owner: 'SPEC-0010', kind: 'requires', target: 'SPEC-0004'}),
      {kind: 'requires', from: 'SPEC-0010', to: 'SPEC-0004'},
    );
  });
});

describe('authoredEdges', () => {
  it('flattens every front-matter edge list', () => {
    const edges = authoredEdges(
      record('ADR-0003', {
        extends: ['ADR-0001'],
        enables: ['ADR-0004', 'ADR-0011'],
        related: ['ADR-0005'],
      }),
    );
    assert.equal(edges.length, 4);
    assert.deepEqual(
      edges.map((edge) => `${edge.kind}:${edge.target}`).sort(),
      [
        'enables:ADR-0004',
        'enables:ADR-0011',
        'extends:ADR-0001',
        'related:ADR-0005',
      ],
    );
  });
});

describe('buildEdges', () => {
  // This is the exact shape ADR-0014 calls out: ADR-0001 declares
  // enables:[ADR-0002, ADR-0003] while both of those declare extends:[ADR-0001].
  it('stores a relationship authored from both ends exactly once', () => {
    const edges = buildEdges([
      record('ADR-0001', {enables: ['ADR-0002', 'ADR-0003']}),
      record('ADR-0002', {extends: ['ADR-0001']}),
      record('ADR-0003', {extends: ['ADR-0001']}),
    ]);
    assert.deepEqual(edges, [
      {kind: 'extends', from: 'ADR-0002', to: 'ADR-0001'},
      {kind: 'extends', from: 'ADR-0003', to: 'ADR-0001'},
    ]);
  });

  it('de-duplicates a symmetric relationship authored from both ends', () => {
    const edges = buildEdges([
      record('ADR-0005', {related: ['ADR-0008']}),
      record('ADR-0008', {related: ['ADR-0005']}),
    ]);
    assert.deepEqual(edges, [
      {kind: 'related', from: 'ADR-0005', to: 'ADR-0008'},
    ]);
  });

  it('drops a self-edge rather than rendering a chip to the current page', () => {
    assert.deepEqual(buildEdges([record('ADR-0001', {related: ['ADR-0001']})]), []);
  });

  it('is stably ordered', () => {
    const first = buildEdges([
      record('ADR-0002', {extends: ['ADR-0001']}),
      record('SPEC-0004', {implements: ['ADR-0009']}),
    ]);
    const second = buildEdges([
      record('SPEC-0004', {implements: ['ADR-0009']}),
      record('ADR-0002', {extends: ['ADR-0001']}),
    ]);
    assert.deepEqual(first, second);
  });
});

describe('buildRelations', () => {
  it('derives the inverse edge no author wrote', () => {
    const edges = buildEdges([record('SPEC-0004', {implements: ['ADR-0009']})]);
    const relations = buildRelations(['ADR-0009', 'SPEC-0004'], edges);
    assert.deepEqual(relations['ADR-0009']!.implementedBy, ['SPEC-0004']);
    assert.deepEqual(relations['SPEC-0004']!.implements, ['ADR-0009']);
    // And the ADR authored nothing in that direction.
    assert.deepEqual(relations['ADR-0009']!.implements, []);
  });

  it('shows a relationship authored from both ends exactly once per side', () => {
    const edges = buildEdges([
      record('ADR-0001', {enables: ['ADR-0002']}),
      record('ADR-0002', {extends: ['ADR-0001']}),
    ]);
    const relations = buildRelations(['ADR-0001', 'ADR-0002'], edges);
    assert.deepEqual(relations['ADR-0001']!.extendedBy, ['ADR-0002']);
    assert.deepEqual(relations['ADR-0002']!.extends, ['ADR-0001']);
    assert.deepEqual(relations['ADR-0001']!.extends, []);
    assert.deepEqual(relations['ADR-0002']!.extendedBy, []);
  });

  it('puts a symmetric relationship in the same bucket from either end', () => {
    const edges = buildEdges([record('ADR-0005', {related: ['ADR-0008']})]);
    const relations = buildRelations(['ADR-0005', 'ADR-0008'], edges);
    assert.deepEqual(relations['ADR-0005']!.related, ['ADR-0008']);
    assert.deepEqual(relations['ADR-0008']!.related, ['ADR-0005']);
  });

  it('gives every known record an entry, even an isolated one', () => {
    const relations = buildRelations(['ADR-0001'], []);
    assert.deepEqual(relations['ADR-0001'], {
      extends: [],
      extendedBy: [],
      related: [],
      implements: [],
      implementedBy: [],
      requires: [],
      requiredBy: [],
    });
  });

  it('sorts each bucket, so record.json does not churn on file order', () => {
    const edges = buildEdges([
      record('ADR-0001', {enables: ['ADR-0009', 'ADR-0002', 'ADR-0005']}),
    ]);
    const relations = buildRelations(['ADR-0001'], edges);
    assert.deepEqual(relations['ADR-0001']!.extendedBy, [
      'ADR-0002',
      'ADR-0005',
      'ADR-0009',
    ]);
  });
});
