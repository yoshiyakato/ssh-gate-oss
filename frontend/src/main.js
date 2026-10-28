import 'admin-lte/dist/css/adminlte.min.css';
import 'admin-lte/dist/js/adminlte.min.js';
import 'bootstrap-icons/font/bootstrap-icons.css';
import '@xterm/xterm/css/xterm.css';
import './style.css';

import logoUrl from './assets/ssh-gate-logo.png';

import {
  DeleteCommandRequest,
  DeleteMCPSettings,
  GenerateMCPToken,
  GetInitialData,
  SaveMCPSettings,
} from '../wailsjs/go/main/App';
import { Environment, EventsOn } from '../wailsjs/runtime/runtime';

import { connStateLabelKey, escapeHtml, interpolate } from './model.js';
import { I18N } from './i18n.js';
import { pageTitle, renderNav as navHtml, renderSkeleton } from './shell.js';
import { handleSidebarClick, updateSidebarStatus } from './sidebar.js';
import {
  confirmLeaveEditor,
  createEditorState,
  editorHasFocus,
  handleEditorChange,
  handleEditorClick,
  handleEditorInput,
  handleEditorSubmit,
  promptPassphrase,
  reconcileEditor,
  refreshEditorChrome,
  renderConnectionsPage,
  selectConnection,
} from './editor.js';
import {
  activateConnection,
  activateTerminalTab,
  closeTabsForConnection,
  closeTerminalTab,
  getTerminalState,
  handleTerminalChange,
  handleTerminalClick,
  initTerminalTabs,
  openTerminalTab,
  reconcileTerminalTabs,
  renderTerminalEmpty,
  renderTerminalNewShell,
  restoreTerminalTabs,
  setTerminalPageVisible,
  setTerminalServerFilter,
} from './terminal-tabs.js';
import {
  createApprovalsState,
  handleApprovalsClick,
  handleApprovalsInput,
  renderCommandsPage,
  selectedCommandRequest,
} from './approvals.js';
import {
  agentsSectionBusy,
  forgetIssuedAgentToken,
  handleAgentsClick,
  handleAgentsInput,
  handleAgentsSubmit,
  mountAgentsSection,
} from './agents.js';
import { ALL_SERVERS, normalizeServerFilter, renderServerTabs, renderShellSubtabs } from './servertabs.js';
import { createShortcutHandler, cycleIndex, cycleServer, detectPlatform, resolveTabIndex } from './shortcuts.js';
import { deriveServerView, loadServerFilter, saveServerFilter } from './serverview.js';
import { handleCredentialsClick, mountKeychainNotices, recoverCredential } from './credentials.js';

document.body.className = 'layout-fixed sidebar-expand-lg bg-body-tertiary';

const app = document.querySelector('#app');

// localStorage can be missing or throw (private mode, blocked site data);
// every use is best-effort.
function safeLocalStorage() {
  try {
    return window.localStorage ?? null;
  } catch {
    return null;
  }
}

const storage = safeLocalStorage();

function detectLang() {
  try {
    const saved = storage?.getItem('sshgate-lang');
    if (saved === 'ja' || saved === 'en') return saved;
  } catch {
    // localStorage unavailable; fall through to navigator.
  }
  return String(navigator.language || '').toLowerCase().startsWith('en') ? 'en' : 'ja';
}

function t(key, vars) {
  return interpolate(I18N[state.lang]?.[key] ?? I18N.ja[key] ?? key, vars);
}

function setLang(lang) {
  state.lang = lang;
  try {
    storage?.setItem('sshgate-lang', lang);
  } catch {
    // Persisting language is best-effort.
  }
  renderAll();
}

const state = {
  lang: detectLang(),
  active: 'connections',
  // Bumped before every user action that changes the backend, so a poll that
  // was already in flight cannot overwrite the result with older data.
  dataGen: 0,
  // Highest sessionsSeq applied; older sessions snapshots are dropped.
  sessionsSeq: 0,
  data: {
    connections: [],
    requests: [],
    agentPolicies: [],
    // Per-agent tokens (no secrets): approvals.js reads them to tell
    // whether an agent can use an approval bypass.
    agentTokens: [],
    // Always the latest poll: the sidebar's MCP block reads it.
    mcp: {},
    storageError: '',
    // Passwords the startup migration could not move to the keychain.
    keychainPending: 0,
    sessions: [],
    terminals: [],
    runningTotal: 0,
  },
  // What the settings form is drawn from. It stops following state.data.mcp
  // while the form has unsaved edits (dirty.settings).
  settingsMcp: {},
  // Owned by editor.js (selection and draft are keyed by connection id).
  editor: createEditorState(),
  // Owned by approvals.js (the cached execution-target lookup).
  approvals: createApprovalsState(),
  selectedRequestId: '',
  // The top server tab (phase4-ui-integration §1): '' is 「すべて」, otherwise
  // a Connection.id. Only setServerFilter / reconcileServerFilter write it.
  serverFilter: loadServerFilter(storage),
  // False until the first successful refresh: the saved value is checked
  // against the connection list only once there is one.
  serverFilterReady: false,
  // Connections saved with settings an open tab does not use yet
  // (reconnect_required / serial_mode_not_applied): their tab offers a
  // reconnect.
  reconnectNeeded: new Set(),
  dirty: {
    settings: false,
    // The approval command textarea was edited; polling must not reset it.
    approval: false,
  },
  deleteCommand: {
    open: false,
    requestId: '',
  },
  // Token returned by GenerateMCPToken; kept only until the user leaves the page.
  issuedToken: '',
};

// 'darwin' | 'windows' | 'linux' from the Wails runtime; editor.js disables
// serial options the OS cannot do. Unknown until Environment() answers.
let platform = '';

