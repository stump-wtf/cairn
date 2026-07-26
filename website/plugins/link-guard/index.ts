// Governing: ADR-0014, SPEC-0010 REQ "No Repository Links"
//
// The rendered-output half of the link guard, as a `postBuild` hook.
//
// This is a plugin rather than a line in `docusaurus.config.ts` for one reason:
// `postBuild` is the only hook that runs after the HTML has been written, and
// it exists only on a plugin. The check itself lives in
// `website/scripts/check-links.mjs` next to its tests — this file is the wiring.
//
// `postBuild` does not run for `docusaurus start`, and that asymmetry is the
// point rather than an oversight: the dev server never writes HTML to disk, so
// there is no rendered output to scan. design.md ("Validation runs where it can
// actually run") accepts the resulting feedback latency, because scanning source
// for link-shaped strings instead would miss every link the theme assembles at
// render time.

import type {Plugin} from '@docusaurus/types';

// Plain ESM rather than TypeScript, matching scripts/check-tokens.mjs: the
// checker is shared with scripts/check-links.test.mjs, which `npm test` runs
// under bare `node`.
import {assertNoRepositoryLinks} from '../../scripts/check-links.mjs';

export default function cairnLinkGuardPlugin(): Plugin {
  return {
    name: 'cairn-link-guard',

    async postBuild({outDir, siteConfig}) {
      const {summary} = assertNoRepositoryLinks({
        outDir,
        siteUrl: siteConfig.url,
      });
      // eslint-disable-next-line no-console
      console.log(summary);
    },
  };
}
