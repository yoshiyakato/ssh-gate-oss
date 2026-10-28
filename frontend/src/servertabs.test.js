// Run with: node --test frontend/src/servertabs.test.js  (no jsdom needed)
import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  ALL_SERVERS,
  SERVERTABS_I18N,
  serverStateSymbol,
  normalizeServerFilter,
  serverTabBadges,
  buildServerTabs,
  serverTabText,
  serverTabAriaLabel,
  createServerFilterSetter,
  nextTabIndex,
  parseShellLabel,
  shellTabLabel,
  buildShellSubtabs,
  shellTabAriaLabel,
  renderServerTabs,
  renderShellSubtabs,
  SERVER_CONN_STATES,
  serverStateLabel,
  serialDetail,
} from './servertabs.js';

function makeT(lang) {
  return (key, vars = {}) => {
    const s = SERVERTABS_I18N[lang][key];
    if (s == null) throw new Error(`missing key ${key}`);
    return s.replace(/\{(\w+)\}/g, (m, k) => (k in vars ? String(vars[k]) : m));
  };
}
const tJa = makeT('ja');
const tEn = makeT('en');

const sessions = [
  { connId: 'c1', connName: 'web-01', kind: 'ssh', connState: 'connected', shells: 2, pending: 1 },
  { connId: 'c2', connName: 'db-stg', kind: 'ssh', connState: 'disconnected', shells: 0, pending: 0 },
  { connId: 'c3', connName: 'router-01', kind: 'ssh', connState: 'error', shells: 0, pending: 2 },
  { connId: 'c4', connName: 'usb-serial', kind: 'serial', connState: 'in_use', shells: 1, pending: 0 },
];

test('i18n: ja and en have identical keys, all prefixed srvtab.', () => {
  const ja = Object.keys(SERVERTABS_I18N.ja).sort();
  const en = Object.keys(SERVERTABS_I18N.en).sort();
  assert.deepEqual(ja, en);
  for (const k of ja) assert.ok(k.startsWith('srvtab.'), k);
});

test('serverStateSymbol matches §6.3 / model.js mapping', () => {
  assert.equal(serverStateSymbol('connected'), '●');
  assert.equal(serverStateSymbol('disconnected'), '○');
  assert.equal(serverStateSymbol('connecting'), '○');
  assert.equal(serverStateSymbol('error'), '▲');
  assert.equal(serverStateSymbol('in_use'), '◆');
  assert.equal(serverStateSymbol('weird'), '○');
});

test('normalizeServerFilter falls back to All for unknown / removed ids', () => {
  assert.equal(normalizeServerFilter('c1', sessions), 'c1');
  assert.equal(normalizeServerFilter('gone', sessions), ALL_SERVERS);
  assert.equal(normalizeServerFilter('', sessions), ALL_SERVERS);
  assert.equal(normalizeServerFilter(undefined, null), ALL_SERVERS);
});

test('serverTabBadges omits zero counts', () => {
  assert.deepEqual(serverTabBadges({ connId: 'x', connState: 'connected', shells: 2, pending: 1 }), {
    symbol: '●', shells: '⧉2', pending: '⏳1',
  });
  assert.deepEqual(serverTabBadges({ connId: 'x', connState: 'disconnected', shells: 0, pending: -3 }), {
    symbol: '○', shells: '', pending: '',
  });
});

test('buildServerTabs: All first, sessions order kept, selection normalized', () => {
  const v = buildServerTabs(sessions, { selected: 'c1' });
  assert.equal(v.selected, 'c1');
  assert.deepEqual(v.tabs.map((x) => x.connId), ['', 'c1', 'c2', 'c3', 'c4']);
  assert.deepEqual(v.tabs.map((x) => x.selected), [false, true, false, false, false]);
  assert.equal(v.tabs[0].pending, 3);
  assert.equal(v.tabs[0].shells, 3);

  const v2 = buildServerTabs(sessions, { selected: 'deleted', extraPending: 4 });
  assert.equal(v2.selected, ALL_SERVERS);
  assert.equal(v2.tabs[0].selected, true);
  assert.equal(v2.tabs[0].badges.pending, '⏳7');
  assert.equal(v2.tabs[4].kind, 'serial');
});

