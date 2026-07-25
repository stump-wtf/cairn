// Governing: ADR-0014, SPEC-0010 REQ "Record Content Pipeline"
//
// The record plugin. It is thin on purpose: the heavy lifting already ran from
// the async config factory before this plugin was constructed. What is left is
// the development loop.
//
// `getPathsToWatch()` is the whole reason this is a Docusaurus plugin and not a
// prebuild script. A prebuild script runs once and then `docusaurus start`
// serves a frozen snapshot of the record for the rest of the session, so an
// author editing an ADR sees nothing change and concludes the pipeline is
// broken. With this plugin registered, Docusaurus watches the record source
// trees (`@docusaurus/core/lib/commands/start/watcher.js` →
// `getPluginPathsToWatch`) and re-runs `loadContent()` on a change, which
// re-stages; the docs plugin's own watcher then picks up the rewritten staged
// files.

import type {LoadContext, Plugin} from '@docusaurus/types';

import {generateRecord} from './generate.ts';
import {recordWatchPaths, createRecordPaths} from './paths.ts';
import type {RecordData} from './types.ts';

export interface RecordPluginOptions {
  /** Docs route base; must match the docs preset's `routeBasePath`. */
  routeBase?: string;
}

export default function cairnRecordPlugin(
  context: LoadContext,
  options: RecordPluginOptions = {},
): Plugin<RecordData> {
  const paths = createRecordPaths(context.siteDir);

  return {
    name: 'cairn-record',

    getPathsToWatch() {
      // Absolute globs. Docusaurus rewrites them relative to siteDir, so these
      // become `../docs/adrs/**/*.md` — the record lives outside the site
      // directory and that is not a mistake to be corrected.
      return recordWatchPaths(paths);
    },

    async loadContent() {
      // On a cold start the tree is already current from config load, so this is
      // a refresh rather than a first write. The generator is idempotent and
      // skips files whose bytes have not changed, so a no-op edit does not
      // bounce the docs plugin.
      const {data} = await generateRecord({
        siteDir: context.siteDir,
        routeBase: options.routeBase,
      });
      return data;
    },

    // Governing: ADR-0014, SPEC-0010 REQ "Derived Data Module"
    //
    // Deliberately no `contentLoaded`, and specifically no `setGlobalData`.
    //
    // Plugin global data is serialised into `globalData.json`, which the client
    // entry imports unconditionally, so publishing the record that way puts all
    // 49 KB of it into `main.js` — paid for on every route, including the
    // homepage, which renders none of it. It also makes the record reachable by
    // two different paths, and "the pipeline emits *a single* derived data
    // module" is the requirement, not a stylistic preference.
    //
    // The single module is `src/generated/record.json`, imported through
    // `@site/src/data/record`. A plain import works at module scope, in a page
    // and inside MDX alike, and webpack keeps it in the chunks that actually
    // reference it.
  };
}
