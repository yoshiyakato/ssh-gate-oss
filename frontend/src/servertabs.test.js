// Pure view-model tests plus a minimal fake DOM (no jsdom APIs are used, so a
// stray innerHTML write throws instead of silently passing).
import { test, expect } from 'vitest';
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
  expect(ja).toStrictEqual(en);
  for (const k of ja) expect(k.startsWith('srvtab.'), k).toBeTruthy();
});

test('serverStateSymbol matches §6.3 / model.js mapping', () => {
  expect(serverStateSymbol('connected')).toBe('●');
  expect(serverStateSymbol('disconnected')).toBe('○');
  expect(serverStateSymbol('connecting')).toBe('○');
  expect(serverStateSymbol('error')).toBe('▲');
  expect(serverStateSymbol('in_use')).toBe('◆');
  expect(serverStateSymbol('weird')).toBe('○');
});

test('normalizeServerFilter falls back to All for unknown / removed ids', () => {
  expect(normalizeServerFilter('c1', sessions)).toBe('c1');
  expect(normalizeServerFilter('gone', sessions)).toBe(ALL_SERVERS);
  expect(normalizeServerFilter('', sessions)).toBe(ALL_SERVERS);
  expect(normalizeServerFilter(undefined, null)).toBe(ALL_SERVERS);
});

test('serverTabBadges omits zero counts', () => {
  expect(serverTabBadges({ connId: 'x', connState: 'connected', shells: 2, pending: 1 })).toStrictEqual({
    symbol: '●', shells: '⧉2', pending: '⏳1',
  });
  expect(serverTabBadges({ connId: 'x', connState: 'disconnected', shells: 0, pending: -3 })).toStrictEqual({
    symbol: '○', shells: '', pending: '',
  });
});

test('buildServerTabs: All first, sessions order kept, selection normalized', () => {
  const v = buildServerTabs(sessions, { selected: 'c1' });
  expect(v.selected).toBe('c1');
  expect(v.tabs.map((x) => x.connId)).toStrictEqual(['', 'c1', 'c2', 'c3', 'c4']);
  expect(v.tabs.map((x) => x.selected)).toStrictEqual([false, true, false, false, false]);
  expect(v.tabs[0].pending).toBe(3);
  expect(v.tabs[0].shells).toBe(3);

  const v2 = buildServerTabs(sessions, { selected: 'deleted', extraPending: 4 });
  expect(v2.selected).toBe(ALL_SERVERS);
  expect(v2.tabs[0].selected).toBe(true);
  expect(v2.tabs[0].badges.pending).toBe('⏳7');
  expect(v2.tabs[4].kind).toBe('serial');
});

test('serverTabText renders the §6.4 wire text', () => {
  const v = buildServerTabs(sessions);
  expect(v.tabs.map((x) => serverTabText(x, tJa))).toStrictEqual([
    'すべて ⏳3', '● web-01 ⧉2 ⏳1', '○ db-stg', '▲ router-01 ⏳2', '◆ usb-serial ⧉1',
  ]);
  expect(serverTabText(buildServerTabs([]).tabs[0], tEn)).toBe('All');
});

test('serverTabAriaLabel in JA and EN, zero items omitted', () => {
  const v = buildServerTabs(sessions);
  expect(serverTabAriaLabel(v.tabs[1], tJa, 'ja')).toBe('web-01、接続中、シェル2、承認待ち1件');
  expect(serverTabAriaLabel(v.tabs[1], tEn, 'en')).toBe('web-01, Connected, 2 shells, 1 awaiting approval');
  expect(serverTabAriaLabel(v.tabs[2], tJa, 'ja')).toBe('db-stg、未接続');
  expect(serverTabAriaLabel(v.tabs[0], tJa, 'ja')).toBe('すべての接続先、承認待ち3件');
  expect(serverTabAriaLabel(buildServerTabs([]).tabs[0], tEn, 'en')).toBe('All connections');
});

test('createServerFilterSetter writes state.serverFilter and reports changes only', () => {
  const state = { serverFilter: '' };
  const calls = [];
  const set = createServerFilterSetter(state, { onChange: (id) => calls.push(id) });
  expect(set('c1')).toBe(true);
  expect(state.serverFilter).toBe('c1');
  expect(set('c1')).toBe(false);
  expect(set('')).toBe(true);
  expect(state.serverFilter).toBe('');
  expect(set(null)).toBe(false);
  expect(calls).toStrictEqual(['c1', '']);
});

