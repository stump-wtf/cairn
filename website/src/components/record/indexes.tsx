// Governing: ADR-0014, SPEC-0010 REQ "Derived Data Module"
// Governing: ADR-0014, SPEC-0010 REQ "Generated Decisions Tree"
// Governing: ADR-0014, SPEC-0010 REQ "Generated Specs Tree"
//
// The two generated indexes, and the status badge they share.
//
// These live in their own module rather than beside the record prose components,
// and the reason is bundling, not tidiness. The prose components are imported by
// every one of the staged record pages, so webpack hoists their module into the
// site-wide entry chunk. Anything in that module that reaches
// `@site/src/data/record` therefore drags the whole 67 KB derived data module
// into `main.js`, where the homepage — which renders no record data at all —
// pays for it. Only these two components need the data, and only two staged
// `index.mdx` pages import them, so keeping them separate confines the payload
// to the routes that actually read it.
//
// No colour literal appears here, and none may: the palette lives in exactly one
// file, `src/css/custom.css`.

import React, {type ReactNode} from 'react';
import Link from '@docusaurus/Link';

import {decisions, specs} from '@site/src/data/record';

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
          {/*
            Governing: ADR-0014, SPEC-0010 REQ "WCAG 2.1 AA & Semantics",
            scenario "Generated page heading order".

            `h2`, not `h3`. The staged `docs/specs/index.mdx` carries a single
            `# Specifications` and no other heading, so a card titled `h3` put
            an h1 → h3 jump on a shipped page. How large the title *looks* is a
            styling question and is answered from the stylesheet; the tag is the
            document outline and is not free to follow it.
          */}
          <h2>
            <Link to={spec.href}>
              <code>{spec.id}</code> {spec.title}
            </Link>
          </h2>
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