// ctx is the only channel between the page modules (phase2-contract §2.2).
const ctx = {
  state,
  t,
  lang: () => state.lang,
  toast: showToast,
  renderPage,
  renderSidebar,
  refresh: refreshData,
  bumpGen: () => ++state.dataGen,
  navigate,
  openTerminal: (connId) => openTerminalTab(connId),
  closeTabsForConnection: (connId) => closeTabsForConnection(connId),
  promptPassphrase: (conn) => promptPassphrase(conn, ctx),
  showConnection,
  sessionFor: (connId) => (state.data.sessions ?? []).find((session) => session.connId === connId),
  copyText,
  // phase4-ui-integration §1.1
  serverFilter: () => state.serverFilter,
  setServerFilter: (connId, opts) => setServerFilter(connId, opts),
  // editor.js: the user moved the list selection (phase4 §2.3, 一覧 → タブ).
  onSelectionChange: (id, { reason } = {}) => {
    if (reason !== 'delete') syncServerFilterFromEditor(id);
  },
  onConnectionSaved: (result) => noteConnectionSaved(result),
  platform: () => platform,
  // needs_password / keyring_unavailable (phase5-keychain §8).
  recoverCredential: (conn, result, opts) => recoverCredential(ctx, conn, result, opts),
  startErrorMessage: (result) => startErrorMessage(result),
  terminalKeyHandler: null, // set below, before initTerminalTabs
};

function field(object, key, fallback = '') {
  return object?.[key] ?? fallback;
}

function boolAttr(value) {
  return value ? 'checked' : '';
}

function mcpStatusClass(status) {
  if (status === '起動中') return 'text-bg-success';
  if (status === 'エラー') return 'text-bg-danger';
  return 'text-bg-secondary';
}

function mcpStatusLabel(status) {
  if (status === '起動中') return t('mcp.status.running');
  if (status === 'エラー') return t('mcp.status.error');
  return t('mcp.status.stopped');
}

function mcpStatusBadge(status, className = '') {
  const value = status || '停止中';
  return `<span class="badge ${mcpStatusClass(value)} ${className}">${escapeHtml(mcpStatusLabel(value))}</span>`;
}

function mcpEndpoint(mcp) {
  return mcp.localURL || `http://127.0.0.1:${mcp.port || 8787}${mcp.mcpPath || '/mcp'}`;
}

function agentConfig(mcp) {
  const server = { type: 'http', url: mcpEndpoint(mcp) };
  if (mcp.bearerEnabled) {
    server.headers = { Authorization: `Bearer ${state.issuedToken || '<発行したトークン>'}` };
  }
  return JSON.stringify({ mcpServers: { 'ssh-gete': server } }, null, 2);
}

async function copyText(text) {
  const value = String(text ?? '');
  if (!value) return;
  try {
    await navigator.clipboard.writeText(value);
  } catch {
    const textarea = document.createElement('textarea');
    textarea.value = value;
    textarea.setAttribute('readonly', '');
    textarea.style.position = 'fixed';
    textarea.style.opacity = '0';
    document.body.appendChild(textarea);
    textarea.select();
    document.execCommand('copy');
    textarea.remove();
  }
  showToast(t('common.copied'));
}

// The toast lives outside #app so showing/hiding it never re-renders the
// page (a full render would reset unsaved form input and the terminal).
// variant 'warning' is for results that need the user to act.
function showToast(message, { variant = 'dark' } = {}) {
  let root = document.querySelector('#toast-root');
  if (!root) {
    root = document.createElement('div');
    root.id = 'toast-root';
    document.body.appendChild(root);
  }
  const tone = variant === 'warning' ? 'text-bg-warning' : 'text-bg-dark';
  root.innerHTML = message
    ? `<div class="toast show align-items-center ${tone} border-0 ssh-toast" role="status"><div class="d-flex"><div class="toast-body">${escapeHtml(message)}</div></div></div>`
    : '';
  window.clearTimeout(showToast.timer);
  showToast.timer = window.setTimeout(() => { root.innerHTML = ''; }, 3200);
}

// StartTerminal failure codes (phase4-serial-patch §10-4, and the keychain
// codes of phase5-keychain): JA shows the backend message (it names the
// device and the other connection), EN a fixed text per code.
const START_ERROR_CODES = ['port_in_use', 'port_busy', 'permission_denied', 'port_not_found', 'invalid_serial_mode', 'open_failed',
  'needs_password', 'keyring_unavailable'];

function startErrorMessage(result) {
  const code = result?.code ?? '';
  if (state.lang === 'en' && START_ERROR_CODES.includes(code)) return t(`term.code.${code}`);
  return String(result?.message ?? '');
}

// SaveConnection codes about open serial tabs (phase4-serial-patch §10-3).
function noteConnectionSaved(result) {
  const code = result?.code ?? '';
  const id = result?.id ?? '';
  if (code === 'serial_mode_applied') {
    state.reconnectNeeded.delete(id);
    showToast(state.lang === 'en' ? t('term.save.serial_mode_applied') : result.message);
    return;
  }
  if (code === 'serial_mode_not_applied' || code === 'reconnect_required') {
    if (id) state.reconnectNeeded.add(id);
    showToast(state.lang === 'en' ? t(`term.save.${code}`) : result.message, { variant: 'warning' });
  }
}

// ---------------------------------------------------------------- rendering

function renderHeader() {
  const title = pageTitle(state.active, t);
  const set = (id, text) => {
    const element = document.getElementById(id);
    if (element) element.textContent = text;
  };
  set('header-title', title);
  set('page-title', title);
  set('breadcrumb-current', title);
  set('breadcrumb-root', t('breadcrumb.root'));
  document.getElementById('sidebar-toggle')?.setAttribute('aria-label', t('sidebar.toggle'));
}

let lastNavHtml = '';

// Rewritten only when it changes, so the 5s poll does not steal focus from a
// nav button. The pending badge is never filtered by the server tab.
function renderNav() {
  const root = document.getElementById('sidebar-nav');
  if (!root) return;
  const waitCount = (state.data.requests ?? []).filter((request) => request.status === 'pending').length;
  const html = navHtml(state.active, t, { commands: waitCount });
  if (html === lastNavHtml) return;
  lastNavHtml = html;
  root.innerHTML = html;
}

// The sidebar always shows every connection (phase4 §2.5).
function renderSidebar() {
  updateSidebarStatus(document.getElementById('sidebar-status'), state.data, { t, lang: state.lang, now: Date.now() });
}

