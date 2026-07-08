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
        <p className={styles.eyebrow}>◇&nbsp; AI-NATIVE ARTIFACT SHARING</p>

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
          <Link className="button button--secondary button--lg" to="/docs/architecture">
            Architecture &amp; specs
          </Link>
          <Link
            className={clsx('button button--lg', styles.ghost)}
            href="https://github.com/joestump/cairn">
            GitHub&nbsp;↗
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
            <em>zsh — cairn</em>
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
      <Hero />
      <main>
        <HomepageFeatures />
      </main>
    </Layout>
  );
}
