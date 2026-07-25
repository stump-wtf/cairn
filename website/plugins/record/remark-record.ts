// Governing: ADR-0014, SPEC-0010 REQ "Record Prose to Structured Components"
// Governing: ADR-0014, SPEC-0010 REQ "CommonMark Fidelity"
//
// The compile-time half of the pipeline: a remark plugin, registered ahead of
// Docusaurus's own remark plugins via `beforeDefaultRemarkPlugins`, that turns
// record prose into components on the parsed tree.
//
// Registering *before* the defaults matters twice over:
//   - `remark/headings` and `remark/toc` run after us, so headings we nest
//     inside component subtrees still get anchors and still reach the
//     on-this-page column;
//   - the raw-HTML literalisation has to happen before `rehype-raw` turns an
//     `<id>` into a real element, and before the heading slugger reads the text.
//
// The plugin is gated on the file being staged record content: hand-written
// narrative pages under `website/docs/` are site content and are left alone.

/* eslint-disable @typescript-eslint/no-explicit-any */

import {
  RECORD_COMPONENTS,
  literaliseRawHtml,
  structureRequirements,
  wrapRfc2119Keywords,
} from './mdast.ts';
import {createRecordPaths, isStagedRecordFile} from './paths.ts';

export interface RemarkRecordOptions {
  /** The Docusaurus siteDir; the staged trees are resolved from it. */
  siteDir: string;
}

/** Where the record components live. One module, one injected import. */
export const RECORD_COMPONENTS_MODULE = '@site/src/components/record';

/**
 * Build the `mdxjsEsm` node that brings the record components into scope.
 *
 * The import cannot be written into the staged markdown. Those files are
 * `format: 'md'` precisely so that a bare `<` or `{` in record prose is inert;
 * in that mode an `import …` line is just a paragraph of text. And switching the
 * record to `.mdx` to make the import work is the trade this whole design
 * refuses: it would make every `<id>` in the record a parse error.
 *
 * So the import is injected as an AST node. MDX passes `mdxjsEsm` through from
 * mdast to hast untouched (`@mdx-js/mdx/lib/node-types.js`) and Docusaurus adds
 * it to `rehype-raw`'s passThrough list for `md` documents
 * (`@docusaurus/mdx-loader/lib/processor.js`), so it survives to `recmaDocument`
 * and becomes a real top-level import.
 */
async function createComponentImport(): Promise<any> {
  const {parse} = await import('acorn');
  const value = `import {${RECORD_COMPONENTS.join(', ')}} from '${RECORD_COMPONENTS_MODULE}';`;
  const estree = parse(value, {
    ecmaVersion: 'latest',
    sourceType: 'module',
    locations: true,
  });
  return {type: 'mdxjsEsm', value, data: {estree}};
}

/**
 * A remark transformer. Exported as a plugin factory so `docusaurus.config.ts`
 * can bind it to the site directory.
 */
export default function remarkRecord(options: RemarkRecordOptions) {
  const paths = createRecordPaths(options.siteDir);

  return async function transformer(tree: any, file: any): Promise<void> {
    if (!isStagedRecordFile(paths, file?.path)) {
      return;
    }

    // Order is load-bearing. Literalise raw HTML first, so `<id>` is text before
    // anything else reads the tree; structure the requirement sections next, so
    // the keyword pass sees the final shape; wrap keywords last, so the
    // structural matchers never have to look through an `<Rfc2119>` wrapper.
    literaliseRawHtml(tree);
    await structureRequirements(tree);
    wrapRfc2119Keywords(tree);

    // After the front-matter node, not before it: the `yaml` node is inert by
    // the time we see it, but keeping it first keeps the tree honest.
    const insertAt = tree.children[0]?.type === 'yaml' ? 1 : 0;
    tree.children.splice(insertAt, 0, await createComponentImport());
  };
}
