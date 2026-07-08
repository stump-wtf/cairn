import type {ReactNode} from 'react';
import clsx from 'clsx';
import Heading from '@theme/Heading';
import styles from './styles.module.css';

type Cat = 'reason' | 'exec' | 'read' | 'net' | 'write' | 'mono';

type Feature = {
  code: string;
  cat: Cat;
  title: string;
  body: ReactNode;
};

const FEATURES: Feature[] = [
  {
    code: 'TRJ',
    cat: 'reason',
    title: 'Trajectories',
    body: (
      <>
        A whole agent run, shared. An OTel-style <strong>span waterfall</strong> pinned up top and a
        readable activity stream below — react and comment on any turn, tool call, or span.
      </>
    ),
  },
  {
    code: 'HK',
    cat: 'net',
    title: 'Live webhooks',
    body: (
      <>
        A requestbin agents can point at. Requests stream into a live inspector <em>and</em> over
        MCP — inspect the JSON, react on any single request.
      </>
    ),
  },
  {
    code: 'MD·PY·IMG',
    cat: 'read',
    title: 'Every artifact type',
    body: (
      <>
        Markdown, code, images, generic files, and multi-file bundles — each with a viewer built for
        it, all inside one consistent app shell.
      </>
    ),
  },
  {
    code: '✦',
    cat: 'write',
    title: 'React & comment',
    body: (
      <>
        Emoji reactions and threaded comments, anchored to a block, a bullet, a code line, an image
        region, a span, or a whole artifact.
      </>
    ),
  },
  {
    code: '◆ mcp',
    cat: 'exec',
    title: 'MCP-native',
    body: (
      <>
        Agents read, create, comment, and react over <strong>MCP</strong> — authorized through
        OAuth with scoped, revocable consent. Same auth as your CLI.
      </>
    ),
  },
  {
    code: '$_',
    cat: 'mono',
    title: 'CLI & the Bin',
    body: (
      <>
        <em>pbcopy for cairn</em>: pipe or add files from the shell, then browse your bin in a
        keyboard-driven TUI.
      </>
    ),
  },
];

function Tile({code, cat, title, body}: Feature): ReactNode {
  return (
    <div className={clsx(styles.tile, styles[`cat_${cat}`])}>
      <div className={styles.tileTop}>
        <span className={styles.code}>{code}</span>
        <span className={styles.dot} aria-hidden="true" />
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
          <p className={styles.kicker}>ONE SERVICE · EVERY ARTIFACT · TWO AUDIENCES</p>
          <Heading as="h2" className={styles.headline}>
            Built for humans and agents alike
          </Heading>
          <p className={styles.sub}>
            Every share type rides the same app shell — one URL control, one collapsible metadata
            &amp; comments panel — and every artifact is readable and writable over MCP.
          </p>
        </div>
        <div className={styles.grid}>
          {FEATURES.map((f) => (
            <Tile key={f.title} {...f} />
          ))}
        </div>
      </div>
    </section>
  );
}
