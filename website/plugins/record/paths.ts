// Governing: ADR-0014, SPEC-0010 REQ "Record Content Pipeline"
//
// Every path this pipeline touches, resolved in one place.
//
// Two things make path handling load-bearing here. First, the record sources
// live OUTSIDE siteDir (`<repo>/docs/...` while the site is `<repo>/website/`),
// so nothing may assume a path sits under the site directory. Second, this
// module is loaded through three different runtimes — jiti (Docusaurus config
// and plugin loading, CommonJS), plain `node` (the `generate` script and the
// unit tests, ESM) and webpack (the remark plugin) — so it must not reach for
// `__dirname` or `import.meta.url`, neither of which exists in all three.
// siteDir is therefore always passed in by the caller that knows it.

import path from 'node:path';

export interface RecordPaths {
  /** `<repo>/website` — the Docusaurus siteDir. */
  siteDir: string;
  /** `<repo>` — one level above siteDir. The record lives here, not under it. */
  repoRoot: string;
  adrDir: string;
  specsDir: string;
  /**
   * The documentation content root. One `plugin-content-docs` instance serves
   * the tracked narrative pages and the git-ignored staged record side by side,
   * because `plugin-content-docs` takes a single `path` string.
   */
  docsDir: string;
  /** Staged output. Git-ignored; nothing here is ever committed. */
  stagedDecisionsDir: string;
  stagedSpecsDir: string;
  /** The derived data module. Git-ignored; rewritten on every load. */
  generatedDir: string;
  recordJson: string;
}

/** Docs route base, as configured on the docs preset. */
export const DOCS_ROUTE_BASE = '/docs';

/** Directory names of the two staged trees, relative to the docs content root. */
export const DECISIONS_SEGMENT = 'decisions';
export const SPECS_SEGMENT = 'specs';

export function createRecordPaths(siteDir: string): RecordPaths {
  const resolvedSiteDir = path.resolve(siteDir);
  const repoRoot = path.resolve(resolvedSiteDir, '..');
  const docsDir = path.join(resolvedSiteDir, 'docs');
  const generatedDir = path.join(resolvedSiteDir, 'src', 'generated');
  return {
    siteDir: resolvedSiteDir,
    repoRoot,
    adrDir: path.join(repoRoot, 'docs', 'adrs'),
    specsDir: path.join(repoRoot, 'docs', 'openspec', 'specs'),
    docsDir,
    stagedDecisionsDir: path.join(docsDir, DECISIONS_SEGMENT),
    stagedSpecsDir: path.join(docsDir, SPECS_SEGMENT),
    generatedDir,
    recordJson: path.join(generatedDir, 'record.json'),
  };
}

/**
 * Watch globs for `getPathsToWatch()`. Docusaurus rewrites absolute paths to
 * siteDir-relative ones and hands them to chokidar with `cwd: siteDir`
 * (`core/lib/commands/start/watcher.js`), so these legitimately become
 * `../docs/adrs/**` — outside the site directory, which is the point.
 */
export function recordWatchPaths(paths: RecordPaths): string[] {
  return [
    path.join(paths.adrDir, '**', '*.md'),
    path.join(paths.specsDir, '**', '*.md'),
  ];
}

/** Repo-relative, POSIX-separated — the form error messages and JSON use. */
export function repoRelative(paths: RecordPaths, absPath: string): string {
  return path.relative(paths.repoRoot, absPath).split(path.sep).join('/');
}

/**
 * True when the file being compiled is staged record content. The remark
 * transform is gated on this: hand-written narrative pages are authored as
 * site content and are none of the record pipeline's business.
 */
export function isStagedRecordFile(
  paths: RecordPaths,
  filePath: string | undefined,
): boolean {
  if (!filePath) {
    return false;
  }
  const resolved = path.resolve(filePath);
  return (
    resolved.startsWith(paths.stagedDecisionsDir + path.sep) ||
    resolved.startsWith(paths.stagedSpecsDir + path.sep)
  );
}
