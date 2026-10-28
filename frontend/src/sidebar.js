// Sidebar status block (#sidebar-status), design-v1 §6.1–6.3.
//
// This module only ever writes inside #sidebar-status. It never touches
// #page-root or #terminal-dock, so the 5s poll and sessions:changed can redraw
// it freely while xterm instances stay mounted.

import { renderSidebarAISessions } from './aisessions.js';
import {
  connStateLabelKey,
  connStateSymbol,
  escapeHtml,
  formatElapsed,
  summarizeSessions,
} from './model.js';

export const SIDEBAR_I18N = {
  ja: {
    'side.lang': '言語',
    'side.sessionsTitle': 'SSHセッション',
    'side.summary': '接続中 {connected} / 登録 {saved}',
    'side.errors': 'エラー {n}',
    'side.errorsOne': 'エラー {n}',
    'side.shells': 'シェル{n}',
    'side.shellsOne': 'シェル{n}',
    'side.running': '実行{n}',
    'side.ariaRunning': '実行中{n}件',
    'side.more': '他 {n} 件',
    'side.empty': '接続中のセッションはありません',
    'side.agentsTitle': 'エージェント（MCP）',
    'side.mcp.listening': '待受中',
    'side.mcp.stopped': '停止中',
    'side.mcp.error': 'エラー',
    'side.activeAgents': 'アクティブなエージェント {n}（直近5分）',
    'side.pending': '承認待ち {n}',
    'side.storageError': '保存先エラー：設定を保存できません',
  },
  en: {
    'side.lang': 'Language',
    'side.sessionsTitle': 'SSH sessions',
    'side.summary': '{connected} connected / {saved} saved',
    'side.errors': '{n} errors',
    'side.errorsOne': '{n} error',
    'side.shells': '{n} shells',
    'side.shellsOne': '{n} shell',
    'side.running': '{n} running',
    'side.ariaRunning': '{n} running',
    'side.more': '+{n} more',
    'side.empty': 'No active sessions',
    'side.agentsTitle': 'Agents (MCP)',
    'side.mcp.listening': 'Listening',
    'side.mcp.stopped': 'Stopped',
    'side.mcp.error': 'Error',
    'side.activeAgents': 'Active agents {n} (last 5 min)',
    'side.pending': 'Awaiting approval {n}',
    'side.storageError': 'Storage error: changes cannot be saved',
  },
};

const SESSION_ROW_LIMIT = 5;

// ctx.lang is a function while main.js may also pass the plain code; accept both.
function langCode(lang) {
  const value = typeof lang === 'function' ? lang() : lang;
  return value === 'en' ? 'en' : 'ja';
}

// EN needs a singular form for "1 shell" / "1 error"; JA uses the same text.
function countLabel(t, key, n) {
  return t(n === 1 ? `${key}One` : key, { n });
}

function kindIcon(kind) {
  return kind === 'serial' ? 'bi-usb-symbol' : 'bi-hdd-network';
}

function rowDisplayName(row) {
  return row.kind === 'serial' && row.label ? row.label : row.name;
}

// Connected and in-use rows show elapsed time and counts like the wireframe;
// every other state shows its label as text so the state never relies on the
// symbol or color alone.
function rowShowsStateText(connState) {
  return connState !== 'connected' && connState !== 'in_use';
}

function rowMetaParts(row, t, lang) {
  const parts = [];
  if (rowShowsStateText(row.connState)) parts.push(t(connStateLabelKey(row.connState)));
  if (row.elapsedMs != null) parts.push(formatElapsed(row.elapsedMs, lang));
  const counts = [];
  if (row.shells > 0) counts.push(countLabel(t, 'side.shells', row.shells));
  if (row.running > 0) counts.push(t('side.running', { n: row.running }));
  if (counts.length) parts.push(counts.join(' · '));
  return parts;
}

// sessionRowAriaLabel reads "web-01、接続中、12分、シェル2、実行中1件" /
// "web-01, Connected, 12m, 2 shells, 1 running". Zero counts are omitted.
export function sessionRowAriaLabel(row, t, lang) {
  const code = langCode(lang);
  const parts = [row.name];
  if (row.kind === 'serial' && row.label && row.label !== row.name) parts.push(row.label);
  parts.push(t(connStateLabelKey(row.connState)));
  if (row.elapsedMs != null) parts.push(formatElapsed(row.elapsedMs, code));
  if (row.shells > 0) parts.push(countLabel(t, 'side.shells', row.shells));
  if (row.running > 0) parts.push(t('side.ariaRunning', { n: row.running }));
  return parts.join(code === 'en' ? ', ' : '、');
}

