import 'admin-lte/dist/css/adminlte.min.css';
import 'admin-lte/dist/js/adminlte.min.js';
import 'bootstrap-icons/font/bootstrap-icons.css';
import '@xterm/xterm/css/xterm.css';
import './style.css';

import logoUrl from './assets/ssh-gate-logo.png';

import {
  ApproveCommand,
  ApproveCommandAndBypassAgent,
  ApproveCommandConfirmed,
  DeleteCommandRequest,
  DeleteMCPSettings,
  GenerateMCPToken,
  GetInitialData,
  RejectCommand,
  RevokeAgentApprovalBypass,
  SaveMCPSettings,
} from '../wailsjs/go/main/App';
import { EventsOn } from '../wailsjs/runtime/runtime';

import { escapeHtml, interpolate, requestStateLabelKey } from './model.js';
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
  closeTabsForConnection,
  handleTerminalChange,
  handleTerminalClick,
  initTerminalTabs,
  openTerminalTab,
  reconcileTerminalTabs,
  renderTerminalPage,
  restoreTerminalTabs,
  setTerminalPageVisible,
} from './terminal-tabs.js';

document.body.className = 'layout-fixed sidebar-expand-lg bg-body-tertiary';

const app = document.querySelector('#app');


function detectLang() {
  try {
    const saved = localStorage.getItem('sshgate-lang');
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
    localStorage.setItem('sshgate-lang', lang);
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
    // Always the latest poll: the sidebar's MCP block reads it.
    mcp: {},
    storageError: '',
    sessions: [],
    terminals: [],
    runningTotal: 0,
  },
  // What the settings form is drawn from. It stops following state.data.mcp
  // while the form has unsaved edits (dirty.settings).
  settingsMcp: {},
  // Owned by editor.js (selection and draft are keyed by connection id).
  editor: createEditorState(),
  selectedRequestId: '',
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

// Request status is a backend state code (pending, running, succeeded, ...).
function requestStatusLabel(status) {
  const key = requestStateLabelKey(status);
  const label = t(key);
  return label === key ? status : label;
}

function requestStatusClass(status) {
  if (status === 'pending') return 'text-bg-warning';
  if (status === 'running' || status === 'auto_running') return 'text-bg-info';
  if (status === 'succeeded' || status === 'auto_succeeded') return 'text-bg-success';
  if (status === 'failed' || status === 'auto_failed' || status === 'interrupted') return 'text-bg-danger';
  return 'text-bg-secondary';
}

function formatTimestamp(value) {
  if (!value) return '-';
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  const pad = (n) => String(n).padStart(2, '0');
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())} ${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}`;
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
function showToast(message) {
  let root = document.querySelector('#toast-root');
  if (!root) {
    root = document.createElement('div');
    root.id = 'toast-root';
    document.body.appendChild(root);
  }
  root.innerHTML = message
    ? `<div class="toast show align-items-center text-bg-dark border-0 ssh-toast"><div class="d-flex"><div class="toast-body">${escapeHtml(message)}</div></div></div>`
    : '';
  window.clearTimeout(showToast.timer);
  showToast.timer = window.setTimeout(() => { root.innerHTML = ''; }, 3200);
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
// nav button.
function renderNav() {
  const root = document.getElementById('sidebar-nav');
  if (!root) return;
  const waitCount = (state.data.requests ?? []).filter((request) => request.status === 'pending').length;
  const html = navHtml(state.active, t, { commands: waitCount });
  if (html === lastNavHtml) return;
  lastNavHtml = html;
  root.innerHTML = html;
}

function renderSidebar() {
  updateSidebarStatus(document.getElementById('sidebar-status'), state.data, { t, lang: state.lang, now: Date.now() });
}

// renderPage redraws #page-root only. The terminal panes live in
// #terminal-dock and are only shown or hidden here.
function renderPage() {
  const root = document.getElementById('page-root');
  if (!root) return;
  if (state.active === 'connections') root.innerHTML = renderConnectionsPage(state, ctx);
  else if (state.active === 'commands') root.innerHTML = renderCommands();
  else if (state.active === 'terminal') root.innerHTML = renderTerminalPage(state, t);
  else root.innerHTML = renderSettings();
  setTerminalPageVisible(state.active === 'terminal');
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
  if (state.active === 'settings') return state.dirty.settings;
  if (state.active === 'commands') return state.dirty.approval;
  return false;
}

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
  state.data.mcp = next.mcp ?? {};
  if (!protectSettings) state.settingsMcp = state.data.mcp;
  state.data.storageError = next.storageError ?? '';
  applySessions(next);
  reconcileEditor(state);
  reconcileTerminalTabs(state.data.terminals);
  renderSidebar();
  renderNav();
  if (!pageIsBusy()) renderPage();
  else if (state.active === 'connections') refreshEditorChrome(ctx);
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
  renderSidebar();
  // The tab strip shows live tab states.
  if (state.active === 'terminal') renderPage();
  // The editor's connection badge and disconnect button follow the sessions;
  // the inputs are left alone.
  else if (state.active === 'connections') refreshEditorChrome(ctx);
});

// ------------------------------------------------------------ navigation

async function navigate(page) {
  if (page === state.active) return true;
  if (state.active === 'connections' && !(await confirmLeaveEditor(ctx))) return false;
  if (state.active === 'settings') {
    state.issuedToken = '';
    state.dirty.settings = false;
  }
  if (state.active === 'commands') state.dirty.approval = false;
  state.active = page;
  renderHeader();
  renderNav();
  renderPage();
  return true;
}

// Sidebar row click: jump to that connection's terminal tab if it has one,
// otherwise open it in the editor.
async function showConnection(connId) {
  if (activateConnection(connId)) {
    await navigate('terminal');
    return;
  }
  if (await navigate('connections')) await selectConnection(connId, ctx);
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

// The pending request the on-screen approval form belongs to, or null when the
// form is gone, was drawn for another request than the selection, or the
// request is no longer pending. Approve / reject act only on this one, so an
// edited command can never run under another request's id.
function approvalTarget() {
  const formId = document.querySelector('#approval-command')?.dataset.requestId ?? '';
  const selected = selectedCommandRequest();
  if (!formId || !selected || selected.id !== formId || selected.status !== 'pending') return null;
  return selected;
}

// Called when the approval form no longer matches its request: redraw it so
// the user sees what is pending now.
function refuseStaleApproval() {
  state.dirty.approval = false;
  showToast(t('cmd.staleSelection'));
  renderPage();
}

// Selection is tracked by request ID, not by list index: the poll prepends new
// (auto-executed) entries to state.data.requests, so indexes shift under the user.
function selectedCommandRequest() {
  const requests = state.data.requests ?? [];
  return requests.find((request) => request.id === state.selectedRequestId)
    ?? requests.find((request) => request.status === 'pending')
    ?? null;
}

function renderCommands() {
  const requests = state.data.requests ?? [];
  const queueRequests = requests.filter((request) => request.status === 'pending');
  const selected = selectedCommandRequest();
  const selectedId = selected?.id ?? '';
  // Pin the request whose approval form is drawn: a poll that prepends a new
  // pending request must not move the fallback selection under an edit.
  if (selected?.status === 'pending') state.selectedRequestId = selected.id;
  return `
    <div class="row g-3">
      <div class="col-12 col-xl-4">
        <div class="card card-outline card-warning h-100">
          <div class="card-header">
            <div class="d-flex align-items-center justify-content-between">
              <h3 class="card-title fw-bold">${escapeHtml(t('cmd.queueTitle'))}</h3>
              <span class="badge text-bg-warning">${queueRequests.length}${escapeHtml(t('cmd.queueWaiting'))}</span>
            </div>
          </div>
          <div class="card-body p-0">
            <div class="list-group list-group-flush ssh-list">
              ${queueRequests.length ? queueRequests.map((request) => requestRow(request, selectedId)).join('') : emptyBlock(t('cmd.queueEmpty'))}
            </div>
          </div>
        </div>
      </div>
      <div class="col-12 col-xl-8">
        ${selected ? commandDetail(selected) : emptyCommandDetail()}
      </div>
      <div class="col-12">
        <div class="card">
          <div class="card-header">
            <h3 class="card-title fw-bold">${escapeHtml(t('cmd.historyTitle'))}</h3>
          </div>
          <div class="card-body p-0">
            ${requests.length ? historyTable(requests, selectedId) : emptyBlock(t('cmd.historyEmpty'))}
          </div>
        </div>
      </div>
    </div>
  `;
}

function requestRow(request, selectedId) {
  return `
    <button class="list-group-item list-group-item-action ${request.id === selectedId ? 'active' : ''}" type="button" data-select-request="${escapeHtml(request.id)}">
      <div class="d-flex align-items-start justify-content-between gap-2">
        <div>
          <div class="fw-bold">${escapeHtml(request.id)}</div>
          <small>${escapeHtml(request.host || '-')} / ${escapeHtml(request.requestedBy || t('cmd.unknownAgent'))} / ${escapeHtml(formatTimestamp(request.requestedAt))}</small>
        </div>
        <span class="badge ${requestStatusClass(request.status)}">${escapeHtml(requestStatusLabel(request.status))}</span>
      </div>
      <code class="d-block mt-2 small text-truncate">${escapeHtml(request.command)}</code>
    </button>
  `;
}

function commandDetail(selected) {
  const bypassEnabled = agentBypassEnabled(selected.requestedBy);
  const pending = selected.status === 'pending';
  const bypassAllowed = Boolean(state.data.mcp?.bypassAllowed);
  const executed = selected.executedCommand || '';
  const edited = executed && executed !== selected.command;
  return `
    <div class="card card-outline ${pending ? 'card-warning' : 'card-secondary'} h-100">
      <div class="card-header">
        <div class="d-flex align-items-start justify-content-between gap-3">
          <div>
            <h3 class="card-title fw-bold mb-1">${escapeHtml(pending ? t('cmd.approveTitle') : t('cmd.detailTitle'))}</h3>
            <div class="text-secondary small">${escapeHtml(pending ? t('cmd.approveSubtitle') : t('cmd.detailSubtitle'))}</div>
          </div>
          <span class="badge ${requestStatusClass(selected.status)}">${escapeHtml(requestStatusLabel(selected.status))}</span>
        </div>
      </div>
      <div class="card-body">
        <div class="command-agent mb-3">
          <span>${escapeHtml(t('cmd.agent'))}</span>
          <strong>${escapeHtml(selected.requestedBy || t('cmd.unknownAgent'))}</strong>
          ${bypassEnabled ? `<em>${escapeHtml(t('cmd.bypassing'))}</em> <button class="btn btn-link btn-sm p-0 ms-2" type="button" data-action="revoke-bypass" data-agent-name="${escapeHtml(selected.requestedBy)}">${escapeHtml(t('cmd.revokeBypass'))}</button>` : ''}
        </div>
        <div class="small text-secondary mb-3">${escapeHtml(selected.host || '-')} · ${escapeHtml(formatTimestamp(selected.requestedAt))} · risk ${escapeHtml(selected.risk)}${selected.decidedAt ? ` · ${escapeHtml(t('cmd.decided'))} ${escapeHtml(formatTimestamp(selected.decidedAt))} (${escapeHtml(selected.decidedBy)})` : ''}${selected.exitCode !== null && selected.exitCode !== undefined ? ` · ${escapeHtml(t('cmd.exitCode'))} ${escapeHtml(selected.exitCode)}` : ''}</div>

        <div class="mb-3">
          <div class="d-flex justify-content-between align-items-center mb-2">
            <h5 class="fw-bold mb-0">${escapeHtml(t('cmd.plannedCommand'))}</h5>
            <button class="btn btn-outline-secondary btn-sm" type="button" data-action="copy-command">${escapeHtml(t('common.copy'))}</button>
          </div>
          ${pending
            ? `<textarea class="form-control font-monospace" id="approval-command" data-request-id="${escapeHtml(selected.id)}" rows="3" spellcheck="false" autocapitalize="none" autocorrect="off">${escapeHtml(selected.command)}</textarea>
               <div class="form-text">${escapeHtml(t('cmd.editHint'))}</div>`
            : `<pre class="command-block"><code>${escapeHtml(executed || selected.command)}</code></pre>
               ${edited ? `<div class="small text-warning-emphasis mt-1">${escapeHtml(t('cmd.edited'))}: <code>${escapeHtml(selected.command)}</code></div>` : ''}`}
        </div>

        <div class="mb-3">
          <h5 class="fw-bold">${escapeHtml(t('cmd.reason'))}</h5>
          <div class="reason-box">${escapeHtml(selected.reason || t('cmd.reasonEmpty'))}</div>
        </div>

        <div class="row g-3">
          ${terminalPanel('stdout', selected.stdout || (pending ? t('cmd.stdoutPlaceholder') : t('cmd.stdoutEmpty')))}
          ${terminalPanel('stderr', selected.stderr || t('cmd.stderrPlaceholder'), true)}
        </div>
      </div>
      <div class="card-footer d-flex gap-2">
        <button class="btn btn-outline-danger" type="button" data-action="delete-command-request" data-command-request-id="${escapeHtml(selected.id)}">
          <i class="bi bi-trash me-1"></i>${escapeHtml(t('cmd.deleteHistory'))}
        </button>
        ${pending ? `
        <button class="btn btn-danger" type="button" data-action="reject-command">${escapeHtml(t('cmd.reject'))}</button>
        <span title="${bypassAllowed ? '' : escapeHtml(t('cmd.bypassNeedsBearer'))}">
          <button class="btn btn-outline-primary" type="button" data-action="approve-bypass-agent" ${bypassAllowed ? '' : 'disabled'}>
            ${escapeHtml(t('cmd.approveBypass'))}
          </button>
        </span>
        <button class="btn btn-primary ms-auto" type="button" data-action="approve-command">${escapeHtml(t('cmd.approve'))}</button>
        ` : ''}
      </div>
    </div>
  `;
}

function agentBypassEnabled(agentName) {
  const name = String(agentName ?? '').trim();
  return Boolean(name && (state.data.agentPolicies ?? []).some((policy) => policy.agentName === name && policy.approvalBypass));
}

function emptyCommandDetail() {
  return `
    <div class="card h-100">
      <div class="card-body empty-state">
        <i class="bi bi-terminal fs-1 text-secondary"></i>
        <h4>${escapeHtml(t('cmd.emptyTitle'))}</h4>
        <p>${escapeHtml(t('cmd.emptyDesc'))}</p>
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

