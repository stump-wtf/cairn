import {themes as prismThemes} from 'prism-react-renderer';
import type {Config, Plugin} from '@docusaurus/types';
import type * as Preset from '@docusaurus/preset-classic';
import {assertTokens} from './scripts/check-tokens.mjs';
import {assertNoEditUrl} from './scripts/check-links.mjs';
import {assertFirstPartyAssets} from './scripts/check-assets.mjs';
import {
  assertA11y,
  assertRecordSemantics,
  assertRenderedSemantics,
} from './scripts/check-a11y.mjs';

import {generateRecord} from './plugins/record/generate.ts';
import remarkRecord from './plugins/record/remark-record.ts';
import cairnLinkGuardPlugin from './plugins/link-guard/index.ts';

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
 * Governing: ADR-0014, SPEC-0010 REQ "First-Party Asset Loading"
 *
 * Same placement, same reason. REQ "First-Party Asset Loading"'s scenario
 * "Remote font import in source" is a check against the FILE, so it belongs
 * beside the token guard rather than in the postBuild bundle scan, which cannot
 * run during `docusaurus start` at all. The two families this site uses are
 * vendored under `src/css/fonts/`; re-introducing an `@import` from a font CDN
 * fails here, in the dev server, naming the file and the host.
 */
// eslint-disable-next-line no-console
console.log(assertFirstPartyAssets().summary);

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

/**
 * Governing: ADR-0014, SPEC-0010 REQ "WCAG 2.1 AA & Semantics", scenario
 * "Generated page heading order".
 *
 * The last of the three heading-order passes, and the only one that reads the
 * page as a browser receives it. The two above it read markdown — the source
 * tree and then the staged record — and markdown is where *authors* put
 * headings. A generated index page is one authored `#` heading plus a React
 * component, and every other heading on `/docs/specs` is emitted from `.tsx`;
 * no markdown check can see those, and one of them shipped an h1 → h3 jump.
 *
 * `postBuild` is the earliest hook where the HTML exists, so this cannot run in
 * the dev server. That is the correct division: the source-level passes are
 * fast and fire on `docusaurus start`, and this one gates the artifact that
 * actually deploys. It is a plugin rather than an npm script for the same
 * reason the others are config-load assertions — `npx docusaurus build` routes
 * around `npm run build`, but it cannot route around a registered plugin.
 */
