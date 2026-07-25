import {themes as prismThemes} from 'prism-react-renderer';
import type {Config} from '@docusaurus/types';
import type * as Preset from '@docusaurus/preset-classic';
import {assertTokens} from './scripts/check-tokens.mjs';
import {assertA11y, assertRecordSemantics} from './scripts/check-a11y.mjs';

import {generateRecord} from './plugins/record/generate.ts';
import remarkRecord from './plugins/record/remark-record.ts';

// This runs in Node.js - Don't use client-side code here (browser APIs, JSX...)

// Governing: ADR-0014, SPEC-0010 REQ "Record Content Pipeline"
//
// The site directory. `__dirname` is safe here: this file is only ever loaded
// through jiti (`@docusaurus/utils` `loadFreshModule`), which loads it as
// CommonJS.
const siteDir = __dirname;

const DOCS_ROUTE_BASE = '/docs';

/**
 * Governing: ADR-0014, SPEC-0010 REQ "Design Token Source of Truth"
 *
 * The token-layer guard is a SOURCE-level check, and design.md ("Validation runs
 * where it can actually run") puts those at CONFIG LOAD rather than in a
 * pre-build npm script: config load happens on every `docusaurus build` *and*
 * every `docusaurus start`, so an author who hardcodes a colour or recolours a
 * category finds out in the dev server instead of after merge. A throw here also
 * means there is no way around it — `npx docusaurus build` bypasses anything
 * hanging off `npm run build`, but it cannot bypass loading the config.
 */
// eslint-disable-next-line no-console
console.log(assertTokens().summary);

/**
 * Governing: ADR-0014, SPEC-0010 REQ "Contrast",
 *            SPEC-0010 REQ "Keyboard Navigation & Focus Management",
 *            SPEC-0010 REQ "WCAG 2.1 AA & Semantics"
 *
 * The same argument as the line above, for the same reason: an accessibility
 * regression that only CI can see is one a developer ships and then discovers.
 * This half re-measures every rendered colour pair in both colour modes and
 * asserts the focus and reduced-motion machinery exists; the heading-order half
 * needs the staged record and runs from the factory below, once it is on disk.
 */
// eslint-disable-next-line no-console
console.log(assertA11y().summary);

/**
 * Governing: ADR-0014, SPEC-0010 REQ "Design Token Source of Truth"
 *
 * Docusaurus turns `plain.color` / `plain.backgroundColor` into the inline
 * `--prism-color` / `--prism-background-color` pair on every code block, and an
 * inline style cannot be overridden from a stylesheet. Left alone, vsDark's
 * #1E1E1E ground would therefore paint every code block on a #0a0b0d page and
 * be unreachable from the token definition. Pointing the plain pair at `var()`
 * references keeps the colour literal out of this file and makes the code
 * surface resolve through src/css/custom.css like every other surface.
 *
 * The syntax token colours stay vsDark's: they are a highlighting theme shipped
 * by a dependency rather than part of the site palette, and they are built for
 * exactly this ground.
 */
const cairnPrismTheme = {
  ...prismThemes.vsDark,
  plain: {
    ...prismThemes.vsDark.plain,
    color: 'var(--cairn-code-fg)',
    backgroundColor: 'var(--cairn-code-bg)',
  },
};

