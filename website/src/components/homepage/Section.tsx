// Governing: ADR-0014, SPEC-0010 REQ "Homepage"
//
// The furniture every homepage section below the hero shares: the section
// wrapper, the kicker/headline/sub header, the screenshot frame, and the link
// to the specification that owns the section.
//
// `SpecLink` is the reason this file exists rather than each section rolling
// its own heading markup. A specification's id, title and route are facts the
// record owns, so a homepage section may not type them: it names an id and the
// derived data module supplies the rest — SPEC-0010 REQ "Homepage". If the
// record renames SPEC-0003, this page renames it too, with no edit here.

import type {ReactNode} from 'react';
import clsx from 'clsx';
import Link from '@docusaurus/Link';
import useBaseUrl from '@docusaurus/useBaseUrl';
import Heading from '@theme/Heading';
import {refFor} from '@site/src/data/record';
import styles from './styles.module.css';

/**
 * ADR-0009's recommended categories plus the neutral default. A string outside
 * this union still renders: it matches no `.cat_*` rule and keeps the initial
 * `--accent: var(--cat-other)` — SPEC-0010 REQ "Category Palette Immutability".
 */
export type Cat =
  | 'reason'
  | 'exec'
  | 'read'
  | 'net'
  | 'write'
  | 'search'
  | 'plan'
  | 'tool'
  | 'analyze'
  | 'test'
  | 'fix'
  | 'fail'
  | 'meta'
  | 'other';

/** The class carrying a category's accent, or nothing for an unknown string. */
export function catClass(cat: string): string | undefined {
  return styles[`cat_${cat}`];
}

export function Section({
  id,
  band,
  children,
}: {
  id: string;
  /** Alternating ground, so adjacent sections do not run together. */
  band?: boolean;
  children: ReactNode;
}): ReactNode {
  return (
    <section id={id} className={clsx(styles.section, band && styles.band)}>
      <div className="container">{children}</div>
    </section>
  );
}

export function SectionHead({
  kicker,
  headline,
  children,
  center,
}: {
  kicker: string;
  headline: string;
  children?: ReactNode;
  center?: boolean;
}): ReactNode {
  return (
    <div className={clsx(styles.head, center && styles.headCenter)}>
      {/* The same uppercase-mono label role as the hero's eyebrow, from the one
          definition in custom.css — SPEC-0010 REQ "Typographic Roles". */}
      <p className={clsx('cairn-label', styles.kicker)}>{kicker}</p>
      <Heading as="h2" className={styles.headline}>
        {headline}
      </Heading>
      {children ? <p className={styles.sub}>{children}</p> : null}
    </div>
  );
}

/**
 * A link to the specification that governs the section. Both the title and the
 * route come from the derived data module; only the id is written here.
 */
export function SpecLink({id}: {id: string}): ReactNode {
  const ref = refFor(id);
  return (
    <Link className={styles.specLink} to={ref.href}>
      <span className={clsx('cairn-mono', styles.specId)}>{ref.id}</span>
      <span>{ref.title} →</span>
    </Link>
  );
}

/**
 * A product screenshot. Dimensions are given so the frame reserves its space
 * before the image arrives, and every shot carries prose alt text describing
 * what the reader would otherwise miss.
 */
export function Shot({
  src,
  alt,
  width,
  height,
}: {
  src: string;
  alt: string;
  width: number;
  height: number;
}): ReactNode {
  return (
    <img
      className={styles.shot}
      src={useBaseUrl(src)}
      alt={alt}
      width={width}
      height={height}
      loading="lazy"
      decoding="async"
    />
  );
}
