// Governing: ADR-0014, SPEC-0010 REQ "Derived Design-Language Page"
//
// The design-language page. ADR-0014's last decision driver is that "the site
// must demonstrate the design language, not merely describe it", so every
// element on this page is the real thing rather than a picture of it: the
// category chips are `custom.css`'s own `.chip` / `.chip-<category>` rules, and
// every swatch is painted by a `var()` reference to the token it is labelled
// with.
//
// Nothing here restates a value. The token NAMES, their RESOLVED values, the
// membership of the ADR-0009 recommended set, and which token is the neutral
// default are all read out of `src/generated/tokens.json`, which the pipeline
// derives from `src/css/custom.css` and from ADR-0009 itself. Change a token and
// rebuild: this file is not edited and the page shows the new value.
//
// No colour literal appears here, and none may: the palette lives in exactly one
// file, `src/css/custom.css`.

import React, {type CSSProperties, type ReactNode} from 'react';

import {
  categories,
  families,
  neutralDefault,
  ramps,
  tokens,
} from '@site/src/data/tokens';

import styles from './styles.module.css';

/**
 * An accent handed to a component as a custom property. The value is a `var()`
 * reference to a token name the pipeline read out of the token definition, so
 * no colour crosses this boundary — only the name of one.
 */
function accent(token: string, property: string): CSSProperties {
  return {[property]: `var(${token})`} as CSSProperties;
}

function Swatch({token, value}: {token: string; value: string}): ReactNode {
  return (
    <li className={styles.swatch}>
      <span
        className={styles.swatchColor}
        style={accent(token, '--swatch-accent')}
        aria-hidden="true"
      />
      <span className={styles.swatchMeta}>
        <span className={styles.swatchToken}>{token}</span>
        <span className={styles.swatchValue}>{value}</span>
      </span>
    </li>
  );
}

/** The surface ramps, in the order the token definition declares them. */
function Surfaces(): ReactNode {
  return (
    <section className={styles.section}>
      <h2 id="surfaces">Surfaces</h2>
      <p>
        Two ramps, both declared unconditionally so either is reachable in either
        colour mode. <strong>Ink</strong> is the dark-first ground Cairn is drawn
        for; <strong>paper</strong> is the light ramp. Every surface that renders
        a category accent as text pins the ink ramp rather than following the
        colour mode, because an accent is a dark-ground text colour.
      </p>
      {ramps.map((ramp) => (
        <div key={ramp.name}>
          <h3 id={`${ramp.name}-ramp`}>
            <span className="cairn-mono">{ramp.name}</span> ramp
          </h3>
          <ul className={styles.ramp}>
            {ramp.tokens.map((swatch) => (
              <Swatch
                key={swatch.token}
                token={swatch.token}
                value={swatch.value}
              />
            ))}
          </ul>
        </div>
      ))}
    </section>
  );
}

/**
 * The ADR-0009 span categories. The chip is `custom.css`'s own, so this section
 * cannot drift from the component the rest of the site renders — and a category
 * this page does not know about would still render, in the neutral default,
 * because that is what `.chip` falls back to.
 */
function Categories(): ReactNode {
  const recommended = categories.filter((category) => category.recommended);
  return (
    <section className={styles.section}>
      <h2 id="category-accents">Category accents</h2>
      <p>
        One accent per span category ADR-0009 recommends
        {recommended.length > 0 ? ` — ${recommended.length} of them — ` : ' '}
        plus the neutral default. They are constant across colour modes because
        they encode a <em>category</em>, not a surface, and they are fixed by
        ADR-0009: this site consumes them and never redefines them.
      </p>
      <ul className={styles.categories}>
        {categories.map((category) => (
          <li key={category.token} className={styles.category}>
            <span className={`chip chip-${category.name}`}>{category.name}</span>
            <span className={styles.categoryMeta}>
              <span className={styles.categoryToken}>{category.token}</span>
              <span>{category.value}</span>
            </span>
          </li>
        ))}
      </ul>
      {neutralDefault ? (
        <p>
          <strong>A category outside the recommended set is not a new colour.</strong>{' '}
          Any category name ADR-0009 does not define — anything the record has not
          decided on — renders in the neutral default,{' '}
          <code>{neutralDefault.token}</code> ({neutralDefault.value}). Site
          source may emit a category class for any string; an unrecognised one
          resolves to that token rather than carrying a value of its own.
        </p>
      ) : null}
    </section>
  );
}

/**
 * The type badge — the mono pill the app shell puts beside an artifact title.
 *
 * The set rendered is the accent set, and that is the honest set to render: a
 * badge's colour is a category accent, and the record does not own a
 * machine-readable list of badge codes anywhere the pipeline can read. Rendering
 * the badge across every accent shows the form and the whole palette it is
 * drawn from without hardcoding a share-type list this page would then have to
 * be kept in step with by hand.
 */
function TypeBadges(): ReactNode {
  return (
    <section className={styles.section}>
      <h2 id="type-badges">Type badges</h2>
      <p>
        A type badge is machine content: a JetBrains Mono pill on the pinned
        machine surface, tinted with its accent and outlined in the same accent
        at higher opacity, with a lit dot. It carries its own label, so the badge
        is still readable with colour removed. Every badge takes its accent from
        a category token — the full set is shown here.
      </p>
      <ul className={styles.badges}>
        {categories.map((category) => (
          <li
            key={category.token}
            className={styles.badge}
            style={accent(category.token, '--badge-accent')}
          >
            <span className={styles.badgeDot} aria-hidden="true" />
            {category.name}
          </li>
        ))}
      </ul>
    </section>
  );
}

/** The two families and the rule that decides between them. */
function Typography(): ReactNode {
  const [sans, mono] = families;
  return (
    <section className={styles.section}>
      <h2 id="type">Type</h2>
      <p>
        <strong>
          If a human wrote it, it is {sans ? sans.primary : 'the sans family'}. If a
          machine emitted it — an id, a path, a span name, a TTL, an HTTP method, a
          command, code — it is {mono ? mono.primary : 'the mono family'}. Never
          mono for a sentence.
        </strong>{' '}
        Both families are declared once, with system fallbacks, so a page stays
        legible when a font resource fails to load.
      </p>
      <ul className={styles.families}>
        {families.map((family) => {
          const isMono = family.token.endsWith('mono');
          return (
            <li key={family.token} className={styles.family}>
              <p
                className={styles.familySample}
                style={{fontFamily: `var(${family.token})`}}
              >
                {isMono
                  ? 'cairn.sh/aB3xQ7 · TRJ · 11 spans · 34.2s · ttl 7d'
                  : 'Everything above was decided in public.'}
              </p>
              <p className={styles.familyStack}>
                {family.token}: {family.stack}
              </p>
            </li>
          );
        })}
      </ul>
    </section>
  );
}

export function DesignLanguage(): ReactNode {
  return (
    <>
      <p className={styles.sourceLine}>
        Every value on this page is read from <code>{tokens.sourcePath}</code> at
        build time. Nothing here is typed by hand, and changing a token changes
        this page.
      </p>
      <Surfaces />
      <Categories />
      <TypeBadges />
      <Typography />
    </>
  );
}

export default DesignLanguage;
