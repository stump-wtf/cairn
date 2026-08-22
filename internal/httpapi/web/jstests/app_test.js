// Unit test for the Bin lens math exported by app.js (#67, #136): the
// per-visibility counts over the loaded rows, the type-menu option math, and
// the URL-hash round-trip that makes a filtered view bookmarkable. These run
// under plain node — the module's top-level listeners are registered against a
// minimal stub, so requiring it never touches a real DOM.
'use strict';

var assert = require('assert');

// Minimal browser stubs so app.js's top-level addEventListener calls (CSRF,
// Alpine init, the bin IIFE's DOMContentLoaded check) are no-ops at require
// time. Only the pieces app.js touches at load are stubbed.
global.document = {
  addEventListener: function () {},
  readyState: 'loading',
  querySelector: function () { return null; },
  querySelectorAll: function () { return []; }
};
global.window = { addEventListener: function () {}, location: { hash: '', pathname: '/', search: '' } };
// navigator is only read inside Alpine's copy() (never at require time), and
// node defines it as a getter-only global, so it needs no stub here.

var app = require('../assets/app.js');

// ---- Visibility lenses ----------------------------------------------------

// 1. Shared and private are COMPLEMENTS, so the two lenses always sum to `all`.
//    This is the whole point of the change: the old Bin/Shared/From-agents tabs
//    overlapped, so on an ordinary bin (everything agent-pushed and link-shared)
//    all three showed the same count and none of them filtered anything.
assert.deepStrictEqual(
  app.binCountsByScope([{ shared: true }, { shared: true }, { shared: true }]),
  { all: 3, shared: 3, private: 0 },
  'an all-shared bin puts every row in one lens and none in the other'
);
assert.deepStrictEqual(
  app.binCountsByScope([{ shared: true }, { shared: false }, { shared: false }]),
  { all: 3, shared: 1, private: 2 },
  'a mixed bin splits across the complements'
);
assert.deepStrictEqual(app.binCountsByScope([]), { all: 0, shared: 0, private: 0 });

// 2. Counts tolerate the malformed entries a partially-rendered row list can
//    produce, rather than throwing and taking the whole listing down with it.
//    A row with no readable flag counts as private — the safe reading, since it
//    must never be presented as shared.
assert.deepStrictEqual(
  app.binCountsByScope([null, undefined, {}, { shared: true }]),
  { all: 4, shared: 1, private: 3 },
  'missing flags read as private, never as a throw'
);

// 3. The lens predicate agrees with the counts: one definition, three callers
//    (row filter, counts, empty-state check).
assert.strictEqual(app.binScopeAccepts('all', true), true);
assert.strictEqual(app.binScopeAccepts('all', false), true);
assert.strictEqual(app.binScopeAccepts('shared', true), true);
assert.strictEqual(app.binScopeAccepts('shared', false), false);
assert.strictEqual(app.binScopeAccepts('private', false), true);
assert.strictEqual(app.binScopeAccepts('private', true), false);
assert.strictEqual(app.binScopeAccepts('nonsense', false), true, 'an unknown lens narrows nothing');

// ---- Type menu ------------------------------------------------------------

// 4. Options come from the loaded rows, one per distinct type, ordered by count
//    descending then name — stable across rebuilds, so an HTMX "load more"
//    cannot reshuffle checkboxes under the pointer.
assert.deepStrictEqual(
  app.binTypeCounts([
    { type: 'markdown', name: 'markdown' },
    { type: 'trajectory', name: 'trace' },
    { type: 'markdown', name: 'markdown' },
    { type: 'image', name: 'image' },
    { type: 'trajectory', name: 'trace' }
  ]),
  [
    { type: 'markdown', name: 'markdown', count: 2, total: 2 },
    { type: 'trajectory', name: 'trace', count: 2, total: 2 },
    { type: 'image', name: 'image', count: 1, total: 1 }
  ],
  'options are counted per type, ordered by total then name'
);

// 5. A row with no type produces no option rather than a nameless one, and a
//    row missing its display name falls back to the registry key.
assert.deepStrictEqual(
  app.binTypeCounts([{ type: '' }, null, { type: 'code' }]),
  [{ type: 'code', name: 'code', count: 1, total: 1 }],
  'typeless rows are skipped; a missing name falls back to the key'
);

// 5b. The counts are FACET counts: ineligible rows (filtered out by the
//     visibility tab or the text box) still contribute their OPTION but not
//     their count, so the menu says how many rows you would actually get. A
//     type whose every row is ineligible stays listed at 0 rather than
//     vanishing — otherwise a ticked type could disappear with no way to untick
//     it and no way to see why the Bin looks empty.
assert.deepStrictEqual(
  app.binTypeCounts([
    { type: 'markdown', name: 'markdown', eligible: true },
    { type: 'markdown', name: 'markdown', eligible: false },
    { type: 'image', name: 'image', eligible: false }
  ]),
  [
    { type: 'markdown', name: 'markdown', count: 1, total: 2 },
    { type: 'image', name: 'image', count: 0, total: 1 }
  ],
  'ineligible rows keep their option and their total, but not the facet count'
);

// 5c. Ordering is by the LOADED total, not the facet count, so narrowing the
//     text box (which only moves facet counts) cannot reshuffle the checkboxes
//     under the pointer while the menu is open.
assert.deepStrictEqual(
  app.binTypeCounts([
    { type: 'markdown', name: 'markdown', eligible: false },
    { type: 'markdown', name: 'markdown', eligible: false },
    { type: 'image', name: 'image', eligible: true }
  ]).map(function (t) { return t.type; }),
  ['markdown', 'image'],
  'the type with more loaded rows stays first even when its facet count is lower'
);

