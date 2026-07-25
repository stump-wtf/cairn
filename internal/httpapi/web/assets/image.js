// Image viewer progressive enhancement (SPEC-0003 REQ "Image Viewer", REQ
// "Image Annotation Anchors", REQ "Progressive Enhancement", #69). The server
// has already rendered the image itself (internal/imageview, via the registry
// BodyViewer capability) and the whole-artifact comment thread, including
// every image_region comment's normalized pin coordinates as
// data-pin-x/data-pin-y attributes on its `.comment` card (SPEC-0006). This
// script layers on:
//
//   - the bespoke SVG pin overlay: hydrate existing pins straight from the
//     already-rendered comment thread (no extra round-trip), let a reader
//     drop a new pin by clicking (or, keyboard, activating) the image, and
//     hand its normalized {x,y} to the shell's whole-artifact comment
//     composer (mirroring markdown.js's select-text-to-comment hidden-field
//     handoff — setHidden/resetComposerAnchor's shape, renamed for pins);
//   - clicking (or activating) an existing pin marker scrolls its comment
//     into view in the panel and flashes it;
//   - the whole-image react-below cluster: fetch the artifact's reaction
//     tallies once (GET /v1/artifacts/{id}/reactions — the same JSON surface
//     trajectory.js/markdown.js already read), render a persisted pill+count
//     per emoji, and wire a SINGLE clean picker-button click path (pick →
//     close the picker with focus restore → POST → update the pill in
//     place). This is deliberately not the double-handler shape #66 found in
//     trajectory.js's picker — this is new code for a new viewer, so it
//     starts from the fixed shape instead of copying the bug.
//
// If this script fails to load, the image itself still renders (a plain
// same-origin <img src>, SPEC-0003 Scenario "Pin overlay unavailable") and the
// whole-artifact comment composer is pure HTMX (needs no image.js), so a
// reader can still comment; only the interactive pin-drop and the reaction
// "+" affordance go inert, and the reaction total is still visible with no JS
// at all via the MetadataPanel "reactions" field (internal/sharetype/
// builtin.go). Everything below is plain vanilla JS from 'self' with no eval,
// so it needs nothing the shell CSP forbids (reading cookies/DOM attributes
// and calling fetch to same-origin are both allowed).
(function () {
  'use strict';

  var REACTIONS = ['👍', '🎉', '👀', '🚀', '❤️', '🤔', '🔥'];

  function csrfToken() {
    var m = document.cookie.match(/(?:^|;\s*)cairn_csrf=([^;]+)/);
    return m ? decodeURIComponent(m[1]) : '';
  }

  // A signed-out click on an annotation control routes to login instead of
  // silently no-oping (the write endpoints would 401 anyway); ?next returns
  // the reader to this artifact once the session exists (matches markdown.js
  // and trajectory.js's requireLogin).
  function requireLogin() {
    window.location.href = '/login?next=' + encodeURIComponent(window.location.pathname);
  }

  function prefersReduced() {
    return window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches;
  }

  function cssEsc(s) {
    return (window.CSS && window.CSS.escape) ? window.CSS.escape(s) : String(s).replace(/"/g, '\\"');
  }

  // --- pin overlay ------------------------------------------------------

  function clampUnit(v) { return v < 0 ? 0 : (v > 1 ? 1 : v); }

  // placeMarker renders one pin glyph (a bespoke inline SVG teardrop,
  // ADR-0011) at the layer's normalized (x,y), positioned via CSSOM (not an
  // inline style attribute in markup, so it needs nothing the shell's
  // style-src 'self' CSP forbids). A committed pin (commentID set) is a real
  // <button> so a keyboard/AT user can jump to its thread (WCAG 2.1.1); a
  // pending, uncommitted drop is not yet interactive.
  function placeMarker(layer, x, y, commentID) {
    var el = document.createElement(commentID ? 'button' : 'div');
    if (commentID) el.type = 'button';
    el.className = 'img-pin' + (commentID ? '' : ' img-pin-pending');
    el.style.left = (x * 100) + '%';
    el.style.top = (y * 100) + '%';
    el.innerHTML = '<svg viewBox="0 0 20 26" width="18" height="24" aria-hidden="true">' +
      '<path d="M10 0C4.5 0 0 4.4 0 9.8 0 16.9 10 26 10 26s10-9.1 10-16.2C20 4.4 15.5 0 10 0z"/>' +
      '<circle cx="10" cy="9.7" r="3.4" class="img-pin-hole"></circle></svg>';
    if (commentID) {
      el.setAttribute('aria-label', 'Jump to the comment pinned here');
      el.addEventListener('click', function (e) {
        e.stopPropagation();
        jumpToComment(commentID);
      });
    } else {
      el.setAttribute('aria-hidden', 'true');
    }
    layer.appendChild(el);
    return el;
  }

  function ensurePanelOpen() {
    var panel = document.querySelector('.panel');
    var toggle = document.querySelector('.panel-toggle');
    if (panel && toggle && panel.offsetParent === null) toggle.click();
  }

  function jumpToComment(id) {
    ensurePanelOpen();
    var el = document.getElementById('comment-' + id);
    if (!el) return;
    el.scrollIntoView({ block: 'center', behavior: prefersReduced() ? 'auto' : 'smooth' });
    el.classList.remove('comment-flash');
    void el.offsetWidth; // restart the animation
    el.classList.add('comment-flash');
    setTimeout(function () { el.classList.remove('comment-flash'); }, 900);
  }

  // hydratePins (re-)renders a marker for every image_region comment already
  // in the server-rendered panel thread. Re-run after an HTMX append so a
  // freshly-posted pin appears immediately with no reload (SPEC-0003 Scenario
  // "Drop a pin to comment").
  function hydratePins(layer) {
    layer.innerHTML = '';
    document.querySelectorAll('#comment-list .comment[data-pin-x]').forEach(function (card) {
      var x = parseFloat(card.getAttribute('data-pin-x'));
      var y = parseFloat(card.getAttribute('data-pin-y'));
      if (isNaN(x) || isNaN(y)) return;
      var m = /^comment-(\d+)$/.exec(card.id || '');
      placeMarker(layer, x, y, m ? m[1] : null);
    });
  }

  var pending = null;

  function clearPending() {
    if (pending) { pending.remove(); pending = null; }
  }

  function canWrite() {
    return document.querySelector('.comment-composer') !== null;
  }

  function dropPinAt(layer, x, y) {
    if (!canWrite()) { requireLogin(); return; }
    clearPending();
    pending = placeMarker(layer, clampUnit(x), clampUnit(y), null);
    startPinComment(clampUnit(x), clampUnit(y));
  }

  function startPinComment(x, y) {
    var form = document.querySelector('.comment-composer');
    var textarea = document.getElementById('comment-body');
    if (!form || !textarea) { clearPending(); return; }
    setHidden(form, 'pin_x', String(x));
    setHidden(form, 'pin_y', String(y));
    var chip = document.getElementById('img-pin-chip');
    if (!chip) {
      chip = document.createElement('p');
      chip.id = 'img-pin-chip';
      chip.className = 'img-pin-chip';
      form.insertBefore(chip, form.firstChild);
    }
    chip.textContent = 'commenting on a pin at ' + Math.round(x * 100) + '%, ' + Math.round(y * 100) + '%';
    textarea.focus();
  }

  function setHidden(form, name, value) {
    var input = form.querySelector('input[name="' + name + '"]');
    if (!input) {
      input = document.createElement('input');
      input.type = 'hidden';
      input.name = name;
      form.appendChild(input);
    }
    input.value = value;
  }

  // resetPinComposer clears the pending-pin state after ANY comment post
  // (pinned or not — a reader may cancel a pin by posting a plain
  // whole-artifact comment instead), matching markdown.js's
  // resetComposerAnchor.
  function resetPinComposer() {
    var form = document.querySelector('.comment-composer');
    if (form) {
      ['pin_x', 'pin_y'].forEach(function (n) {
        var i = form.querySelector('input[name="' + n + '"]');
        if (i) i.value = '';
      });
    }
    var chip = document.getElementById('img-pin-chip');
    if (chip) chip.remove();
    clearPending();
  }

  // --- whole-image react-below cluster -----------------------------------

  function reactionFetch(id, method, emoji) {
    return fetch('/v1/artifacts/' + encodeURIComponent(id) + '/reactions', {
      method: method,
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrfToken() },
      body: JSON.stringify({ anchor_type: 'artifact', anchor_ref: {}, emoji: emoji })
    }).then(function (r) { return { ok: r.ok, status: r.status }; })
      .catch(function () { return { ok: false, status: 0 }; });
  }

  function makePill(cluster, emoji, count, reacted, id) {
    var add = cluster.querySelector('[data-img-react-add]');
    var pill = document.createElement('button');
    pill.type = 'button';
    pill.className = 'img-react-pill' + (reacted ? ' on' : '');
    pill.dataset.emoji = emoji;
    pill.setAttribute('aria-pressed', reacted ? 'true' : 'false');
    pill.setAttribute('aria-label', 'React ' + emoji + ', ' + count + ' so far');
    var glyph = document.createElement('span');
    glyph.className = 'img-react-emoji';
    glyph.textContent = emoji;
    var counter = document.createElement('span');
    counter.className = 'img-react-count';
    counter.textContent = String(count);
    pill.appendChild(glyph);
    pill.appendChild(document.createTextNode(' '));
    pill.appendChild(counter);
    pill.addEventListener('click', function () { togglePill(id, cluster, pill); });
    cluster.insertBefore(pill, add);
    return pill;
  }

  function togglePill(id, cluster, pill) {
    var on = pill.getAttribute('aria-pressed') === 'true';
    var emoji = pill.dataset.emoji;
    reactionFetch(id, on ? 'DELETE' : 'POST', emoji).then(function (res) {
      if (res.status === 401) { requireLogin(); return; }
      if (!res.ok) return;
      var countEl = pill.querySelector('.img-react-count');
      var n = parseInt(countEl.textContent, 10) || 0;
      n = on ? Math.max(n - 1, 0) : n + 1;
      if (n === 0 && on) { pill.remove(); return; }
      countEl.textContent = String(n);
      pill.setAttribute('aria-pressed', on ? 'false' : 'true');
      pill.classList.toggle('on', !on);
      pill.setAttribute('aria-label', 'React ' + emoji + ', ' + n + ' so far');
    });
  }

  function reactFor(id, cluster, emoji) {
    var existing = cluster.querySelector('.img-react-pill[data-emoji="' + cssEsc(emoji) + '"]');
    if (existing) {
      if (existing.getAttribute('aria-pressed') !== 'true') togglePill(id, cluster, existing);
      return;
    }
    reactionFetch(id, 'POST', emoji).then(function (res) {
      if (res.status === 401) { requireLogin(); return; }
      if (!res.ok) return;
      makePill(cluster, emoji, 1, true, id);
    });
  }

  // openPicker is a focus-trapped popover with ONE click path per button:
  // close the picker (restoring focus to the trigger) then react. Unlike
  // trajectory.js's picker (#66), no second listener is layered on
  // afterward — this IS the single, clean path from the start.
  function openPicker(id, cluster, add) {
    var open = document.querySelector('.img-react-picker');
    if (open) open.remove();
    var pick = document.createElement('div');
    pick.className = 'img-react-picker';
    pick.setAttribute('role', 'menu');
    pick.setAttribute('aria-label', 'Choose a reaction');

    var buttons = REACTIONS.map(function (emoji) {
      var b = document.createElement('button');
      b.type = 'button';
      b.setAttribute('role', 'menuitem');
      b.setAttribute('aria-label', 'React ' + emoji);
      b.textContent = emoji;
      pick.appendChild(b);
      return b;
    });

    document.body.appendChild(pick);
    var r = add.getBoundingClientRect();
    pick.style.left = (window.scrollX + r.left) + 'px';
    pick.style.top = (window.scrollY + r.bottom + 6) + 'px';

    var closed = false;
    function close(restoreFocus) {
      if (closed) return;
      closed = true;
      pick.remove();
      document.removeEventListener('click', onDocClick, true);
      pick.removeEventListener('keydown', onKeydown);
      if (restoreFocus && document.contains(add)) add.focus();
    }
    function onDocClick(e) {
      if (!pick.contains(e.target) && e.target !== add) close(false);
    }
    function onKeydown(e) {
      if (e.key === 'Escape') { e.preventDefault(); close(true); return; }
      if (e.key !== 'Tab' || buttons.length === 0) return;
      var first = buttons[0];
      var last = buttons[buttons.length - 1];
      if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
      else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
    }
    buttons.forEach(function (b) {
      b.addEventListener('click', function () {
        var emoji = b.textContent;
        close(true);
        reactFor(id, cluster, emoji);
      });
    });
    pick.addEventListener('keydown', onKeydown);
    if (buttons.length) buttons[0].focus();
    // Defer the outside-click listener so the click that opened the picker
    // does not immediately close it. Capture phase so it fires before other
    // document handlers (matches trajectory.js's openPicker/markdown.js's
    // showPicker).
    setTimeout(function () { document.addEventListener('click', onDocClick, true); }, 0);
  }

  // hydrateReactions fetches the artifact's reaction tallies once and renders
  // the persisted pills+counts (SPEC-0003: "Whole-image reactions … render as
  // pills with counts and persist", #66/#69).
  function hydrateReactions(id, cluster) {
    fetch('/v1/artifacts/' + encodeURIComponent(id) + '/reactions', { credentials: 'same-origin' })
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (data) {
        if (!data || !data.reactions) return;
        data.reactions.forEach(function (t) {
          if (t.anchor_type !== 'artifact') return;
          makePill(cluster, t.emoji, t.count, t.reacted, id);
        });
      })
      .catch(function () { /* the picker still works from zero; nothing to surface */ });
  }

  // --- init -----------------------------------------------------------------

  function init() {
    var viewer = document.querySelector('[data-img-viewer]');
    if (!viewer) return;
    var id = viewer.getAttribute('data-artifact-id') || '';
    var frame = viewer.querySelector('[data-img-frame]');
    var layer = viewer.querySelector('[data-img-pins]');
    var cluster = viewer.querySelector('[data-img-react-cluster]');
    var add = viewer.querySelector('[data-img-react-add]');

    if (layer) hydratePins(layer);
    if (cluster && id) hydrateReactions(id, cluster);

    if (frame && layer) {
      frame.addEventListener('click', function (e) {
        if (e.target.closest && e.target.closest('.img-pin')) return;
        var img = frame.querySelector('.img-body');
        if (!img) return;
        var r = img.getBoundingClientRect();
        dropPinAt(layer, (e.clientX - r.left) / r.width, (e.clientY - r.top) / r.height);
      });
      // Keyboard activation (Enter/Space) drops a pin at the frame's center —
      // a reader without a pointer can still comment on the image, even
      // without pixel-level placement (WCAG 2.1.1 Keyboard).
      frame.addEventListener('keydown', function (e) {
        if (e.target !== frame) return;
        if (e.key !== 'Enter' && e.key !== ' ') return;
        e.preventDefault();
        dropPinAt(layer, 0.5, 0.5);
      });
    }

    if (add && cluster) {
      add.addEventListener('click', function () {
        if (!canWrite()) { requireLogin(); return; }
        openPicker(id, cluster, add);
      });
    }

    // A successful comment post (the shell's HTMX form) clears any pending
    // pin state and re-hydrates the pin layer, so a fresh pin appears
    // immediately (SPEC-0003 Scenario "Drop a pin to comment") and a
    // cancelled/plain post leaves no stray pending marker behind.
    document.body.addEventListener('htmx:afterOnLoad', function (e) {
      if (e.target && e.target.closest && e.target.closest('.comment-composer')) {
        resetPinComposer();
        if (layer) hydratePins(layer);
      }
    });
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
