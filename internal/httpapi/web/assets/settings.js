// Settings page interactivity (issue #75). Written as plain vanilla JS, not
// an Alpine component: the create-token and revoke-token actions each mutate
// an arbitrary-length token list and need real DOM insertion, which — like
// the Bin filter and the Share dialog in app.js — the Alpine CSP build's
// bare-identifier-only directives cannot express cleanly. This talks
// straight to the SAME POST/GET/DELETE /v1/tokens JSON endpoints a scripted
// bearer caller uses (pat.go): there is exactly one implementation of
// "mint/list/revoke a PAT" in Cairn, and this page is only a view over it.
//
// CSP note: everything here is same-origin fetch() + DOM APIs, no eval or
// inline handlers, so it runs cleanly under the shell's `script-src 'self'`
// (webCSP, web.go) and the JSON API's `connect-src 'self'`.
(function () {
  // readCSRFCookie mirrors app.js's htmx:configRequest reader: the double-
  // submit CSRF token lives in the readable cairn_csrf cookie the session
  // login sets, and every mutating fetch here echoes it in the X-CSRF-Token
  // header so enforceCSRF (csrf.go) accepts the request (SPEC-0006 REQ "CSRF
  // Protection").
  function readCSRFCookie() {
    var m = document.cookie.match(/(?:^|;\s*)cairn_csrf=([^;]+)/);
    return m ? decodeURIComponent(m[1]) : '';
  }

  // relativeAgo renders an ISO timestamp as a compact relative age, mirroring
  // web.go's humanizeSince closely enough for the couple of seconds between a
  // token's creation and this script rendering its row — a full page reload
  // always re-renders the exact server-computed string.
  function relativeAgo(iso) {
    var then = new Date(iso).getTime();
    if (isNaN(then)) return '';
    var seconds = Math.max(0, (Date.now() - then) / 1000);
    if (seconds < 60) return 'just now';
    var minutes = seconds / 60;
    if (minutes < 60) return Math.floor(minutes) + 'm ago';
    var hours = minutes / 60;
    if (hours < 24) return Math.floor(hours) + 'h ago';
    return Math.floor(hours / 24) + 'd ago';
  }

  function buildTokenRow(tok) {
    var tr = document.createElement('tr');
    tr.className = 'token-row';
    tr.id = 'token-' + tok.id;
    tr.dataset.tokenId = tok.id;

    var nameCell = document.createElement('td');
    nameCell.className = 'token-cell-name';
    nameCell.textContent = tok.name;
    if (tok.is_agent) {
      var badge = document.createElement('span');
      badge.className = 'token-agent-badge';
      badge.title = 'minted for an agent';
      badge.textContent = '◆ agent';
      nameCell.appendChild(document.createTextNode(' '));
      nameCell.appendChild(badge);
    }

    var scopeCell = document.createElement('td');
    scopeCell.className = 'token-cell-scopes';
    var scopeCode = document.createElement('code');
    scopeCode.textContent = (tok.scopes || []).join(' ');
    scopeCell.appendChild(scopeCode);

    var createdCell = document.createElement('td');
    createdCell.className = 'token-cell-created';
    createdCell.textContent = relativeAgo(tok.created_at);

    var lastUsedCell = document.createElement('td');
    lastUsedCell.className = 'token-cell-lastused';
    lastUsedCell.textContent = 'never';

    var actionCell = document.createElement('td');
    actionCell.className = 'token-cell-action';
    var revokeBtn = document.createElement('button');
    revokeBtn.type = 'button';
    revokeBtn.className = 'token-revoke-btn';
    revokeBtn.dataset.tokenRevoke = '1';
    revokeBtn.dataset.tokenId = tok.id;
    revokeBtn.dataset.tokenName = tok.name;
    revokeBtn.textContent = 'Revoke';
    actionCell.appendChild(revokeBtn);

    tr.appendChild(nameCell);
    tr.appendChild(scopeCell);
    tr.appendChild(createdCell);
    tr.appendChild(lastUsedCell);
    tr.appendChild(actionCell);
    return tr;
  }

  function initTokens() {
    var form = document.querySelector('[data-token-form]');
    var rowsBody = document.querySelector('[data-token-rows]');
    var secretCard = document.querySelector('[data-token-secret]');
    var secretValue = document.querySelector('[data-token-secret-value]');
    var secretLive = document.querySelector('[data-token-secret-live]');
    var errorBox = document.querySelector('[data-token-error]');
    if (!form || !rowsBody) return;

    function showError(msg) {
      if (!errorBox) return;
      errorBox.textContent = msg;
      errorBox.hidden = false;
    }
    function clearError() {
      if (!errorBox) return;
      errorBox.hidden = true;
      errorBox.textContent = '';
    }

    form.addEventListener('submit', function (e) {
      e.preventDefault();
      clearError();
      var data = new FormData(form);
      var name = (data.get('name') || '').toString().trim();
      var scopes = data.getAll('scopes');
      var isAgent = data.get('is_agent') === '1';
      if (!name) {
        showError('Name is required.');
        return;
      }
      if (scopes.length === 0) {
        showError('Select at least one scope.');
        return;
      }
      var submitBtn = form.querySelector('.token-create-btn');
      if (submitBtn) submitBtn.disabled = true;

      fetch('/v1/tokens', {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          'X-CSRF-Token': readCSRFCookie(),
        },
        body: JSON.stringify({ name: name, scopes: scopes, is_agent: isAgent }),
        credentials: 'same-origin',
      })
        .then(function (resp) {
          return resp.json().then(function (body) { return { ok: resp.ok, body: body }; });
        })
        .then(function (result) {
          if (!result.ok) {
            var msg = (result.body && result.body.error && result.body.error.message) || 'Could not create token.';
            showError(msg);
            return;
          }
          var tok = result.body;
          // Reveal the one-time plaintext (issue #75: "show the plaintext
          // ONCE with a copy button"). Focus moves to the reveal region so a
          // screen reader announces it immediately, and the live region
          // repeats the warning for anyone not looking at the screen.
          if (secretCard && secretValue) {
            secretValue.textContent = tok.token;
            secretCard.hidden = false;
            secretCard.setAttribute('tabindex', '-1');
            secretCard.focus();
            if (secretLive) secretLive.textContent = '⚠ Store this token now — you won’t see it again.';
          }
          var emptyRow = rowsBody.querySelector('[data-token-empty]');
          if (emptyRow) emptyRow.remove();
          rowsBody.insertBefore(buildTokenRow(tok), rowsBody.firstChild);
          form.reset();
        })
        .catch(function () {
          showError('Network error creating token. Try again.');
        })
        .finally(function () {
          if (submitBtn) submitBtn.disabled = false;
        });
    });

    var copyBtn = document.querySelector('[data-token-copy]');
    if (copyBtn && secretValue) {
      copyBtn.addEventListener('click', function () {
        var value = secretValue.textContent || '';
        var defaultLabel = copyBtn.textContent;
        var done = function () {
          copyBtn.textContent = 'copied ✓';
          setTimeout(function () { copyBtn.textContent = defaultLabel; }, 1300);
        };
        if (navigator.clipboard && navigator.clipboard.writeText) {
          navigator.clipboard.writeText(value).then(done).catch(done);
        } else {
          done();
        }
      });
    }

    // Revoke: event-delegated so it also covers a row this script just
    // inserted, with no separate re-bind step.
    rowsBody.addEventListener('click', function (e) {
      var btn = e.target.closest ? e.target.closest('[data-token-revoke]') : null;
      if (!btn) return;
      var id = btn.dataset.tokenId;
      var name = btn.dataset.tokenName || 'this token';
      if (!id) return;
      if (!window.confirm('Revoke "' + name + '"? Anything using it stops working immediately.')) {
        return;
      }
      btn.disabled = true;
      fetch('/v1/tokens/' + encodeURIComponent(id), {
        method: 'DELETE',
        headers: { 'X-CSRF-Token': readCSRFCookie() },
        credentials: 'same-origin',
      })
        .then(function (resp) {
          if (!resp.ok && resp.status !== 204) {
            btn.disabled = false;
            return;
          }
          var row = document.getElementById('token-' + id);
          if (!row) return;
          row.classList.add('token-row-revoked');
          var actionCell = row.querySelector('.token-cell-action');
          if (actionCell) {
            actionCell.textContent = '';
            var label = document.createElement('span');
            label.className = 'token-revoked-label';
            label.textContent = 'revoked just now';
            actionCell.appendChild(label);
          }
        })
        .catch(function () {
          btn.disabled = false;
        });
    });
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', initTokens);
  } else {
    initTokens();
  }
})();
