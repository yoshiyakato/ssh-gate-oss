// The "AI sessions" page (phase7-ai-session §8): the list of AI-only SSH
// sessions, a read-only viewer of one session's raw output, force close (with
// a confirm dialog), the AI session settings, the context box the approval
// screen shows for a session line, and the counts the sidebar shows.
//
// Rules this module keeps:
// - Humans never type into an AI session (§4). The viewer xterm is created
//   with disableStdin:true and no onData handler; no OSC 52 / link addon is
//   loaded and title changes are ignored (§6.2).
// - Everything is built with DOM calls (no innerHTML): session reasons, cwd,
//   lines and output all come from agents and remote hosts.
// - The backend is reached through the Wails bindings on window.go.main.App
//   (or an object passed to setAISessionsApi in tests), not through a static
//   import of wailsjs/go/main/App. The bindings for phase 7 only exist after
//   `wails generate module`, and a named import of a missing export would
//   break the build of every page before that.
// - main.js owns the wiring (§8.3): renderAISessionsPage from renderPage,
//   handleAISessionsClick from the click delegate, onAISessionData and
//   onAISessionsChanged from EventsOn, disposeAISessionViewer when leaving
//   the page, aiSessionsBusy from pageIsBusy, AI_SESSIONS_I18N in i18n.js.

import { Terminal } from '@xterm/xterm';
import { FitAddon } from '@xterm/addon-fit';
import { escapeHtml } from './model.js';

// ------------------------------------------------------------------ i18n

const CLOSE_REASONS = [
  'agent_closed', 'human_closed', 'idle_timeout', 'max_lifetime', 'disconnected',
  'connection_deleted', 'target_changed', 'token_revoked', 'ai_session_disabled',
  'remote_exit', 'conn_lost', 'app_shutdown', 'app_restart', 'open_failed',
];
const SESSION_STATES = ['opening', 'idle', 'pending_approval', 'busy', 'closing', 'closed'];
const LINE_STATES = [
  'pending', 'running', 'auto_running', 'succeeded', 'auto_succeeded', 'failed',
  'auto_failed', 'rejected', 'expired', 'withdrawn', 'interrupted',
];
const REASON_CODES = [
  'cd_builtin', 'env_overlay', 'env_overlay_unsafe', 'dangerous_env',
  'shell_state_change', 'detaches', 'session_tainted',
];

