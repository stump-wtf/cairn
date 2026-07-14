// The webhook inspector viewer's behaviors (SPEC-0005 REQ "Inspector Viewer",
// ADR-0011 "bespoke vanilla-JS widget", issue #86): the reaction picker (#66
// click-time render pattern — see hook.css's file header on why this is a
// straight copy of trajectory.js's picker rather than a shared module: no
// bundler, one <script> per view, matching markdown.js/image.js/code.js's
// established precedent of each carrying its own copy), lazy spilled-body
// fetch, and the live SSE tail (mirroring trajectory.js's wireLive — connect
// fresh with no Last-Event-ID, replay the endpoint's currently-retained
// buffer, and skip anything already server-rendered by seq membership).
//
// Everything here is progressive enhancement: the server already rendered
// the full request list (newest first), every request's headers and body,
// and the reaction pills, so the page is complete and screen-reader operable
// with JS disabled (SPEC-0005 "server-rendered request list readable with JS
// off; live tail layers on"). Plain vanilla JS — no eval, no inline
// expressions, no inline style="…" attributes (geometry is applied via CSSOM
// setters only) — so it runs under the shell's 'self'-only CSP.
(function () {
  'use strict';

  function csrf() {
    var m = document.cookie.match(/(?:^|;\s*)cairn_csrf=([^;]+)/);
    return m ? decodeURIComponent(m[1]) : '';
  }

  function cssEsc(s) { return (window.CSS && CSS.escape) ? CSS.escape(s) : String(s).replace(/"/g, '\\"'); }

  // A signed-out click on an annotation control routes to login instead of
  // silently no-oping (the write endpoint would 401 anyway); ?next returns
  // the user to this endpoint once the session exists (matches trajectory.js/
  // image.js/markdown.js).
  function requireLogin() {
    window.location.href = '/login?next=' + encodeURIComponent(window.location.pathname);
  }

  // ---- Reactions -----------------------------------------------------------
  var REACT_PALETTE = ['🔥', '👍', '👀', '🎯', '🙏', '🚀', '✅', '🐛'];

  // The webhook viewer renders NO comment composer at all (SPEC-0006 REQ
  // "Webhook Reaction-Only Asymmetry" — the whole Comments panel section is
  // absent, see shell.html's `{{if .CommentsSupported}}`), so — unlike
  // trajectory.js/image.js, which read a comment-composer/comment-form
  // element's presence as their "am I signed in" signal — this reads the
  // shell body's own data-authenticated attribute (shell.html) instead.
  function canWrite() {
    return document.body.dataset.authenticated === '1';
  }

  // anchor_ref shape: the whole-endpoint cluster (no data-span-id) reacts on
  // the `artifact` anchor with `{}`; a per-request cluster carries the
  // request's seq in data-span-id (the reaction-cluster template's generic
  // per-cluster item-id slot, reused verbatim from trajectory.html — see
  // hook_view.go's hookRequestRow.React doc comment) and reacts on
  // `webhook_request` with `{"request_id": "<seq>"}`, matching
  // sharetype.webhookRequestLocator.
  function anchorBody(cluster, emoji) {
    var body = { anchor_type: cluster.dataset.anchorType, emoji: emoji };
    if (cluster.dataset.spanId) body.anchor_ref = { request_id: cluster.dataset.spanId };
    else body.anchor_ref = {};
    return body;
  }

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

  // reactionFetch resolves to {ok, status} (never rejects) so callers can
  // tell "not signed in" (401 → route to login) apart from any other failure
  // (surface an inline error) rather than treating every non-2xx the same
  // way (#66).
  function reactionFetch(hookID, method, body) {
    return fetch('/v1/artifacts/' + encodeURIComponent(hookID) + '/reactions', {
      method: method,
      headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf() },
      credentials: 'same-origin',
      body: JSON.stringify(body)
    }).then(function (r) { return { ok: r.ok, status: r.status }; })
      .catch(function () { return { ok: false, status: 0 }; });
  }

  function togglePill(hookID, cluster, pill) {
    var on = pill.getAttribute('aria-pressed') === 'true';
    var emoji = pill.dataset.emoji;
    var method = on ? 'DELETE' : 'POST';
    reactionFetch(hookID, method, anchorBody(cluster, emoji)).then(function (res) {
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

  function reactFor(hookID, cluster, emoji) {
    var existing = cluster.querySelector('.react-pill[data-emoji="' + cssEsc(emoji) + '"]');
    if (existing) { togglePill(hookID, cluster, existing); return; }
    reactionFetch(hookID, 'POST', anchorBody(cluster, emoji)).then(function (res) {
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
      pill.addEventListener('click', function () { togglePill(hookID, cluster, pill); });
      cluster.insertBefore(pill, cluster.querySelector('[data-react-add]'));
    });
  }

  function openPicker(hookID, cluster, add) {
    var open = document.querySelector('.react-picker');
    if (open) open.remove();
    var pick = document.createElement('div');
    pick.className = 'react-picker';
    document.body.appendChild(pick);
    var r = add.getBoundingClientRect();
    pick.style.left = (window.scrollX + r.left) + 'px';
    pick.style.top = (window.scrollY + r.bottom + 6) + 'px';

    var buttons = [];
    var closed = false;
    // Single teardown: remove the picker, drop listeners, restore focus to
    // the ＋ trigger (SPEC-0005 Keyboard Navigation & Focus Management —
    // MANDATORY).
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
    REACT_PALETTE.forEach(function (emoji) {
      var b = document.createElement('button');
      b.type = 'button';
      b.textContent = emoji;
      b.setAttribute('aria-label', 'React ' + emoji);
      b.addEventListener('click', function () { close(true); reactFor(hookID, cluster, emoji); });
      pick.appendChild(b);
      buttons.push(b);
    });
    pick.addEventListener('keydown', onKeydown);
    if (buttons.length) buttons[0].focus();
    // Defer the outside-click listener so the click that opened the picker
    // does not immediately close it. Capture phase so it fires before row
    // handlers.
    setTimeout(function () { document.addEventListener('click', onDocClick, true); }, 0);
  }

  function wireReactions(root, hookID) {
    root.querySelectorAll('[data-react-cluster]').forEach(function (cluster) {
      cluster.querySelectorAll('.react-pill').forEach(function (pill) {
        pill.addEventListener('click', function () {
          if (!canWrite()) { requireLogin(); return; }
          togglePill(hookID, cluster, pill);
        });
      });
      var add = cluster.querySelector('[data-react-add]');
      if (add) add.addEventListener('click', function () {
        if (!canWrite()) { requireLogin(); return; }
        openPicker(hookID, cluster, add);
      });
    });
  }

  // ---- Lazy spilled-body fetch ---------------------------------------------
  // A spilled (oversized) body is fetched raw text on expand and set via
  // textContent — never re-highlighted client-side. Highlighting stays a
  // server-only concern throughout this app (internal/code.Render runs only
  // in the Go process; see internal/httpapi/hook_view.go's renderHookBody),
  // exactly as the code viewer's own doc comment establishes ("Highlighting
  // is done entirely on the server ... only the interactive
  // line-comment/react affordances are JS-layered").
  function wireLazyBody(root) {
    root.querySelectorAll('[data-hook-body-load]').forEach(function (btn) {
      btn.addEventListener('click', function () {
        var pre = btn.closest('[data-hook-body-lazy]');
        var url = pre.dataset.bodyUrl;
        btn.disabled = true;
        btn.textContent = 'Loading…';
        fetch(url, { credentials: 'same-origin' }).then(function (r) { return r.text(); }).then(function (text) {
          pre.textContent = text;
        }).catch(function () { btn.textContent = 'Failed to load'; btn.disabled = false; });
      });
    });
  }

  // ---- Status-mix bar geometry (CSSOM, not inline style — CSP style-src) --
  function layoutMix(root) {
    root.querySelectorAll('.hook-mix-seg').forEach(function (seg) {
      var pct = parseFloat(seg.dataset.pct || '0');
      seg.style.width = (isNaN(pct) ? 0 : pct) + '%';
    });
  }

  // ---- Keyboard: arrow keys move focus within the request list (SPEC-0005
  // Keyboard Navigation & Focus Management: "arrow keys within the request
  // list"). ----
  function wireArrowNav(list) {
    list.addEventListener('keydown', function (e) {
      if (e.key !== 'ArrowDown' && e.key !== 'ArrowUp') return;
      var summary = e.target.closest ? e.target.closest('.hook-row-summary') : null;
      if (!summary) return;
      var summaries = Array.prototype.slice.call(list.querySelectorAll(':scope > li > .hook-row-summary'));
      var i = summaries.indexOf(summary);
      if (i === -1) return;
      var next = e.key === 'ArrowDown' ? i + 1 : i - 1;
      if (next < 0 || next >= summaries.length) return;
      e.preventDefault();
      summaries[next].focus();
    });
  }

  // ---- Sorted insertion (newest-first, SPEC-0005 "newest prepended") ------
  // Correct regardless of how much of the retained buffer the server already
  // rendered: walks the list once, inserting before the first existing row
  // whose seq is lower than the new one (or at the end when none is). A
  // naive always-prepend-to-top (which is what trajectory.js's chronological
  // append can get away with) would misorder here, because a fresh SSE
  // connection always replays from the START of the still-retained buffer
  // (hook_stream.go), which can include requests older than the ones the
  // server already rendered on page load.
  function insertRowSorted(list, li, seq) {
    var rows = list.querySelectorAll(':scope > li');
    for (var i = 0; i < rows.length; i++) {
      var row = rows[i].querySelector('[data-hook-row]');
      if (row && seq > parseInt(row.dataset.seq, 10)) {
        list.insertBefore(li, rows[i]);
        return;
      }
    }
    list.appendChild(li);
  }

  // Builds one request row's DOM element-by-element — never innerHTML with
  // request data — because a captured request's method/path/headers/body are
  // untrusted, attacker-controlled bytes (SPEC-0005 Security REQ "No Payload
  // Execution / Inert Capture": "the inspector MUST render payloads as inert
  // escaped text"); textContent is inherently inert, mirroring
  // trajectory.js's appendStreamRow doc comment on the same hazard.
  function buildRowElement(req, hookID) {
    var li = document.createElement('li');
    var details = document.createElement('details');
    details.className = 'hook-row';
    details.dataset.hookRow = '';
    details.dataset.seq = String(req.seq);
    details.dataset.status = String(req.status);

    var summary = document.createElement('summary');
    summary.className = 'hook-row-summary';
    summary.tabIndex = 0;

    var method = document.createElement('span');
    method.className = 'hook-method';
    method.dataset.method = req.method || '';
    method.textContent = req.method || '';
    summary.appendChild(method);

    var path = document.createElement('span');
    path.className = 'hook-path';
    path.textContent = (req.path || '/') + (req.query ? '?' + req.query : '');
    summary.appendChild(path);

    var status = document.createElement('span');
    status.className = 'hook-status';
    var cls = (req.status >= 100 && req.status <= 599) ? (Math.floor(req.status / 100) + 'xx') : '?';
    status.dataset.class = cls;
    status.textContent = String(req.status);
    summary.appendChild(status);

    var meta = document.createElement('span');
    meta.className = 'hook-row-meta';
    meta.textContent = 'just now' + (req.body_size ? ' · ' + humanizeBytes(req.body_size) : '');
    summary.appendChild(meta);

    details.appendChild(summary);

    var detail = document.createElement('div');
    detail.className = 'hook-row-detail';

    var headerEntries = [];
    if (req.headers) {
      Object.keys(req.headers).sort().forEach(function (name) {
        (req.headers[name] || []).forEach(function (v) { headerEntries.push([name, v]); });
      });
    }
    if (headerEntries.length) {
      var hh = document.createElement('h4');
      hh.className = 'hook-detail-h';
      hh.textContent = 'Headers';
      detail.appendChild(hh);
      var dl = document.createElement('dl');
      dl.className = 'hook-headers';
      headerEntries.forEach(function (pair) {
        var dt = document.createElement('dt'); dt.textContent = pair[0];
        var dd = document.createElement('dd'); dd.textContent = pair[1];
        dl.appendChild(dt); dl.appendChild(dd);
      });
      detail.appendChild(dl);
    }

    if (req.body || req.body_ref) {
      var bh = document.createElement('h4');
      bh.className = 'hook-detail-h';
      bh.textContent = 'Body';
      detail.appendChild(bh);
      var pre = document.createElement('pre');
      pre.className = 'hook-body-pre';
      if (req.body) {
        // req.body is base64 (encoding/json's []byte encoding, requestView.Body)
        // — decoded to inert plain text, never re-highlighted client-side (see
        // this file's header comment: highlighting stays server-only).
        try { pre.textContent = atob(req.body); } catch (e) { pre.textContent = ''; }
      } else if (req.body_ref) {
        pre.dataset.hookBodyLazy = '';
        pre.dataset.bodyUrl = '/v1/hooks/' + encodeURIComponent(hookID) + '/requests/' + encodeURIComponent(req.seq) + '/body';
        var loadBtn = document.createElement('button');
        loadBtn.type = 'button';
        loadBtn.className = 'hook-body-load';
        loadBtn.dataset.hookBodyLoad = '';
        loadBtn.textContent = 'Load body ↧';
        pre.appendChild(loadBtn);
      }
      detail.appendChild(pre);
    }

    var actions = document.createElement('div');
    actions.className = 'hook-row-actions';
    actions.appendChild(buildReactionCluster(String(req.seq), 'Reactions on ' + (req.method || '') + ' ' + (req.path || '/')));
    detail.appendChild(actions);

    details.appendChild(detail);
    li.appendChild(details);
    return li;
  }

  // Zero-pill reaction cluster for a freshly-appended live row, matching the
  // server's "reaction-cluster" template shape so wireReactions (already
  // scanning `[data-react-cluster]`) treats it identically to a
  // server-rendered one (mirrors trajectory.js's buildReactionCluster).
  function buildReactionCluster(seq, label) {
    var cluster = document.createElement('div');
    cluster.className = 'react-cluster';
    cluster.setAttribute('role', 'group');
    cluster.setAttribute('aria-label', label);
    cluster.dataset.reactCluster = '';
    cluster.dataset.anchorType = 'webhook_request';
    cluster.dataset.spanId = seq;
    var add = document.createElement('button');
    add.type = 'button'; add.className = 'react-add'; add.dataset.reactAdd = '';
    add.setAttribute('aria-label', 'Add a reaction');
    add.setAttribute('aria-haspopup', 'true');
    add.textContent = '＋';
    cluster.appendChild(add);
    return cluster;
  }

  function humanizeBytes(n) {
    if (n < 1024) return n + ' B';
    if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' kB';
    return (n / (1024 * 1024)).toFixed(1) + ' MB';
  }

  // Recomputes the status-mix bar + legend from the DOM's current
  // [data-hook-row] set (no running counter to drift out of agreement,
  // mirroring trajectory.js's refreshLiveStats). No-ops if the endpoint
  // rendered with zero requests at load (no mix section exists yet) — an
  // edge case left for the next full page load rather than growing the DOM
  // shape live, same trade-off trajectory.js documents for its own
  // time-by-category bar.
  function refreshStatusMix(viewer) {
    var bar = viewer.querySelector('[data-hook-mix-bar]');
    var legend = viewer.querySelector('[data-hook-mix-legend]');
    if (!bar || !legend) return;
    var counts = {};
    var total = 0;
    viewer.querySelectorAll('[data-hook-row]').forEach(function (row) {
      var s = parseInt(row.dataset.status, 10);
      var cls = (s >= 100 && s <= 599) ? (Math.floor(s / 100) + 'xx') : '?';
      counts[cls] = (counts[cls] || 0) + 1;
      total++;
    });
    ['1xx', '2xx', '3xx', '4xx', '5xx'].forEach(function (cls) {
      var n = counts[cls] || 0;
      var seg = bar.querySelector('.hook-mix-seg[data-class="' + cls + '"]');
      var item = legend.querySelector('li[data-class="' + cls + '"]');
      if (n === 0) {
        if (seg) seg.remove();
        if (item) item.remove();
        return;
      }
      if (!seg) {
        seg = document.createElement('span');
        seg.className = 'hook-mix-seg';
        seg.dataset.class = cls;
        bar.appendChild(seg);
      }
      seg.dataset.pct = String(total > 0 ? (n / total * 100) : 0);
      if (!item) {
        item = document.createElement('li');
        item.dataset.class = cls;
        var sw = document.createElement('span');
        sw.className = 'swatch'; sw.dataset.class = cls; sw.setAttribute('aria-hidden', 'true');
        item.appendChild(sw);
        item.appendChild(document.createTextNode(cls + ' '));
        var countEl = document.createElement('span');
        countEl.className = 'hook-mix-count';
        item.appendChild(countEl);
        legend.appendChild(item);
      }
      item.querySelector('.hook-mix-count').textContent = String(n);
    });
    layoutMix(viewer);
    var totalEl = document.querySelector('[data-hook-total]');
    if (totalEl) totalEl.textContent = String(total);
  }

  // ---- Live SSE tail --------------------------------------------------------
  function wireLive(viewer, hookID) {
    if (viewer.dataset.hookLive !== '1' || typeof EventSource === 'undefined') return;
    var list = viewer.querySelector('[data-hook-list]');
    var region = viewer.querySelector('[data-hook-live-region]');
    var seen = {};
    viewer.querySelectorAll('[data-hook-row]').forEach(function (row) { seen[row.dataset.seq] = true; });

    var es = new EventSource('/v1/hooks/' + encodeURIComponent(hookID) + '/stream');
    es.addEventListener('request', function (ev) {
      var req;
      try { req = JSON.parse(ev.data); } catch (e) { return; }
      if (!req || seen[String(req.seq)]) return;
      seen[String(req.seq)] = true;

      var empty = viewer.querySelector('[data-hook-empty]');
      if (empty) empty.remove();

      var li = buildRowElement(req, hookID);
      insertRowSorted(list, li, req.seq);
      wireReactions(li, hookID);
      wireLazyBody(li);
      refreshStatusMix(viewer);

      if (region) region.textContent = 'Request captured: ' + (req.method || '') + ' ' + (req.path || '/');
    });
    es.onerror = function () { /* EventSource auto-reconnects */ };
  }

  // ---- Init ------------------------------------------------------------
  function init() {
    var viewer = document.querySelector('[data-hook-viewer]');
    if (!viewer) return;
    var hookID = viewer.dataset.hookId;
    layoutMix(viewer);
    wireReactions(document, hookID);
    wireLazyBody(document);
    var list = viewer.querySelector('[data-hook-list]');
    if (list) wireArrowNav(list);
    wireLive(viewer, hookID);
  }

  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', init);
  else init();
})();
