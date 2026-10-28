// servertabs.js — §6.4 global server tabs and the terminal-page shell sub-tabs.
//
// Data shapes come from docs/spec/phase2-contract.md:
//   - sessions:  SessionSummary[] (state.data.sessions) — connId, connName, kind,
//                connState, shells, connecting, running, pending, label
//   - terminal:  getTerminalState() of terminal-tabs.js — { tabs: Map(id -> {connId,
//                label, status, ...}), order: string[], activeId }
//
// This module imports nothing (contract §2.1). User strings only ever reach the
// DOM through textContent / setAttribute; innerHTML is never used.

/** state.serverFilter value meaning "すべて / All". */
export const ALL_SERVERS = '';

export const SERVERTABS_I18N = {
  ja: {
    'srvtab.all': 'すべて',
    'srvtab.tablist': '接続先で絞り込み',
    'srvtab.shells': 'シェル{n}',
    'srvtab.pending': '承認待ち{n}件',
    'srvtab.ariaAll': 'すべての接続先',
    'srvtab.ariaAllPending': 'すべての接続先、承認待ち{n}件',
    'srvtab.subtablist': '{name} のシェル',
    'srvtab.subtablistAll': 'すべてのシェル',
    'srvtab.newShell': '＋新しいシェル',
    'srvtab.newShellAria': '{name} で新しいシェルを開く',
    'srvtab.exited': '終了',
    'srvtab.starting': '接続処理中',
    'srvtab.conn.connected': '接続中',
    'srvtab.conn.connecting': '接続処理中…',
    'srvtab.conn.idle': '待機中',
    'srvtab.conn.disconnected': '未接続',
    'srvtab.conn.error': '接続エラー',
    'srvtab.conn.in_use': '使用中',
    'srvtab.conn.available': '利用可能',
    'srvtab.conn.missing': '見つかりません',
  },
  en: {
    'srvtab.all': 'All',
    'srvtab.tablist': 'Filter by connection',
    'srvtab.shells': '{n} shells',
    'srvtab.pending': '{n} awaiting approval',
    'srvtab.ariaAll': 'All connections',
    'srvtab.ariaAllPending': 'All connections, {n} awaiting approval',
    'srvtab.subtablist': 'Shells on {name}',
    'srvtab.subtablistAll': 'All shells',
    'srvtab.newShell': '+ New shell',
    'srvtab.newShellAria': 'Open a new shell on {name}',
    'srvtab.exited': 'Exited',
    'srvtab.starting': 'Connecting',
    'srvtab.conn.connected': 'Connected',
    'srvtab.conn.connecting': 'Connecting…',
    'srvtab.conn.idle': 'Idle',
    'srvtab.conn.disconnected': 'Not connected',
    'srvtab.conn.error': 'Connection error',
    'srvtab.conn.in_use': 'In use',
    'srvtab.conn.available': 'Available',
    'srvtab.conn.missing': 'Not found',
  },
};

// §6.3 / contract §2.6 symbols; serial available / missing are both ○
// (phase4-serial-patch.md §10.7). Every connState the backend can send in
// phase 4 is listed, so the class loop and the dictionary cover the same set.
// Kept equal to model.js connStateSymbol (model.test.js checks it).
const SYMBOLS = {
  connected: '●',
  in_use: '◆',
  error: '▲',
  connecting: '○',
  disconnected: '○',
  idle: '●',
  available: '○',
  missing: '○',
};

/** Every connState that gets an `is-<state>` class and a `srvtab.conn.*` label. */
export const SERVER_CONN_STATES = Object.freeze(Object.keys(SYMBOLS));

/**
 * Fallback for model.js connStateSymbol (no cross-module import here). The
 * Integrator should pass model.js's function as `symbolOf` so both agree.
 */
export function serverStateSymbol(connState) {
  return SYMBOLS[connState] || '○';
}