export const AI_SESSIONS_I18N = {
  ja: {
    'nav.aisessions': 'AI セッション',
    'side.aiSessions': 'AI セッション {n}（承認待ち {p}）',
    'side.aiSessionsOnly': 'AI セッション {n}',
    'ais.title': 'AI セッション',
    'ais.subtitle': 'エージェント専用の継続 SSH セッションです。人は閲覧と強制終了だけができ、入力はできません。',
    'ais.reload': '再読み込み',
    'ais.loading': '読み込み中…',
    'ais.loadFailed': 'AI セッションを読み込めませんでした: {message}',
    'ais.empty': '開いている AI セッションはありません。',
    'ais.disabled': 'AI セッションは無効です。設定で有効にできます',
    'ais.col.state': '状態',
    'ais.col.id': 'セッション',
    'ais.col.connection': '接続先',
    'ais.col.agent': 'エージェント',
    'ais.col.mode': 'モード',
    'ais.col.cwd': '場所',
    'ais.col.time': '経過 / 終了',
    'ais.col.lines': '行',
    'ais.col.actions': '操作',
    'ais.state.opening': '開始中',
    'ais.state.idle': '待機中',
    'ais.state.pending_approval': '承認待ち',
    'ais.state.busy': '実行中',
    'ais.state.closing': '終了中',
    'ais.state.closed': '終了',
    'ais.state.closedWith': '終了（{reason}）',
    'ais.close.agent_closed': 'エージェントが終了',
    'ais.close.human_closed': '人が強制終了',
    'ais.close.idle_timeout': 'アイドルで終了',
    'ais.close.max_lifetime': '最大時間で終了',
    'ais.close.disconnected': '切断で終了',
    'ais.close.connection_deleted': '接続先の削除で終了',
    'ais.close.target_changed': '実行先の変更で終了',
    'ais.close.token_revoked': 'トークン失効で終了',
    'ais.close.ai_session_disabled': '機能 OFF で終了',
    'ais.close.remote_exit': 'リモートで終了',
    'ais.close.conn_lost': '接続断',
    'ais.close.app_shutdown': 'アプリ終了',
    'ais.close.app_restart': '再起動で回収',
    'ais.close.open_failed': '開始に失敗',
    'ais.close.unknown': '終了（{code}）',
    'ais.line.pending': '承認待ち',
    'ais.line.running': '実行中',
    'ais.line.auto_running': '実行中（承認省略）',
    'ais.line.succeeded': '成功',
    'ais.line.auto_succeeded': '成功（承認省略）',
    'ais.line.failed': '失敗',
    'ais.line.auto_failed': '失敗（承認省略）',
    'ais.line.rejected': '拒否',
    'ais.line.expired': '期限切れ',
    'ais.line.withdrawn': '取り下げ',
    'ais.line.interrupted': '中断',
    'ais.mode.exec': 'exec',
    'ais.mode.shell': 'shell',
    'ais.tainted': 'シェルの状態（alias / 関数 / trap など）が変更されています。以後の行はすべて承認が必要です',
    'ais.taintedBadge': '汚染',
    'ais.pendingBadge': '承認待ち {n}',
    'ais.redacted': '秘密らしい文字列を {n} 件伏せました',
    'ais.missed': '以前の出力は破棄されました（{n} バイト）',
    'ais.readonly': '読み取り専用',
    'ais.view': '閲覧',
    'ais.viewing': '閲覧中: {id}',
    'ais.viewerClose': '閲覧をやめる',
    'ais.viewerReadFailed': '出力を読み込めませんでした: {message}',
    'ais.viewerClosed': 'セッションは終了しました（{reason}）',
    'ais.viewerReason': '目的: {reason}',
    'ais.history': '送信した行',
    'ais.historyEmpty': 'まだ行は送られていません。',
    'ais.toApproval': '承認画面へ',
    'ais.forceClose': '強制終了',
    'ais.forceCloseAll': 'すべて強制終了',
    'ais.confirmClose': 'このセッションを終了します。承認待ちの行は取り下げられ、実行中の行は中止されます。',
    'ais.confirmClose.title': 'AI セッション {id} を強制終了しますか？',
    'ais.confirmCloseAll.title': 'すべての AI セッションを強制終了しますか？',
    'ais.confirmCloseAll.body': '開いている {n} 件のセッションを終了します。承認待ちの行は取り下げられ、実行中の行は中止されます。',
    'ais.confirm.cancel': 'キャンセル',
    'ais.confirm.close': '閉じる',
    'ais.time.hm': '{h}時間{m}分',
    'ais.time.m': '{m}分',
    'ais.time.lessThanMinute': '1分未満',
    'ais.settings': '設定',
    'ais.settings.title': 'AI セッションの設定',
    'ais.settings.loadFailed': '設定を読み込めませんでした: {message}',
    'ais.settings.enabled': 'AI セッションを有効にする',
    'ais.settings.enabledHelp': 'エージェント専用トークンで認証したエージェントだけが使えます。High の行は常に人の承認が必要です。',
    'ais.settings.idleMinutes': 'アイドルタイムアウト（分）',
    'ais.settings.maxMinutes': '最大寿命（分）',
    'ais.settings.perAgent': '1 エージェントあたりの同時数',
    'ais.settings.total': '全体の同時数',
    'ais.settings.bypassMaxRisk': '承認省略の上限',
    'ais.settings.bypassHelp': '継続セッションでは副作用が積み重なるため、既定は Low です。High は選べません。',
    'ais.settings.ringKB': 'リングバッファ（KiB）',
    'ais.settings.auditKB': '監査に残す出力（KiB / セッション）',
    'ais.settings.shellConns': 'shell モードを許す接続先',
    'ais.settings.shellHelp': 'shell モードではシェルの状態が行をまたいで残ります。必要な接続先だけを選んでください。',
    'ais.settings.noConnections': '接続先がありません。',
    'ais.settings.range': '{min}〜{max} の整数で入力してください',
    'ais.settings.save': '保存',
    'ais.settings.cancel': '取り消し',
    'ais.settings.invalid': '入力に誤りがあります',
    'ais.risk.Low': 'Low まで',
    'ais.risk.Medium': 'Medium まで',
    'ais.reason.cd_builtin': 'ゲートウェイが解釈する cd',
    'ais.reason.env_overlay': '環境変数の上乗せ',
    'ais.reason.env_overlay_unsafe': '安全でない名前の環境変数が上乗せされています',
    'ais.reason.dangerous_env': '危険な名前の環境変数',
    'ais.reason.shell_state_change': 'シェルの状態を変える構文',
    'ais.reason.detaches': 'セッションの外で動き続ける可能性',
    'ais.reason.session_tainted': '汚染されたセッション',
    'ais.reason.base': 'コマンド単体の判定: {risk}',
    'approval.aiLine': 'AI セッション内の行',
    'approval.aiContext': '実行場所 {cwd} · 直前の行',
    'approval.aiMeta': '{id} · {agent} · {mode} · {seq} 行目',
    'approval.aiCwd': '実行場所',
    'approval.aiEnv': '環境変数の上乗せ',
    'approval.aiEnvNone': 'なし',
    'approval.aiEnvUnset': '（削除）',
    'approval.aiMasked': '［伏せています］',
    'approval.aiPrevLines': '直前の行',
    'approval.aiPrevNone': 'このセッションの最初の行です。',
    'approval.aiOutput': '直前の出力（末尾）',
    'approval.aiReasons': '分類の理由',
    'approval.aiReadonly': 'AI セッションの行は修正して承認できません（AI が持つ文脈と食い違うため）。',
    'approval.aiBypassNote': '承認省略を許可しても、このセッションでは {risk} を超える行、汚染されたセッションの行、安全でない名前の環境変数を設定する行は承認が必要です。',
    'approval.aiShellNote': 'shell モード: 承認した行で起動したプログラムが、偽のプロンプトを出せる可能性があります。対話プログラムを起動する行は慎重に承認してください。',
  },
  en: {
    'nav.aisessions': 'AI sessions',
    'side.aiSessions': 'AI sessions {n} ({p} awaiting approval)',
    'side.aiSessionsOnly': 'AI sessions {n}',
    'ais.title': 'AI sessions',
    'ais.subtitle': 'Persistent SSH sessions for agents only. People can view and force-close them, but cannot type into them.',
    'ais.reload': 'Reload',
    'ais.loading': 'Loading…',
    'ais.loadFailed': 'Could not load AI sessions: {message}',
    'ais.empty': 'No AI sessions are open.',
    'ais.disabled': 'AI sessions are disabled. Enable them in settings.',
    'ais.col.state': 'State',
    'ais.col.id': 'Session',
    'ais.col.connection': 'Connection',
    'ais.col.agent': 'Agent',
    'ais.col.mode': 'Mode',
    'ais.col.cwd': 'Directory',
    'ais.col.time': 'Age / closed',
    'ais.col.lines': 'Lines',
    'ais.col.actions': 'Actions',
    'ais.state.opening': 'Opening',
    'ais.state.idle': 'Idle',
    'ais.state.pending_approval': 'Awaiting approval',
    'ais.state.busy': 'Running',
    'ais.state.closing': 'Closing',
    'ais.state.closed': 'Closed',
    'ais.state.closedWith': 'Closed ({reason})',
    'ais.close.agent_closed': 'Closed by agent',
    'ais.close.human_closed': 'Force-closed',
    'ais.close.idle_timeout': 'Idle timeout',
    'ais.close.max_lifetime': 'Max lifetime',
    'ais.close.disconnected': 'Disconnected',
    'ais.close.connection_deleted': 'Connection deleted',
    'ais.close.target_changed': 'Target changed',
    'ais.close.token_revoked': 'Token revoked',
    'ais.close.ai_session_disabled': 'Feature disabled',
    'ais.close.remote_exit': 'Remote exit',
    'ais.close.conn_lost': 'Connection lost',
    'ais.close.app_shutdown': 'App shutdown',
    'ais.close.app_restart': 'Recovered after restart',
    'ais.close.open_failed': 'Failed to open',
    'ais.close.unknown': 'Closed ({code})',
    'ais.line.pending': 'Awaiting approval',
    'ais.line.running': 'Running',
    'ais.line.auto_running': 'Running (bypass)',
    'ais.line.succeeded': 'Succeeded',
    'ais.line.auto_succeeded': 'Succeeded (bypass)',
    'ais.line.failed': 'Failed',
    'ais.line.auto_failed': 'Failed (bypass)',
    'ais.line.rejected': 'Rejected',
    'ais.line.expired': 'Expired',
    'ais.line.withdrawn': 'Withdrawn',
    'ais.line.interrupted': 'Interrupted',
    'ais.mode.exec': 'exec',
    'ais.mode.shell': 'shell',
    'ais.tainted': 'Shell state (aliases, functions, traps…) was changed. Every further line needs approval.',
    'ais.taintedBadge': 'Tainted',
    'ais.pendingBadge': '{n} awaiting approval',
    'ais.redacted': '{n} secret-like strings redacted',
    'ais.missed': 'Earlier output was discarded ({n} bytes)',
    'ais.readonly': 'Read-only',
    'ais.view': 'View',
    'ais.viewing': 'Viewing: {id}',
    'ais.viewerClose': 'Stop viewing',
    'ais.viewerReadFailed': 'Could not read the output: {message}',
    'ais.viewerClosed': 'The session has ended ({reason})',
    'ais.viewerReason': 'Purpose: {reason}',
    'ais.history': 'Lines sent',
    'ais.historyEmpty': 'No lines have been sent yet.',
    'ais.toApproval': 'Open approval',
    'ais.forceClose': 'Force close',
    'ais.forceCloseAll': 'Force close all',
    'ais.confirmClose': 'End this session? Pending lines are withdrawn and the running line is stopped.',
    'ais.confirmClose.title': 'Force close AI session {id}?',
    'ais.confirmCloseAll.title': 'Force close every AI session?',
    'ais.confirmCloseAll.body': '{n} open session(s) will be ended. Pending lines are withdrawn and running lines are stopped.',
    'ais.confirm.cancel': 'Cancel',
    'ais.confirm.close': 'Close',
    'ais.time.hm': '{h}h {m}m',
    'ais.time.m': '{m}m',
    'ais.time.lessThanMinute': '<1m',
    'ais.settings': 'Settings',
    'ais.settings.title': 'AI session settings',
    'ais.settings.loadFailed': 'Could not load settings: {message}',
    'ais.settings.enabled': 'Enable AI sessions',
    'ais.settings.enabledHelp': 'Only agents authenticated with their own agent token can use them. High-risk lines always need a person\'s approval.',
    'ais.settings.idleMinutes': 'Idle timeout (minutes)',
    'ais.settings.maxMinutes': 'Max lifetime (minutes)',
    'ais.settings.perAgent': 'Sessions per agent',
    'ais.settings.total': 'Sessions in total',
    'ais.settings.bypassMaxRisk': 'Approval bypass limit',
    'ais.settings.bypassHelp': 'Side effects add up in a persistent session, so the default is Low. High cannot be chosen.',
    'ais.settings.ringKB': 'Ring buffer (KiB)',
    'ais.settings.auditKB': 'Output kept for audit (KiB per session)',
    'ais.settings.shellConns': 'Connections allowed to use shell mode',
    'ais.settings.shellHelp': 'In shell mode, shell state carries over between lines. Pick only the connections that need it.',
    'ais.settings.noConnections': 'No connections.',
    'ais.settings.range': 'Enter a whole number from {min} to {max}',
    'ais.settings.save': 'Save',
    'ais.settings.cancel': 'Discard',
    'ais.settings.invalid': 'Some values are invalid',
    'ais.risk.Low': 'Up to Low',
    'ais.risk.Medium': 'Up to Medium',
    'ais.reason.cd_builtin': 'cd handled by the gateway',
    'ais.reason.env_overlay': 'Environment variable overlay',
    'ais.reason.env_overlay_unsafe': 'An environment variable with a non-safe name is set',
    'ais.reason.dangerous_env': 'Dangerous environment variable name',
    'ais.reason.shell_state_change': 'Changes shell state',
    'ais.reason.detaches': 'May keep running outside the session',
    'ais.reason.session_tainted': 'Tainted session',
    'ais.reason.base': 'Command on its own: {risk}',
    'approval.aiLine': 'Line in an AI session',
    'approval.aiContext': 'Runs in {cwd} · Previous lines',
    'approval.aiMeta': '{id} · {agent} · {mode} · line {seq}',
    'approval.aiCwd': 'Runs in',
    'approval.aiEnv': 'Environment overlay',
    'approval.aiEnvNone': 'None',
    'approval.aiEnvUnset': '(unset)',
    'approval.aiMasked': '[hidden]',
    'approval.aiPrevLines': 'Previous lines',
    'approval.aiPrevNone': 'This is the first line of the session.',
    'approval.aiOutput': 'Recent output (tail)',
    'approval.aiReasons': 'Why it was classified this way',
    'approval.aiReadonly': 'Lines in an AI session cannot be edited before approval (the agent\'s context would no longer match).',
    'approval.aiBypassNote': 'Even with approval bypass, lines above {risk}, lines in a tainted session and lines that set a non-safe environment variable still need approval in this session.',
    'approval.aiShellNote': 'Shell mode: a program started by an approved line could print a fake prompt. Approve lines that start interactive programs with care.',
  },
};

// ------------------------------------------------------------------ backend

