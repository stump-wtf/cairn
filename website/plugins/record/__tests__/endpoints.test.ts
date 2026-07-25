// Governing: ADR-0014, SPEC-0010 REQ "Derived HTTP Reference Page"
//
// The unit tests fix the shapes the record actually ships — three different
// column layouts, a combined method/path cell, a spaced-dash auth cell — because
// those are what a naive positional parser gets wrong. The end-to-end tests
// assert invariants over the real record rather than today's totals: asserting
// "34 endpoints" would be the same fact-written-twice defect ADR-0014 exists to
// prevent, one layer down.

import assert from 'node:assert/strict';
import path from 'node:path';
import {before, describe, it} from 'node:test';

import {
  ENDPOINT_SECTION_NAMES,
  WEBSITE_CAPABILITY,
  buildEndpointReference,
  collectEndpointSections,
} from '../endpoints.ts';
import {generateRecord} from '../generate.ts';
import {literaliseRawHtml, parseRecordMarkdown} from '../mdast.ts';
import {createRecordPaths} from '../paths.ts';
import type {EndpointSection, RecordData, SpecEntry} from '../types.ts';

const siteDir = path.resolve(import.meta.dirname, '..', '..', '..');

async function sectionsOf(markdown: string): Promise<EndpointSection[]> {
  const tree = await parseRecordMarkdown(markdown);
  literaliseRawHtml(tree);
  return collectEndpointSections(tree);
}

describe('collectEndpointSections', () => {
  it('reads a combined "Method & Path" column', async () => {
    const [section] = await sectionsOf(
      '# SPEC-0006: Thing\n\n## HTTP Endpoints\n\n' +
        '| Method & Path | Purpose | Auth |\n|---|---|---|\n' +
        '| `GET /v1/artifacts/{id}/reactions` | List reactions | Public — gated by link capability (ADR-0007) |\n',
    );
    assert.ok(section);
    assert.equal(section.title, 'HTTP Endpoints');
    assert.equal(section.rows.length, 1);
    assert.equal(section.rows[0]!.method, 'GET');
    assert.equal(section.rows[0]!.path, '/v1/artifacts/{id}/reactions');
    assert.equal(section.rows[0]!.authPosture, 'Public');
    // The justification the record wrote is kept, not thrown away with the dash.
    assert.match(section.rows[0]!.auth!, /gated by link capability/);
  });

  it('keeps a parenthetical the author wrote beside the path', async () => {
    const [section] = await sectionsOf(
      '## HTTP Endpoints\n\n| Method & Path | Purpose | Auth |\n|---|---|---|\n' +
        '| `ANY /h/{id}` (also `hook.cairn.sh/{id}`) | Ingress | **Public** |\n',
    );
    assert.equal(section!.rows[0]!.method, 'ANY');
    assert.equal(section!.rows[0]!.path, '/h/{id}');
    assert.equal(section!.rows[0]!.note, '(also hook.cairn.sh/{id})');
    assert.equal(section!.rows[0]!.authPosture, 'Public');
  });

  it('identifies columns by header, not by position', async () => {
    // `Endpoint | Method | …` puts the path first; `Method | Path | …` puts the
    // method first. Both ship in the record today.
    const [pathFirst] = await sectionsOf(
      '## Endpoint Table\n\n| Endpoint | Method | Purpose | Auth |\n|---|---|---|---|\n' +
        '| `/oauth/token` | POST | Code exchange | Client-authenticated (PKCE) |\n',
    );
    const [methodFirst] = await sectionsOf(
      '## REST Endpoints\n\n| Method | Path | Purpose | Auth |\n|---|---|---|---|\n' +
        '| POST | `/v1/artifacts` | Create an artifact | Required |\n',
    );
    assert.deepEqual(
      [pathFirst!.rows[0]!.method, pathFirst!.rows[0]!.path],
      ['POST', '/oauth/token'],
    );
    // A hyphen without surrounding spaces is part of the posture, not a split.
    assert.equal(pathFirst!.rows[0]!.authPosture, 'Client-authenticated (PKCE)');
    assert.deepEqual(
      [methodFirst!.rows[0]!.method, methodFirst!.rows[0]!.path],
      ['POST', '/v1/artifacts'],
    );
  });

  it('derives the streaming flag rather than taking a list of it', async () => {
    const [section] = await sectionsOf(
      '## Endpoint Table\n\n| Endpoint | Method | Purpose | Auth |\n|---|---|---|---|\n' +
        '| `/mcp` | POST / GET (SSE) | MCP transport | Required |\n' +
        '| `/v1/runs/{id}/stream` | GET | Live span appends | Link-cap |\n' +
        '| `/v1/artifacts` | POST | Create | Required |\n',
    );
    assert.deepEqual(
      section!.rows.map((row) => row.streaming),
      [true, true, false],
    );
  });

  it('ignores a heading that is not one of the enumerated names', async () => {
    const sections = await sectionsOf(
      '## Requirements\n\n| Method & Path | Purpose | Auth |\n|---|---|---|\n' +
        '| `GET /nope` | Not an endpoint table | Required |\n',
    );
    assert.deepEqual(sections, []);
  });

  it('contributes nothing for a table with no path-bearing column', async () => {
    const sections = await sectionsOf(
      '## Web Routes\n\n| Surface | Notes |\n|---|---|\n| Web | Something |\n',
    );
    assert.deepEqual(sections, []);
  });

  it('stops collecting at the next section', async () => {
    const sections = await sectionsOf(
      '## HTTP Endpoints\n\n| Method & Path | Purpose | Auth |\n|---|---|---|\n' +
        '| `GET /a` | A | Required |\n\n' +
        '## Security Requirements\n\n| Method & Path | Purpose | Auth |\n|---|---|---|\n' +
        '| `GET /b` | B | Required |\n',
    );
    assert.equal(sections.length, 1);
    assert.deepEqual(
      sections[0]!.rows.map((row) => row.path),
      ['/a'],
    );
  });

  it('records the anchor Docusaurus will emit for the section heading', async () => {
    const [section] = await sectionsOf(
      '# SPEC-0007: Thing\n\n## Endpoint Table\n\n' +
        '| Endpoint | Method | Purpose | Auth |\n|---|---|---|---|\n' +
        '| `/mcp` | POST | Transport | Required |\n',
    );
    assert.equal(section!.anchor, 'endpoint-table');
  });
});