function currentView() {
  return deriveServerView(state, getTerminalState());
}

// applyTerminalFilter mirrors the filter into the panes and moves the active
// tab into it (phase4 §2.4). It returns the view after that move.
function applyTerminalFilter() {
  let view = currentView();
  setTerminalServerFilter(view.filter);
  if (view.activeTermId && view.activeTermId !== getTerminalState().activeId) {
    activateTerminalTab(view.activeTermId);
    view = currentView();
  }
  return view;
}

function selectTerminalTab(termId) {
  activateTerminalTab(termId);
  renderPage();
}

// renderServerTabsBar redraws the top tabs and the shell sub-tabs only.
// Both are diffed by data key, so nodes (and focus) survive every poll.
function renderServerTabsBar(view = currentView()) {
  const tabs = document.getElementById('server-tabs');
  if (tabs) {
    renderServerTabs(tabs, view.tabs, {
      t,
      lang: state.lang,
      onSelect: (connId) => setServerFilter(connId, { source: 'tab' }),
      labelKeyOf: connStateLabelKey,
    });
  }
  const sub = document.getElementById('shell-subtabs');
  if (!sub) return;
  sub.hidden = state.active !== 'terminal';
  if (!sub.hidden) {
    renderShellSubtabs(sub, view.subtabs, {
      t,
      lang: state.lang,
      onActivate: (termId) => selectTerminalTab(termId),
      onNewShell: (connId) => openTerminalTab(connId),
    });
  }
}

// renderTerminalScreen is #page-root on the terminal page (phase4 §2.4): the
// actions of the active tab and, under 「すべて」, the new-shell picker. The
// tab strip itself is #shell-subtabs.
function renderTerminalScreen(view) {
  const entry = getTerminalState().tabs.get(view.activeTermId);
  let actions = '';
  if (entry) {
    const id = escapeHtml(entry.id);
    const exited = entry.status === 'exited';
    const stale = !exited && state.reconnectNeeded.has(entry.connId);
    const note = exited
      ? t(`tabs.exit.${entry.exitReason || 'conn_lost'}`)
      : stale ? t('term.settingsChanged') : '';
    actions = `
      <div class="term-active-actions${exited ? ' is-exited' : ''}${entry.exitReason === 'device_removed' ? ' is-device-removed' : ''}">
        ${note ? `<span class="term-active-note${exited ? ' text-danger-emphasis' : ' text-warning-emphasis'}" role="status"><i class="bi ${exited ? 'bi-exclamation-octagon' : 'bi-exclamation-triangle'} me-1" aria-hidden="true"></i>${escapeHtml(note)}</span>` : ''}
        ${exited ? `<button class="btn btn-sm btn-primary" type="button" data-action="term-reconnect" data-term-id="${id}"><i class="bi bi-arrow-repeat me-1"></i>${escapeHtml(t('tabs.reconnect'))}</button>` : ''}
        ${stale ? `<button class="btn btn-sm btn-warning" type="button" data-action="restart-terminal" data-term-id="${id}"><i class="bi bi-arrow-repeat me-1"></i>${escapeHtml(t('tabs.reconnect'))}</button>` : ''}
        <button class="btn btn-sm btn-outline-secondary" type="button" data-action="term-close" data-term-id="${id}" aria-label="${escapeHtml(t('tabs.closeTab', { label: entry.label }))}"><i class="bi bi-x-lg me-1"></i>${escapeHtml(t('tabs.close'))}</button>
      </div>`;
  }
  return `
    <div class="term-toolbar">
      ${actions}
      ${view.filter === ALL_SERVERS ? renderTerminalNewShell(state, t) : ''}
    </div>
    ${view.subtabs.items.length ? '' : renderTerminalEmpty(state, t, { serverFilter: view.filter })}
  `;
}

// renderPage redraws #page-root only. The terminal panes live in
// #terminal-dock and are only shown or hidden here.
function renderPage() {
  const root = document.getElementById('page-root');
  if (!root) return;
  const view = applyTerminalFilter();
  if (state.active === 'connections') root.innerHTML = renderConnectionsPage(state, ctx);
  else if (state.active === 'commands') renderCommandsPage(root, ctx, view);
  else if (state.active === 'terminal') root.innerHTML = renderTerminalScreen(view);
  else {
    root.innerHTML = renderSettings();
    mountKeychainNotices(root.querySelector('#keychain-notices'), ctx);
    mountAgentsSection(root.querySelector('#agents-root'), ctx);
  }
  // Hidden as a whole when the filter has no tab: another server's pane
  // must not stay visible (and typeable) under this server's tab.
  setTerminalPageVisible(view.stackVisible);
  renderServerTabsBar(view);
}

function renderModals() {
  const root = document.getElementById('modal-root');
  if (root) root.innerHTML = state.deleteCommand.open ? deleteCommandModal() : '';
}

function renderAll() {
  renderHeader();
  renderNav();
  renderSidebar();
  renderPage();
  renderModals();
}

// The page is left alone while the user is in the middle of an edit there.
function pageIsBusy() {
  if (state.active === 'connections') return editorHasFocus();
  if (state.active === 'settings') return state.dirty.settings || agentsSectionBusy();
  if (state.active === 'commands') return state.dirty.approval;
  return false;
}

// ---------------------------------------------------------- server filter

// setServerFilter is the only writer of state.serverFilter (phase4 §2.0). It
// resolves to true when the filter changed. On the connections page the list
// selection follows the tab first, through the unsaved-changes dialog; if
// that is cancelled the filter stays and the tabs are redrawn as they were.
async function setServerFilter(connId, { source = 'tab', render = true } = {}) {
  if (!state.serverFilterReady) {
    renderServerTabsBar();
    return false;
  }
  const next = normalizeServerFilter(connId, state.data.sessions);
  if (next === state.serverFilter) {
    // Pressing the selected tab again re-selects its row (e.g. after the
    // list moved to a new draft).
    if (source === 'tab' && state.active === 'connections' && next && state.editor.selectedId !== next) {
      await selectConnection(next, ctx, { notify: false });
    }
    renderServerTabsBar();
    return false;
  }
  if (state.active === 'connections' && next && source !== 'editor') {
    if (!(await selectConnection(next, ctx, { notify: false }))) {
      renderServerTabsBar();
      return false;
    }
  }
  if (state.active === 'commands' && state.dirty.approval) {
    // The filter can take the edited request off the screen.
    if (!window.confirm(t('cmd.discardEditOnFilter'))) {
      renderServerTabsBar();
      return false;
    }
    state.dirty.approval = false;
  }
  state.serverFilter = next;
  saveServerFilter(storage, next);
  if (render) renderPage();
  else {
    applyTerminalFilter();
    renderServerTabsBar();
  }
  return true;
}

