// Governing: ADR-0014, SPEC-0010 REQ "Derived Status, Dates, and Counts"
//
// The status badge, and the one import of the record stylesheet.
//
// It lives in its own module because both consumers need it and neither should
// have to import the other: the generated indexes (`./indexes`, loaded only by
// the two staged index pages) and the detail-page metadata bar (`./meta`, loaded
// by every record page). Importing `./indexes` from `./meta` would put the whole
// decision table and spec-card list on every record page for the sake of a
// twelve-line span.
//
// `record.css` is imported here, once, for the same reason: both consumers are
// downstream of this module, so the stylesheet follows them onto exactly the
// routes that render record chrome and no others.
//
// No colour literal appears here, and none may: the palette lives in exactly one
// file, `src/css/custom.css`.

import React, {type ReactNode} from 'react';

import './record.css';

/**
 * A status, derived from front-matter and carried as TEXT — SPEC-0010 REQ
 * "Derived Status, Dates, and Counts": the badge's colour may not be the only
 * carrier of its meaning.
 *
 * Two things beyond the word do that work. `data-status` drives a per-status
 * glyph, so the six statuses stay distinguishable with colour removed and to a
 * reader who cannot separate the accents; the glyph is `aria-hidden` because it
 * says nothing the adjacent word does not already say.
 */
export function StatusBadge({status}: {status: string}): ReactNode {
  return (
    <span className="cairn-record-status" data-status={status}>
      <span className="cairn-record-status__mark" aria-hidden="true" />
      {status.toUpperCase()}
    </span>
  );
}
