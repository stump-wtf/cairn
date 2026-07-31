// Governing: ADR-0014, SPEC-0010 REQ "Homepage"
//
// The flagship share type, and the span waterfall that argues for it.
//
// The waterfall runs on STATIC ILLUSTRATIVE DATA — design.md's component
// inventory says so ("Waterfall (homepage only, static data)"). What is *not*
// hand-written is any number derived from that data: the run duration, the span
// count, the tool-call count, the axis ticks, the bar geometry and the
// time-by-category breakdown are all computed from SPANS below. A homepage that
// typed "34.2s" beside a waterfall drawn from different numbers would be the
// same class of defect as a hand-counted requirement total, one layer down.
//
// The panel pins the ink ramp in both colour modes because it renders five
// category accents as text and as bars — SPEC-0010 REQ "Category Accents Are
// Dark-Ground Text Colours".

import type {ReactNode} from 'react';
import clsx from 'clsx';
import {Section, SectionHead, SpecLink, catClass, type Cat} from './Section';
import styles from './styles.module.css';

type SpanSource = {
  name: string;
  cat: Cat;
  /** Leaf duration in seconds. A parent's duration is the sum of its children. */
  seconds?: number;
  children?: SpanSource[];
  /** Leaf spans that are tool invocations rather than model turns. */
  tool?: boolean;
};

const SPANS: SpanSource[] = [
  {name: 'plan the audit', cat: 'reason', seconds: 2.2},
  {name: 'bash npm ls --all', cat: 'exec', seconds: 2.9, tool: true},
  {name: 'read package.json', cat: 'read', seconds: 0.9, tool: true},
  {name: 'rank by severity', cat: 'reason', seconds: 1.8},
  {
    name: 'advisory lookup',
    cat: 'net',
    children: [
      {name: 'web_search CVE legacy-jwt', cat: 'net', seconds: 3.4, tool: true},
      {name: 'web_fetch nvd.nist.gov', cat: 'net', seconds: 4.1, tool: true},
      {name: 'summarize risk', cat: 'reason', seconds: 2.1},
    ],
  },
  {name: 'read yarn.lock', cat: 'read', seconds: 2.5, tool: true},
  {name: 'compose findings', cat: 'reason', seconds: 6.0},
  {name: 'write checkout-web-audit.md', cat: 'write', seconds: 2.7, tool: true},
  {name: 'final summary', cat: 'reason', seconds: 3.7},
];

type LaidOutSpan = {
  name: string;
  cat: Cat;
  start: number;
  seconds: number;
  depth: number;
  isGroup: boolean;
  tool: boolean;
};

function durationOf(span: SpanSource): number {
  return span.children ? span.children.reduce((n, c) => n + durationOf(c), 0) : (span.seconds ?? 0);
}

/** Sequential layout: siblings run back to back, children inside their parent. */
function layOut(spans: SpanSource[], start: number, depth: number, out: LaidOutSpan[]): number {
  let cursor = start;
  for (const span of spans) {
    const seconds = durationOf(span);
    out.push({
      name: span.name,
      cat: span.cat,
      start: cursor,
      seconds,
      depth,
      isGroup: Boolean(span.children),
      tool: Boolean(span.tool),
    });
    if (span.children) layOut(span.children, cursor, depth + 1, out);
    cursor += seconds;
  }
  return cursor;
}

const ROWS: LaidOutSpan[] = [];
const TOTAL = layOut(SPANS, 0, 0, ROWS);
const LEAVES = ROWS.filter((r) => !r.isGroup);
const TOOL_CALLS = LEAVES.filter((r) => r.tool).length;

/** Seconds per category, over leaves only, heaviest first. */
const BY_CATEGORY: {cat: Cat; seconds: number}[] = Object.entries(
  LEAVES.reduce<Record<string, number>>((acc, r) => {
    acc[r.cat] = (acc[r.cat] ?? 0) + r.seconds;
    return acc;
  }, {}),
)
  .map(([cat, seconds]) => ({cat: cat as Cat, seconds}))
  .sort((a, b) => b.seconds - a.seconds);

const TICKS = [0, 0.25, 0.5, 0.75, 1].map((f) => f * TOTAL);