test('nextTabIndex: arrows wrap, Home/End, other keys ignored', () => {
  expect(nextTabIndex('ArrowRight', 2, 3)).toBe(0);
  expect(nextTabIndex('ArrowLeft', 0, 3)).toBe(2);
  expect(nextTabIndex('Home', 2, 3)).toBe(0);
  expect(nextTabIndex('End', 0, 3)).toBe(2);
  expect(nextTabIndex('a', 0, 3)).toBe(-1);
  expect(nextTabIndex('ArrowRight', 0, 0)).toBe(-1);
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
  for (const [inp, want] of table) expect(parseShellLabel(inp), String(inp)).toStrictEqual(want);
  expect(shellTabLabel({ label: 'web-01 #2' })).toBe('web-01 #2');
  expect(shellTabLabel({ label: 'web-01 #2' }, { compact: true })).toBe('#2');
  expect(shellTabLabel({ label: 'old-name #2' }, { connName: 'web-01' })).toBe('web-01 #2');
  expect(shellTabLabel({ label: 'odd', connId: 'c9' })).toBe('odd');
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
  expect(v.items.map((i) => i.text)).toStrictEqual(['web-01 #1', '#2', '#3']);
  expect(v.items.map((i) => i.fullLabel)).toStrictEqual(['web-01 #1', 'web-01 #2', 'web-01 #3']);
  expect(v.items.map((i) => i.active)).toStrictEqual([false, true, false]);
  expect(v.newShell).toStrictEqual({ connId: 'c1', name: 'web-01' });
});

test('buildShellSubtabs with All: every tab labelled, no new-shell item', () => {
  const v = buildShellSubtabs(terminalState(), { serverFilter: '', sessions });
  expect(v.items.map((i) => i.text)).toStrictEqual(['web-01 #1', 'usb-serial #1', 'web-01 #2', 'web-01 #3']);
  expect(v.newShell).toBe(null);
  const stale = buildShellSubtabs(terminalState(), { serverFilter: 'gone', sessions });
  expect(stale.items.length).toBe(4);
  expect(buildShellSubtabs(null).items.length).toBe(0);
});

test('shellTabAriaLabel adds status for exited / starting', () => {
  const v = buildShellSubtabs(terminalState(), { sessions });
  expect(shellTabAriaLabel(v.items[0], tJa, 'ja')).toBe('web-01 #1');
  expect(shellTabAriaLabel(v.items[1], tJa, 'ja')).toBe('usb-serial #1、終了');
  expect(shellTabAriaLabel(v.items[2], tEn, 'en')).toBe('web-01 #2, Connecting');
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
  expect(root.getAttribute('role')).toBe('tablist');
  expect(root.children.length).toBe(6);
  expect(root.children[5].children[1].textContent).toBe('<img src=x onerror=alert(1)>');
  const web = root.children[1];
  expect(web.getAttribute('aria-selected')).toBe('false');
  expect(web.getAttribute('aria-label')).toBe('web-01、接続中、シェル2、承認待ち1件');
  expect(root.children[0].getAttribute('tabindex')).toBe('0');

  root.dispatch('click', { target: web.children[1] });
  expect(state.serverFilter).toBe('c1');
  expect(picks).toStrictEqual(['c1']);

  // re-render: c2 removed, order changed; existing nodes are reused
  const next = [sessions[3], { ...sessions[0], shells: 3 }];
  renderServerTabs(root, buildServerTabs(next, { selected: state.serverFilter }), { t: tEn, lang: 'en', onSelect });
  expect(root.children.map((c) => c.getAttribute('data-key'))).toStrictEqual(['*all', 'c4', 'c1']);
  expect(root.children[2]).toBe(web);
  expect(web.getAttribute('aria-selected')).toBe('true');
  expect(web.children[2].textContent).toBe('⧉3');
  expect(root.children[0].children[1].textContent).toBe('All');

  // keyboard: ArrowRight from c1 wraps to All and selects it
  root.dispatch('keydown', { key: 'ArrowRight', target: web, preventDefault() {} });
  expect(state.serverFilter).toBe('');
  expect(doc.focused).toBe(root.children[0]);
});

test('renderShellSubtabs: tabs plus new-shell button, callbacks', () => {
  const doc = fakeDoc();
  const root = doc.createElement('div');
  const activated = [];
  const opened = [];
  const opts = { t: tJa, onActivate: (id) => activated.push(id), onNewShell: (c) => opened.push(c) };
  renderShellSubtabs(root, buildShellSubtabs(terminalState(), { serverFilter: 'c1', sessions }), opts);
  expect(root.children.map((c) => c.textContent)).toStrictEqual(['web-01 #1', '#2', '#3', '＋新しいシェル']);
  expect(root.getAttribute('aria-label')).toBe('web-01 のシェル');
  expect(root.children[1].classList.contains('is-starting')).toBeTruthy();
  expect(root.children[1].getAttribute('aria-selected')).toBe('true');
  root.dispatch('click', { target: root.children[0] });
  root.dispatch('click', { target: root.children[3] });
  expect(activated).toStrictEqual(['T1']);
  expect(opened).toStrictEqual(['c1']);

  const first = root.children[0];
  renderShellSubtabs(root, buildShellSubtabs(terminalState(), { serverFilter: '', sessions }), opts);
  expect(root.children.map((c) => c.textContent)).toStrictEqual(['web-01 #1', 'usb-serial #1', 'web-01 #2', 'web-01 #3']);
  expect(root.children[0]).toBe(first);
  expect(root.getAttribute('aria-label')).toBe('すべてのシェル');
});