/** Label for a connState: model.js connStateLabelKey when given, else srvtab.conn.*. */
export function serverStateLabel(connState, t, labelKeyOf) {
  if (typeof labelKeyOf === 'function') {
    const key = labelKeyOf(connState);
    if (key) return t(key);
  }
  const state = SYMBOLS[connState] ? connState : 'disconnected';
  return t(`srvtab.conn.${state}`);
}

function count(value) {
  const n = Number(value);
  return Number.isFinite(n) && n > 0 ? Math.floor(n) : 0;
}

/** Returns `filter` if it names a saved connection, otherwise ALL_SERVERS. */
export function normalizeServerFilter(filter, sessions) {
  if (!filter) return ALL_SERVERS;
  return (sessions || []).some((s) => s && s.connId === filter) ? filter : ALL_SERVERS;
}

/** Badge strings for one server tab: '⧉2' / '⏳1', empty when zero. */
export function serverTabBadges(tab, symbolOf = serverStateSymbol) {
  const shells = count(tab && tab.shells);
  const pending = count(tab && tab.pending);
  const sym = typeof symbolOf === 'function' ? symbolOf : serverStateSymbol;
  return {
    symbol: tab && tab.connId ? sym(tab.connState) || serverStateSymbol(tab.connState) : '',
    shells: shells ? `⧉${shells}` : '',
    pending: pending ? `⏳${pending}` : '',
  };
}

/**
 * View model for the top tab strip. `extraPending` lets the caller add pending
 * requests whose connection is unknown to the "All" tab total.
 * @returns {{ selected: string, tabs: Array<{key,connId,name,kind,connState,shells,pending,selected,badges}> }}
 */
export function buildServerTabs(sessions, { selected = ALL_SERVERS, extraPending = 0, symbolOf } = {}) {
  const list = (sessions || []).filter((s) => s && s.connId);
  const sel = normalizeServerFilter(selected, list);
  const rows = list.map((s) => {
    const tab = {
      key: s.connId,
      connId: s.connId,
      name: String(s.connName || s.connId),
      kind: s.kind === 'serial' ? 'serial' : 'ssh',
      connState: s.connState || 'disconnected',
      shells: count(s.shells),
      pending: count(s.pending),
      selected: sel === s.connId,
    };
    tab.badges = serverTabBadges(tab, symbolOf);
    return tab;
  });
  const totalPending = rows.reduce((a, r) => a + r.pending, 0) + count(extraPending);
  const all = {
    key: '*all',
    connId: ALL_SERVERS,
    name: '',
    kind: '',
    connState: '',
    shells: rows.reduce((a, r) => a + r.shells, 0),
    pending: totalPending,
    selected: sel === ALL_SERVERS,
  };
  all.badges = { symbol: '', shells: '', pending: totalPending ? `⏳${totalPending}` : '' };
  return { selected: sel, tabs: [all, ...rows] };
}

/** Visible text of a tab, e.g. "● web-01 ⧉2 ⏳1" or "すべて". */
export function serverTabText(tab, t) {
  if (!tab.connId) return [t('srvtab.all'), tab.badges.pending].filter(Boolean).join(' ');
  return [tab.badges.symbol, tab.name, tab.badges.shells, tab.badges.pending].filter(Boolean).join(' ');
}

/** aria-label, e.g. "web-01、接続中、シェル2、承認待ち1件" / "web-01, Connected, 2 shells, 1 awaiting approval". */
export function serverTabAriaLabel(tab, t, lang = 'ja', labelKeyOf) {
  if (!tab.connId) {
    return tab.pending ? t('srvtab.ariaAllPending', { n: tab.pending }) : t('srvtab.ariaAll');
  }
  const parts = [tab.name, serverStateLabel(tab.connState, t, labelKeyOf)];
  if (tab.shells) parts.push(t('srvtab.shells', { n: tab.shells }));
  if (tab.pending) parts.push(t('srvtab.pending', { n: tab.pending }));
  return parts.join(lang === 'en' ? ', ' : '、');
}

/**
 * Callback that stores a tab choice in state.serverFilter.
 * onChange(connId) runs only when the value actually changed (e.g. renderPage).
 */
