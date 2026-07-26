// Governing: ADR-0014, SPEC-0010 REQ "Record Content Pipeline"
// Governing: ADR-0014, SPEC-0010 REQ "Derived Data Module"
//
// The generator. One module, called from two places:
//
//   1. `docusaurus.config.ts`, which exports an async factory and awaits this
//      before returning the config object. `loadSiteConfig` awaits a function
//      config (`@docusaurus/core/lib/server/config.js`) and `loadContext`
//      completes before `loadPlugins` (`.../server/site.js`), so this is the
//      last hook that is reliably ahead of *all* plugin initialisation.
//   2. the record plugin's `loadContent()`, so an edit made while the dev server
//      is running re-stages.
//
// It deliberately is not an `npm run generate && docusaurus build` chain. That
// chain leaves `docusaurus start` serving a frozen snapshot while an author
// edits ADRs, which is the failure that makes people conclude the pipeline is
// broken. It is also not a plugin `loadContent()` alone: plugin content loading
// runs inside one `Promise.all`
// (`@docusaurus/core/lib/server/plugins/plugins.js`), so the docs plugin would
// glob the staged tree concurrently with this writing it and build N would
// publish what build N−1 staged.
//
// The generator is idempotent, so being called twice on a cold start is
// harmless.

import path from 'node:path';

import {
  RecordError,
  listAdrFiles,
  listCapabilityDirs,
  parseDesignFile,
  parseRecordFile,
} from './parse.ts';
import {buildBadgeSet} from './badges.ts';
import {assertNoLiteralCounts} from './counts.ts';
import {buildEdges, buildRelations} from './graph.ts';
import {buildEndpointReference} from './endpoints.ts';
import {collectDesignTokens, stageDesignTokens} from './tokens.ts';
import {
  DOCS_ROUTE_BASE,
  createRecordPaths,
  repoRelative,
  type RecordPaths,
} from './paths.ts';
import {
  decisionDocId,
  decisionHref,
  designDocId,
  specDocId,
  specHref,
  stageRecord,
} from './stage.ts';
import {
  assertNoSecretsInData,
  assertStagedCoverage,
  validateRecord,
} from './validate.ts';
import type {
  DecisionEntry,
  EndpointSection,
  ParsedRecord,
  RecordData,
  RecordRef,
  SpecEntry,
} from './types.ts';

export {RecordError};

export interface GenerateOptions {
  siteDir: string;
  /** Docs route base; must match the docs preset's `routeBasePath`. */
  routeBase?: string;
}

export interface GenerateResult {
  data: RecordData;
  paths: RecordPaths;
  /** Absolute paths of everything staged, for logging. */
  written: string[];
}

/**
 * Read the record, derive everything, validate, stage, and emit the data module.
 */
