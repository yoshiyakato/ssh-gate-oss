// Tests for webadmin/headless-shim.js (docs/spec/phase6-headless.md §5, §4.3, §10).
//
// The shim is a classic browser script. Each test loads it into a fresh
// node:vm context whose global object is a small fake `window` (fetch,
// location, timers, document), so no browser or jsdom is needed.
//
// Run: node --test webadmin/headless-shim.test.mjs

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, existsSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import vm from 'node:vm';

const here = dirname(fileURLToPath(import.meta.url));
const repoRoot = join(here, '..');
const shimSource = readFileSync(join(here, 'headless-shim.js'), 'utf8');

// ---------------------------------------------------------------- harness

// Values built inside the vm context have that realm's prototypes, so compare
// their JSON form (every payload the shim handles is JSON anyway).
const same = (actual, expected, msg) => assert.deepEqual(JSON.parse(JSON.stringify(actual)), expected, msg);

const settle = async (n = 8) => {
  for (let i = 0; i < n; i++) await new Promise((r) => setImmediate(r));
};

function jsonResponse(status, body) {
  return new Response(body === undefined ? '' : JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

// A controllable SSE stream: push(text) enqueues, end() closes.
function sseStream() {
  const enc = new TextEncoder();
  let ctrl;
  const stream = new ReadableStream({
    start(c) {
      ctrl = c;
    },
  });
  return {
    response: new Response(stream, { status: 200, headers: { 'Content-Type': 'text/event-stream' } }),
    push: (text) => ctrl.enqueue(enc.encode(text)),
    end: () => ctrl.close(),
  };
}

function event(id, n, d, o) {
  const ev = { n, d };
  if (o !== undefined) ev.o = o;
  return `id: ${id}\ndata: ${JSON.stringify(ev)}\n\n`;
}

// load() evaluates the shim. `events` is a queue of responses (or functions
// returning one) for successive GET /api/events; when empty the request stays
// pending. `api(url, init)` answers every other fetch.
function load({ events = [], api = () => jsonResponse(404, { error: 'unknown method' }), path = '/', search = '', language = 'en-US', readyState = 'complete', storage = {} } = {}) {
  const fetches = [];
  const timers = [];
  const assigned = [];
  const opened = [];
  const logs = [];
  const bodyChildren = [];
  const listeners = {};
  const clipboard = { text: 'clip', fail: false };

  function makeEl(tag) {
    const el = {
      tagName: tag.toUpperCase(),
      style: {},
      attrs: {},
      children: [],
      textContent: '',
      handlers: {},
      setAttribute(k, v) {
        this.attrs[k] = String(v);
      },
      getAttribute(k) {
        return this.attrs[k];
      },
      appendChild(c) {
        this.children.push(c);
        return c;
      },
      addEventListener(type, fn) {
        this.handlers[type] = fn;
      },
    };
    return el;
  }

  const document = {
    title: '',
    readyState,
    body: { appendChild: (c) => (bodyChildren.push(c), c) },
    createElement: makeEl,
    addEventListener: (type, fn) => {
      listeners[type] = fn;
    },
  };

  const console_ = {};
  for (const m of ['log', 'debug', 'info', 'warn', 'error']) console_[m] = (...a) => logs.push([m, ...a]);

  const win = {
    fetch: (url, init = {}) => {
      fetches.push({ url, init });
      if (url === '/api/events') {
        const next = events.shift();
        if (next === undefined) return new Promise(() => {});
        const r = typeof next === 'function' ? next() : next;
        return r instanceof Error ? Promise.reject(r) : Promise.resolve(r);
      }
      return Promise.resolve(api(url, init));
    },
    location: {
      pathname: path,
      search,
      hash: '',
      href: 'http://127.0.0.1:8788' + path + search,
      assign: (u) => assigned.push(u),
      reload: () => assigned.push('reload'),
    },
    navigator: {
      language,
      clipboard: {
        readText: async () => {
          if (clipboard.fail) throw new Error('denied');
          return clipboard.text;
        },
        writeText: async (t) => {
          if (clipboard.fail) throw new Error('denied');
          clipboard.text = t;
        },
      },
    },
    screen: { width: 1920, height: 1080 },
    innerWidth: 1280,
    innerHeight: 800,
    open: (...a) => opened.push(a),
    document,
    console: console_,
    setTimeout: (fn, delay) => {
      timers.push({ fn, delay });
      return timers.length;
    },
    clearTimeout: (h) => {
      if (timers[h - 1]) timers[h - 1].cleared = true;
    },
    TextDecoder,
    URL,
    Promise,
    localStorage: {
      getItem: (k) => (k in storage ? storage[k] : null),
      setItem: (k, v) => {
        storage[k] = String(v);
      },
      removeItem: (k) => {
        delete storage[k];
      },
    },
  };
  win.window = win;
  vm.createContext(win);
  vm.runInContext(shimSource, win, { filename: 'headless-shim.js' });

  return {
    win,
    storage,
    fetches,
    timers,
    assigned,
    opened,
    logs,
    bodyChildren,
    docListeners: listeners,
    clipboard,
    eventFetches: () => fetches.filter((f) => f.url === '/api/events'),
    // Fires the most recent pending timer.
    fireTimer() {
      const t = [...timers].reverse().find((x) => !x.fired && !x.cleared);
      assert.ok(t, 'no pending timer');
      t.fired = true;
      t.fn();
      return t.delay;
    },
    record(name) {
      const got = [];
      win.runtime.EventsOn(name, (...a) => got.push(a.length === 1 ? a[0] : a));
      return got;
    },
  };
}

// ---------------------------------------------------------------- runtime coverage

test('window.runtime has every export of wailsjs/runtime/runtime.js', (t) => {
  const p = join(repoRoot, 'frontend', 'wailsjs', 'runtime', 'runtime.js');
  if (!existsSync(p)) {
    t.skip('frontend/wailsjs/runtime/runtime.js is not generated');
    return;
  }
  const src = readFileSync(p, 'utf8');
  const names = [...src.matchAll(/export function (\w+)\s*\(/g)].map((m) => m[1]);
  assert.ok(names.length > 50, `too few exports parsed: ${names.length}`);
  const h = load();
  const missing = names.filter((n) => typeof h.win.runtime[n] !== 'function');
  same(missing, []);
});

test('window.go.main.App covers every method in wailsjs/go/main/App.js', (t) => {
  const p = join(repoRoot, 'frontend', 'wailsjs', 'go', 'main', 'App.js');
  if (!existsSync(p)) {
    t.skip('frontend/wailsjs/go/main/App.js is not generated');
    return;
  }
  const names = [...readFileSync(p, 'utf8').matchAll(/export function (\w+)\s*\(/g)].map((m) => m[1]);
  const h = load();
  for (const n of names) assert.equal(typeof h.win.go.main.App[n], 'function', n);
});

// ---------------------------------------------------------------- calls (§3.1, §5.2)

test('a call posts the arguments as a JSON array with the CSRF header', async () => {
  const h = load({ api: () => jsonResponse(200, { result: { ok: 1 } }) });
  const res = await h.win.go.main.App.SaveConnection({ id: '', name: 'prod web' }, undefined);
  same(res, { ok: 1 });
  const f = h.fetches.find((x) => x.url.startsWith('/api/call/'));
  assert.equal(f.url, '/api/call/SaveConnection');
  assert.equal(f.init.method, 'POST');
  assert.equal(f.init.credentials, 'same-origin');
  assert.equal(f.init.headers['Content-Type'], 'application/json');
  assert.equal(f.init.headers['X-SSH-Gete-CSRF'], '1');
  assert.equal(f.init.body, '[{"id":"","name":"prod web"},null]');
});

test('a call with no arguments sends [] and a null result resolves null', async () => {
  const h = load({ api: () => jsonResponse(200, { result: null }) });
  assert.equal(await h.win.go.main.App.CloseAllTerminals(), null);
  assert.equal(h.fetches.at(-1).init.body, '[]');
});

test('the method name is URL-encoded', async () => {
  const h = load({ api: () => jsonResponse(404, { error: 'unknown method' }) });
  await assert.rejects(h.win.go.main.App['a/b?c'](), (e) => e === 'HTTP 404: unknown method');
  assert.equal(h.fetches.at(-1).url, '/api/call/a%2Fb%3Fc');
});

test('the proxy is not a thenable and ignores symbols', async () => {
  const h = load();
  assert.equal(h.win.go.main.App.then, undefined);
  assert.equal(h.win.go.main.App[Symbol.toPrimitive], undefined);
  // Resolving a promise with the proxy must not call /api/call/then.
  const v = await Promise.resolve(h.win.go.main.App);
  assert.equal(v, h.win.go.main.App);
  assert.equal(h.fetches.filter((f) => f.url.startsWith('/api/call/')).length, 0);
});

test('a Go error rejects with the plain string, like Wails', async () => {
  const h = load({ api: () => jsonResponse(200, { error: 'host key mismatch' }) });
  await assert.rejects(h.win.go.main.App.GetHostKeyInfo('c1'), (e) => e === 'host key mismatch');
});

test('non-200 statuses reject with "HTTP <status>: <error>"', async () => {
  for (const [status, body, want] of [
    [400, { error: "received 1 arguments to method 'main.App.X', expected 2" }, "HTTP 400: received 1 arguments to method 'main.App.X', expected 2"],
    [500, { error: 'internal error' }, 'HTTP 500: internal error'],
    [503, { error: 'shutting down' }, 'HTTP 503: shutting down'],
    [429, undefined, 'HTTP 429: error'],
  ]) {
    const h = load({ api: () => jsonResponse(status, body) });
    await assert.rejects(h.win.go.main.App.GetInitialData(), (e) => e === want);
    same(h.assigned, []);
  }
});

test('a network failure rejects with a string', async () => {
  const h = load({ api: () => Promise.reject(new TypeError('Failed to fetch')) });
  await assert.rejects(h.win.go.main.App.GetInitialData(), (e) => e === 'Failed to fetch');
});

test('401 sends the page to /login?next=<current path> once', async () => {
  const h = load({ path: '/', search: '?tab=1', api: () => jsonResponse(401, { error: 'login required' }) });
  await assert.rejects(h.win.go.main.App.GetInitialData(), (e) => e === 'login required');
  await assert.rejects(h.win.go.main.App.GetMCPSettings());
  same(h.assigned, ['/login?next=%2F%3Ftab%3D1']);
});

// ---------------------------------------------------------------- local events (Wails events.js semantics)

test('EventsOnMultiple runs max times; -1 is unlimited; the returned function unsubscribes', () => {
  const { win } = load();
  const rt = win.runtime;
  const a = [];
  const b = [];
  const c = [];
  rt.EventsOnMultiple('x', (v) => a.push(v), 2);
  rt.EventsOnMultiple('x', (v) => b.push(v), -1);
  const off = rt.EventsOn('x', (v) => c.push(v));
  rt.EventsEmit('x', 1);
  off();
  rt.EventsEmit('x', 2);
  rt.EventsEmit('x', 3);
  same(a, [1, 2]);
  same(b, [1, 2, 3]);
  same(c, [1]);
});

test('EventsOnce fires once; EventsEmit passes every argument', () => {
  const { win } = load();
  const got = [];
  win.runtime.EventsOnce('y', (...args) => got.push(args));
  win.runtime.EventsEmit('y', 'a', 2);
  win.runtime.EventsEmit('y', 'b');
  same(got, [['a', 2]]);
});

test('EventsOff removes the named events; EventsOffAll removes everything', () => {
  const { win } = load();
  const rt = win.runtime;
  const got = [];
  for (const n of ['p', 'q', 'r']) rt.EventsOn(n, () => got.push(n));
  rt.EventsOff('p', 'q');
  for (const n of ['p', 'q', 'r']) rt.EventsEmit(n);
  same(got, ['r']);
  rt.EventsOffAll();
  rt.EventsEmit('r');
  same(got, ['r']);
});

test('a throwing listener does not stop the others', () => {
  const h = load();
  const got = [];
  h.win.runtime.EventsOn('z', () => {
    throw new Error('boom');
  });
  h.win.runtime.EventsOn('z', () => got.push(1));
  h.win.runtime.EventsEmit('z');
  same(got, [1]);
  assert.equal(h.logs.at(-1)[0], 'error');
});

// ---------------------------------------------------------------- SSE parsing

test('the SSE parser handles split chunks, CRLF, comments, id and multi-line data', () => {
  const { win } = load();
  const out = [];
  const feed = win.__sshGeteHeadless.createSSEParser((m) => out.push(m));
  feed(': ping\n\n');
  feed('id: e-1\r\nda');
  feed('ta: {"a":\r');
  feed('\ndata: 1}\r\n\r\n');
  feed('data:x\n\nid: e-3\ndata: y\nevent: ignored\nretry: 5\n');
  assert.equal(out.length, 2);
  feed('\n');
  same(
    out.map((m) => ({ ...m })),
    [
      { id: 'e-1', data: '{"a":\n1}' },
      { id: null, data: 'x' },
      { id: 'e-3', data: 'y' },
    ]
  );
});

test('an id-only block with no data is not dispatched', () => {
  const { win } = load();
  const out = [];
  const feed = win.__sshGeteHeadless.createSSEParser((m) => out.push(m));
  feed('id: 1\n\n');
  assert.equal(out.length, 0);
});

// ---------------------------------------------------------------- SSE transport (§4.1, §4.3)

test('the event stream is fetched with the CSRF header and events reach subscribers as Wails payloads', async () => {
  const s = sseStream();
  const h = load({ events: [s.response] });
  const data = h.record('terminal:data');
  const sessions = h.record('sessions:changed');
  await settle();
  const f = h.eventFetches()[0];
  assert.equal(f.init.method, 'GET');
  assert.equal(f.init.headers['X-SSH-Gete-CSRF'], '1');
  assert.equal(f.init.headers['Last-Event-ID'], undefined);
  s.push(': ping\n\n');
  s.push(event('ep-1', 'terminal:data', { id: 'TERM-1', data: 'aGk=' }, 2));
  s.push(event('ep-2', 'sessions:changed', { sessionsSeq: 7 }));
  await settle();
  same(data, [{ id: 'TERM-1', data: 'aGk=' }]);
  same(sessions, [{ sessionsSeq: 7 }]);
  assert.equal(h.win.__sshGeteHeadless.badgeState(), 'online');
});

test('reconnects with Last-Event-ID and backs off 0.5s doubling to 10s, reset after a success', async () => {
  const s1 = sseStream();
  const fail = () => jsonResponse(502, {});
  const s2 = sseStream();
  const h = load({ events: [s1.response, new Error('net'), fail, fail, fail, fail, fail, fail, s2.response, new Error('net')] });
  await settle();
  s1.push(event('ep-41', 'x', {}));
  s1.end();
  await settle();
  assert.equal(h.win.__sshGeteHeadless.badgeState(), 'offline');
  const delays = [];
  for (let i = 0; i < 8; i++) {
    delays.push(h.fireTimer());
    await settle();
  }
  same(delays, [500, 1000, 2000, 4000, 8000, 10000, 10000, 10000]);
  const fs = h.eventFetches();
  assert.equal(fs.length, 9);
  for (const f of fs.slice(1)) assert.equal(f.init.headers['Last-Event-ID'], 'ep-41');
  // s2 connected (200) → backoff resets.
  assert.equal(h.win.__sshGeteHeadless.backoffMs(), 500);
  s2.end();
  await settle();
  assert.equal(h.fireTimer(), 500);
});

test('401 on the event stream goes to the login page and stops reconnecting', async () => {
  const h = load({ path: '/', events: [jsonResponse(401, { error: 'login required' })] });
  await settle();
  same(h.assigned, ['/login?next=%2F']);
  assert.equal(h.timers.length, 0);
});

test('headless:resync synthesizes dropped data, exits, then sessions — in that order', async () => {
  const s1 = sseStream();
  const s2 = sseStream();
  const h = load({
    events: [s1.response, s2.response],
    api: () => jsonResponse(200, { result: null }),
  });
  const order = [];
  for (const n of ['terminal:data', 'terminal:exit', 'sessions:changed']) {
    h.win.runtime.EventsOn(n, (p) => order.push([n, p]));
  }
  // Gaps are reported in the order the page learned the terminals (TERM-B first).
  // TERM-A has streamed 10 bytes; TERM-B was started from this page but has no data yet;
  // TERM-C was never seen here.
  await h.win.go.main.App.StartTerminal('TERM-B', 'c1', 80, 24);
  await settle();
  s1.push(event('e1-5', 'terminal:data', { id: 'TERM-A', data: 'MDEyMzQ1Njc4OQ==' }, 10));
  await settle();
  order.length = 0;
  s1.end();
  await settle();
  h.fireTimer();
  await settle();
  assert.equal(h.eventFetches()[1].init.headers['Last-Event-ID'], 'e1-5');
  const sessions = { sessionsSeq: 99, sessions: [] };
  s2.push(
    event('e2-1', 'headless:resync', {
      sessions,
      offsets: { 'TERM-A': 25, 'TERM-B': 4, 'TERM-C': 1000 },
      exits: [
        { id: 'TERM-B', reason: 'remote_exit', exitCode: 1 },
        { id: 'TERM-C', reason: 'conn_lost' },
      ],
    })
  );
  await settle();
  same(
    order.map(([n, p]) => [n, { ...p }]),
    [
      ['terminal:data', { id: 'TERM-B', data: '', dropped: 4 }],
      ['terminal:data', { id: 'TERM-A', data: '', dropped: 15 }],
      ['terminal:exit', { id: 'TERM-B', reason: 'remote_exit', exitCode: 1 }],
      ['sessions:changed', { sessionsSeq: 99, sessions: [] }],
    ]
  );
  // The real exit for TERM-B arriving later is not delivered twice.
  order.length = 0;
  s2.push(event('e2-2', 'terminal:exit', { id: 'TERM-B', reason: 'remote_exit', exitCode: 1 }));
  // TERM-A continues from offset 25: a second resync with no gap synthesizes nothing for it.
  s2.push(event('e2-3', 'terminal:data', { id: 'TERM-A', data: 'eA==' }, 26));
  s2.push(event('e2-4', 'headless:resync', { sessions, offsets: { 'TERM-A': 26 }, exits: [] }));
  await settle();
  same(
    order.map(([n]) => n),
    ['terminal:data', 'sessions:changed']
  );
});

test('headless:shutdown marks the badge', async () => {
  const s = sseStream();
  const h = load({ events: [s.response] });
  await settle();
  s.push(event('e-1', 'headless:shutdown', {}));
  s.end();
  await settle();
  assert.equal(h.win.__sshGeteHeadless.badgeState(), 'shutdown');
  const badge = h.bodyChildren[0];
  assert.equal(badge.id, 'ssh-gete-headless-badge');
  assert.match(badge.children[1].textContent, /server has stopped/);
});

// ---------------------------------------------------------------- badge and logout

test('the badge mounts on DOMContentLoaded while the document is loading', () => {
  const h = load({ readyState: 'loading', language: 'ja-JP' });
  assert.equal(h.bodyChildren.length, 0);
  h.docListeners.DOMContentLoaded();
  assert.equal(h.bodyChildren.length, 1);
  assert.equal(h.bodyChildren[0].children[2].textContent, 'ログアウト');
});

test('logout posts /api/logout with the CSRF header and goes to /login', async () => {
  const h = load({ api: () => new Response(null, { status: 204 }) });
  const btn = h.bodyChildren[0].children[2];
  btn.handlers.click();
  await settle();
  const f = h.fetches.find((x) => x.url === '/api/logout');
  assert.equal(f.init.method, 'POST');
  assert.equal(f.init.headers['X-SSH-Gete-CSRF'], '1');
  assert.equal(f.init.headers['Content-Type'], 'application/json');
  same(h.assigned, ['/login']);
});

// ---------------------------------------------------------------- other runtime functions

test('Environment() fetches /api/env once and returns the server platform', async () => {
  const h = load({ api: (url) => (url === '/api/env' ? jsonResponse(200, { buildType: 'production', platform: 'linux', arch: 'arm64' }) : jsonResponse(404, {})) });
  const a = await h.win.runtime.Environment();
  const b = await h.win.runtime.Environment();
  same({ ...a }, { buildType: 'production', platform: 'linux', arch: 'arm64' });
  assert.equal(a, b);
  const f = h.fetches.filter((x) => x.url === '/api/env');
  assert.equal(f.length, 1);
  assert.equal(f[0].init.headers['X-SSH-Gete-CSRF'], '1');
});

test('Environment() retries after a failure', async () => {
  let n = 0;
  const h = load({ api: () => (++n === 1 ? jsonResponse(503, { error: 'shutting down' }) : jsonResponse(200, { platform: 'darwin', arch: 'amd64' })) });
  await assert.rejects(h.win.runtime.Environment(), (e) => e === 'HTTP 503: shutting down');
  assert.equal((await h.win.runtime.Environment()).platform, 'darwin');
});

test('BrowserOpenURL opens only http and https', () => {
  const h = load();
  h.win.runtime.BrowserOpenURL('https://example.com/a');
  h.win.runtime.BrowserOpenURL('javascript:alert(1)');
  h.win.runtime.BrowserOpenURL('file:///etc/passwd');
  h.win.runtime.BrowserOpenURL('/relative');
  same(h.opened, [
    ['https://example.com/a', '_blank', 'noopener,noreferrer'],
    ['http://127.0.0.1:8788/relative', '_blank', 'noopener,noreferrer'],
  ]);
});

test('clipboard functions fall back to "" / false', async () => {
  const h = load();
  assert.equal(await h.win.runtime.ClipboardGetText(), 'clip');
  assert.equal(await h.win.runtime.ClipboardSetText('new'), true);
  assert.equal(h.clipboard.text, 'new');
  h.clipboard.fail = true;
  assert.equal(await h.win.runtime.ClipboardGetText(), '');
  assert.equal(await h.win.runtime.ClipboardSetText('x'), false);
});

test('window queries, notifications, Quit and title', async () => {
  const h = load();
  const rt = h.win.runtime;
  assert.equal(await rt.WindowIsFullscreen(), false);
  assert.equal(await rt.WindowIsMaximised(), false);
  assert.equal(await rt.WindowIsMinimised(), false);
  assert.equal(await rt.WindowIsNormal(), true);
  same({ ...(await rt.WindowGetSize()) }, { w: 1280, h: 800 });
  same({ ...(await rt.WindowGetPosition()) }, { x: 0, y: 0 });
  const screens = await rt.ScreenGetAll();
  same({ ...screens[0] }, { isCurrent: true, isPrimary: true, width: 1920, height: 1080 });
  assert.equal(rt.CanResolveFilePaths(), false);
  assert.equal(await rt.IsNotificationAvailable(), false);
  assert.equal(await rt.SendNotification({}), undefined);
  rt.WindowSetTitle('ssh-gete');
  assert.equal(h.win.document.title, 'ssh-gete');
  rt.Quit();
  assert.equal(h.logs.at(-1)[0], 'warn');
  rt.WindowReload();
  same(h.assigned, ['reload']);
  // No-op window functions do not call the server.
  rt.WindowMaximise();
  rt.WindowSetSize(1, 2);
  assert.equal(h.fetches.filter((f) => f.url !== '/api/events').length, 0);
});

test('the shim contains no eval or inline-script constructs (CSP script-src self)', () => {
  assert.doesNotMatch(shimSource, /\beval\s*\(|new Function\s*\(|innerHTML|document\.write/);
});

// Review finding 5: every /api/* request carries the session key login.js
// stored; logout forgets it; without a stored key the header is left out.
test('the session key from localStorage is sent on calls, events, env and logout', async () => {
  const storage = { 'ssh-gete-session-key': 'key-123' };
  const h = load({ storage, api: () => jsonResponse(200, { result: 'x' }) });
  await h.win.go.main.App.GetInitialData();
  await h.win.runtime.Environment();
  await settle();
  for (const url of ['/api/call/GetInitialData', '/api/env', '/api/events']) {
    const f = h.fetches.find((x) => x.url === url);
    assert.ok(f, 'no fetch of ' + url);
    assert.equal(f.init.headers['X-SSH-Gete-Session'], 'key-123', url);
  }
  // A re-login in another tab replaces the key; the next call uses it.
  storage['ssh-gete-session-key'] = 'key-456';
  await h.win.go.main.App.GetInitialData();
  assert.equal(h.fetches.at(-1).init.headers['X-SSH-Gete-Session'], 'key-456');
  const badgeButton = h.bodyChildren[0] && h.bodyChildren[0].children[2];
  assert.ok(badgeButton, 'no logout button');
  badgeButton.handlers.click();
  await settle();
  const lo = h.fetches.find((x) => x.url === '/api/logout');
  assert.equal(lo.init.headers['X-SSH-Gete-Session'], 'key-456');
  assert.equal('ssh-gete-session-key' in storage, false, 'logout kept the key');
});

test('without a stored session key no session header is sent', async () => {
  const h = load({ api: () => jsonResponse(200, { result: 'x' }) });
  await h.win.go.main.App.GetInitialData();
  assert.equal('X-SSH-Gete-Session' in h.fetches.at(-1).init.headers, false);
});
