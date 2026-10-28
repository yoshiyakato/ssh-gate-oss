// headless-shim.js — replaces the Wails bridge when ssh-gete runs with
// --headless and the frontend is served by the Web admin server.
//
// Spec: docs/spec/phase6-headless.md §3.1 (calls), §4 (events), §5 (shim),
// §6.3 (login / logout). The frontend sources are not changed: this classic
// script is injected right after <head> (§5.1), so it runs before the Vite
// module bundle and window.go / window.runtime already exist when the bundle
// imports wailsjs/go/main/App.js and wailsjs/runtime/runtime.js.
//
// No inline script, no eval: CSP is script-src 'self' (§6.8).
(function () {
  'use strict';

  var w = window;
  var CSRF_HEADER = 'X-SSH-Gete-CSRF';
  // The per-login session key that login.js stored (see login.js). Sent on
  // every /api/* request: the cookie alone is not accepted there.
  var SESSION_HEADER = 'X-SSH-Gete-Session';
  var SESSION_KEY_ITEM = 'ssh-gete-session-key';

  function sessionKey() {
    try {
      var v = w.localStorage.getItem(SESSION_KEY_ITEM);
      if (v) return v;
    } catch (_) {}
    try {
      return w.sessionStorage.getItem(SESSION_KEY_ITEM) || '';
    } catch (_) {
      return '';
    }
  }

  function forgetSessionKey() {
    try {
      w.localStorage.removeItem(SESSION_KEY_ITEM);
    } catch (_) {}
    try {
      w.sessionStorage.removeItem(SESSION_KEY_ITEM);
    } catch (_) {}
  }

  // apiHeaders adds the session key (read each time: another tab may have
  // logged in again and replaced it).
  function apiHeaders(h) {
    var k = sessionKey();
    if (k) h[SESSION_HEADER] = k;
    return h;
  }
  var LOGIN_PATH = '/login';
  var BACKOFF_MIN_MS = 500;
  var BACKOFF_MAX_MS = 10000;

  // ---------------------------------------------------------------- login

  var redirecting = false;

  function currentPath() {
    var l = w.location;
    return (l.pathname || '/') + (l.search || '') + (l.hash || '');
  }

  // §4.3 step 6 / §5.2: a 401 sends the page to /login?next=<current path>.
  function goLogin() {
    if (redirecting) return;
    redirecting = true;
    stopEvents();
    w.location.assign(LOGIN_PATH + '?next=' + encodeURIComponent(currentPath()));
  }

  // ---------------------------------------------------------------- calls

  function errorText(body) {
    return body && typeof body.error === 'string' ? body.error : '';
  }

  function readJSON(res) {
    return res.text().then(function (text) {
      if (!text) return null;
      try {
        return JSON.parse(text);
      } catch (_) {
        return null;
      }
    });
  }

  function messageOf(err) {
    if (typeof err === 'string') return err;
    return err && err.message ? String(err.message) : String(err);
  }

  // §3.1 / §5.2. Resolves with `result`; rejects with a plain string like Wails v2.
  function call(name, args) {
    if (name === 'StartTerminal' && args.length > 0) trackTerminal(args[0]);
    var req;
    try {
      req = w.fetch('/api/call/' + encodeURIComponent(name), {
        method: 'POST',
        credentials: 'same-origin',
        cache: 'no-store',
        headers: apiHeaders({ 'Content-Type': 'application/json', 'X-SSH-Gete-CSRF': '1' }),
        body: JSON.stringify(args),
      });
    } catch (err) {
      return Promise.reject(messageOf(err));
    }
    return Promise.resolve(req).then(
      function (res) {
        return readJSON(res).then(function (body) {
          if (res.status === 401) {
            goLogin();
            throw errorText(body) || 'login required';
          }
          if (res.status !== 200) {
            throw 'HTTP ' + res.status + ': ' + (errorText(body) || res.statusText || 'error');
          }
          if (body && typeof body === 'object' && 'error' in body && body.error != null) {
            throw String(body.error);
          }
          return body && typeof body === 'object' && 'result' in body ? body.result : null;
        });
      },
      function (err) {
        throw messageOf(err);
      }
    );
  }

  var appProxy = new Proxy(
    {},
    {
      get: function (_, name) {
        // `then` must stay undefined so the proxy is never mistaken for a thenable.
        if (typeof name !== 'string' || name === 'then') return undefined;
        return function () {
          return call(name, Array.prototype.slice.call(arguments));
        };
      },
    }
  );

  w.go = { main: { App: appProxy } };

  // ---------------------------------------------------------------- local event bus (Wails events.js semantics)

  var listeners = Object.create(null);

  function Listener(eventName, callback, maxCallbacks) {
    this.eventName = eventName;
    this.callback = callback;
    this.maxCallbacks = maxCallbacks || -1;
  }

  function listenerOff(listener) {
    var list = listeners[listener.eventName];
    if (!list) return;
    var i = list.indexOf(listener);
    if (i >= 0) list.splice(i, 1);
    if (list.length === 0) delete listeners[listener.eventName];
  }

  function EventsOnMultiple(eventName, callback, maxCallbacks) {
    var listener = new Listener(eventName, callback, maxCallbacks);
    (listeners[eventName] = listeners[eventName] || []).push(listener);
    return function () {
      listenerOff(listener);
    };
  }

  // dispatch delivers data (an argument array) to the local subscribers.
  function dispatch(eventName, data) {
    var list = listeners[eventName];
    if (!list) return;
    var snapshot = list.slice();
    for (var i = 0; i < snapshot.length; i++) {
      var l = snapshot[i];
      if (list.indexOf(l) < 0) continue; // removed by an earlier callback
      if (l.maxCallbacks !== -1) {
        l.maxCallbacks -= 1;
        if (l.maxCallbacks <= 0) listenerOff(l);
      }
      try {
        l.callback.apply(null, data);
      } catch (err) {
        w.console.error('[headless] listener for ' + eventName + ' threw', err);
      }
    }
  }

  // ---------------------------------------------------------------- SSE (§4.1, §4.3)

  // createSSEParser returns feed(chunk) for text chunks; onEvent({id, data})
  // fires per dispatched event. Comments (": ping") and unknown fields are ignored.
  function createSSEParser(onEvent) {
    var buf = '';
    var dataLines = [];
    var id = null;
    var hasData = false;

    function line(l) {
      if (l === '') {
        if (hasData) onEvent({ id: id, data: dataLines.join('\n') });
        dataLines = [];
        hasData = false;
        id = null;
        return;
      }
      if (l.charAt(0) === ':') return;
      var c = l.indexOf(':');
      var field = c < 0 ? l : l.slice(0, c);
      var value = c < 0 ? '' : l.slice(c + 1);
      if (value.charAt(0) === ' ') value = value.slice(1);
      if (field === 'data') {
        dataLines.push(value);
        hasData = true;
      } else if (field === 'id') {
        if (value.indexOf('\0') < 0) id = value;
      }
    }

    return function feed(chunk) {
      buf += chunk;
      for (;;) {
        var m = /\r\n|\r|\n/.exec(buf);
        if (!m) break;
        // A lone \r at the end may be the first half of \r\n: wait for more.
        if (m[0] === '\r' && m.index === buf.length - 1) break;
        line(buf.slice(0, m.index));
        buf = buf.slice(m.index + m[0].length);
      }
    };
  }

  var lastEventId = null;
  var backoff = BACKOFF_MIN_MS;
  var reconnectTimer = null;
  var activeReader = null;
  var stopped = false;
  var generation = 0;

  // known[id] = cumulative bytes this tab has accounted for (§4.1 `o`).
  var known = Object.create(null);
  var exited = Object.create(null);

  function trackTerminal(id) {
    if (typeof id !== 'string' || !id) return;
    if (!(id in known)) known[id] = 0;
    delete exited[id];
  }

  function handleResync(d) {
    d = d || {};
    var offsets = d.offsets && typeof d.offsets === 'object' ? d.offsets : {};
    var exits = Array.isArray(d.exits) ? d.exits : [];
    var ids = Object.keys(known);
    var i;
    // 1. Gaps: terminal:data {id, data:"", dropped:N}.
    for (i = 0; i < ids.length; i++) {
      var id = ids[i];
      var off = offsets[id];
      if (typeof off !== 'number') continue;
      var gap = off - known[id];
      known[id] = off;
      if (gap > 0) dispatch('terminal:data', [{ id: id, data: '', dropped: gap }]);
    }
    // 2. Exits for terminals this page knows about.
    for (i = 0; i < exits.length; i++) {
      var ex = exits[i];
      if (!ex || typeof ex.id !== 'string') continue;
      if (!(ex.id in known) || exited[ex.id]) continue;
      exited[ex.id] = true;
      delete known[ex.id];
      dispatch('terminal:exit', [ex]);
    }
    // 3. The fresh sessions snapshot.
    if (d.sessions != null) dispatch('sessions:changed', [d.sessions]);
  }

  function handleMessage(msg) {
    if (msg.id != null) lastEventId = msg.id;
    var ev;
    try {
      ev = JSON.parse(msg.data);
    } catch (_) {
      return;
    }
    if (!ev || typeof ev.n !== 'string') return;
    var d = ev.d;
    switch (ev.n) {
      case 'headless:resync':
        handleResync(d);
        break;
      case 'headless:shutdown':
        setBadge('shutdown');
        break;
      case 'terminal:data':
        if (d && typeof d.id === 'string' && typeof ev.o === 'number' && !exited[d.id]) {
          known[d.id] = ev.o;
        }
        break;
      case 'terminal:exit':
        if (d && typeof d.id === 'string') {
          if (exited[d.id]) return; // already delivered (e.g. by a resync)
          exited[d.id] = true;
          delete known[d.id];
        }
        break;
    }
    dispatch(ev.n, [d]);
  }

  function scheduleReconnect(gen) {
    if (stopped || gen !== generation || reconnectTimer !== null) return;
    if (badgeState !== 'shutdown') setBadge('offline');
    var delay = backoff;
    backoff = Math.min(backoff * 2, BACKOFF_MAX_MS);
    reconnectTimer = w.setTimeout(function () {
      reconnectTimer = null;
      connectEvents();
    }, delay);
  }

  function connectEvents() {
    if (stopped) return;
    var gen = ++generation;
    var headers = apiHeaders({ Accept: 'text/event-stream', 'X-SSH-Gete-CSRF': '1' });
    if (lastEventId) headers['Last-Event-ID'] = lastEventId;
    var req;
    try {
      req = w.fetch('/api/events', { method: 'GET', credentials: 'same-origin', cache: 'no-store', headers: headers });
    } catch (_) {
      scheduleReconnect(gen);
      return;
    }
    Promise.resolve(req)
      .then(function (res) {
        if (gen !== generation) return;
        if (res.status === 401) {
          goLogin();
          return;
        }
        if (res.status !== 200 || !res.body) {
          scheduleReconnect(gen);
          return;
        }
        backoff = BACKOFF_MIN_MS;
        setBadge('online');
        var reader = res.body.getReader();
        activeReader = reader;
        var decoder = new w.TextDecoder();
        var feed = createSSEParser(handleMessage);
        function pump() {
          return reader.read().then(function (r) {
            if (gen !== generation) return;
            if (r.done) {
              feed(decoder.decode());
              throw new Error('stream closed');
            }
            feed(typeof r.value === 'string' ? r.value : decoder.decode(r.value, { stream: true }));
            return pump();
          });
        }
        return pump();
      })
      .catch(function () {
        if (gen === generation) activeReader = null;
        scheduleReconnect(gen);
      });
  }

  function stopEvents() {
    stopped = true;
    generation++;
    if (reconnectTimer !== null) {
      w.clearTimeout(reconnectTimer);
      reconnectTimer = null;
    }
    if (activeReader) {
      try {
        activeReader.cancel();
      } catch (_) {
        /* ignore */
      }
      activeReader = null;
    }
  }

  // ---------------------------------------------------------------- badge (§5.3 追加 UI)

  var ja = /^ja\b/i.test((w.navigator && w.navigator.language) || '');
  var TEXT = ja
    ? { connecting: '接続中…', online: '接続中', offline: '切断（再接続を待っています）', shutdown: 'サーバーが停止しました', logout: 'ログアウト' }
    : { connecting: 'Connecting…', online: 'Connected', offline: 'Disconnected (retrying)', shutdown: 'The server has stopped', logout: 'Log out' };
  var COLORS = { connecting: '#6c757d', online: '#198754', offline: '#dc3545', shutdown: '#dc3545' };

  var badgeState = 'connecting';
  var badge = null;
  var badgeDot = null;
  var badgeText = null;

  function setBadge(state) {
    badgeState = state;
    if (!badge) return;
    badge.setAttribute('data-state', state);
    badgeDot.style.background = COLORS[state] || COLORS.connecting;
    badge.style.borderColor = state === 'online' ? '#ced4da' : COLORS[state] || '#ced4da';
    badgeText.textContent = 'headless · ' + (TEXT[state] || state);
  }

  function logout() {
    stopEvents();
    var done = function () {
      redirecting = true;
      w.location.assign(LOGIN_PATH);
    };
    Promise.resolve()
      .then(function () {
        return w.fetch('/api/logout', {
          method: 'POST',
          credentials: 'same-origin',
          cache: 'no-store',
          headers: apiHeaders({ 'Content-Type': 'application/json', 'X-SSH-Gete-CSRF': '1' }),
          body: '{}',
        });
      })
      .then(function () {
        forgetSessionKey();
        done();
      }, function () {
        forgetSessionKey();
        done();
      });
  }

  function mountBadge() {
    var doc = w.document;
    if (badge || !doc || !doc.body) return;
    badge = doc.createElement('div');
    badge.id = 'ssh-gete-headless-badge';
    badge.setAttribute('role', 'status');
    var s = badge.style;
    s.position = 'fixed';
    s.right = '8px';
    s.bottom = '8px';
    s.zIndex = '2147483000';
    s.display = 'flex';
    s.alignItems = 'center';
    s.gap = '6px';
    s.padding = '2px 8px';
    s.font = '12px/1.6 system-ui, sans-serif';
    s.color = '#212529';
    s.background = 'rgba(255,255,255,0.92)';
    s.border = '1px solid #ced4da';
    s.borderRadius = '12px';
    badgeDot = doc.createElement('span');
    badgeDot.style.width = '8px';
    badgeDot.style.height = '8px';
    badgeDot.style.borderRadius = '50%';
    badgeText = doc.createElement('span');
    var btn = doc.createElement('button');
    btn.type = 'button';
    btn.textContent = TEXT.logout;
    btn.style.font = 'inherit';
    btn.style.padding = '0 6px';
    btn.style.border = '1px solid #adb5bd';
    btn.style.borderRadius = '8px';
    btn.style.background = 'transparent';
    btn.style.cursor = 'pointer';
    btn.addEventListener('click', logout);
    badge.appendChild(badgeDot);
    badge.appendChild(badgeText);
    badge.appendChild(btn);
    doc.body.appendChild(badge);
    setBadge(badgeState);
  }

  // ---------------------------------------------------------------- window.runtime (§5.3)

  function noop() {}
  function resolved(v) {
    return function () {
      return Promise.resolve(v);
    };
  }
  function log(method) {
    return function (message) {
      var c = w.console;
      (c[method] || c.log).call(c, message);
    };
  }

  var envPromise = null;
  function Environment() {
    if (!envPromise) {
      envPromise = Promise.resolve()
        .then(function () {
          return w.fetch('/api/env', {
            method: 'GET',
            credentials: 'same-origin',
            cache: 'no-store',
            headers: apiHeaders({ 'X-SSH-Gete-CSRF': '1' }),
          });
        })
        .then(function (res) {
          return readJSON(res).then(function (body) {
            if (res.status === 401) {
              goLogin();
              throw errorText(body) || 'login required';
            }
            if (res.status !== 200 || !body) throw 'HTTP ' + res.status + ': ' + (errorText(body) || 'error');
            return { buildType: body.buildType || 'production', platform: body.platform || '', arch: body.arch || '' };
          });
        })
        .catch(function (err) {
          envPromise = null; // let a later call retry
          throw messageOf(err);
        });
    }
    return envPromise;
  }

  var runtime = {
    LogPrint: log('log'),
    LogTrace: log('debug'),
    LogDebug: log('debug'),
    LogInfo: log('info'),
    LogWarning: log('warn'),
    LogError: log('error'),
    LogFatal: log('error'),

    EventsOnMultiple: EventsOnMultiple,
    EventsOn: function (eventName, callback) {
      return EventsOnMultiple(eventName, callback, -1);
    },
    EventsOnce: function (eventName, callback) {
      return EventsOnMultiple(eventName, callback, 1);
    },
    EventsOff: function (eventName) {
      for (var i = 0; i < arguments.length; i++) delete listeners[arguments[i]];
    },
    EventsOffAll: function () {
      var names = Object.keys(listeners);
      for (var i = 0; i < names.length; i++) delete listeners[names[i]];
    },
    // Local only: there is no Go-side receiver in headless mode.
    EventsEmit: function (eventName) {
      dispatch(eventName, Array.prototype.slice.call(arguments, 1));
    },

    WindowReload: function () {
      w.location.reload();
    },
    WindowReloadApp: function () {
      w.location.reload();
    },
    WindowSetTitle: function (title) {
      w.document.title = title;
    },
    WindowSetAlwaysOnTop: noop,
    WindowSetSystemDefaultTheme: noop,
    WindowSetLightTheme: noop,
    WindowSetDarkTheme: noop,
    WindowCenter: noop,
    WindowFullscreen: noop,
    WindowUnfullscreen: noop,
    WindowSetSize: noop,
    WindowSetMaxSize: noop,
    WindowSetMinSize: noop,
    WindowSetPosition: noop,
    WindowHide: noop,
    WindowShow: noop,
    WindowMaximise: noop,
    WindowToggleMaximise: noop,
    WindowUnmaximise: noop,
    WindowMinimise: noop,
    WindowUnminimise: noop,
    WindowSetBackgroundColour: noop,
    WindowIsFullscreen: resolved(false),
    WindowIsMaximised: resolved(false),
    WindowIsMinimised: resolved(false),
    WindowIsNormal: resolved(true),
    WindowGetSize: function () {
      return Promise.resolve({ w: w.innerWidth, h: w.innerHeight });
    },
    WindowGetPosition: resolved({ x: 0, y: 0 }),
    ScreenGetAll: function () {
      var sc = w.screen || {};
      return Promise.resolve([{ isCurrent: true, isPrimary: true, width: sc.width, height: sc.height }]);
    },

    BrowserOpenURL: function (url) {
      var u;
      try {
        u = new w.URL(String(url), w.location.href);
      } catch (_) {
        return;
      }
      if (u.protocol !== 'http:' && u.protocol !== 'https:') return;
      w.open(u.href, '_blank', 'noopener,noreferrer');
    },
    Environment: Environment,
    Quit: function () {
      w.console.warn('[headless] Quit() is ignored: the browser cannot stop the server');
    },
    Hide: noop,
    Show: noop,

    ClipboardGetText: function () {
      return Promise.resolve()
        .then(function () {
          return w.navigator.clipboard.readText();
        })
        .then(
          function (t) {
            return typeof t === 'string' ? t : '';
          },
          function () {
            return '';
          }
        );
    },
    ClipboardSetText: function (text) {
      return Promise.resolve()
        .then(function () {
          return w.navigator.clipboard.writeText(text);
        })
        .then(
          function () {
            return true;
          },
          function () {
            return false;
          }
        );
    },

    OnFileDrop: noop,
    OnFileDropOff: noop,
    ResolveFilePaths: noop,
    CanResolveFilePaths: function () {
      return false;
    },

    InitializeNotifications: resolved(undefined),
    CleanupNotifications: resolved(undefined),
    RegisterNotificationCategory: resolved(undefined),
    RemoveNotificationCategory: resolved(undefined),
    RemoveAllPendingNotifications: resolved(undefined),
    RemovePendingNotification: resolved(undefined),
    RemoveAllDeliveredNotifications: resolved(undefined),
    RemoveDeliveredNotification: resolved(undefined),
    RemoveNotification: resolved(undefined),
    SendNotification: resolved(undefined),
    SendNotificationWithActions: resolved(undefined),
    IsNotificationAvailable: resolved(false),
    RequestNotificationAuthorization: resolved(false),
    CheckNotificationAuthorization: resolved(false),
  };

  w.runtime = runtime;

  // Read-only hooks for webadmin/headless-shim.test.mjs. The frontend only
  // checks that it exists (shortcuts.js shortcutPlatformFor: browser chords).
  Object.defineProperty(w, '__sshGeteHeadless', {
    value: Object.freeze({
      createSSEParser: createSSEParser,
      stop: stopEvents,
      badgeState: function () {
        return badgeState;
      },
      backoffMs: function () {
        return backoff;
      },
    }),
  });

  // ---------------------------------------------------------------- start

  connectEvents();
  if (w.document && w.document.readyState !== 'loading') {
    mountBadge();
  } else if (w.document) {
    w.document.addEventListener('DOMContentLoaded', mountBadge);
  }
})();
