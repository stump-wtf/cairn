// Shell interactivity for the Cairn web app (ADR-0011). Written against the
// Alpine CSP build (@alpinejs/csp): directive expressions may only be bare
// property access or method calls registered here — no inline expressions — so
// the shell needs no 'unsafe-inline'/'unsafe-eval'-of-page-source in its CSP.
//
// Two view-local behaviors live here, both without a server round-trip
// (SPEC-0001 REQ "One URL Control with Copy and MCP Affordance", REQ
// "Collapsible Metadata + Comments Panel"):
//   - the URL control: toggle the shown value between the web link and the
//     mcp:// handle, and copy the active one to the clipboard; and
//   - the details panel: collapse/expand, animating grid-template-columns.
// Double-submit CSRF for session-authenticated HTMX posts (SPEC-0006 REQ "CSRF
// Protection"): copy the readable cairn_csrf cookie into the X-CSRF-Token header
// the server matches against it. A cross-site attacker can force the ambient
// session cookie to ride along but cannot read this cookie to set the header, so
// the match proves same-origin. Token (API/MCP/CLI) callers carry no cookie and
// are exempt server-side. Reading a cookie needs no eval, so this stays within
// the shell's 'self'-only CSP.
document.addEventListener('htmx:configRequest', (evt) => {
  const m = document.cookie.match(/(?:^|;\s*)cairn_csrf=([^;]+)/);
  if (m) {
    evt.detail.headers['X-CSRF-Token'] = decodeURIComponent(m[1]);
  }
});

document.addEventListener('alpine:init', () => {
  window.Alpine.data('shell', () => ({
    mode: 'link',
    panelOpen: true,
    copied: false,
    link: '',
    mcp: '',

    init() {
      this.link = this.$root.dataset.link || '';
      this.mcp = this.$root.dataset.mcp || '';
    },

    isLink() { return this.mode === 'link'; },
    isMcp() { return this.mode === 'mcp'; },
    showLink() { this.mode = 'link'; },
    showMcp() { this.mode = 'mcp'; },

    notCopied() { return !this.copied; },

    copy() {
      const value = this.isLink() ? this.link : this.mcp;
      const done = () => {
        this.copied = true;
        setTimeout(() => { this.copied = false; }, 1300);
      };
      if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(value).then(done).catch(done);
      } else {
        done();
      }
    },

    togglePanel() { this.panelOpen = !this.panelOpen; },
    panelExpanded() { return this.panelOpen ? 'true' : 'false'; },
    panelClass() { return this.panelOpen ? 'panel-open' : 'panel-closed'; },
  }));
});

