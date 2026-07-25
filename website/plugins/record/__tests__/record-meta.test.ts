// Governing: ADR-0014, SPEC-0010 REQ "Derived Cross-Reference Graph"
// Governing: ADR-0014, SPEC-0010 REQ "Derived Status, Dates, and Counts"
//
// The two halves of the metadata bar that can be wrong without anything
// crashing: which staged pages get one, and which relation buckets it shows.
//
// A bar mounted on the wrong page renders nothing and looks like a styling bug;
// a bar mounted with the wrong doc id renders ANOTHER record's status and graph
// and looks like a correct page. Both are cheap to test here and expensive to
// notice in a browser, so both are tested here.

import assert from 'node:assert/strict';
import {describe, it} from 'node:test';

import {createRecordPaths, recordDetailDocId} from '../paths.ts';
import {chipGroups} from '../../../src/components/record/relations.ts';
import {emptyRelations} from '../graph.ts';

const paths = createRecordPaths('/repo/website');

describe('recordDetailDocId', () => {
  it('gives a decision page the doc id its DecisionEntry carries', () => {
    assert.equal(
      recordDetailDocId(paths, '/repo/website/docs/decisions/ADR-0009.md'),
      'decisions/ADR-0009',
    );
  });

  it('gives a specification page the doc id its SpecEntry carries', () => {
    assert.equal(
      recordDetailDocId(paths, '/repo/website/docs/specs/trajectory-share/index.md'),
      'specs/trajectory-share/index',
    );
  });

  it('refuses the two generated index pages', () => {
    assert.equal(
      recordDetailDocId(paths, '/repo/website/docs/decisions/index.mdx'),
      null,
    );
    assert.equal(recordDetailDocId(paths, '/repo/website/docs/specs/index.mdx'), null);
  });

  it('refuses a paired design document, which has no status or graph of its own', () => {
    assert.equal(
      recordDetailDocId(paths, '/repo/website/docs/specs/trajectory-share/design.md'),
      null,
    );
  });

  it('refuses a hand-written narrative page and an unknown caller', () => {
    assert.equal(recordDetailDocId(paths, '/repo/website/docs/overview.md'), null);
    assert.equal(recordDetailDocId(paths, undefined), null);
  });
});

describe('chipGroups', () => {
  it('renders no group for a record with no edges', () => {
    assert.deepEqual(chipGroups(emptyRelations()), []);
  });

  it('labels the computed half of the graph by direction, not by front-matter key', () => {
    // ADR-0001's shape in the record as it stands: nothing authored outward on
    // this page, seven decisions and a specification pointing back at it.
    const groups = chipGroups({
      ...emptyRelations(),
      extendedBy: ['ADR-0002', 'ADR-0003'],
      implementedBy: ['SPEC-0002'],
    });
    assert.deepEqual(
      groups.map((group) => [group.kind, group.label, group.direction]),
      [
        ['extendedBy', 'Extended by', 'inbound'],
        ['implementedBy', 'Implemented by', 'inbound'],
      ],
    );
    assert.deepEqual(groups[0]!.ids, ['ADR-0002', 'ADR-0003']);
  });

  it('puts what a record claims before what claims it, and related last', () => {
    const groups = chipGroups({
      extends: ['ADR-0001'],
      extendedBy: ['ADR-0007'],
      related: ['ADR-0009'],
      implements: ['ADR-0011'],
      implementedBy: ['SPEC-0004'],
      requires: ['SPEC-0002'],
      requiredBy: ['SPEC-0006'],
    });
    assert.deepEqual(
      groups.map((group) => group.kind),
      [
        'extends',
        'extendedBy',
        'implements',
        'implementedBy',
        'requires',
        'requiredBy',
        'related',
      ],
    );
  });
});