function cairnRenderedA11yPlugin(): Plugin {
  return {
    name: 'cairn-rendered-a11y',
    async postBuild({outDir}) {
      const rendered = assertRenderedSemantics({outDir});
      // eslint-disable-next-line no-console
      console.log(
        `[cairn-a11y] ${rendered.files} rendered pages carry exactly one h1 and no ` +
          `skipped heading level`,
      );
    },
  };
}

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

  /*
   * Governing: ADR-0014, SPEC-0010 REQ "Link and Anchor Integrity"
   *
   * Both of these were `warn`, which on a site whose entire subject is a
   * cross-referenced record means the reader finds the dangling link, not the
   * build. A record that publishes 14 decisions and 10 specifications is one
   * renamed heading away from a broken `#anchor`, and a warning in a CI log
   * nobody reads is indistinguishable from no check at all.
   *
   * Worth knowing where these do NOT fire. `handleBrokenLinks` has exactly one
   * caller — `@docusaurus/core/lib/commands/build/buildLocale.js` — so neither
   * setting can fail anything under `docusaurus start`. That is why SPEC-0010
   * REQ "Build and Deployment" requires CI to run a full build on every pull
   * request: without one, setting these to `throw` changes nothing at all.
   */
  onBrokenLinks: 'throw',
  onBrokenAnchors: 'throw',

  i18n: {
    defaultLocale: 'en',
    locales: ['en'],
  },

  // Treat .md as CommonMark so spec/ADR-style content (<id>, {…}, comparisons)
  // never trips the MDX parser; .mdx still gets full MDX.
  markdown: {
    format: 'detect',
    hooks: {
      // A `[SPEC-0004](./specs/does-not-exist/index.md)` in a narrative page is
      // resolved here, by the MDX loader, before `onBrokenLinks` ever sees a
      // route — so leaving this at `warn` would leave a hole in the check above.
      // Unlike the two settings above this one does run under `docusaurus
      // start`, which is the only broken-link feedback the dev server gives.
      onBrokenMarkdownLinks: 'throw',
    },
  },

  // Governing: ADR-0014, SPEC-0010 REQ "WCAG 2.1 AA & Semantics",
  //            SPEC-0010 REQ "Dynamic Content Regions"
  // The on-this-page column is the one required landmark theme-classic does not
  // render, and below 997px its collapse button ships no `aria-expanded`.
  // `clientModules` fixes both without spending the capability's single
  // theme-wrap budget. See the module for the trade-off.
  clientModules: ['./src/clientModules/tocAccessibility.ts'],

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

    // Governing: ADR-0014, SPEC-0010 REQ "No Repository Links"
    // The rendered-output half of the link guard. `postBuild` only — there is
    // no rendered output to scan during `docusaurus start`.
    cairnLinkGuardPlugin,
    // Governing: ADR-0014, SPEC-0010 REQ "Security Headers"
    // Governing: ADR-0014, SPEC-0010 REQ "Deployment Least Privilege"
    // A `postBuild` hook and nothing else: it seals a hashed Content-Security-
    // Policy into every emitted page, writes `_headers` for a header-capable
    // host, and fails the build if the bundle carries a third-party subresource,
    // a credential, or a private host name. Registered last so it runs over a
    // finished bundle.
    cairnRenderedA11yPlugin,
    './plugins/security/index.ts',
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
    /*
     * Governing: ADR-0014, SPEC-0010 REQ "Site Chrome and Layout"
     *
     * Every value here is theme-classic's own default. They are stated anyway,
     * because each one is load-bearing for a MUST in the spec and a default is
     * not a decision until it is written down:
     *
     * - `hideable: false` — the rail is *persistent*. With `hideable: true`
     *   theme-classic renders a collapse handle that reduces the sidebar to a
     *   30px stub, and a reader who hits it once loses the record tree for the
     *   rest of the session.
     * - `autoCollapseCategories: false` — opening a decision must not close the
     *   Specifications group. The two trees are meant to be visible from each
     *   other; that is the whole reason the record is served from one content
     *   instance.
     * - `tableOfContents` 2–3 — `### Requirement:` is an h3 in every
     *   specification, so a max of 2 would empty the on-this-page column on
     *   exactly the pages that need it most.
     */
    docs: {
      sidebar: {
        hideable: false,
        autoCollapseCategories: false,
      },
    },
    tableOfContents: {
      minHeadingLevel: 2,
      maxHeadingLevel: 3,
    },
    navbar: {
      title: 'Cairn',
      logo: {
        alt: 'Cairn',
        src: 'img/logo.svg',
      },
      // The navbar is sticky (theme-classic always applies `navbar--fixed-top`);
      // `hideOnScroll` would make it *not* persist, which the spec's "sticky top
      // navigation bar" rules out.
      hideOnScroll: false,
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
      // Governing: ADR-0014, SPEC-0010 REQ "Code Block Fidelity"
      //
      // The floor the requirement names, stated in full rather than trimmed to
      // what is missing today. theme-classic seeds the highlighter from
      // prism-react-renderer's bundled grammars and then `require`s
      // `prismjs/components/prism-<lang>` for each entry here
      // (`theme/prism-include-languages.ts`), so the covered set is the union
      // of the two. That bundle happens to carry `go`, `json`, `sql` and
      // `yaml` today and does not carry `bash` — but it is a dependency's
      // internal choice, and a requirement of this site should not be
      // satisfied by one. Naming all five makes the floor independent of it.
      //
      // scripts/prism-languages.test.mjs reads the floor out of the
      // specification's own text and checks this list against it, so a
      // language added to the requirement cannot be forgotten here.
      additionalLanguages: ['bash', 'go', 'json', 'sql', 'yaml'],
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
  /*
   * Governing: ADR-0014, SPEC-0010 REQ "No Repository Links"
   *
   * The configuration half of the link guard, run here rather than as a comment
   * on the docs preset. A comment saying "no editUrl" is exactly as strong as
   * the attention of the next person to edit this file; this walks the whole
   * config object and names the key path it found, so `editUrl` cannot be
   * reintroduced on the docs preset, on the blog, or on a plugin entry nobody
   * thought to guard.
   */
  // eslint-disable-next-line no-console
  console.log(assertNoEditUrl(config).summary);

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
