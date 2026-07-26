// Governing: ADR-0014, SPEC-0010 REQ "Derived Design-Language Page"
//
// The badge set is read out of prose, so the tests fix the boundary rather than
// today's total: which shapes count as a declaration, which do not, and that the
// real record still yields a non-empty set attributed to real records. Asserting
// "seven badges" here would be the fact-written-twice defect ADR-0014 exists to
// prevent.

import assert from 'node:assert/strict';
import path from 'node:path';
import {before, describe, it} from 'node:test';

import {buildBadgeSet, collectBadgeCodes} from '../badges.ts';
import {generateRecord} from '../generate.ts';
import {literaliseRawHtml, parseRecordMarkdown} from '../mdast.ts';
import type {ParsedRecord, RecordData, RecordRef} from '../types.ts';

const siteDir = path.resolve(import.meta.dirname, '..', '..', '..');

async function codesOf(markdown: string): Promise<string[]> {
  const tree = await parseRecordMarkdown(markdown);
  literaliseRawHtml(tree);
  return collectBadgeCodes(tree);
}

describe('collectBadgeCodes', () => {
  it('reads a list of codes out of a badge bullet', async () => {
    // ADR-0002's `ShareType` seam, verbatim in shape.
    assert.deepEqual(
      await codesOf(
        '* **Badge** — the short code shown in the shell and the Bin\n' +
          '  (`MD`, `PY`/lang, `IMG`, `FILE`/`GZ`, `HK`, `TRJ`; bundle renders its own badge).\n',
      ),
      ['MD', 'PY', 'IMG', 'FILE', 'GZ', 'HK', 'TRJ'],
    );
  });

  it('reads a single declaration in an overview sentence', async () => {
    assert.deepEqual(
      await codesOf(
        'The **trajectory** share type (badge `TRJ`, URL `cairn.sh/run/<id>`) captures a run.\n',
      ),
      ['TRJ'],
    );
  });

  it('does not treat a code span in an unrelated block as a badge', async () => {
    // The word has to be in the SAME block; a file-wide scan would turn every
    // upper-case code span in a decision that mentions badges into one.
    assert.deepEqual(
      await codesOf(
        'The type badge sits beside the title.\n\n' +
          'Spans carry an `HTTP` verb and a `TTL`.\n',
      ),
      [],
    );
  });

  it('ignores code spans that are not badge-shaped', async () => {
    assert.deepEqual(
      await codesOf(
        'Badges MUST meet AA contrast against the `#0A0B0D` canvas, per `--cat-other`.\n',
      ),
      [],
    );
  });
});

describe('buildBadgeSet', () => {
  const ref = (id: string): RecordRef => ({id, title: id, href: `/docs/${id}`});
  const rec = (
    id: string,
    badgeCodes: string[],
  ): Pick<ParsedRecord, 'id' | 'badgeCodes'> => ({id, badgeCodes});

  it('keeps first-declaration order and every declaring record', () => {
    const badges = buildBadgeSet(
      [rec('ADR-0002', ['MD', 'TRJ']), rec('SPEC-0004', ['TRJ'])],
      {'ADR-0002': ref('ADR-0002'), 'SPEC-0004': ref('SPEC-0004')},
    );
    assert.deepEqual(
      badges.map((badge) => badge.code),
      ['MD', 'TRJ'],
    );
    assert.deepEqual(
      badges[1]!.sources.map((source) => source.id),
      ['ADR-0002', 'SPEC-0004'],
    );
  });

  it('fails loudly rather than publishing an empty badge section', () => {
    assert.throws(() => buildBadgeSet([rec('ADR-0001', [])], {}), /no share-type badge/);
  });
});

describe('the badge set over the real record', () => {
  let data: RecordData;

  before(async () => {
    ({data} = await generateRecord({siteDir}));
  });

  it('publishes a set the record declares, attributed to real records', () => {
    assert.ok(data.badges.length > 0);
    for (const badge of data.badges) {
      assert.match(badge.code, /^[A-Z][A-Z0-9]{1,4}$/);
      assert.ok(badge.sources.length > 0, `${badge.code} cites no record`);
      for (const source of badge.sources) {
        assert.ok(
          data.refs[source.id],
          `${badge.code} cites unknown record ${source.id}`,
        );
        assert.equal(source.href, data.refs[source.id]!.href);
      }
    }
  });

  it('is not the category palette under another name', () => {
    // The defect this replaced: rendering the ADR-0009 span categories twice and
    // captioning the second copy "type badges".
    const codes = new Set(data.badges.map((badge) => badge.code.toLowerCase()));
    assert.ok(!codes.has('reason'));
    assert.ok(!codes.has('exec'));
  });
});