// reconcileServerFilter drops a filter whose connection was deleted
// (phase4 §3.1). At startup ('init') that happens silently.
function reconcileServerFilter({ reason }) {
  if (reason === 'init') state.serverFilterReady = true;
  if (!state.serverFilterReady) return;
  const next = normalizeServerFilter(state.serverFilter, state.data.sessions);
  if (next === state.serverFilter) return;
  state.serverFilter = next;
  saveServerFilter(storage, next);
  if (reason !== 'init') showToast(t('srvfilter.removed'));
}

// List → tab (phase4 §2.3). A new draft ('') does not move the tab; neither
// does a selection whose session is not listed yet (right after a save).
function syncServerFilterFromEditor(id) {
  if (!id || id === state.serverFilter) return;
  if (!(state.data.sessions ?? []).some((session) => session.connId === id)) return;
  setServerFilter(id, { source: 'editor', render: state.active !== 'connections' });
}

// ------------------------------------------------------------ shortcuts

const MODAL_ROOTS = ['#editor-modal-root', '#agents-modal-root', '#credential-modal-root'];

function modalOpen() {
  return MODAL_ROOTS.some((selector) => Boolean(document.querySelector(selector)?.childElementCount))
    || Boolean(state.deleteCommand.open);
}

function isTextInputFocused() {
  const el = document.activeElement;
  if (!el || el === document.body) return false;
  return el.matches?.('input, textarea, select, [contenteditable=""], [contenteditable="true"]') ?? false;
}

function activeTabConnId() {
  const terminal = getTerminalState();
  return terminal.tabs.get(terminal.activeId)?.connId ?? '';
}

// Sub-tab moves follow the filtered order on screen, not every tab.
function moveSubtab(delta) {
  const items = currentView().subtabs.items;
  const current = items.findIndex((item) => item.id === getTerminalState().activeId);
  const next = cycleIndex(current, items.length, delta);
  if (next >= 0) selectTerminalTab(items[next].id);
}

function pickSubtab(index) {
  const items = currentView().subtabs.items;
  const next = resolveTabIndex(index, items.length);
  if (next >= 0) selectTerminalTab(items[next].id);
}

function stepServer(delta) {
  const ids = currentView().tabs.tabs.filter((tab) => tab.connId).map((tab) => tab.connId);
  return setServerFilter(cycleServer(state.serverFilter, ids, delta), { source: 'shortcut' });
}

const shortcutPlatform = detectPlatform();

// One instance goes to every xterm (terminal-tabs.js createTab) and to the
// document: shortcuts.js dedupes per event only within one instance.
const termShortcuts = createShortcutHandler({
  platform: shortcutPlatform,
  enabled: () => state.active === 'terminal' && !modalOpen(),
  actions: {
    newShell: () => {
      const connId = state.serverFilter || activeTabConnId();
      if (connId) return openTerminalTab(connId);
      return undefined;
    },
    closeTab: () => {
      // The tab the screen shows: '' when the selected server has no shells
      // (the raw active id may then be another server's hidden tab).
      const id = currentView().activeTermId;
      if (id) return closeTerminalTab(id);
      return undefined;
    },
    nextTab: () => moveSubtab(+1),
    prevTab: () => moveSubtab(-1),
    selectTab: ({ index }) => pickSubtab(index),
    nextServer: () => stepServer(+1),
    prevServer: () => stepServer(-1),
    allServers: () => setServerFilter(ALL_SERVERS, { source: 'shortcut' }),
  },
  onError: (error) => showToast(String(error?.message ?? error)),
});

// Other pages: server moves only, and never while typing.
const pageShortcuts = createShortcutHandler({
  platform: shortcutPlatform,
  enabled: () => state.active !== 'terminal' && !modalOpen() && !isTextInputFocused(),
  actions: {
    nextServer: () => stepServer(+1),
    prevServer: () => stepServer(-1),
    allServers: () => setServerFilter(ALL_SERVERS, { source: 'shortcut' }),
  },
  onError: (error) => showToast(String(error?.message ?? error)),
});

ctx.terminalKeyHandler = termShortcuts;

document.addEventListener('keydown', (event) => {
  (state.active === 'terminal' ? termShortcuts : pageShortcuts).onDocumentKeydown(event);
});

// ------------------------------------------------------------------ data

function applySessions(next) {
  const seq = Number(next?.sessionsSeq ?? 0);
  if (seq < state.sessionsSeq) return false;
  state.sessionsSeq = seq;
  state.data.sessions = next.sessions ?? [];
  state.data.terminals = next.terminals ?? [];
  state.data.runningTotal = next.runningTotal ?? 0;
  return true;
}

let refreshChain = Promise.resolve();
let refreshesQueued = 0;
let tabsRestored = false;