test('serverTabText renders the §6.4 wire text', () => {
  const v = buildServerTabs(sessions);
  assert.deepEqual(v.tabs.map((x) => serverTabText(x, tJa)), [
    'すべて ⏳3', '● web-01 ⧉2 ⏳1', '○ db-stg', '▲ router-01 ⏳2', '◆ usb-serial ⧉1',
  ]);
  assert.equal(serverTabText(buildServerTabs([]).tabs[0], tEn), 'All');
});

test('serverTabAriaLabel in JA and EN, zero items omitted', () => {
  const v = buildServerTabs(sessions);
  assert.equal(serverTabAriaLabel(v.tabs[1], tJa, 'ja'), 'web-01、接続中、シェル2、承認待ち1件');
  assert.equal(serverTabAriaLabel(v.tabs[1], tEn, 'en'), 'web-01, Connected, 2 shells, 1 awaiting approval');
  assert.equal(serverTabAriaLabel(v.tabs[2], tJa, 'ja'), 'db-stg、未接続');
  assert.equal(serverTabAriaLabel(v.tabs[0], tJa, 'ja'), 'すべての接続先、承認待ち3件');
  assert.equal(serverTabAriaLabel(buildServerTabs([]).tabs[0], tEn, 'en'), 'All connections');
});

test('createServerFilterSetter writes state.serverFilter and reports changes only', () => {
  const state = { serverFilter: '' };
  const calls = [];
  const set = createServerFilterSetter(state, { onChange: (id) => calls.push(id) });
  assert.equal(set('c1'), true);
  assert.equal(state.serverFilter, 'c1');
  assert.equal(set('c1'), false);
  assert.equal(set(''), true);
  assert.equal(state.serverFilter, '');
  assert.equal(set(null), false);
  assert.deepEqual(calls, ['c1', '']);
});

test('nextTabIndex: arrows wrap, Home/End, other keys ignored', () => {
  assert.equal(nextTabIndex('ArrowRight', 2, 3), 0);
  assert.equal(nextTabIndex('ArrowLeft', 0, 3), 2);
  assert.equal(nextTabIndex('Home', 2, 3), 0);
  assert.equal(nextTabIndex('End', 0, 3), 2);
  assert.equal(nextTabIndex('a', 0, 3), -1);
  assert.equal(nextTabIndex('ArrowRight', 0, 0), -1);
});

test('parseShellLabel / shellTabLabel', () => {
  const table = [
    ['web-01 #1', { base: 'web-01', n: 1 }],
    ['my host #12', { base: 'my host', n: 12 }],
    ['a#b #3', { base: 'a#b', n: 3 }],
    ['#4', { base: '', n: 4 }],
    ['plain', { base: 'plain', n: null }],
    [undefined, { base: '', n: null }],
  ];
  for (const [inp, want] of table) assert.deepEqual(parseShellLabel(inp), want, String(inp));
  assert.equal(shellTabLabel({ label: 'web-01 #2' }), 'web-01 #2');
  assert.equal(shellTabLabel({ label: 'web-01 #2' }, { compact: true }), '#2');
  assert.equal(shellTabLabel({ label: 'old-name #2' }, { connName: 'web-01' }), 'web-01 #2');
  assert.equal(shellTabLabel({ label: 'odd', connId: 'c9' }), 'odd');
});

function terminalState() {
  const tabs = new Map([
    ['T1', { connId: 'c1', label: 'web-01 #1', status: 'open' }],
    ['T2', { connId: 'c4', label: 'usb-serial #1', status: 'exited' }],
    ['T3', { connId: 'c1', label: 'web-01 #2', status: 'starting' }],
    ['T4', { connId: 'c1', label: 'old #3', status: 'open' }],
  ]);
  return { tabs, order: ['T1', 'T2', 'T3', 'T4', 'missing'], activeId: 'T3' };
}

test('buildShellSubtabs with a server filter: "web-01 #1 | #2 | ＋新しいシェル"', () => {
  const v = buildShellSubtabs(terminalState(), { serverFilter: 'c1', sessions });
  assert.deepEqual(v.items.map((i) => i.text), ['web-01 #1', '#2', '#3']);
  assert.deepEqual(v.items.map((i) => i.fullLabel), ['web-01 #1', 'web-01 #2', 'web-01 #3']);
  assert.deepEqual(v.items.map((i) => i.active), [false, true, false]);
  assert.deepEqual(v.newShell, { connId: 'c1', name: 'web-01' });
});

