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
  // span that extends the timeline re-proportions the whole waterfall. Returns
  // the wall time (ms) it computed, so callers that also need it (the ruler,
  // the live stats refresh) don't re-derive it from a second bar scan.
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
    return wall;
  }

  // Re-render the five ruler ticks (0/25/50/75/100%) against a (possibly
  // grown) wall time, so the ruler never drifts out of agreement with the bar
  // geometry above it (#38 review note 3: "ruler vs bar scale can diverge on
  // live runs"). Order matches the server's fixed quartile ticks 1:1, so no
  // data-* percentage is needed on the tick spans themselves.
  function layoutRuler(root, wall) {
    var ticks = root.querySelectorAll('.wf-tick');
    var pcts = [0, 25, 50, 75, 100];
    if (ticks.length !== pcts.length) return;
    ticks.forEach(function (tick, i) {
      tick.textContent = secs(wall * pcts[i] / 100) + 's';
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

  // A signed-out click on an annotation control routes to login instead of
  // silently no-oping (the write endpoints would 401 anyway); ?next returns
  // the user to this run once the session exists.
  function requireLogin() {
    window.location.href = '/login?next=' + encodeURIComponent(window.location.pathname);
  }

  function wireReactions(root, runID, canWrite) {
    root.querySelectorAll('[data-react-cluster]').forEach(function (cluster) {
      cluster.querySelectorAll('.react-pill').forEach(function (pill) {
        pill.addEventListener('click', function () {
          if (!canWrite) { requireLogin(); return; }
          togglePill(runID, cluster, pill);
        });
      });
      var add = cluster.querySelector('[data-react-add]');
      if (add) add.addEventListener('click', function () {
        if (!canWrite) { requireLogin(); return; }
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

  // A reaction write failed for a reason other than "not signed in": surface it
  // rather than swallowing it (#66). A small transient role="status" toast near
  // the cluster tells the reader to retry instead of the click silently
  // appearing to do nothing.
  function flashReactError(cluster) {
    var existing = cluster.querySelector('.react-error');
    if (existing) existing.remove();
    var el = document.createElement('span');
    el.className = 'react-error';
    el.setAttribute('role', 'status');
    el.textContent = 'Could not react — try again.';
    cluster.appendChild(el);
    setTimeout(function () { if (el.parentNode) el.remove(); }, 3500);
  }

  function togglePill(runID, cluster, pill) {
    var on = pill.getAttribute('aria-pressed') === 'true';
    var emoji = pill.dataset.emoji;
    var method = on ? 'DELETE' : 'POST';
    reactionFetch(runID, method, anchorBody(cluster, emoji)).then(function (res) {
      if (!res.ok) {
        if (res.status === 401) { requireLogin(); return; }
        flashReactError(cluster);
        return;
      }
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
    // Reacting from the picker: an existing pill (whether or not the viewer has
    // already reacted with it) is toggled — reacting the same emoji again turns
    // it off, exactly like clicking the pill itself does — otherwise a fresh
    // pill is created and posted.
    var existing = cluster.querySelector('.react-pill[data-emoji="' + cssEsc(emoji) + '"]');
    if (existing) {
      togglePill(runID, cluster, existing);
      return;
    }
    reactionFetch(runID, 'POST', anchorBody(cluster, emoji)).then(function (res) {
      if (!res.ok) {
        if (res.status === 401) { requireLogin(); return; }
        flashReactError(cluster);
        return;
      }
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

  // reactionFetch resolves to {ok, status} (never rejects) so callers can tell
  // "not signed in" (401 → route to login) apart from any other failure (surface
  // an inline error) rather than treating every non-2xx the same way (#66).
  function reactionFetch(runID, method, body) {
    return fetch('/v1/artifacts/' + encodeURIComponent(runID) + '/reactions', {
      method: method,
      headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf() },
      credentials: 'same-origin',
      body: JSON.stringify(body)
    }).then(function (r) { return { ok: r.ok, status: r.status }; })
      .catch(function () { return { ok: false, status: 0 }; });
  }

  function openPicker(runID, cluster, add) {
    var open = document.querySelector('.react-picker');
    if (open) open.remove();
    if (activeCommentComposer) activeCommentComposer.close(false);
    var pick = document.createElement('div');
    pick.className = 'react-picker';
    document.body.appendChild(pick);
    var r = add.getBoundingClientRect();
    pick.style.left = (window.scrollX + r.left) + 'px';
    pick.style.top = (window.scrollY + r.bottom + 6) + 'px';

    var buttons = [];
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
    // A single, clean click path per palette button (#66 — previously TWO
    // competing listeners raced: an original pick.remove()+reactFor() handler
    // PLUS a second close(true) handler added afterward, which made the
    // optimistic pill insert unreliable): close the picker with focus restore
    // FIRST, then post the reaction and let reactFor insert/increment the pill
    // on success.
    REACT_PALETTE.forEach(function (emoji) {
      var b = document.createElement('button');
      b.type = 'button';
      b.textContent = emoji;
      b.setAttribute('aria-label', 'React ' + emoji);
      b.addEventListener('click', function () {
        close(true);
        reactFor(runID, cluster, emoji);
      });
      pick.appendChild(b);
      buttons.push(b);
    });
    pick.addEventListener('keydown', onKeydown);
    if (buttons.length) buttons[0].focus();
    // Defer the outside-click listener so the click that opened the picker
    // does not immediately close it. Capture phase so it fires before row handlers.
    setTimeout(function () { document.addEventListener('click', onDocClick, true); }, 0);
  }

  // ---- Comment anchoring (span + selection) ------------------------------
  // wireComments wires two independent things behind one gate:
  //   1. the RUN panel's composer form — whole-run (unanchored) comments and
  //      the select-text-to-comment flow (wireSelection, below) — unchanged
  //      from before #58, still an HTMX form post.
  //   2. every per-span/per-turn "💬 comment" button (comment-anchor-btn):
  //      instead of scrolling the anchor over to that far-away RUN panel
  //      form, a click opens a small floating composer AT the clicked line
  //      (openCommentComposer, mirroring openPicker's popover pattern right
  //      above), which posts straight to the same POST /{id}/comments
  //      endpoint the form uses (#58).
  // Both a signed-out click on a span button (previously a silent no-op —
  // the button rendered but was never wired when the RUN panel form was
  // absent) and one on the react-add trigger route to /login?next=,
  // consistent with the reaction affordance fixed in #41.
  //
  // Returns a handle exposing wireCommentButtons so a live-appended stream
  // row's button (wireLive, below) can be wired against the same runID/
  // canWrite closure the initial page load used.
  function wireComments(root, runID, canWrite) {
    var form = root.querySelector('[data-comment-form]');
    var chip = form && form.querySelector('[data-anchor-chip]');
    var chipLabel = form && form.querySelector('[data-anchor-label]');
    var fStart = form && form.querySelector('[data-f-start]');
    var fEnd = form && form.querySelector('[data-f-end]');
    var fQuote = form && form.querySelector('[data-f-quote]');
    var textarea = form && form.querySelector('#comment-body');

    function clearAnchor() {
      if (!form) return;
      fStart.value = ''; fEnd.value = ''; fQuote.value = '';
      if (chip) chip.hidden = true;
    }
    function setSelection(start, end, quote) {
      if (!form) return;
      clearAnchor();
      fStart.value = String(start); fEnd.value = String(end); fQuote.value = quote;
      if (chipLabel) chipLabel.textContent = 'on “' + quote.slice(0, 34) + '”';
      if (chip) chip.hidden = false;
      if (textarea) textarea.focus();
    }

    if (form) {
      var clearBtn = form.querySelector('[data-anchor-clear]');
      if (clearBtn) clearBtn.addEventListener('click', clearAnchor);

      // Clear the selection state after a successful HTMX comment post.
      form.addEventListener('htmx:afterRequest', function (e) {
        if (e.detail && e.detail.successful) { clearAnchor(); if (textarea) textarea.value = ''; }
      });

      wireSelection(root, setSelection);
    }

    function wireCommentButtons(scope) {
      scope.querySelectorAll('[data-comment-span]').forEach(function (btn) {
        btn.addEventListener('click', function () {
          if (!canWrite) { requireLogin(); return; }
          openCommentComposer(root, runID, btn);
        });
      });
    }
    wireCommentButtons(root);

    return { wireCommentButtons: wireCommentButtons };
  }

  // A single floating comment composer open at a time (mirrors the reaction
  // picker's implicit singleton — clicking a second "💬 comment" button
  // replaces, rather than stacks on top of, the first).
  var activeCommentComposer = null;

  // Opens a labeled, focus-trapped floating composer at the clicked span/turn
  // button: a textarea + Post/Cancel, Escape to dismiss, focus restored to the
  // trigger on close (SPEC-0004 Keyboard Navigation & Focus Management —
  // MANDATORY, same bar as openPicker above). Posting submits a
  // trajectory_span-anchored comment (span_id = the button's data-comment-span)
  // to the same `POST /{id}/comments` endpoint the RUN panel form uses; the
  // server-rendered comment partial it returns is appended to the RUN panel's
  // #comment-list (mirroring the form's own hx-swap="beforeend") and the
  // Comments count is bumped, so the new comment is visible in the panel
  // without a full reload — the anchor visibly attached at the point of
  // authorship even though the record of it lives in the panel thread (#58).
  function openCommentComposer(root, runID, trigger) {
    if (activeCommentComposer) activeCommentComposer.close(false);

    var spanID = trigger.dataset.commentSpan;
    var label = trigger.dataset.commentName || spanID;
    var taID = 'inline-comment-' + Math.random().toString(36).slice(2);

    var box = document.createElement('div');
    box.className = 'comment-inline';
    box.setAttribute('role', 'group');
    box.setAttribute('aria-label', 'Comment on ' + label);

    var heading = document.createElement('p');
    heading.className = 'comment-inline-label';
    heading.textContent = 'commenting on ' + label;
    box.appendChild(heading);

    var lbl = document.createElement('label');
    lbl.className = 'sr-only';
    lbl.setAttribute('for', taID);
    lbl.textContent = 'Comment on ' + label;
    box.appendChild(lbl);

    var ta = document.createElement('textarea');
    ta.id = taID;
    ta.rows = 3;
    ta.placeholder = 'comment on this span…';
    box.appendChild(ta);

    var err = document.createElement('p');
    err.className = 'comment-inline-error';
    err.setAttribute('role', 'status');
    err.hidden = true;
    box.appendChild(err);

    var foot = document.createElement('div');
    foot.className = 'comment-inline-foot';
    var cancelBtn = document.createElement('button');
    cancelBtn.type = 'button'; cancelBtn.className = 'comment-inline-cancel'; cancelBtn.textContent = 'Cancel';
    var postBtn = document.createElement('button');
    postBtn.type = 'button'; postBtn.className = 'comment-inline-post'; postBtn.textContent = 'Post';
    foot.appendChild(cancelBtn); foot.appendChild(postBtn);
    box.appendChild(foot);

    document.body.appendChild(box);

    // Position at the trigger, clamped so a span near the right/bottom edge
    // doesn't render the composer off-screen.
    var r = trigger.getBoundingClientRect();
    var maxLeft = window.scrollX + document.documentElement.clientWidth - box.offsetWidth - 12;
    var left = Math.min(window.scrollX + r.left, Math.max(maxLeft, window.scrollX + 12));
    box.style.left = left + 'px';
    box.style.top = (window.scrollY + r.bottom + 6) + 'px';

    var focusables = [ta, cancelBtn, postBtn];
    var closed = false;
    function close(restoreFocus) {
      if (closed) return;
      closed = true;
      activeCommentComposer = null;
      box.remove();
      document.removeEventListener('click', onDocClick, true);
      box.removeEventListener('keydown', onKeydown);
      if (restoreFocus && trigger && document.contains(trigger)) trigger.focus();
    }
    function onDocClick(e) {
      if (!box.contains(e.target)) close(false);
    }
    // Trap Tab within the composer and dismiss on Escape.
    function onKeydown(e) {
      if (e.key === 'Escape') { e.preventDefault(); close(true); return; }
      if (e.key !== 'Tab') return;
      var first = focusables[0], last = focusables[focusables.length - 1];
      if (e.shiftKey && document.activeElement === first) {
        e.preventDefault(); last.focus();
      } else if (!e.shiftKey && document.activeElement === last) {
        e.preventDefault(); first.focus();
      }
    }
    box.addEventListener('keydown', onKeydown);
    cancelBtn.addEventListener('click', function () { close(true); });
    postBtn.addEventListener('click', function () {
      var body = ta.value.trim();
      if (!body) { ta.focus(); return; }
      err.hidden = true;
      postBtn.disabled = true; cancelBtn.disabled = true; ta.disabled = true;
      // application/x-www-form-urlencoded, matching the RUN panel <form>'s
      // default enctype: the server side (webbin.go handleWebComment) only
      // calls r.ParseForm(), which does not parse multipart/form-data, so a
      // FormData body here would silently arrive as an empty form.
      var params = new URLSearchParams();
      params.set('span_id', spanID);
      params.set('body', body);
      fetch('/' + encodeURIComponent(runID) + '/comments', {
        method: 'POST',
        headers: { 'Content-Type': 'application/x-www-form-urlencoded', 'X-CSRF-Token': csrf() },
        credentials: 'same-origin',
        body: params.toString()
      }).then(function (resp) {
        if (!resp.ok) throw new Error('post failed');
        return resp.text();
      }).then(function (html) {
        appendCommentHTML(root, html);
        close(true);
      }).catch(function () {
        postBtn.disabled = false; cancelBtn.disabled = false; ta.disabled = false;
        err.hidden = false; err.textContent = 'Could not post comment — try again.';
        ta.focus();
      });
    });

    activeCommentComposer = { close: close };
    ta.focus();
    // Defer the outside-click listener so the click that opened the composer
    // does not immediately close it. Capture phase so it fires before row
    // handlers (matches openPicker above).
    setTimeout(function () { document.addEventListener('click', onDocClick, true); }, 0);
  }

  // Appends the server-rendered comment partial (already HTML-escaped by
  // html/template — the same trust boundary the RUN panel form's own
  // hx-swap="beforeend" HTMX post relies on) to the RUN panel's #comment-list,
  // dropping the "No comments yet" placeholder if this is the first comment,
  // and bumps the Comments panel-heading count so both stay in agreement with
  // no full-page reload (#58).
  function appendCommentHTML(root, html) {
    var list = root.querySelector('#comment-list');
    if (!list) return;
    var empty = list.querySelector('.comments-empty');
    if (empty) empty.remove();
    var tmp = document.createElement('div');
    tmp.innerHTML = html;
    while (tmp.firstChild) list.appendChild(tmp.firstChild);
    var count = root.querySelector('#comments-h .count');
    if (count) count.textContent = String((num(count.textContent) || 0) + 1);
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
  // A live span updates three things, all derived from the same span rows so
  // they never disagree (mirroring the server's SPEC-0004 "Derived Run
  // Statistics" invariant, kept client-side across the live tail):
  //   1. the waterfall (bar + ruler geometry);
  //   2. the activity stream (an appended turn, nested under its sub-agent
  //      parent if one is already on the page — #38 review note 2); and
  //   3. the RUN panel's live-derivable stats: wall time, span/tool-call
  //      counts, the sub-agent count in the legend, and the time-by-category
  //      bar (#38 review note 2).
  // The one figure NOT kept live is the tokens tile: the SSE span payload
  // (streamSpanView, internal/httpapi/stream.go) carries no per-span token
  // count, so there is nothing to accumulate from — it reflects the value as
  // of page load/reconnect and is intentionally left alone rather than faked.
  function wireLive(root, canWrite, commentsHandle) {
    var wf = root.querySelector('.waterfall');
    if (!wf || wf.dataset.live !== '1' || typeof EventSource === 'undefined') return;
    var runID = wf.dataset.runId;
    var region = root.querySelector('[data-live-region]');
    var seen = {};
    root.querySelectorAll('.wf-row[data-spanjump]').forEach(function (r) { seen[r.dataset.spanjump] = true; });
    var wfRows = root.querySelector('[data-wf-rows]');
    var stream = root.querySelector('#stream');

    var es = new EventSource('/v1/runs/' + encodeURIComponent(runID) + '/stream');
    es.addEventListener('span', function (ev) {
      var span;
      try { span = JSON.parse(ev.data); } catch (e) { return; }
      if (!span || seen[span.span_id]) return;
      seen[span.span_id] = true;
      appendWaterfallRow(wfRows, span, runID);
      var wall = layoutWaterfall(root);
      layoutRuler(root, wall);
      if (stream) appendStreamRow(stream, span, runID, canWrite, commentsHandle);
      refreshLiveStats(root, wall);
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

  // Recompute the RUN panel's live-derivable figures from the current DOM
  // state (every `.wf-bar` the waterfall now holds, replay + live alike), so
  // there is one source of truth and no incremental counter to drift. Cheap
  // for the span counts a live run realistically reaches.
  function refreshLiveStats(root, wall) {
    var bars = root.querySelectorAll('.wf-bar');
    var spanCount = bars.length, toolCalls = 0, subAgents = 0;
    var catMs = {};
    bars.forEach(function (bar) {
      var row = bar.closest('.wf-row');
      var dur = num(bar.dataset.durMs);
      if (bar.dataset.cat) catMs[bar.dataset.cat] = (catMs[bar.dataset.cat] || 0) + dur;
      if (row && row.querySelector('.tool-chip')) toolCalls++;
      if (row && row.classList.contains('wf-subagent')) subAgents++;
    });

    var metaLine = root.querySelector('.app-header .artifact-meta');
    if (metaLine) metaLine.textContent = spanCount + ' spans · ' + secs(wall) + 's';

    var legendMeta = root.querySelector('.wf-legendmeta');
    if (legendMeta) {
      var model = legendMeta.dataset.model || '';
      legendMeta.textContent = model + ' · ' + subAgents + ' sub-agent · ' + toolCalls + ' tool calls';
    }

    root.querySelectorAll('.stat-tile').forEach(function (tile) {
      var label = tile.querySelector('.stat-label');
      var val = tile.querySelector('.stat-val');
      if (!label || !val) return;
      var unitEl = val.querySelector('.stat-unit');
      var text;
      switch (label.textContent) {
        case 'wall time': text = secs(wall); break;
        case 'spans': text = String(spanCount); break;
        case 'tool calls': text = String(toolCalls); break;
        default: return; // 'tokens': not derivable client-side, see wireLive doc above.
      }
      if (unitEl && val.firstChild) val.firstChild.textContent = text;
      else val.textContent = text;
    });

    // Time-by-category bar + legend: only present in the DOM if the initial
    // render had at least one categorized-time segment (`{{if .Categories}}`
    // in trajectory.html). A run that opens with literally zero elapsed time
    // won't have the section to update into until the next full page load —
    // an edge case left undone rather than growing the DOM shape live.
    var catBar = root.querySelector('[data-cat-bar]');
    if (catBar) {
      ['reason', 'net', 'exec', 'read', 'write'].forEach(function (cat) {
        var ms = catMs[cat] || 0;
        var seg = catBar.querySelector('.cat-seg[data-cat="' + cat + '"]');
        if (seg) seg.dataset.pct = String(clamp(wall > 0 ? (ms / wall * 100) : 0));
        var legendDur = root.querySelector('.cat-legend li[data-cat="' + cat + '"] .cat-dur');
        if (legendDur && ms > 0) legendDur.textContent = secs(ms) + 's';
      });
      layoutCategoryBar(root);
    }
  }

  // Append one activity-stream turn for a live span, mirroring the server's
  // "stream-row" template (trajectory.html) closely enough for visual and
  // interactive parity: role marker, tool/reason chip, collapsible detail
  // (inline body, lazy-loaded spilled output, or a produced-artifact card),
  // a working reaction cluster, and (if signed in) the comment-anchor button.
  // Nested under its sub-agent parent's `.subagent-children` when that parent
  // is already on the page; appended at top level otherwise (a child whose
  // parent hasn't streamed in yet — the waterfall still places it correctly
  // via depth/indent, so nothing is lost, just not re-nested here).
  //
  // Every field taken from `span` is untrusted agent content (stream.go
  // "Active content in a span output"), so this builds elements and sets
  // textContent throughout — never innerHTML with span data.
  function appendStreamRow(stream, span, runID, canWrite, commentsHandle) {
    var role = 'reason', openByDefault = false;
    if (span.tool === 'sub-agent') { role = 'sub-agent'; openByDefault = true; }
    else if (span.tool) { role = 'tool'; openByDefault = span.category === 'exec' || span.category === 'write'; }

    var turn = document.createElement('div');
    turn.className = 'turn turn-' + role;
    turn.dataset.turnrow = span.span_id;
    turn.dataset.durMs = String(span.duration_ms || 0);
    turn.dataset.parentSpanId = span.parent_span_id || '';
    turn.dataset.trajrow = '';

    var rail = document.createElement('div'); rail.className = 'turn-rail';
    var marker = document.createElement('span');
    marker.className = 'marker marker-' + role;
    marker.dataset.cat = span.category;
    marker.setAttribute('aria-hidden', 'true');
    rail.appendChild(marker);

    var body = document.createElement('div'); body.className = 'turn-body';
    var details = document.createElement('details'); details.className = 'turn-collapse';
    if (openByDefault) details.open = true;
    details.dataset.trajhead = '';

    var summary = document.createElement('summary'); summary.className = 'turn-summary';
    if (span.tool) {
      var chip = document.createElement('span'); chip.className = 'tool-chip'; chip.dataset.tool = span.tool; chip.textContent = span.tool;
      summary.appendChild(chip);
    } else {
      var reasonChip = document.createElement('span'); reasonChip.className = 'chip chip-reason'; reasonChip.textContent = 'reason';
      summary.appendChild(reasonChip);
    }
    var nameEl = document.createElement('span'); nameEl.className = 'turn-name'; nameEl.textContent = span.name || span.span_id;
    summary.appendChild(nameEl);
    var metaEl = document.createElement('span'); metaEl.className = 'turn-meta';
    metaEl.textContent = '· ' + secs(span.duration_ms) + 's';
    summary.appendChild(metaEl);
    details.appendChild(summary);

    var detail = document.createElement('div'); detail.className = 'turn-detail';
    if (span.output_ref) {
      var strip = document.createElement('div'); strip.className = 'output-strip'; strip.textContent = 'stdout · truncated';
      var pre = document.createElement('pre'); pre.className = 'output-pre';
      pre.dataset.outputUrl = '/v1/runs/' + encodeURIComponent(runID) + '/spans/' + encodeURIComponent(span.span_id) + '/output';
      var loadBtn = document.createElement('button'); loadBtn.type = 'button'; loadBtn.className = 'output-load'; loadBtn.dataset.outputLoad = '';
      loadBtn.textContent = 'Load output ↧';
      pre.appendChild(loadBtn);
      detail.appendChild(strip); detail.appendChild(pre);
    } else if (span.output) {
      var strip2 = document.createElement('div'); strip2.className = 'output-strip'; strip2.textContent = 'stdout';
      var pre2 = document.createElement('pre'); pre2.className = 'output-pre'; pre2.textContent = span.output;
      detail.appendChild(strip2); detail.appendChild(pre2);
    }
    if (span.produced_artifact_ids && span.produced_artifact_ids.length) {
      var pid = span.produced_artifact_ids[0];
      var card = document.createElement('a'); card.className = 'artifact-card'; card.href = '/' + encodeURIComponent(pid);
      var achip = document.createElement('span'); achip.className = 'artifact-chip'; achip.textContent = 'MD';
      var ameta = document.createElement('span'); ameta.className = 'artifact-meta';
      var aname = document.createElement('span'); aname.className = 'artifact-name'; aname.textContent = span.name || pid;
      var anote = document.createElement('span'); anote.className = 'artifact-note'; anote.textContent = 'artifact produced by this run — opens the markdown share ↗';
      ameta.appendChild(aname); ameta.appendChild(anote);
      var abadge = document.createElement('span'); abadge.className = 'badge badge-shared'; abadge.textContent = 'SHARED';
      card.appendChild(achip); card.appendChild(ameta); card.appendChild(abadge);
      detail.appendChild(card);
    }
    var childrenBox = null;
    if (role === 'sub-agent') {
      childrenBox = document.createElement('div'); childrenBox.className = 'subagent-children';
      detail.appendChild(childrenBox);
      // A child can stream in before its sub-agent parent (arrival order isn't
      // guaranteed) — reparent any such orphans, previously appended at top
      // level, into this container now that it exists, in their original
      // arrival order.
      stream.querySelectorAll('.turn[data-parent-span-id="' + cssEsc(span.span_id) + '"]').forEach(function (orphan) {
        childrenBox.appendChild(orphan);
      });
      metaEl.textContent = '· ' + childrenBox.querySelectorAll(':scope > .turn').length + ' tools · ' + secs(span.duration_ms) + 's';
    }
    details.appendChild(detail);
    body.appendChild(details);

    var actions = document.createElement('div'); actions.className = 'turn-actions';
    var anchorType = span.tool ? 'trajectory_toolcall' : 'trajectory_turn';
    actions.appendChild(buildReactionCluster(anchorType, span.span_id, 'Reactions on ' + (span.name || span.span_id)));
    // Rendered regardless of canWrite, matching the server-rendered "stream-row"
    // template: a signed-out click still routes to /login?next= (wired below via
    // commentsHandle), rather than the button silently being absent (#58).
    var commentBtn = document.createElement('button'); commentBtn.type = 'button'; commentBtn.className = 'comment-anchor-btn';
    commentBtn.dataset.commentSpan = span.span_id;
    commentBtn.dataset.commentName = span.name || '';
    commentBtn.setAttribute('aria-label', 'Comment on ' + (span.name || span.span_id));
    commentBtn.textContent = '💬 comment';
    actions.appendChild(commentBtn);
    body.appendChild(actions);

    turn.appendChild(rail); turn.appendChild(body);
    wireLazyOutput(turn);
    wireReactions(turn, runID, canWrite);
    commentsHandle.wireCommentButtons(turn);

    var parent = span.parent_span_id ? stream.querySelector('.turn[data-turnrow="' + cssEsc(span.parent_span_id) + '"]') : null;
    var container = parent ? parent.querySelector('.subagent-children') : null;
    // A sub-agent parent rendered with no children yet (`{{if .Children}}` was
    // false at page load, or it was itself a live-appended row with none so
    // far) has no `.subagent-children` box on the page — create it now rather
    // than falling back to top-level placement.
    if (parent && !container && parent.classList.contains('turn-sub-agent')) {
      var parentDetail = parent.querySelector('.turn-detail');
      if (parentDetail) {
        container = document.createElement('div');
        container.className = 'subagent-children';
        parentDetail.appendChild(container);
      }
    }
    if (container) container.appendChild(turn);
    else stream.appendChild(turn);

    // A sub-agent parent's meta line ("· N tools · Xs") is only knowable once
    // its children have streamed in — recompute it now that this one landed,
    // from the parent's own data-dur-ms (set by the server template and by
    // this function alike) rather than re-parsing rendered text.
    if (parent && container && parent.classList.contains('turn-sub-agent')) {
      var pMeta = parent.querySelector('.turn-summary .turn-meta');
      if (pMeta) {
        var count = container.querySelectorAll(':scope > .turn').length;
        pMeta.textContent = '· ' + count + ' tools · ' + secs(num(parent.dataset.durMs)) + 's';
      }
    }
  }

  // Zero-pill reaction cluster for a freshly-appended live row, matching the
  // server's "reaction-cluster" template shape so wireReactions (already
  // scanning `[data-react-cluster]`) treats it identically to a server-
  // rendered one.
  function buildReactionCluster(anchorType, spanID, label) {
    var cluster = document.createElement('div');
    cluster.className = 'react-cluster';
    cluster.setAttribute('role', 'group');
    cluster.setAttribute('aria-label', label);
    cluster.dataset.reactCluster = '';
    cluster.dataset.anchorType = anchorType;
    if (spanID) cluster.dataset.spanId = spanID;
    var add = document.createElement('button');
    add.type = 'button'; add.className = 'react-add'; add.dataset.reactAdd = '';
    add.setAttribute('aria-label', 'Add a reaction');
    add.setAttribute('aria-haspopup', 'true');
    add.textContent = '＋';
    cluster.appendChild(add);
    return cluster;
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
    var commentsHandle = wireComments(root, runID, canWrite);
    wireLazyOutput(root);
    wireLive(root, canWrite, commentsHandle);
    window.addEventListener('resize', function () { layoutWaterfall(root); });
  }

  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', init);
  else init();
})();