let apiOverride = null;

// setAISessionsApi replaces the Wails bindings (tests). null restores them.
export function setAISessionsApi(api) {
  apiOverride = api ?? null;
}

function bindings() {
  return apiOverride ?? globalThis.window?.go?.main?.App ?? {};
}

async function call(name, ...args) {
  const fn = bindings()[name];
  if (typeof fn !== 'function') throw new Error(`${name} is not available`);
  return fn(...args);
}

// ------------------------------------------------------------------ model

const RELOAD_AFTER_MS = 4000;
const PREV_LINES = 3;
const TAIL_LINES = 20;
const ENV_VALUE_MAX = 64;
const SECRET_NAME = /TOKEN|SECRET|PASSWORD|PASSWD|KEY/i;

function pick(obj, ...names) {
  for (const name of names) {
    if (obj?.[name] != null) return obj[name];
  }
  return undefined;
}

function str(value) {
  return value == null ? '' : String(value);
}

// normalizeAISession maps an AISessionInfo (camelCase from Go, or the
// snake_case of the MCP list_sessions output) to the fields this page uses.
export function normalizeAISession(raw) {
  const s = raw ?? {};
  const closeReason = str(pick(s, 'closeReason', 'close_reason'));
  const closedAt = timeField(pick(s, 'closedAt', 'closed_at'));
  let state = str(pick(s, 'state', 'sessionState', 'session_state'));
  if (!state && (closeReason || closedAt)) state = 'closed';
  return {
    id: str(pick(s, 'id', 'sessionId', 'session_id')),
    agentName: str(pick(s, 'agentName', 'agent', 'owner', 'agent_name')),
    connId: str(pick(s, 'connId', 'connectionId', 'connection_id')),
    connName: str(pick(s, 'connName', 'connectionName', 'hostName', 'connection', 'host_name')),
    mode: str(pick(s, 'mode')) || 'exec',
    shell: str(pick(s, 'shell')),
    state: state || 'idle',
    cwd: str(pick(s, 'cwd')),
    tainted: Boolean(pick(s, 'tainted')),
    reason: str(pick(s, 'reason')),
    openedAt: timeField(pick(s, 'openedAt', 'opened_at')),
    lastActivityAt: timeField(pick(s, 'lastActivityAt', 'last_activity_at')),
    idleExpiresAt: timeField(pick(s, 'idleExpiresAt', 'idle_expires_at')),
    expiresAt: timeField(pick(s, 'expiresAt', 'expires_at')),
    closedAt,
    closeReason,
    lines: Number(pick(s, 'lineCount', 'line_count', 'lines')) || 0,
    pendingRequestId: str(pick(s, 'pendingRequestId', 'pending_request_id')),
    redactions: Number(pick(s, 'redactions')) || 0,
  };
}

export function isAISessionClosed(session) {
  return session.state === 'closed' || Boolean(session.closedAt) || Boolean(session.closeReason);
}

// Go's zero time.Time ("0001-01-01T00:00:00Z") means "not set".
const MIN_TIME_MS = Date.UTC(1971, 0, 1);

function timeOf(value) {
  const ms = Date.parse(value ?? '');
  return Number.isNaN(ms) || ms < MIN_TIME_MS ? 0 : ms;
}

function timeField(value) {
  const text = str(value);
  return timeOf(text) ? text : '';
}

// sortAISessions puts open sessions first (newest first), then the closed
// ones still kept as tombstones (most recently closed first).
export function sortAISessions(list) {
  return [...list].sort((a, b) => {
    const ac = isAISessionClosed(a);
    const bc = isAISessionClosed(b);
    if (ac !== bc) return ac ? 1 : -1;
    const diff = ac ? timeOf(b.closedAt) - timeOf(a.closedAt) : timeOf(b.openedAt) - timeOf(a.openedAt);
    return diff || a.id.localeCompare(b.id);
  });
}

// summarizeAISessions counts open sessions and the ones waiting for a
// person's approval. It takes a session list, or the `aiSessions` object of
// the sessions:changed payload ({open, pendingApproval, enabled?}).
export function summarizeAISessions(list) {
  if (list && !Array.isArray(list) && typeof list === 'object') {
    const summary = {
      open: Math.max(0, Number(list.open) || 0),
      pendingApproval: Math.max(0, Number(list.pendingApproval ?? list.pending_approval) || 0),
    };
    if (typeof list.enabled === 'boolean') summary.enabled = list.enabled;
    return summary;
  }
  let open = 0;
  let pendingApproval = 0;
  for (const raw of Array.isArray(list) ? list : []) {
    const session = normalizeAISession(raw);
    if (isAISessionClosed(session)) continue;
    open += 1;
    if (session.state === 'pending_approval' || session.pendingRequestId) pendingApproval += 1;
  }
  return { open, pendingApproval };
}

// renderSidebarAISessions returns the sidebar line (an HTML string, like the
// rest of sidebar.js) or '' when it should be hidden: no open session and
// the feature is not known to be on (§8.1).
export function renderSidebarAISessions(counts, t) {
  const summary = summarizeAISessions(counts ?? {});
  if (!summary.open && summary.enabled !== true) return '';
  const label = summary.pendingApproval
    ? t('side.aiSessions', { n: summary.open, p: summary.pendingApproval })
    : t('side.aiSessionsOnly', { n: summary.open });
  return `<button type="button" class="side-more side-aisessions" data-action="side-aisessions"><i class="bi bi-robot me-1" aria-hidden="true"></i>${escapeHtml(label)} <span aria-hidden="true">→</span></button>`;
}

export function aiSessionStateLabelKey(code) {
  return SESSION_STATES.includes(code) ? `ais.state.${code}` : 'ais.state.idle';
}

export function aiSessionCloseReasonKey(code) {
  return CLOSE_REASONS.includes(code) ? `ais.close.${code}` : 'ais.close.unknown';
}

export function aiSessionLineStateLabel(code, t) {
  return LINE_STATES.includes(code) ? t(`ais.line.${code}`) : str(code);
}

// aiSessionReasonLabel translates one classification reason code (§3.2).
export function aiSessionReasonLabel(code, t) {
  const value = str(code);
  if (value.startsWith('base:')) return t('ais.reason.base', { risk: value.slice(5) });
  return REASON_CODES.includes(value) ? t(`ais.reason.${value}`) : value;
}

export function closeReasonLabel(code, t) {
  return t(aiSessionCloseReasonKey(code), { code: str(code) || '-' });
}

export function shortSessionId(id) {
  const value = str(id);
  return value.length > 8 ? `${value.slice(0, 8)}…` : value;
}

// formatAge renders how long ago `from` was (minutes granularity).
export function formatAge(from, t, now = Date.now()) {
  const ms = timeOf(from);
  if (!ms) return '-';
  const minutes = Math.floor(Math.max(0, now - ms) / 60000);
  if (minutes < 1) return t('ais.time.lessThanMinute');
  const h = Math.floor(minutes / 60);
  const m = minutes % 60;
  return h > 0 ? t('ais.time.hm', { h, m }) : t('ais.time.m', { m });
}

function formatClock(value) {
  const ms = timeOf(value);
  if (!ms) return '-';
  const date = new Date(ms);
  const pad = (n) => String(n).padStart(2, '0');
  return `${pad(date.getHours())}:${pad(date.getMinutes())}`;
}

