// Approval queue, approval detail and command history (the 'commands' page),
// split out of main.js. Phase 3 adds the resolved execution target
// (GetApprovalTarget), the per-code handling of approval results and the
// approval-bypass confirmation; phase 4 adds the serverFilter view.
//
// Contract with main.js (phase2-contract §2.1/§2.2):
// - Only model.js is imported; everything else comes through ctx.
// - state.selectedRequestId and state.dirty.approval keep their phase 2
//   meaning. state.approvals (createApprovalsState) is owned by this module.
// - `view` is deriveServerView(...)'s result (phase4-ui-integration §2.1):
//   { filter, queue, history, selectedRequest, hiddenPending, unknownIds }.
//   Without it the page shows every request, as in phase 2.
// - Request-derived strings are put in with textContent / value only; the
//   page is built with DOM nodes, never with innerHTML.
// - The audit #22/#40 rule is kept: approve / bypass / reject act only on the
//   request the on-screen form was drawn for, and refuse with
//   cmd.staleSelection otherwise.

import {
  ApproveCommand,
  ApproveCommandAndBypassAgent,
  ApproveCommandConfirmed,
  GetApprovalTarget,
  RejectCommand,
  RevokeAgentApprovalBypass,
} from '../wailsjs/go/main/App';

import { requestStateLabelKey } from './model.js';