async function fetchData({ silent = false } = {}) {
  const gen = state.dataGen;
  let next;
  try {
    next = await GetInitialData();
  } catch (error) {
    if (!silent) {
      const root = document.getElementById('page-root') ?? app;
      root.innerHTML = `<div class="fatal-error">${escapeHtml(t('fatal.init'))}<pre>${escapeHtml(error)}</pre></div>`;
    }
    return false;
  }
  // A user action started after this request went out; its own refresh
  // brings newer data.
  if (state.dataGen !== gen) return true;
  const protectSettings = state.active === 'settings' && state.dirty.settings;
  state.data.connections = next.connections ?? [];
  state.data.requests = next.requests ?? [];
  state.data.agentPolicies = next.agentPolicies ?? [];
  state.data.agentTokens = next.agentTokens ?? [];
  state.data.mcp = next.mcp ?? {};
  if (!protectSettings) state.settingsMcp = state.data.mcp;
  state.data.storageError = next.storageError ?? '';
  state.data.keychainPending = Number(next.keychainPending ?? 0);
  applySessions(next);
  reconcileEditor(state);
  reconcileServerFilter({ reason: state.serverFilterReady ? 'refresh' : 'init' });
  for (const id of state.reconnectNeeded) {
    if (!state.data.connections.some((conn) => conn.id === id)) state.reconnectNeeded.delete(id);
  }
  reconcileTerminalTabs(state.data.terminals);
  renderSidebar();
  renderNav();
  if (!pageIsBusy()) renderPage();
  else {
    // The tab bar is outside #page-root: it always follows the data.
    renderServerTabsBar(applyTerminalFilter());
    if (state.active === 'connections') refreshEditorChrome(ctx);
  }
  // Startup restore runs on the first refresh that succeeds, so a failed
  // first GetInitialData does not leave live backend terminals without tabs.
  if (!tabsRestored) {
    tabsRestored = true;
    if (!(await restoreTerminalTabs())) tabsRestored = false;
  }
  return true;
}

// refreshData runs after any refresh already in flight, so a caller that just
// changed the backend always gets a fetch that started after its change. A
// caller's refresh also bumps dataGen, so a poll that went out while the
// change was being made (and may predate it) is discarded instead of
// applied first.
function refreshData(options = {}) {
  if (!options.poll) state.dataGen += 1;
  refreshesQueued += 1;
  const run = refreshChain.then(() => fetchData(options)).finally(() => { refreshesQueued -= 1; });
  refreshChain = run.catch(() => {});
  return run;
}

// The periodic poll never stacks behind another request.
function pollData() {
  if (refreshesQueued > 0) return;
  refreshData({ silent: true, poll: true });
}

EventsOn('sessions:changed', (payload) => {
  if (!applySessions(payload)) return;
  reconcileServerFilter({ reason: 'sessions' });
  renderSidebar();
  // The sub-tabs show live tab states and serial line settings.
  if (state.active === 'terminal') renderPage();
  else {
    renderServerTabsBar(applyTerminalFilter());
    // The editor's connection badge and disconnect button follow the
    // sessions; the inputs are left alone.
    if (state.active === 'connections') refreshEditorChrome(ctx);
  }
});

// ------------------------------------------------------------ navigation

async function navigate(page) {
  if (page === state.active) return true;
  if (state.active === 'connections' && !(await confirmLeaveEditor(ctx))) return false;
  if (state.active === 'settings') {
    state.issuedToken = '';
    state.dirty.settings = false;
    forgetIssuedAgentToken();
  }
  if (state.active === 'commands') state.dirty.approval = false;
  state.active = page;
  renderHeader();
  renderNav();
  renderPage();
  // Tab → list (phase4 §2.3): entering the editor selects the filtered
  // connection. Leaving the editor already went through its guard.
  if (page === 'connections' && state.serverFilter && state.editor.selectedId !== state.serverFilter) {
    await selectConnection(state.serverFilter, ctx, { notify: false });
  }
  return true;
}

// Sidebar row click (phase4 §2.5): select its server tab, then jump to the
// connection's terminal tab if it has one, otherwise open it in the editor.
async function showConnection(connId) {
  const target = normalizeServerFilter(connId, state.data.sessions);
  const changed = await setServerFilter(connId, { source: 'sidebar', render: false });
  // Cancelled in an unsaved-changes dialog (editor, or the edited approval
  // command): stay where we are.
  if (!changed && target && state.serverFilter !== target) return;
  if (activateConnection(connId)) {
    await navigate('terminal');
  } else if (await navigate('connections')) {
    await selectConnection(connId, ctx, { notify: false });
  }
  renderAll();
}

// ---------------------------------------------------------- commands page

function deleteCommandModal() {
  return `
    <div class="modal-backdrop show"></div>
    <div class="modal d-block" tabindex="-1" role="dialog" aria-modal="true">
      <div class="modal-dialog modal-dialog-centered">
        <div class="modal-content">
          <div class="modal-header">
            <h5 class="modal-title fw-bold">${escapeHtml(t('delCmd.title'))}</h5>
            <button class="btn-close" type="button" data-action="close-delete-command" aria-label="${escapeHtml(t('common.close'))}"></button>
          </div>
          <div class="modal-body">
            <p class="mb-2"><strong>${escapeHtml(state.deleteCommand.requestId)}</strong> ${escapeHtml(t('delCmd.body1pre'))}</p>
            <p class="text-secondary mb-0">${escapeHtml(t('delCmd.body2'))}</p>
          </div>
          <div class="modal-footer">
            <button class="btn btn-outline-secondary" type="button" data-action="close-delete-command">${escapeHtml(t('common.cancel'))}</button>
            <button class="btn btn-danger" type="button" data-action="confirm-delete-command">
              <i class="bi bi-trash me-1"></i>${escapeHtml(t('common.delete'))}
            </button>
          </div>
        </div>
      </div>
    </div>
  `;
}

// ---------------------------------------------------------- settings page