function formatTimestamp(value) {
  const ms = timeOf(value);
  if (!ms) return '';
  const date = new Date(ms);
  const pad = (n) => String(n).padStart(2, '0');
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())} ${pad(date.getHours())}:${pad(date.getMinutes())}`;
}

// ------------------------------------------------------------------ state

const ais = {
  list: [],
  loaded: false,
  loadedAt: 0,
  loading: false,
  error: '',
  loadSeq: 0,
  busy: '',
  viewingId: '',
  detail: null,     // AISessionDetail of the viewed session
  detailSeq: 0,
  settings: null,   // AISessionSettings as loaded
  settingsError: '',
  settingsLoading: false,
  settingsOpen: false,
  draft: null,      // edited copy while the form is open
  dirty: false,
};

let mountedRoot = null;
let mountedCtx = null;

// viewer: the one read-only xterm (§8.2). It is kept across page re-renders
// (its host element is moved into the new layout) and disposed when the
// user stops viewing or leaves the page.
let viewer = null;

// ------------------------------------------------------------------ DOM

function el(tag, attrs = {}, ...children) {
  const node = document.createElement(tag);
  for (const [name, value] of Object.entries(attrs)) {
    if (value == null || value === false) continue;
    if (name === 'className') node.className = value;
    else if (name === 'value') node.value = value;
    else if (name === 'checked') node.checked = Boolean(value);
    else node.setAttribute(name, value === true ? '' : String(value));
  }
  for (const child of children.flat()) {
    if (child == null || child === false) continue;
    node.append(typeof child === 'string' || typeof child === 'number' ? document.createTextNode(String(child)) : child);
  }
  return node;
}

function icon(name, extra = 'me-1') {
  return el('i', { className: `bi ${name} ${extra}`.trim(), 'aria-hidden': 'true' });
}

function actionButton(action, label, { className = 'btn-outline-secondary btn-sm', iconName = '', data = {}, disabled = false } = {}) {
  const attrs = { className: `btn ${className}`, type: 'button', 'data-action': action, disabled: Boolean(ais.busy) || disabled };
  for (const [key, value] of Object.entries(data)) attrs[`data-${key}`] = value;
  return el('button', attrs, iconName ? icon(iconName) : null, label);
}

const STATE_SYMBOL = { opening: '◌', idle: '●', pending_approval: '⏳', busy: '▶', closing: '◌', closed: '○' };
const STATE_BADGE = {
  opening: 'text-bg-secondary', idle: 'text-bg-success', pending_approval: 'text-bg-warning',
  busy: 'text-bg-primary', closing: 'text-bg-secondary', closed: 'text-bg-light border',
};

function stateCell(session, t) {
  const closed = isAISessionClosed(session);
  const code = closed ? 'closed' : (SESSION_STATES.includes(session.state) ? session.state : 'idle');
  const label = closed
    ? t('ais.state.closedWith', { reason: closeReasonLabel(session.closeReason, t) })
    : t(aiSessionStateLabelKey(code));
  // Symbol + text + icon: the state never depends on color alone (§6.1).
  return el('span', { className: `badge ${STATE_BADGE[code]} ais-state`, 'data-state': code },
    el('span', { 'aria-hidden': 'true' }, `${STATE_SYMBOL[code]} `),
    icon('bi-robot'),
    label);
}

function sessionRow(session, ctx, now) {
  const { t } = ctx;
  const closed = isAISessionClosed(session);
  const viewing = ais.viewingId === session.id;
  const modeCell = el('td', { className: 'text-nowrap' }, t(`ais.mode.${session.mode === 'shell' ? 'shell' : 'exec'}`));
  if (session.tainted) {
    modeCell.append(' ', el('span', { className: 'badge text-bg-danger ais-tainted', title: t('ais.tainted') },
      el('span', { 'aria-hidden': 'true' }, '⚠ '), t('ais.taintedBadge')));
  }
  const linesCell = el('td', { className: 'text-nowrap' }, String(session.lines));
  if (!closed && session.pendingRequestId) {
    linesCell.append(' ', el('button', {
      className: 'btn btn-link btn-sm p-0 ais-pending-link', type: 'button',
      'data-action': 'ais-open-request', 'data-request-id': session.pendingRequestId,
    }, t('ais.pendingBadge', { n: 1 })));
  }
  const timeCell = closed
    ? el('td', { className: 'text-nowrap', title: formatTimestamp(session.closedAt) }, formatClock(session.closedAt))
    : el('td', { className: 'text-nowrap', title: formatTimestamp(session.expiresAt) }, formatAge(session.openedAt, t, now));
  const actions = el('td', { className: 'text-end text-nowrap' },
    actionButton('ais-view', t('ais.view'), {
      className: viewing ? 'btn-secondary btn-sm' : 'btn-outline-secondary btn-sm',
      iconName: 'bi-eye', data: { 'session-id': session.id },
    }));
  if (!closed) {
    actions.append(' ', actionButton('ais-close', t('ais.forceClose'), {
      className: 'btn-outline-danger btn-sm', iconName: 'bi-x-octagon', data: { 'session-id': session.id },
    }));
  }
  return el('tr', { 'data-ais-row': session.id, className: viewing ? 'table-active' : null, 'aria-current': viewing ? 'true' : null },
    el('td', {}, stateCell(session, t)),
    el('td', { className: 'font-monospace text-nowrap', title: session.id }, shortSessionId(session.id)),
    el('td', {}, session.connName || session.connId || '-'),
    el('td', {}, session.agentName || '-'),
    modeCell,
    el('td', { className: 'font-monospace text-break' }, session.cwd || '-'),
    timeCell,
    linesCell,
    actions,
  );
}

function renderList(ctx, now) {
  const { t } = ctx;
  if (!ais.loaded && ais.loading) return el('p', { className: 'text-secondary mb-0' }, t('ais.loading'));
  if (!ais.list.length) return el('p', { className: 'text-secondary mb-0 ais-empty' }, t('ais.empty'));
  return el('div', { className: 'table-responsive' },
    el('table', { className: 'table table-sm align-middle mb-0 ais-table' },
      el('thead', {}, el('tr', {},
        el('th', {}, t('ais.col.state')),
        el('th', {}, t('ais.col.id')),
        el('th', {}, t('ais.col.connection')),
        el('th', {}, t('ais.col.agent')),
        el('th', {}, t('ais.col.mode')),
        el('th', {}, t('ais.col.cwd')),
        el('th', {}, t('ais.col.time')),
        el('th', {}, t('ais.col.lines')),
        el('th', { className: 'text-end' }, t('ais.col.actions')),
      )),
      el('tbody', {}, ais.list.map((session) => sessionRow(session, ctx, now))),
    ),
  );
}

// detailHistory reads the sent lines out of an AISessionDetail.
function detailHistory(detail) {
  const raw = pick(detail, 'history', 'recentLines', 'lineHistory')
    ?? (Array.isArray(detail?.lines) ? detail.lines : []);
  return (Array.isArray(raw) ? raw : []).map((line) => ({
    seq: Number(pick(line, 'seq', 'sessionSeq')) || 0,
    requestId: str(pick(line, 'requestId', 'request_id', 'id')),
    line: str(pick(line, 'line', 'command', 'requestedCommand')),
    state: str(pick(line, 'state', 'status')),
    exitCode: pick(line, 'exitCode', 'exit_code'),
  }));
}

function detailSession(detail) {
  if (!detail) return null;
  const inner = detail.session ?? detail.info ?? detail;
  return normalizeAISession(inner);
}

function historyItem(line, t) {
  const state = line.state;
  const done = ['succeeded', 'auto_succeeded', 'failed', 'auto_failed', 'interrupted'].includes(state);
  const ok = state === 'succeeded' || state === 'auto_succeeded';
  const symbol = state === 'pending' ? '⏳' : done ? (ok ? '✓' : '✗') : state === 'running' || state === 'auto_running' ? '▶' : '·';
  const item = el('li', { className: 'ais-history-item', 'data-state': state, 'data-seq': line.seq },
    el('span', { className: 'text-secondary' }, `#${line.seq}`),
    ' ',
    el('code', {}, line.line),
    ' ',
    el('span', { 'aria-hidden': 'true' }, `${symbol} `),
    el('span', { className: 'small' },
      aiSessionLineStateLabel(state, t),
      line.exitCode != null && done ? ` (${line.exitCode})` : ''),
  );
  if (state === 'pending' && line.requestId) {
    item.append(' ', el('button', {
      className: 'btn btn-link btn-sm p-0 ais-pending-link', type: 'button',
      'data-action': 'ais-open-request', 'data-request-id': line.requestId,
    }, t('ais.toApproval'), el('span', { 'aria-hidden': 'true' }, ' →')));
  }
  return item;
}

function renderViewer(ctx) {
  const { t } = ctx;
  const id = ais.viewingId;
  const listed = ais.list.find((session) => session.id === id);
  const session = listed ?? detailSession(ais.detail);
  const history = detailHistory(ais.detail);
  const redactions = Number(pick(ais.detail, 'redactions')) || session?.redactions || 0;
  const header = el('div', { className: 'card-header d-flex align-items-center justify-content-between gap-2' },
    el('div', {},
      el('span', { className: 'fw-bold', id: 'ais-viewer-title' }, t('ais.viewing', { id })),
      ' ',
      el('span', { className: 'badge text-bg-secondary ais-readonly' }, icon('bi-lock'), t('ais.readonly')),
    ),
    actionButton('ais-viewer-close', t('ais.viewerClose'), { iconName: 'bi-x-lg' }),
  );
  const body = el('div', { className: 'card-body' });
  if (session?.reason) body.append(el('div', { className: 'small text-secondary mb-2 ais-viewer-reason' }, t('ais.viewerReason', { reason: session.reason })));
  if (session?.tainted) body.append(el('div', { className: 'alert alert-warning py-2 small ais-viewer-tainted' }, el('span', { 'aria-hidden': 'true' }, '⚠ '), t('ais.tainted')));
  if (redactions > 0) body.append(el('div', { className: 'small text-secondary mb-2 ais-redacted' }, icon('bi-shield-lock'), t('ais.redacted', { n: redactions })));
  body.append(
    el('div', { className: 'ais-viewer-slot mb-2', id: 'ais-viewer-slot' }),
    el('div', { className: 'fw-bold small mb-1' }, t('ais.history')),
    history.length
      ? el('ol', { className: 'list-unstyled mb-0 small ais-history' }, history.map((line) => historyItem(line, t)))
      : el('p', { className: 'text-secondary small mb-0' }, t('ais.historyEmpty')),
  );
  return el('section', { className: 'card mt-3 ais-viewer', 'aria-labelledby': 'ais-viewer-title', 'data-session-id': id }, header, body);
}

