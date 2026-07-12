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
//   - member-scoped block reactions: a `＋` inside a member pane posts a
//     bundle_file reaction (carrying the member name + block id) so it resolves
//     to that member, not the bundle — intercepted in the capture phase so
//     markdown.js's md_block handler (an anchor the bundle forbids) never runs.
//
// If this script fails to load the rail links still navigate (full render) and
// the whole-bundle composer still posts, so core reading degrades gracefully.
// Everything is plain vanilla JS from 'self' with no eval, within the shell CSP.
(function () {
  'use strict';

  var REACTIONS = ['👍', '🎉', '👀', '🚀', '❤️', '🤔'];

  function artifactID() {
    var seg = window.location.pathname.replace(/^\/+/, '').split('/');
    return seg[0] || '';
  }

  function csrfToken() {
    var m = document.cookie.match(/(?:^|;\s*)cairn_csrf=([^;]+)/);
    return m ? decodeURIComponent(m[1]) : '';
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
    document.body.addEventListener('htmx:afterSwap', function (e) {
      if (!e.target || e.target.id !== 'bundle-pane') return;
      var sel = rail.querySelector('[aria-selected="true"]');
      if (sel) pane.setAttribute('aria-labelledby', sel.id);
      if (focusPaneNext) { pane.focus(); focusPaneNext = false; }
    });
  }

  // --- member-scoped block reaction picker ----------------------------------

  var openPicker = null;

  function closePicker(restoreFocus) {
    if (!openPicker) return;
    var trigger = openPicker.trigger;
    openPicker.el.remove();
    openPicker = null;
    if (restoreFocus && trigger) trigger.focus();
  }

  function postMemberReaction(member, blockID, emoji, onDone) {
    var id = artifactID();
    if (!id) { onDone(false); return; }
    var ref = { name: member };
    if (blockID) ref.block_id = blockID;
    fetch('/v1/artifacts/' + encodeURIComponent(id) + '/reactions', {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrfToken() },
      body: JSON.stringify({ anchor_type: 'bundle_file', anchor_ref: ref, emoji: emoji })
    }).then(function (resp) { onDone(resp.ok); }).catch(function () { onDone(false); });
  }

  function showMemberPicker(trigger, member) {
    closePicker(false);
    var blockID = trigger.getAttribute('data-block-id') || '';
    var pop = document.createElement('div');
    pop.className = 'md-sel-toolbar md-react-picker';
    pop.setAttribute('role', 'menu');
    pop.setAttribute('aria-label', 'Choose a reaction');

    REACTIONS.forEach(function (emoji) {
      var b = document.createElement('button');
      b.type = 'button';
      b.setAttribute('role', 'menuitem');
      b.setAttribute('aria-label', 'React ' + emoji);
      b.textContent = emoji;
      b.addEventListener('click', function () {
        postMemberReaction(member, blockID, emoji, function (ok) {
          if (ok) trigger.setAttribute('data-reacted', emoji);
          closePicker(true);
        });
      });
      pop.appendChild(b);
    });

    document.body.appendChild(pop);
    var r = trigger.getBoundingClientRect();
    pop.style.top = (window.scrollY + r.bottom + 6) + 'px';
    pop.style.left = (window.scrollX + r.left) + 'px';

    openPicker = { el: pop, trigger: trigger };
    var items = pop.querySelectorAll('button');
    if (items.length) items[0].focus();

    // Focus trap: Tab/Shift+Tab cycle within, Escape closes and restores focus.
    pop.addEventListener('keydown', function (e) {
      if (e.key === 'Escape') { e.preventDefault(); closePicker(true); return; }
      if (e.key !== 'Tab') return;
      var idx = Array.prototype.indexOf.call(items, document.activeElement);
      e.preventDefault();
      var nxt = e.shiftKey ? idx - 1 : idx + 1;
      if (nxt < 0) nxt = items.length - 1;
      if (nxt >= items.length) nxt = 0;
      items[nxt].focus();
    });
  }

  // Capture-phase interception: a `＋` inside a member pane posts a member-scoped
  // bundle_file reaction. Stopping propagation here keeps markdown.js's bubble
  // handler (which would post an md_block anchor the bundle rejects) from firing.
  document.addEventListener('click', function (e) {
    var trigger = e.target.closest ? e.target.closest('.md-react') : null;
    if (!trigger) {
      if (openPicker && !(e.target.closest && e.target.closest('.md-react-picker'))) {
        closePicker(false);
      }
      return;
    }
    var member = trigger.closest('[data-member]');
    if (!member) return; // not a bundle pane — leave markdown.js to handle it
    e.preventDefault();
    e.stopPropagation();
    if (openPicker && openPicker.trigger === trigger) {
      closePicker(true);
    } else {
      showMemberPicker(trigger, member.getAttribute('data-member'));
    }
  }, true);

  function init() { initRail(); }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