test('buildShellSubtabs with All: every tab labelled, no new-shell item', () => {
  const v = buildShellSubtabs(terminalState(), { serverFilter: '', sessions });
  assert.deepEqual(v.items.map((i) => i.text), ['web-01 #1', 'usb-serial #1', 'web-01 #2', 'web-01 #3']);
  assert.equal(v.newShell, null);
  const stale = buildShellSubtabs(terminalState(), { serverFilter: 'gone', sessions });
  assert.equal(stale.items.length, 4);
  assert.equal(buildShellSubtabs(null).items.length, 0);
});

test('shellTabAriaLabel adds status for exited / starting', () => {
  const v = buildShellSubtabs(terminalState(), { sessions });
  assert.equal(shellTabAriaLabel(v.items[0], tJa, 'ja'), 'web-01 #1');
  assert.equal(shellTabAriaLabel(v.items[1], tJa, 'ja'), 'usb-serial #1、終了');
  assert.equal(shellTabAriaLabel(v.items[2], tEn, 'en'), 'web-01 #2, Connecting');
});

// ---- minimal fake DOM (enough for the keyed diff; innerHTML is forbidden) ----

class FakeEl {
  constructor(doc, tag) {
    this.ownerDocument = doc;
    this.tagName = tag.toUpperCase();
    this.children = [];
    this.parent = null;
    this.attrs = new Map();
    this.listeners = {};
    this._text = '';
    this.hidden = false;
    const cls = new Set();
    this.classList = {
      contains: (c) => cls.has(c),
      toggle: (c, on) => (on ? cls.add(c) : cls.delete(c)),
      add: (c) => cls.add(c),
    };
    this._cls = cls;
  }
  set className(v) { this._cls.clear(); for (const c of v.split(/\s+/).filter(Boolean)) this._cls.add(c); }
  set innerHTML(_) { throw new Error('innerHTML must not be used'); }
  set type(v) { this.attrs.set('type', v); }
  get textContent() { return this._text + this.children.map((c) => c.textContent).join(''); }
  set textContent(v) { this.children = []; this._text = String(v); }
  get firstElementChild() { return this.children[0] || null; }
  get nextElementSibling() {
    if (!this.parent) return null;
    const s = this.parent.children;
    return s[s.indexOf(this) + 1] || null;
  }
  setAttribute(k, v) { this.attrs.set(k, String(v)); }
  getAttribute(k) { return this.attrs.has(k) ? this.attrs.get(k) : null; }
  hasAttribute(k) { return this.attrs.has(k); }
  removeAttribute(k) { this.attrs.delete(k); }
  appendChild(c) { return this.insertBefore(c, null); }
  insertBefore(c, ref) {
    if (c.parent) c.remove();
    c.parent = this;
    const i = ref ? this.children.indexOf(ref) : -1;
    if (i < 0) this.children.push(c); else this.children.splice(i, 0, c);
    return c;
  }
  remove() {
    if (!this.parent) return;
    const s = this.parent.children;
    s.splice(s.indexOf(this), 1);
    this.parent = null;
  }
  contains(n) { for (let x = n; x; x = x.parent) if (x === this) return true; return false; }
  closest(sel) {
    const attr = /^\[([\w-]+)\]$/.exec(sel)[1];
    for (let x = this; x; x = x.parent) if (x.hasAttribute(attr)) return x;
    return null;
  }
  querySelectorAll(sel) {
    const [, k, v] = /^\[([\w-]+)="([^"]+)"\]$/.exec(sel);
    const out = [];
    const walk = (n) => { for (const c of n.children) { if (c.getAttribute(k) === v) out.push(c); walk(c); } };
    walk(this);
    return out;
  }
  addEventListener(type, fn) { (this.listeners[type] ||= []).push(fn); }
  dispatch(type, event) { for (const fn of this.listeners[type] || []) fn(event); }
  focus() { this.ownerDocument.focused = this; }
}
const fakeDoc = () => { const d = { createElement: (t) => new FakeEl(d, t) }; return d; };

