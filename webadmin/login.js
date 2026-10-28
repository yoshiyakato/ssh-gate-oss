// login.js — the /login page of the headless Web admin (phase6-headless.md
// §6.3). POST /api/login {"token"} or {"password"} with fetch (guardHTTP requires JSON, and
// the API guard requires X-SSH-Gete-CSRF), then go to ?next= when it is a
// same-origin path. No inline script: CSP is script-src 'self' (§6.8).
(function (root) {
  'use strict';

  var TEXT = {
    ja: {
      lead: '管理トークンか管理パスワードを入力してください。',
      label: '管理トークンまたはパスワード',
      submit: 'ログイン',
      hint: 'トークンはサーバーで ssh-gete --print-admin-token を実行すると表示されます。パスワードは ssh-gete --set-admin-password で設定できます。',
      empty: 'トークンかパスワードを入力してください。',
      invalid: 'トークンかパスワードが違います。',
      locked: '失敗が続いたため、しばらくログインできません。',
      stopping: 'サーバーは停止中です。',
      failed: 'ログインできませんでした',
    },
    en: {
      lead: 'Enter the admin token or the admin password.',
      label: 'Admin token or password',
      submit: 'Log in',
      hint: 'Run ssh-gete --print-admin-token on the server to show the token. Set a password with ssh-gete --set-admin-password.',
      empty: 'Enter the token or the password.',
      invalid: 'The token or password is not correct.',
      locked: 'Too many failed attempts. Try again later.',
      stopping: 'The server is shutting down.',
      failed: 'Could not log in',
    },
  };

  function pickLang(languages) {
    var list = languages || [];
    for (var i = 0; i < list.length; i++) {
      var l = String(list[i] || '').toLowerCase();
      if (l.indexOf('ja') === 0) return 'ja';
      if (l.indexOf('en') === 0) return 'en';
    }
    return 'ja';
  }

  // safeNext accepts only a path on this origin: "/x", not "//host",
  // "/\host", "http:", or anything with a control character.
  function safeNext(search) {
    var next = '/';
    try {
      var v = new URLSearchParams(search || '').get('next');
      if (v) next = v;
    } catch (_) {
      return '/';
    }
    if (next.charAt(0) !== '/' || next.charAt(1) === '/' || next.charAt(1) === '\\') return '/';
    if (/[\u0000-\u001f\u007f\\]/.test(next)) return '/';
    if (next === '/login' || next.indexOf('/login?') === 0 || next.indexOf('/login/') === 0) return '/';
    return next;
  }

  function messageFor(status, t) {
    if (status === 401) return t.invalid;
    if (status === 429) return t.locked;
    if (status === 503) return t.stopping;
    return t.failed + ' (HTTP ' + status + ')';
  }

  // loginBody sends an admin token (always "sgadm_..."; the server refuses a
  // password with that prefix) as {"token"} and anything else as
  // {"password"}, untrimmed: the server rejects passwords with outer spaces.
  function loginBody(secret) {
    var raw = String(secret == null ? '' : secret);
    var trimmed = raw.trim();
    if (trimmed.indexOf('sgadm_') === 0) return { token: trimmed };
    return { password: raw };
  }

  // The login response carries a per-login session key (X-SSH-Gete-Session-Key).
  // It is kept in localStorage, which only this origin (scheme, host and
  // port) can read, and the shim sends it on every /api/* request. The
  // cookie alone is not enough: browsers also send it to other ports of
  // the same host.
  var SESSION_KEY_ITEM = 'ssh-gete-session-key';
  var SESSION_KEY_HEADER = 'X-SSH-Gete-Session-Key';

  function saveSessionKey(win, key) {
    if (!key) return;
    try {
      win.localStorage.setItem(SESSION_KEY_ITEM, key);
      return;
    } catch (_) {}
    try {
      win.sessionStorage.setItem(SESSION_KEY_ITEM, key);
    } catch (_) {}
  }

  function login(fetchFn, secret, onKey) {
    return fetchFn('/api/login', {
      method: 'POST',
      credentials: 'same-origin',
      cache: 'no-store',
      headers: { 'Content-Type': 'application/json', 'X-SSH-Gete-CSRF': '1' },
      body: JSON.stringify(loginBody(secret)),
    }).then(function (res) {
      if ((res.status === 204 || res.status === 200) && onKey && res.headers && typeof res.headers.get === 'function') {
        onKey(res.headers.get(SESSION_KEY_HEADER) || '');
      }
      return res.status;
    });
  }

  function init(doc, win) {
    var lang = pickLang(win.navigator && (win.navigator.languages || [win.navigator.language]));
    var t = TEXT[lang];
    doc.documentElement.lang = lang;
    var nodes = doc.querySelectorAll('[data-i18n]');
    for (var i = 0; i < nodes.length; i++) {
      var key = nodes[i].getAttribute('data-i18n');
      if (t[key]) nodes[i].textContent = t[key];
    }
    var form = doc.getElementById('login-form');
    var input = doc.getElementById('token');
    var button = doc.getElementById('submit');
    var message = doc.getElementById('message');
    if (!form || !input) return;
    input.focus();
    form.addEventListener('submit', function (ev) {
      ev.preventDefault();
      var secret = String(input.value || '');
      if (!secret.trim()) {
        message.textContent = t.empty;
        return;
      }
      button.disabled = true;
      message.textContent = '';
      login(win.fetch.bind(win), secret, function (key) {
        saveSessionKey(win, key);
      }).then(
        function (status) {
          if (status === 204 || status === 200) {
            input.value = '';
            win.location.replace(safeNext(win.location.search));
            return;
          }
          button.disabled = false;
          message.textContent = messageFor(status, t);
          input.select();
        },
        function (err) {
          button.disabled = false;
          message.textContent = t.failed + ': ' + (err && err.message ? err.message : String(err));
        }
      );
    });
  }

  var api = { pickLang: pickLang, safeNext: safeNext, messageFor: messageFor, loginBody: loginBody, login: login, init: init, saveSessionKey: saveSessionKey, SESSION_KEY_ITEM: SESSION_KEY_ITEM, TEXT: TEXT };
  if (typeof module === 'object' && module.exports) {
    module.exports = api;
  } else if (root && root.document) {
    if (root.document.readyState === 'loading') {
      root.document.addEventListener('DOMContentLoaded', function () { init(root.document, root); });
    } else {
      init(root.document, root);
    }
  }
})(typeof window !== 'undefined' ? window : undefined);