test('serial available / missing: label, symbol and is-<state> class (no raw keys)', () => {
  for (const st of SERVER_CONN_STATES) {
    expect(SERVERTABS_I18N.ja[`srvtab.conn.${st}`], st).toBeTruthy();
    expect(SERVERTABS_I18N.en[`srvtab.conn.${st}`], st).toBeTruthy();
  }
  expect(serverStateSymbol('available')).toBe('○');
  expect(serverStateSymbol('missing')).toBe('○');
  const ser = [
    { connId: 's1', connName: 'COM3', kind: 'serial', connState: 'missing' },
    { connId: 's2', connName: 'usb', kind: 'serial', connState: 'available' },
  ];
  const v = buildServerTabs(ser);
  expect(serverTabAriaLabel(v.tabs[1], tJa)).toBe('COM3、見つかりません');
  expect(serverTabAriaLabel(v.tabs[2], tEn, 'en')).toBe('usb, Available');
  const doc = fakeDoc();
  const root = doc.createElement('div');
  renderServerTabs(root, v, { t: tJa, onSelect() {} });
  expect(root.children[1].classList.contains('is-missing')).toBeTruthy();
  expect(root.children[2].classList.contains('is-available')).toBeTruthy();
  // model.js functions win when the Integrator passes them.
  const tModel = (k) => (k === 'state.conn.missing' ? 'MODEL' : tJa(k));
  expect(serverStateLabel('missing', tModel, (c) => `state.conn.${c}`)).toBe('MODEL');
  const v2 = buildServerTabs(ser, { symbolOf: () => '?' });
  expect(v2.tabs[1].badges.symbol).toBe('?');
  // unknown state never yields a raw key
  expect(serverStateLabel('weird', tJa)).toBe('未接続');
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
  expect(state.serverFilter).toBe('c1');
  expect(prevented).toBe(false);
  root.dispatch('keydown', { key: 'ArrowRight', target: web, preventDefault() {} });
  expect(state.serverFilter).toBe('c2');
});

test('shell sub-tabs stay keyboard-reachable when the active terminal is filtered out', () => {
  const ts = terminalState();
  ts.activeId = 'T2'; // usb-serial, outside the c1 filter
  const v = buildShellSubtabs(ts, { serverFilter: 'c1', sessions });
  expect(v.items.map((i) => i.focusable)).toStrictEqual([true, false, false]);
  const doc = fakeDoc();
  const root = doc.createElement('div');
  renderShellSubtabs(root, v, { t: tJa });
  expect(root.children.slice(0, 3).map((c) => c.getAttribute('tabindex'))).toStrictEqual(['0', '-1', '-1']);
  expect(root.children.every((c) => c.getAttribute('aria-selected') !== 'true')).toBeTruthy();
  // with an active item, only it is focusable
  const v2 = buildShellSubtabs(terminalState(), { serverFilter: 'c1', sessions });
  expect(v2.items.map((i) => i.focusable)).toStrictEqual([false, true, false]);
});

test('serial sub-tab shows port @baud mode from TerminalInfo (§10.6) and follows updates', () => {
  expect(serialDetail({ kind: 'serial', port: '/dev/cu.usbserial-10', baud: 9600, serialMode: '7E1' })).toBe('/dev/cu.usbserial-10 @9600 7E1');
  expect(serialDetail({ kind: 'ssh', port: 'x' })).toBe('');
  expect(serialDetail(null)).toBe('');
  const info = (mode) => [{ id: 'T2', connId: 'c4', kind: 'serial', port: '/dev/cu.usbserial-10', baud: 9600, serialMode: mode }];
  const doc = fakeDoc();
  const root = doc.createElement('div');
  let v = buildShellSubtabs(terminalState(), { serverFilter: 'c4', sessions, terminals: info('8N1') });
  expect(v.items[0].text).toBe('usb-serial #1 /dev/cu.usbserial-10 @9600 8N1');
  renderShellSubtabs(root, v, { t: tJa });
  const node = root.children[0];
  expect(node.getAttribute('title')).toBe('usb-serial #1 /dev/cu.usbserial-10 @9600 8N1');
  // sessions:changed after an in-place setMode
  v = buildShellSubtabs(terminalState(), { serverFilter: 'c4', sessions, terminals: info('7E1') });
  renderShellSubtabs(root, v, { t: tJa });
  expect(root.children[0]).toBe(node);
  expect(node.textContent).toBe('usb-serial #1 /dev/cu.usbserial-10 @9600 7E1');
  expect(node.getAttribute('aria-label')).toBe('usb-serial #1 /dev/cu.usbserial-10 @9600 7E1、終了');
  // SSH tabs are unchanged and have no title
  v = buildShellSubtabs(terminalState(), { serverFilter: 'c1', sessions, terminals: info('7E1') });
  expect(v.items.map((i) => i.text)).toStrictEqual(['web-01 #1', '#2', '#3']);
});
