// Governing: ADR-0014, SPEC-0010 REQ "Derived Data Module"
// Governing: ADR-0014, SPEC-0010 REQ "Referential Integrity and Front-Matter Validation"

import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import {after, before, describe, it} from 'node:test';

import {
  RecordError,
  idNumber,
  isAdrId,
  isSpecId,
  normaliseDate,
  normaliseStatus,
  parseRecordFile,
  splitFrontMatter,
} from '../parse.ts';
import {createRecordPaths} from '../paths.ts';
import {validateRecord} from '../validate.ts';
import type {ParsedRecord} from '../types.ts';

describe('splitFrontMatter', () => {
  it('returns the body byte-for-byte', () => {
    const raw = '---\nstatus: accepted\n---\n\n# ADR-0001: Thing\n\nBody < here.\n';
    const {frontMatter, body} = splitFrontMatter(raw);
    assert.equal(frontMatter, 'status: accepted\n');
    assert.equal(body, '\n# ADR-0001: Thing\n\nBody < here.\n');
  });

  it('keeps front-matter comments in the block for the YAML parser to ignore', () => {
    const raw =
      '---\nstatus: accepted\n# Do NOT author inverse edges.\nextends: [ADR-0001]\n---\n\nBody\n';
    const {frontMatter} = splitFrontMatter(raw);
    assert.match(frontMatter, /# Do NOT author inverse edges\./);
    assert.match(frontMatter, /extends: \[ADR-0001\]/);
  });

  it('handles an empty front-matter block', () => {
    const {frontMatter, body} = splitFrontMatter('---\n---\nBody\n');
    assert.equal(frontMatter, '');
    assert.equal(body, 'Body\n');
  });

  it('passes a file with no front-matter through untouched', () => {
    const raw = '# Design: Thing\n\nProse.\n';
    assert.deepEqual(splitFrontMatter(raw), {frontMatter: '', body: raw});
  });
});

describe('front-matter scalars', () => {
  it('accepts a YAML date object and a quoted string alike', () => {
    assert.equal(normaliseDate(new Date('2026-07-08T00:00:00Z')), '2026-07-08');
    assert.equal(normaliseDate('2026-07-08'), '2026-07-08');
  });

  it('rejects anything that is not a parseable YYYY-MM-DD date', () => {
    assert.equal(normaliseDate('July 2026'), null);
    assert.equal(normaliseDate('2026-7-8'), null);
    assert.equal(normaliseDate(undefined), null);
    assert.equal(normaliseDate(new Date('nonsense')), null);
  });

  it('accepts the status enum case-insensitively and nothing else', () => {
    assert.equal(normaliseStatus('accepted'), 'accepted');
    assert.equal(normaliseStatus('ACCEPTED'), 'accepted');
    assert.equal(normaliseStatus('draft'), 'draft');
    assert.equal(normaliseStatus('superseded'), 'superseded');
    assert.equal(normaliseStatus('shipped'), null);
    assert.equal(normaliseStatus(undefined), null);
  });
});

describe('identifiers', () => {
  it('recognises the two record namespaces', () => {
    assert.ok(isAdrId('ADR-0009'));
    assert.ok(!isAdrId('SPEC-0009'));
    assert.ok(isSpecId('SPEC-0004'));
    assert.ok(!isSpecId('ADR-4'));
  });

  it('reads the number from the identifier, not from file order', () => {
    assert.equal(idNumber('ADR-0014'), 14);
    assert.equal(idNumber('SPEC-0001'), 1);
  });
});

describe('parseRecordFile', () => {
  let dir: string;
  let paths: ReturnType<typeof createRecordPaths>;

  before(async () => {
    dir = await fs.mkdtemp(path.join(os.tmpdir(), 'cairn-record-'));
    // createRecordPaths treats its argument as siteDir and looks for the record
    // one level *above* it, which is the real layout: the record is not inside
    // the site directory.
    await fs.mkdir(path.join(dir, 'repo', 'docs', 'adrs'), {recursive: true});
    paths = createRecordPaths(path.join(dir, 'repo', 'website'));
  });

  after(async () => {
    await fs.rm(dir, {recursive: true, force: true});
  });

  async function write(name: string, contents: string): Promise<string> {
    const absPath = path.join(paths.adrDir, name);
    await fs.writeFile(absPath, contents, 'utf8');
    return absPath;
  }

  it('derives id, number and title from the heading', async () => {
    const absPath = await write(
      'ADR-0042-a-thing.md',
      '---\nstatus: accepted\ndate: 2026-07-08\ndecision-makers: joestump\n---\n\n# ADR-0042: A Thing With `code` In It\n\nThe first paragraph.\n',
    );
    const record = await parseRecordFile({paths, absPath, kind: 'ADR'});
    assert.equal(record.id, 'ADR-0042');
    assert.equal(record.number, 42);
    assert.equal(record.title, 'A Thing With code In It');
    assert.equal(record.status, 'accepted');
    assert.equal(record.date, '2026-07-08');
    assert.equal(record.summary, 'The first paragraph.');
    assert.equal(record.sourcePath, 'docs/adrs/ADR-0042-a-thing.md');
  });

  it('names the file and the field when status is missing', async () => {
    const absPath = await write(
      'ADR-0043-no-status.md',
      '---\ndate: 2026-07-08\n---\n\n# ADR-0043: No Status\n\nProse.\n',
    );
    await assert.rejects(
      () => parseRecordFile({paths, absPath, kind: 'ADR'}),
      (error: Error) => {
        assert.ok(error instanceof RecordError);
        assert.match(error.message, /ADR-0043-no-status\.md/);
        assert.match(error.message, /missing front-matter field 'status'/);
        return true;
      },
    );
  });

  it('names the offending value when status is outside the enum', async () => {
    const absPath = await write(
      'ADR-0044-bad-status.md',
      '---\nstatus: shipped\ndate: 2026-07-08\n---\n\n# ADR-0044: Bad Status\n\nProse.\n',
    );
    await assert.rejects(
      () => parseRecordFile({paths, absPath, kind: 'ADR'}),
      /'status' is 'shipped'/,
    );
  });

  it('names the offending value when the date will not parse', async () => {
    const absPath = await write(
      'ADR-0045-bad-date.md',
      '---\nstatus: accepted\ndate: someday\n---\n\n# ADR-0045: Bad Date\n\nProse.\n',
    );
    await assert.rejects(
      () => parseRecordFile({paths, absPath, kind: 'ADR'}),
      /'date' is 'someday'/,
    );
  });

  it('rejects a heading that does not carry the identifier', async () => {
    const absPath = await write(
      'ADR-0046-bad-heading.md',
      '---\nstatus: accepted\ndate: 2026-07-08\n---\n\n# A Thing\n\nProse.\n',
    );
    await assert.rejects(
      () => parseRecordFile({paths, absPath, kind: 'ADR'}),
      /does not match '# ADR-XXXX: <title>'/,
    );
  });

  it('rejects a graph edge that is not a list of identifiers', async () => {
    const absPath = await write(
      'ADR-0047-bad-edge.md',
      '---\nstatus: accepted\ndate: 2026-07-08\nextends: ADR-0001\n---\n\n# ADR-0047: Bad Edge\n\nProse.\n',
    );
    await assert.rejects(
      () => parseRecordFile({paths, absPath, kind: 'ADR'}),
      /'extends' must be a list of record identifiers/,
    );
  });

  it('does not republish front-matter the renderer does not need', async () => {
    const absPath = await write(
      'ADR-0048-extras.md',
      '---\nstatus: accepted\ndate: 2026-07-08\ndecision-makers: joestump\nconsulted: someone\n---\n\n# ADR-0048: Extras\n\nProse.\n',
    );
    const record = await parseRecordFile({paths, absPath, kind: 'ADR'});
    assert.deepEqual(Object.keys(record.authored), []);
    assert.ok(!('decision-makers' in record));
  });
});

function stub(
  id: string,
  overrides: Partial<ParsedRecord> = {},
): ParsedRecord {
  return {
    id,
    number: Number(id.slice(-4)),
    title: id,
    status: 'accepted',
    date: '2026-07-08',
    summary: '',
    authored: {},
    requirements: [],
    scenarioCount: 0,
    absPath: `/tmp/${id}.md`,
    sourcePath: `docs/adrs/${id}.md`,
    body: '',
    ...overrides,
  };
}

describe('validateRecord', () => {
  const ok = {
    decisions: [stub('ADR-0001')],
    specs: [
      stub('SPEC-0001', {
        authored: {implements: ['ADR-0001']},
        sourcePath: 'docs/openspec/specs/thing/spec.md',
      }),
    ],
    adrSourcePaths: ['docs/adrs/ADR-0001.md'],
    capabilityDirs: ['docs/openspec/specs/thing'],
    renderedCapabilityDirs: ['docs/openspec/specs/thing'],
  };

  it('accepts a coherent record', () => {
    assert.doesNotThrow(() => validateRecord(ok));
  });

  it('names the referring file and the unresolved identifier', () => {
    assert.throws(
      () =>
        validateRecord({
          ...ok,
          decisions: [
            stub('ADR-0001', {authored: {related: ['ADR-0099']}}),
          ],
        }),
      (error: Error) => {
        assert.match(error.message, /docs\/adrs\/ADR-0001\.md/);
        assert.match(error.message, /'related' names 'ADR-0099'/);
        assert.match(error.message, /resolves to no decision record/);
        return true;
      },
    );
  });

  it('requires a `requires:` entry to resolve to a specification, not a decision', () => {
    assert.throws(
      () =>
        validateRecord({
          ...ok,
          specs: [
            stub('SPEC-0001', {
              authored: {implements: ['ADR-0001'], requires: ['ADR-0001']},
              sourcePath: 'docs/openspec/specs/thing/spec.md',
            }),
          ],
        }),
      /resolves to no capability specification/,
    );
  });

  it('requires every specification to declare what it implements', () => {
    assert.throws(
      () =>
        validateRecord({
          ...ok,
          specs: [
            stub('SPEC-0001', {
              sourcePath: 'docs/openspec/specs/thing/spec.md',
            }),
          ],
        }),
      /missing front-matter field 'implements'/,
    );
  });

  it('fails and names a decision file that produced no page', () => {
    assert.throws(
      () =>
        validateRecord({
          ...ok,
          adrSourcePaths: [
            'docs/adrs/ADR-0001.md',
            'docs/adrs/ADR-0002-forgotten.md',
          ],
        }),
      (error: Error) => {
        assert.match(error.message, /ADR-0002-forgotten\.md/);
        assert.match(error.message, /produced no page/);
        return true;
      },
    );
  });

  it('fails and names a capability directory that produced no card', () => {
    assert.throws(
      () =>
        validateRecord({
          ...ok,
          capabilityDirs: [
            'docs/openspec/specs/thing',
            'docs/openspec/specs/other',
          ],
        }),
      /docs\/openspec\/specs\/other/,
    );
  });

  it('rejects a duplicate identifier and names both files', () => {
    assert.throws(
      () =>
        validateRecord({
          ...ok,
          decisions: [
            stub('ADR-0001', {sourcePath: 'docs/adrs/ADR-0001-a.md'}),
            stub('ADR-0001', {sourcePath: 'docs/adrs/ADR-0001-b.md'}),
          ],
          adrSourcePaths: ['docs/adrs/ADR-0001-a.md', 'docs/adrs/ADR-0001-b.md'],
        }),
      (error: Error) => {
        assert.match(error.message, /ADR-0001-b\.md/);
        assert.match(error.message, /already used by docs\/adrs\/ADR-0001-a\.md/);
        return true;
      },
    );
  });
});