// Strings already in BASE_I18N (cmd.*, hist.*, common.*) are reused as they
// are; everything new is under `appr.`.
export const APPROVALS_I18N = {
  ja: {
    'appr.target.title': '実行先',
    'appr.target.loading': '実行先を確認しています…',
    'appr.target.error': '実行先を確認できませんでした。承認するとバックエンドが改めて確認します',
    'appr.target.name': '接続先',
    'appr.target.resolvedBy': '照合方法',
    'appr.target.resolvedBy.id': '接続先ID（要求時に記録）',
    'appr.target.resolvedBy.name': '接続先名（IDの無い古い要求）',
    'appr.target.requested': '要求時の実行先',
    'appr.target.current': '現在の実行先',
    'appr.target.noFp': '記録なし',
    'appr.target.hostKey': 'ホスト鍵の指紋',
    'appr.target.hostKeyUnknown': '未登録（GUIから初めて接続したときに記録されます）',
    'appr.target.notFound': '実行先が接続先一覧にありません（削除された可能性があります）。承認しても実行されず、失敗として記録されます',
    'appr.target.notExecutable': 'シリアルの接続先ではコマンドを実行できません。拒否してください',
    'appr.target.changed': '要求後に実行先が変わりました（{from} → {to}）。この要求は承認できません',
    'appr.target.changedAction': '拒否してエージェントに再依頼させてください',
    'appr.blocked.changed': '実行先が変わったため承認できません',
    'appr.blocked.notExecutable': 'シリアルの接続先では実行できません',
    'appr.unknownConnection': '不明な接続先',
    'appr.hiddenPending': 'ほかの接続先に承認待ちが {n} 件あります',
    'appr.showAll': 'すべて表示',
    'appr.queueEmptyFiltered': '{name} の承認待ちはありません',
    'appr.historyEmptyFiltered': '{name} の履歴はありません',
    'appr.code.not_pending': 'この要求は既に処理されています。表示を更新しました',
    'appr.code.expired': '承認期限を過ぎたため、この要求は失効しました',
    'appr.code.risk_increased': '修正したコマンドのリスクが要求時より上がりました',
    'appr.code.bearer_required': '承認省略には、MCP設定で発行するBearerトークンかエージェント専用トークンが必要です。要求は承認待ちのままです',
    'appr.code.target_changed': '要求後に実行先が変わったため承認できませんでした。拒否してエージェントに再依頼させてください',
    'appr.code.shutting_down': '終了処理中のため承認できません',
    'appr.code.serial_not_supported': 'シリアルの接続先では承認・実行できません。この要求は拒否してください',
    'appr.riskCanceled': '承認を取りやめました。修正したコマンドはそのまま残しています',
    'appr.bypassRiskIncreased': '修正でリスクが上がったため、承認省略つきでは承認できません。修正したコマンドは残しています。内容を確認して「承認して実行」を使ってください',
    'appr.bypass.needsAgent': 'エージェント名の無い要求には承認省略を使えません',
    'appr.bypass.confirm': '{agent} の承認省略を有効にして、このコマンドを承認・実行します。\n\n・有効期間: 8時間（過ぎると自動で無効になります）\n・対象: リスク Medium まで（High は常に承認キューに入ります）\n・{agent} が専用のエージェントトークンで MCP を呼び出したときだけ効きます（名乗りや共有トークンでは効きません）\n・設定の承認必須ルール（sudo・本番・書き込み）に当たるコマンドは、引き続き承認が必要です\n・この画面の「承認省略を取り消す」でいつでも取り消せます',
    'appr.bypass.noTokenYet': '{agent} 専用のエージェントトークンはまだありません。MCP設定で発行するまで承認省略は効きません',
    'appr.bypass.ask': 'よろしいですか？',
    'appr.bypass.until': '{time} まで',
    'appr.bypass.inactive': '専用トークンが無いため無効',
  },
  en: {
    'appr.target.title': 'Execution target',
    'appr.target.loading': 'Checking the execution target…',
    'appr.target.error': 'Could not check the execution target. The backend checks it again on approval',
    'appr.target.name': 'Connection',
    'appr.target.resolvedBy': 'Matched by',
    'appr.target.resolvedBy.id': 'Connection ID (recorded with the request)',
    'appr.target.resolvedBy.name': 'Connection name (older request without an ID)',
    'appr.target.requested': 'Target when requested',
    'appr.target.current': 'Target now',
    'appr.target.noFp': 'Not recorded',
    'appr.target.hostKey': 'Host key fingerprint',
    'appr.target.hostKeyUnknown': 'Not trusted yet (recorded the first time you connect from the GUI)',
    'appr.target.notFound': 'The target is not in the connection list (it may have been deleted). Approving will not run it; it is recorded as failed',
    'appr.target.notExecutable': 'Commands cannot run on a serial connection. Reject this request',
    'appr.target.changed': 'The target changed after the request ({from} → {to}). This request cannot be approved',
    'appr.target.changedAction': 'Reject it and have the agent ask again',
    'appr.blocked.changed': 'Cannot approve: the target changed',
    'appr.blocked.notExecutable': 'Cannot run on a serial connection',
    'appr.unknownConnection': 'Unknown connection',
    'appr.hiddenPending': '{n} more awaiting approval on other connections',
    'appr.showAll': 'Show all',
    'appr.queueEmptyFiltered': 'Nothing awaiting approval on {name}',
    'appr.historyEmptyFiltered': 'No history for {name}',
    'appr.code.not_pending': 'This request was already handled. The view was refreshed',
    'appr.code.expired': 'This request expired before it was approved',
    'appr.code.risk_increased': 'The edited command has a higher risk than the request',
    'appr.code.bearer_required': 'Approval bypass needs a Bearer token or an agent token (issue one in MCP settings). The request is still pending',
    'appr.code.target_changed': 'Not approved: the target changed after the request. Reject it and have the agent ask again',
    'appr.code.shutting_down': 'Cannot approve while the app is shutting down',
    'appr.code.serial_not_supported': 'Commands cannot be approved or run on a serial connection. Reject this request',
    'appr.riskCanceled': 'Approval canceled. Your edited command is kept',
    'appr.bypassRiskIncreased': 'The edit raised the risk, so it cannot be approved with bypass. Your edit is kept; review it and use “Approve & Execute”',
    'appr.bypass.needsAgent': 'Approval bypass needs a request with an agent name',
    'appr.bypass.confirm': 'Turn on approval bypass for {agent}, and approve and run this command.\n\n- Lasts 8 hours (then turns off by itself)\n- Covers risk up to Medium (High always goes to the approval queue)\n- Works only when {agent} calls MCP with its own agent token (not a declared name or the shared token)\n- Commands matched by the approval rules in settings (sudo, production, write) still need approval\n- You can revoke it any time with “Revoke approval bypass” on this screen',
    'appr.bypass.noTokenYet': '{agent} has no agent token yet. The bypass does nothing until you issue one in MCP settings',
    'appr.bypass.ask': 'Continue?',
    'appr.bypass.until': 'until {time}',
    'appr.bypass.inactive': 'inactive: no agent token',
  },
};

// The ActionResult codes approve can return (approval.go).
export const APPROVAL_CODES = Object.freeze(['not_pending', 'expired', 'risk_increased', 'bearer_required', 'target_changed', 'shutting_down', 'serial_not_supported']);

export function createApprovalsState() {
  return {
    // Last GetApprovalTarget result that was applied ({ requestId, ... }).
    target: null,
    // Bumped per lookup; only the newest lookup is applied.
    seq: 0,
  };
}

function approvalsState(state) {
  if (!state.approvals) state.approvals = createApprovalsState();
  return state.approvals;
}

// ------------------------------------------------------------ pure helpers

function text(value) {
  return String(value ?? '').trim();
}