// ------------------------------------------------------------------ settings

const NUMBER_FIELDS = [
  { key: 'idleMinutes', label: 'ais.settings.idleMinutes', min: 1, max: 120, def: 10 },
  { key: 'maxMinutes', label: 'ais.settings.maxMinutes', min: 5, max: 480, def: 60 },
  { key: 'perAgent', label: 'ais.settings.perAgent', min: 1, max: 8, def: 3 },
  { key: 'total', label: 'ais.settings.total', min: 1, max: 64, def: 16 },
  { key: 'ringKB', label: 'ais.settings.ringKB', min: 64, max: 8192, def: 1024 },
  { key: 'auditKB', label: 'ais.settings.auditKB', min: 0, max: 16384, def: 1024 },
];

export const AI_SESSION_SETTINGS_DEFAULTS = Object.freeze({
  enabled: false,
  idleMinutes: 10,
  maxMinutes: 60,
  perAgent: 3,
  total: 16,
  bypassMaxRisk: 'Low',
  shellConnIds: [],
  ringKB: 1024,
  auditKB: 1024,
});

// normalizeAISessionSettings fills the defaults of §5 for missing fields and
// keeps any field this page does not know, so a save does not drop it.
export function normalizeAISessionSettings(raw) {
  const s = { ...AI_SESSION_SETTINGS_DEFAULTS, ...(raw ?? {}) };
  s.enabled = Boolean(s.enabled);
  s.bypassMaxRisk = s.bypassMaxRisk === 'Medium' ? 'Medium' : 'Low';
  s.shellConnIds = Array.isArray(s.shellConnIds) ? s.shellConnIds.filter(Boolean).map(String) : [];
  return s;
}

// validateAISessionSettings returns {field: message} for out-of-range values.
export function validateAISessionSettings(draft, t) {
  const errors = {};
  for (const field of NUMBER_FIELDS) {
    const raw = draft?.[field.key];
    const value = typeof raw === 'string' && raw.trim() !== '' ? Number(raw) : raw;
    if (typeof value !== 'number' || !Number.isInteger(value) || value < field.min || value > field.max) {
      errors[field.key] = t('ais.settings.range', { min: field.min, max: field.max });
    }
  }
  if (draft && draft.bypassMaxRisk !== 'Low' && draft.bypassMaxRisk !== 'Medium') {
    errors.bypassMaxRisk = t('ais.settings.invalid');
  }
  return errors;
}

function settingsValue(key) {
  return (ais.draft ?? ais.settings ?? AI_SESSION_SETTINGS_DEFAULTS)[key];
}

function renderSettings(ctx) {
  const { t } = ctx;
  const errors = ais.draft ? validateAISessionSettings(ais.draft, t) : {};
  const form = el('form', { className: 'ais-settings-form', 'data-ais-form': 'settings', novalidate: true });
  form.append(
    el('div', { className: 'form-check form-switch mb-1' },
      el('input', { className: 'form-check-input', type: 'checkbox', id: 'ais-set-enabled', 'data-ais-field': 'enabled', checked: settingsValue('enabled'), 'aria-describedby': 'ais-set-enabled-help' }),
      el('label', { className: 'form-check-label fw-bold', for: 'ais-set-enabled' }, t('ais.settings.enabled')),
    ),
    el('div', { className: 'form-text mt-0 mb-3', id: 'ais-set-enabled-help' }, t('ais.settings.enabledHelp')),
  );
  const grid = el('div', { className: 'row g-2 mb-3' });
  for (const field of NUMBER_FIELDS) {
    const id = `ais-set-${field.key}`;
    const error = errors[field.key];
    grid.append(el('div', { className: 'col-sm-6 col-lg-4' },
      el('label', { className: 'form-label small mb-1', for: id }, t(field.label)),
      el('input', {
        className: `form-control form-control-sm${error ? ' is-invalid' : ''}`, type: 'number', id,
        min: field.min, max: field.max, step: 1, inputmode: 'numeric', 'data-ais-field': field.key,
        value: str(settingsValue(field.key)),
      }),
      error ? el('div', { className: 'invalid-feedback' }, error) : null,
    ));
  }
  form.append(grid);
  const risk = settingsValue('bypassMaxRisk');
  form.append(
    el('label', { className: 'form-label small mb-1', for: 'ais-set-bypassMaxRisk' }, t('ais.settings.bypassMaxRisk')),
    el('select', { className: 'form-select form-select-sm mb-1', id: 'ais-set-bypassMaxRisk', 'data-ais-field': 'bypassMaxRisk', 'aria-describedby': 'ais-set-bypass-help' },
      ['Low', 'Medium'].map((value) => el('option', { value, selected: risk === value }, t(`ais.risk.${value}`)))),
    el('div', { className: 'form-text mt-0 mb-3', id: 'ais-set-bypass-help' }, t('ais.settings.bypassHelp')),
  );
  const connections = (ctx.state?.data?.connections ?? []).filter((conn) => conn?.id && conn.kind !== 'serial');
  const allowed = new Set(settingsValue('shellConnIds') ?? []);
  const fieldset = el('fieldset', { className: 'mb-3' },
    el('legend', { className: 'form-label small mb-1' }, t('ais.settings.shellConns')),
    el('div', { className: 'form-text mt-0 mb-1' }, t('ais.settings.shellHelp')));
  if (!connections.length) fieldset.append(el('p', { className: 'text-secondary small mb-0' }, t('ais.settings.noConnections')));
  for (const conn of connections) {
    const id = `ais-set-shell-${conn.id}`;
    fieldset.append(el('div', { className: 'form-check' },
      el('input', { className: 'form-check-input', type: 'checkbox', id, 'data-ais-field': 'shellConnIds', 'data-conn-id': conn.id, checked: allowed.has(conn.id) }),
      el('label', { className: 'form-check-label', for: id }, conn.name || conn.id)));
  }
  form.append(fieldset);
  const invalid = Object.keys(errors).length > 0;
  form.append(el('div', { className: 'd-flex gap-2 justify-content-end' },
    actionButton('ais-settings-cancel', t('ais.settings.cancel'), { disabled: !ais.dirty }),
    actionButton('ais-settings-save', t('ais.settings.save'), { className: 'btn-primary btn-sm', iconName: 'bi-save', disabled: !ais.dirty || invalid }),
  ));
  // Inputs are tracked here (the form is ours), so main.js needs no input
  // delegate for this page.
  const onEdit = (event) => editSetting(event.target);
  form.addEventListener('input', onEdit);
  form.addEventListener('change', onEdit);
  form.addEventListener('submit', (event) => event.preventDefault());
  const body = el('div', { className: 'card-body' });
  if (ais.settingsError) body.append(el('div', { className: 'alert alert-danger ais-settings-error' }, ais.settingsError));
  body.append(ais.settings || ais.draft ? form : el('p', { className: 'text-secondary mb-0' }, t('ais.loading')));
  return el('section', { className: 'card mt-3 ais-settings', 'aria-labelledby': 'ais-settings-title' },
    el('div', { className: 'card-header' }, el('h4', { className: 'card-title h6 fw-bold mb-0', id: 'ais-settings-title' }, t('ais.settings.title'))),
    body);
}

function editSetting(target) {
  const key = target?.dataset?.aisField;
  if (!key) return;
  if (!ais.draft) ais.draft = normalizeAISessionSettings(ais.settings);
  if (key === 'enabled') ais.draft.enabled = Boolean(target.checked);
  else if (key === 'bypassMaxRisk') ais.draft.bypassMaxRisk = target.value;
  else if (key === 'shellConnIds') {
    const ids = new Set(ais.draft.shellConnIds);
    if (target.checked) ids.add(target.dataset.connId);
    else ids.delete(target.dataset.connId);
    ais.draft.shellConnIds = [...ids];
  } else {
    const value = target.value.trim();
    ais.draft[key] = value === '' ? '' : Number(value);
  }
  ais.dirty = true;
  // Redraw only the buttons and validation; a full redraw would steal focus.
  const keepFocus = target.id;
  const selection = typeof target.selectionStart === 'number' ? [target.selectionStart, target.selectionEnd] : null;
  rerender();
  const next = keepFocus ? document.getElementById(keepFocus) : null;
  if (next && document.activeElement !== next) {
    next.focus();
    try {
      if (selection && typeof next.setSelectionRange === 'function') next.setSelectionRange(...selection);
    } catch {
      // number inputs do not support selection ranges in every engine.
    }
  }
}

// ------------------------------------------------------------------ page