test('renderServerTabs: keyed diff reuses nodes, textContent only, aria, click callback', () => {
  const doc = fakeDoc();
  const root = doc.createElement('div');
  const state = { serverFilter: '' };
  const picks = [];
  const onSelect = createServerFilterSetter(state, { onChange: (id) => picks.push(id) });

  const evil = [{ connId: 'x', connName: '<img src=x onerror=alert(1)>', connState: 'connected', shells: 1 }];
  renderServerTabs(root, buildServerTabs([...sessions, ...evil], { selected: state.serverFilter }), { t: tJa, onSelect });
  assert.equal(root.getAttribute('role'), 'tablist');
  assert.equal(root.children.length, 6);
  assert.equal(root.children[5].children[1].textContent, '<img src=x onerror=alert(1)>');
  const web = root.children[1];
  assert.equal(web.getAttribute('aria-selected'), 'false');
  assert.equal(web.getAttribute('aria-label'), 'web-01、接続中、シェル2、承認待ち1件');
  assert.equal(root.children[0].getAttribute('tabindex'), '0');

  root.dispatch('click', { target: web.children[1] });
  assert.equal(state.serverFilter, 'c1');
  assert.deepEqual(picks, ['c1']);

  // re-render: c2 removed, order changed; existing nodes are reused
  const next = [sessions[3], { ...sessions[0], shells: 3 }];
  renderServerTabs(root, buildServerTabs(next, { selected: state.serverFilter }), { t: tEn, lang: 'en', onSelect });
  assert.deepEqual(root.children.map((c) => c.getAttribute('data-key')), ['*all', 'c4', 'c1']);
  assert.equal(root.children[2], web);
  assert.equal(web.getAttribute('aria-selected'), 'true');
  assert.equal(web.children[2].textContent, '⧉3');
  assert.equal(root.children[0].children[1].textContent, 'All');

  // keyboard: ArrowRight from c1 wraps to All and selects it
  root.dispatch('keydown', { key: 'ArrowRight', target: web, preventDefault() {} });
  assert.equal(state.serverFilter, '');
  assert.equal(doc.focused, root.children[0]);
});

test('renderShellSubtabs: tabs plus new-shell button, callbacks', () => {
  const doc = fakeDoc();
  const root = doc.createElement('div');
  const activated = [];
  const opened = [];
  const opts = { t: tJa, onActivate: (id) => activated.push(id), onNewShell: (c) => opened.push(c) };
  renderShellSubtabs(root, buildShellSubtabs(terminalState(), { serverFilter: 'c1', sessions }), opts);
  assert.deepEqual(root.children.map((c) => c.textContent), ['web-01 #1', '#2', '#3', '＋新しいシェル']);
  assert.equal(root.getAttribute('aria-label'), 'web-01 のシェル');
  assert.ok(root.children[1].classList.contains('is-starting'));
  assert.equal(root.children[1].getAttribute('aria-selected'), 'true');
  root.dispatch('click', { target: root.children[0] });
  root.dispatch('click', { target: root.children[3] });
  assert.deepEqual(activated, ['T1']);
  assert.deepEqual(opened, ['c1']);

  const first = root.children[0];
  renderShellSubtabs(root, buildShellSubtabs(terminalState(), { serverFilter: '', sessions }), opts);
  assert.deepEqual(root.children.map((c) => c.textContent), ['web-01 #1', 'usb-serial #1', 'web-01 #2', 'web-01 #3']);
  assert.equal(root.children[0], first);
  assert.equal(root.getAttribute('aria-label'), 'すべてのシェル');
});

test('serial available / missing: label, symbol and is-<state> class (no raw keys)', () => {
  for (const st of SERVER_CONN_STATES) {
    assert.ok(SERVERTABS_I18N.ja[`srvtab.conn.${st}`], st);
    assert.ok(SERVERTABS_I18N.en[`srvtab.conn.${st}`], st);
  }
  assert.equal(serverStateSymbol('available'), '○');
  assert.equal(serverStateSymbol('missing'), '○');
  const ser = [
    { connId: 's1', connName: 'COM3', kind: 'serial', connState: 'missing' },
    { connId: 's2', connName: 'usb', kind: 'serial', connState: 'available' },
  ];
  const v = buildServerTabs(ser);
  assert.equal(serverTabAriaLabel(v.tabs[1], tJa), 'COM3、見つかりません');
  assert.equal(serverTabAriaLabel(v.tabs[2], tEn, 'en'), 'usb, Available');
  const doc = fakeDoc();
  const root = doc.createElement('div');
  renderServerTabs(root, v, { t: tJa, onSelect() {} });
  assert.ok(root.children[1].classList.contains('is-missing'));
  assert.ok(root.children[2].classList.contains('is-available'));
  // model.js functions win when the Integrator passes them.
  const tModel = (k) => (k === 'state.conn.missing' ? 'MODEL' : tJa(k));
  assert.equal(serverStateLabel('missing', tModel, (c) => `state.conn.${c}`), 'MODEL');
  const v2 = buildServerTabs(ser, { symbolOf: () => '?' });
  assert.equal(v2.tabs[1].badges.symbol, '?');
  // unknown state never yields a raw key
  assert.equal(serverStateLabel('weird', tJa), '未接続');
});