export function formatTimestamp(value) {
  if (!value) return '-';
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return String(value);
  const pad = (n) => String(n).padStart(2, '0');
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())} ${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}`;
}

export function requestStatusLabel(status, t) {
  const key = requestStateLabelKey(status);
  const label = t(key);
  return label === key ? String(status ?? '') : label;
}

export function requestStatusClass(status) {
  if (status === 'pending') return 'text-bg-warning';
  if (status === 'running' || status === 'auto_running') return 'text-bg-info';
  if (status === 'succeeded' || status === 'auto_succeeded') return 'text-bg-success';
  if (status === 'failed' || status === 'auto_failed' || status === 'interrupted') return 'text-bg-danger';
  return 'text-bg-secondary';
}

// commandsView turns deriveServerView's result (or nothing) into what the page
// draws. Selection is by request ID, never by list index: polls prepend new
// entries. With a view, a request outside the filter is never selected, so a
// hidden request can never be approved.
export function commandsView(state, view) {
  const requests = state.data?.requests ?? [];
  if (view) {
    const filter = text(view.filter);
    return {
      filter,
      filterName: filter ? connectionName(state, filter) : '',
      queue: view.queue ?? [],
      history: view.history ?? [],
      selectedRequest: view.selectedRequest ?? null,
      hiddenPending: Math.max(0, Number(view.hiddenPending ?? 0)),
      unknownIds: view.unknownIds instanceof Set ? view.unknownIds : new Set(view.unknownIds ?? []),
    };
  }
  const queue = requests.filter((request) => request.status === 'pending');
  return {
    filter: '',
    filterName: '',
    queue,
    history: requests,
    selectedRequest: requests.find((request) => request.id === state.selectedRequestId) ?? queue[0] ?? null,
    hiddenPending: 0,
    unknownIds: new Set(),
  };
}

function connectionName(state, connId) {
  const conn = (state.data?.connections ?? []).find((c) => c.id === connId);
  if (conn?.name) return conn.name;
  const session = (state.data?.sessions ?? []).find((s) => s.connId === connId);
  return session?.connName || connId;
}

export function selectedCommandRequest(state, view) {
  return commandsView(state, view).selectedRequest;
}

export function agentPolicy(state, agentName) {
  const name = text(agentName);
  if (!name) return null;
  return (state.data?.agentPolicies ?? []).find((policy) => policy.agentName === name) ?? null;
}

export function agentBypassEnabled(state, agentName) {
  return Boolean(agentPolicy(state, agentName)?.approvalBypass);
}

export function agentHasToken(state, agentName) {
  const name = text(agentName);
  if (!name) return false;
  if ((state.data?.agentTokens ?? []).some((token) => token.agentName === name)) return true;
  return Boolean(agentPolicy(state, name)?.hasToken);
}

// bypassAvailability mirrors the backend precondition (approval.go): an agent
// name, and the shared Bearer token or the agent's own token.
export function bypassAvailability(state, request) {
  if (!text(request?.requestedBy)) return { allowed: false, reasonKey: 'appr.bypass.needsAgent' };
  if (state.data?.mcp?.bypassAllowed || agentHasToken(state, request.requestedBy)) return { allowed: true, reasonKey: '' };
  return { allowed: false, reasonKey: 'cmd.bypassNeedsBearer' };
}

// approvalBlock says whether the target lookup forbids approving. fpChanged
// is refused by the backend too (REQ-011: target_changed); a serial target
// cannot run commands at all. A target that was not found stays approvable
// (phase4-ui-integration §3.3: the backend records it as failed).
export function approvalBlock(target) {
  if (!target) return { blocked: false, reasonKey: '' };
  if (target.fpChanged) return { blocked: true, reasonKey: 'appr.blocked.changed' };
  if (target.found && target.executable === false) return { blocked: true, reasonKey: 'appr.blocked.notExecutable' };
  return { blocked: false, reasonKey: '' };
}

export function resolvedByLabel(resolvedBy, t) {
  if (resolvedBy === 'id') return t('appr.target.resolvedBy.id');
  if (resolvedBy === 'name') return t('appr.target.resolvedBy.name');
  return String(resolvedBy || '-');
}

// approvalResultMessage is the toast for an approve / reject result: the
// localized text for a known failure code, otherwise the backend message.
export function approvalResultMessage(result, t) {
  if (result && !result.ok && APPROVAL_CODES.includes(result.code)) return t(`appr.code.${result.code}`);
  return String(result?.message ?? '');
}

export function riskConfirmText(result, t, lang) {
  // The backend message (JA) carries the "Low → High" detail.
  const head = lang === 'ja' && result?.message ? result.message : t('appr.code.risk_increased');
  return `${head}\n\n${t('cmd.riskConfirm')}`;
}

export function bypassConfirmText(state, request, t) {
  const agent = text(request?.requestedBy);
  const parts = [t('appr.bypass.confirm', { agent })];
  if (!agentHasToken(state, agent)) parts.push(t('appr.bypass.noTokenYet', { agent }));
  parts.push(t('appr.bypass.ask'));
  return parts.join('\n\n');
}

// --------------------------------------------------------------- DOM helpers

function el(tag, attrs = {}, ...children) {
  const node = document.createElement(tag);
  for (const [key, value] of Object.entries(attrs ?? {})) {
    if (value === undefined || value === null || value === false) continue;
    if (key === 'class') node.className = value;
    else if (key === 'dataset') Object.assign(node.dataset, value);
    else if (key === 'text') node.textContent = String(value);
    else if (key === 'value') node.value = String(value);
    else if (key === 'disabled') node.disabled = true;
    else node.setAttribute(key, value === true ? '' : String(value));
  }
  appendChildren(node, children);
  return node;
}

function appendChildren(node, children) {
  for (const child of children.flat(Infinity)) {
    if (child === null || child === undefined || child === false || child === '') continue;
    node.append(child instanceof Node ? child : document.createTextNode(String(child)));
  }
}

function icon(name, extra = '') {
  return el('i', { class: `bi ${name}${extra ? ` ${extra}` : ''}`, 'aria-hidden': 'true' });
}

function statusBadge(status, t) {
  return el('span', { class: `badge ${requestStatusClass(status)}`, text: requestStatusLabel(status, t) });
}

function unknownBadge(t) {
  return el('span', { class: 'badge text-bg-secondary unknown-conn me-1', text: t('appr.unknownConnection') });
}

function emptyBlock(message) {
  return el('div', { class: 'p-4 text-center text-secondary', text: message });
}

// ------------------------------------------------------------------ render

// renderCommandsPage draws the commands page into root (#page-root).
export function renderCommandsPage(root, ctx, view) {
  if (!root) return;
  const { state, t } = ctx;
  const model = commandsView(state, view);
  const selected = model.selectedRequest;
  // Pin the request whose approval form is drawn: a poll that prepends a new
  // pending request must not move the fallback selection under an edit.
  if (selected?.status === 'pending') state.selectedRequestId = selected.id;
  const selectedId = selected?.id ?? '';

  const queueCard = el('div', { class: 'card card-outline card-warning h-100' },
    el('div', { class: 'card-header' },
      el('div', { class: 'd-flex align-items-center justify-content-between' },
        el('h3', { class: 'card-title fw-bold', text: t('cmd.queueTitle') }),
        el('span', { class: 'badge text-bg-warning approvals-queue-count', text: `${model.queue.length}${t('cmd.queueWaiting')}` }))),
    model.hiddenPending > 0 ? hiddenPendingNotice(model.hiddenPending, t) : null,
    el('div', { class: 'card-body p-0' },
      el('div', { class: 'list-group list-group-flush ssh-list' },
        model.queue.length
          ? model.queue.map((request) => requestRow(request, selectedId, { unknown: model.unknownIds.has(request.id), t }))
          : emptyBlock(model.filter ? t('appr.queueEmptyFiltered', { name: model.filterName }) : t('cmd.queueEmpty')))));

  const historyCard = el('div', { class: 'card' },
    el('div', { class: 'card-header' }, el('h3', { class: 'card-title fw-bold', text: t('cmd.historyTitle') })),
    el('div', { class: 'card-body p-0' },
      model.history.length
        ? historyTable(model.history, selectedId, model.unknownIds, t)
        : emptyBlock(model.filter ? t('appr.historyEmptyFiltered', { name: model.filterName }) : t('cmd.historyEmpty'))));

  const page = el('div', { class: 'row g-3' },
    el('div', { class: 'col-12 col-xl-4' }, queueCard),
    el('div', { class: 'col-12 col-xl-8' }, selected ? commandDetail(selected, ctx) : emptyCommandDetail(t)),
    el('div', { class: 'col-12' }, historyCard));
  root.replaceChildren(page);

  if (selected?.status === 'pending') {
    // The cached lookup (if it is for this request) was drawn above; refresh
    // it in the background. The result patches the panel in place, so it
    // never redraws the page (or a textarea being edited).
    loadApprovalTarget(ctx, selected.id).catch(() => {});
  }
}

function hiddenPendingNotice(count, t) {
  return el('div', { class: 'alert alert-info d-flex align-items-center justify-content-between gap-2 m-2 mb-0 py-2 approvals-hidden-pending', role: 'status' },
    el('span', { text: t('appr.hiddenPending', { n: count }) }),
    el('button', { class: 'btn btn-sm btn-outline-primary', type: 'button', dataset: { action: 'srvfilter-show-all' }, text: t('appr.showAll') }));
}

export function requestRow(request, selectedId, { unknown = false, t }) {
  return el('button', {
    class: `list-group-item list-group-item-action ${request.id === selectedId ? 'active' : ''}`.trim(),
    type: 'button',
    dataset: { selectRequest: request.id },
  },
  el('div', { class: 'd-flex align-items-start justify-content-between gap-2' },
    el('div', {},
      el('div', { class: 'fw-bold', text: request.id }),
      el('small', {},
        unknown ? unknownBadge(t) : null,
        `${request.host || '-'} / ${request.requestedBy || t('cmd.unknownAgent')} / ${formatTimestamp(request.requestedAt)}`)),
    statusBadge(request.status, t)),
  el('code', { class: 'd-block mt-2 small text-truncate', text: request.command ?? '' }));
}

export function historyTable(requests, selectedId, unknownIds, t) {
  const head = el('thead', {}, el('tr', {},
    el('th', { text: t('hist.time') }),
    el('th', { text: t('hist.agent') }),
    el('th', { text: t('hist.status') }),
    el('th', { text: t('hist.command') }),
    el('th', { text: t('hist.reason') }),
    el('th', { class: 'text-end', text: t('hist.action') })));
  const body = el('tbody', {}, requests.map((request) => el('tr', {
    class: request.id === selectedId ? 'table-active' : '',
    dataset: { selectRequest: request.id },
    style: 'cursor:pointer',
  },
  el('td', { class: 'text-nowrap', text: formatTimestamp(request.requestedAt) }),
  el('td', {},
    unknownIds?.has(request.id) ? unknownBadge(t) : null,
    request.host || '-',
    el('br'),
    el('small', { class: 'text-secondary', text: request.requestedBy || t('cmd.unknownAgent') })),
  el('td', {}, statusBadge(request.status, t)),
  el('td', {}, el('code', { text: request.executedCommand || request.command || '' })),
  el('td', { text: request.reason || '-' }),
  el('td', { class: 'text-end' },
    el('button', {
      class: 'btn btn-outline-danger btn-sm',
      type: 'button',
      dataset: { action: 'delete-command-request', commandRequestId: request.id },
      title: t('cmd.deleteHistory'),
      'aria-label': t('cmd.deleteHistory'),
    }, icon('bi-trash'))))));
  return el('div', { class: 'table-responsive' }, el('table', { class: 'table table-hover align-middle mb-0' }, head, body));
}

function metaLine(selected, t) {
  let line = `${selected.host || '-'} · ${formatTimestamp(selected.requestedAt)} · risk ${selected.risk ?? ''}`;
  if (selected.decidedAt) line += ` · ${t('cmd.decided')} ${formatTimestamp(selected.decidedAt)} (${selected.decidedBy ?? ''})`;
  if (selected.exitCode !== null && selected.exitCode !== undefined) line += ` · ${t('cmd.exitCode')} ${selected.exitCode}`;
  return el('div', { class: 'small text-secondary mb-3', text: line });
}

function agentLine(selected, ctx) {
  const { state, t } = ctx;
  const policy = agentPolicy(state, selected.requestedBy);
  const bypassing = Boolean(policy?.approvalBypass);
  const notes = [];
  if (bypassing && policy.expiresAt) notes.push(t('appr.bypass.until', { time: formatTimestamp(policy.expiresAt) }));
  if (bypassing && policy.active === false && !agentHasToken(state, selected.requestedBy)) notes.push(t('appr.bypass.inactive'));
  return el('div', { class: 'command-agent mb-3' },
    el('span', { text: t('cmd.agent') }),
    ' ',
    el('strong', { text: selected.requestedBy || t('cmd.unknownAgent') }),
    bypassing ? [
      ' ',
      el('em', { text: notes.length ? `${t('cmd.bypassing')} (${notes.join(', ')})` : t('cmd.bypassing') }),
      ' ',
      el('button', {
        class: 'btn btn-link btn-sm p-0 ms-2',
        type: 'button',
        dataset: { action: 'revoke-bypass', agentName: selected.requestedBy },
        text: t('cmd.revokeBypass'),
      }),
    ] : null);
}

// aiLineContextSlot is the box with the AI session context of a line
// (phase7 §3.7); main.js fills it through ctx.mountAILineContext.
function aiLineContextSlot(selected, ctx) {
  const slot = el('div', { class: 'ai-line-context-slot', dataset: { requestId: selected.id } });
  ctx.mountAILineContext?.(slot, selected.id);
  return slot;
}

function terminalPanel(title, content, error = false) {
  return el('div', { class: 'col-12 col-lg-6' },
    el('div', { class: `terminal-panel${error ? ' terminal-error' : ''}` },
      el('div', { class: 'terminal-title', text: title }),
      el('pre', { text: content })));
}

export function commandDetail(selected, ctx) {
  const { state, t } = ctx;
  const pending = selected.status === 'pending';
  const executed = selected.executedCommand || '';
  const edited = executed && executed !== selected.command;

  const commandBlock = pending
    ? [
      el('textarea', {
        class: 'form-control font-monospace',
        id: 'approval-command',
        dataset: { requestId: selected.id },
        // A line of an AI session cannot be edited (phase7 §3.6, §8.2).
        readonly: selected.sessionId ? true : undefined,
        rows: 3,
        spellcheck: 'false',
        autocapitalize: 'none',
        autocorrect: 'off',
        value: selected.command ?? '',
      }),
      el('div', { class: 'form-text', text: selected.sessionId ? t('approval.aiReadonly') : t('cmd.editHint') }),
    ]
    : [
      el('pre', { class: 'command-block' }, el('code', { text: executed || selected.command || '' })),
      edited ? el('div', { class: 'small text-warning-emphasis mt-1' }, `${t('cmd.edited')}: `, el('code', { text: selected.command ?? '' })) : null,
    ];

  const body = el('div', { class: 'card-body' },
    agentLine(selected, ctx),
    metaLine(selected, t),
    pending ? targetPanel(selected.id, cachedTarget(state, selected.id), t) : null,
    selected.sessionId ? aiLineContextSlot(selected, ctx) : null,
    el('div', { class: 'mb-3' },
      el('div', { class: 'd-flex justify-content-between align-items-center mb-2' },
        el('h5', { class: 'fw-bold mb-0', text: t('cmd.plannedCommand') }),
        el('button', { class: 'btn btn-outline-secondary btn-sm', type: 'button', dataset: { action: 'copy-command' }, text: t('common.copy') })),
      commandBlock),
    el('div', { class: 'mb-3' },
      el('h5', { class: 'fw-bold', text: t('cmd.reason') }),
      el('div', { class: 'reason-box', text: selected.reason || t('cmd.reasonEmpty') })),
    el('div', { class: 'row g-3' },
      terminalPanel('stdout', selected.stdout || (pending ? t('cmd.stdoutPlaceholder') : t('cmd.stdoutEmpty'))),
      terminalPanel('stderr', selected.stderr || t('cmd.stderrPlaceholder'), true)));

  const footer = el('div', { class: 'card-footer d-flex gap-2' },
    el('button', {
      class: 'btn btn-outline-danger',
      type: 'button',
      dataset: { action: 'delete-command-request', commandRequestId: selected.id },
    }, icon('bi-trash', 'me-1'), t('cmd.deleteHistory')));
  if (pending) {
    footer.append(
      el('button', { class: 'btn btn-danger', type: 'button', dataset: { action: 'reject-command' }, text: t('cmd.reject') }),
      el('span', { class: 'approve-bypass-wrap' },
        el('button', { class: 'btn btn-outline-primary', type: 'button', dataset: { action: 'approve-bypass-agent' }, text: t('cmd.approveBypass') })),
      el('button', { class: 'btn btn-primary ms-auto', type: 'button', dataset: { action: 'approve-command' }, text: t('cmd.approve') }));
  }

  const card = el('div', { class: `card card-outline ${pending ? 'card-warning' : 'card-secondary'} h-100` },
    el('div', { class: 'card-header' },
      el('div', { class: 'd-flex align-items-start justify-content-between gap-3' },
        el('div', {},
          el('h3', { class: 'card-title fw-bold mb-1', text: pending ? t('cmd.approveTitle') : t('cmd.detailTitle') }),
          el('div', { class: 'text-secondary small', text: pending ? t('cmd.approveSubtitle') : t('cmd.detailSubtitle') })),
        statusBadge(selected.status, t))),
    body,
    footer);
  if (pending) applyApproveButtons(card, state, selected, cachedTarget(state, selected.id), t);
  return card;
}

export function emptyCommandDetail(t) {
  return el('div', { class: 'card h-100' },
    el('div', { class: 'card-body empty-state' },
      icon('bi-terminal', 'fs-1 text-secondary'),
      el('h4', { text: t('cmd.emptyTitle') }),
      el('p', { text: t('cmd.emptyDesc') })));
}

function cachedTarget(state, requestId) {
  const target = approvalsState(state).target;
  return target && target.requestId === requestId ? target : null;
}

// targetPanel is the phase 3 "where will this run" block. `target` is null
// while the first lookup for this request is in flight.
export function targetPanel(requestId, target, t) {
  const panel = el('div', { class: 'approval-target mb-3', id: 'approval-target', dataset: { requestId } });
  fillTargetPanel(panel, target, t);
  return panel;
}

function fillTargetPanel(panel, target, t) {
  const children = [el('h5', { class: 'fw-bold', text: t('appr.target.title') })];
  if (!target) {
    children.push(el('div', { class: 'small text-secondary approval-target-loading', text: t('appr.target.loading') }));
  } else if (target.lookupError) {
    children.push(el('div', { class: 'alert alert-warning py-2 mb-0 approval-target-error', role: 'alert', text: t('appr.target.error') }));
  } else if (!target.found) {
    children.push(el('div', { class: 'alert alert-warning py-2 mb-0 approval-target-missing', role: 'alert', text: t('appr.target.notFound') }));
  } else {
    if (target.fpChanged) {
      children.push(el('div', { class: 'alert alert-danger py-2 approval-target-changed', role: 'alert' },
        el('div', { class: 'fw-bold', text: t('appr.target.changed', { from: target.requestedFp || '-', to: target.currentFp || '-' }) }),
        el('div', { text: t('appr.target.changedAction') })));
    } else if (target.executable === false) {
      children.push(el('div', { class: 'alert alert-warning py-2 approval-target-serial', role: 'alert', text: t('appr.target.notExecutable') }));
    }
    const row = (label, ...value) => [el('dt', { class: 'col-sm-4', text: label }), el('dd', { class: 'col-sm-8 mb-1' }, ...value)];
    const fp = (value, cls) => (value ? el('code', { class: cls, text: value }) : el('span', { class: `text-secondary ${cls}`, text: t('appr.target.noFp') }));
    const keys = target.hostKey?.keys ?? [];
    const hostKey = target.executable === false
      ? null
      : row(t('appr.target.hostKey'), keys.length
        ? keys.map((key) => el('div', { class: 'approval-hostkey' }, el('span', { class: 'text-secondary me-1', text: key.type ?? '' }), el('code', { text: key.fingerprint ?? '' })))
        : el('span', { class: 'text-secondary approval-hostkey-unknown', text: t('appr.target.hostKeyUnknown') }));
    children.push(el('dl', { class: 'row small mb-0' },
      row(t('appr.target.name'), `${target.name || '-'}${target.type ? ` (${target.type})` : ''}`),
      row(t('appr.target.resolvedBy'), resolvedByLabel(target.resolvedBy, t)),
      row(t('appr.target.requested'), fp(target.requestedFp, 'approval-fp-requested')),
      row(t('appr.target.current'), fp(target.currentFp, `approval-fp-current${target.fpChanged ? ' text-danger fw-bold' : ''}`)),
      hostKey));
  }
  panel.replaceChildren(...children);
}

// applyApproveButtons sets the approve / bypass buttons inside scope from the
// target lookup and the bypass precondition.
function applyApproveButtons(scope, state, request, target, t) {
  const block = approvalBlock(target);
  const approve = scope.querySelector('[data-action="approve-command"]');
  if (approve) {
    approve.disabled = block.blocked;
    if (block.blocked) approve.title = t(block.reasonKey);
    else approve.removeAttribute('title');
  }
  const bypass = scope.querySelector('[data-action="approve-bypass-agent"]');
  if (bypass) {
    const availability = bypassAvailability(state, request);
    const reasonKey = block.blocked ? block.reasonKey : availability.reasonKey;
    bypass.disabled = block.blocked || !availability.allowed;
    // A disabled button gets no hover events; the title goes on the wrapper.
    const wrap = bypass.closest('.approve-bypass-wrap') ?? bypass;
    if (reasonKey) wrap.title = t(reasonKey);
    else wrap.removeAttribute('title');
  }
}

// loadApprovalTarget asks the backend where the request would run now. The
// newest lookup is stored and patched into the on-screen panel; the result is
// returned either way (the approve handler uses its own fresh lookup).
export async function loadApprovalTarget(ctx, requestId) {
  const { state } = ctx;
  const own = approvalsState(state);
  const seq = ++own.seq;
  let target;
  try {
    target = { ...(await GetApprovalTarget(requestId)), requestId };
  } catch {
    target = { requestId, found: false, lookupError: true };
  }
  if (seq !== own.seq) return target;
  own.target = target;
  patchTargetPanel(ctx, target);
  return target;
}

function patchTargetPanel(ctx, target) {
  const { state, t } = ctx;
  const panel = document.getElementById('approval-target');
  if (!panel || panel.dataset.requestId !== target.requestId) return;
  fillTargetPanel(panel, target, t);
  const request = (state.data?.requests ?? []).find((r) => r.id === target.requestId);
  const card = panel.closest('.card') ?? document;
  if (request) applyApproveButtons(card, state, request, target, t);
}

// ---------------------------------------------------------------- handlers

// approvalFormTarget is the pending request the on-screen approval form
// belongs to, or null when the form is gone, was drawn for another request
// than the selection, or the request is no longer pending (audit #22/#40).
export function approvalFormTarget(state, view) {
  const formId = document.querySelector('#approval-command')?.dataset.requestId ?? '';
  const selected = selectedCommandRequest(state, view);
  if (!formId || !selected || selected.id !== formId || selected.status !== 'pending') return null;
  return selected;
}

function refuseStaleApproval(ctx) {
  ctx.state.dirty.approval = false;
  ctx.toast(ctx.t('cmd.staleSelection'));
  ctx.renderPage();
}

// Leaves the edited command in place: the user can fix it and retry.
function keepEdit(ctx, button) {
  ctx.state.dirty.approval = true;
  if (button) button.disabled = false;
}

async function finishApproval(ctx, request, result) {
  const { state, t } = ctx;
  if (result?.code === 'target_changed') await loadApprovalTarget(ctx, request.id);
  // Keep the request selected so its stdout/stderr (or new state) shows.
  state.selectedRequestId = request.id ?? '';
  ctx.toast(approvalResultMessage(result, t));
  await ctx.refresh({ silent: true });
}

async function approve(ctx, view, button, { bypass }) {
  const { state, t } = ctx;
  const selected = selectedCommandRequest(state, view);
  if (!selected) return;
  if (approvalFormTarget(state, view) !== selected) {
    refuseStaleApproval(ctx);
    return;
  }
  const id = selected.id ?? '';
  const command = document.querySelector('#approval-command')?.value ?? selected.command ?? '';
  if (button) button.disabled = true;

  // Look the target up again right before approving; the backend refuses a
  // changed target anyway (target_changed), this just says so up front.
  const target = await loadApprovalTarget(ctx, id);
  // A poll may have redrawn the page meanwhile; the form must still be this
  // request's.
  if (approvalFormTarget(state, view)?.id !== id) {
    refuseStaleApproval(ctx);
    return;
  }
  const block = approvalBlock(target);
  if (block.blocked) {
    ctx.toast(t(block.reasonKey === 'appr.blocked.changed' ? 'appr.code.target_changed' : 'appr.target.notExecutable'));
    return;
  }
  if (bypass) {
    const availability = bypassAvailability(state, selected);
    if (!availability.allowed) {
      ctx.toast(t(availability.reasonKey));
      if (button) button.disabled = false;
      return;
    }
    if (!window.confirm(bypassConfirmText(state, selected, t))) {
      if (button) button.disabled = false;
      return;
    }
  }

  state.dirty.approval = false;
  ctx.bumpGen();
  let result = bypass ? await ApproveCommandAndBypassAgent(id, command) : await ApproveCommand(id, command);
  if (!result?.ok && result?.code === 'risk_increased') {
    if (bypass) {
      // There is no "confirmed + bypass" path: approve normally instead.
      keepEdit(ctx, button);
      ctx.toast(t('appr.bypassRiskIncreased'));
      return;
    }
    if (!window.confirm(riskConfirmText(result, t, ctx.lang?.()))) {
      keepEdit(ctx, button);
      ctx.toast(t('appr.riskCanceled'));
      return;
    }
    ctx.bumpGen();
    result = await ApproveCommandConfirmed(id, command);
  }
  if (!result?.ok && result?.code === 'bearer_required') {
    // Nothing changed on the backend; the request is still pending.
    keepEdit(ctx, button);
    ctx.toast(approvalResultMessage(result, t));
    return;
  }
  await finishApproval(ctx, selected, result);
}

async function reject(ctx, view) {
  const { state, t } = ctx;
  const selected = selectedCommandRequest(state, view);
  if (!selected) return;
  if (approvalFormTarget(state, view) !== selected) {
    refuseStaleApproval(ctx);
    return;
  }
  state.dirty.approval = false;
  ctx.bumpGen();
  const result = await RejectCommand(selected.id ?? '');
  ctx.toast(approvalResultMessage(result, t));
  await ctx.refresh({ silent: true });
}

async function copyCommand(ctx, view) {
  const value = String(selectedCommandRequest(ctx.state, view)?.command ?? '');
  if (!value) return;
  if (ctx.copyText) {
    await ctx.copyText(value);
    return;
  }
  try {
    await navigator.clipboard.writeText(value);
    ctx.toast(ctx.t('common.copied'));
  } catch {
    // Clipboard unavailable; ctx.copyText has the textarea fallback.
  }
}

// handleApprovalsClick handles the commands page's own actions. It returns
// true when it handled the event. delete-command-request (the modal) and
// srvfilter-show-all stay with main.js.
export async function handleApprovalsClick(event, ctx, view) {
  const actionElement = event.target.closest?.('[data-action]');
  const action = actionElement?.dataset.action;
  if (action === 'approve-command') {
    await approve(ctx, view, actionElement, { bypass: false });
    return true;
  }
  if (action === 'approve-bypass-agent') {
    await approve(ctx, view, actionElement, { bypass: true });
    return true;
  }
  if (action === 'reject-command') {
    await reject(ctx, view);
    return true;
  }
  if (action === 'revoke-bypass') {
    ctx.bumpGen();
    const result = await RevokeAgentApprovalBypass(actionElement.dataset.agentName ?? '');
    ctx.toast(result?.message ?? '');
    await ctx.refresh({ silent: true });
    return true;
  }
  if (action === 'copy-command') {
    await copyCommand(ctx, view);
    return true;
  }
  if (action) return false;

  const row = event.target.closest?.('[data-select-request]');
  if (row) {
    ctx.state.selectedRequestId = row.dataset.selectRequest;
    ctx.state.dirty.approval = false;
    ctx.renderPage();
    return true;
  }
  return false;
}

export function handleApprovalsInput(event, ctx) {
  if (event.target?.id !== 'approval-command') return false;
  ctx.state.dirty.approval = true;
  return true;
}
