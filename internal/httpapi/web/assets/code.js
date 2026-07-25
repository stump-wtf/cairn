// Code viewer progressive enhancement (SPEC-0003 REQ "Code Annotation
// Anchors", REQ "Progressive Enhancement", Accessibility REQ). The server has
// already rendered a readable, keyboard-navigable, syntax-highlighted,
// line-numbered source with a working outline; this layers on the
// interactive affordances:
//
//   - a per-line reaction picker (posts a code_line reaction to the /v1
//     annotation API) whose pill+count render immediately on pick AND persist
//     across reload (fetched once on load from GET /v1/artifacts/{id}/
//     reactions) — the same "click-time insert, reload-stable" contract
//     trajectory.js's react-pill established (and #66 fixed), applied here;
//   - a shift-click line-range selection that reacts on a code_range;
//   - a per-line 💬 button that anchors the shell's existing comment composer
//     to that line (code_line);
//   - select-text-to-comment over the highlighted source, anchoring a
//     text_selection comment exactly like markdown.js's selection flow, with
//     offsets computed over the same character stream the raw body carries
//     (see internal/code/render.go: chroma never rewrites source text, so —
//     unlike markdown's goldmark transform — no reconciliation is needed
//     between "what's on screen" and "what the body byte-for-byte contains");
//   - a SYMBOLS outline jump flash, mirroring markdown.js's TOC flash.
//
// If this script fails to load, the highlighted, line-numbered source still
// reads and the whole-artifact composer still posts, so core reading degrades
// gracefully (SPEC-0003 REQ "Progressive Enhancement"). Everything is plain
// vanilla JS from 'self' with no eval, so it needs nothing the shell CSP
// forbids.
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

  function cssEsc(s) { return (window.CSS && window.CSS.escape) ? window.CSS.escape(s) : String(s).replace(/"/g, '\\"'); }

  function requireLogin() {
    window.location.href = '/login?next=' + encodeURIComponent(window.location.pathname);
  }

  // --- reaction API ----------------------------------------------------

  function reactionFetch(method, anchorType, anchorRef, emoji) {
    var id = artifactID();
    return fetch('/v1/artifacts/' + encodeURIComponent(id) + '/reactions', {
      method: method,
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrfToken() },
      body: JSON.stringify({ anchor_type: anchorType, anchor_ref: anchorRef, emoji: emoji })
    }).then(function (resp) {
      if (resp.status === 401) { requireLogin(); return { ok: false }; }
      return { ok: resp.ok, status: resp.status };
    }).catch(function () { return { ok: false, status: 0 }; });
  }

  // annotationsFor returns (creating if absent) the .code-annotations
  // container a line's pills/comment marker append into, and reveals it.
  function annotationsFor(line) {
    var row = document.getElementById('L' + line);
    if (!row) return null;
    var box = row.querySelector('[data-code-annotations]');
    if (box) box.classList.add('code-annotations-visible');
    return box;
  }

  function findPill(box, emoji) {
    return box.querySelector('.code-react-pill[data-emoji="' + cssEsc(emoji) + '"]');
  }

  function makePill(anchorType, anchorRef, emoji, count, reacted) {
    var pill = document.createElement('button');
    pill.type = 'button';
    pill.className = 'code-react-pill' + (reacted ? ' on' : '');
    pill.dataset.emoji = emoji;
    pill.setAttribute('aria-pressed', reacted ? 'true' : 'false');
    pill.setAttribute('aria-label', 'React ' + emoji + ', ' + count + ' so far');
    pill.innerHTML = '<span class="code-react-emoji"></span> <span class="code-react-count">' + count + '</span>';
    pill.querySelector('.code-react-emoji').textContent = emoji;
    pill.addEventListener('click', function () { togglePill(anchorType, anchorRef, pill); });
    return pill;
  }

  function togglePill(anchorType, anchorRef, pill) {
    var on = pill.getAttribute('aria-pressed') === 'true';
    var emoji = pill.dataset.emoji;
    reactionFetch(on ? 'DELETE' : 'POST', anchorType, anchorRef, emoji).then(function (r) {
      if (!r.ok) return;
      var countEl = pill.querySelector('.code-react-count');
      var n = parseInt(countEl.textContent, 10) || 0;
      n = on ? Math.max(n - 1, 0) : n + 1;
      countEl.textContent = String(n);
      pill.setAttribute('aria-pressed', on ? 'false' : 'true');
      pill.setAttribute('aria-label', 'React ' + emoji + ', ' + n + ' so far');
      pill.classList.toggle('on', !on);
      if (n === 0 && on) pill.remove();
    });
  }

  // ensureRangeChip inserts the "L{start}–{end}" label a code_range reaction's
  // pill(s) sit next to, if one is not already there — shared by the
  // click-time insert (reactWithEmoji) and the load-time tally render
  // (renderRangeTally) so a range reaction looks identical whichever path
  // rendered it first.
  function ensureRangeChip(box, start, end) {
    var key = start + '-' + end;
    if (box.querySelector('.code-range-chip[data-range="' + key + '"]')) return;
    var label = document.createElement('span');
    label.className = 'code-range-chip';
    label.setAttribute('data-range', key);
    label.textContent = 'L' + start + '–' + end;
    box.appendChild(label);
  }

  // reactWithEmoji posts a first-time reaction (from the picker) and inserts
  // its pill immediately — the click-time half of the "render reliably, then
  // persist" contract.
  function reactWithEmoji(anchorType, anchorRef, emoji, box) {
    var existing = findPill(box, emoji);
    if (existing) {
      if (existing.getAttribute('aria-pressed') !== 'true') togglePill(anchorType, anchorRef, existing);
      return;
    }
    reactionFetch('POST', anchorType, anchorRef, emoji).then(function (r) {
      if (!r.ok) return;
      if (anchorType === 'code_range') ensureRangeChip(box, anchorRef.start, anchorRef.end);
      box.appendChild(makePill(anchorType, anchorRef, emoji, 1, true));
    });
  }

  // --- persisted tallies (load-time) ------------------------------------

  // loadTallies fetches the artifact's reaction tallies once on page load and
  // renders every code_line/code_range pill with its real count and the
  // viewer's own reacted state, so a pill persists across reload exactly as
  // it read before (the "reload-stable" half of the contract).
  function loadTallies() {
    var id = artifactID();
    if (!id) return;
    fetch('/v1/artifacts/' + encodeURIComponent(id) + '/reactions', { credentials: 'same-origin' })
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (data) {
        if (!data || !data.reactions) return;
        data.reactions.forEach(function (t) {
          if (t.anchor_type === 'code_line') {
            renderLineTally(t);
          } else if (t.anchor_type === 'code_range') {
            renderRangeTally(t);
          }
        });
      })
      .catch(function () {});
  }

  function parseAnchorKey(key) {
    try { return JSON.parse(key); } catch (e) { return null; }
  }

  function renderLineTally(t) {
    var loc = parseAnchorKey(t.anchor_key);
    if (!loc || !loc.line) return;
    var box = annotationsFor(loc.line);
    if (!box) return;
    box.appendChild(makePill('code_line', { line: loc.line }, t.emoji, t.count, t.reacted));
  }

  function renderRangeTally(t) {
    var loc = parseAnchorKey(t.anchor_key);
    if (!loc || !loc.start || !loc.end) return;
    var box = annotationsFor(loc.start);
    if (!box) return;
    ensureRangeChip(box, loc.start, loc.end);
    box.appendChild(makePill('code_range', { start: loc.start, end: loc.end }, t.emoji, t.count, t.reacted));
  }

  // --- reaction picker (focus-trapped popover, mirrors markdown.js) --------

  var openPicker = null;

  function closePicker(restoreFocus) {
    if (!openPicker) return;
    var trigger = openPicker.trigger;
    openPicker.el.remove();
    openPicker = null;
    if (restoreFocus && trigger) trigger.focus();
  }

  function showPicker(trigger, anchorType, anchorRef, box) {
    closePicker(false);
    var pop = document.createElement('div');
    pop.className = 'code-react-picker';
    pop.setAttribute('role', 'menu');
    pop.setAttribute('aria-label', 'Choose a reaction');

    REACTIONS.forEach(function (emoji) {
      var b = document.createElement('button');
      b.type = 'button';
      b.setAttribute('role', 'menuitem');
      b.setAttribute('aria-label', 'React ' + emoji);
      b.textContent = emoji;
      b.addEventListener('click', function () {
        reactWithEmoji(anchorType, anchorRef, emoji, box);
        closePicker(true);
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

    pop.addEventListener('keydown', function (e) {
      if (e.key === 'Escape') { e.preventDefault(); closePicker(true); return; }
      if (e.key !== 'Tab') return;
      var idx = Array.prototype.indexOf.call(items, document.activeElement);
      e.preventDefault();
      var next = e.shiftKey ? idx - 1 : idx + 1;
      if (next < 0) next = items.length - 1;
      if (next >= items.length) next = 0;
      items[next].focus();
    });
  }

  document.addEventListener('click', function (e) {
    var trigger = e.target.closest ? e.target.closest('.code-react[data-anchor-type]') : null;
    if (trigger) {
      e.preventDefault();
      var line = parseInt(trigger.getAttribute('data-line'), 10);
      var box = annotationsFor(line);
      if (openPicker && openPicker.trigger === trigger) { closePicker(true); return; }
      showPicker(trigger, 'code_line', { line: line }, box);
      return;
    }
    var rangeTrigger = e.target.closest ? e.target.closest('.code-range-react') : null;
    if (rangeTrigger) {
      e.preventDefault();
      var start = parseInt(rangeTrigger.getAttribute('data-start'), 10);
      var end = parseInt(rangeTrigger.getAttribute('data-end'), 10);
      var rbox = annotationsFor(start);
      showPicker(rangeTrigger, 'code_range', { start: start, end: end }, rbox);
      clearRangeSelection();
      return;
    }
    if (openPicker && !e.target.closest('.code-react-picker')) closePicker(false);
  });

  // --- code_range: shift-click a second line number to select a range -----

  var rangeAnchor = null;

  function clearRangeSelection() {
    document.querySelectorAll('.code-row.code-range-selecting').forEach(function (r) {
      r.classList.remove('code-range-selecting');
    });
    var chip = document.querySelector('.code-range-react');
    if (chip) chip.remove();
    rangeAnchor = null;
  }

  function wireRangeSelection(viewer) {
    viewer.addEventListener('click', function (e) {
      var link = e.target.closest ? e.target.closest('.code-lineno-link') : null;
      if (!link) return;
      var line = parseInt(link.closest('.code-row').getAttribute('data-line'), 10);
      if (!e.shiftKey || rangeAnchor === null) {
        clearRangeSelection();
        if (e.shiftKey) return; // shift-click with no prior anchor: nothing to range from
        rangeAnchor = line;
        return;
      }
      e.preventDefault();
      var start = Math.min(rangeAnchor, line);
      var end = Math.max(rangeAnchor, line);
      clearRangeSelection();
      if (start === end) { rangeAnchor = start; return; }
      for (var n = start; n <= end; n++) {
        var row = document.getElementById('L' + n);
        if (row) row.classList.add('code-range-selecting');
      }
      var endRow = document.getElementById('L' + end);
      if (endRow) {
        var chip = document.createElement('button');
        chip.type = 'button';
        chip.className = 'code-range-react';
        chip.dataset.start = String(start);
        chip.dataset.end = String(end);
        chip.setAttribute('aria-label', 'React to lines ' + start + ' through ' + end);
        chip.textContent = '＋ L' + start + '–' + end;
        endRow.querySelector('.code-line-cell').appendChild(chip);
      }
      rangeAnchor = null;
    });
  }

  // --- comment on a line ---------------------------------------------------

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

  function clearHidden(form, name) {
    var input = form.querySelector('input[name="' + name + '"]');
    if (input) input.value = '';
  }

  function startLineComment(line) {
    var form = document.querySelector('.comment-composer');
    var textarea = document.getElementById('comment-body');
    if (!form || !textarea) return;
    ['sel_start', 'sel_end', 'quote'].forEach(function (n) { clearHidden(form, n); });
    setHidden(form, 'line', String(line));

    var chip = document.getElementById('code-line-chip');
    if (!chip) {
      chip = document.createElement('p');
      chip.id = 'code-line-chip';
      chip.className = 'code-stats';
      form.insertBefore(chip, form.firstChild);
    }
    chip.textContent = 'commenting on line ' + line;
    textarea.focus();
  }

  function resetComposerAnchor() {
    var form = document.querySelector('.comment-composer');
    if (!form) return;
    ['sel_start', 'sel_end', 'quote', 'line'].forEach(function (n) { clearHidden(form, n); });
    var chip = document.getElementById('code-line-chip');
    if (chip) chip.remove();
  }

  // --- select-text-to-comment ------------------------------------------

  var selToolbar = null;

  function clearSelToolbar() {
    if (selToolbar) { selToolbar.remove(); selToolbar = null; }
  }

  // codeOffset maps a (node, offset) inside a `.code-line-code` span to a
  // character offset over the WHOLE source, walking rows in document order
  // and adding one synthetic "\n" between consecutive rows (render.go strips
  // each rendered line's trailing newline so it never visually wraps inside
  // its table cell — see trimLineEnding — so the DOM text stream is exactly
  // the raw source with those newlines removed, and this reconstructs them).
  function codeOffset(table, node, offset) {
    var rows = table.querySelectorAll('.code-row');
    var count = 0;
    for (var i = 0; i < rows.length; i++) {
      if (i > 0) count += 1;
      var span = rows[i].querySelector('.code-line-code');
      if (!span) continue;
      var walker = document.createTreeWalker(span, NodeFilter.SHOW_TEXT, null);
      var cur;
      while ((cur = walker.nextNode())) {
        if (cur === node) return count + offset;
        count += cur.nodeValue.length;
      }
    }
    return count;
  }

  function onSelection() {
    var sel = window.getSelection();
    if (!sel || sel.isCollapsed || sel.rangeCount === 0) { clearSelToolbar(); return; }
    var table = document.querySelector('[data-code-table]');
    if (!table) return;
    var range = sel.getRangeAt(0);
    if (!table.contains(range.commonAncestorContainer)) { clearSelToolbar(); return; }
    var quote = sel.toString();
    if (!quote.trim()) { clearSelToolbar(); return; }

    var start = codeOffset(table, range.startContainer, range.startOffset);
    var end = start + quote.length;

    clearSelToolbar();
    selToolbar = document.createElement('div');
    selToolbar.className = 'code-sel-toolbar';
    var btn = document.createElement('button');
    btn.type = 'button';
    btn.textContent = '💬 Comment';
    btn.setAttribute('aria-label', 'Comment on the selected text');
    btn.addEventListener('click', function () { startSelectionComment(start, end, quote); });
    selToolbar.appendChild(btn);
    document.body.appendChild(selToolbar);
    var r = range.getBoundingClientRect();
    selToolbar.style.top = (window.scrollY + r.top - 42) + 'px';
    selToolbar.style.left = (window.scrollX + r.left) + 'px';
  }

  function startSelectionComment(start, end, quote) {
    var form = document.querySelector('.comment-composer');
    var textarea = document.getElementById('comment-body');
    if (!form || !textarea) { clearSelToolbar(); return; }
    clearHidden(form, 'line');
    setHidden(form, 'sel_start', String(start));
    setHidden(form, 'sel_end', String(end));
    setHidden(form, 'quote', quote);

    var chip = document.getElementById('code-line-chip');
    if (!chip) {
      chip = document.createElement('p');
      chip.id = 'code-line-chip';
      chip.className = 'code-stats';
      form.insertBefore(chip, form.firstChild);
    }
    var shown = quote.length > 34 ? quote.slice(0, 34) + '…' : quote;
    chip.textContent = 'commenting on “' + shown + '”';
    clearSelToolbar();
    textarea.focus();
  }

  // --- outline jump flash -------------------------------------------------

  function flashLine() {
    var hash = window.location.hash.slice(1);
    if (!hash) return;
    document.querySelectorAll('.code-row.code-line-active').forEach(function (r) {
      r.classList.remove('code-line-active');
    });
    var row = document.getElementById(hash);
    if (row && /^L\d+$/.test(hash)) row.classList.add('code-line-active');
  }

  // --- init -----------------------------------------------------------------

  function init() {
    var viewer = document.querySelector('.code-viewer');
    if (!viewer) return;

    document.querySelectorAll('.code-comment-btn').forEach(function (btn) {
      btn.addEventListener('click', function (e) {
        e.preventDefault();
        startLineComment(parseInt(btn.getAttribute('data-line'), 10));
      });
    });

    wireRangeSelection(viewer);

    document.addEventListener('mouseup', onSelection);
    document.addEventListener('keyup', function (e) {
      if (e.shiftKey || e.key === 'Shift') onSelection();
    });

    window.addEventListener('hashchange', flashLine);
    document.body.addEventListener('htmx:afterOnLoad', function (e) {
      if (e.target && e.target.closest && e.target.closest('.comment-composer')) {
        resetComposerAnchor();
      }
    });

    flashLine();
    loadTallies();
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