function renderSessionRow(row, t, lang) {
  const classes = ['side-session-row'];
  if (row.connState === 'error') classes.push('is-error');
  if (row.kind === 'serial') classes.push('is-serial');
  const name = rowDisplayName(row);
  const meta = rowMetaParts(row, t, lang).join(lang === 'en' ? ' · ' : '  ');
  const stateLabel = t(connStateLabelKey(row.connState));
  return `<button type="button" class="${classes.join(' ')}" data-action="side-open-conn" data-conn-id="${escapeHtml(row.connId)}" aria-label="${escapeHtml(sessionRowAriaLabel(row, t, lang))}" title="${escapeHtml(`${row.name} — ${stateLabel}`)}">
  <span class="side-symbol" aria-hidden="true">${escapeHtml(row.symbol || connStateSymbol(row.connState))}</span>
  <i class="bi ${kindIcon(row.kind)}" aria-hidden="true"></i>
  <span class="side-name">${escapeHtml(name)}</span>
  ${meta ? `<span class="side-meta">${escapeHtml(meta)}</span>` : ''}
</button>`;
}

function renderSessionsSection(data, t, lang, now) {
  const view = summarizeSessions(data?.sessions || [], { now, limit: SESSION_ROW_LIMIT });
  const rows = view.rows.length
    ? view.rows.map((row) => renderSessionRow(row, t, lang)).join('')
    : `<div class="side-empty">${escapeHtml(t('side.empty'))}</div>`;
  const more = view.more > 0
    ? `<button type="button" class="side-more" data-action="side-more">${escapeHtml(t('side.more', { n: view.more }))} <span aria-hidden="true">→</span></button>`
    : '';
  const errors = view.errors > 0
    ? `<span class="side-summary-error">${escapeHtml(countLabel(t, 'side.errors', view.errors))}</span>`
    : '';
  return `<section class="side-section side-sessions">
  <div class="side-section-title">${escapeHtml(t('side.sessionsTitle'))}</div>
  <div class="side-summary"><span>${escapeHtml(t('side.summary', { connected: view.connected, saved: view.saved }))}</span>${errors}</div>
  ${rows}
  ${more}
</section>`;
}

// mcp.status is still the backend's Japanese text (起動中/停止中/エラー); the
// codes are accepted too so this keeps working once the backend switches.
function mcpStatusCode(status) {
  if (status === '起動中' || status === 'running' || status === 'listening') return 'listening';
  if (status === 'エラー' || status === 'error') return 'error';
  return 'stopped';
}

const MCP_SYMBOLS = { listening: '●', stopped: '○', error: '▲' };

function mcpAddress(mcp) {
  const host = String(mcp?.listenAddress || '');
  const port = mcp?.port;
  if (!host || !port) return '';
  return host.includes(':') && !host.startsWith('[') ? `[${host}]:${port}` : `${host}:${port}`;
}

function pendingCount(requests) {
  return (requests || []).filter((request) => request?.status === 'pending').length;
}

function renderMCPSection(data, t) {
  const mcp = data?.mcp || {};
  const code = mcpStatusCode(mcp.status);
  const address = code === 'listening' ? mcpAddress(mcp) : '';
  const title = code === 'error' && mcp.lastError ? ` title="${escapeHtml(mcp.lastError)}"` : '';
  const pending = pendingCount(data?.requests);
  return `<section class="side-section side-agents">
  <div class="side-section-title">${escapeHtml(t('side.agentsTitle'))}</div>
  <div class="side-mcp-line is-${code}"${title}>MCP <span class="side-symbol" aria-hidden="true">${MCP_SYMBOLS[code]}</span> <span>${escapeHtml(t(`side.mcp.${code}`))}</span>${address ? ` <span class="side-meta">${escapeHtml(address)}</span>` : ''}</div>
  <div class="side-mcp-line">${escapeHtml(t('side.activeAgents', { n: Number(mcp.connectedAgents) || 0 }))}</div>
  ${renderSidebarAISessions(data?.aiSessions, t)}
  <button type="button" class="side-more side-pending" data-action="side-approvals">${escapeHtml(t('side.pending', { n: pending }))} <span aria-hidden="true">→</span></button>
</section>`;
}

