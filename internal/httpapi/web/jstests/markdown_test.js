// Unit test for the pure helpers markdown.js exports for click-to-copy (#133):
// artifactIDFromPath (which id segment a viewer page carries) and sourceURLFor
// (the same-origin link-capability route that streams the ORIGINAL markdown
// source for the document ⋮ menu's Copy Markdown item). These run under plain
// node — the module's top-level listeners are registered against a minimal
// stub, so requiring it never touches a real DOM.
'use strict';

var assert = require('assert');

// Minimal browser stubs so markdown.js's top-level registrations (the picker
// and doc-menu click handlers, the DOMContentLoaded check) are no-ops at
// require time. Only the pieces the module touches at load are stubbed.
global.document = {
  addEventListener: function () {},
  readyState: 'loading',
  querySelector: function () { return null; },
  querySelectorAll: function () { return []; }
};
global.window = { addEventListener: function () {}, location: { pathname: '/' } };

var md = require('../assets/markdown.js');

// 1. artifactIDFromPath picks the id out of a bare share URL.
assert.strictEqual(md.artifactIDFromPath('/RhGHlvCZ'), 'RhGHlvCZ', 'bare share URL yields the id');
assert.strictEqual(md.artifactIDFromPath('/RhGHlvCZ/some-deep-link'), 'RhGHlvCZ', 'only the first segment is the id');
assert.strictEqual(md.artifactIDFromPath('///RhGHlvCZ'), 'RhGHlvCZ', 'leading slashes collapse');

// 2. Degenerate paths degrade to '' rather than throwing, so a viewer that
//    somehow renders off an artifact page simply offers no copy source.
assert.strictEqual(md.artifactIDFromPath('/'), '', 'root path has no id');
assert.strictEqual(md.artifactIDFromPath(''), '', 'empty path has no id');
assert.strictEqual(md.artifactIDFromPath(null), '', 'missing path has no id');

// 3. sourceURLFor resolves the standalone artifact's download route.
assert.strictEqual(md.sourceURLFor('RhGHlvCZ', null), '/RhGHlvCZ/download', 'standalone viewer uses the download route');
assert.strictEqual(md.sourceURLFor('RhGHlvCZ', ''), '/RhGHlvCZ/download', 'an empty member name means standalone');

// 4. Inside a bundle member pane the member's body route is used instead.
assert.strictEqual(md.sourceURLFor('RhGHlvCZ', 'notes.md'), '/RhGHlvCZ/members/notes.md', 'member viewer uses the member body route');

// 5. Path-unsafe characters are percent-encoded in BOTH segments — a member
//    name with a space or slash must never alter the route's shape.
assert.strictEqual(md.sourceURLFor('RhGHlvCZ', 'my notes.md'), '/RhGHlvCZ/members/my%20notes.md', 'space in member name is encoded');
assert.strictEqual(md.sourceURLFor('RhGHlvCZ', 'sub/dir/notes.md'), '/RhGHlvCZ/members/sub%2Fdir%2Fnotes.md', 'slashes in member name are encoded');
assert.strictEqual(md.sourceURLFor('a b', null), '/a%20b/download', 'space in the id is encoded');

// 6. No id means no route: the menu item surfaces a copy failure rather than
//    fetching a relative URL against the wrong page.
assert.strictEqual(md.sourceURLFor('', null), '', 'no id yields no URL');
assert.strictEqual(md.sourceURLFor('', 'notes.md'), '', 'no id yields no URL even with a member');

console.log('markdown_js_test: ok');
