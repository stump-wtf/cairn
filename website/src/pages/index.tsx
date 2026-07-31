import type {ReactNode} from 'react';
import clsx from 'clsx';
import Link from '@docusaurus/Link';
import Layout from '@theme/Layout';
import Heading from '@theme/Heading';
import HomepageFeatures from '@site/src/components/HomepageFeatures';
import CoreLoop from '@site/src/components/homepage/CoreLoop';
import TypeRegistry from '@site/src/components/homepage/TypeRegistry';
import Trajectory from '@site/src/components/homepage/Trajectory';
import SurfaceParity from '@site/src/components/homepage/SurfaceParity';
import DesignRecord from '@site/src/components/homepage/DesignRecord';
import styles from './index.module.css';

const TYPE_BADGES: {label: string; cat: string}[] = [
  {label: 'MD', cat: 'read'},
  {label: 'PY', cat: 'exec'},
  {label: 'IMG', cat: 'net'},
  {label: 'FILE', cat: 'read'},
  {label: 'HK', cat: 'net'},
  {label: 'TRC', cat: 'reason'},
];

function Hero(): ReactNode {
  return (
    <header className={styles.hero}>
      <div className={styles.heroGlow} aria-hidden="true" />
      <div className={clsx('container', styles.heroInner)}>
        {/* `cairn-label` is the one definition of the uppercase-mono label role
            — SPEC-0010 REQ "Typographic Roles". The module class adds only
            colour and spacing. */}
        {/* The lozenge is ornament: it carries no meaning the label does not
            already carry, and a screen reader announcing "white diamond"
            ahead of every heading is noise — SPEC-0010 REQ "WCAG 2.1 AA &
            Semantics" ("decorative glyphs MUST be hidden"). */}
        <p className={clsx('cairn-label', styles.eyebrow)}>
          <span aria-hidden="true">◇&nbsp;</span> AI-NATIVE ARTIFACT SHARING
        </p>

        <Heading as="h1" className={styles.title}>
          Share anything.
          <br />
          <span className={styles.grad}>Human or agent.</span>
        </Heading>

        <p className={styles.subtitle}>
          <strong>Cairn</strong> is a pastebin, gist, and requestbin for the agent era. Pipe
          anything in and get a shareable, agent-native link back — carrying provenance, reactions,
          and comments. Humans post from the CLI and web; agents read, create, comment, and react
          over MCP.
        </p>

        <div className={styles.ctas}>
          <Link className="button button--primary button--lg" to="/docs/intro">
            Read the docs
          </Link>
          {/* The way INTO the product. A plain <a> with the absolute product
              URL, deliberately, twice over: /bin is an app route, not a page
              of this site, so a Docusaurus <Link to="/bin"> is a broken link
              to the build checker on every target; and this bundle also ships
              to Pages (stump-wtf.pages.stump.rocks/cairn/), where a relative
              /bin would 404 — the absolute URL lands on the app from both.
              /bin is auth-gated, so for a signed-out visitor this IS the
              sign-in flow (303 → /auth/login?next=/bin). The former
              "Decisions & specs" button this replaces survives as the navbar's
              Decisions/Specs items and the design-record section's own CTAs. */}
          <a className="button button--secondary button--lg" href="https://cairn.stump.wtf/bin">
            Open your bin
          </a>
        </div>

        <div
          className={styles.terminal}
          role="img"
          aria-label="Terminal: cat checkout-web-audit.md piped to cairn returns the short link cairn.sh/9qz1a, copied to the clipboard">
          <div className={styles.termBar} aria-hidden="true">
            <span />
            <span />
            <span />
            {/* A window title is machine content: `cairn-mono` is the role. */}
            <em className="cairn-mono">zsh — cairn</em>
          </div>
          <pre className={styles.termBody}>
            <code>
              <span className={styles.prompt}>$</span> cat checkout-web-audit.md | cairn{'\n'}
              <span className={styles.ok}>✓</span>{' '}
              <span className={styles.link}>cairn.sh/9qz1a</span>{'  '}
              <span className={styles.dim}>· md · ⧗ expires 7d · 🔒 you + anyone with link</span>
            </code>
          </pre>
          <div className={styles.badges} aria-hidden="true">
            {TYPE_BADGES.map((b) => (
              <span key={b.label} className={clsx('chip', `chip-${b.cat}`)}>
                {b.label}
              </span>
            ))}
          </div>
        </div>
      </div>
    </header>
  );
}

/*
 * Governing: ADR-0014, SPEC-0010 REQ "Homepage"
 *
 * The section order is normative, not editorial. REQ "Homepage" requires, in
 * order: the hero with its pipe-in/link-back terminal, the core loop, the
 * share-type registry, the trajectory section with its span waterfall, surface
 * parity across web/CLI/MCP, the promises, and an entry point into the design
 * record. Reordering these is a specification change.
 *
 * Every section's prose is the homepage's own. The only facts the record owns —
 * the decision, specification, requirement and scenario totals, and every
 * specification title and route behind a "SPEC-XXXX …" link — are read from the
 * derived data module, so this page cannot go stale the way the deleted
 * hand-typed docs/specifications.md did.
 */
export default function Home(): ReactNode {
  return (
    <Layout
      title="Cairn — AI-native artifact sharing"
      description="A pastebin, gist, and requestbin for the agent era. Humans post from the CLI and web; agents read, create, comment, and react over MCP.">
      {/*
        Governing: ADR-0014, SPEC-0010 REQ "Keyboard Navigation & Focus
        Management", scenario "Skipping the navigation rail", and REQ "WCAG 2.1
        AA & Semantics" (landmark regions).

        The hero is INSIDE `main`, and that is the whole point. theme-classic's
        skip link resolves its target with `document.querySelector('main')`, so
        with the hero outside it "skip to main content" jumped the h1, the
        product claim and both calls to action — it skipped the content. Being
        inside `main` also stops the hero's `header` element from registering as
        a second `banner` landmark: a `header` scoped to `main` is a section
        header, not a page banner.
      */}
      <main>
        <Hero />
        <CoreLoop />
        <TypeRegistry />
        <Trajectory />
        <SurfaceParity />
        <HomepageFeatures />
        <DesignRecord />
      </main>
    </Layout>
  );
}