function renderSettings() {
  const mcp = state.settingsMcp ?? {};
  const endpoint = mcpEndpoint(mcp);
  const config = agentConfig(mcp);
  return `
    <div id="keychain-notices" class="keychain-notices" hidden></div>
    <form class="row g-3" id="mcp-form">
      <div class="col-12 col-xl-8">
        <div class="card card-outline card-primary">
          <div class="card-header">
            <div class="d-flex align-items-start justify-content-between gap-3">
              <div>
                <h3 class="card-title fw-bold mb-1">${escapeHtml(t('mcp.title'))}</h3>
                <div class="text-secondary small">${escapeHtml(t('mcp.subtitle'))}</div>
              </div>
              ${mcpStatusBadge(mcp.status)}
            </div>
          </div>
          <div class="card-body">
            <div class="row g-3">
              ${checkCol(t('mcp.enable'), 'enabled', mcp.enabled)}
              ${checkCol(t('mcp.bearer'), 'bearerEnabled', mcp.bearerEnabled)}
              ${inputCol(t('mcp.listenAddress'), 'listenAddress', mcp.listenAddress)}
              ${inputCol(t('mcp.port'), 'port', mcp.port, 'number')}
              ${selectCol(t('mcp.transport'), 'transport', mcp.transport, ['Streamable HTTP'])}
              ${inputCol(t('mcp.baseURLHint'), 'baseURL', mcp.baseURL)}
              ${inputCol(t('mcp.mcpPath'), 'mcpPath', mcp.mcpPath)}
              ${inputCol(t('mcp.healthPath'), 'healthPath', mcp.healthPath)}
              ${inputCol(t('mcp.auditPath'), 'auditPath', mcp.auditPath)}
              ${inputCol(t('mcp.tokenName'), 'tokenName', mcp.tokenName)}
              ${inputCol(t('mcp.tokenInput'), 'tokenInput', '', 'password')}
              ${readonlyCol(t('mcp.tokenPreview'), mcp.tokenFromEnv ? t('mcp.tokenFromEnv') : mcp.tokenPreview)}
              ${inputCol(t('mcp.allowedOrigins'), 'allowedOrigins', (mcp.allowedOrigins ?? []).join(', '), 'text', 'col-12')}
              ${selectCol(t('mcp.tlsMode'), 'tlsMode', mcp.tlsMode, ['ローカル平文', '内蔵TLS（未実装）', 'リバースプロキシ終端'], 'col-12 col-md-6', { 'ローカル平文': t('mcp.tls.local'), '内蔵TLS（未実装）': t('mcp.tls.builtin'), 'リバースプロキシ終端': t('mcp.tls.proxy') })}
              ${selectCol(t('mcp.proxyMode'), 'proxyMode', mcp.proxyMode, ['無効', 'nginx', 'Caddy', 'Cloudflare Tunnel'], 'col-12 col-md-6', { '無効': t('mcp.proxy.disabled') })}
              ${inputCol(t('mcp.maxBodyKB'), 'maxBodyKB', mcp.maxBodyKB, 'number')}
              ${inputCol(t('mcp.maxOutputKB'), 'maxOutputKB', mcp.maxOutputKB, 'number')}
              ${inputCol(t('mcp.requestTimeout'), 'requestTimeout', mcp.requestTimeout, 'number')}
              ${inputCol(t('mcp.defaultConnectTimeout'), 'defaultConnectTimeout', mcp.defaultConnectTimeout, 'number')}
              ${inputCol(t('mcp.defaultCommandTimeout'), 'defaultCommandTimeout', mcp.defaultCommandTimeout, 'number')}
              ${checkCol(t('mcp.strictHostKey'), 'strictHostKey', mcp.strictHostKey)}
              ${checkCol(t('mcp.requireApprovalSudo'), 'requireApprovalSudo', mcp.requireApprovalSudo)}
              ${checkCol(t('mcp.requireApprovalProd'), 'requireApprovalProd', mcp.requireApprovalProd)}
              ${checkCol(t('mcp.requireApprovalWriteOp'), 'requireApprovalWriteOp', mcp.requireApprovalWriteOp)}
              ${readonlyCol(t('mcp.configPath'), mcp.configPath, 'col-12')}
            </div>
            ${mcp.bypassAllowed ? '' : `
            <div class="alert alert-warning mt-3 mb-0 d-flex align-items-center justify-content-between gap-3">
              <span>${escapeHtml(t('mcp.bypassOff'))}</span>
              ${mcp.tokenFromEnv ? '' : `<button class="btn btn-warning btn-sm flex-shrink-0" type="button" data-action="generate-token"><i class="bi bi-key me-1"></i>${escapeHtml(t('mcp.generateToken'))}</button>`}
            </div>`}
            ${state.issuedToken ? `
            <div class="alert alert-success mt-3 mb-0">
              <div class="fw-bold mb-1">${escapeHtml(t('mcp.tokenOnce'))}</div>
              <div class="input-group">
                <input class="form-control font-monospace" value="${escapeHtml(state.issuedToken)}" readonly>
                <button class="btn btn-outline-secondary" type="button" data-action="copy-token"><i class="bi bi-copy"></i></button>
              </div>
            </div>` : ''}
            <div class="callout callout-info mt-3 mb-0">${escapeHtml(t('mcp.callout'))}</div>
            ${mcp.lastError ? `<div class="alert alert-danger mt-3 mb-0">${escapeHtml(mcp.lastError)}</div>` : ''}
          </div>
          <div class="card-footer d-flex gap-2">
            <button class="btn btn-outline-primary" type="button" data-action="refresh-mcp"><i class="bi bi-arrow-clockwise me-1"></i>${escapeHtml(t('mcp.refresh'))}</button>
            <button class="btn btn-outline-danger" type="button" data-action="delete-mcp"><i class="bi bi-trash me-1"></i>${escapeHtml(t('mcp.deleteSettings'))}</button>
            <button class="btn btn-primary ms-auto" type="submit">${escapeHtml(t('mcp.saveStart'))}</button>
          </div>
        </div>
      </div>
      <div class="col-12 col-xl-4">
        <div class="card card-outline card-info">
          <div class="card-header">
            <h3 class="card-title fw-bold">${escapeHtml(t('mcp.endpointTitle'))}</h3>
          </div>
          <div class="card-body">
            ${endpointRow('MCP', mcp.mcpPath, mcp.bearerEnabled ? t('mcp.bearerRequired') : t('mcp.noAuth'))}
            ${endpointRow('Health', mcp.healthPath, t('mcp.noAuth'))}
            ${endpointRow('Audit API', mcp.auditPath, mcp.bearerEnabled ? t('mcp.bearerRequired') : t('mcp.noAuth'))}
            <div class="mt-3">
              <label class="form-label fw-bold">${escapeHtml(t('mcp.connectURL'))} — ${escapeHtml(t('mcp.localURL'))}</label>
              <div class="input-group">
                <input class="form-control" value="${escapeHtml(endpoint)}" readonly>
                <button class="btn btn-outline-secondary" type="button" data-action="copy-mcp-endpoint"><i class="bi bi-copy"></i></button>
              </div>
            </div>
            <div class="mt-3">
              <label class="form-label fw-bold">${escapeHtml(t('mcp.connectURL'))} — ${escapeHtml(t('mcp.lanURL'))}</label>
              ${mcp.lanURL ? `
              <div class="input-group">
                <input class="form-control ${mcp.lanReachable ? '' : 'text-secondary'}" value="${escapeHtml(mcp.lanURL)}" readonly>
                <button class="btn btn-outline-secondary" type="button" data-action="copy-lan-url" ${mcp.lanReachable ? '' : 'disabled'}><i class="bi bi-copy"></i></button>
              </div>
              ${mcp.lanReachable ? '' : `<div class="form-text text-warning-emphasis">${escapeHtml(t('mcp.lanUnreachable', { addr: mcp.listenAddress || '127.0.0.1' }))}</div>`}`
              : `<div class="form-text">${escapeHtml(t('mcp.lanNone'))}</div>`}
            </div>
          </div>
        </div>
        <div class="card">
          <div class="card-header">
            <div class="d-flex align-items-center justify-content-between">
              <h3 class="card-title fw-bold">${escapeHtml(t('mcp.usageTitle'))}</h3>
              <button class="btn btn-outline-secondary btn-sm" type="button" data-action="copy-agent-config"><i class="bi bi-copy me-1"></i>${escapeHtml(t('common.copy'))}</button>
            </div>
          </div>
          <div class="card-body">
            <pre class="command-block"><code>${escapeHtml(config)}</code></pre>
            <div class="usage-list mt-3">
              ${usageRow('list_hosts', t('mcp.usage.listHosts'))}
              ${usageRow('execute_command', t('mcp.usage.executeCommand'))}
              ${usageRow('request_command_execution', t('mcp.usage.requestExec'))}
              ${usageRow('get_request_status', t('mcp.usage.getStatus'))}
              ${usageRow('get_command_history', t('mcp.usage.getHistory'))}
            </div>
          </div>
        </div>
      </div>
    </form>
    <div class="row g-3 mt-0">
      <div class="col-12" id="agents-root"></div>
    </div>
  `;
}

