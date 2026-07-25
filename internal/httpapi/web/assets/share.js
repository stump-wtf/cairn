// Share dialog owner controls (issue #94, SPEC-0009 REQ "Owner-Only Policy
// Changes", REQ "Id Rotation as Revoke-a-Leaked-Link", REQ "Default 7-Day
// TTL, Owner-Adjustable, Visible Countdown"). Plain vanilla JS, not an Alpine
// component — same reasoning as settings.js: these mutations need real
// network round trips and, for rotate, a full-page redirect, which the
// Alpine CSP build's bare-identifier-only directives cannot express cleanly.
// This talks straight to the owner-only PATCH/POST /v1/artifacts/{id}/policy|
// ttl|rotate JSON endpoints (policy.go) — there is exactly one implementation
// of "change sharing/TTL/rotate the id" in Cairn, and this is only a view
// over it, the same "one implementation, one view" discipline settings.js
// follows for tokens/MCP sessions.
//
// CSP note: everything here is same-origin fetch() + DOM APIs, no eval or
// inline handlers, so it runs cleanly under the shell's `script-src 'self'`
// (webCSP, web.go) and the JSON API's `connect-src 'self'`. The template
// (base.html share-dialog) never renders these controls for a non-owner, so
// this script has nothing to wire on a read-only view (the querySelectors
// below simply find nothing and every init function no-ops).
(function () {
  // readCSRFCookie mirrors app.js's htmx:configRequest reader and settings.js's
  // own copy: the double-submit CSRF token lives in the readable cairn_csrf
  // cookie the session login sets, echoed back in X-CSRF-Token so enforceCSRF
  // (csrf.go) accepts the request (SPEC-0006 REQ "CSRF Protection").
  function readCSRFCookie() {
    var m = document.cookie.match(/(?:^|;\s*)cairn_csrf=([^;]+)/);
    return m ? decodeURIComponent(m[1]) : '';
  }

  // accessLabel mirrors web.go's shareAccessLabel so the dialog's access line
  // updates instantly from the mutation response without a full reload.
  function accessLabel(visibility) {
    return visibility === 'private' ? '🔒 you only' : '🔒 you + anyone with link';
  }

  // applyExpiry writes the server-computed countdown (artifactResponse's
  // expires_in) into every place on the page that shows it: the Share
  // dialog's own line and the shell/trajectory panel's provenance line, so a
  // TTL change is reflected everywhere without a reload (SPEC-0009 REQ
  // "...MUST update expires_at and reflect the new countdown everywhere").
  function applyExpiry(expiresIn) {
    var text = expiresIn || 'expired';
    document.querySelectorAll('[data-share-expires]').forEach(function (el) { el.textContent = text; });
    document.querySelectorAll('[data-provenance-expires]').forEach(function (el) { el.textContent = text; });
  }

  function jsonFetch(url, method, body) {
    return fetch(url, {
      method: method,
      headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': readCSRFCookie() },
      body: body === undefined ? undefined : JSON.stringify(body),
      credentials: 'same-origin',
    }).then(function (resp) {
      return resp.json().catch(function () { return {}; }).then(function (data) {
        return { ok: resp.ok, status: resp.status, body: data };
      });
    });
  }

  function errorMessage(result, fallback) {
    return (result.body && result.body.error && result.body.error.message) || fallback;
  }

  function initShareOwnerControls() {
    var dialog = document.querySelector('[data-share-dialog]');
    var controls = document.querySelector('[data-share-owner-controls]');
    if (!dialog || !controls) return; // non-owner / signed-out view: nothing to wire

    var id = dialog.getAttribute('data-share-id');
    if (!id) return;
    var base = '/v1/artifacts/' + encodeURIComponent(id);
    var live = controls.querySelector('[data-share-owner-live]');

    function announce(msg) {
      if (live) live.textContent = msg;
    }

    // TTL: extend or shorten expiry to N days from now.
    var ttlInput = controls.querySelector('[data-share-ttl-input]');
    var ttlBtn = controls.querySelector('[data-share-ttl-save]');
    if (ttlInput && ttlBtn) {
      ttlBtn.addEventListener('click', function () {
        var days = parseInt(ttlInput.value, 10);
        if (!days || days < 1 || days > 30) {
          announce('Enter a number of days between 1 and 30.');
          ttlInput.focus();
          return;
        }
        ttlBtn.disabled = true;
        jsonFetch(base + '/ttl', 'PATCH', { ttl_seconds: days * 86400 })
          .then(function (result) {
            if (!result.ok) {
              announce(errorMessage(result, 'Could not update expiry.'));
              return;
            }
            applyExpiry(result.body.expires_in);
            ttlInput.value = '';
            announce('Expiry updated — ' + (result.body.expires_in || 'expired') + '.');
          })
          .catch(function () { announce('Network error updating expiry. Try again.'); })
          .finally(function () { ttlBtn.disabled = false; });
      });
    }

    // Visibility: flip link access within the allowed set (ADR-0007).
    var visSelect = controls.querySelector('[data-share-visibility-select]');
    var visBtn = controls.querySelector('[data-share-visibility-save]');
    if (visSelect && visBtn) {
      visBtn.addEventListener('click', function () {
        var visibility = visSelect.value;
        visBtn.disabled = true;
        jsonFetch(base + '/policy', 'PATCH', { visibility: visibility })
          .then(function (result) {
            if (!result.ok) {
              announce(errorMessage(result, 'Could not update link access.'));
              return;
            }
            var label = document.querySelector('[data-share-access-label]');
            if (label) label.textContent = accessLabel(result.body.visibility);
            announce('Link access updated.');
          })
          .catch(function () { announce('Network error updating link access. Try again.'); })
          .finally(function () { visBtn.disabled = false; });
      });
    }

    // Rotate: mint a new id, invalidate the old one, redirect to the new
    // location. Destructive and irreversible, so it is gated on a native
    // confirm() (window.confirm — no custom modal needed, mirrors the
    // token/MCP-session revoke confirms in settings.js) whose message is
    // authored in the template via data-confirm.
    var rotateBtn = controls.querySelector('[data-share-rotate]');
    if (rotateBtn) {
      rotateBtn.addEventListener('click', function () {
        var msg = rotateBtn.getAttribute('data-confirm') || 'Rotate this link? The old one stops working immediately.';
        if (!window.confirm(msg)) return;
        rotateBtn.disabled = true;
        jsonFetch(base + '/rotate', 'POST')
          .then(function (result) {
            if (!result.ok) {
              announce(errorMessage(result, 'Could not rotate the link.'));
              rotateBtn.disabled = false;
              return;
            }
            announce('Link rotated — redirecting to the new link…');
            window.location.href = result.body.url;
          })
          .catch(function () {
            announce('Network error rotating the link. Try again.');
            rotateBtn.disabled = false;
          });
      });
    }
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', initShareOwnerControls);
  } else {
    initShareOwnerControls();
  }
})();
