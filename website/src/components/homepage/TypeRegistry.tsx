// Governing: ADR-0014, SPEC-0010 REQ "Homepage"
//
// The share-type registry: one card per type Cairn serves. The prose is the
// homepage's own; the specification links are derived.
//
// The point the section is making is that adding a type means registering a
// viewer rather than building an app, so every card is deliberately the same
// shape — badge, name, prose, and the artifact as it actually renders.

import type {ReactNode} from 'react';
import clsx from 'clsx';
import Heading from '@theme/Heading';
import {Section, SectionHead, Shot, SpecLink, catClass, type Cat} from './Section';
import styles from './styles.module.css';

type ShareType = {
  /** The type badge as the app shell renders it. */
  code: string;
  cat: Cat;
  name: string;
  body: ReactNode;
  shot?: {src: string; alt: string; width: number; height: number};
  extra?: ReactNode;
  wide?: boolean;
};

const TYPES: ShareType[] = [
  {
    code: 'MD',
    cat: 'read',
    name: 'Markdown',
    body: (
      <>
        Rendered markdown with a live table of contents. React under any block; select text and a
        margin comment lands beside it.
      </>
    ),
    shot: {
      src: '/img/shots/viewer-md-react.png',
      alt: 'The markdown viewer showing a rendered dependency-audit document. Emoji reaction counts sit under the heading and under individual findings, two threaded comments from Sam and Rae float in the margin beside the text they refer to, and a right-hand rail lists the provenance (Claude, via MCP, pushed 2 hours ago), the short link, the expiry, and a contents outline.',
      width: 916,
      height: 504,
    },
  },
  {
    code: 'PY',
    cat: 'exec',
    name: 'Code',
    body: (
      <>
        Highlighted source with line numbers and a symbol outline. Comment a single line or an
        arbitrary selection.
      </>
    ),
    shot: {
      src: '/img/shots/viewer-code.png',
      alt: 'The code viewer showing a twelve-line Python file with line numbers and syntax highlighting. A comment from Rae is anchored to line 10 and the highlighted expression on that line is underlined; the right-hand rail lists provenance, the symbol outline, and file facts — Python, 12 lines, 1.1 KB, expires in 29 days.',
      width: 916,
      height: 480,
    },
  },
  {
    code: 'IMG',
    cat: 'net',
    name: 'Image',
    body: (
      <>
        Drop pins to anchor a comment to a region of the image. Reactions live below the frame,
        dimensions and format beside it.
      </>
    ),
    shot: {
      src: '/img/shots/viewer-image.png',
      alt: 'The image viewer showing a 1600 by 900 PNG with two numbered pins dropped onto different regions of the picture, an "add pin" control above it, and a right-hand rail listing provenance, format, file size, and the pin and reaction counts.',
      width: 916,
      height: 464,
    },
  },
  {
    code: 'GZ',
    cat: 'meta',
    name: 'File',
    body: (
      <>
        Anything unpreviewable: size, format, a <code>sha256</code> checksum and a download. Still
        reactable, still discussable.
      </>
    ),
    shot: {
      src: '/img/shots/viewer-file.png',
      alt: 'The generic file viewer for a 42.7 MB gzipped SQL dump: a type badge, the note "not previewable", a full-width download button, a copyable sha256 checksum, a one-line summary of what the archive contains, a reaction count, and a comment from Sam asking whether the dump is anonymised.',
      width: 444,
      height: 426,
    },
  },
  {
    code: 'HK',
    cat: 'net',
    name: 'Webhook',
    wide: true,
    body: (
      <>
        A requestbin that stays open. Requests stream into the inspector over SSE with the JSON
        highlighted and reactions per request — and agents tail the same stream at{' '}
        <code>mcp://cairn/hook/&lt;id&gt;</code>.
      </>
    ),
    extra: (
      <div className={styles.hookStream}>
        <div>
          <span className={styles.hookMethod}>POST</span> <span className={styles.hookPath}>/hook/3v8qd</span> · 200 · 1.2 KB · 12:04:18
        </div>
        <div>
          {'{ '}
          <span className={styles.hookKey}>&quot;type&quot;</span>:{' '}
          <span className={styles.hookVal}>&quot;payment_intent.succeeded&quot;</span>,{' '}
          <span className={styles.hookKey}>&quot;amount&quot;</span>:{' '}
          <span className={styles.hookVal}>4200</span>
          {' }'}
        </div>
        <div>
          <span className={styles.hookMethod}>POST</span> <span className={styles.hookPath}>/hook/3v8qd</span> · 200 · 0.9 KB · 12:03:51
        </div>
      </div>
    ),
  },
  {
    code: 'RUN',
    cat: 'reason',
    name: 'Run',
    wide: true,
    body: (
      <>
        The flagship type: a whole agent run, shared. An OTel-style span waterfall over the
        activity stream that produced the artifacts — and every <code>write</code> span links to
        what it wrote. <strong>The waterfall is the next section.</strong>
      </>
    ),
  },
  {
    code: '4 FILES',
    cat: 'plan',
    name: 'Bundle',
    body: (
      <>
        Mixed media under one URL. Humans switch files in the rail; agents read the same tree over
        MCP. Comments stay pinned to the file they were left on.
      </>
    ),
    shot: {
      src: '/img/shots/web-bundle.png',
      alt: 'The bundle viewer: a left rail listing four files — a markdown document, a Python file, a PNG and a gzipped dump, each with its own comment or pin count — the selected markdown file rendered in the middle, and a right-hand rail carrying provenance, the expiry, and comments that name the file each was left on.',
      width: 924,
      height: 504,
    },
  },
];

export default function TypeRegistry(): ReactNode {
  return (
    <Section id="types" band>
      <SectionHead kicker="◇  TYPE REGISTRY" headline="One shell. A viewer per type.">
        Same header, same metadata rail, same annotation layer — the body is whatever the artifact
        needs. Adding a type means registering a viewer, not building an app.
      </SectionHead>

      <div className={styles.registry}>
        {TYPES.map((t) => (
          <article
            key={t.name}
            className={clsx(styles.typeCard, catClass(t.cat), t.wide && styles.typeCardWide)}>
            <div className={styles.typeTop}>
              {/* The type badge is machine content, tinted by its category. */}
              <span className={clsx('chip', `chip-${t.cat}`)}>{t.code}</span>
              <Heading as="h3" className={styles.typeName}>
                {t.name}
              </Heading>
              {t.code === 'HK' ? (
                // The state is spelled out, not left to the dot's colour —
                // SPEC-0010 REQ "Non-Colour Encoding".
                <span className={clsx('chip', 'chip-exec', styles.live)}>
                  <span aria-hidden="true">●</span> live
                </span>
              ) : null}
            </div>
            <p className={styles.typeBody}>{t.body}</p>
            {t.shot ? <Shot {...t.shot} /> : null}
            {t.extra ?? null}
          </article>
        ))}
      </div>

      <div className={styles.headCenter}>
        <SpecLink id="SPEC-0003" />
      </div>
    </Section>
  );
}
