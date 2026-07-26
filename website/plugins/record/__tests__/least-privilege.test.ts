// Governing: ADR-0014, SPEC-0010 REQ "Deployment Least Privilege"
//
// The derived data module half of the requirement. The bundle half is tested in
// scripts/scan-bundle.test.mjs; both call the same pattern set, and the test
// below exists so that "the data module is scanned too" is a fact rather than a
// comment.

import assert from 'node:assert/strict';
import {describe, it} from 'node:test';

import {assertNoSecretsInData} from '../validate.ts';
import type {RecordData} from '../types.ts';

function data(overrides: Partial<RecordData> = {}): RecordData {
  return {
    decisions: [],
    specs: [],
    counts: {decisions: 0, specifications: 0, requirements: 0, scenarios: 0},
    graph: {edges: [], relations: {}},
    refs: {},
    ...overrides,
  } as RecordData;
}

describe('secrets in the derived data module', () => {
  it('passes a data module carrying only what the site renders', () => {
    assert.doesNotThrow(() =>
      assertNoSecretsInData(
        data({
          refs: {
            'ADR-0001': {
              id: 'ADR-0001',
              title: 'Cairn as an AI-Native Artifact Store',
              href: '/docs/decisions/ADR-0001',
            },
          },
        }),
      ),
    );
  });

  it('fails when a summary drags a private host name into the module', () => {
    // A summary is derived from an ADR's first paragraph, so a decision that
    // discusses the forge in its opening line would publish that hostname in
    // the data module every page loads.
    assert.throws(
      () =>
        assertNoSecretsInData(
          data({
            refs: {
              'ADR-0001': {
                id: 'ADR-0001',
                title: 'Mirrors from gitea.stump.rocks',
                href: '/docs/decisions/ADR-0001',
              },
            },
          }),
        ),
      /private forge hostname/,
    );
  });

  it('fails when a credential reaches the module', () => {
    assert.throws(
      () =>
        assertNoSecretsInData(
          data({
            refs: {
              'ADR-0001': {
                id: 'ADR-0001',
                title: 'Token ghp_0123456789abcdef0123456789abcdef0123',
                href: '/docs/decisions/ADR-0001',
              },
            },
          }),
        ),
      /GitHub token/,
    );
  });

  it("names the module rather than a file, because that is where the value is", () => {
    assert.throws(
      () => assertNoSecretsInData(data({refs: {x: {id: 'x', title: '10.0.4.17', href: '/x'}}})),
      /derived record data module/,
    );
  });
});