// buildAISessionsPage returns the page element for the current module state.
export function buildAISessionsPage(ctx, { now = Date.now() } = {}) {
  const { t } = ctx;
  const counts = summarizeAISessions(ais.list);
  const header = el('div', { className: 'card-header' },
    el('div', { className: 'd-flex align-items-start justify-content-between gap-3 flex-wrap' },
      el('div', {},
        el('h3', { className: 'card-title fw-bold mb-1', id: 'ais-title' }, icon('bi-robot'), t('ais.title')),
        el('div', { className: 'text-secondary small' }, t('ais.subtitle')),
      ),
      el('div', { className: 'd-flex gap-2' },
        actionButton('ais-reload', t('ais.reload'), { iconName: 'bi-arrow-clockwise' }),
        counts.open ? actionButton('ais-close-all', t('ais.forceCloseAll'), { className: 'btn-outline-danger btn-sm', iconName: 'bi-x-octagon' }) : null,
        actionButton('ais-settings-toggle', t('ais.settings'), {
          className: ais.settingsOpen ? 'btn-secondary btn-sm' : 'btn-outline-secondary btn-sm', iconName: 'bi-gear',
        }),
      ),
    ),
  );
  const body = el('div', { className: 'card-body' });
  if (ais.error) body.append(el('div', { className: 'alert alert-danger ais-error' }, ais.error));
  if (ais.settings && !ais.settings.enabled) body.append(el('div', { className: 'alert alert-secondary ais-disabled' }, t('ais.disabled')));
  body.append(renderList(ctx, now));
  const page = el('div', { className: 'ais-page' },
    el('section', { className: 'card card-outline card-info ais-list', 'aria-labelledby': 'ais-title' }, header, body));
  if (ais.viewingId) page.append(renderViewer(ctx));
  if (ais.settingsOpen) page.append(renderSettings(ctx));
  return page;
}

function attachViewer() {
  if (!viewer || !mountedRoot) return;
  const slot = mountedRoot.querySelector('#ais-viewer-slot');
  if (!slot) return;
  if (viewer.host.parentElement !== slot) slot.appendChild(viewer.host);
  fitViewer();
}

function rerender() {
  if (!mountedRoot || !mountedRoot.isConnected || !mountedCtx) return;
  // Detach the xterm host first so replaceChildren does not drop it.
  viewer?.host?.remove();
  mountedRoot.replaceChildren(buildAISessionsPage(mountedCtx));
  attachViewer();
}

// renderAISessionsPage draws the page into root (#page-root) and fetches the
// list and settings unless they are fresh.
export function renderAISessionsPage(root, ctx) {
  if (!root) return;
  mountedRoot = root;
  mountedCtx = ctx;
  rerender();
  if (ais.viewingId && !viewer) {
    startViewer(ais.viewingId);
    loadDetail(ais.viewingId);
  }
  if (!ais.loading && (!ais.loaded || Date.now() - ais.loadedAt > RELOAD_AFTER_MS)) loadAISessions();
  if (!ais.settings && !ais.settingsLoading) loadAISessionSettings();
}

// ------------------------------------------------------------------ data

export async function loadAISessions() {
  const seq = ++ais.loadSeq;
  ais.loading = true;
  rerender();
  try {
    const list = await call('ListAISessions');
    if (seq !== ais.loadSeq) return;
    applyList(list);
    ais.loaded = true;
    ais.loadedAt = Date.now();
    ais.error = '';
  } catch (error) {
    if (seq !== ais.loadSeq) return;
    ais.error = mountedCtx ? mountedCtx.t('ais.loadFailed', { message: String(error?.message ?? error) }) : String(error);
  } finally {
    if (seq === ais.loadSeq) {
      ais.loading = false;
      rerender();
    }
  }
}

function applyList(list) {
  ais.list = sortAISessions((Array.isArray(list) ? list : []).map(normalizeAISession).filter((s) => s.id));
  noteViewerClosed();
}

async function loadDetail(id) {
  const seq = ++ais.detailSeq;
  try {
    const detail = await call('GetAISession', id);
    if (seq !== ais.detailSeq || ais.viewingId !== id) return;
    ais.detail = detail ?? null;
  } catch {
    if (seq !== ais.detailSeq) return;
    ais.detail = null;
  }
  noteViewerClosed();
  rerender();
}

export async function loadAISessionSettings() {
  ais.settingsLoading = true;
  try {
    const settings = await call('GetAISessionSettings');
    ais.settings = normalizeAISessionSettings(settings);
    ais.settingsError = '';
  } catch (error) {
    ais.settingsError = mountedCtx ? mountedCtx.t('ais.settings.loadFailed', { message: String(error?.message ?? error) }) : String(error);
  } finally {
    ais.settingsLoading = false;
    rerender();
  }
}

// onAISessionsChanged handles the aisessions:changed event. A payload that
// carries the list is applied; otherwise the list is fetched again, but only
// while the page is on screen (it is marked stale otherwise).
export function onAISessionsChanged(payload) {
  const list = Array.isArray(payload) ? payload : payload?.sessions;
  if (Array.isArray(list)) {
    ais.loadSeq += 1; // a fetch already in flight is older than this
    applyList(list);
    ais.loaded = true;
    ais.loadedAt = Date.now();
    ais.loading = false;
    rerender();
  } else if (mountedRoot?.isConnected) {
    loadAISessions();
  } else {
    ais.loadedAt = 0;
  }
  if (ais.viewingId && mountedRoot?.isConnected) loadDetail(ais.viewingId);
}

// ------------------------------------------------------------------ viewer

function b64ToBytes(value) {
  if (value instanceof Uint8Array) return value;
  if (Array.isArray(value)) return Uint8Array.from(value);
  const text = str(value);
  if (!text) return new Uint8Array(0);
  const bin = atob(text);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i += 1) out[i] = bin.charCodeAt(i);
  return out;
}

function noticeLine(text) {
  return `\r\n\x1b[2m[${text}]\x1b[0m\r\n`;
}

function viewerT() {
  return mountedCtx?.t ?? viewer?.t ?? ((key, vars) => `${key}${vars ? ` ${JSON.stringify(vars)}` : ''}`);
}

function fitViewer() {
  try {
    viewer?.fit?.fit();
  } catch {
    // Not laid out yet (hidden page, jsdom).
  }
}

// mountAISessionViewer creates the read-only xterm for one session inside
// container, fills it from the raw ring (ReadAISessionRaw(id, 0)) and then
// follows aisession:data. deps: {Terminal, FitAddon, readRaw, t,
// keyHandler} for tests and for main.js's shortcuts. Resolves to the viewer.
export async function mountAISessionViewer(container, sessionId, deps = {}) {
  disposeAISessionViewer();
  const TerminalCtor = deps.Terminal ?? Terminal;
  const FitCtor = deps.FitAddon === undefined ? FitAddon : deps.FitAddon;
  const readRaw = deps.readRaw ?? ((id, since) => call('ReadAISessionRaw', id, since));
  const host = document.createElement('div');
  host.className = 'ais-viewer-term';
  host.dataset.sessionId = sessionId;
  host.style.height = '360px';
  container?.appendChild(host);
  const xterm = new TerminalCtor({
    disableStdin: true,
    cursorBlink: false,
    cursorInactiveStyle: 'none',
    convertEol: true,
    scrollback: 5000,
    fontSize: 13,
    fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Consolas, monospace',
    theme: { background: '#0b1120', foreground: '#d8e2f2' },
  });
  let fit = null;
  if (FitCtor) {
    fit = new FitCtor();
    xterm.loadAddon(fit);
  }
  xterm.open(host);
  // Swallow OSC 52 (clipboard) even though no addon handles it, so a later
  // addon cannot start honoring it here by accident.
  try {
    xterm.parser?.registerOscHandler?.(52, () => true);
  } catch {
    // Older xterm without the parser API.
  }
  if (typeof deps.keyHandler === 'function') xterm.attachCustomKeyEventHandler?.(deps.keyHandler);
  const v = {
    id: sessionId, xterm, fit, host, cursor: null, pending: [], loading: true,
    closedNoted: false, t: deps.t ?? null,
  };
  viewer = v;
  fitViewer();
  try {
    const res = await readRaw(sessionId, 0);
    if (viewer !== v) return v;
    applySnapshot(v, res ?? {});
  } catch (error) {
    if (viewer !== v) return v;
    xterm.write(noticeLine((v.t ?? viewerT())('ais.viewerReadFailed', { message: String(error?.message ?? error) })));
  }
  v.loading = false;
  const queued = v.pending;
  v.pending = [];
  for (const payload of queued) writeData(v, payload);
  noteViewerClosed();
  return v;
}

function applySnapshot(v, res) {
  const t = v.t ?? viewerT();
  const bytes = b64ToBytes(pick(res, 'data', 'raw'));
  const missed = Number(pick(res, 'missedBytes', 'missed_bytes')) || 0;
  const start = Number(pick(res, 'startCursor', 'start_cursor', 'start'));
  if (missed > 0) v.xterm.write(noticeLine(t('ais.missed', { n: missed })));
  if (bytes.length) v.xterm.write(bytes);
  const end = Number(pick(res, 'endCursor', 'end_cursor', 'cursor'));
  if (Number.isFinite(end)) v.cursor = end;
  else if (Number.isFinite(start)) v.cursor = start + bytes.length;
  else v.cursor = null;
}

