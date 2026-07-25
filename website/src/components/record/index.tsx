// Governing: ADR-0014, SPEC-0010 REQ "Record Prose to Structured Components"
//
// The components a transformed record document renders through. The remark
// transform injects one ESM import of this module into every staged record page —
// see `plugins/record/remark-record.ts` for why the import cannot be written into
// the record text.
//
// Because that import lands in every record page, this module is in the site-wide
// entry chunk, so nothing here may reach `@site/src/data/record`: that would ship
// the whole derived data module to every route including the homepage. The two
// generated indexes, which genuinely need the data, live in `./indexes` and are
// imported only by the two staged index pages.
//
// These are deliberately structure-only: semantic elements plus stable
// `cairn-record-*` class hooks, no stylesheet of their own. The visual design of
// the record pages and the generated indexes is a separate piece of work; this
// file exists so that the pipeline has something real to bind to, and so that
// styling it later is a CSS change rather than a rewrite.
//
// No colour literal appears here, and none may: the palette lives in exactly one
// file, `src/css/custom.css`.

import React, {type ReactNode} from 'react';

/**
 * A `### Requirement:` section. The section's own heading is the first child, so
 * its anchor and its table-of-contents entry are the ones Docusaurus generated.
 */
export function RequirementBlock({
  name,
  children,
}: {
  name?: string;
  children?: ReactNode;
}): ReactNode {
  return (
    <section className="cairn-record-requirement" data-requirement={name}>
      {children}
    </section>
  );
}

/** A `#### Scenario:` section. */
export function ScenarioBlock({
  name,
  children,
}: {
  name?: string;
  children?: ReactNode;
}): ReactNode {
  return (
    <section className="cairn-record-scenario" data-scenario={name}>
      {children}
    </section>
  );
}

/** The WHEN/THEN pair of one scenario. */
export function ScenarioSteps({children}: {children?: ReactNode}): ReactNode {
  return <div className="cairn-record-scenario-steps">{children}</div>;
}

/**
 * One `- **WHEN** …` / `- **THEN** …` step. The keyword is rendered as text so
 * the step is identifiable with colour removed.
 */
export function ScenarioStep({
  step,
  children,
}: {
  step?: string;
  children?: ReactNode;
}): ReactNode {
  return (
    <div className="cairn-record-scenario-step" data-step={step}>
      <span className="cairn-record-scenario-step__label">{step}</span>
      <div className="cairn-record-scenario-step__body">{children}</div>
    </div>
  );
}

/**
 * An RFC 2119 keyword. `<strong>` rather than a colour: the emphasis has to
 * survive colour being removed, and the keyword is normative text, not decoration.
 */
export function Rfc2119({
  keyword,
  children,
}: {
  keyword?: string;
  children?: ReactNode;
}): ReactNode {
  return (
    <strong className="cairn-record-rfc2119" data-keyword={keyword}>
      {children}
    </strong>
  );
}