// 6. An EMPTY selection means "all types", never "no types" — otherwise a type
//    arriving on a later keyset page would be invisible behind a selection made
//    before it loaded.
assert.strictEqual(app.binTypeAccepts([], 'markdown'), true);
assert.strictEqual(app.binTypeAccepts(['markdown'], 'markdown'), true);
assert.strictEqual(app.binTypeAccepts(['markdown'], 'image'), false);
assert.strictEqual(app.binTypeAccepts(['markdown', 'image'], 'image'), true);

// 7. Toggling is duplicate-free and non-mutating, so a double-fired change
//    event cannot enter the same type twice or corrupt the caller's array.
var sel = ['markdown'];
assert.deepStrictEqual(app.binToggleType(sel, 'image', true), ['markdown', 'image']);
assert.deepStrictEqual(app.binToggleType(sel, 'markdown', true), ['markdown'], 're-checking is idempotent');
assert.deepStrictEqual(app.binToggleType(sel, 'markdown', false), []);
assert.deepStrictEqual(sel, ['markdown'], 'the input selection is never mutated');

// 8. The button says what you filtered TO when that is one thing, and how many
//    when it is not.
var present = [
  { type: 'markdown', name: 'markdown', count: 2, total: 2 },
  { type: 'trajectory', name: 'trace', count: 1, total: 1 }
];
assert.strictEqual(app.binTypeSummary([], present), 'All types');
assert.strictEqual(app.binTypeSummary(['trajectory'], present), 'trace', 'one type shows its reader-facing name');
assert.strictEqual(app.binTypeSummary(['markdown', 'trajectory'], present), '2 types');
assert.strictEqual(app.binTypeSummary(['gone'], present), 'gone', 'a type no longer loaded still labels itself');

// ---- URL hash round-trip --------------------------------------------------

// 9. The hash restores the visibility lens, defaulting to 'all' for an absent,
//    malformed, or out-of-vocabulary value so a stale/hand-edited hash can never
//    select a lens that doesn't exist.
assert.strictEqual(app.binScopeFromHash(''), 'all');
assert.strictEqual(app.binScopeFromHash('#'), 'all');
assert.strictEqual(app.binScopeFromHash('#scope=shared'), 'shared');
assert.strictEqual(app.binScopeFromHash('#scope=private'), 'private');
assert.strictEqual(app.binScopeFromHash('#scope=all'), 'all');
assert.strictEqual(app.binScopeFromHash('#scope=agents'), 'all', 'the retired lens no longer resolves');
assert.strictEqual(app.binScopeFromHash('#scope=nope'), 'all', 'unknown scope falls back to all');
assert.strictEqual(app.binScopeFromHash('#type=code&scope=private'), 'private', 'scope amid other hash params');
assert.strictEqual(app.binScopeFromHash('#scope=SHARED'), 'all', 'scope is case-sensitive, uppercase is not a scope');

// 10. A hash carrying a scope-SHAPED key that is not the scope key must not be
//     mistaken for one — the delimiter alternation is what stops `#myscope=x`
//     from selecting a lens the user never asked for. The same guard protects
//     the type key.
assert.strictEqual(app.binScopeFromHash('#myscope=shared'), 'all', 'a suffix match is not the scope key');
assert.strictEqual(app.binScopeFromHash('#scope='), 'all', 'an empty scope value falls back');
assert.strictEqual(app.binScopeFromHash('#scope=shared&scope=private'), 'shared', 'the first scope wins');
assert.deepStrictEqual(app.binTypesFromHash('#mytype=code'), [], 'a suffix match is not the type key');

// 11. Types round-trip as a comma-separated list, de-duplicated and trimmed.
//     Keys are deliberately NOT validated against a vocabulary: the share-type
//     registry is extensible (ADR-0002), so an allowlist here would silently
//     drop a newly registered type out of a shared URL. An unknown key matches
//     no row, which is harmless.
assert.deepStrictEqual(app.binTypesFromHash(''), []);
assert.deepStrictEqual(app.binTypesFromHash('#type='), []);
assert.deepStrictEqual(app.binTypesFromHash('#type=markdown'), ['markdown']);
assert.deepStrictEqual(app.binTypesFromHash('#type=markdown,image'), ['markdown', 'image']);
assert.deepStrictEqual(app.binTypesFromHash('#type=markdown%2Cimage'), ['markdown', 'image'], 'an encoded comma decodes');
assert.deepStrictEqual(app.binTypesFromHash('#type=markdown, image ,markdown'), ['markdown', 'image'], 'trimmed and de-duplicated');
assert.deepStrictEqual(app.binTypesFromHash('#scope=private&type=code'), ['code'], 'types amid other hash params');
assert.deepStrictEqual(app.binTypesFromHash('#type=brand-new-type'), ['brand-new-type'], 'unknown keys survive; the registry is extensible');

// 12. The hash is empty at the defaults, so an unfiltered Bin keeps a clean URL,
//     and every non-default state round-trips through both readers.
assert.strictEqual(app.binHashFromState('all', []), '');
assert.strictEqual(app.binHashFromState('private', []), '#scope=private');
assert.strictEqual(app.binHashFromState('all', ['markdown', 'image']), '#type=markdown%2Cimage');
var h = app.binHashFromState('shared', ['markdown', 'trajectory']);
assert.strictEqual(app.binScopeFromHash(h), 'shared');
assert.deepStrictEqual(app.binTypesFromHash(h), ['markdown', 'trajectory']);

console.log('app_js_test: ok');
