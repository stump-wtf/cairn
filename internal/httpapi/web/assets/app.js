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

// Bin filtering (SPEC-0001 The Bin Listing; design turn 6c). The visibility
// tabs (All/Shared/Private), the type menu, and the text filter are view-local
// lenses over the rows the server already rendered — they hide/show `.bin-row`
// elements, they never re-query. That keeps the keyset pagination invariant
// intact (the server still owns ordering + the cursor) and means the no-JS page
// IS the full Bin, so the listing degrades gracefully (SPEC-0001 Progressive
// Enhancement). Reading DOM data-* attributes needs no eval, so this stays
// within the shell's 'self'-only CSP. Runs as plain vanilla JS (not Alpine)
// because it touches an arbitrary-length row list, which the Alpine CSP build
// cannot express inline.
//
// The lenses replaced the old Bin/Shared/From-agents tabs (#136): those
// three scopes overlapped almost completely — an ordinary bin is agent-pushed
// and link-shared end to end, so all three tabs showed the same count and
// filtering by any of them changed nothing. Visibility and type are the two
// axes that actually partition a bin.
(function () {
  function initBin() {
    const toolbar = document.querySelector('[data-bin-toolbar]');
    const rowsBox = document.querySelector('[data-bin-rows]');
    if (!toolbar || !rowsBox) return;
    const tabs = Array.from(toolbar.querySelectorAll('.bin-tab'));
    const filter = toolbar.querySelector('[data-bin-filter]');
    const noMatch = document.querySelector('[data-bin-nomatch]');
    const typeBox = toolbar.querySelector('[data-bin-types]');
    const typeList = toolbar.querySelector('[data-bin-types-list]');
    const typeLabel = toolbar.querySelector('[data-bin-types-label]');
    const typeReset = toolbar.querySelector('[data-bin-types-reset]');
    let scope = 'all';
    // The selected type keys. Empty means "all types" — never an enumeration of
    // everything, so a type that arrives on the next keyset page is included by
    // default rather than silently filtered out by a stale selection.
    let types = [];
    // The option rows currently rendered in the menu, keyed by type key, so a
    // rebuild after "load more" reuses the existing checkbox (and its focus)
    // instead of tearing the menu down under the pointer.
    const typeOptions = new Map();

    // The visibility scopes are client-side lenses over the loaded rows, so the
    // per-tab counts count the same loaded rows the tabs filter — both see
    // exactly the loaded page, never a total the lens can't back up. Counts
    // refresh on apply() so an HTMX "load more" keeps them honest.
    //
    // Only the narrowing lenses get their own empty copy: `all` is empty only
    // when the bin itself is, and that case never reaches here — the server
    // renders its own empty state instead of a row list. The scope vocabulary
    // lives in binScopeFromHash, the one place that has to validate it.
    const EMPTY_COPY = {
      shared: 'Nothing shared yet — everything here is private to you.',
      private: 'Nothing private — everything here has a shareable link.'
    };
    const NO_MATCH_COPY = 'No artifacts match this filter.';

    function rowType(row) { return row.dataset.type || ''; }

    // rowMatchesText and rowInScope are the two lenses OTHER than type, split
    // out because the type menu's own counts have to honour them (a facet count
    // that ignores the active filters is exactly the lie the old tabs told).
    function rowMatchesText(row) {
      const q = (filter && filter.value ? filter.value : '').trim().toLowerCase();
      return !q || (row.dataset.search || '').toLowerCase().indexOf(q) !== -1;
    }

    function rowMatches(row) {
      if (!rowInScope(row, scope)) return false;
      if (!binTypeAccepts(types, rowType(row))) return false;
      return rowMatchesText(row);
    }

    // rowInScope answers "would this row survive the visibility lens alone",
    // which is what separates "this lens is empty" from "your text/type filter
    // matched nothing".
    function rowInScope(row, s) {
      return binScopeAccepts(s, row.dataset.shared === '1');
    }

    // buildTypeMenu renders one checkbox per type present in the loaded rows,
    // with that type's count. Types come from the rows themselves, so the menu
    // can never offer a type the bin does not hold — and a "load more" that
    // brings in a new type adds its option rather than leaving it unreachable.
    // Checked state lives in `types`, not in the DOM, so a rebuild preserves it.
    //
    // The counts are FACET counts: they count only rows that pass the other
    // active lenses (visibility + text), so "markdown 2" under the Private tab
    // means two private markdown rows, not two anywhere. They deliberately
    // ignore the type selection itself — otherwise ticking one type would zero
    // every other option and you could never widen the filter. An option whose
    // facet count is 0 stays listed (and dimmed) rather than vanishing, so a
    // ticked type is always reachable to untick.
    function buildTypeMenu(rows) {
      if (!typeList || !typeBox) return;
      const present = binTypeCounts(rows.map(function (row) {
        return {
          type: rowType(row),
          name: row.dataset.typeName || rowType(row),
          eligible: rowInScope(row, scope) && rowMatchesText(row)
        };
      }));
      // A single-type bin has nothing to choose between, so the menu stays
      // hidden rather than offering a filter that can only be a no-op.
      typeBox.hidden = !binTypeMenuVisible(present.length, types.length);
      if (typeBox.hidden && typeBox.open) typeBox.open = false;

      const seen = new Set();
      present.forEach(function (t) {
        seen.add(t.type);
        let opt = typeOptions.get(t.type);
        if (!opt) {
          const label = document.createElement('label');
          label.className = 'bin-type-opt';
          const box = document.createElement('input');
          box.type = 'checkbox';
          box.value = t.type;
          const name = document.createElement('span');
          name.className = 'bin-type-name';
          const count = document.createElement('span');
          count.className = 'bin-type-count';
          label.appendChild(box);
          label.appendChild(name);
          label.appendChild(count);
          box.addEventListener('change', function () {
            types = binToggleType(types, t.type, box.checked);
            writeStateToURL();
            apply();
          });
          opt = { label: label, box: box, name: name, count: count };
          typeOptions.set(t.type, opt);
        }
        opt.name.textContent = t.name;
        opt.count.textContent = String(t.count);
        opt.box.checked = types.indexOf(t.type) !== -1;
        opt.label.classList.toggle('bin-type-opt-empty', t.count === 0);
        typeList.appendChild(opt.label);
      });
      // Drop options for types no longer loaded (only reachable if rows are
      // ever removed), so the menu never outlives its rows.
      typeOptions.forEach(function (opt, key) {
        if (!seen.has(key)) {
          if (opt.label.parentNode) opt.label.parentNode.removeChild(opt.label);
          typeOptions.delete(key);
        }
      });
      if (typeLabel) typeLabel.textContent = binTypeSummary(types, present);
      typeBox.classList.toggle('bin-types-active', types.length > 0);
      if (typeReset) typeReset.disabled = types.length === 0;
    }

    function apply() {
      const rows = Array.from(rowsBox.querySelectorAll('.bin-row'));
      buildTypeMenu(rows);
      let shown = 0;
      rows.forEach(function (row) {
        const ok = rowMatches(row);
        row.hidden = !ok;
        if (ok) shown++;
      });
      // Refresh each tab's count over the loaded rows, so a lens that is empty
      // says so before you click it. The counting itself is the exported pure
      // helper (binCountsByScope) so the node test pins the exact figures the
      // tabs show.
      const flags = rows.map(function (row) {
        return { shared: row.dataset.shared === '1' };
      });
      const counts = binCountsByScope(flags);
      tabs.forEach(function (tab) {
        const s = tab.dataset.scope || 'all';
        const n = counts[s] || 0;
        const c = tab.querySelector('[data-tab-count]');
        if (c) c.textContent = String(n);
        // data-label, never textContent: the count span is INSIDE the button,
        // so textContent already reads "All 24" and the fallback would build
        // "All 24 — 24 artifacts".
        tab.setAttribute('aria-label', (tab.dataset.label || '') + ' — ' + n + ' artifacts');
      });
      // Per-scope empty state: when the active scope has no rows of its own,
      // say which lens is empty rather than showing a bare "no match"
      // (distinct from the text/type filters' "no artifacts match" case).
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

    // The active lenses live in the URL hash (`#scope=private&type=markdown,image`)
    // so a filtered view is bookmarkable/shareable and survives reload.
    function writeStateToURL() {
      const url = window.location.pathname + window.location.search + binHashFromState(scope, types);
      if (window.history && window.history.replaceState) window.history.replaceState(null, '', url);
    }

    function selectTab(tab, updateURL) {
      scope = tab.dataset.scope || 'all';
      tabs.forEach(function (t) {
        const on = t === tab;
        t.setAttribute('aria-selected', on ? 'true' : 'false');
        t.tabIndex = on ? 0 : -1;
      });
      if (updateURL !== false) writeStateToURL();
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
    if (typeReset) {
      typeReset.addEventListener('click', function () {
        types = [];
        writeStateToURL();
        apply();
      });
    }

    // Re-apply after an HTMX "load more" swaps the next keyset page in, so the
    // active lenses also govern the newly appended rows.
    document.body.addEventListener('htmx:afterSwap', function (e) {
      if (rowsBox.contains(e.target) || e.target === rowsBox) apply();
    });

    // Restore the lenses from the URL on load, then render. A hash the user
    // edited by hand (back/forward) re-selects without re-pushing the hash.
    function readStateFromURL() {
      const hash = window.location.hash || '';
      scope = binScopeFromHash(hash);
      types = binTypesFromHash(hash);
      const t = tabs.find(function (x) { return (x.dataset.scope || 'all') === scope; });
      if (t) selectTab(t, false);
      else apply();
    }
    readStateFromURL();
    window.addEventListener('hashchange', readStateFromURL);

    apply();
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', initBin);
  } else {
    initBin();
  }
})();

// ---- Bin lens math (exported for the node unit test) ----------------------
// The DOM-free core of the Bin lenses, lifted out of the IIFE so app_test.js
// can pin visibility counts, type-menu construction, and URL-hash round-tripping
// without a browser. The in-page code calls the same functions.

// BIN_SCOPES is the visibility vocabulary, in one place: `all` (everything
// loaded), `shared` (link visibility — you plus anyone with the URL, ADR-0007),
// and `private` (only you). It is exactly the partition the artifact model
// supports, which is why the old three-overlapping-tabs arrangement could not
// filter anything.
const BIN_SCOPES = ['all', 'shared', 'private'];

// binScopeAccepts is the single definition of what each visibility lens admits,
// shared by the row filter, the per-lens counts, and the empty-state check, so
// the three can never disagree about which rows a lens holds.
function binScopeAccepts(scope, shared) {
  if (scope === 'shared') return !!shared;
  if (scope === 'private') return !shared;
  return true;
}

// binCountsByScope counts loaded rows per visibility lens from their {shared}
// flags, over exactly the rows the client-side lenses filter. `shared` and
// `private` are complements, so they always sum to `all` — the counts partition
// the bin instead of overlapping it.
function binCountsByScope(flags) {
  const counts = { all: flags.length, shared: 0, private: 0 };
  flags.forEach(function (f) {
    if (f && f.shared) counts.shared++;
    else counts.private++;
  });
  return counts;
}

// binTypeAccepts admits a row's type against the selected set. An EMPTY
// selection means "all types" rather than "none", so a type arriving on a later
// keyset page is visible by default instead of hidden by a selection made
// before it loaded.
function binTypeAccepts(selected, type) {
  return selected.length === 0 || selected.indexOf(type) !== -1;
}

// binTypeCounts collapses the loaded rows' {type, name, eligible} triples into
// the menu's option list: one entry per distinct type LOADED, carrying its
// reader-facing name, its FACET count (`count` — how many rows of that type
// survive the other active lenses) and its loaded `total`. `eligible: false`
// still contributes the option and the total (so a ticked type never disappears
// out from under the pointer) but not the facet count. Omitting `eligible`
// counts every row, which is the unfiltered case.
//
// Ordered by TOTAL descending then name — deliberately not by the facet count,
// which changes on every keystroke in the text box and would reshuffle the
// checkboxes under the pointer while the menu is open. Sorting on the total
// means the order only moves when a "load more" brings in new rows. Rows
// missing a type are skipped rather than producing a nameless option.
function binTypeCounts(rows) {
  const byType = new Map();
  rows.forEach(function (r) {
    if (!r || !r.type) return;
    const hit = r.eligible === undefined || r.eligible ? 1 : 0;
    const cur = byType.get(r.type);
    if (cur) { cur.count += hit; cur.total++; }
    else byType.set(r.type, { type: r.type, name: r.name || r.type, count: hit, total: 1 });
  });
  return Array.from(byType.values()).sort(function (a, b) {
    if (b.total !== a.total) return b.total - a.total;
    return a.name < b.name ? -1 : (a.name > b.name ? 1 : 0);
  });
}

// binToggleType adds or removes one type from the selection, keeping it free of
// duplicates. Returns a new array so callers never mutate state in place.
function binToggleType(selected, type, on) {
  const next = selected.filter(function (t) { return t !== type; });
  if (on) next.push(type);
  return next;
}

// binTypeMenuVisible decides whether the type menu is shown at all. A bin
// holding fewer than two types has nothing to choose between, so the menu
// hides rather than offering a filter that can only be a no-op — UNLESS a
// selection is already active. The menu holds the only control that can clear
// one, so hiding it while it filters strands the Bin on "No artifacts match
// this filter" with no visible cause and no way back except hand-editing the
// URL. That is reachable in normal use: `#type=code` in a bookmark or a pasted
// link outlives the code artifact it was made for (TTL expiry, deletion), and
// what it leaves behind is a bin whose remaining rows are all one other type.
// Keeping the menu up whenever it filters preserves the same invariant the
// dimmed zero-count options do — a ticked type is always reachable to untick.
function binTypeMenuVisible(presentCount, selectedCount) {
  return presentCount >= 2 || selectedCount > 0;
}

// binTypeSummary is the menu button's label: "All types" for an empty
// selection, the type's own name when exactly one is chosen (so the button says
// what you filtered to, not how many boxes you ticked), and a count beyond
// that. `present` is the option list, used only to resolve a key to its
// reader-facing name.
function binTypeSummary(selected, present) {
  if (selected.length === 0) return 'All types';
  if (selected.length === 1) {
    const hit = present.find(function (p) { return p.type === selected[0]; });
    return hit ? hit.name : selected[0];
  }
  return selected.length + ' types';
}

// binScopeFromHash reads the active visibility lens out of the URL hash
// (`#scope=private`), defaulting to 'all' for an absent, malformed, or
// out-of-vocabulary value, so a hand-edited or stale hash can never select a
// lens that doesn't exist.
function binScopeFromHash(hash) {
  const m = (hash || '').match(/(?:^|#|&)scope=([a-z]+)/);
  return (m && BIN_SCOPES.indexOf(m[1]) !== -1) ? m[1] : 'all';
}

// binTypesFromHash reads the selected types out of the URL hash
// (`#type=markdown,image`) as a comma-separated list of registry keys. Keys are
// NOT validated against a vocabulary here the way scopes are: the share-type
// registry is extensible (ADR-0002), so the list of valid keys is server data,
// not a constant the shell may hardcode. An unknown key is harmless — it simply
// matches no row — while a hardcoded allowlist would silently drop a newly
// registered type from a shared URL.
function binTypesFromHash(hash) {
  const m = (hash || '').match(/(?:^|#|&)type=([^&]*)/);
  if (!m) return [];
  const out = [];
  decodeURIComponent(m[1]).split(',').forEach(function (raw) {
    const t = raw.trim();
    if (t && out.indexOf(t) === -1) out.push(t);
  });
  return out;
}

// binHashFromState is the inverse: the hash for the current lenses, empty when
// both are at their defaults so an unfiltered Bin keeps a clean URL. Round-trips
// with binScopeFromHash/binTypesFromHash.
function binHashFromState(scope, types) {
  const parts = [];
  if (scope && scope !== 'all') parts.push('scope=' + encodeURIComponent(scope));
  if (types && types.length) parts.push('type=' + encodeURIComponent(types.join(',')));
  return parts.length ? '#' + parts.join('&') : '';
}

if (typeof module !== 'undefined' && module.exports) {
  module.exports = Object.assign(module.exports || {}, {
    binScopeAccepts: binScopeAccepts,
    binCountsByScope: binCountsByScope,
    binTypeAccepts: binTypeAccepts,
    binTypeCounts: binTypeCounts,
    binToggleType: binToggleType,
    binTypeSummary: binTypeSummary,
    binTypeMenuVisible: binTypeMenuVisible,
    binScopeFromHash: binScopeFromHash,
    binTypesFromHash: binTypesFromHash,
    binHashFromState: binHashFromState
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
