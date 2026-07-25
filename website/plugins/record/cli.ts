// Governing: ADR-0014, SPEC-0010 REQ "Record Content Pipeline"
//
// A standalone entry point for the generator, used by `npm run generate`.
//
// This exists for the tools that are not Docusaurus: `tsc` needs the derived
// data module on disk before it can typecheck the components that import it, and
// a clean checkout has no staged tree. It is emphatically NOT the build's
// generation step — `docusaurus build` and `docusaurus start` both generate from
// the config factory, because a `generate && build` chain is what leaves the dev
// server serving a stale record.

import process from 'node:process';

import {generateRecord} from './generate.ts';

const siteDir = process.argv[2] ?? process.cwd();

generateRecord({siteDir})
  .then(({data, written}) => {
    process.stdout.write(
      `cairn-record: staged ${data.counts.decisions} decisions and ` +
        `${data.counts.specifications} specifications ` +
        `(${data.counts.requirements} requirements, ${data.counts.scenarios} scenarios) ` +
        `into ${written.length} files\n`,
    );
  })
  .catch((error: unknown) => {
    process.stderr.write(`${(error as Error).message}\n`);
    process.exitCode = 1;
  });
