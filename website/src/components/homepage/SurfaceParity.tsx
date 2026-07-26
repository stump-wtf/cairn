// Governing: ADR-0014, SPEC-0010 REQ "Homepage"
//
// Surface parity across web, CLI and MCP. This is the only stateful thing on
// the homepage, and design.md says why it may not simply render blank without
// JavaScript: "a marketing page that renders blank without JS is a poor
// advertisement for a project that server-serves its actual product".
//
// HOW THE NO-JS FALLBACK WORKS, since the shape is load-bearing:
//
//   * every panel is in the DOM on every render — the tab state only decides
//     which one is *displayed*, so the server-rendered HTML already contains
//     all three panels in full;
//   * the `<noscript>` stylesheet below un-hides all of them, hides the tab
//     list, and separates the panels into a stacked column.
//
// A browser with JavaScript never parses `<noscript>` content, so the tabbed
// presentation costs nothing; a browser without it never runs the tab handler,
// and gets the stacked column instead of two hidden panels and three dead
// buttons — SPEC-0010 REQ "Homepage", scenario "Homepage without JavaScript".
//
// The alternative — deciding the layout from a `useEffect` "have we mounted"
// flag — would render stacked, then collapse to tabs a frame later, for every
// reader. Shipping the fallback as CSS keeps the flash out of the common case.

import {useCallback, useRef, useState, type KeyboardEvent, type ReactNode} from 'react';
import clsx from 'clsx';
import Heading from '@theme/Heading';
import {Section, SectionHead, Shot, SpecLink} from './Section';
import styles from './styles.module.css';

type Surface = {
  key: string;
  tab: string;
  title: string;
  body: ReactNode;
  points: ReactNode[];
  spec: string;
  shot: {src: string; alt: string; width: number; height: number};
};

const SURFACES: Surface[] = [
  {
    key: 'web',
    tab: 'Web',
    title: 'The Bin',
    body: (
      <>
        One app shell for every share: same header, same URL control, same collapsible metadata and
        comments panel. <strong>The Bin</strong> lists what you and your agents have dropped — type
        badge, title, provenance, counts.
      </>
    ),
    points: [
      <>Highlight any text and the comment lands in the panel beside it.</>,
      <>
        One URL control switches between the human link and <code>mcp://</code>.
      </>,
      <>Provenance, TTL and access policy are always on screen, never in a menu.</>,
    ],
    spec: 'SPEC-0001',
    shot: {
      src: '/img/shots/web-markdown.png',
      alt: 'The Cairn web app shell rendering a markdown artifact: a header carrying the Cairn mark, a type badge, the filename and a URL control offering both the short link and an MCP link; the rendered document with inline reactions in the middle; and a details rail listing provenance (Claude, via MCP, pushed 2 hours ago), an expiry of 5 days, a contents outline and a comment count.',
      width: 924,
      height: 504,
    },
  },
  {
    key: 'cli',
    tab: 'CLI',
    title: 'cat file | cairn',
    body: (
      <>
        Pipe in, get a link — that is the whole contract. The CLI holds the same auth your agent
        holds, so a share made from the shell is a share your agent can already read.
      </>
    ),
    points: [
      <>
        <code>cat file | cairn</code> — pipe in, short URL on the clipboard.
      </>,
      <>
        <code>cairn add f1 f2 f3</code> — per-file progress, then one bundle URL.
      </>,
      <>
        <code>cairn ls</code> — the Bin as a TUI: <code>↑/k</code> <code>↓/j</code>{' '}
        <code>/</code> filter <code>enter</code> open <code>s</code> share <code>q</code> quit.
      </>,
    ],
    spec: 'SPEC-0008',
    shot: {
      src: '/img/shots/cli-ls.png',
      alt: 'The cairn ls terminal UI listing four artifacts in the Bin — a markdown audit, a Python script, a PNG and a live webhook endpoint — each with a type badge, a one-line description, and a provenance line reading "claude · via mcp" with an age, comment and reaction counts. A key hint bar along the bottom shows the navigation, filter, open, share and quit bindings.',
      width: 648,
      height: 434,
    },
  },
  {
    key: 'mcp',
    tab: 'MCP',
    title: 'Your agent, authorized',
    body: (
      <>
        Cairn is an MCP server with OAuth, not a bolted-on API key. Your agent consents once to
        three scopes and then reads, creates, comments and reacts <strong>as you</strong> —
        including tailing live webhook and trajectory streams.
      </>
    ),
    points: [
      <>Read artifacts you can access.</>,
      <>Create and push new artifacts.</>,
      <>Comment and react on your behalf — revocable at any time in settings.</>,
    ],
    spec: 'SPEC-0007',
    shot: {
      src: '/img/shots/mcp-consent.png',
      alt: 'The Cairn MCP authorization screen at cairn.sh/mcp/authorize: "Claude Desktop wants to connect to your Cairn workspace over MCP", the signed-in account, and a list of the three scopes being granted — read artifacts you can access, create and push new artifacts, comment and react on your behalf — above Cancel and Authorize buttons and a note that access is revocable in settings.',
      width: 566,
      height: 504,
    },
  },
];