export function createServerFilterSetter(state, { onChange } = {}) {
  return (connId) => {
    const next = connId ? String(connId) : ALL_SERVERS;
    if ((state.serverFilter || ALL_SERVERS) === next) return false;
    state.serverFilter = next;
    if (onChange) onChange(next);
    return true;
  };
}

/** Index of the tab to focus for a key on a tablist, or -1. (ARIA tabs pattern.) */
export function nextTabIndex(key, current, length) {
  if (length <= 0) return -1;
  switch (key) {
    case 'ArrowRight': return (current + 1) % length;
    case 'ArrowLeft': return (current - 1 + length) % length;
    case 'Home': return 0;
    case 'End': return length - 1;
    default: return -1;
  }
}

// ---- shell sub-tabs (terminal page) ----------------------------------------

/** Splits an F1 label "web-01 #2" into { base: 'web-01', n: 2 }; n is null if absent. */
export function parseShellLabel(label) {
  const s = String(label ?? '');
  const m = /^(.*\S)\s+#(\d+)$/.exec(s) || /^()#(\d+)$/.exec(s);
  if (!m) return { base: s, n: null };
  return { base: m[1], n: Number(m[2]) };
}

/**
 * Label for one shell tab. Under a server filter only the first visible tab
 * carries the server name ("web-01 #1 | #2"); with "All" every tab is labelled
 * "web-01 #2". `connName` (current name from sessions) wins over the name baked
 * into the F1 label, so renames are followed.
 */
export function shellTabLabel(entry, { connName = '', compact = false } = {}) {
  const { base, n } = parseShellLabel(entry && entry.label);
  const name = connName || base || (entry && entry.connId) || '';
  if (n == null) return name;
  return compact ? `#${n}` : `${name} #${n}`;
}

/**
 * Serial line detail of a TerminalInfo, e.g. "/dev/cu.usbserial-10 @9600 7E1"
 * (phase4-serial-patch.md §10.6). Empty for SSH or when nothing is known.
 */
export function serialDetail(info) {
  if (!info || info.kind !== 'serial') return '';
  const parts = [];
  if (info.port) parts.push(String(info.port));
  if (count(info.baud)) parts.push(`@${count(info.baud)}`);
  if (info.serialMode) parts.push(String(info.serialMode));
  return parts.join(' ');
}

/**
 * View model for the terminal sub-tab row.
 * @param terminal { tabs: Map, order: string[], activeId } from getTerminalState()
 * @param opts.terminals TerminalInfo[] (state.data.terminals) — serial tabs show
 *        port / baud / serialMode from it, refreshed on sessions:changed.
 * @returns {{ items: Array<{id,connId,text,fullLabel,detail,status,active,focusable}>, newShell: null | {connId,name} }}
 *   `focusable` marks the one item that gets tabindex=0 (the active one, or the
 *   first item when the active terminal is outside the filter).
 */
export function buildShellSubtabs(terminal, { serverFilter = ALL_SERVERS, sessions = [], terminals = [] } = {}) {
  const tabs = terminal && terminal.tabs instanceof Map ? terminal.tabs : new Map();
  const order = terminal && Array.isArray(terminal.order) ? terminal.order : [...tabs.keys()];
  const names = new Map((sessions || []).filter(Boolean).map((s) => [s.connId, s.connName || '']));
  const infos = new Map((Array.isArray(terminals) ? terminals : []).filter((x) => x && x.id).map((x) => [x.id, x]));
  const filter = normalizeServerFilter(serverFilter, sessions);
  const items = [];
  for (const id of order) {
    const entry = tabs.get(id);
    if (!entry) continue;
    if (filter && entry.connId !== filter) continue;
    const connName = names.get(entry.connId) || '';
    const detail = serialDetail(infos.get(id));
    const label = shellTabLabel(entry, { connName });
    const fullLabel = detail ? `${label} ${detail}` : label;
    const short = filter && items.length > 0 ? shellTabLabel(entry, { connName, compact: true }) : label;
    items.push({
      id,
      connId: entry.connId,
      text: detail ? `${short} ${detail}` : short,
      fullLabel,
      detail,
      status: entry.status || 'open',
      active: id === terminal.activeId,
      focusable: false,
    });
  }
  const focus = items.find((it) => it.active) || items[0];
  if (focus) focus.focusable = true;
  const newShell = filter ? { connId: filter, name: names.get(filter) || filter } : null;
  return { items, newShell };
}