// writeData writes one aisession:data chunk. `cursor` is the raw cursor of
// the chunk's first byte; chunks already covered by the snapshot are
// skipped or trimmed, and a gap is shown as discarded output.
function writeData(v, payload) {
  const t = v.t ?? viewerT();
  let bytes = b64ToBytes(payload.data);
  const dropped = Number(payload.dropped) || 0;
  const start = Number(pick(payload, 'cursor', 'rawCursor', 'raw_cursor', 'start'));
  if (Number.isFinite(start) && v.cursor != null) {
    const end = start + bytes.length;
    if (end <= v.cursor && bytes.length) return;
    if (start < v.cursor) bytes = bytes.subarray(v.cursor - start);
    else if (start > v.cursor) v.xterm.write(noticeLine(t('ais.missed', { n: start - v.cursor })));
    v.cursor = end;
  } else if (Number.isFinite(start)) {
    v.cursor = start + bytes.length;
  } else if (v.cursor != null) {
    v.cursor += bytes.length;
  }
  if (dropped > 0) v.xterm.write(noticeLine(t('ais.missed', { n: dropped })));
  if (bytes.length) v.xterm.write(bytes);
}

// onAISessionData handles aisession:data: {id, data (base64), cursor?,
// dropped?}. Only the viewed session's chunks are written.
export function onAISessionData(payload) {
  if (!viewer || !payload || payload.id !== viewer.id) return;
  if (viewer.loading) {
    viewer.pending.push(payload);
    return;
  }
  writeData(viewer, payload);
}

function noteViewerClosed() {
  if (!viewer || viewer.loading || viewer.closedNoted) return;
  const session = ais.list.find((s) => s.id === viewer.id) ?? (ais.viewingId === viewer.id ? detailSession(ais.detail) : null);
  if (!session || !isAISessionClosed(session)) return;
  viewer.closedNoted = true;
  const t = viewer.t ?? viewerT();
  viewer.xterm.write(noticeLine(t('ais.viewerClosed', { reason: closeReasonLabel(session.closeReason, t) })));
}

// disposeAISessionViewer drops the viewer xterm. main.js calls it when the
// user leaves the page; coming back reads the ring again (§8.2).
export function disposeAISessionViewer() {
  if (!viewer) return;
  const v = viewer;
  viewer = null;
  try {
    v.xterm.dispose();
  } catch {
    // Already disposed.
  }
  v.host.remove();
}

function startViewer(id) {
  if (!mountedRoot) return;
  const slot = mountedRoot.querySelector('#ais-viewer-slot');
  if (!slot) return;
  mountAISessionViewer(slot, id, { t: mountedCtx?.t, keyHandler: mountedCtx?.terminalKeyHandler });
}

function openViewer(id) {
  if (!id) return;
  if (ais.viewingId === id && viewer?.id === id) return;
  ais.viewingId = id;
  ais.detail = null;
  disposeAISessionViewer();
  rerender();
  startViewer(id);
  loadDetail(id);
}

function closeViewer() {
  ais.viewingId = '';
  ais.detail = null;
  ais.detailSeq += 1;
  disposeAISessionViewer();
  rerender();
}

// ------------------------------------------------------------------ dialog

let dialogChain = Promise.resolve();

function dialogRoot() {
  let root = document.querySelector('#aisessions-modal-root');
  if (!root) {
    root = el('div', { id: 'aisessions-modal-root' });
    document.body.appendChild(root);
  }
  return root;
}

// confirmAISessionDialog shows a modal outside #app and resolves true only
// for the OK button (Cancel, ×, the backdrop and Escape resolve false).
export function confirmAISessionDialog(ctx, { title, body, okLabel }) {
  const { t } = ctx;
  const run = dialogChain.then(() => new Promise((resolve) => {
    const previousFocus = document.activeElement;
    const titleId = 'aisessions-modal-title';
    const cancel = el('button', { className: 'btn btn-outline-secondary', type: 'button', 'data-ais-dialog': 'cancel' }, t('ais.confirm.cancel'));
    const ok = el('button', { className: 'btn btn-danger', type: 'button', 'data-ais-dialog': 'ok' }, okLabel);
    const modal = el('div', { className: 'modal d-block', tabindex: '-1', role: 'dialog', 'aria-modal': 'true', 'aria-labelledby': titleId, 'data-ais-dialog': 'backdrop' },
      el('div', { className: 'modal-dialog modal-dialog-centered' },
        el('div', { className: 'modal-content' },
          el('div', { className: 'modal-header' },
            el('h5', { className: 'modal-title fw-bold', id: titleId }, title),
            el('button', { className: 'btn-close', type: 'button', 'data-ais-dialog': 'close', 'aria-label': t('ais.confirm.close') }),
          ),
          el('div', { className: 'modal-body' }, el('p', { className: 'mb-0' }, body)),
          el('div', { className: 'modal-footer' }, cancel, ok),
        ),
      ),
    );
    const host = el('div', {}, el('div', { className: 'modal-backdrop show' }), modal);
    dialogRoot().appendChild(host);
    let done = false;
    const finish = (value) => {
      if (done) return;
      done = true;
      host.remove();
      if (previousFocus && previousFocus.isConnected && typeof previousFocus.focus === 'function') previousFocus.focus();
      resolve(value);
    };
    host.addEventListener('click', (event) => {
      const target = event.target.closest('[data-ais-dialog]');
      if (!target) return;
      const kind = target.dataset.aisDialog;
      if (kind === 'backdrop' && event.target !== target) return;
      finish(kind === 'ok');
    });
    host.addEventListener('keydown', (event) => {
      if (event.key === 'Escape') {
        event.preventDefault();
        finish(false);
      }
    });
    // Destructive: focus starts on Cancel so Enter does not confirm.
    cancel.focus();
  }));
  dialogChain = run.catch(() => {});
  return run;
}

// ------------------------------------------------------------------ actions

async function runAction(ctx, action, fn) {
  ais.busy = action;
  ais.loadSeq += 1;
  rerender();
  ctx.bumpGen?.();
  let result;
  try {
    result = await fn();
  } catch (error) {
    result = { ok: false, message: String(error?.message ?? error) };
  } finally {
    ais.busy = '';
  }
  if (result?.message) ctx.toast?.(result.message);
  await loadAISessions();
  if (ais.viewingId) loadDetail(ais.viewingId);
  return result ?? { ok: false, message: '' };
}

async function forceClose(ctx, id) {
  const { t } = ctx;
  const ok = await confirmAISessionDialog(ctx, {
    title: t('ais.confirmClose.title', { id: shortSessionId(id) }),
    body: t('ais.confirmClose'),
    okLabel: t('ais.forceClose'),
  });
  if (!ok) return false;
  const result = await runAction(ctx, 'ais-close', () => call('CloseAISession', id));
  return Boolean(result.ok);
}

async function forceCloseAll(ctx) {
  const { t } = ctx;
  const n = summarizeAISessions(ais.list).open;
  if (!n) return false;
  const ok = await confirmAISessionDialog(ctx, {
    title: t('ais.confirmCloseAll.title'),
    body: t('ais.confirmCloseAll.body', { n }),
    okLabel: t('ais.forceCloseAll'),
  });
  if (!ok) return false;
  const result = await runAction(ctx, 'ais-close-all', () => call('CloseAllAISessions'));
  return Boolean(result.ok);
}

async function saveSettings(ctx) {
  if (!ais.draft) return false;
  const errors = validateAISessionSettings(ais.draft, ctx.t);
  if (Object.keys(errors).length) {
    ctx.toast?.(ctx.t('ais.settings.invalid'));
    rerender();
    return false;
  }
  const payload = normalizeAISessionSettings(ais.draft);
  ais.busy = 'ais-settings-save';
  rerender();
  ctx.bumpGen?.();
  let result;
  try {
    result = await call('SaveAISessionSettings', payload);
  } catch (error) {
    result = { ok: false, message: String(error?.message ?? error) };
  } finally {
    ais.busy = '';
  }
  if (result?.message) ctx.toast?.(result.message);
  if (result?.ok) {
    ais.draft = null;
    ais.dirty = false;
    await loadAISessionSettings();
    loadAISessions();
  } else {
    rerender();
  }
  return Boolean(result?.ok);
}