function inputCol(label, name, value, type = 'text', col = 'col-12 col-md-6') {
  return `
    <div class="${col}">
      <label class="form-label fw-bold">${label}</label>
      <input class="form-control" name="${name}" type="${type}" value="${escapeHtml(value)}" autocapitalize="none" autocorrect="off" autocomplete="off" spellcheck="false">
    </div>
  `;
}

function readonlyCol(label, value, col = 'col-12 col-md-6') {
  return `
    <div class="${col}">
      <label class="form-label fw-bold">${label}</label>
      <input class="form-control" value="${escapeHtml(value)}" readonly>
    </div>
  `;
}

function selectCol(label, name, selected, options, col = 'col-12 col-md-6', labels = {}) {
  // The option value stays the raw (backend) string so the saved form value is
  // unchanged; only the visible text is localized via the optional labels map.
  return `
    <div class="${col}">
      <label class="form-label fw-bold">${label}</label>
      <select class="form-select" name="${name}">
        ${options.map((option) => `<option value="${escapeHtml(option)}" ${option === selected ? 'selected' : ''}>${escapeHtml(labels[option] ?? option)}</option>`).join('')}
      </select>
    </div>
  `;
}

function checkCol(label, name, value, col = 'col-12 col-md-6') {
  return `
    <div class="${col}">
      <div class="form-check form-switch mt-4">
        <input class="form-check-input" type="checkbox" role="switch" id="${name}" name="${name}" ${boolAttr(value)}>
        <label class="form-check-label fw-bold" for="${name}">${label}</label>
      </div>
    </div>
  `;
}

function endpointRow(label, path, auth) {
  return `
    <div class="endpoint-row">
      <span>${label}</span>
      <code>${escapeHtml(path)}</code>
      <strong>${auth}</strong>
    </div>
  `;
}

function usageRow(name, description) {
  return `
    <div class="usage-row">
      <code>${escapeHtml(name)}</code>
      <span>${escapeHtml(description)}</span>
    </div>
  `;
}

function collectMCPForm() {
  const form = document.querySelector('#mcp-form');
  const data = Object.fromEntries(new FormData(form));
  const checked = (name) => Boolean(form.querySelector(`[name="${name}"]`)?.checked);
  return {
    enabled: checked('enabled'),
    listenAddress: field(data, 'listenAddress'),
    port: Number(field(data, 'port', 8787)),
    transport: field(data, 'transport'),
    baseURL: field(data, 'baseURL'),
    mcpPath: field(data, 'mcpPath'),
    healthPath: field(data, 'healthPath'),
    auditPath: field(data, 'auditPath'),
    bearerEnabled: checked('bearerEnabled'),
    tokenName: field(data, 'tokenName'),
    tokenInput: field(data, 'tokenInput'),
    allowedOrigins: field(data, 'allowedOrigins').split(',').map((origin) => origin.trim()).filter(Boolean),
    tlsMode: field(data, 'tlsMode'),
    proxyMode: field(data, 'proxyMode'),
    maxBodyKB: Number(field(data, 'maxBodyKB', 256)),
    maxOutputKB: Number(field(data, 'maxOutputKB', 128)),
    requestTimeout: Number(field(data, 'requestTimeout', 30)),
    defaultConnectTimeout: Number(field(data, 'defaultConnectTimeout', 10)),
    defaultCommandTimeout: Number(field(data, 'defaultCommandTimeout', 30)),
    strictHostKey: checked('strictHostKey'),
    autoExecuteLowRisk: false,
    requireApprovalSudo: checked('requireApprovalSudo'),
    requireApprovalProd: checked('requireApprovalProd'),
    requireApprovalWriteOp: checked('requireApprovalWriteOp'),
    configPath: state.settingsMcp?.configPath ?? '',
  };
}

// ------------------------------------------------------------- handlers

