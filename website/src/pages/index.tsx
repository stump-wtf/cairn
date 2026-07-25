import type {ReactNode} from 'react';
import clsx from 'clsx';
import Link from '@docusaurus/Link';
import Layout from '@theme/Layout';
import Heading from '@theme/Heading';
import HomepageFeatures from '@site/src/components/HomepageFeatures';
import styles from './index.module.css';

const TYPE_BADGES: {label: string; cat: string}[] = [
  {label: 'MD', cat: 'read'},
  {label: 'PY', cat: 'exec'},
  {label: 'IMG', cat: 'net'},
  {label: 'FILE', cat: 'read'},
  {label: 'HK', cat: 'net'},
  {label: 'TRJ', cat: 'reason'},
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
          {/* Governing: ADR-0014, SPEC-0010 REQ "No Repository Links" — the
              hand-maintained /docs/architecture index is deleted, so this points
              at the generated decisions tree; and there is no forge link to
              follow, because the full record renders on the site. */}
          <Link className="button button--secondary button--lg" to="/docs/decisions">
            Decisions &amp; specs
          </Link>
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
        <HomepageFeatures />
      </main>
    </Layout>
  );
}
