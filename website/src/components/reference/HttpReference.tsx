// Governing: ADR-0014, SPEC-0010 REQ "Derived HTTP Reference Page"
//
// The HTTP reference. Every row here was read out of a specification's own
// endpoint table at build time; ADR-0014 lists "the API reference surface" among
// the things derived from "the specs' own endpoint tables" precisely so that a
// hand-kept second copy of the API can never drift from the first.
//
// Two things this page is careful about, both because SPEC-0010 requires them.
//
// It links every row to the table it came from, not merely to the specification,
// so a reader who wants the justification behind an auth posture lands on the
// paragraph that gave it.
//
// It does not claim to be complete, and it says which specifications it is
// missing. Four of ten carry none of the enumerated section names, so a reader
// who took this page for the whole API would be wrong — and the list of silent
// specifications is derived too, which is the only way it can stay true.
//
// No colour literal appears here, and none may: the palette lives in exactly one
// file, `src/css/custom.css`.

import React, {type ReactNode} from 'react';
import Link from '@docusaurus/Link';

import {endpoints, type EndpointRow} from '@site/src/data/record';

import styles from './styles.module.css';

/** English list, so the coverage note reads as a sentence rather than an array. */
function joinNames(items: ReactNode[]): ReactNode[] {
  return items.flatMap((item, index) => {
    if (index === 0) {
      return [item];
    }
    const separator = index === items.length - 1 ? ' and ' : ', ';
    return [separator, item];
  });
}

function EndpointTable({rows}: {rows: EndpointRow[]}): ReactNode {
  return (
    <div className={styles.tableScroll}>
      <table className={styles.endpoints}>
        <thead>
          <tr>
            <th scope="col">Method</th>
            <th scope="col">Path</th>
            <th scope="col">Purpose</th>
            <th scope="col">Auth</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((row) => (
            <tr key={`${row.specId}-${row.method ?? ''}-${row.path}`}>
              <td className={styles.method}>{row.method ?? '—'}</td>
              <td className={styles.path}>
                <code>{row.path}</code>
                {row.note ? (
                  <span className={styles.pathNote}>{row.note}</span>
                ) : null}
              </td>
              <td className={styles.purpose}>
                {row.purpose}
                {/* Plain `.chip`, which resolves to the neutral default: a
                    stream marker is not an ADR-0009 span category and must not
                    borrow one's accent. */}
                {row.streaming ? <> <span className="chip">stream</span></> : null}
              </td>
              <td>
                {row.authPosture ? (
                  <span className={styles.posture}>{row.authPosture}</span>
                ) : null}
                {row.auth && row.auth !== row.authPosture ? (
                  <span className={styles.authNote}>
                    {row.auth.slice(row.authPosture?.length ?? 0).replace(/^\s*[—–-]\s*/, '')}
                  </span>
                ) : null}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

/**
 * What this page does and does not cover. Derived from the same pass that built
 * the rows, so a specification that grows an endpoint table stops being listed
 * here without anybody noticing it had been.
 */
function Scope(): ReactNode {
  const silent = endpoints.coverage.filter((spec) => spec.rowCount === 0);
  const contributing = endpoints.coverage.filter((spec) => spec.rowCount > 0);
  return (
    <div className={styles.scope}>
      <p>
        <strong>This is not the whole API.</strong> It is every row of every
        endpoint table the specifications carry, and the specifications name those
        sections{' '}
        {joinNames(
          endpoints.sectionNames.map((name) => <code key={name}>## {name}</code>),
        )}
        . A specification that documents its surface some other way contributes
        nothing, and {contributing.length} of {endpoints.coverage.length}{' '}
        specifications contribute at all.
      </p>
      {silent.length > 0 ? (
        <div>
          <p>Carrying no endpoint section, and therefore no rows below:</p>
          <ul className={styles.scopeList}>
            {silent.map((spec) => (
              <li key={spec.specId}>
                <Link to={spec.specHref}>
                  <code>{spec.specId}</code> {spec.specTitle}
                </Link>
              </li>
            ))}
          </ul>
        </div>
      ) : null}
    </div>
  );
}

/**
 * The streaming surface, called out separately because SPEC-0010 asks for it and
 * because a long-lived SSE connection is a different thing to a request/response
 * endpoint. Which rows qualify is derived — an SSE method, an SSE purpose, or a
 * `/stream` path — not listed.
 */
function Streaming(): ReactNode {
  const rows = endpoints.rows.filter((row) => row.streaming);
  if (rows.length === 0) {
    return null;
  }
  return (
    <section className={styles.section}>
      <h2 id="streaming">Streaming</h2>
      <p>
        Endpoints the specifications describe as server-sent event streams rather
        than as request/response calls.
      </p>
      <EndpointTable rows={rows} />
    </section>
  );
}

export function HttpReference(): ReactNode {
  const contributing = endpoints.coverage.filter((spec) => spec.rowCount > 0);
  return (
    <>
      <p className={styles.sourceLine}>
        {endpoints.rows.length} endpoints, read at build time out of the endpoint
        tables in {contributing.length} specifications. Nothing on this page is
        typed by hand; adding a row to a specification adds it here.
      </p>
      <Scope />
      {contributing.map((spec) => {
        const rows = endpoints.rows.filter((row) => row.specId === spec.specId);
        return (
          <section key={spec.specId} className={styles.section}>
            <h2 id={spec.specId.toLowerCase()}>
              <Link to={spec.specHref}>
                <code>{spec.specId}</code> {spec.specTitle}
              </Link>
            </h2>
            <p className={styles.sourceLine}>
              Derived from{' '}
              {joinNames(
                spec.sections.map((section) => {
                  const href = rows.find((row) => row.section === section)
                    ?.sectionHref;
                  return href ? (
                    <Link key={section} to={href}>
                      ## {section}
                    </Link>
                  ) : (
                    <span key={section}>## {section}</span>
                  );
                }),
              )}
            </p>
            <EndpointTable rows={rows} />
          </section>
        );
      })}
      <Streaming />
    </>
  );
}

export default HttpReference;
