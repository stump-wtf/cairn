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
    // The freshly-swapped pane's triggers are also unhydrated (server-rendered
    // markup carries no reaction state, ADR-0002) — re-hydrate them the same
    // way the initial pane is below (#72 follow-up).
    document.body.addEventListener('htmx:afterSwap', function (e) {
      if (!e.target || e.target.id !== 'bundle-pane') return;
      var sel = rail.querySelector('[aria-selected="true"]');
      if (sel) pane.setAttribute('aria-labelledby', sel.id);
      if (focusPaneNext) { pane.focus(); focusPaneNext = false; }
      hydrateMemberReactions(viewer);
    });

    // Hydrate the initially server-rendered active pane's own reactions too
    // (not just the ones loaded after a later HTMX swap), so the "reacted"
    // marker survives a plain page reload (#72 follow-up).
    hydrateMemberReactions(viewer);
  }

  // --- member-scoped block reaction picker ----------------------------------
  //
  // Click-time feedback (issue #72, adapting the #66 treatment to the bundle
  // viewer's per-member ENGAGEMENT-COUNT model rather than markdown.js's
  // per-block PILL model): the bundle rail never shows a pill per block — it
  // shows one aggregated `.bundle-file-count` badge per member, sourced
  // server-side from memberReactionCounts. So "click-time feedback" here means
  // two things happening synchronously with the click, not on next reload: (1)
  // the trigger itself flips into a persistent "reacted" visual state (CSS in
  // bundle.css keyed off [data-reacted], which — pre-#72 — this set but never
  // styled), and (2) the owning member's rail badge count is bumped in place
  // (bumpFileEngagement), because that count is otherwise only ever correct
  // again after a full reload re-runs memberReactionCounts server-side.
  //
  // Reacting the same emoji a trigger already shows now toggles it off
  // (DELETE), matching SPEC-0006 idempotent reactions and the #66 toggle
  // convention; a different emoji simply reacts again (a bundle_file anchor
  // permits multiple emoji per actor, same as md_block). A write failure
  // routes 401 (not signed in) to /login with a return path, exactly like
  // markdown.js's handleReactError; any other failure surfaces a small inline
  // toast instead of the click silently doing nothing.
  //
  // hydrateMemberReactions ports markdown.js's loadReactions (a #66 fix) to
  // this trigger's single data-reacted slot: without it, a reload drops the
  // marker even though the server still has the viewer's reaction, so (a) the
  // "on" CSS/aria state vanishes, (b) toggle-off (DELETE) becomes unreachable
  // until the viewer reacts again in this session, and (c) that re-react hits
  // the POST branch with already=false, so the server correctly no-ops
  // (200, created=false) while the click handler still bumps the rail badge —
  // a client-only over-count that heals only on the NEXT reload. Fixing the
  // marker closes that gap at its source.

  var openPicker = null;

  function closePicker(restoreFocus) {
    if (!openPicker) return;
    var trigger = openPicker.trigger;
    openPicker.el.remove();
    openPicker = null;
    if (restoreFocus && trigger) trigger.focus();
  }

  // reactMemberFetch posts or removes (toggles) a bundle_file reaction scoped
  // to one member (+ optional block). Never throws — onDone(ok, status) always
  // fires so the caller can route a 401 (not signed in) apart from any other
  // failure (#66's contract, mirrored from markdown.js's reactionFetch).
  function reactMemberFetch(method, member, blockID, emoji, onDone) {
    var id = artifactID();
    if (!id) { onDone(false, 0); return; }
    var ref = { name: member };
    if (blockID) ref.block_id = blockID;
    fetch('/v1/artifacts/' + encodeURIComponent(id) + '/reactions', {
      method: method,
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrfToken() },
      body: JSON.stringify({ anchor_type: 'bundle_file', anchor_ref: ref, emoji: emoji })
    }).then(function (resp) {
      onDone(resp.ok, resp.status);
    }).catch(function () {
      onDone(false, 0);
    });
  }

  // handleMemberReactError is the shared failure path: a signed-out write
  // 401s → route to login with a return path (mirroring markdown.js/#66, #41);
  // any other failure surfaces a small inline toast rather than the click
  // silently appearing to do nothing.
  function handleMemberReactError(trigger, status) {
    if (status === 401) {
      window.location.href = '/login?next=' + encodeURIComponent(window.location.pathname);
      return;
    }
    var el = document.createElement('span');
    el.className = 'md-react-error';
    el.setAttribute('role', 'status');
    el.textContent = 'Could not react — try again.';
    var parent = trigger.parentNode;
    parent.insertBefore(el, trigger.nextSibling);
    setTimeout(function () { if (el.parentNode) el.remove(); }, 3500);
  }

  // baseReactLabel returns (and caches, on first touch, in data-base-label)
  // the trigger's own aria-label as server/markdown.js rendered it ("React to
  // this block" for a block trigger, "React to this item" for a md.js-injected
  // bullet trigger) so reactedLabel can restore it verbatim on toggle-off
  // instead of hardcoding one trigger kind's wording.
  function baseReactLabel(trigger) {
    var base = trigger.getAttribute('data-base-label');
    if (!base) {
      base = trigger.getAttribute('aria-label') || 'React to this block';
      trigger.setAttribute('data-base-label', base);
    }
    return base;
  }

  // cssEscape falls back to manual escaping only for engines without
  // CSS.escape (matching markdown.js's flashTOC helper).
  function cssEscape(s) {
    return (window.CSS && window.CSS.escape) ? window.CSS.escape(s) : s.replace(/[^\w-]/g, '\\$&');
  }

  // hydrateMemberReactions fetches the artifact's current per-anchor tallies
  // (the same GET markdown.js's loadReactions draws from, #66) and marks every
  // bundle_file trigger the viewer has already reacted to, so the "reacted"
  // marker — and the CSS "on" state and aria-label it drives — survives a
  // reload instead of only ever existing until the next navigation (#72
  // follow-up). Every block trigger on the page shares the same markup
  // (data-anchor-type="md_block", internal/markdown/viewer.go) regardless of
  // whether it sits in a bundle member pane, so a tally is matched back to its
  // trigger by member (the anchor_key's `name`, matched against the owning
  // pane's data-member) plus block id — not by data-anchor-type, which the
  // bundle_file tally does not carry.
  function hydrateMemberReactions(viewer) {
    var id = artifactID();
    if (!id) return;
    fetch('/v1/artifacts/' + encodeURIComponent(id) + '/reactions', { credentials: 'same-origin' })
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (data) {
        if (!data || !data.reactions) return;
        data.reactions.forEach(function (t) {
          if (t.anchor_type !== 'bundle_file' || !t.reacted) return;
          var key;
          try { key = JSON.parse(t.anchor_key); } catch (e) { return; }
          if (!key || !key.name || !key.block_id) return;
          var pane = viewer.querySelector('[data-member="' + cssEscape(key.name) + '"]');
          if (!pane) return;
          var trigger = pane.querySelector('.md-react[data-block-id="' + cssEscape(key.block_id) + '"]');
          if (!trigger) return;
          trigger.setAttribute('data-reacted', t.emoji);
          trigger.setAttribute('aria-label', baseReactLabel(trigger) + ' — reacted ' + t.emoji);
        });
      })
      .catch(function () { /* reactions are progressive enhancement; a failed fetch just leaves markers unset */ });
  }

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
      // A single, clean click path (#66, matching markdown.js/the trajectory
      // viewer's fix): close the picker WITH focus restore first, then post
      // and update the trigger + rail badge on success — no competing
      // handlers racing to tear down the popover.
      //
      // The rail badge bump is gated on the exact status the server used to
      // report whether anything actually changed (SPEC-0006 "Idempotent
      // Reactions": POST returns 201 on a real create, 200 on a no-op repeat;
      // DELETE always 204). Bumping on every ok POST — not just a 201 — is
      // exactly the drift a reload can otherwise reintroduce: the trigger's
      // marker only tracks the LAST emoji reacted, so reacting emojiA then
      // emojiB then emojiA again re-POSTs an emoji the server already has on
      // file for this viewer/anchor, the server correctly 200/no-ops it, and
      // an unconditional bump would still count it. Checking status is a
      // second, independent guard against that over-count even when the
      // marker itself (hydrateMemberReactions) is out of sync.
      b.addEventListener('click', function () {
        var already = trigger.getAttribute('data-reacted') === emoji;
        closePicker(true);
        reactMemberFetch(already ? 'DELETE' : 'POST', member, blockID, emoji, function (ok, status) {
          if (!ok) { handleMemberReactError(trigger, status); return; }
          if (already) {
            trigger.removeAttribute('data-reacted');
            trigger.setAttribute('aria-label', baseReactLabel(trigger));
            if (status === 204) bumpFileEngagement(member, -1);
          } else {
            trigger.setAttribute('data-reacted', emoji);
            trigger.setAttribute('aria-label', baseReactLabel(trigger) + ' — reacted ' + emoji);
            if (status === 201) bumpFileEngagement(member, 1);
          }
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
