// Unit test for the pure row-anchor math exported by trajectory.js — the
// scrubber's row↔time map that issue #61 flagged. These run under plain node
// (no DOM, no browser): the module guards its init behind a `document` check,
// so requiring it only exercises the exported helpers.
//
// The fixture mirrors the shape buildWaterfall (trajectory_view.go) emits: a
// depth-first walk, so a sub-agent PARENT row precedes its children even
// though the parent outlasts them.
'use strict';

var assert = require('assert');
var tj = require('./trajectory.js');

// A sub-agent excursion: parent [0..200], children nested inside it, then a
// following top-level span. Anchors are START times (posMs) in row order.
var subAgentRowStarts = [0, 20, 50, 80, 220]; // parent, 3 children, next

// 1. Start anchors keep a parent BEFORE its children, so the raw sequence
//    through a sub-agent is already monotone — the midpoint anchor that broke
//    this is gone.
assert.deepStrictEqual(
  subAgentRowStarts.map(tj.anchorTimeFor),
  [0, 20, 50, 80, 220],
  'start anchors sort a sub-agent parent before its children'
);

// 2. The envelope over that sequence is the identity (nothing to clamp), so
//    the inverse map resolves a time inside the excursion to a row inside it —
//    the rows are no longer unreachable by scrubbing.
assert.deepStrictEqual(
  tj.envelopeAnchors(subAgentRowStarts),
  [0, 20, 50, 80, 220],
  'the sub-agent envelope is monotone and preserves the child rows'
);

// 3. Genuine time overlap (a top-level span starting before an earlier span's
//    deep child) is clamped UP to the predecessor, in reading order — never
//    backward.
assert.deepStrictEqual(
  tj.envelopeAnchors([0, 150, 100, 220]),
  [0, 150, 150, 220],
  'an overlapped later row inherits the running max, keeping time non-decreasing'
);

// 4. Unanchored rows (live-appended, no posMs) pass through as null and do not
//    advance the carrier, so a trailing run of them inherits a sane time.
assert.deepStrictEqual(
  tj.envelopeAnchors([0, null, 50, null]),
  [0, null, 50, null],
  'nulls pass through without dragging the envelope back to zero'
);

// 5. anchorTimeFor maps a missing posMs to null (no anchor), never to 0 — the
//    bug that pinned every streamed row at the start of the run.
assert.strictEqual(tj.anchorTimeFor(null), null);
assert.strictEqual(tj.anchorTimeFor(undefined), null);
assert.strictEqual(tj.anchorTimeFor(42), 42);

// 6. The envelope never walks time backward for any row list — the invariant
//    both syncPan and scrollTopForTime rely on.
[subAgentRowStarts, [0, 150, 100, 220], [5], [], [0, 0, 0], [100, 1, 2, 3]].forEach(function (rows) {
  var env = tj.envelopeAnchors(rows);
  for (var i = 1; i < env.length; i++) {
    if (env[i] !== null && env[i - 1] !== null) {
      assert.ok(env[i] >= env[i - 1], 'envelope monotone at index ' + i + ' for ' + JSON.stringify(rows));
    }
  }
});

// ---- Axis math: the time↔pixel conversions over the collapsed track --------
// A 30s run zoomed 3x in a 900px pane whose sticky label column eats 228px:
// the axis is 3 * (900 - 228) = 2016px wide and 672px of it is on screen.
var AXIS_W = 2016, VIEW_W = 672, TOTAL = 30000;

// 7. The box's width is the visible fraction of the AXIS, not of the whole row.
//    Measuring the row (label column included) is what let the box claim ticks
//    whose bars were still off screen.
var atStart = tj.viewportGeom(AXIS_W, VIEW_W, 0);
assert.strictEqual(atStart.frac, VIEW_W / AXIS_W, 'box width is viewW/axisW');
assert.strictEqual(atStart.at, 0, 'unscrolled pan sits at the start of its travel');

// 8. `at` saturates at 1 exactly when the pan reaches the end of the axis, so
//    the box's right edge lands at 100% and not short of it.
var atEnd = tj.viewportGeom(AXIS_W, VIEW_W, AXIS_W - VIEW_W);
assert.strictEqual(atEnd.at, 1, 'a fully scrolled pan uses up all of its travel');
assert.ok(Math.abs(atEnd.at * (1 - atEnd.frac) + atEnd.frac - 1) < 1e-9,
  'the box right edge reaches exactly 100% at the end of the run');

// 9. An axis that fits on screen has nothing to pan: full-width box, at 0 —
//    and no division by a zero travel.
var noPan = tj.viewportGeom(600, 900, 0);
assert.strictEqual(noPan.frac, 1, 'an axis narrower than the pane fills the box');
assert.strictEqual(noPan.at, 0, 'no travel means no pan');

// 10. `at * (1 - frac)` is the window's LEFT edge as a fraction of the run —
//     the quantity layoutRuler turns into its `from` label. It must equal
//     scrollLeft/axisW, or the ruler and the box label different instants.
[0, 300, 900, AXIS_W - VIEW_W].forEach(function (sl) {
  var g = tj.viewportGeom(AXIS_W, VIEW_W, sl);
  assert.ok(Math.abs(g.at * (1 - g.frac) - sl / AXIS_W) < 1e-9,
    'ruler `from` matches the box left edge at scrollLeft ' + sl);
});

// 11. timeToScrollLeft and centreTimeForScrollLeft are exact inverses. The
//     pointer drag round-trips through both (scrollLeft → time → scrollTop →
//     syncPan → scrollLeft), so any mismatch makes a held drag creep.
[0, 5000, 15000, 29000].forEach(function (t) {
  var sl = tj.timeToScrollLeft(t, TOTAL, AXIS_W, VIEW_W);
  assert.ok(Math.abs(tj.centreTimeForScrollLeft(sl, TOTAL, AXIS_W, VIEW_W) - t) < 1e-9,
    'time round-trips through scrollLeft at t=' + t);
});

// 12. The conversions divide into the AXIS, never the axis plus the label
//     column. Centring the midpoint of the run must land the pan exactly
//     halfway through its travel — off-by-a-label-column is the #61 bug.
assert.strictEqual(
  tj.timeToScrollLeft(TOTAL / 2, TOTAL, AXIS_W, VIEW_W),
  AXIS_W / 2 - VIEW_W / 2,
  'the midpoint of the run centres at the midpoint of the travel'
);

// 13. Degenerate inputs (pre-layout reads, where widths are 0) return finite
//     numbers rather than NaN/Infinity — those would poison scrollLeft and
//     blank the pane.
[[0, 0, 0], [0, 100, 50], [100, 0, 0]].forEach(function (a) {
  var g = tj.viewportGeom(a[0], a[1], a[2]);
  assert.ok(isFinite(g.frac) && isFinite(g.at), 'viewportGeom finite for ' + JSON.stringify(a));
  assert.ok(isFinite(tj.timeToScrollLeft(1000, 0, a[0], a[1])), 'timeToScrollLeft finite for ' + JSON.stringify(a));
  assert.ok(isFinite(tj.centreTimeForScrollLeft(a[2], 0, a[0], a[1])), 'centreTime finite for ' + JSON.stringify(a));
});

console.log('trajectory_js_test: ok');
