// Terminal tabs (design-v1 §5.2, phase2-contract §2.3).
//
// Every tab owns one xterm instance mounted in its own .term-pane inside
// #terminal-stack. The stack lives in #terminal-dock, outside #page-root, so
// renderPage() and page navigation never destroy an xterm: only closing a tab
// (or exit with reason "user") disposes it. #page-root only gets the tab strip
// from renderTerminalPage().

import { Terminal } from '@xterm/xterm';
import { FitAddon } from '@xterm/addon-fit';
import {
  CloseTerminal,
  ListTerminalSessions,
  ResizeTerminal,
  SendTerminalInput,
  StartTerminal,
  TestConnectionWithPassphrase,
} from '../wailsjs/go/main/App';
import { EventsOn } from '../wailsjs/runtime/runtime';
import { escapeHtml } from './model';

export const TERMINAL_I18N = {
  ja: {
    'tabs.newShell': '新しいシェル',
    'tabs.close': '閉じる',
    'tabs.closeTab': '{label} を閉じる',
    'tabs.reconnect': '再接続',
    'tabs.reconnecting': '再接続しています',
    'tabs.empty': '開いているターミナルはありません',
    'tabs.emptyHint': '接続先を選んで「新しいシェル」を押してください',
    'tabs.restored': '以前の出力は表示できません',
    'tabs.dropped': '出力が多すぎるため {n} バイトを省略しました',
    'tabs.starting': '接続中…',
    'tabs.exited': '終了',
    'tabs.startFailed': '接続できませんでした',
    'tabs.lost': 'セッションが見つかりません',
    'tabs.exit.user': '閉じました',
    'tabs.exit.remote_exit': 'セッションが終了しました',
    'tabs.exit.conn_lost': '接続が切れました',
    'tabs.exit.device_removed': 'デバイスが外れました',
    'tabs.exit.io_error': '入出力エラー',
    'tabs.selectTarget': '接続先',
    'tabs.noConnections': '接続先がありません',
    'tabs.unnamed': '無名',
  },
  en: {
    'tabs.newShell': 'New shell',
    'tabs.close': 'Close',
    'tabs.closeTab': 'Close {label}',
    'tabs.reconnect': 'Reconnect',
    'tabs.reconnecting': 'Reconnecting',
    'tabs.empty': 'No open terminals',
    'tabs.emptyHint': 'Pick a connection and press "New shell"',
    'tabs.restored': 'Earlier output is not available',
    'tabs.dropped': 'Skipped {n} bytes of output',
    'tabs.starting': 'Connecting…',
    'tabs.exited': 'Ended',
    'tabs.startFailed': 'Could not connect',
    'tabs.lost': 'Session no longer exists',
    'tabs.exit.user': 'Closed',
    'tabs.exit.remote_exit': 'Session ended',
    'tabs.exit.conn_lost': 'Connection lost',
    'tabs.exit.device_removed': 'Device removed',
    'tabs.exit.io_error': 'I/O error',
    'tabs.selectTarget': 'Connection',
    'tabs.noConnections': 'No connections',
    'tabs.unnamed': 'Unnamed',
  },
};

const EXIT_REASONS = ['user', 'remote_exit', 'conn_lost', 'device_removed', 'io_error'];
// One SendTerminalInput carries at most this many UTF-16 code units (the
// backend limit is 64KiB of bytes, so 16Ki chars always fits).
const INPUT_CHUNK = 16 * 1024;
// reconcileTerminalTabs marks an open tab exited only after it has been
// missing from this many consecutive snapshots: a poll that was in flight
// while StartTerminal finished can legitimately lack the new id.
const RECONCILE_MISSES = 2;

const terminal = {
  tabs: new Map(), // id -> { id, connId, kind, baseName, n, label, status, xterm, fit, onDataDisp, sendQueue, pendingInput, sending, lastSize, needsFit, pane, usedSeq, restored, misses }
  order: [],       // id の表示順
  activeId: '',
};
// status: 'starting' | 'open' | 'exited'

let ctx = null;
let stack = null;
let eventsWired = false;
let resizeWired = false;
let fitFrame = 0;
let useCounter = 0;
let newTarget = ''; // 「新しいシェル」の select で選ばれている connId
const labelCounters = new Map(); // connId -> 最後に使った #n（減らさない）

function tr(key, vars) {
  return ctx?.t ? ctx.t(key, vars) : key;
}

function b64ToBytes(b64) {
  const bin = atob(b64);
  const bytes = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i += 1) bytes[i] = bin.charCodeAt(i);
  return bytes;
}

