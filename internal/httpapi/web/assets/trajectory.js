// Trajectory viewer behaviors (ADR-0011 "bespoke vanilla-JS/SVG widget"): the
// span waterfall geometry, the span↔stream cross-highlight, reaction/comment
// pickers, lazy span-output fetch, and the live SSE span stream. Plain vanilla
// JS — no eval, no inline expressions — so it runs under the shell's
// 'self'-only CSP (bar geometry is applied via CSSOM setters, which CSP
// style-src does not intercept, never via inline style attributes).
//
// Everything here is progressive enhancement: the server already rendered the
// waterfall rows, the full activity stream, the run stats, and the comment
// thread, so the page is complete and screen-reader operable with JS disabled.
(function () {
  'use strict';

  function csrf() {
    var m = document.cookie.match(/(?:^|;\s*)cairn_csrf=([^;]+)/);
    return m ? decodeURIComponent(m[1]) : '';
  }

  // ---- Waterfall geometry ------------------------------------------------
  // Lay out every bar's left/width as a fraction of the wall time, derived as
  // max(start+duration) across all bars. Re-run after a live span append so a
  // span that extends the timeline re-proportions the whole waterfall.
  function layoutWaterfall(root) {
    var bars = root.querySelectorAll('.wf-bar');
    var wall = 1;
    bars.forEach(function (bar) {
      var end = num(bar.dataset.startMs) + num(bar.dataset.durMs);
      if (end > wall) wall = end;
    });
    bars.forEach(function (bar) {
      var left = num(bar.dataset.startMs) / wall * 100;
      var width = num(bar.dataset.durMs) / wall * 100;
      bar.style.left = clamp(left) + '%';
      bar.style.width = Math.max(clamp(width), 0.6) + '%';
      var dur = bar.querySelector('.wf-dur');
      // Keep the duration caption inside the track for bars near the right edge.
      if (dur) {
        if (left > 72) { dur.style.left = 'auto'; dur.style.right = 'calc(100% + 6px)'; }
        else { dur.style.right = 'auto'; dur.style.left = 'calc(100% + 6px)'; }
      }
    });
  }

  function layoutCategoryBar(root) {
    root.querySelectorAll('.cat-seg').forEach(function (seg) {
      seg.style.width = clamp(parseFloat(seg.dataset.pct || '0')) + '%';
    });
  }

  function num(v) { var n = parseInt(v, 10); return isNaN(n) ? 0 : n; }
  function clamp(n) { if (n < 0) return 0; if (n > 100) return 100; return n; }

  // ---- Span → stream cross-highlight ------------------------------------
  function wireWaterfallJumps(root) {
    root.querySelectorAll('.wf-row').forEach(function (row) {
      row.addEventListener('click', function () { jumpToSpan(row.dataset.spanjump, root); });
    });
  }

  function jumpToSpan(spanID, root) {
    root.querySelectorAll('.wf-row.wf-selected').forEach(function (r) { r.classList.remove('wf-selected'); });
    var wfRow = root.querySelector('.wf-row[data-spanjump="' + cssEsc(spanID) + '"]');
    if (wfRow) wfRow.classList.add('wf-selected');
    var turn = root.querySelector('.turn[data-turnrow="' + cssEsc(spanID) + '"]');
    if (!turn) return;
    var det = turn.querySelector('details.turn-collapse');
    if (det) det.open = true;
    turn.scrollIntoView({ block: 'center', behavior: prefersReduced() ? 'auto' : 'smooth' });
    turn.classList.remove('wf-flash');
    void turn.offsetWidth; // restart the animation
    var target = turn.querySelector('.turn-summary') || turn;
    turn.classList.add('wf-flash');
    setTimeout(function () { turn.classList.remove('wf-flash'); }, 900);
  }

  function cssEsc(s) { return (window.CSS && CSS.escape) ? CSS.escape(s) : String(s).replace(/"/g, '\\"'); }
  function prefersReduced() { return window.matchMedia && matchMedia('(prefers-reduced-motion: reduce)').matches; }

  // ---- Reactions ---------------------------------------------------------
  var REACT_PALETTE = ['🔥', '👍', '👀', '🎯', '🙏', '🚀', '✅', '🐛'];

  function wireReactions(root, runID, canWrite) {
    root.querySelectorAll('[data-react-cluster]').forEach(function (cluster) {
      cluster.querySelectorAll('.react-pill').forEach(function (pill) {
        pill.addEventListener('click', function () {
          if (!canWrite) return;
          togglePill(runID, cluster, pill);
        });
      });
      var add = cluster.querySelector('[data-react-add]');
      if (add) add.addEventListener('click', function () {
        if (!canWrite) return;
        openPicker(runID, cluster, add);
      });
    });
  }

  function anchorBody(cluster, emoji) {
    var body = { anchor_type: cluster.dataset.anchorType, emoji: emoji };
    if (cluster.dataset.spanId) body.anchor_ref = { span_id: cluster.dataset.spanId };
    else body.anchor_ref = {};
    return body;
  }

  function togglePill(runID, cluster, pill) {
    var on = pill.getAttribute('aria-pressed') === 'true';
    var emoji = pill.dataset.emoji;
    var method = on ? 'DELETE' : 'POST';
    reactionFetch(runID, method, anchorBody(cluster, emoji)).then(function (ok) {
      if (!ok) return;
      var countEl = pill.querySelector('.react-count');
      var n = parseInt(countEl.textContent, 10) || 0;
      n = on ? Math.max(n - 1, 0) : n + 1;
      countEl.textContent = String(n);
      pill.setAttribute('aria-pressed', on ? 'false' : 'true');
      pill.classList.toggle('on', !on);
      if (n === 0 && on) pill.remove();
    });
  }

  function reactFor(runID, cluster, emoji) {
    // Reacting from the picker: bump an existing pill or create a new one.
    var existing = cluster.querySelector('.react-pill[data-emoji="' + cssEsc(emoji) + '"]');
    if (existing) {
      if (existing.getAttribute('aria-pressed') !== 'true') togglePill(runID, cluster, existing);
      return;
    }
    reactionFetch(runID, 'POST', anchorBody(cluster, emoji)).then(function (ok) {
      if (!ok) return;
      var pill = document.createElement('button');
      pill.type = 'button';
      pill.className = 'react-pill on';
      pill.dataset.emoji = emoji;
      pill.setAttribute('aria-pressed', 'true');
      pill.setAttribute('aria-label', 'React ' + emoji + ', 1 so far');
      pill.innerHTML = '<span class="react-emoji"></span> <span class="react-count">1</span>';
      pill.querySelector('.react-emoji').textContent = emoji;
      pill.addEventListener('click', function () { togglePill(runID, cluster, pill); });
      cluster.insertBefore(pill, cluster.querySelector('[data-react-add]'));
    });
  }

  function reactionFetch(runID, method, body) {
    return fetch('/v1/artifacts/' + encodeURIComponent(runID) + '/reactions', {
      method: method,
      headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf() },
      credentials: 'same-origin',
      body: JSON.stringify(body)
    }).then(function (r) { return r.ok; }).catch(function () { return false; });
  }

  function openPicker(runID, cluster, add) {
    var open = document.querySelector('.react-picker');
    if (open) open.remove();
    var pick = document.createElement('div');
    pick.className = 'react-picker';
    REACT_PALETTE.forEach(function (emoji) {
      var b = document.createElement('button');
      b.type = 'button';
      b.textContent = emoji;
      b.setAttribute('aria-label', 'React ' + emoji);
      b.addEventListener('click', function () { pick.remove(); reactFor(runID, cluster, emoji); });
      pick.appendChild(b);
    });
    document.body.appendChild(pick);
    var r = add.getBoundingClientRect();
    pick.style.left = (window.scrollX + r.left) + 'px';
    pick.style.top = (window.scrollY + r.bottom + 6) + 'px';

    var buttons = pick.querySelectorAll('button');
    var closed = false;
    // Single teardown: remove the picker, drop listeners, and restore focus to the
    // ＋ trigger (SPEC-0004 Keyboard Navigation & Focus Management — MANDATORY).
    function close(restoreFocus) {
      if (closed) return;
      closed = true;
      pick.remove();
      document.removeEventListener('click', onDocClick, true);
      pick.removeEventListener('keydown', onKeydown);
      if (restoreFocus && add && document.contains(add)) add.focus();
    }
    function onDocClick(e) {
      if (!pick.contains(e.target) && e.target !== add) close(false);
    }
    // Trap Tab within the picker and dismiss on Escape.
    function onKeydown(e) {
      if (e.key === 'Escape') { e.preventDefault(); close(true); return; }
      if (e.key !== 'Tab' || buttons.length === 0) return;
      var firstBtn = buttons[0];
      var lastBtn = buttons[buttons.length - 1];
      if (e.shiftKey && document.activeElement === firstBtn) {
        e.preventDefault(); lastBtn.focus();
      } else if (!e.shiftKey && document.activeElement === lastBtn) {
        e.preventDefault(); firstBtn.focus();
      }
    }
    // A picked emoji closes the picker AND restores focus to the trigger.
    buttons.forEach = Array.prototype.forEach;
    Array.prototype.forEach.call(buttons, function (b) {
      b.addEventListener('click', function () { close(true); });
    });
    pick.addEventListener('keydown', onKeydown);
    if (buttons.length) buttons[0].focus();
    // Defer the outside-click listener so the click that opened the picker
    // does not immediately close it. Capture phase so it fires before row handlers.
    setTimeout(function () { document.addEventListener('click', onDocClick, true); }, 0);
  }

  // ---- Comment anchoring (span + selection) ------------------------------
  function wireComments(root) {
    var form = root.querySelector('[data-comment-form]');
    if (!form) return;
    var chip = form.querySelector('[data-anchor-chip]');
    var chipLabel = form.querySelector('[data-anchor-label]');
    var fSpan = form.querySelector('[data-f-span]');
    var fStart = form.querySelector('[data-f-start]');
    var fEnd = form.querySelector('[data-f-end]');
    var fQuote = form.querySelector('[data-f-quote]');
    var textarea = form.querySelector('#comment-body');

    function clearAnchor() {
      fSpan.value = ''; fStart.value = ''; fEnd.value = ''; fQuote.value = '';
      if (chip) chip.hidden = true;
    }
    function setSpan(spanID, name) {
      clearAnchor();
      fSpan.value = spanID;
      if (chipLabel) chipLabel.textContent = 'on ' + (name || spanID);
      if (chip) chip.hidden = false;
      if (textarea) textarea.focus();
    }
    function setSelection(start, end, quote) {
      clearAnchor();
      fStart.value = String(start); fEnd.value = String(end); fQuote.value = quote;
      if (chipLabel) chipLabel.textContent = 'on “' + quote.slice(0, 34) + '”';
      if (chip) chip.hidden = false;
      if (textarea) textarea.focus();
    }

    var clearBtn = form.querySelector('[data-anchor-clear]');
    if (clearBtn) clearBtn.addEventListener('click', clearAnchor);

    root.querySelectorAll('[data-comment-span]').forEach(function (btn) {
      btn.addEventListener('click', function () { setSpan(btn.dataset.commentSpan, btn.dataset.commentName); });
    });

    // Clear the anchor + selection state after a successful HTMX comment post.
    form.addEventListener('htmx:afterRequest', function (e) {
      if (e.detail && e.detail.successful) { clearAnchor(); if (textarea) textarea.value = ''; }
    });

    wireSelection(root, setSelection);
  }

  // Floating selection toolbar over the activity stream: on a text selection,
  // offer "comment" (a text_selection anchor carrying the quote, ≤34 shown as
  // context) — the design's select→comment affordance.
  function wireSelection(root, setSelection) {
    var stream = root.querySelector('[data-webselectable]');
    if (!stream) return;
    var bar = null;
    function removeBar() { if (bar) { bar.remove(); bar = null; } }

    document.addEventListener('selectionchange', function () {
      var sel = window.getSelection();
      if (!sel || sel.isCollapsed || sel.rangeCount === 0) { removeBar(); return; }
      var range = sel.getRangeAt(0);
      if (!stream.contains(range.commonAncestorContainer)) { removeBar(); return; }
      var text = sel.toString().trim();
      if (!text) { removeBar(); return; }
      removeBar();
      bar = document.createElement('div');
      bar.className = 'sel-toolbar';
      var b = document.createElement('button');
      b.type = 'button';
      b.textContent = '💬 comment';
      b.addEventListener('mousedown', function (ev) { ev.preventDefault(); });
      b.addEventListener('click', function () {
        // Offsets are relative to the stream's text content; the server stores
        // the quote self-describingly, so approximate offsets are acceptable
        // (SPEC-0006 text_selection carries its own quote).
        var start = streamOffset(stream, range.startContainer, range.startOffset);
        setSelection(start, start + text.length, text);
        removeBar();
        window.getSelection().removeAllRanges();
      });
      bar.appendChild(b);
      document.body.appendChild(bar);
      var rect = range.getBoundingClientRect();
      bar.style.left = (window.scrollX + rect.left) + 'px';
      bar.style.top = (window.scrollY + rect.top - 38) + 'px';
    });
  }

  function streamOffset(stream, node, offset) {
    var walker = document.createTreeWalker(stream, NodeFilter.SHOW_TEXT, null);
    var total = 0, n;
    while ((n = walker.nextNode())) {
      if (n === node) return total + offset;
      total += n.textContent.length;
    }
    return total;
  }

  // ---- Lazy span output --------------------------------------------------
  function wireLazyOutput(root) {
    root.querySelectorAll('[data-output-load]').forEach(function (btn) {
      btn.addEventListener('click', function () {
        var pre = btn.closest('.output-pre');
        var url = pre.dataset.outputUrl;
        btn.disabled = true;
        btn.textContent = 'Loading…';
        fetch(url, { credentials: 'same-origin' }).then(function (r) { return r.text(); }).then(function (text) {
          pre.textContent = text;
        }).catch(function () { btn.textContent = 'Failed to load'; btn.disabled = false; });
      });
    });
  }

  // ---- Live SSE span stream ----------------------------------------------
  function wireLive(root) {
    var wf = root.querySelector('.waterfall');
    if (!wf || wf.dataset.live !== '1' || typeof EventSource === 'undefined') return;
    var runID = wf.dataset.runId;
    var region = root.querySelector('[data-live-region]');
    var seen = {};
    root.querySelectorAll('.wf-row[data-spanjump]').forEach(function (r) { seen[r.dataset.spanjump] = true; });
    var wfRows = root.querySelector('[data-wf-rows]');

    var es = new EventSource('/v1/runs/' + encodeURIComponent(runID) + '/stream');
    es.addEventListener('span', function (ev) {
      var span;
      try { span = JSON.parse(ev.data); } catch (e) { return; }
      if (!span || seen[span.span_id]) return;
      seen[span.span_id] = true;
      appendWaterfallRow(wfRows, span, runID);
      layoutWaterfall(root);
      if (region) region.textContent = 'Span added: ' + (span.name || span.span_id);
    });
    es.addEventListener('status', function (ev) {
      var data;
      try { data = JSON.parse(ev.data); } catch (e) { return; }
      if (data && data.status === 'closed') {
        es.close();
        var badge = root.querySelector('[data-live-badge]');
        if (badge) badge.remove();
        if (region) region.textContent = 'Run complete.';
      }
    });
    es.onerror = function () { /* EventSource auto-reconnects with Last-Event-ID */ };
  }

  function appendWaterfallRow(wfRows, span, runID) {
    var li = document.createElement('li');
    var btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'wf-row' + (span.depth > 0 ? ' wf-child' : '') + (span.tool === 'sub-agent' ? ' wf-subagent' : '');
    btn.dataset.spanjump = span.span_id;
    btn.setAttribute('aria-label', (span.name || span.span_id) + ' — ' + span.category + ', ' + secs(span.duration_ms) + 's');

    var label = document.createElement('span');
    label.className = 'wf-label';
    var sw = document.createElement('span');
    sw.className = 'swatch';
    sw.dataset.cat = span.category;
    label.appendChild(sw);
    if (span.depth > 0) { var br = document.createElement('span'); br.className = 'wf-branch'; br.textContent = '└'; label.appendChild(br); }
    if (span.tool) { var tc = document.createElement('span'); tc.className = 'tool-chip'; tc.dataset.tool = span.tool; tc.textContent = span.tool; label.appendChild(tc); }
    var nm = document.createElement('span'); nm.className = 'wf-name'; nm.textContent = span.name || span.span_id; label.appendChild(nm);

    var track = document.createElement('span'); track.className = 'wf-track';
    var bar = document.createElement('span'); bar.className = 'wf-bar'; bar.dataset.cat = span.category;
    bar.dataset.startMs = String(span.start_offset_ms || 0);
    bar.dataset.durMs = String(span.duration_ms || 0);
    var dur = document.createElement('span'); dur.className = 'wf-dur'; dur.textContent = secs(span.duration_ms) + 's';
    bar.appendChild(dur); track.appendChild(bar);

    btn.appendChild(label); btn.appendChild(track); li.appendChild(btn);
    btn.addEventListener('click', function () { jumpToSpan(span.span_id, document); });
    wfRows.appendChild(li);
  }

  function secs(ms) { return (Math.round((ms || 0) / 100) / 10).toFixed(1); }

  // ---- Init --------------------------------------------------------------
  function init() {
    var root = document;
    var wf = root.querySelector('.waterfall');
    if (!wf) return;
    var runID = wf.dataset.runId;
    var canWrite = !!root.querySelector('[data-comment-form]');
    layoutWaterfall(root);
    layoutCategoryBar(root);
    wireWaterfallJumps(root);
    wireReactions(root, runID, canWrite);
    wireComments(root);
    wireLazyOutput(root);
    wireLive(root);
    window.addEventListener('resize', function () { layoutWaterfall(root); });
  }

  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', init);
  else init();
})();