const secs = (n: number) => `${n.toFixed(1)}s`;
const pct = (n: number) => `${((n / TOTAL) * 100).toFixed(3)}%`;

function Waterfall(): ReactNode {
  return (
    <div className={styles.panel}>
      <div className={styles.panelBar}>
        <span className="chip chip-reason">TRC</span>
        <span className={styles.panelTitle}>checkout-web-audit</span>
        <span>
          · trace · {ROWS.length} spans · {secs(TOTAL)}
        </span>
        <span className={styles.panelLink}>cairn.sh/run/8kd2p</span>
      </div>

      <p className={styles.panelLabel}>Call trace</p>

      {/* The legend names every category the bars paint, in text, so the chart
          is readable with colour removed — SPEC-0010 REQ "Non-Colour Encoding". */}
      <div className={styles.legend}>
        {BY_CATEGORY.map(({cat}) => (
          <span key={cat} className={clsx('chip', `chip-${cat}`)}>
            {cat}
          </span>
        ))}
      </div>

      <ol className={styles.waterfall}>
        {ROWS.map((row) => (
          <li key={`${row.name}-${row.start}`} className={clsx(styles.span, catClass(row.cat))}>
            <span className={clsx(styles.spanName, row.depth > 0 && styles.spanChild)}>
              {row.name}
              <span className={styles.spanCat}>{row.cat}</span>
            </span>
            <span className={styles.spanTrack}>
              <span
                className={styles.spanBar}
                style={{left: pct(row.start), width: pct(row.seconds)}}
                aria-hidden="true"
              />
            </span>
            <span className={styles.spanDur}>{secs(row.seconds)}</span>
          </li>
        ))}
      </ol>

      <div className={styles.axis} aria-hidden="true">
        {TICKS.map((t) => (
          <span key={t}>{secs(t)}</span>
        ))}
      </div>
    </div>
  );
}

function RunPanel(): ReactNode {
  return (
    <div className={styles.panel}>
      <p className={styles.panelLabel}>Run provenance</p>
      <p className={clsx('cairn-mono', styles.panelText)}>
        claude · sonnet-4.6
        <br />
        via MCP · captured 2h ago · expires in 7d
      </p>

      <p className={styles.panelLabel}>Run stats</p>
      <div className={styles.stats}>
        <div className={styles.stat}>
          <span className={styles.statValue}>{secs(TOTAL)}</span>
          <span className={styles.statLabel}>total</span>
        </div>
        <div className={styles.stat}>
          <span className={styles.statValue}>{ROWS.length}</span>
          <span className={styles.statLabel}>spans</span>
        </div>
        <div className={styles.stat}>
          <span className={styles.statValue}>{TOOL_CALLS}</span>
          <span className={styles.statLabel}>tool calls</span>
        </div>
        <div className={styles.stat}>
          <span className={styles.statValue}>{BY_CATEGORY.length}</span>
          <span className={styles.statLabel}>categories</span>
        </div>
      </div>

      <p className={styles.panelLabel}>Time by category</p>
      <ul className={styles.byCat}>
        {BY_CATEGORY.map(({cat, seconds}) => (
          <li key={cat} className={clsx(styles.byCatRow, catClass(cat))}>
            <span className={styles.byCatName}>{cat}</span>
            <span className={styles.spanTrack}>
              <span className={styles.spanBar} style={{left: 0, width: pct(seconds)}} aria-hidden="true" />
            </span>
            <span className={styles.byCatValue}>{secs(seconds)}</span>
          </li>
        ))}
      </ul>
    </div>
  );
}

export default function Trajectory(): ReactNode {
  return (
    <Section id="trajectory">
      <SectionHead kicker="◇  FLAGSHIP · TRC" headline="A whole agent run, shared.">
        Your agent captures its run as OTel spans and drops it as a trace. The waterfall shows
        where the time actually went — reasoning, exec, reads, network, writes — and every{' '}
        <strong>write</strong> span links to the artifact it produced. A run is not a log; it is a
        shareable object with reactions and comments on any span.
      </SectionHead>

      <div className={styles.trajectory}>
        <Waterfall />
        <RunPanel />
      </div>

      <SpecLink id="SPEC-0004" />
    </Section>
  );
}
