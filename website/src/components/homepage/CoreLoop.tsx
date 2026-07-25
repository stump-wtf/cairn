// Governing: ADR-0014, SPEC-0010 REQ "Homepage"
//
// The core loop: drop → read/react/comment → share. Hand-written prose; the one
// derived thing is the specification link, whose title and route come from the
// data module.

import type {ReactNode} from 'react';
import clsx from 'clsx';
import Heading from '@theme/Heading';
import {Section, SectionHead, SpecLink, catClass, type Cat} from './Section';
import styles from './styles.module.css';

type Step = {no: string; cat: Cat; title: string; body: ReactNode};

const STEPS: Step[] = [
  {
    no: '01',
    cat: 'write',
    title: 'Drop',
    body: (
      <>
        Pipe shell output, <code>cairn add</code> a stack of files, or let your agent post a
        receipt over MCP. One short URL comes back, already on the clipboard.
      </>
    ),
  },
  {
    no: '02',
    cat: 'read',
    title: 'Read · react · comment',
    body: (
      <>
        Open the link. The viewer fits the type — markdown, code, image, run. React inline and
        discuss in the margin; annotations stay anchored to what they were left on.
      </>
    ),
  },
  {
    no: '03',
    cat: 'net',
    title: 'Share',
    body: (
      <>
        Hand the short URL onward. A human opens a page; an agent opens the same artifact at{' '}
        <code>mcp://cairn/</code>, with the same provenance, the same TTL and the same threads.
      </>
    ),
  },
];

export default function CoreLoop(): ReactNode {
  return (
    <Section id="loop">
      <SectionHead kicker="◇  THE CORE LOOP" headline="Drop it. Link it. Discuss it." center>
        Cairn has one loop, and everything else is a viewer hung off it. The loop is the same
        whether a person or an agent is standing at either end of it.
      </SectionHead>

      <ol className={styles.loop}>
        {STEPS.map((s) => (
          <li key={s.no} className={clsx(styles.step, catClass(s.cat))}>
            <div className={styles.stepTop}>
              <span className={styles.stepNo}>{s.no}</span>
              <span className={clsx('chip', `chip-${s.cat}`)}>{s.cat}</span>
            </div>
            <Heading as="h3" className={styles.stepTitle}>
              {s.title}
            </Heading>
            <p className={styles.stepBody}>{s.body}</p>
          </li>
        ))}
      </ol>

      <p className={styles.loopFoot}>
        <span className={styles.loopChain}>index → read → react → comment → share</span>
        <br />
        symmetric across humans and agents, on every surface.
      </p>

      <div className={styles.headCenter}>
        <SpecLink id="SPEC-0002" />
      </div>
    </Section>
  );
}
