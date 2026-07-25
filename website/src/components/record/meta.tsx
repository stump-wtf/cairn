// Governing: ADR-0014, SPEC-0010 REQ "Derived Cross-Reference Graph"
// Governing: ADR-0014, SPEC-0010 REQ "Derived Status, Dates, and Counts"
//
// The metadata bar every record detail page carries under its title.
//
// It is mounted by the pipeline, not by the record text: `remark-record.ts`
// injects `<RecordMeta docId="…" />` as an AST node after the document's `# `
// heading, for the same reason the prose components are injected that way — a
// record that contained literal component markup could no longer contain a bare
// `<` or `{`. The `docId` handed in is the staged file's own path, so the bar
// cannot be pointed at the wrong record and no record id is ever typed twice.
//
// Everything rendered here is read from the derived data module: status, date,
// counts, and both directions of the cross-reference graph. Nothing is passed in
// as a prop except the identity of the page, and nothing is restated.
//
// No colour literal appears here, and none may: the palette lives in exactly one
// file, `src/css/custom.css`.

import React, {type ReactNode} from 'react';
import Link from '@docusaurus/Link';

import {
  decisions,
  refFor,
  relationsFor,
  specs,
  type DecisionEntry,
  type SpecEntry,
} from '@site/src/data/record';

import {chipGroups} from './relations';
import {StatusBadge} from './status';

/** One `<dt>`/`<dd>` pair. Grouped in a `div` so the pair survives wrapping. */
function Fact({label, children}: {label: string; children: ReactNode}): ReactNode {
  return (
    <div className="cairn-record-fact">
      <dt className="cairn-label">{label}</dt>
      <dd>{children}</dd>
    </div>
  );
}

/**
 * The record whose page this is, looked up by the doc id the pipeline derived
 * from the staged path.
 *
 * A miss is not an error: the two staged index pages and the paired design
 * documents are staged record content but are not records, and a design document
 * has no status, date or graph edges of its own to show.
 */
function findRecord(docId: string): DecisionEntry | SpecEntry | undefined {
  return (
    decisions.find((decision) => decision.docId === docId) ??
    specs.find((spec) => spec.docId === docId)
  );
}

export function RecordMeta({docId}: {docId: string}): ReactNode {
  const entry = findRecord(docId);
  if (!entry) {
    return null;
  }

  const groups = chipGroups(relationsFor(entry.id));
  const spec = 'requirementCount' in entry ? entry : null;

  return (
    <aside
      className="cairn-record-meta"
      aria-label={`${entry.id} record metadata`}>
      <dl className="cairn-record-meta__facts">
        <Fact label="Status">
          <StatusBadge status={entry.status} />
        </Fact>
        <Fact label="Date">
          {/* The machine-readable date and the rendered one are the same
              front-matter string, so the two can never disagree. */}
          <time className="cairn-record-meta__date" dateTime={entry.date}>
            {entry.date}
          </time>
        </Fact>
        {spec ? (
          <>
            <Fact label="Requirements">
              <span className="cairn-record-meta__count">
                {spec.requirementCount}
              </span>
            </Fact>
            <Fact label="Scenarios">
              <span className="cairn-record-meta__count">
                {spec.scenarioCount}
              </span>
            </Fact>
          </>
        ) : null}
      </dl>

      {groups.length > 0 ? (
        <dl className="cairn-record-meta__graph">
          {groups.map((group) => (
            <div
              key={group.kind}
              className="cairn-record-meta__group"
              data-relation={group.kind}>
              {/* The label states the direction in words. An inbound group —
                  "Extended by", "Implemented by" — is the half of the graph no
                  author writes, computed by the pipeline so the record keeps its
                  forward-only authoring convention. */}
              <dt className="cairn-label">{group.label}</dt>
              <dd>
                <ul className="cairn-record-chips">
                  {group.ids.map((id) => {
                    const ref = refFor(id);
                    return (
                      <li key={id}>
                        <Link
                          className="cairn-record-chip"
                          to={ref.href}
                          data-direction={group.direction}>
                          <span className="cairn-record-chip__id cairn-mono">
                            {ref.id}
                          </span>
                          <span className="cairn-record-chip__title">
                            {ref.title}
                          </span>
                        </Link>
                      </li>
                    );
                  })}
                </ul>
              </dd>
            </div>
          ))}
        </dl>
      ) : null}
    </aside>
  );
}

export default RecordMeta;