describe('buildEndpointReference', () => {
  const spec = (id: string, capability: string): SpecEntry =>
    ({
      id,
      capability,
      title: id,
      href: `/docs/specs/${capability}`,
    }) as SpecEntry;

  it("drops the website specification's own route table", () => {
    const sections: EndpointSection[] = [
      {
        title: 'Web Routes',
        anchor: 'web-routes',
        rows: [
          {
            method: null,
            path: '/docs/decisions',
            note: null,
            purpose: 'Decisions index',
            auth: null,
            authPosture: null,
            streaming: false,
          },
        ],
      },
    ];
    const result = buildEndpointReference(
      [spec('SPEC-0010', WEBSITE_CAPABILITY)],
      new Map([['SPEC-0010', sections]]),
    );
    assert.deepEqual(result.rows, []);
    assert.equal(result.coverage[0]!.rowCount, 0);
    assert.deepEqual(result.coverage[0]!.sections, []);
  });

  it('fails loudly if the excluded capability no longer exists', () => {
    assert.throws(
      () => buildEndpointReference([spec('SPEC-0001', 'somewhere-else')], new Map()),
      /public-website-and-design-record/,
    );
  });
});

describe('the derived reference surface over the real record', () => {
  let data: RecordData;

  before(async () => {
    ({data} = await generateRecord({siteDir}));
  });

  it('scans exactly the enumerated section names', () => {
    assert.deepEqual(data.endpoints.sectionNames, [...ENDPOINT_SECTION_NAMES]);
    for (const spec of data.endpoints.coverage) {
      for (const section of spec.sections) {
        assert.ok(
          (ENDPOINT_SECTION_NAMES as readonly string[]).includes(section),
          `${spec.specId} contributed an unenumerated section '${section}'`,
        );
      }
    }
  });

  it('covers every specification, contributing or not', () => {
    assert.deepEqual(
      data.endpoints.coverage.map((spec) => spec.specId),
      data.specs.map((spec) => spec.id),
    );
    // The page may not claim a complete API surface, which is only honest if
    // some specification is genuinely silent. If that ever stops being true the
    // page's own wording should be revisited rather than this assertion relaxed.
    assert.ok(
      data.endpoints.coverage.some((spec) => spec.rowCount === 0),
      'no specification is silent; the reference page may now be complete',
    );
  });

  it('publishes no row from the website specification', () => {
    const website = data.specs.find(
      (spec) => spec.capability === WEBSITE_CAPABILITY,
    );
    assert.ok(website);
    assert.equal(
      data.endpoints.rows.filter((row) => row.specId === website.id).length,
      0,
    );
  });

  it('links every row to the table it came from', () => {
    const paths = createRecordPaths(siteDir);
    assert.ok(paths.specsDir);
    assert.ok(data.endpoints.rows.length > 0);
    for (const row of data.endpoints.rows) {
      const owner = data.specs.find((spec) => spec.id === row.specId);
      assert.ok(owner, `row ${row.path} names an unknown spec ${row.specId}`);
      assert.equal(row.specHref, owner.href);
      assert.ok(
        row.sectionHref.startsWith(`${owner.href}#`),
        `row ${row.path} does not deep-link into ${owner.id}`,
      );
      assert.ok(row.path.startsWith('/'), `row path '${row.path}' is not a path`);
    }
  });

  it('carries the auth posture wherever the source states one', () => {
    for (const row of data.endpoints.rows) {
      if (row.auth === null) {
        assert.equal(row.authPosture, null);
        continue;
      }
      assert.ok(row.authPosture);
      assert.ok(row.auth.startsWith(row.authPosture));
    }
  });
});
