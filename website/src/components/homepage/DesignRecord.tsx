// Governing: ADR-0014, SPEC-0010 REQ "Homepage"
//
// The entry point into the published design record, and the one section of the
// homepage that states facts the record owns.
//
// Not one of the four numbers below is typed here: they come from the derived
// data module, which the pipeline computes from docs/adrs/ and
// docs/openspec/specs/ at config load. Adding a decision therefore changes this
// section with no edit under src/pages/ or src/components/ — SPEC-0010 REQ
// "Homepage", scenario "Counts derive from the record". The hand-typed totals in
// the deleted website/docs/specifications.md are precisely what this section
// exists not to become.
//
// The prose around the numbers is the homepage's own, and it is written so that
// no sentence restates a count: the numbers appear once, in the stat blocks.

import type {ReactNode} from 'react';
import Link from '@docusaurus/Link';
import {counts} from '@site/src/data/record';
import {Section, SectionHead} from './Section';
import styles from './styles.module.css';

const STATS: {value: number; label: string}[] = [
  {value: counts.decisions, label: 'decisions'},
  {value: counts.specifications, label: 'capability specs'},
  {value: counts.requirements, label: 'requirements'},
  {value: counts.scenarios, label: 'scenarios'},
];

export default function DesignRecord(): ReactNode {
  return (
    <Section id="record" band>
      <SectionHead
        kicker="◇  THE DESIGN RECORD"
        headline="Everything above was decided in public."
        center>
        The decision records, the capability specifications and their RFC-2119 requirements — with
        the rejected options still in them — are published on this site in full, generated straight
        from the repository. No account, no forge, no hop off the page.
      </SectionHead>

      <div className={styles.recordGrid}>
        {STATS.map((s) => (
          <div key={s.label} className={styles.recordStat}>
            <span className={styles.recordCount}>{s.value}</span>
            <span className={styles.recordCountLabel}>{s.label}</span>
          </div>
        ))}
      </div>

      <div className={styles.recordCtas}>
        <Link className="button button--primary button--lg" to="/docs/decisions">
          Read the decisions
        </Link>
        <Link className="button button--secondary button--lg" to="/docs/specs">
          Browse the specs
        </Link>
        <Link className="button button--secondary button--lg" to="/docs/intro">
          Start with the overview
        </Link>
      </div>
    </Section>
  );
}