export async function generateRecord(
  options: GenerateOptions,
): Promise<GenerateResult> {
  const paths = createRecordPaths(options.siteDir);
  const routeBase = options.routeBase ?? DOCS_ROUTE_BASE;

  const adrFiles = await listAdrFiles(paths);
  const capabilityDirs = await listCapabilityDirs(paths);

  const decisionRecords: ParsedRecord[] = [];
  for (const absPath of adrFiles) {
    decisionRecords.push(
      await parseRecordFile({paths, absPath, kind: 'ADR'}),
    );
  }

  const specRecords: ParsedRecord[] = [];
  const capabilityOf = new Map<string, string>();
  const designs = new Map<
    string,
    {sourcePath: string; body: string; title: string}
  >();

  for (const dir of capabilityDirs) {
    const specPath = path.join(dir, 'spec.md');
    let record: ParsedRecord;
    try {
      record = await parseRecordFile({paths, absPath: specPath, kind: 'SPEC'});
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code === 'ENOENT') {
        throw new RecordError(
          repoRelative(paths, dir),
          `capability directory has no spec.md, so it produces no card`,
        );
      }
      throw error;
    }
    specRecords.push(record);
    capabilityOf.set(record.id, path.basename(dir));

    const designPath = path.join(dir, 'design.md');
    try {
      const design = await parseDesignFile(paths, designPath);
      designs.set(record.id, {
        sourcePath: design.sourcePath,
        body: design.body,
        title: design.title,
      });
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== 'ENOENT') {
        throw error;
      }
      // A capability with no paired design document is legal; the spec only
      // requires the design page to be reachable *when it exists*.
    }
  }

  validateRecord({decisions: decisionRecords, specs: specRecords});

  const decisions: DecisionEntry[] = decisionRecords
    .map((record) => ({
      id: record.id,
      number: record.number,
      title: record.title,
      status: record.status,
      date: record.date,
      summary: record.summary,
      docId: decisionDocId(record.id),
      href: decisionHref(routeBase, record.id),
      sourcePath: record.sourcePath,
    }))
    .sort((a, b) => a.number - b.number);

  // Specification numbering comes from each `# SPEC-XXXX:` heading, never from
  // directory order: the alphabetically first capability directory is
  // `annotations`, which is SPEC-0006.
  const specs: SpecEntry[] = specRecords
    .map((record) => {
      const capability = capabilityOf.get(record.id)!;
      const hasDesign = designs.has(record.id);
      return {
        id: record.id,
        number: record.number,
        title: record.title,
        capability,
        status: record.status,
        date: record.date,
        summary: record.summary,
        requirementCount: record.requirements.length,
        scenarioCount: record.scenarioCount,
        requirements: record.requirements,
        docId: specDocId(capability),
        href: specHref(routeBase, capability),
        designHref: hasDesign ? `${specHref(routeBase, capability)}/design` : null,
        designDocId: hasDesign ? designDocId(capability) : null,
        sourcePath: record.sourcePath,
      };
    })
    .sort((a, b) => a.number - b.number);

  const allRecords = [...decisionRecords, ...specRecords];
  const edges = buildEdges(allRecords);
  const relations = buildRelations(
    allRecords.map((record) => record.id),
    edges,
  );

  const refs: Record<string, RecordRef> = {};
  for (const decision of decisions) {
    refs[decision.id] = {
      id: decision.id,
      title: decision.title,
      href: decision.href,
    };
  }
  for (const spec of specs) {
    refs[spec.id] = {id: spec.id, title: spec.title, href: spec.href};
  }

  // Governing: ADR-0014, SPEC-0010 REQ "Derived HTTP Reference Page"
  // The reference page's rows are flattened here rather than in the parser,
  // because which sections reach the page is a policy about the site (the
  // website specification's own route table is not a service API) and parsing
  // has no business knowing about it.
  const endpointSections = new Map<string, EndpointSection[]>(
    specRecords.map((record) => [record.id, record.endpointSections]),
  );
  const endpoints = buildEndpointReference(specs, endpointSections);

  // Governing: ADR-0014, SPEC-0010 REQ "Derived Design-Language Page"
  // The badge set is record-derived, not stylesheet-derived, so it belongs to
  // the record's module rather than to the token inventory — the requirement
  // that names the derived data module names "badge" among the things that must
  // read from it. Decisions come first so the set reads in the order ADR-0002
  // wrote it.
  const badges = buildBadgeSet(allRecords, refs);

  const data: RecordData = {
    decisions,
    specs,
    badges,
    endpoints,
    counts: {
      decisions: decisions.length,
      specifications: specs.length,
      requirements: specs.reduce(
        (total, spec) => total + spec.requirementCount,
        0,
      ),
      scenarios: specs.reduce((total, spec) => total + spec.scenarioCount, 0),
    },
    graph: {edges, relations},
    refs,
  };

  // Governing: ADR-0014, SPEC-0010 REQ "Deployment Least Privilege"
  // Checked on the derived object, before it is written anywhere.
  assertNoSecretsInData(data);

  const bodies = new Map<string, {sourcePath: string; body: string}>();
  for (const record of allRecords) {
    bodies.set(record.id, {
      sourcePath: record.sourcePath,
      body: record.body,
    });
  }

  const written = await stageRecord({
    paths,
    data,
    bodies,
    designBodies: designs,
  });

  // Governing: ADR-0014, SPEC-0010 REQ "Derived Design-Language Page"
  // Read from `src/css/custom.css`, not from the record, and emitted beside the
  // record's module rather than inside it — see `tokens.ts` for why the two are
  // kept apart.
  written.push(
    await stageDesignTokens(paths, await collectDesignTokens(paths)),
  );

  // Coverage is asserted against the staged tree rather than against the lists
  // above, so that the check is capable of failing. See `assertStagedCoverage`.
  await assertStagedCoverage({paths, adrFiles, capabilityDirs, data});

  // Governing: ADR-0014, SPEC-0010 REQ "Derived Status, Dates, and Counts"
  //
  // Deliberately last, and deliberately here rather than in a lint script. It
  // needs the derived counts to explain itself ("the record has 14; read
  // counts.decisions"), and it has to run inside the pipeline so that writing
  // "14 decisions" into the homepage fails the dev server rather than surviving
  // until someone reads the page. See counts.ts for why it is an AST walk.
  await assertNoLiteralCounts({paths, counts: data.counts});

  return {data, paths, written};
}
