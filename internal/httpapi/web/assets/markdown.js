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

  // postReaction sends an idempotent reaction to the annotation API. A session
  // (cookie) caller rides the double-submit CSRF header the server matches; a
  // token caller is exempt. Failures are surfaced inline, never thrown.
  function postReaction(anchorType, anchorRef, emoji, onDone) {
    var id = artifactID();
    if (!id) return;
    fetch('/v1/artifacts/' + encodeURIComponent(id) + '/reactions', {
      method: 'POST',
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

  // --- reaction picker (focus-trapped popover) ------------------------------

  var openPicker = null;

  function closePicker(restoreFocus) {
    if (!openPicker) return;
    var trigger = openPicker.trigger;
    openPicker.el.remove();
    openPicker = null;
    if (restoreFocus && trigger) trigger.focus();
  }

  function anchorRefFor(trigger) {
    var type = trigger.getAttribute('data-anchor-type');
    if (type === 'md_bullet') {
      return {
        type: type,
        ref: { block_id: trigger.getAttribute('data-block-id'), path: JSON.parse(trigger.getAttribute('data-path')) }
      };
    }
    return { type: 'md_block', ref: { block_id: trigger.getAttribute('data-block-id') } };
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
      b.addEventListener('click', function () {
        postReaction(anchor.type, anchor.ref, emoji, function (ok, status) {
          // A signed-out reaction 401s: route to login with a return path
          // instead of silently swallowing the failure.
          if (!ok && status === 401) {
            window.location.href = '/login?next=' + encodeURIComponent(window.location.pathname);
            return;
          }
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

  function init() {
    var viewer = document.querySelector('.md-viewer');
    if (!viewer) return;
    decorateBullets(viewer);
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
