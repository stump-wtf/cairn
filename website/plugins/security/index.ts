// Governing: ADR-0014, SPEC-0010 REQ "Security Headers"
// Governing: ADR-0014, SPEC-0010 REQ "First-Party Asset Loading"
// Governing: ADR-0014, SPEC-0010 REQ "Deployment Least Privilege"
//
// Everything this capability can do about response headers on a static bundle,
// in one `postBuild` hook. The policy itself lives in
// `scripts/security-policy.mjs`; this file is the part that has to touch the
// emitted files.
//
// Three steps, in order, because each depends on the last:
//
//   1. SEAL. Docusaurus emits two inline scripts on every page — the base-URL
//      banner and the colour-mode bootstrap. A CSP that allowed them by
//      `'unsafe-inline'` would allow every injected script too, so the policy
//      names them by sha256 hash instead. The hashes can only be computed once
//      the HTML is final, which is precisely now, and they are computed PER PAGE
//      from the bytes the browser will parse — not from a table someone
//      maintains, which would be the same "written twice" defect ADR-0014 is
//      about, one layer down.
//
//      The tag is inserted immediately after `<head>` rather than injected via
//      `injectHtmlTags`. A meta-delivered policy applies only to what the parser
//      sees AFTER it, so its position is load-bearing, and a plugin's head tags
//      land wherever Docusaurus decides to put them.
//
//   2. WRITE `_headers`. The union of every page's hashes, plus the directives
//      and the headers a `<meta>` cannot deliver at all. Inert on GitHub Pages;
//      read by Netlify and Cloudflare Pages. See security-policy.mjs for why
//      this exists in that state rather than not existing.
//
//   3. SCAN. The bundle is checked for a third-party subresource and for a
//      credential or private host name, and the build fails on either. This runs
//      here, inside `docusaurus build`, so that a local build and CI reach the
//      same verdict and no deploy path can skip it — REQ "Deployment Least
//      Privilege" requires the scan to happen before the deploy step, and the
//      strongest form of "before" is "inside the thing that produces the
//      artifact".
//
// There is deliberately no dev-server equivalent. `docusaurus start` renders no
// HTML to disk, and its HMR runtime needs allowances a published page must not
// have; a CSP that had to accommodate both would be the weaker of the two.

import fs from 'node:fs/promises';
import path from 'node:path';

import type {LoadContext, Plugin} from '@docusaurus/types';

import {
  inlineScriptHashes,
  metaPolicy,
  renderHeadersFile,
} from '../../scripts/security-policy.mjs';
import {assertBundle} from '../../scripts/scan-bundle.mjs';

const HEAD_OPEN = /<head(\s[^>]*)?>/i;

export default function cairnSecurityPlugin(
  _context: LoadContext,
): Plugin<void> {
  return {
    name: 'cairn-security',

    async postBuild({outDir}) {
      const pages = await htmlFiles(outDir);
      const allHashes = new Set<string>();

      for (const file of pages) {
        const html = await fs.readFile(file, 'utf8');

        const hashes = inlineScriptHashes(html);
        for (const hash of hashes) {
          allHashes.add(hash);
        }

        const policy = metaPolicy(hashes);
        // The policy contains single quotes (`'self'`, the hashes) and no double
        // quotes, so a double-quoted attribute needs no escaping. Asserted rather
        // than assumed, because a silently mangled policy is a policy that stops
        // applying without anyone noticing.
        if (policy.includes('"')) {
          throw new Error(
            `cairn-security: the CSP contains a double quote and would break the meta ` +
              `attribute: ${policy}`,
          );
        }

        const tag = `<meta http-equiv="Content-Security-Policy" content="${policy}">`;
        if (!HEAD_OPEN.test(html)) {
          throw new Error(
            `cairn-security: ${path.relative(outDir, file)} has no <head>, so the ` +
              `Content-Security-Policy cannot be delivered on it.`,
          );
        }
        await fs.writeFile(
          file,
          html.replace(HEAD_OPEN, (open) => `${open}${tag}`),
          'utf8',
        );
      }

      await fs.writeFile(
        path.join(outDir, '_headers'),
        renderHeadersFile([...allHashes].sort()),
        'utf8',
      );

      // Deliberately after the rewrite: the scan must see what ships, including
      // the tag just inserted.
      const result = assertBundle({outDir});
      // eslint-disable-next-line no-console
      console.log(
        `[cairn-security] ${result.summary} ` +
          `CSP sealed over ${pages.length} pages with ${allHashes.size} inline-script ` +
          `hashes; _headers written for a header-capable host.`,
      );
    },
  };
}

async function htmlFiles(dir: string): Promise<string[]> {
  const entries = await fs.readdir(dir, {withFileTypes: true});
  const out: string[] = [];
  for (const entry of entries) {
    const absPath = path.join(dir, entry.name);
    if (entry.isDirectory()) {
      out.push(...(await htmlFiles(absPath)));
    } else if (entry.isFile() && /\.html?$/i.test(entry.name)) {
      out.push(absPath);
    }
  }
  return out.sort();
}
