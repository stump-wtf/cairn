// Unit test for the Bin tab scope/count math exported by app.js (issue #67):
// the per-tab counts over the loaded rows, and the URL-hash scope that makes a
// filtered view bookmarkable. These run under plain node — the module's
// top-level listeners are registered against a minimal stub, so requiring it
// never touches a real DOM.
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

// 1. Counts count exactly the loaded rows each lens filters. An identical
//    corpus (everything agent-pushed and shared — the case from #67) yields
//    identical counts, so the tabs are VISIBLY identical rather than
//    mysteriously so.
assert.deepStrictEqual(
  app.binCountsByScope([
    { agent: true, shared: true },
    { agent: true, shared: true },
    { agent: true, shared: true }
  ]),
  { all: 3, shared: 3, agents: 3 },
  'identical scopes produce identical counts'
);

// 2. A mixed corpus splits the lenses truthfully.
assert.deepStrictEqual(
  app.binCountsByScope([
    { agent: true, shared: true },   // both lenses
    { agent: true, shared: false },  // agent-only
    { agent: false, shared: true },  // shared-only
    { agent: false, shared: false }  // neither
  ]),
  { all: 4, shared: 2, agents: 2 },
  'mixed rows split across the scopes'
);

// 3. Empty bin: every count is zero.
assert.deepStrictEqual(app.binCountsByScope([]), { all: 0, shared: 0, agents: 0 });

// 4. The URL hash restores the scope, defaulting to 'all' for an absent,
//    malformed, or out-of-vocabulary value so a stale/hand-edited hash can
//    never select a lens that doesn't exist.
assert.strictEqual(app.binScopeFromHash(''), 'all');
assert.strictEqual(app.binScopeFromHash('#'), 'all');
assert.strictEqual(app.binScopeFromHash('#scope=shared'), 'shared');
assert.strictEqual(app.binScopeFromHash('#scope=agents'), 'agents');
assert.strictEqual(app.binScopeFromHash('#scope=all'), 'all');
assert.strictEqual(app.binScopeFromHash('#scope=nope'), 'all', 'unknown scope falls back to all');
assert.strictEqual(app.binScopeFromHash('#other=1&scope=shared'), 'shared', 'scope amid other hash params');
assert.strictEqual(app.binScopeFromHash('#scope=SHARED'), 'all', 'scope is case-sensitive, uppercase is not a scope');

console.log('app_js_test: ok');