/** aria-label of a shell sub-tab: full label plus status for non-open tabs. */
export function shellTabAriaLabel(item, t, lang = 'ja') {
  const sep = lang === 'en' ? ', ' : '、';
  if (item.status === 'exited') return item.fullLabel + sep + t('srvtab.exited');
  if (item.status === 'starting') return item.fullLabel + sep + t('srvtab.starting');
  return item.fullLabel;
}

// ---- DOM (keyed diff; textContent only) ------------------------------------

const wired = new WeakSet();

function setAttr(el, name, value) {
  if (value == null || value === false) {
    if (el.hasAttribute(name)) el.removeAttribute(name);
  } else if (el.getAttribute(name) !== String(value)) {
    el.setAttribute(name, String(value));
  }
}

function setText(el, text) {
  if (el.textContent !== text) el.textContent = text;
}

function setClass(el, name, on) {
  if (el.classList.contains(name) !== on) el.classList.toggle(name, on);
}

/** Make `parent`'s keyed children equal to `specs`, reusing nodes by data-key. */
function reconcileChildren(parent, specs, create, update) {
  const doc = parent.ownerDocument;
  const existing = new Map();
  for (const child of Array.from(parent.children)) existing.set(child.getAttribute('data-key'), child);
  let cursor = parent.firstElementChild;
  for (const spec of specs) {
    let el = existing.get(spec.key);
    if (el) existing.delete(spec.key);
    else {
      el = create(doc, spec);
      el.setAttribute('data-key', spec.key);
    }
    update(el, spec);
    if (el !== cursor) parent.insertBefore(el, cursor);
    else cursor = cursor.nextElementSibling;
  }
  for (const stale of existing.values()) stale.remove();
}

function wireTablist(container, onPick) {
  if (wired.has(container)) return;
  wired.add(container);
  container.addEventListener('click', (event) => {
    const btn = event.target && event.target.closest ? event.target.closest('[data-key]') : null;
    if (!btn || !container.contains(btn)) return;
    onPick(btn, event);
  });
  container.addEventListener('keydown', (event) => {
    // Modified arrows (e.g. Cmd+Alt+Right = nextServer) belong to shortcuts.js;
    // handling them here too would move twice.
    if (event.metaKey || event.ctrlKey || event.altKey) return;
    const tabs = Array.from(container.querySelectorAll('[role="tab"]'));
    const cur = tabs.indexOf(event.target);
    const next = nextTabIndex(event.key, cur, tabs.length);
    if (cur < 0 || next < 0) return;
    event.preventDefault();
    tabs[next].focus();
    onPick(tabs[next], event);
  });
}

/**
 * Render/diff the top server tabs into `container` (e.g. #server-tabs).
 * opts: { t, lang, onSelect(connId), labelKeyOf } — onSelect is usually main.js
 * setServerFilter; labelKeyOf is model.js connStateLabelKey (falls back to srvtab.conn.*).
 * The latest onSelect is used even though listeners are attached once.
 */