const config: Config = {
  title: 'Cairn',
  tagline: 'AI-native artifact sharing — pbcopy for the agent era.',
  favicon: 'img/favicon.svg',

  future: {
    v4: true,
  },

  // GitHub Pages project site: https://joestump.github.io/cairn/
  // (For a custom domain like cairn.sh, set url to the domain and baseUrl to '/'.)
  url: 'https://joestump.github.io',
  baseUrl: '/cairn/',
  trailingSlash: false,

  organizationName: 'joestump',
  projectName: 'cairn',

  onBrokenLinks: 'warn',

  i18n: {
    defaultLocale: 'en',
    locales: ['en'],
  },

  // Treat .md as CommonMark so spec/ADR-style content (<id>, {…}, comparisons)
  // never trips the MDX parser; .mdx still gets full MDX.
  markdown: {
    format: 'detect',
    hooks: {
      onBrokenMarkdownLinks: 'warn',
    },
  },

  // Governing: ADR-0014, SPEC-0010 REQ "WCAG 2.1 AA & Semantics"
  // The on-this-page column is the one required landmark theme-classic does not
  // render; `clientModules` gives it a `navigation` role without spending the
  // capability's single theme-wrap budget. See the module for the trade-off.
  clientModules: ['./src/clientModules/tocLandmark.ts'],

  plugins: [
    // Governing: ADR-0014, SPEC-0010 REQ "Record Content Pipeline"
    // The record has already been staged by the factory below. This plugin
    // exists for `getPathsToWatch()`: without it, `docusaurus start` serves a
    // frozen snapshot of the record for the rest of the session.
    // The `.ts` extension is required, not stylistic: Docusaurus resolves a
    // local plugin path with Node's `require.resolve`
    // (`core/lib/server/plugins/moduleShorthand.js`), which knows nothing about
    // TypeScript and would not find `index.ts` by directory resolution. jiti
    // does the transpiling once the exact path is known.
    ['./plugins/record/index.ts', {routeBase: DOCS_ROUTE_BASE}],
  ],

  presets: [
    [
      'classic',
      {
        docs: {
          sidebarPath: './sidebars.ts',
          // Governing: ADR-0014, SPEC-0010 REQ "No Repository Links"
          // No `editUrl`. The full record text renders on the site, so there
          // is nothing an "edit this page" link could usefully point at, and
          // the repository it pointed at is not publicly readable.

          // Governing: ADR-0014, SPEC-0010 REQ "Record Prose to Structured Components"
          // Registered *before* the default remark plugins so that the
          // structural rewrite happens before `remark/headings` assigns
          // anchors, before `remark/toc` collects them, and before
          // `rehype-raw` turns a tag-shaped word like `<id>` into a real
          // element.
          beforeDefaultRemarkPlugins: [[remarkRecord, {siteDir}]],
        },
        blog: false,
        theme: {
          customCss: './src/css/custom.css',
        },
      } satisfies Preset.Options,
    ],
  ],

  themeConfig: {
    metadata: [
      {name: 'description', content: 'Cairn — AI-native artifact sharing. A pastebin/gist/requestbin for the agent era: humans post from CLI/web, agents read & write over MCP.'},
    ],
    colorMode: {
      defaultMode: 'dark',
      respectPrefersColorScheme: false,
      disableSwitch: false,
    },
    navbar: {
      title: 'Cairn',
      logo: {
        alt: 'Cairn',
        src: 'img/logo.svg',
      },
      items: [
        {
          type: 'docSidebar',
          sidebarId: 'docsSidebar',
          position: 'left',
          label: 'Docs',
        },
        // Governing: ADR-0014, SPEC-0010 REQ "No Repository Links"
        // These replace the forge link and the two hand-maintained index pages
        // this capability deletes.
        {to: '/docs/decisions', label: 'Decisions', position: 'left'},
        {to: '/docs/specs', label: 'Specs', position: 'left'},
      ],
    },
    footer: {
      style: 'dark',
      links: [
        {
          title: 'Docs',
          items: [
            {label: 'Overview', to: '/docs/intro'},
            {label: 'Share types', to: '/docs/share-types'},
            {label: 'Surfaces', to: '/docs/surfaces'},
            {label: 'Annotations', to: '/docs/annotations'},
          ],
        },
        {
          title: 'Design record',
          items: [
            {label: 'Decisions', to: '/docs/decisions'},
            {label: 'Specifications', to: '/docs/specs'},
          ],
        },
      ],
      copyright: `Cairn · built spec-first · © ${new Date().getFullYear()} Joe Stump`,
    },
    prism: {
      theme: cairnPrismTheme,
      darkTheme: cairnPrismTheme,
      additionalLanguages: ['bash', 'go', 'json'],
    },
  } satisfies Preset.ThemeConfig,
};

/**
 * The config is exported as an async factory, and that is the load-bearing part
 * of the whole pipeline.
 *
 * `loadSiteConfig` awaits a function config
 * (`@docusaurus/core/lib/server/config.js`) and `loadContext` completes before
 * `loadPlugins` (`.../server/site.js`), so awaiting the generator here is the
 * last point that is reliably ahead of *all* plugin initialisation. Staging from
 * a plugin instead does not work: `plugin-content-docs` stat-checks its content
 * directory in its own factory, and all plugin factories — and later all
 * `loadContent()` calls — run inside `Promise.all`, so the docs plugin would race
 * the generator and build N would publish what build N−1 staged.
 *
 * The config object itself stays a plain top-level `const`: the factory only
 * wraps it. Re-indenting the whole object into the function body would turn a
 * twenty-line change into a two-hundred-line diff over the `themeConfig` that
 * concurrent work on the navbar, footer and chrome also has to touch.
 *
 * The consequence, accepted in ADR-0014: `docusaurus build` alone is no longer
 * sufficient on a clean checkout for anything that loads this config by another
 * path.
 */
export default async function createConfig(): Promise<Config> {
  const {data} = await generateRecord({
    siteDir,
    routeBase: DOCS_ROUTE_BASE,
  });

  /**
   * Governing: ADR-0014, SPEC-0010 REQ "WCAG 2.1 AA & Semantics", scenario
   * "Generated page heading order".
   *
   * After staging, not before: this reads the transformed record on disk, so it
   * checks what the docs plugin is about to serve rather than what the source
   * tree happens to contain.
   */
  const semantics = assertRecordSemantics({root: siteDir});

  // eslint-disable-next-line no-console
  console.log(
    `[cairn-a11y] ${semantics.files} documentation pages carry exactly one h1 and ` +
      `no skipped heading level`,
  );

  // eslint-disable-next-line no-console
  console.log(
    `[cairn-record] ${data.counts.decisions} decisions, ` +
      `${data.counts.specifications} specifications, ` +
      `${data.counts.requirements} requirements, ` +
      `${data.counts.scenarios} scenarios staged from the design record`,
  );

  return config;
}