test('tablist ignores modified arrows (left to shortcuts.js, no double move)', () => {
  const doc = fakeDoc();
  const root = doc.createElement('div');
  const state = { serverFilter: 'c1' };
  const onSelect = createServerFilterSetter(state);
  renderServerTabs(root, buildServerTabs(sessions, { selected: 'c1' }), { t: tJa, onSelect });
  const web = root.children[1];
  let prevented = false;
  root.dispatch('keydown', { key: 'ArrowRight', metaKey: true, altKey: true, target: web, preventDefault() { prevented = true; } });
  root.dispatch('keydown', { key: 'ArrowRight', ctrlKey: true, shiftKey: true, altKey: true, target: web, preventDefault() { prevented = true; } });
  assert.equal(state.serverFilter, 'c1');
  assert.equal(prevented, false);
  root.dispatch('keydown', { key: 'ArrowRight', target: web, preventDefault() {} });
  assert.equal(state.serverFilter, 'c2');
});

test('shell sub-tabs stay keyboard-reachable when the active terminal is filtered out', () => {
  const ts = terminalState();
  ts.activeId = 'T2'; // usb-serial, outside the c1 filter
  const v = buildShellSubtabs(ts, { serverFilter: 'c1', sessions });
  assert.deepEqual(v.items.map((i) => i.focusable), [true, false, false]);
  const doc = fakeDoc();
  const root = doc.createElement('div');
  renderShellSubtabs(root, v, { t: tJa });
  assert.deepEqual(root.children.slice(0, 3).map((c) => c.getAttribute('tabindex')), ['0', '-1', '-1']);
  assert.ok(root.children.every((c) => c.getAttribute('aria-selected') !== 'true'));
  // with an active item, only it is focusable
  const v2 = buildShellSubtabs(terminalState(), { serverFilter: 'c1', sessions });
  assert.deepEqual(v2.items.map((i) => i.focusable), [false, true, false]);
});

test('serial sub-tab shows port @baud mode from TerminalInfo (§10.6) and follows updates', () => {
  assert.equal(serialDetail({ kind: 'serial', port: '/dev/cu.usbserial-10', baud: 9600, serialMode: '7E1' }), '/dev/cu.usbserial-10 @9600 7E1');
  assert.equal(serialDetail({ kind: 'ssh', port: 'x' }), '');
  assert.equal(serialDetail(null), '');
  const info = (mode) => [{ id: 'T2', connId: 'c4', kind: 'serial', port: '/dev/cu.usbserial-10', baud: 9600, serialMode: mode }];
  const doc = fakeDoc();
  const root = doc.createElement('div');
  let v = buildShellSubtabs(terminalState(), { serverFilter: 'c4', sessions, terminals: info('8N1') });
  assert.equal(v.items[0].text, 'usb-serial #1 /dev/cu.usbserial-10 @9600 8N1');
  renderShellSubtabs(root, v, { t: tJa });
  const node = root.children[0];
  assert.equal(node.getAttribute('title'), 'usb-serial #1 /dev/cu.usbserial-10 @9600 8N1');
  // sessions:changed after an in-place setMode
  v = buildShellSubtabs(terminalState(), { serverFilter: 'c4', sessions, terminals: info('7E1') });
  renderShellSubtabs(root, v, { t: tJa });
  assert.equal(root.children[0], node);
  assert.equal(node.textContent, 'usb-serial #1 /dev/cu.usbserial-10 @9600 7E1');
  assert.equal(node.getAttribute('aria-label'), 'usb-serial #1 /dev/cu.usbserial-10 @9600 7E1、終了');
  // SSH tabs are unchanged and have no title
  v = buildShellSubtabs(terminalState(), { serverFilter: 'c1', sessions, terminals: info('7E1') });
  assert.deepEqual(v.items.map((i) => i.text), ['web-01 #1', '#2', '#3']);
});
