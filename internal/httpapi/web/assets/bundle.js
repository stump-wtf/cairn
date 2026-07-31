// Bundle viewer progressive enhancement (SPEC-0003 REQ "Bundle Viewer",
// Accessibility REQ "Keyboard Navigation & Focus Management", issue #19). The
// server has already rendered a readable file rail whose items are real links to
// `/{id}?file=<name>` — so a no-JS reader browses member to member with a full
// render — plus the active member's pane. This layers on:
//
//   - roving-tabindex keyboard nav of the rail (a vertical tablist): arrows move
//     focus between files, Home/End jump to ends, Enter/Space (native <a>
//     activation) select — manual activation, exactly the pattern the spec's
//     keyboard scenario describes;
//   - HTMX pane swaps (the links carry hx-get) with focus moved into the freshly
//     loaded pane and the panel relabelled by the active file; and
//   - the file rail's per-member engagement badge, kept current at click time as
//     reactions land inside the pane.
//
// Reactions THEMSELVES are not implemented here. A markdown member renders the
// same viewer fragment as a standalone markdown artifact, so markdown.js owns
// every block/bullet affordance on both surfaces and simply derives a
// member-scoped bundle_file anchor when its viewer sits inside a member pane
// (see anchorRefFor there). This file used to carry a second, parallel reaction
// implementation for the in-bundle case; it drifted from the one beside it —
// dropping the bullet path so every bullet of a list collapsed onto its block,
// and rendering no pills at all, so a reaction posted, counted, and then showed
// the reader nothing. One implementation cannot drift from itself.
//
// If this script fails to load the rail links still navigate (full render) and
// the whole-bundle composer still posts, so core reading degrades gracefully.
// Everything is plain vanilla JS from 'self' with no eval, within the shell CSP.
(function () {
  'use strict';

  // cssEscape falls back to manual escaping only for engines without
  // CSS.escape (matching markdown.js's flashTOC helper).
  function cssEscape(s) {
    return (window.CSS && window.CSS.escape) ? window.CSS.escape(s) : s.replace(/[^\w-]/g, '\\$&');
  }

  // --- file rail (vertical tablist, manual activation) ----------------------

  function initRail() {
    var viewer = document.querySelector('[data-bundle-viewer]');
    if (!viewer) return;
    var rail = viewer.querySelector('[role="tablist"]');
    var pane = document.getElementById('bundle-pane');
    if (!rail || !pane) return;
    var focusPaneNext = false;

    function tabs() {
      return Array.prototype.slice.call(rail.querySelectorAll('[role="tab"]'));
    }

    function select(tab) {
      tabs().forEach(function (t) {
        var on = t === tab;
        t.setAttribute('aria-selected', on ? 'true' : 'false');
        t.classList.toggle('bundle-file-active', on);
        t.tabIndex = on ? 0 : -1;
      });
      pane.setAttribute('aria-labelledby', tab.id);
      var fileInput = document.querySelector('.comment-composer [data-bundle-file]');
      if (fileInput) fileInput.value = tab.getAttribute('data-file-name') || '';
    }

    rail.addEventListener('keydown', function (e) {
      var list = tabs();
      var i = list.indexOf(document.activeElement);
      if (i < 0) return;
      // Space (and Enter) MUST activate the focused tab. On an <a> element the
      // native click only fires on Enter — Space scrolls the page instead — so
      // we activate explicitly and swallow the default scroll.
      // Governing: SPEC-0003 Accessibility REQ "Keyboard Navigation & Focus
      // Management" (Enter/Space MUST activate a tab).
      if (e.key === ' ' || e.key === 'Spacebar' || e.key === 'Enter') {
        e.preventDefault();
        list[i].click();
        return;
      }
      var next = -1;
      if (e.key === 'ArrowDown' || e.key === 'ArrowRight') next = (i + 1) % list.length;
      else if (e.key === 'ArrowUp' || e.key === 'ArrowLeft') next = (i - 1 + list.length) % list.length;
      else if (e.key === 'Home') next = 0;
      else if (e.key === 'End') next = list.length - 1;
      else return;
      e.preventDefault();
      list.forEach(function (t) { t.tabIndex = (t === list[next]) ? 0 : -1; });
      list[next].focus();
    });

    rail.addEventListener('click', function (e) {
      var tab = e.target.closest ? e.target.closest('[role="tab"]') : null;
      if (!tab) return;
      select(tab);
      focusPaneNext = true;
      // htmx (present) swaps only the pane; without it the href navigates.
    });

    // After htmx swaps the pane in, relabel it by the active file and move focus
    // into it so a keyboard/AT user lands on the freshly loaded content
    // (predictable focus on switch, SPEC-0003 a11y). aria-live already announces.
    // The swapped-in pane's own reaction state is re-hydrated by markdown.js,
    // which listens for the same swap.
    document.body.addEventListener('htmx:afterSwap', function (e) {
      if (!e.target || e.target.id !== 'bundle-pane') return;
      var sel = rail.querySelector('[aria-selected="true"]');
      if (sel) pane.setAttribute('aria-labelledby', sel.id);
      if (focusPaneNext) { pane.focus(); focusPaneNext = false; }
    });
  }

  // --- per-member engagement badge ------------------------------------------

  // bumpFileEngagement adjusts the file rail's per-member engagement badge in
  // place by `delta` reactions, so the count the rail shows updates at
  // click-time instead of only after a reload re-runs memberReactionCounts
  // server-side (#72). data-reactions/data-comments (rendered by shell.html
  // alongside the badge) are the running per-member counters this reads and
  // rewrites; the badge itself is created once engagement goes above zero and
  // removed once it returns to zero, mirroring the server's own
  // {{if gt .Engagement 0}} gate.
  function bumpFileEngagement(member, delta) {
    var row = document.querySelector('.bundle-rail [data-file-name="' + cssEscape(member) + '"]');
    if (!row) return;
    var reactions = Math.max((parseInt(row.getAttribute('data-reactions'), 10) || 0) + delta, 0);
    row.setAttribute('data-reactions', String(reactions));
    var comments = parseInt(row.getAttribute('data-comments'), 10) || 0;
    var engagement = reactions + comments;
    var meta = row.querySelector('.bundle-file-meta');
    var badge = row.querySelector('.bundle-file-count');
    if (engagement > 0) {
      if (!badge && meta) {
        badge = document.createElement('span');
        badge.className = 'bundle-file-count';
        meta.appendChild(badge);
      }
      if (badge) {
        badge.textContent = String(engagement);
        badge.title = reactions + ' reactions · ' + comments + ' comments';
        badge.setAttribute('aria-label', engagement + ' annotations');
      }
    } else if (badge) {
      badge.remove();
    }
  }

  // markdown.js announces every reaction it lands inside a member pane, already
  // gated on the status the server used to report whether anything actually
  // changed, so a repeat react never over-counts the badge.
  document.addEventListener('cairn:member-reaction', function (e) {
    if (!e.detail || !e.detail.member || !e.detail.delta) return;
    bumpFileEngagement(e.detail.member, e.detail.delta);
  });

  function init() { initRail(); }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