// The commands page's approve / reject / bypass / copy / selection live in
// approvals.js; what stays here is the delete modal, the 「すべて表示」 of
// the filtered queue, and the settings page.
async function handleCommonClick(event) {
  const actionElement = event.target.closest('[data-action]');
  const action = actionElement?.dataset.action;
  if (!action) return;

  if (action === 'delete-command-request') {
    // The request the detail pane shows (filtered, by id; never by index).
    const requestId = actionElement?.dataset.commandRequestId || selectedCommandRequest(state, currentView())?.id || '';
    if (!requestId) return;
    state.deleteCommand = { open: true, requestId };
    renderModals();
    return;
  }

  if (action === 'close-delete-command') {
    state.deleteCommand = { open: false, requestId: '' };
    renderModals();
    return;
  }

  if (action === 'confirm-delete-command') {
    const requestId = state.deleteCommand.requestId;
    if (!requestId) return;
    ctx.bumpGen();
    const result = await DeleteCommandRequest(requestId);
    if (result.ok) {
      state.data.requests = state.data.requests.filter((request) => request.id !== requestId);
      if (state.selectedRequestId === requestId) {
        state.selectedRequestId = '';
      }
    }
    state.deleteCommand = { open: false, requestId: '' };
    renderModals();
    showToast(result.message);
    await refreshData({ silent: true });
    return;
  }

  if (action === 'srvfilter-show-all') {
    await setServerFilter(ALL_SERVERS, { source: 'commands' });
    return;
  }

  if (action === 'restart-terminal') {
    // An open tab whose saved settings changed: close it and open a new one
    // (a new TERM id) on the same connection.
    const id = actionElement.dataset.termId ?? '';
    const connId = getTerminalState().tabs.get(id)?.connId ?? '';
    if (!connId) return;
    state.reconnectNeeded.delete(connId);
    await closeTerminalTab(id);
    await openTerminalTab(connId);
    return;
  }

  if (action === 'copy-mcp-endpoint') {
    await copyText(mcpEndpoint(state.settingsMcp ?? {}));
    return;
  }

  if (action === 'copy-agent-config') {
    await copyText(agentConfig(state.settingsMcp ?? {}));
    return;
  }

  if (action === 'generate-token') {
    ctx.bumpGen();
    const result = await GenerateMCPToken();
    showToast(result.message);
    if (result.token) state.issuedToken = result.token;
    state.dirty.settings = false;
    await refreshData({ silent: true });
    return;
  }

  if (action === 'copy-token') {
    await copyText(state.issuedToken);
    return;
  }

  if (action === 'copy-lan-url') {
    await copyText(state.settingsMcp?.lanURL ?? '');
    return;
  }

  if (action === 'refresh-mcp') {
    await refreshData({ silent: true });
    showToast(t('mcp.refreshed'));
    return;
  }

  if (action === 'delete-mcp') {
    if (!window.confirm(t('mcp.deleteConfirm'))) {
      return;
    }
    ctx.bumpGen();
    const result = await DeleteMCPSettings();
    showToast(result.message);
    state.dirty.settings = false;
    await refreshData({ silent: true });
  }
}

app.addEventListener('click', async (event) => {
  // servertabs.js handles its own buttons on #server-tabs / #shell-subtabs.
  if (event.target.closest('[data-action^="srvtab-"]')) return;

  const langButton = event.target.closest('[data-lang]');
  if (langButton) {
    const lang = langButton.dataset.lang;
    if (lang && lang !== state.lang) setLang(lang);
    return;
  }

  if (event.target.closest('#sidebar-status') && await handleSidebarClick(event, ctx)) return;

  const nav = event.target.closest('[data-nav]');
  if (nav) {
    await navigate(nav.dataset.nav);
    return;
  }

  if (state.active === 'connections' && await handleEditorClick(event, ctx)) return;
  if (state.active === 'terminal' && await handleTerminalClick(event, ctx)) return;
  if (state.active === 'commands' && await handleApprovalsClick(event, ctx, currentView())) return;
  if (state.active === 'settings' && await handleAgentsClick(event, ctx)) return;
  if (state.active === 'settings' && await handleCredentialsClick(event, ctx)) return;

  await handleCommonClick(event);
});

app.addEventListener('submit', async (event) => {
  event.preventDefault();
  if (state.active === 'settings' && await handleAgentsSubmit(event, ctx)) return;
  if (state.active === 'connections' && await handleEditorSubmit(event, ctx)) return;
  if (event.target.id === 'mcp-form') {
    const form = collectMCPForm();
    ctx.bumpGen();
    const result = await SaveMCPSettings(form);
    showToast(result.message);
    if (!result.ok) {
      state.settingsMcp = { ...state.settingsMcp, ...form, lastError: result.message };
      renderPage();
      return;
    }
    state.dirty.settings = false;
    await refreshData({ silent: true });
  }
});

app.addEventListener('input', (event) => {
  if (state.active === 'connections' && handleEditorInput(event, ctx)) return;
  if (state.active === 'commands' && handleApprovalsInput(event, ctx)) return;
  if (state.active === 'settings' && handleAgentsInput(event)) return;
  if (event.target.closest('#mcp-form')) state.dirty.settings = true;
});

app.addEventListener('change', async (event) => {
  if (state.active === 'connections' && await handleEditorChange(event, ctx)) return;
  if (state.active === 'terminal' && handleTerminalChange(event, ctx)) return;
  if (event.target.closest('#mcp-form')) state.dirty.settings = true;
});

// ------------------------------------------------------------- startup

function loadPlatform() {
  try {
    Promise.resolve(Environment())
      .then((env) => { platform = String(env?.platform ?? ''); })
      .catch(() => {});
  } catch {
    // No Wails runtime (tests); OS-specific serial checks stay off.
  }
}

async function start() {
  app.innerHTML = renderSkeleton({ t, logoUrl });
  // ctx.terminalKeyHandler is already set: every xterm gets the shortcuts.
  initTerminalTabs(ctx);
  loadPlatform();
  renderAll();
  window.setInterval(pollData, 5000);
  // On failure the error stays in #page-root until a later poll succeeds
  // (that poll also restores the terminal tabs and checks the saved filter).
  if (!(await refreshData())) return;
  renderAll();
}

start();