/**
 * The no-JS stylesheet. Built from the hashed CSS-module class names so it can
 * never point at a class the bundler renamed.
 */
const NOSCRIPT_CSS = `
.${styles.tablist} { display: none !important; }
.${styles.panelHidden} { display: block !important; }
.${styles.surfacePanel} + .${styles.surfacePanel} {
  margin-top: 2.4rem;
  padding-top: 2.4rem;
  border-top: 1px solid var(--cairn-border);
}
`;

export default function SurfaceParity(): ReactNode {
  const [active, setActive] = useState(0);
  const tabRefs = useRef<(HTMLButtonElement | null)[]>([]);

  /* Arrow / Home / End move selection and focus together, which is what the
     tab pattern asks for; the roving tabindex below keeps exactly one tab in
     the page's tab order. */
  const onKeyDown = useCallback((event: KeyboardEvent<HTMLDivElement>) => {
    const keys: Record<string, (i: number) => number> = {
      ArrowRight: (i) => (i + 1) % SURFACES.length,
      ArrowLeft: (i) => (i - 1 + SURFACES.length) % SURFACES.length,
      Home: () => 0,
      End: () => SURFACES.length - 1,
    };
    const move = keys[event.key];
    if (!move) return;
    event.preventDefault();
    setActive((current) => {
      const next = move(current);
      tabRefs.current[next]?.focus();
      return next;
    });
  }, []);

  return (
    <Section id="surfaces" band>
      <SectionHead kicker="◇  SURFACE PARITY" headline="Three surfaces, one core.">
        Index, read, react, comment, share — every operation exists on all three surfaces or it
        does not ship. No surface is a second-class client.
      </SectionHead>

      {/* eslint-disable-next-line react/no-danger */}
      <noscript dangerouslySetInnerHTML={{__html: `<style>${NOSCRIPT_CSS}</style>`}} />

      <div
        className={styles.tablist}
        role="tablist"
        aria-label="Cairn surfaces"
        onKeyDown={onKeyDown}>
        {SURFACES.map((s, i) => (
          <button
            key={s.key}
            ref={(el) => {
              tabRefs.current[i] = el;
            }}
            type="button"
            role="tab"
            id={`surface-tab-${s.key}`}
            aria-controls={`surface-panel-${s.key}`}
            aria-selected={active === i}
            tabIndex={active === i ? 0 : -1}
            className={styles.tab}
            onClick={() => setActive(i)}>
            <span className={styles.tabDot} aria-hidden="true" />
            {s.tab}
          </button>
        ))}
      </div>

      {SURFACES.map((s, i) => (
        <div
          key={s.key}
          id={`surface-panel-${s.key}`}
          role="tabpanel"
          aria-labelledby={`surface-tab-${s.key}`}
          className={clsx(styles.surfacePanel, active !== i && styles.panelHidden)}>
          <div className={styles.surface}>
            <div>
              <Heading as="h3" className={styles.surfaceTitle}>
                {s.title}
              </Heading>
              <p className={styles.surfaceBody}>{s.body}</p>
              <ul className={styles.surfaceList}>
                {s.points.map((p, n) => (
                  // eslint-disable-next-line react/no-array-index-key
                  <li key={n}>
                    <span aria-hidden="true">✓</span>
                    <span>{p}</span>
                  </li>
                ))}
              </ul>
              <SpecLink id={s.spec} />
            </div>
            <Shot {...s.shot} />
          </div>
        </div>
      ))}
    </Section>
  );
}
