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

console.log('trajectory_js_test: ok');