function findConnection(connId) {
  return (ctx?.state?.data?.connections ?? []).find((conn) => conn.id === connId) ?? null;
}

function connKind(conn) {
  return conn?.type === 'Serial' ? 'serial' : 'ssh';
}

function nextLabelNumber(connId) {
  const n = (labelCounters.get(connId) ?? 0) + 1;
  labelCounters.set(connId, n);
  return n;
}

function tabLabel(baseName, n) {
  return `${baseName || tr('tabs.unnamed')} #${n}`;
}

function terminalPageActive() {
  return ctx?.state?.active === 'terminal';
}

// Redraw the tab strip if it is on screen. The xterm panes are untouched.
function renderTabsIfVisible() {
  if (terminalPageActive() && typeof ctx.renderPage === 'function') ctx.renderPage();
}

function stackVisible() {
  return Boolean(stack) && !stack.hidden;
}

export function newTerminalId() {
  const c = globalThis.crypto;
  if (c && typeof c.randomUUID === 'function') return `TERM-${c.randomUUID()}`;
  const bytes = new Uint8Array(16);
  if (c && typeof c.getRandomValues === 'function') {
    c.getRandomValues(bytes);
  } else {
    for (let i = 0; i < bytes.length; i += 1) bytes[i] = Math.floor(Math.random() * 256);
  }
  bytes[6] = (bytes[6] & 0x0f) | 0x40; // version 4
  bytes[8] = (bytes[8] & 0x3f) | 0x80; // RFC 4122 variant
  const hex = Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('');
  return `TERM-${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}

function ensureStack() {
  if (stack && stack.isConnected) return stack;
  stack = document.getElementById('terminal-stack');
  if (!stack) {
    stack = document.createElement('div');
    stack.id = 'terminal-stack';
    stack.className = 'terminal-stack';
    stack.hidden = true;
    const dock = document.getElementById('terminal-dock') ?? document.body;
    dock.appendChild(stack);
  }
  return stack;
}

function wireResize() {
  if (resizeWired) return;
  resizeWired = true;
  const onResize = () => scheduleFit();
  if (typeof ResizeObserver === 'function') {
    new ResizeObserver(onResize).observe(stack);
  } else if (typeof window !== 'undefined') {
    window.addEventListener('resize', onResize);
  }
}

// Coalesce bursts of resize notifications into one fit per frame.
function scheduleFit() {
  for (const [id, entry] of terminal.tabs) {
    if (id !== terminal.activeId) entry.needsFit = true;
  }
  if (typeof requestAnimationFrame !== 'function') {
    fitActive();
    return;
  }
  if (fitFrame) return;
  fitFrame = requestAnimationFrame(() => {
    fitFrame = 0;
    fitActive();
  });
}

function fitActive() {
  const entry = terminal.tabs.get(terminal.activeId);
  if (entry) fitTab(entry);
}

// Fit one tab to its pane and tell the backend when the grid changed. Only
// the active tab is visible, so only it can be measured; the rest stay
// needsFit until activated.
function fitTab(entry) {
  if (!stackVisible() || entry.id !== terminal.activeId) {
    entry.needsFit = true;
    return;
  }
  try {
    entry.fit.fit();
  } catch (error) {
    // pane not laid out yet; ignored.
    entry.needsFit = true;
    return;
  }
  entry.needsFit = false;
  syncSize(entry);
}

function currentSize(entry) {
  return { cols: entry.xterm.cols || 80, rows: entry.xterm.rows || 24 };
}

function syncSize(entry) {
  if (entry.status !== 'open') return;
  const size = currentSize(entry);
  if (entry.lastSize && entry.lastSize.cols === size.cols && entry.lastSize.rows === size.rows) return;
  entry.lastSize = size;
  Promise.resolve(ResizeTerminal(entry.id, size.cols, size.rows)).catch(() => {});
}

function createTab(id, connId, kind, baseName, n) {
  const pane = document.createElement('div');
  pane.className = 'term-pane';
  pane.dataset.termId = id;
  ensureStack().appendChild(pane);

  const xterm = new Terminal({
    fontSize: 13,
    fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Consolas, monospace',
    cursorBlink: true,
    theme: { background: '#0b1120', foreground: '#d8e2f2' },
  });
  const fit = new FitAddon();
  xterm.loadAddon(fit);
  xterm.open(pane);

  const entry = {
    id,
    connId,
    kind,
    baseName,
    n,
    label: tabLabel(baseName, n),
    status: 'starting',
    xterm,
    fit,
    onDataDisp: null,
    sendQueue: Promise.resolve(),
    pendingInput: '',
    sending: false,
    lastSize: null,
    needsFit: true,
    pane,
    usedSeq: 0,
    restored: false,
    misses: 0,
  };
  // The closure holds the entry, not the id: reconnect re-keys the entry.
  entry.onDataDisp = xterm.onData((data) => queueInput(entry, data));
  pane.addEventListener('mousedown', () => {
    if (entry.xterm) entry.xterm.focus();
  });

  terminal.tabs.set(id, entry);
  terminal.order.push(id);
  return entry;
}

function removeTab(id) {
  const entry = terminal.tabs.get(id);
  if (!entry) return false;
  const index = terminal.order.indexOf(id);
  if (index >= 0) terminal.order.splice(index, 1);
  terminal.tabs.delete(id);
  entry.status = 'exited';
  entry.pendingInput = '';
  try {
    entry.onDataDisp?.dispose();
    entry.xterm.dispose();
  } catch (error) {
    // already disposed; ignored.
  }
  entry.pane.remove();
  if (terminal.activeId === id) {
    terminal.activeId = '';
    const next = terminal.order[Math.min(index, terminal.order.length - 1)];
    if (next) activateTerminalTab(next);
  }
  return true;
}

function setPaneActive() {
  for (const [id, entry] of terminal.tabs) {
    entry.pane.classList.toggle('is-active', id === terminal.activeId);
  }
}

function focusActive() {
  const entry = terminal.tabs.get(terminal.activeId);
  if (!entry || !stackVisible() || entry.status === 'exited') return;
  // DOM may still be settling right after a render; focus on the next tick.
  setTimeout(() => {
    if (terminal.tabs.get(terminal.activeId) === entry && stackVisible()) entry.xterm.focus();
  }, 0);
}

export function activateTerminalTab(id) {
  const entry = terminal.tabs.get(id);
  if (!entry) return;
  terminal.activeId = id;
  useCounter += 1;
  entry.usedSeq = useCounter;
  setPaneActive();
  fitTab(entry);
  focusActive();
}

export function activateConnection(connId) {
  let best = null;
  for (const entry of terminal.tabs.values()) {
    if (entry.connId === connId && (!best || entry.usedSeq > best.usedSeq)) best = entry;
  }
  if (!best) return false;
  activateTerminalTab(best.id);
  renderTabsIfVisible();
  return true;
}

export function terminalTabCount(connId) {
  // Live tabs only (starting/open). Exited tabs hold no backend session.
  let count = 0;
  for (const entry of terminal.tabs.values()) {
    if (entry.status === 'exited') continue;
    if (connId === undefined || entry.connId === connId) count += 1;
  }
  return count;
}

export function getTerminalState() {
  return terminal;
}

// ---- input ---------------------------------------------------------------

function queueInput(entry, data) {
  if (entry.status !== 'open' || terminal.tabs.get(entry.id) !== entry) return;
  entry.pendingInput += data;
  if (entry.sending) return;
  entry.sending = true;
  entry.sendQueue = entry.sendQueue.then(() => flushInput(entry));
}

function takeChunk(text) {
  if (text.length <= INPUT_CHUNK) return text.length;
  let end = INPUT_CHUNK;
  const code = text.charCodeAt(end - 1);
  if (code >= 0xd800 && code <= 0xdbff) end -= 1; // don't split a surrogate pair
  return end;
}

async function flushInput(entry) {
  try {
    while (entry.pendingInput && entry.status === 'open' && terminal.tabs.get(entry.id) === entry) {
      const end = takeChunk(entry.pendingInput);
      const chunk = entry.pendingInput.slice(0, end);
      entry.pendingInput = entry.pendingInput.slice(end);
      let result;
      try {
        result = await SendTerminalInput(entry.id, chunk);
      } catch (error) {
        result = { ok: false, message: String(error) };
      }
      if (!result?.ok) {
        entry.pendingInput = '';
        // no_session: the session is gone and terminal:exit tells the user.
        if (result?.code !== 'no_session' && result?.message) ctx?.toast?.(result.message);
        break;
      }
    }
  } finally {
    entry.sending = false;
  }
}

// ---- backend events ------------------------------------------------------

function onTerminalData(payload) {
  const entry = payload && terminal.tabs.get(payload.id);
  if (!entry) return; // 知らない id は捨てる
  if (payload.dropped > 0) {
    entry.xterm.write(`\r\n\x1b[2m[${tr('tabs.dropped', { n: payload.dropped })}]\x1b[0m\r\n`);
  }
  if (payload.data) entry.xterm.write(b64ToBytes(payload.data));
}

function exitLine(reason, message, exitCode) {
  const key = EXIT_REASONS.includes(reason) ? reason : 'conn_lost';
  const detail = message ? `: ${message}` : '';
  const code = exitCode != null ? ` (${exitCode})` : '';
  return `\r\n\x1b[33m[${tr(`tabs.exit.${key}`)}${detail}${code}]\x1b[0m\r\n`;
}

function markExited(entry, line) {
  entry.status = 'exited';
  entry.pendingInput = '';
  entry.misses = 0;
  entry.xterm.options.disableStdin = true;
  if (line) entry.xterm.write(line);
}

function onTerminalExit(payload) {
  const entry = payload && terminal.tabs.get(payload.id);
  if (!entry) return;
  if (payload.reason === 'user') {
    removeTab(payload.id);
  } else {
    markExited(entry, exitLine(payload.reason, payload.message, payload.exitCode));
  }
  renderTabsIfVisible();
}

// ---- lifecycle -----------------------------------------------------------

export function initTerminalTabs(appCtx) {
  ctx = appCtx;
  ensureStack();
  wireResize();
  if (eventsWired) return;
  eventsWired = true;
  EventsOn('terminal:data', onTerminalData);
  EventsOn('terminal:exit', onTerminalExit);
}

// Returns false when the backend could not be asked, so the caller can retry.
export async function restoreTerminalTabs() {
  let infos;
  try {
    infos = await ListTerminalSessions();
  } catch (error) {
    return false;
  }
  const missing = (infos ?? [])
    .filter((info) => info?.id && !terminal.tabs.has(info.id))
    .sort((a, b) => String(a.startedAt).localeCompare(String(b.startedAt)));
  if (!missing.length) return true;
  for (const info of missing) {
    const entry = createTab(info.id, info.connId, info.kind === 'serial' ? 'serial' : 'ssh', info.connName, nextLabelNumber(info.connId));
    entry.restored = true;
    entry.status = info.status === 'open' ? 'open' : 'starting';
    entry.xterm.write(`\x1b[2m[${tr('tabs.restored')}]\x1b[0m\r\n`);
  }
  if (!terminal.tabs.has(terminal.activeId)) activateTerminalTab(terminal.order[0]);
  renderTabsIfVisible();
  return true;
}

// Mark open tabs the backend no longer has as exited (a lost terminal:exit).
// Starting tabs are left to their StartTerminal call, except restored ones,
// which have no call to wait for. Also follows connection renames.
export function reconcileTerminalTabs(terminals) {
  const live = new Map((terminals ?? []).map((info) => [info.id, info]));
  let changed = false;
  for (const entry of terminal.tabs.values()) {
    const info = live.get(entry.id);
    if (info?.connName && info.connName !== entry.baseName) {
      entry.baseName = info.connName;
      entry.label = tabLabel(entry.baseName, entry.n);
      changed = true;
    }
    if (entry.status === 'exited') continue;
    if (entry.status === 'starting' && !entry.restored) continue;
    if (info) {
      entry.misses = 0;
      if (entry.status === 'starting' && info.status === 'open') {
        entry.status = 'open';
        syncSize(entry);
        changed = true;
      }
      continue;
    }
    entry.misses += 1;
    if (entry.misses >= RECONCILE_MISSES) {
      markExited(entry, `\r\n\x1b[33m[${tr('tabs.lost')}]\x1b[0m\r\n`);
      changed = true;
    }
  }
  if (changed) renderTabsIfVisible();
}

export function setTerminalPageVisible(visible) {
  const el = ensureStack();
  const wasHidden = el.hidden;
  el.hidden = !visible;
  if (!visible) return;
  const entry = terminal.tabs.get(terminal.activeId);
  if (!entry) return;
  if (wasHidden || entry.needsFit) fitTab(entry);
  if (wasHidden) focusActive();
}

// StartTerminal for an entry already in the map (status 'starting'). On
// needs_passphrase the same id is retried once after the passphrase is
// accepted. Returns the openTerminalTab result shape.
async function startSession(entry, { fresh }) {
  const conn = findConnection(entry.connId);
  let retried = false;
  for (;;) {
    const id = entry.id;
    const size = currentSize(entry);
    entry.lastSize = size;
    let result;
    try {
      result = await StartTerminal(id, entry.connId, size.cols, size.rows);
    } catch (error) {
      result = { ok: false, code: 'connect_failed', message: String(error) };
    }
    result = result ?? { ok: false, code: 'connect_failed', message: '' };
    // Closed (or re-keyed by reconnect) while connecting.
    if (terminal.tabs.get(id) !== entry || entry.id !== id) {
      return { ok: false, id, code: result.ok ? 'canceled' : (result.code || 'canceled'), message: result.message || '' };
    }
    if (result.ok) {
      // An exit may have arrived before StartTerminal resolved; keep it.
      if (entry.status === 'starting') {
        entry.status = 'open';
        entry.misses = 0;
        if (terminal.activeId === id) fitTab(entry);
        syncSize(entry);
        if (terminal.activeId === id) focusActive();
      }
      renderTabsIfVisible();
      return { ok: true, id, message: result.message || '' };
    }
    if (result.code === 'needs_passphrase' && !retried && conn && ctx?.promptPassphrase) {
      retried = true;
      const pass = await ctx.promptPassphrase(conn);
      if (terminal.tabs.get(id) !== entry) return { ok: false, id, code: 'canceled', message: '' };
      if (pass) {
        let test;
        try {
          test = await TestConnectionWithPassphrase(entry.connId, pass);
        } catch (error) {
          test = { ok: false, message: String(error) };
        }
        if (terminal.tabs.get(id) !== entry) return { ok: false, id, code: 'canceled', message: '' };
        if (test?.ok) continue;
        result = { ok: false, code: test?.code || 'needs_passphrase', message: test?.message || result.message };
      } else {
        result = { ok: false, code: 'canceled', message: '' };
      }
    }
    if (fresh) {
      removeTab(id);
    } else {
      // A reconnect keeps the scrollback; the tab goes back to exited.
      markExited(entry, `\r\n\x1b[31m[${tr('tabs.startFailed')}${result.message ? `: ${result.message}` : ''}]\x1b[0m\r\n`);
    }
    if (result.code !== 'canceled' && result.message) ctx?.toast?.(result.message);
    renderTabsIfVisible();
    return { ok: false, id, code: result.code || 'connect_failed', message: result.message || '' };
  }
}

export async function openTerminalTab(connId) {
  if (!ctx) return { ok: false, id: '', code: 'not_ready', message: '' };
  if (!(await ctx.navigate('terminal'))) return { ok: false, id: '', code: 'canceled', message: '' };
  const conn = findConnection(connId);
  const id = newTerminalId();
  const entry = createTab(id, connId, connKind(conn), conn?.name ?? '', nextLabelNumber(connId));
  newTarget = connId;
  activateTerminalTab(id);
  renderTabsIfVisible();
  return startSession(entry, { fresh: true });
}

export async function closeTerminalTab(id) {
  const existed = removeTab(id);
  if (existed) renderTabsIfVisible();
  try {
    const result = await CloseTerminal(id);
    if (result && !result.ok && result.message) ctx?.toast?.(result.message);
  } catch (error) {
    ctx?.toast?.(String(error));
  }
}

// Drops every frontend tab of connId, exited ones included. Called after
// DisconnectConnection / DeleteConnection succeeds: a tab that was
// reconnecting gets StartTerminal "canceled" (no terminal:exit(user)), and
// exited tabs hold no backend session, so neither would leave otherwise.
export function closeTabsForConnection(connId) {
  if (!connId) return 0;
  const ids = terminal.order.filter((id) => terminal.tabs.get(id)?.connId === connId);
  for (const id of ids) removeTab(id);
  if (ids.length) renderTabsIfVisible();
  return ids.length;
}

export async function reconnectTerminalTab(id) {
  const entry = terminal.tabs.get(id);
  if (!entry || entry.status !== 'exited') return;
  const newId = newTerminalId();
  terminal.tabs.delete(id);
  terminal.tabs.set(newId, entry);
  const index = terminal.order.indexOf(id);
  if (index >= 0) terminal.order[index] = newId;
  if (terminal.activeId === id) terminal.activeId = newId;
  entry.id = newId;
  entry.pane.dataset.termId = newId;
  entry.status = 'starting';
  entry.restored = false;
  entry.misses = 0;
  entry.pendingInput = '';
  entry.lastSize = null;
  entry.xterm.options.disableStdin = false;
  entry.xterm.write(`\r\n\x1b[2m──── ${tr('tabs.reconnecting')} ────\x1b[0m\r\n`);
  activateTerminalTab(newId);
  renderTabsIfVisible();
  await startSession(entry, { fresh: false });
}

// ---- #page-root ----------------------------------------------------------

function tabHtml(entry, t) {
  const active = entry.id === terminal.activeId;
  const stateClass = entry.status === 'exited' ? ' is-exited' : entry.status === 'starting' ? ' is-starting' : '';
  const icon = entry.kind === 'serial' ? 'bi-usb-symbol' : 'bi-hdd-network';
  const statusText = entry.status === 'exited' ? t('tabs.exited') : entry.status === 'starting' ? t('tabs.starting') : '';
  const id = escapeHtml(entry.id);
  return `
    <div class="term-tab${active ? ' is-active' : ''}${stateClass}" role="tab" aria-selected="${active ? 'true' : 'false'}">
      <button type="button" data-action="term-activate" data-term-id="${id}" title="${escapeHtml(entry.label)}"><i class="bi ${icon} me-1" aria-hidden="true"></i>${escapeHtml(entry.label)}${statusText ? ` <span class="term-tab-status">(${escapeHtml(statusText)})</span>` : ''}</button>
      ${entry.status === 'exited' ? `<button type="button" class="term-tab-reconnect" data-action="term-reconnect" data-term-id="${id}">${escapeHtml(t('tabs.reconnect'))}</button>` : ''}
      <button type="button" class="term-tab-close" data-action="term-close" data-term-id="${id}" aria-label="${escapeHtml(t('tabs.closeTab', { label: entry.label }))}" title="${escapeHtml(t('tabs.close'))}"><i class="bi bi-x"></i></button>
    </div>
  `;
}

export function renderTerminalPage(state, t) {
  const connections = state?.data?.connections ?? [];
  if (!connections.some((conn) => conn.id === newTarget)) {
    const active = terminal.tabs.get(terminal.activeId);
    newTarget = connections.some((conn) => conn.id === active?.connId) ? active.connId : (connections[0]?.id ?? '');
  }
  const tabs = terminal.order.map((id) => terminal.tabs.get(id)).filter(Boolean);
  return `
    <div class="term-toolbar">
      <div class="term-tabs" role="tablist">
        ${tabs.map((entry) => tabHtml(entry, t)).join('')}
      </div>
      <div class="term-new">
        <select class="form-select form-select-sm" data-action="term-new-select" aria-label="${escapeHtml(t('tabs.selectTarget'))}" ${connections.length ? '' : 'disabled'}>
          ${connections.length
            ? connections.map((conn) => `
              <option value="${escapeHtml(conn.id)}" ${conn.id === newTarget ? 'selected' : ''}>[${conn.type === 'Serial' ? 'Serial' : 'SSH'}] ${escapeHtml(conn.name || t('tabs.unnamed'))}</option>
            `).join('')
            : `<option value="">${escapeHtml(t('tabs.noConnections'))}</option>`}
        </select>
        <button class="btn btn-sm btn-primary" type="button" data-action="term-new" ${connections.length ? '' : 'disabled'}><i class="bi bi-plus-lg me-1"></i>${escapeHtml(t('tabs.newShell'))}</button>
      </div>
    </div>
    ${tabs.length ? '' : `<div class="term-empty"><div class="fw-bold">${escapeHtml(t('tabs.empty'))}</div><div class="text-secondary small">${escapeHtml(t('tabs.emptyHint'))}</div></div>`}
  `;
}

export async function handleTerminalClick(event, appCtx) {
  const target = event.target?.closest?.('[data-action^="term-"]');
  if (!target) return false;
  const action = target.dataset.action;
  const id = target.dataset.termId ?? '';
  if (appCtx) ctx = appCtx;
  if (action === 'term-activate') {
    activateTerminalTab(id);
    renderTabsIfVisible();
    return true;
  }
  if (action === 'term-close') {
    await closeTerminalTab(id);
    return true;
  }
  if (action === 'term-reconnect') {
    await reconnectTerminalTab(id);
    return true;
  }
  if (action === 'term-new') {
    const select = target.closest('.term-new')?.querySelector('[data-action="term-new-select"]');
    const connId = select?.value || newTarget;
    if (connId) await openTerminalTab(connId);
    return true;
  }
  // term-new-select is handled on change.
  return action === 'term-new-select';
}

export function handleTerminalChange(event) {
  const target = event.target?.closest?.('[data-action="term-new-select"]');
  if (!target) return false;
  newTarget = target.value;
  return true;
}
