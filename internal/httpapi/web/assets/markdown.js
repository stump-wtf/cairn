// Markdown viewer progressive enhancement (SPEC-0003 REQ "Markdown Annotation
// Anchors", REQ "Progressive Enhancement", Accessibility REQ). The server has
// already rendered readable, keyboard-navigable prose with a working TOC and a
// react `＋` affordance per block; this layers on the interactive affordances:
//
//   - the block/bullet reaction picker (posts an md_block or md_bullet reaction
//     to the /v1 annotation API, CSRF-guarded for the ambient session);
//   - the md_bullet affordance, injected to the left of every list item so a
//     reader can react on a single bullet (anchor carries {block_id, path});
//   - select-text-to-comment: a floating toolbar over a prose selection captures
//     a text_selection anchor (offsets + quoted substring) and hands it to the
//     shell's comment composer; and
//   - a TOC active-item flash on jump.
//
// If this script fails to load the prose still reads and the whole-artifact
// composer still posts, so core reading degrades gracefully. Everything is plain
// vanilla JS from 'self' with no eval, so it needs nothing the shell CSP forbids
// (reading a cookie and calling fetch to same-origin are both allowed).
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

  // memberNameFor returns the bundle member a node lives in, or null when the
  // markdown viewer is the whole artifact (the standalone page).
  //
  // A markdown member of a bundle renders through this exact same viewer
  // fragment, so every affordance below is shared — only the ANCHOR differs.
  // The bundle share type forbids md_block/md_bullet (the block ids of two
  // members would collide on the one artifact), so inside a member pane the
  // same block and bullet triggers post a member-scoped bundle_file anchor
  // carrying {name, block_id[, path]} instead (SPEC-0003 REQ "Bundle Viewer",
  // SPEC-0006 REQ "Registry-Gated Anchor Capabilities").
  function memberNameFor(node) {
    var pane = node && node.closest ? node.closest('[data-member]') : null;
    return pane ? pane.getAttribute('data-member') : null;
  }

  // reactionFetch posts or removes (toggles) a reaction against the annotation
  // API. A session (cookie) caller rides the double-submit CSRF header the
  // server matches; a token caller is exempt. Never throws — onDone(ok, status)
  // always fires so a caller can tell "not signed in" (401) apart from any
  // other failure (#66) rather than treating every non-2xx the same way.
  function reactionFetch(method, anchorType, anchorRef, emoji, onDone) {
    var id = artifactID();
    if (!id) { onDone(false, 0); return; }
    fetch('/v1/artifacts/' + encodeURIComponent(id) + '/reactions', {
      method: method,
      credentials: 'same-origin',
      headers: {
        'Content-Type': 'application/json',
        'X-CSRF-Token': csrfToken()
      },
      body: JSON.stringify({ anchor_type: anchorType, anchor_ref: anchorRef, emoji: emoji })
    }).then(function (resp) {
      onDone(resp.ok, resp.status);
    }).catch(function () {
      onDone(false, 0);
    });
  }

  // handleReactError is the shared failure path for both the picker and an
  // existing pill's toggle click: a signed-out write 401s → route to login with
  // a return path (mirroring the trajectory viewer, #41); any other failure
  // surfaces a small inline toast rather than the click silently appearing to
  // do nothing (#66).
  function handleReactError(trigger, status) {
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

  // ---- block/bullet reaction pills ----------------------------------------
  // The markdown viewer renders only the react `＋` affordance server-side (no
  // reaction state — the body fragment is a pure, annotation-agnostic render of
  // the type-only BodyViewer capability, ADR-0002). Existing tallies and every
  // click-time pill are therefore rendered here, progressively (ADR-0011): the
  // prose and the ＋ affordance work with JS disabled, exactly as before.

  // pillHost returns the element a trigger's pills live in.
  //
  // A BLOCK trigger's pills sit as siblings beside it, flowing after the prose.
  // A BULLET's go in a container appended to the END of its <li>, so they read
  // as their own line under the item text:
  //
  //     • Some bullet point text
  //       👍 2  🎉 1
  //
  // They used to be inserted before the trigger, which is the li's first child
  // — so every reaction pushed itself in FRONT of the item's text. One pill was
  // merely odd; several turned the start of the line into a row of emoji.
  function pillHost(trigger) {
    if (trigger.getAttribute('data-anchor-type') !== 'md_bullet') return trigger.parentNode;
    var li = trigger.closest('li');
    if (!li) return trigger.parentNode;
    var host = li.querySelector(':scope > .md-bullet-reacts');
    if (!host) {
      host = document.createElement('div');
      host.className = 'md-bullet-reacts';
      li.appendChild(host);
    }
    return host;
  }

  // findPill returns this emoji's already-rendered pill for a trigger, or null
  // if none exists yet. Scoped to DIRECT children of the pill host (`:scope >`)
  // — not all descendants — because a list block's `.md-block` parent also
  // contains its bullets' own `.md-react-pill`s nested inside the list, which
  // must never match a lookup for the block-level trigger's pills.
  function findPill(trigger, emoji) {
    var pills = pillHost(trigger).querySelectorAll(':scope > .md-react-pill');
    for (var i = 0; i < pills.length; i++) {
      if (pills[i].dataset.emoji === emoji) return pills[i];
    }
    return null;
  }

  // pillFor finds (or creates, in the trigger's pill host) this emoji's pill
  // button, so a repeat reaction updates the same element rather than
  // accumulating duplicates.
  function pillFor(trigger, emoji) {
    var found = findPill(trigger, emoji);
    if (found) return found;
    var host = pillHost(trigger);
    var pill = document.createElement('button');
    pill.type = 'button';
    pill.className = 'react-pill md-react-pill';
    pill.dataset.emoji = emoji;
    pill.setAttribute('aria-pressed', 'false');
    var em = document.createElement('span'); em.className = 'react-emoji'; em.textContent = emoji;
    var ct = document.createElement('span'); ct.className = 'react-count'; ct.textContent = '0';
    pill.appendChild(em);
    pill.appendChild(document.createTextNode(' '));
    pill.appendChild(ct);
    pill.addEventListener('click', function () { togglePill(trigger, pill); });
    // A block's pills flow beside the trigger; a bullet's append into its own
    // line container (see pillHost).
    if (host === trigger.parentNode) host.insertBefore(pill, trigger);
    else host.appendChild(pill);
    return pill;
  }

  // setPillCount renders a pill's count/pressed state, removing it once the
  // count reaches zero (toggled off with nobody else reacted with that emoji).
  function setPillCount(pill, count, reacted) {
    pill.querySelector('.react-count').textContent = String(count);
    pill.setAttribute('aria-pressed', reacted ? 'true' : 'false');
    pill.classList.toggle('on', !!reacted);
    pill.setAttribute('aria-label', 'React ' + pill.dataset.emoji + ', ' + count + ' so far');
    if (count <= 0) pill.remove();
  }

  // togglePill posts/removes the viewer's own reaction on an existing pill and
  // updates its count on success — reacting the same emoji again toggles it off
  // (SPEC-0006 idempotent reactions; #66 count-semantics parity with the
  // trajectory viewer).
  function togglePill(trigger, pill) {
    var on = pill.getAttribute('aria-pressed') === 'true';
    var emoji = pill.dataset.emoji;
    var anchor = anchorRefFor(trigger);
    reactionFetch(on ? 'DELETE' : 'POST', anchor.type, anchor.ref, emoji, function (ok, status) {
      if (!ok) { handleReactError(trigger, status); return; }
      var n = parseInt(pill.querySelector('.react-count').textContent, 10) || 0;
      n = on ? Math.max(n - 1, 0) : n + 1;
      setPillCount(pill, n, !on);
      notifyReaction(trigger, status);
    });
  }

  // loadReactions fetches the artifact's current per-anchor tallies (the same
  // GET the trajectory viewer's server-render draws from) and renders a pill
  // for every anchor matching a trigger on the page, so reactions show on
  // reload — not just immediately after a click (#66).
  //
  // Which anchor kind carries this viewer's reactions depends on where it is
  // rendered (see anchorRefFor): md_block/md_bullet standing alone, bundle_file
  // scoped to `member` inside a bundle pane. Everything downstream — pills,
  // counts, whose reaction is "on" — is identical either way, which is the
  // point: a markdown member reads exactly like the standalone document.
  function loadReactions(viewer) {
    var id = artifactID();
    if (!id) return;
    var member = memberNameFor(viewer);
    fetch('/v1/artifacts/' + encodeURIComponent(id) + '/reactions', { credentials: 'same-origin' })
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (data) {
        if (!data || !data.reactions) return;
        data.reactions.forEach(function (t) {
          if (member === null) {
            if (t.anchor_type !== 'md_block' && t.anchor_type !== 'md_bullet') return;
          } else if (t.anchor_type !== 'bundle_file') {
            return;
          }
          var trigger = findTrigger(viewer, member, t.anchor_key);
          if (!trigger) return;
          setPillCount(pillFor(trigger, t.emoji), t.count, t.reacted);
        });
      })
      .catch(function () { /* reactions are progressive enhancement; a failed fetch just leaves no pills */ });
  }

  // findTrigger resolves a server tally's anchor_key (the canonical
  // `{"block_id":…}` / `{"block_id":…,"path":[…]}` / `{"block_id":…,"name":…}`
  // JSON the annotation core groups by) back to the react trigger it belongs
  // to, by comparing PARSED values rather than the raw string (the fields
  // round-trip through JSON identically either way, so this is exact without
  // depending on the server's key formatting).
  //
  // Inside a bundle, `member` is the pane's file name and every tally must
  // match it — one bundle's reactions cover all its members, and two members
  // can hold identical blocks, so the name is what keeps a tally on the file it
  // was left on. A key with a `path` addresses a bullet, one without addresses
  // the whole block; that is the same discriminator on both surfaces, so it is
  // read from the key rather than from an anchor type the bundle_file tally
  // does not carry.
  function findTrigger(viewer, member, anchorKeyJSON) {
    var key;
    try { key = JSON.parse(anchorKeyJSON); } catch (e) { return null; }
    if (member !== null && key.name !== member) return null;
    if (!key.block_id) return null;
    var wantPath = key.path || null;
    var triggers = viewer.querySelectorAll('.md-react[data-block-id="' + cssEscape(key.block_id) + '"]');
    for (var i = 0; i < triggers.length; i++) {
      var t = triggers[i];
      var isBullet = t.getAttribute('data-anchor-type') === 'md_bullet';
      if (!wantPath) {
        if (!isBullet) return t;
        continue;
      }
      if (!isBullet) continue;
      var path;
      try { path = JSON.parse(t.getAttribute('data-path') || '[]'); } catch (e2) { path = []; }
      if (wantPath.length !== path.length) continue;
      var match = true;
      for (var j = 0; j < wantPath.length; j++) { if (wantPath[j] !== path[j]) { match = false; break; } }
      if (match) return t;
    }
    return null;
  }

  // --- reaction picker (focus-trapped popover) ------------------------------

  var openPicker = null;

  function closePicker(restoreFocus) {
    if (!openPicker) return;
    var trigger = openPicker.trigger;
    openPicker.el.remove();
    openPicker = null;
    if (restoreFocus && trigger) trigger.focus();
  }

  // anchorRefFor derives a trigger's anchor from where it sits: the md_block /
  // md_bullet locators on a standalone markdown artifact, and the equivalent
  // member-scoped bundle_file locator inside a bundle's member pane. The bullet
  // `path` rides along in BOTH forms — dropping it inside a bundle collapsed
  // every bullet of a list onto its block, so the second bullet re-posted the
  // first one's anchor and the server no-oped the write.
  function anchorRefFor(trigger) {
    var blockID = trigger.getAttribute('data-block-id');
    var path = null;
    if (trigger.getAttribute('data-anchor-type') === 'md_bullet') {
      try { path = JSON.parse(trigger.getAttribute('data-path') || '[]'); } catch (e) { path = []; }
    }
    var member = memberNameFor(trigger);
    if (member !== null) {
      var ref = { name: member, block_id: blockID };
      if (path) ref.path = path;
      return { type: 'bundle_file', ref: ref };
    }
    if (path) return { type: 'md_bullet', ref: { block_id: blockID, path: path } };
    return { type: 'md_block', ref: { block_id: blockID } };
  }

  // notifyReaction tells the bundle viewer that one of ITS member's reactions
  // changed, so the file rail's aggregated engagement badge moves at click time
  // instead of only after a reload re-runs memberReactionCounts server-side
  // (#72). `delta` is gated on the status the server used to report whether
  // anything actually changed (SPEC-0006 "Idempotent Reactions": 201 on a real
  // create, 200 on a no-op repeat, 204 on every delete), so a repeat never
  // over-counts. A no-op on the standalone page, which has no rail.
  function notifyReaction(trigger, status) {
    var member = memberNameFor(trigger);
    if (member === null) return;
    var delta = status === 201 ? 1 : (status === 204 ? -1 : 0);
    if (!delta) return;
    document.dispatchEvent(new CustomEvent('cairn:member-reaction', {
      detail: { member: member, delta: delta }
    }));
  }

  function showPicker(trigger) {
    closePicker(false);
    var anchor = anchorRefFor(trigger);
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
      // A single, clean click path (#66, matching the trajectory viewer's fix):
      // close the picker WITH focus restore first, then post the reaction and
      // let the pill insert/update on success — no competing handlers racing to
      // tear down the popover. An already-rendered pill for this emoji is
      // toggled (reacting the same emoji again turns it off); otherwise a fresh
      // pill is posted and inserted at count 1.
      b.addEventListener('click', function () {
        closePicker(true);
        var existing = findPill(trigger, emoji);
        if (existing) { togglePill(trigger, existing); return; }
        reactionFetch('POST', anchor.type, anchor.ref, emoji, function (ok, status) {
          if (!ok) { handleReactError(trigger, status); return; }
          setPillCount(pillFor(trigger, emoji), 1, true);
          notifyReaction(trigger, status);
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
      if (e.key === 'Escape') {
        e.preventDefault();
        closePicker(true);
        return;
      }
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
    var trigger = e.target.closest ? e.target.closest('.md-react') : null;
    if (trigger) {
      e.preventDefault();
      if (openPicker && openPicker.trigger === trigger) {
        closePicker(true);
      } else {
        showPicker(trigger);
      }
      return;
    }
    // A click outside an open picker dismisses it.
    if (openPicker && !e.target.closest('.md-react-picker')) {
      closePicker(false);
    }
  });

  // --- md_bullet affordance -------------------------------------------------

  // For every list block, inject a react `＋` to the left of each list item
  // carrying its ordinal path into the bullet tree, so a reader can react on a
  // single bullet (anchor md_bullet {block_id, path}).
  function decorateBullets(root) {
    var lists = root.querySelectorAll('[data-md-list] > .md-block-body');
    lists.forEach(function (body) {
      var block = body.closest('.md-block');
      var blockID = block ? block.getAttribute('data-block-id') : '';
      decorateList(body.querySelector('ul, ol'), blockID, []);
    });
  }

  function decorateList(list, blockID, prefix) {
    if (!list) return;
    var i = 0;
    Array.prototype.forEach.call(list.children, function (li) {
      if (li.tagName !== 'LI') return;
      var path = prefix.concat([i]);
      var btn = document.createElement('button');
      btn.type = 'button';
      btn.className = 'md-react md-bullet-react';
      btn.setAttribute('data-anchor-type', 'md_bullet');
      btn.setAttribute('data-block-id', blockID);
      btn.setAttribute('data-path', JSON.stringify(path));
      btn.setAttribute('aria-label', 'React to this item');
      btn.textContent = '＋';
      li.insertBefore(btn, li.firstChild);
      // Recurse into nested lists.
      var nested = li.querySelector(':scope > ul, :scope > ol');
      if (nested) decorateList(nested, blockID, path);
      i++;
    });
  }

  // --- select-text-to-comment ----------------------------------------------

  var selToolbar = null;

  function clearSelToolbar() {
    if (selToolbar) { selToolbar.remove(); selToolbar = null; }
  }

  // proseOffset maps a (node, offset) inside the prose to a character offset in
  // the prose's text content, so a selection yields {start, end} the
  // text_selection anchor stores alongside its quoted substring (SPEC-0006
  // "Anchor Stability": the anchor is re-checked by matching its quote at its
  // stored offsets against the body).
  //
  // Offset basis: this counts characters over the goldmark-rendered, sanitized
  // block HTML ONLY — i.e. `.md-block-body`'s text, the same text a reader
  // sees as prose. It explicitly EXCLUDES text inside `.md-react` elements
  // (both the per-block ＋ button viewer.go renders as a `.md-block` sibling of
  // `.md-block-body`, and the per-bullet ＋ buttons decorateBullets() injects
  // as the first child of each `<li>`, INSIDE the prose text stream) — without
  // this exclusion those injected glyphs shift every offset after the point
  // they're inserted, so a selection's {start, end} would not describe an
  // offset into the canonical block body the spec anchors against (PR #39
  // review note).
  function proseOffset(prose, node, offset) {
    var walker = document.createTreeWalker(prose, NodeFilter.SHOW_TEXT, {
      acceptNode: function (text) {
        return isInjectedAffordance(text) ? NodeFilter.FILTER_REJECT : NodeFilter.FILTER_ACCEPT;
      }
    });
    var count = 0, cur;
    while ((cur = walker.nextNode())) {
      if (cur === node) return count + offset;
      count += cur.nodeValue.length;
    }
    return count;
  }

  // isInjectedAffordance reports whether a text node lives inside one of the
  // reaction-trigger buttons (`.md-react`, which covers both the block-level
  // and md_bullet-level ＋ affordances) rather than the rendered prose itself.
  function isInjectedAffordance(textNode) {
    return !!(textNode.parentElement && textNode.parentElement.closest('.md-react'));
  }

  function onSelection() {
    var sel = window.getSelection();
    if (!sel || sel.isCollapsed || sel.rangeCount === 0) { clearSelToolbar(); return; }
    var prose = document.querySelector('.md-prose');
    if (!prose) return;
    var range = sel.getRangeAt(0);
    if (!prose.contains(range.commonAncestorContainer)) { clearSelToolbar(); return; }
    var quote = sel.toString().trim();
    if (!quote) { clearSelToolbar(); return; }

    var start = proseOffset(prose, range.startContainer, range.startOffset);
    var end = start + quote.length;

    clearSelToolbar();
    selToolbar = document.createElement('div');
    selToolbar.className = 'md-sel-toolbar';
    var btn = document.createElement('button');
    btn.type = 'button';
    btn.textContent = '💬 Comment';
    btn.setAttribute('aria-label', 'Comment on the selected text');
    btn.addEventListener('click', function () {
      startSelectionComment(start, end, quote);
    });
    selToolbar.appendChild(btn);
    document.body.appendChild(selToolbar);
    var r = range.getBoundingClientRect();
    selToolbar.style.top = (window.scrollY + r.top - 42) + 'px';
    selToolbar.style.left = (window.scrollX + r.left) + 'px';
  }

  // startSelectionComment wires the captured text_selection anchor into the
  // shell's existing comment composer (hidden sel_start/sel_end/quote fields the
  // web comment route reads), then focuses the textarea so the reader types the
  // comment and posts it through the normal CSRF-guarded path.
  function startSelectionComment(start, end, quote) {
    var form = document.querySelector('.comment-composer');
    var textarea = document.getElementById('comment-body');
    if (!form || !textarea) { clearSelToolbar(); return; }
    setHidden(form, 'sel_start', String(start));
    setHidden(form, 'sel_end', String(end));
    setHidden(form, 'quote', quote);

    var chip = document.getElementById('md-sel-chip');
    if (!chip) {
      chip = document.createElement('p');
      chip.id = 'md-sel-chip';
      chip.className = 'md-stats';
      form.insertBefore(chip, form.firstChild);
    }
    chip.textContent = 'commenting on “' + (quote.length > 34 ? quote.slice(0, 34) + '…' : quote) + '”';
    clearSelToolbar();
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

  function resetComposerAnchor() {
    var form = document.querySelector('.comment-composer');
    if (!form) return;
    ['sel_start', 'sel_end', 'quote'].forEach(function (n) {
      var i = form.querySelector('input[name="' + n + '"]');
      if (i) i.value = '';
    });
    var chip = document.getElementById('md-sel-chip');
    if (chip) chip.remove();
  }

  // --- TOC active flash on jump --------------------------------------------

  function flashTOC() {
    var hash = window.location.hash.slice(1);
    if (!hash) return;
    document.querySelectorAll('.md-toc-link.toc-active').forEach(function (a) {
      a.classList.remove('toc-active');
    });
    var link = document.querySelector('.md-toc-link[href="#' + cssEscape(hash) + '"]');
    if (link) link.classList.add('toc-active');
  }

  function cssEscape(s) {
    return (window.CSS && window.CSS.escape) ? window.CSS.escape(s) : s.replace(/[^\w-]/g, '\\$&');
  }

  // --- init -----------------------------------------------------------------

  // hydrateViewer brings one freshly-rendered .md-viewer up to full interactive
  // state. It runs on load AND after every bundle pane swap: the server sends
  // annotation-agnostic markup (ADR-0002), so a member swapped in by HTMX
  // arrives with neither the injected md_bullet affordances nor any reaction
  // state, and without this a reader could react per-bullet on the member the
  // page happened to load with but on no other.
  function hydrateViewer(viewer) {
    if (!viewer) return;
    decorateBullets(viewer);
    // decorateBullets must run first so md_bullet triggers exist on the page
    // for loadReactions to match tallies against (#66).
    loadReactions(viewer);
  }

  function init() {
    hydrateViewer(document.querySelector('.md-viewer'));
    // The listeners below are registered even when this page has no markdown
    // viewer YET — a bundle whose first member is a non-previewable file still
    // reaches a markdown member one pane swap later.
    document.body.addEventListener('htmx:afterSwap', function (e) {
      if (!e.target || e.target.id !== 'bundle-pane') return;
      hydrateViewer(e.target.querySelector('.md-viewer'));
    });
    document.addEventListener('mouseup', onSelection);
    document.addEventListener('keyup', function (e) {
      if (e.shiftKey || e.key === 'Shift') onSelection();
    });
    window.addEventListener('hashchange', flashTOC);
    document.body.addEventListener('htmx:afterOnLoad', function (e) {
      if (e.target && e.target.closest && e.target.closest('.comment-composer')) {
        resetComposerAnchor();
      }
    });
    flashTOC();
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
