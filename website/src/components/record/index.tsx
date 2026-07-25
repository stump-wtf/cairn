// Governing: ADR-0014, SPEC-0010 REQ "Record Prose to Structured Components"
// Governing: ADR-0014, SPEC-0010 REQ "Derived Data Module"
//
// The components a transformed record document and a generated index render
// through. The remark transform injects one ESM import of this module into every
// staged record page — see `plugins/record/remark-record.ts` for why the import
// cannot be written into the record text.
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
import Link from '@docusaurus/Link';

import {decisions, specs} from '@site/src/data/record';

// ---------------------------------------------------------------------------
// Record prose components (bound by the remark transform)
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// Generated indexes (bound by the staged index pages)
// ---------------------------------------------------------------------------

/** A status, carried as text. Colour is never the only carrier of meaning. */
export function StatusBadge({status}: {status: string}): ReactNode {
  return (
    <span className="cairn-record-status" data-status={status}>
      {status.toUpperCase()}
    </span>
  );
}

/**
 * One row per `docs/adrs/ADR-*.md`, in number order, with every cell derived.
 * Nothing here is hand-maintained, which is the entire point of ADR-0014.
 */
export function DecisionIndex(): ReactNode {
  return (
    <table className="cairn-record-index">
      <thead>
        <tr>
          <th scope="col">Decision</th>
          <th scope="col">Status</th>
          <th scope="col">Date</th>
        </tr>
      </thead>
      <tbody>
        {decisions.map((decision) => (
          <tr key={decision.id}>
            <th scope="row">
              <Link to={decision.href}>
                <code>{decision.id}</code> {decision.title}
              </Link>
            </th>
            <td>
              <StatusBadge status={decision.status} />
            </td>
            <td>
              <time dateTime={decision.date}>{decision.date}</time>
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

/**
 * One card per capability directory, ordered by the number in the
 * specification's own `# SPEC-XXXX:` heading — not by directory name, which does
 * not sort into specification order.
 */
export function SpecIndex(): ReactNode {
  return (
    <ul className="cairn-record-cards">
      {specs.map((spec) => (
        <li key={spec.id} className="cairn-record-card">
          <h3>
            <Link to={spec.href}>
              <code>{spec.id}</code> {spec.title}
            </Link>
          </h3>
          <p>{spec.summary}</p>
          <p className="cairn-record-card__meta">
            <StatusBadge status={spec.status} />{' '}
            <span>
              {spec.requirementCount} requirements · {spec.scenarioCount}{' '}
              scenarios
            </span>{' '}
            {spec.designHref ? (
              <Link to={spec.designHref}>Design document</Link>
            ) : null}
          </p>
        </li>
      ))}
    </ul>
  );
}
