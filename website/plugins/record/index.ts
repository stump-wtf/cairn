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

    contentLoaded({content, actions}) {
      // Published as plugin global data as well as `src/generated/record.json`.
      // The JSON module is what components import — a plain import works at
      // module scope, on the homepage and inside MDX alike — while the global
      // data makes the same object reachable through `usePluginData` without a
      // path alias. Both are the same object from the same run; neither is a
      // second source of truth.
      actions.setGlobalData(content);
    },
  };
}
