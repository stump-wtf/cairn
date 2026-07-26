// Governing: ADR-0014, SPEC-0010 REQ "Homepage"
//
// The product promises — the four things true of every artifact, whatever its
// type and whichever surface made it.
//
// WHY THIS FILE IS STILL CALLED HomepageFeatures. Before SPEC-0010 this was a
// nine-tile grid that argued the whole product at once; the sections this story
// adds now make seven of those nine arguments in their own right, and repeating
// them here would have been the homepage disagreeing with itself. What survives
// is the four promises, in the tile visual language that was already here.
//
// The DIRECTORY NAME is load-bearing and is deliberately not being tidied up:
// scripts/check-tokens.mjs names `src/components/HomepageFeatures/styles.module.css`
// as one of the two category-derived class sets whose coverage it verifies
// against ADR-0009. Renaming the directory would make that check silently skip
// (it is guarded by `existsSync`) rather than fail, which is the worst of the
// available outcomes.

import type {ReactNode} from 'react';
import clsx from 'clsx';
import Heading from '@theme/Heading';
import styles from './styles.module.css';

type Cat = 'reason' | 'exec' | 'read' | 'net' | 'write' | 'search' | 'plan' | 'tool' | 'analyze' | 'test' | 'fix' | 'fail' | 'meta' | 'mono' | 'other';

type Guarantee = {
  code: string;
  cat: Cat;
  title: string;
  body: ReactNode;
};

const PROMISES: Guarantee[] = [
  {
    code: '◈',
    cat: 'read',
    title: 'Who made this, and how',
    body: (
      <>
        Every artifact records its origin — human or model, through which channel, and when it was
        captured: <em>claude · sonnet-4.6 · via MCP · captured 2h ago</em>.
      </>
    ),
  },
  {
    code: '⧗',
    cat: 'net',
    title: 'Ephemeral by default',
    body: (
      <>
        A visible TTL on every share — <em>expires in 5d</em> — and a hard delete when it runs out.
        Nothing lingers because someone forgot it existed.
      </>
    ),
  },
  {
    code: '🔒',
    cat: 'write',
    title: 'Link-based access',
    body: (
      <>
        Capability URLs, with the policy on screen rather than buried in a menu:{' '}
        <em>you + anyone with link</em>.
      </>
    ),
  },
  {
    code: '✦',
    cat: 'reason',
    title: 'One annotation layer',
    body: (
      <>
        Reactions and threads with type-specific anchors — a block, a code line, an image region, a
        captured request, a span — and the same layer over MCP.
      </>
    ),
  },
];

function Tile({code, cat, title, body}: Guarantee): ReactNode {
  return (
    <div className={clsx(styles.tile, styles[`cat_${cat}`])}>
      {/*
        Governing: ADR-0014, SPEC-0010 REQ "WCAG 2.1 AA & Semantics" —
        "decorative glyphs MUST be hidden from assistive technology".

        The whole badge is hidden, and it is worth being exact about why,
        because five of the nine codes are glyphs and four are not. `✦`, `▤`,
        `◈`, `⧗` and `$_` announce as "sparkle", "black rectangle", "dollar
        underscore" and so on — noise under any reading.

        `TRJ`, `HK`, `MD·PY·IMG` and `◆ mcp` are text, and they are hidden on
        the other ground the requirement gives: they are not informative. Each
        is a compression of the tile it sits on — `TRJ` of "Trajectories", `HK`
        of "Live webhooks", `◆ mcp` of "MCP-native", `MD·PY·IMG` of a body that
        already spells out "Markdown, code, images, generic files, and
        multi-file bundles". Announced, they would read every tile's subject
        twice, the second time in initials.

        So no badge carries meaning its heading and body do not, which is also
        why the pair is hidden rather than labelled: an alt text here would be
        a second copy of the title, free to drift from the first.
      */}
      <div className={styles.tileTop} aria-hidden="true">
        <span className={styles.code}>{code}</span>
        <span className={styles.dot} />
      </div>
      <Heading as="h3" className={styles.tileTitle}>
        {title}
      </Heading>
      <p className={styles.tileBody}>{body}</p>
    </div>
  );
}

export default function HomepageFeatures(): ReactNode {
  return (
    <section className={styles.features}>
      <div className="container">
        <div className={styles.head}>
          {/* The same label role as the hero's eyebrow, from one definition —
              SPEC-0010 REQ "Typographic Roles". */}
          <p className={clsx('cairn-label', styles.kicker)}>
            ◇&nbsp; THE PROMISES · TRUE OF EVERY ARTIFACT
          </p>
          <Heading as="h2" className={styles.headline}>
            The same four guarantees, whatever you dropped
          </Heading>
          <p className={styles.sub}>
            Provenance, a visible expiry, an access policy you can read, and one annotation layer —
            on every share type and on every surface.
          </p>
        </div>
        <div className={styles.grid}>
          {PROMISES.map((p) => (
            <Tile key={p.title} {...p} />
          ))}
        </div>
      </div>
    </section>
  );
}
