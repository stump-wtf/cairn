import {themes as prismThemes} from 'prism-react-renderer';
import type {Config} from '@docusaurus/types';
import type * as Preset from '@docusaurus/preset-classic';

// This runs in Node.js - Don't use client-side code here (browser APIs, JSX...)

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

  presets: [
    [
      'classic',
      {
        docs: {
          sidebarPath: './sidebars.ts',
          editUrl: 'https://github.com/joestump/cairn/tree/main/website/',
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
        {to: '/docs/architecture', label: 'Architecture', position: 'left'},
        {to: '/docs/specifications', label: 'Specs', position: 'left'},
        {
          href: 'https://github.com/joestump/cairn',
          position: 'right',
          className: 'navbar__item--github',
          'aria-label': 'GitHub repository',
          label: 'GitHub',
        },
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
          title: 'Design',
          items: [
            {label: 'Architecture (ADRs)', to: '/docs/architecture'},
            {label: 'Specifications', to: '/docs/specifications'},
          ],
        },
        {
          title: 'More',
          items: [
            {label: 'GitHub', href: 'https://github.com/joestump/cairn'},
            {label: 'Issues', href: 'https://github.com/joestump/cairn/issues'},
          ],
        },
      ],
      copyright: `Cairn · built spec-first · © ${new Date().getFullYear()} Joe Stump`,
    },
    prism: {
      theme: prismThemes.vsDark,
      darkTheme: prismThemes.vsDark,
      additionalLanguages: ['bash', 'go', 'json'],
    },
  } satisfies Preset.ThemeConfig,
};

export default config;
