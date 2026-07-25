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

import {StatusBadge} from './status';

/**
 * Re-exported from `./status`, where the badge moved so that the metadata bar
 * can render one without pulling the whole decision table onto every record
 * page. Existing importers keep working.
 */
export {StatusBadge};

/**
 * One row per `docs/adrs/ADR-*.md`, in number order, with every cell derived.
 * Nothing here is hand-maintained, which is the entire point of ADR-0014.
 */
export function DecisionIndex(): ReactNode {
  return (
    <table className="cairn-record-index">
      {/* The total is read from the derived data module, never written down —
          REQ "Derived Status, Dates, and Counts" — and doubles as the table's
          accessible description. */}
      <caption>{decisions.length} decision records, in number order</caption>
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