export function renderServerTabs(container, view, { t, lang = 'ja', onSelect, labelKeyOf } = {}) {
  container.__srvtabSelect = onSelect;
  setAttr(container, 'role', 'tablist');
  setAttr(container, 'aria-label', t('srvtab.tablist'));
  setClass(container, 'server-tabs', true);
  wireTablist(container, (btn) => {
    const fn = container.__srvtabSelect;
    if (fn) fn(btn.getAttribute('data-conn-id') || ALL_SERVERS);
  });
  reconcileChildren(
    container,
    view.tabs,
    (doc) => {
      const b = doc.createElement('button');
      b.type = 'button';
      b.className = 'server-tab';
      b.setAttribute('role', 'tab');
      b.setAttribute('data-action', 'srvtab-select');
      for (const part of ['symbol', 'name', 'shells', 'pending']) {
        const span = doc.createElement('span');
        span.className = `server-tab-${part}`;
        if (part !== 'name') span.setAttribute('aria-hidden', 'true');
        b.appendChild(span);
      }
      return b;
    },
    (b, tab) => {
      const [sym, name, shells, pending] = b.children;
      setText(sym, tab.badges.symbol);
      setText(name, tab.connId ? tab.name : t('srvtab.all'));
      setText(shells, tab.badges.shells);
      setText(pending, tab.badges.pending);
      sym.hidden = !tab.badges.symbol;
      shells.hidden = !tab.badges.shells;
      pending.hidden = !tab.badges.pending;
      setAttr(b, 'data-conn-id', tab.connId);
      setAttr(b, 'aria-selected', tab.selected ? 'true' : 'false');
      setAttr(b, 'tabindex', tab.selected ? '0' : '-1');
      setAttr(b, 'aria-label', serverTabAriaLabel(tab, t, lang, labelKeyOf));
      setAttr(b, 'title', serverTabText(tab, t));
      setClass(b, 'is-active', tab.selected);
      setClass(b, 'is-all', !tab.connId);
      setClass(b, 'is-serial', tab.kind === 'serial');
      for (const s of SERVER_CONN_STATES) {
        setClass(b, `is-${s}`, tab.connState === s);
      }
    },
  );
}

/**
 * Render/diff the shell sub-tabs into `container`.
 * opts: { t, lang, onActivate(termId), onNewShell(connId) }.
 */
export function renderShellSubtabs(container, view, { t, lang = 'ja', onActivate, onNewShell } = {}) {
  container.__srvtabSub = { onActivate, onNewShell };
  setAttr(container, 'role', 'tablist');
  setClass(container, 'shell-subtabs', true);
  setAttr(
    container,
    'aria-label',
    view.newShell ? t('srvtab.subtablist', { name: view.newShell.name }) : t('srvtab.subtablistAll'),
  );
  wireTablist(container, (btn) => {
    const h = container.__srvtabSub || {};
    if (btn.getAttribute('data-action') === 'srvtab-new-shell') {
      if (h.onNewShell) h.onNewShell(btn.getAttribute('data-conn-id'));
    } else if (h.onActivate) {
      h.onActivate(btn.getAttribute('data-term-id'));
    }
  });
  const specs = view.items.map((it) => ({ key: `t:${it.id}`, it }));
  if (view.newShell) specs.push({ key: `new:${view.newShell.connId}`, add: view.newShell });
  reconcileChildren(
    container,
    specs,
    (doc, spec) => {
      const b = doc.createElement('button');
      b.type = 'button';
      if (spec.add) {
        b.className = 'shell-subtab-new';
        b.setAttribute('data-action', 'srvtab-new-shell');
      } else {
        b.className = 'shell-subtab';
        b.setAttribute('role', 'tab');
        b.setAttribute('data-action', 'srvtab-sub-activate');
      }
      return b;
    },
    (b, spec) => {
      if (spec.add) {
        setText(b, t('srvtab.newShell'));
        setAttr(b, 'data-conn-id', spec.add.connId);
        setAttr(b, 'aria-label', t('srvtab.newShellAria', { name: spec.add.name }));
        return;
      }
      const it = spec.it;
      setText(b, it.text);
      setAttr(b, 'data-term-id', it.id);
      setAttr(b, 'data-conn-id', it.connId);
      setAttr(b, 'aria-selected', it.active ? 'true' : 'false');
      setAttr(b, 'tabindex', it.focusable || (it.focusable === undefined && it.active) ? '0' : '-1');
      setAttr(b, 'title', it.detail ? it.fullLabel : null);
      setAttr(b, 'aria-label', shellTabAriaLabel(it, t, lang));
      setClass(b, 'is-active', it.active);
      setClass(b, 'is-exited', it.status === 'exited');
      setClass(b, 'is-starting', it.status === 'starting');
    },
  );
}
