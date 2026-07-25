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
  attribute,
  jsxFlowElement,
  literaliseRawHtml,
  structureRequirements,
  wrapRfc2119Keywords,
} from './mdast.ts';
import {
  createRecordPaths,
  isStagedRecordFile,
  recordDetailDocId,
} from './paths.ts';

export interface RemarkRecordOptions {
  /** The Docusaurus siteDir; the staged trees are resolved from it. */
  siteDir: string;
}

/** Where the record components live. One module, one injected import. */
export const RECORD_COMPONENTS_MODULE = '@site/src/components/record';

// Governing: ADR-0014, SPEC-0010 REQ "Derived Cross-Reference Graph"
/**
 * The metadata bar, imported from its own module and only onto record DETAIL
 * pages.
 *
 * It is kept out of `RECORD_COMPONENTS_MODULE` because that module is imported
 * by every staged page, index pages included, and this one reaches the derived
 * data module. Splitting them keeps the derived data on the routes that render
 * a record and off the ones that do not.
 */
export const RECORD_META_MODULE = '@site/src/components/record/meta';
export const RECORD_META_COMPONENT = 'RecordMeta';

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
async function createImport(names: readonly string[], module: string): Promise<any> {
  const {parse} = await import('acorn');
  const value = `import {${names.join(', ')}} from '${module}';`;
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

    // Governing: ADR-0014, SPEC-0010 REQ "Derived Cross-Reference Graph"
    //
    // The metadata bar goes AFTER the document's `# ` heading, and that position
    // is load-bearing twice over. Docusaurus reads the first heading of a
    // document as its `contentTitle` and suppresses its own `<h1>` when it finds
    // one, so a node inserted ahead of the heading would give every record page
    // two titles. And a bar that carries a record's status, date and graph reads
    // as a caption on the title, not as an introduction to it.
    const detailDocId = recordDetailDocId(paths, file?.path);
    if (detailDocId) {
      const heading = tree.children.findIndex(
        (node: any) => node.type === 'heading' && node.depth === 1,
      );
      tree.children.splice(
        heading === -1 ? tree.children.length : heading + 1,
        0,
        jsxFlowElement(
          RECORD_META_COMPONENT,
          [attribute('docId', detailDocId)],
          [],
        ),
      );
    }

    // After the front-matter node, not before it: the `yaml` node is inert by
    // the time we see it, but keeping it first keeps the tree honest. Both
    // imports are spliced last, so the heading index computed above is not
    // shifted out from under the bar.
    const insertAt = tree.children[0]?.type === 'yaml' ? 1 : 0;
    const imports = [await createImport(RECORD_COMPONENTS, RECORD_COMPONENTS_MODULE)];
    if (detailDocId) {
      imports.push(await createImport([RECORD_META_COMPONENT], RECORD_META_MODULE));
    }
    tree.children.splice(insertAt, 0, ...imports);
  };
}