function terminalPanel(title, content, error = false) {
  return `
    <div class="col-12 col-lg-6">
      <div class="terminal-panel ${error ? 'terminal-error' : ''}">
        <div class="terminal-title">${title}</div>
        <pre>${escapeHtml(content)}</pre>
      </div>
    </div>
  `;
}

function historyTable(requests, selectedId) {
  return `
    <div class="table-responsive">
      <table class="table table-hover align-middle mb-0">
        <thead>
          <tr>
            <th>${escapeHtml(t('hist.time'))}</th>
            <th>${escapeHtml(t('hist.agent'))}</th>
            <th>${escapeHtml(t('hist.status'))}</th>
            <th>${escapeHtml(t('hist.command'))}</th>
            <th>${escapeHtml(t('hist.reason'))}</th>
            <th class="text-end">${escapeHtml(t('hist.action'))}</th>
          </tr>
        </thead>
        <tbody>
          ${requests.map((request) => `
            <tr class="${request.id === selectedId ? 'table-active' : ''}" data-select-request="${escapeHtml(request.id)}" style="cursor:pointer">
              <td class="text-nowrap">${escapeHtml(formatTimestamp(request.requestedAt))}</td>
              <td>${escapeHtml(request.host || '-')}<br><small class="text-secondary">${escapeHtml(request.requestedBy || t('cmd.unknownAgent'))}</small></td>
              <td><span class="badge ${requestStatusClass(request.status)}">${escapeHtml(requestStatusLabel(request.status))}</span></td>
              <td><code>${escapeHtml(request.executedCommand || request.command)}</code></td>
              <td>${escapeHtml(request.reason || '-')}</td>
              <td class="text-end">
                <button class="btn btn-outline-danger btn-sm" type="button" data-action="delete-command-request" data-command-request-id="${escapeHtml(request.id)}" title="${escapeHtml(t('cmd.deleteHistory'))}">
                  <i class="bi bi-trash"></i>
                </button>
              </td>
            </tr>
          `).join('')}
        </tbody>
      </table>
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

function emptyBlock(message) {
  return `<div class="p-4 text-center text-secondary">${escapeHtml(message)}</div>`;
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

async function handleCommonClick(event) {
  const actionElement = event.target.closest('[data-action]');
  const action = actionElement?.dataset.action;

  if (action) {
    // Must resolve the same request the detail pane renders (selectedCommandRequest),
    // never by list index: history entries prepend as agents run and indexes shift.
    const selectedRequest = selectedCommandRequest();

    if (action === 'delete-command-request') {
      const requestId = actionElement?.dataset.commandRequestId || selectedRequest?.id || '';
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

    if (action === 'copy-command') {
      await copyText(selectedRequest?.command ?? '');
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

    if (action === 'approve-command') {
      if (!selectedRequest) return;
      if (approvalTarget() !== selectedRequest) {
        refuseStaleApproval();
        return;
      }
      state.dirty.approval = false;
      const command = document.querySelector('#approval-command')?.value ?? selectedRequest.command ?? '';
      actionElement.disabled = true;
      ctx.bumpGen();
      let result = await ApproveCommand(selectedRequest.id ?? '', command);
      if (!result.ok && result.code === 'risk_increased' && window.confirm(`${result.message}\n\n${t('cmd.riskConfirm')}`)) {
        result = await ApproveCommandConfirmed(selectedRequest.id ?? '', command);
      }
      // Keep the approved request selected so its stdout/stderr shows in the pane.
      state.selectedRequestId = selectedRequest.id ?? '';
      showToast(result.message);
      await refreshData({ silent: true });
      return;
    }

    if (action === 'approve-bypass-agent') {
      if (!selectedRequest) return;
      if (approvalTarget() !== selectedRequest) {
        refuseStaleApproval();
        return;
      }
      state.dirty.approval = false;
      const command = document.querySelector('#approval-command')?.value ?? selectedRequest.command ?? '';
      actionElement.disabled = true;
      ctx.bumpGen();
      const result = await ApproveCommandAndBypassAgent(selectedRequest.id ?? '', command);
      state.selectedRequestId = selectedRequest.id ?? '';
      showToast(result.message);
      await refreshData({ silent: true });
      return;
    }

    if (action === 'revoke-bypass') {
      ctx.bumpGen();
      const result = await RevokeAgentApprovalBypass(actionElement.dataset.agentName ?? '');
      showToast(result.message);
      await refreshData({ silent: true });
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

    if (action === 'reject-command') {
      if (!selectedRequest) return;
      if (approvalTarget() !== selectedRequest) {
        refuseStaleApproval();
        return;
      }
      state.dirty.approval = false;
      ctx.bumpGen();
      const result = await RejectCommand(selectedRequest.id ?? '');
      showToast(result.message);
      await refreshData({ silent: true });
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
      return;
    }
  }

  const request = event.target.closest('[data-select-request]');
  if (request) {
    state.selectedRequestId = request.dataset.selectRequest;
    state.dirty.approval = false;
    renderPage();
  }
}

app.addEventListener('click', async (event) => {
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

  await handleCommonClick(event);
});

app.addEventListener('submit', async (event) => {
  event.preventDefault();
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
  if (event.target.id === 'approval-command') state.dirty.approval = true;
  if (event.target.closest('#mcp-form')) state.dirty.settings = true;
});

app.addEventListener('change', async (event) => {
  if (state.active === 'connections' && await handleEditorChange(event, ctx)) return;
  if (state.active === 'terminal' && handleTerminalChange(event, ctx)) return;
  if (event.target.closest('#mcp-form')) state.dirty.settings = true;
});

// ------------------------------------------------------------- startup

async function start() {
  app.innerHTML = renderSkeleton({ t, logoUrl });
  initTerminalTabs(ctx);
  renderAll();
  window.setInterval(pollData, 5000);
  // On failure the error stays in #page-root until a later poll succeeds
  // (that poll also restores the terminal tabs).
  if (!(await refreshData())) return;
  renderAll();
}

start();