// Bin filtering (SPEC-0001 The Bin Listing; design turn 6c). The tabs
// (Bin/Shared/Agents) and the text filter are view-local lenses over the rows
// the server already rendered — they hide/show `.bin-row` elements, they never
// re-query. That keeps the keyset pagination invariant intact (the server still
// owns ordering + the cursor) and means the no-JS page IS the full Bin, so the
// listing degrades gracefully (SPEC-0001 Progressive Enhancement). Reading DOM
// data-* attributes needs no eval, so this stays within the shell's 'self'-only
// CSP. Runs as plain vanilla JS (not Alpine) because it touches an
// arbitrary-length row list, which the Alpine CSP build cannot express inline.
(function () {
  function initBin() {
    const toolbar = document.querySelector('[data-bin-toolbar]');
    const rowsBox = document.querySelector('[data-bin-rows]');
    if (!toolbar || !rowsBox) return;
    const tabs = Array.from(toolbar.querySelectorAll('.bin-tab'));
    const filter = toolbar.querySelector('[data-bin-filter]');
    const noMatch = document.querySelector('[data-bin-nomatch]');
    let scope = 'all';

    // The three scopes are client-side lenses over the loaded rows, so the
    // per-tab counts count the same loaded rows the tabs filter — both see
    // exactly the loaded page, never a total the lens can't back up. Counts
    // refresh on apply() so an HTMX "load more" keeps them honest.
    //
    // Only the narrowing lenses get their own empty copy: `all` is empty only
    // when the bin itself is, and that case never reaches here — the server
    // renders its own empty state instead of a row list. The scope vocabulary
    // lives in binScopeFromHash, the one place that has to validate it.
    const EMPTY_COPY = {
      shared: 'Nothing shared yet.',
      agents: 'No agent pushes yet.'
    };
    const NO_MATCH_COPY = 'No artifacts match this filter.';

    function rowMatches(row) {
      if (scope === 'shared' && row.dataset.shared !== '1') return false;
      if (scope === 'agents' && row.dataset.agent !== '1') return false;
      const q = (filter && filter.value ? filter.value : '').trim().toLowerCase();
      if (q && (row.dataset.search || '').toLowerCase().indexOf(q) === -1) return false;
      return true;
    }

    function rowInScope(row, s) {
      if (s === 'shared') return row.dataset.shared === '1';
      if (s === 'agents') return row.dataset.agent === '1';
      return true;
    }

    function apply() {
      const rows = Array.from(rowsBox.querySelectorAll('.bin-row'));
      let shown = 0;
      rows.forEach(function (row) {
        const ok = rowMatches(row);
        row.hidden = !ok;
        if (ok) shown++;
      });
      // Refresh each tab's count over the loaded rows, so identical scopes are
      // VISIBLY identical (Bin 24 · Shared 24 · From agents 24) rather than
      // mysteriously so (#67). The counting itself is the exported pure helper
      // (binCountsByScope) so the node test pins the exact figures the tabs
      // show.
      const flags = rows.map(function (row) {
        return { agent: row.dataset.agent === '1', shared: row.dataset.shared === '1' };
      });
      const counts = binCountsByScope(flags);
      tabs.forEach(function (tab) {
        const s = tab.dataset.scope || 'all';
        const n = counts[s] || 0;
        const c = tab.querySelector('[data-tab-count]');
        if (c) c.textContent = String(n);
        // data-label, never textContent: the count span is INSIDE the button,
        // so textContent already reads "Bin 24" and the fallback would build
        // "Bin 24 — 24 artifacts".
        tab.setAttribute('aria-label', (tab.dataset.label || '') + ' — ' + n + ' artifacts');
      });
      // Per-scope empty state: when the active scope has no rows of its own,
      // say which lens is empty rather than showing a bare "no match"
      // (distinct from the text filter's "no artifacts match" case).
      //
      // The element is role="status" aria-live="polite", so WRITING to it is
      // what announces it. apply() runs on every keystroke in the filter box,
      // and re-assigning the same string still replaces the text node — which
      // re-announces the message on each character typed. Write only on an
      // actual change.
      if (noMatch) {
        const inScope = rows.filter(function (row) { return rowInScope(row, scope); }).length;
        if (rows.length > 0 && shown === 0) {
          const copy = (inScope === 0 && EMPTY_COPY[scope]) ? EMPTY_COPY[scope] : NO_MATCH_COPY;
          if (noMatch.textContent !== copy) noMatch.textContent = copy;
          noMatch.hidden = false;
        } else {
          noMatch.hidden = true;
        }
      }
    }

    // The active scope lives in the URL hash (`#scope=shared`) so a filtered
    // view is bookmarkable/shareable and survives reload (#67).
    function writeScopeToURL() {
      const hash = scope === 'all' ? '' : '#scope=' + encodeURIComponent(scope);
      const url = window.location.pathname + window.location.search + hash;
      if (window.history && window.history.replaceState) window.history.replaceState(null, '', url);
    }

    function scopeFromURL() {
      return binScopeFromHash(window.location.hash || '');
    }

    function selectTab(tab, updateURL) {
      scope = tab.dataset.scope || 'all';
      tabs.forEach(function (t) {
        const on = t === tab;
        t.setAttribute('aria-selected', on ? 'true' : 'false');
        t.tabIndex = on ? 0 : -1;
      });
      if (updateURL !== false) writeScopeToURL();
      apply();
    }

    tabs.forEach(function (tab, i) {
      tab.addEventListener('click', function () { selectTab(tab); });
      tab.addEventListener('keydown', function (e) {
        let next = -1;
        if (e.key === 'ArrowRight') next = (i + 1) % tabs.length;
        else if (e.key === 'ArrowLeft') next = (i - 1 + tabs.length) % tabs.length;
        else if (e.key === 'Home') next = 0;
        else if (e.key === 'End') next = tabs.length - 1;
        else return;
        e.preventDefault();
        tabs[next].focus();
        selectTab(tabs[next]);
      });
    });
    if (filter) filter.addEventListener('input', apply);

    // Re-apply after an HTMX "load more" swaps the next keyset page in, so the
    // active tab/filter also govern the newly appended rows.
    document.body.addEventListener('htmx:afterSwap', function (e) {
      if (rowsBox.contains(e.target) || e.target === rowsBox) apply();
    });

    // Restore the scope from the URL on load, then render. A hash the user
    // edited by hand (back/forward) re-selects without re-pushing the hash.
    const initial = scopeFromURL();
    const initialTab = tabs.find(function (t) { return (t.dataset.scope || 'all') === initial; });
    if (initialTab) selectTab(initialTab, false);
    window.addEventListener('hashchange', function () {
      const s = scopeFromURL();
      const t = tabs.find(function (x) { return (x.dataset.scope || 'all') === s; });
      if (t && (t.dataset.scope || 'all') !== scope) selectTab(t, false);
    });

    apply();
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', initBin);
  } else {
    initBin();
  }
})();