// handleAISessionsClick handles ais-* actions on the page and the sidebar's
// side-aisessions line. Returns true when the click was handled.
export async function handleAISessionsClick(event, ctx) {
  const target = event?.target?.closest?.('[data-action^="ais-"], [data-action="side-aisessions"]');
  if (!target) return false;
  const action = target.dataset.action;
  const id = target.dataset.sessionId ?? '';
  if (action === 'side-aisessions') {
    await ctx.navigate?.('aisessions');
    return true;
  }
  if (ais.busy && action !== 'ais-viewer-close' && action !== 'ais-view') return true;
  switch (action) {
    case 'ais-reload':
      await loadAISessions();
      if (ais.viewingId) loadDetail(ais.viewingId);
      return true;
    case 'ais-view':
      openViewer(id);
      return true;
    case 'ais-viewer-close':
      closeViewer();
      return true;
    case 'ais-close':
      if (id) await forceClose(ctx, id);
      return true;
    case 'ais-close-all':
      await forceCloseAll(ctx);
      return true;
    case 'ais-open-request': {
      const requestId = target.dataset.requestId ?? '';
      if (requestId && ctx.state) ctx.state.selectedRequestId = requestId;
      await ctx.navigate?.('commands');
      return true;
    }
    case 'ais-settings-toggle':
      ais.settingsOpen = !ais.settingsOpen;
      if (!ais.settingsOpen) {
        ais.draft = null;
        ais.dirty = false;
      }
      rerender();
      if (ais.settingsOpen && !ais.settings && !ais.settingsLoading) loadAISessionSettings();
      return true;
    case 'ais-settings-cancel':
      ais.draft = null;
      ais.dirty = false;
      rerender();
      return true;
    case 'ais-settings-save':
      await saveSettings(ctx);
      return true;
    default:
      return false;
  }
}

// aiSessionsBusy is true while the page must not be redrawn under the user:
// an action is running, or the settings form has unsaved edits or focus.
export function aiSessionsBusy() {
  if (ais.busy || ais.dirty) return true;
  const active = document.activeElement;
  return Boolean(active && active.closest?.('.ais-settings-form'));
}

// ------------------------------------------------------------------ approval screen

function normalizeLineContext(raw) {
  const c = raw ?? {};
  const envRaw = pick(c, 'env', 'envOverlay', 'env_overlay') ?? {};
  let env = [];
  if (Array.isArray(envRaw)) {
    env = envRaw.map((item) => ({ name: str(item?.name), value: item?.value, unset: Boolean(item?.unset) }));
  } else if (envRaw && typeof envRaw === 'object') {
    env = Object.entries(envRaw).map(([name, value]) => ({ name, value, unset: value == null }));
  }
  for (const name of pick(c, 'envUnset', 'env_unset') ?? []) env.push({ name: str(name), value: null, unset: true });
  env = env.filter((item) => item.name).sort((a, b) => a.name.localeCompare(b.name));
  const prevRaw = pick(c, 'previousLines', 'prevLines', 'previous', 'recentLines') ?? [];
  const previous = (Array.isArray(prevRaw) ? prevRaw : []).map((line) => ({
    seq: Number(pick(line, 'seq', 'sessionSeq')) || 0,
    line: str(pick(line, 'line', 'command')),
    state: str(pick(line, 'state', 'status')),
    exitCode: pick(line, 'exitCode', 'exit_code'),
  })).slice(-PREV_LINES);
  const tailRaw = pick(c, 'outputTail', 'output_tail', 'outputLines', 'output');
  let tail = Array.isArray(tailRaw) ? tailRaw.map(str) : str(tailRaw).split('\n');
  while (tail.length && tail[tail.length - 1] === '') tail.pop();
  tail = tail.slice(-TAIL_LINES);
  return {
    sessionId: str(pick(c, 'sessionId', 'session_id', 'id')),
    agentName: str(pick(c, 'agentName', 'agent', 'agent_name')),
    mode: str(pick(c, 'mode')) || 'exec',
    seq: Number(pick(c, 'seq', 'sessionSeq')) || 0,
    cwd: str(pick(c, 'cwd')),
    env,
    tainted: Boolean(pick(c, 'tainted')),
    previous,
    tail,
    reasons: (pick(c, 'reasons', 'riskReasons', 'risk_reasons') ?? []).map(str),
    bypassMaxRisk: str(pick(c, 'bypassMaxRisk', 'bypass_max_risk')) || 'Low',
  };
}

// displayEnvValue cuts a value to 64 characters and hides it when the name
// looks like a secret (§3.7). The backend masks too; this is a second guard.
export function displayEnvValue(name, value, t) {
  if (SECRET_NAME.test(str(name))) return t('approval.aiMasked');
  const text = str(value);
  return text.length > ENV_VALUE_MAX ? `${text.slice(0, ENV_VALUE_MAX)}…` : text;
}

// renderAILineContext returns the box the approval screen shows for a line
// of an AI session (§3.7), built from GetAISessionLineContext(requestID).
export function renderAILineContext(lineCtx, t) {
  const c = normalizeLineContext(lineCtx);
  const box = el('section', { className: 'border rounded p-2 mb-2 ai-line-context', 'data-session-id': c.sessionId, 'aria-labelledby': 'ai-line-context-title' });
  box.append(
    el('div', { className: 'fw-bold mb-1', id: 'ai-line-context-title' }, icon('bi-robot'), t('approval.aiLine')),
    el('div', { className: 'small text-secondary mb-2 font-monospace' }, t('approval.aiMeta', {
      id: c.sessionId || '-', agent: c.agentName || '-', mode: c.mode, seq: c.seq || '-',
    })),
  );
  if (c.tainted) box.append(el('div', { className: 'alert alert-warning py-1 px-2 small mb-2 ai-line-tainted' }, el('span', { 'aria-hidden': 'true' }, '⚠ '), t('ais.tainted')));
  if (c.mode === 'shell') box.append(el('div', { className: 'small text-warning-emphasis mb-2 ai-line-shell-note' }, t('approval.aiShellNote')));
  const dl = el('dl', { className: 'row small mb-2' });
  dl.append(el('dt', { className: 'col-sm-3' }, t('approval.aiCwd')), el('dd', { className: 'col-sm-9 font-monospace text-break mb-1 ai-line-cwd' }, c.cwd || '-'));
  const envDd = el('dd', { className: 'col-sm-9 mb-1 ai-line-env' });
  if (!c.env.length) envDd.append(t('approval.aiEnvNone'));
  else {
    envDd.append(el('ul', { className: 'list-unstyled mb-0 font-monospace' }, c.env.map((item) => el('li', {},
      `${item.name}=`, item.unset ? el('span', { className: 'text-secondary' }, t('approval.aiEnvUnset')) : displayEnvValue(item.name, item.value, t)))));
  }
  dl.append(el('dt', { className: 'col-sm-3' }, t('approval.aiEnv')), envDd);
  box.append(dl);
  box.append(el('div', { className: 'small fw-bold mb-1' }, t('approval.aiPrevLines')));
  box.append(c.previous.length
    ? el('ol', { className: 'list-unstyled small mb-2 ai-line-prev' }, c.previous.map((line) => el('li', {},
      el('span', { className: 'text-secondary' }, `#${line.seq}`),
      ' ',
      el('code', {}, line.line),
      ' ',
      el('span', {}, aiSessionLineStateLabel(line.state, t), line.exitCode != null ? ` (${line.exitCode})` : ''))))
    : el('p', { className: 'small text-secondary mb-2' }, t('approval.aiPrevNone')));
  if (c.tail.length) {
    box.append(
      el('div', { className: 'small fw-bold mb-1' }, t('approval.aiOutput')),
      el('pre', { className: 'command-block small mb-2 ai-line-output' }, c.tail.join('\n')),
    );
  }
  if (c.reasons.length) {
    box.append(
      el('div', { className: 'small fw-bold mb-1' }, t('approval.aiReasons')),
      el('ul', { className: 'small mb-2 ai-line-reasons' }, c.reasons.map((code) => el('li', { 'data-reason': code }, aiSessionReasonLabel(code, t)))),
    );
  }
  box.append(
    el('div', { className: 'small text-secondary ai-line-readonly' }, icon('bi-lock'), t('approval.aiReadonly')),
    el('div', { className: 'small text-secondary ai-line-bypass' }, t('approval.aiBypassNote', { risk: c.bypassMaxRisk === 'Medium' ? 'Medium' : 'Low' })),
  );
  return box;
}

// ------------------------------------------------------------------ tests

// getAISessionsState is for tests and the Integrator's checks; do not mutate.
export function getAISessionsState() {
  return ais;
}

export function getAISessionViewer() {
  return viewer;
}

// resetAISessionsState restores the initial module state (tests only).
export function resetAISessionsState() {
  disposeAISessionViewer();
  Object.assign(ais, {
    list: [], loaded: false, loadedAt: 0, loading: false, error: '', loadSeq: 0, busy: '',
    viewingId: '', detail: null, detailSeq: 0, settings: null, settingsError: '',
    settingsLoading: false, settingsOpen: false, draft: null, dirty: false,
  });
  mountedRoot = null;
  mountedCtx = null;
  apiOverride = null;
  dialogChain = Promise.resolve();
  document.querySelector('#aisessions-modal-root')?.remove();
}