function renderStorageError(data, t) {
  if (!data?.storageError) return '';
  return `<div class="side-storage-error" role="alert" title="${escapeHtml(data.storageError)}"><i class="bi bi-exclamation-triangle-fill" aria-hidden="true"></i> ${escapeHtml(t('side.storageError'))}</div>`;
}

function renderLangToggle(t, lang) {
  const button = (code, label) =>
    `<button class="btn ${lang === code ? 'btn-primary' : 'btn-outline-secondary'}" type="button" data-lang="${code}" aria-pressed="${lang === code}">${label}</button>`;
  return `<div class="d-flex align-items-center justify-content-between mb-2">
  <span class="status-heading mb-0">${escapeHtml(t('side.lang'))}</span>
  <div class="btn-group btn-group-sm" role="group" aria-label="${escapeHtml(t('side.lang'))}">${button('ja', 'JA')}${button('en', 'EN')}</div>
</div>`;
}

// renderSidebarStatus returns the innerHTML of #sidebar-status. data is
// state.data; lang may be the code or ctx.lang.
export function renderSidebarStatus(data, { t, lang, now = Date.now() } = {}) {
  const code = langCode(lang);
  return [
    renderLangToggle(t, code),
    renderSessionsSection(data, t, code, now),
    renderMCPSection(data, t),
    renderStorageError(data, t),
    '<div class="company-name">QuickIterate Co., Ltd.</div>',
  ].filter(Boolean).join('\n');
}

// Last HTML written per root, so an unchanged poll does no DOM work at all.
const lastHtml = new WeakMap();

function syncAttributes(from, to) {
  for (const { name } of [...to.attributes]) {
    if (!from.hasAttribute(name)) to.removeAttribute(name);
  }
  for (const { name, value } of [...from.attributes]) {
    if (to.getAttribute(name) !== value) to.setAttribute(name, value);
  }
}

// A row whose connection changed is a different control; replace it instead of
// patching so focus does not silently move to another connection.
function sameIdentity(a, b) {
  return a.nodeType === b.nodeType
    && a.nodeName === b.nodeName
    && (a.nodeType !== 1 || a.getAttribute('data-conn-id') === b.getAttribute('data-conn-id'));
}

function morphChildren(target, source) {
  const next = [...source.childNodes];
  for (let i = 0; i < next.length; i++) {
    const current = target.childNodes[i];
    const incoming = next[i];
    if (!current) {
      target.appendChild(incoming);
    } else if (current.isEqualNode(incoming)) {
      continue;
    } else if (!sameIdentity(current, incoming)) {
      target.replaceChild(incoming, current);
    } else if (current.nodeType === 1) {
      syncAttributes(incoming, current);
      morphChildren(current, incoming);
    } else {
      current.nodeValue = incoming.nodeValue;
    }
  }
  while (target.childNodes.length > next.length) target.removeChild(target.lastChild);
}

// updateSidebarStatus renders into root (#sidebar-status) and patches only the
// nodes that changed, so polling keeps focus and hover on unchanged rows.
// Returns true when the DOM was touched.
export function updateSidebarStatus(root, data, options) {
  if (!root) return false;
  const html = renderSidebarStatus(data, options);
  if (lastHtml.get(root) === html) return false;
  lastHtml.set(root, html);
  const template = root.ownerDocument.createElement('template');
  template.innerHTML = html;
  morphChildren(root, template.content);
  return true;
}

// handleSidebarClick handles side-* actions inside #sidebar-status. The
// data-lang buttons are left to main.js.
export async function handleSidebarClick(event, ctx) {
  const target = event?.target?.closest?.('[data-action^="side-"]');
  if (!target) return false;
  const action = target.dataset.action;
  if (action === 'side-open-conn') {
    const connId = target.dataset.connId;
    if (connId) await ctx.showConnection(connId);
    return true;
  }
  if (action === 'side-more') {
    await ctx.navigate('connections');
    return true;
  }
  if (action === 'side-approvals') {
    await ctx.navigate('commands');
    return true;
  }
  if (action === 'side-aisessions') {
    await ctx.navigate('aisessions');
    return true;
  }
  return false;
}