// ---- Bin tab scope/count math (exported for the node unit test) ------------
// The DOM-free core of the Bin tab lenses (#67), lifted out of the IIFE so
// app_js_test.js can pin count computation and URL-hash scope restoration
// without a browser. The in-page code calls the same functions.

// binCountsByScope counts loaded rows per tab scope from their {agent, shared}
// flags, over exactly the rows the client-side lenses filter. `all` counts
// every loaded row; `shared`/`agents` count rows carrying that flag.
function binCountsByScope(flags) {
  const counts = { all: flags.length, shared: 0, agents: 0 };
  flags.forEach(function (f) {
    if (f && f.shared) counts.shared++;
    if (f && f.agent) counts.agents++;
  });
  return counts;
}

// binScopeFromHash reads the active scope out of the URL hash (`#scope=shared`),
// defaulting to 'all' for an absent, malformed, or out-of-vocabulary value, so
// a hand-edited or stale hash can never select a lens that doesn't exist.
function binScopeFromHash(hash) {
  const m = (hash || '').match(/(?:^|#|&)scope=([a-z]+)/);
  return (m && ['all', 'shared', 'agents'].indexOf(m[1]) !== -1) ? m[1] : 'all';
}

if (typeof module !== 'undefined' && module.exports) {
  module.exports = Object.assign(module.exports || {}, {
    binCountsByScope: binCountsByScope,
    binScopeFromHash: binScopeFromHash
  });
}

// Share dialog (SPEC-0001 REQ "Share Affordance"; #46): a native <dialog> so
// the browser furnishes the modal focus trap and Escape-to-dismiss for free —
// while a modal <dialog> is open, everything outside it is inert and Escape
// fires 'cancel' then 'close' with no listener needed. This script only opens
// it from the Share button, wires the explicit close affordances, restores
// focus to the Share button on close (the same WCAG 2.1 focus-management bar
// as the reaction picker in trajectory.js/markdown.js — belt-and-suspenders on
// top of the UA's own restoration), and drives the copy-to-clipboard buttons
// for the link/mcp fields inside it. Plain JS rather than an Alpine directive
// because showModal()/close() are imperative calls the CSP build's
// bare-identifier directives do not fit cleanly; reading data-* attributes and
// calling dialog/clipboard methods needs no eval, so this stays within the
// shell's 'self'-only CSP. Runs on both the generic shell and the trajectory
// viewer, which share the one "share-dialog" template partial (ADR-0011).
(function () {
  function initShareDialog() {
    var trigger = document.querySelector('.share-btn');
    var dialog = document.querySelector('[data-share-dialog]');
    if (!trigger || !dialog) return;
    if (typeof dialog.showModal !== 'function') return; // no-op floor: link/mcp already in the header

    function open() { dialog.showModal(); }
    function close() { if (dialog.open) dialog.close(); }

    trigger.addEventListener('click', open);
    dialog.addEventListener('close', function () {
      if (document.contains(trigger)) trigger.focus();
    });

    var closeBtn = dialog.querySelector('[data-share-close]');
    if (closeBtn) closeBtn.addEventListener('click', close);

    // A click landing on the <dialog> element itself (not a descendant) hit
    // the backdrop/padding box outside the card — dismiss, mirroring the
    // reaction picker's outside-click close.
    dialog.addEventListener('click', function (e) {
      if (e.target === dialog) close();
    });

    var live = dialog.querySelector('[data-share-live]');
    Array.prototype.forEach.call(dialog.querySelectorAll('[data-share-copy]'), function (btn) {
      var defaultLabel = btn.textContent;
      btn.addEventListener('click', function () {
        var value = btn.getAttribute('data-share-copy') || '';
        var done = function () {
          btn.textContent = 'copied ✓';
          btn.classList.add('copied');
          if (live) live.textContent = (btn.getAttribute('data-share-field') || 'value') + ' copied to clipboard';
          setTimeout(function () {
            btn.textContent = defaultLabel;
            btn.classList.remove('copied');
          }, 1300);
        };
        if (navigator.clipboard && navigator.clipboard.writeText) {
          navigator.clipboard.writeText(value).then(done).catch(done);
        } else {
          done();
        }
      });
    });
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', initShareDialog);
  } else {
    initShareDialog();
  }
})();
