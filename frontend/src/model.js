// Pure view-model helpers shared by every frontend module (contract §2.6).
// Nothing here touches the DOM, Wails, or module state, so each function can
// be unit tested with plain values.

// MODEL_I18N holds the state labels of design §6.3 under the `state.` prefix.
export const MODEL_I18N = {
  ja: {
    'state.save.saved': '保存済み',
    'state.save.modified': '未保存の変更あり',
    'state.save.new': '未保存（新規）',
    'state.conn.connected': '接続中',
    'state.conn.connecting': '接続処理中…',
    'state.conn.idle': '待機中',
    'state.conn.disconnected': '未接続',
    'state.conn.error': '接続エラー',
    'state.conn.in_use': '使用中',
    'state.conn.available': '利用可能',
    'state.conn.missing': '見つかりません',
    'state.test.label': '最終テスト',
    'state.test.ok': '成功',
    'state.test.failed': '失敗',
    'state.test.never': '未確認',
    'state.req.pending': '承認待ち',
    'state.req.running': '実行中',
    'state.req.succeeded': '完了',
    'state.req.failed': '失敗',
    'state.req.rejected': '拒否',
    'state.req.expired': '期限切れ',
    'state.req.interrupted': '中断（結果不明）',
    'state.req.auto_running': '自動実行中',
    'state.req.auto_succeeded': '自動実行済み',
    'state.req.auto_failed': '自動実行失敗',
  },
  en: {
    'state.save.saved': 'Saved',
    'state.save.modified': 'Unsaved changes',
    'state.save.new': 'Not saved yet',
    'state.conn.connected': 'Connected',
    'state.conn.connecting': 'Connecting…',
    'state.conn.idle': 'Idle',
    'state.conn.disconnected': 'Not connected',
    'state.conn.error': 'Connection error',
    'state.conn.in_use': 'In use',
    'state.conn.available': 'Available',
    'state.conn.missing': 'Not found',
    'state.test.label': 'Last test',
    'state.test.ok': 'OK',
    'state.test.failed': 'Failed',
    'state.test.never': 'Not checked',
    'state.req.pending': 'Awaiting approval',
    'state.req.running': 'Running',
    'state.req.succeeded': 'Completed',
    'state.req.failed': 'Failed',
    'state.req.rejected': 'Rejected',
    'state.req.expired': 'Expired',
    'state.req.interrupted': 'Interrupted',
    'state.req.auto_running': 'Auto-running',
    'state.req.auto_succeeded': 'Auto-completed',
    'state.req.auto_failed': 'Auto-failed',
  },
};

const SAVE_STATES = ['saved', 'modified', 'new'];
const CONN_STATES = ['connected', 'connecting', 'idle', 'disconnected', 'error', 'in_use', 'available', 'missing'];
const REQUEST_STATES = [
  'pending', 'running', 'succeeded', 'failed', 'rejected', 'expired', 'interrupted',
  'auto_running', 'auto_succeeded', 'auto_failed',
];

// Rows the sidebar lists even without a running command (contract §2.6).
const VISIBLE_CONN_STATES = new Set(['connected', 'in_use', 'connecting', 'error']);

export function escapeHtml(value) {
  return String(value ?? '')
    .replaceAll('&', '&amp;')
    .replaceAll('<', '&lt;')
    .replaceAll('>', '&gt;')
    .replaceAll('"', '&quot;')
    .replaceAll("'", '&#039;');
}

// interpolate replaces `{name}` with vars.name. Placeholders without a value
// are left as written so a missing variable is visible instead of blank.
export function interpolate(template, vars) {
  const text = String(template ?? '');
  if (!vars) return text;
  return text.replace(/\{(\w+)\}/g, (match, name) => (
    Object.hasOwn(vars, name) && vars[name] != null ? String(vars[name]) : match
  ));
}

// mergeI18n combines `{ ja, en }` fragments. A key defined twice, or a key
// present in only one language of a fragment, throws so the mistake shows up
// at startup (and in tests) rather than as a raw key on screen.
export function mergeI18n(...dicts) {
  const merged = { ja: {}, en: {} };
  dicts.forEach((dict, index) => {
    const ja = dict?.ja ?? {};
    const en = dict?.en ?? {};
    const onlyJa = Object.keys(ja).filter((key) => !Object.hasOwn(en, key));
    const onlyEn = Object.keys(en).filter((key) => !Object.hasOwn(ja, key));
    if (onlyJa.length || onlyEn.length) {
      throw new Error(`mergeI18n: dictionary #${index} has ja/en mismatch: ${[...onlyJa, ...onlyEn].join(', ')}`);
    }
    for (const key of Object.keys(ja)) {
      if (Object.hasOwn(merged.ja, key)) {
        throw new Error(`mergeI18n: duplicate key "${key}" in dictionary #${index}`);
      }
      merged.ja[key] = ja[key];
      merged.en[key] = en[key];
    }
  });
  return merged;
}

function parseTime(value) {
  if (!value) return null;
  const ms = Date.parse(value);
  return Number.isNaN(ms) ? null : ms;
}

function compareText(a, b) {
  return String(a ?? '').localeCompare(String(b ?? ''));
}

// sortSessionsForSidebar orders sidebar rows: errors, then rows with running
// commands, then the most recently connected, then by name (design §6.1).
// It returns a new array and leaves the input untouched.
export function sortSessionsForSidebar(sessions) {
  return [...(sessions ?? [])].sort((a, b) => {
    const errorRank = Number(b.connState === 'error') - Number(a.connState === 'error');
    if (errorRank) return errorRank;
    const runningRank = Number((b.running ?? 0) > 0) - Number((a.running ?? 0) > 0);
    if (runningRank) return runningRank;
    const since = (parseTime(b.connectedSince) ?? -Infinity) - (parseTime(a.connectedSince) ?? -Infinity);
    if (since && !Number.isNaN(since)) return since;
    return compareText(a.connName, b.connName) || compareText(a.connId, b.connId);
  });
}

// summarizeSessions turns DashboardData.sessions into what the sidebar draws.
export function summarizeSessions(sessions, { now = Date.now(), limit = 5 } = {}) {
  const list = sessions ?? [];
  const visible = sortSessionsForSidebar(
    list.filter((s) => VISIBLE_CONN_STATES.has(s.connState) || (s.running ?? 0) > 0),
  );
  const rows = visible.slice(0, Math.max(0, limit)).map((s) => {
    const since = parseTime(s.connectedSince);
    return {
      connId: s.connId,
      name: s.connName,
      kind: s.kind,
      connState: s.connState,
      symbol: connStateSymbol(s.connState),
      shells: s.shells ?? 0,
      connecting: s.connecting ?? 0,
      running: s.running ?? 0,
      pending: s.pending ?? 0,
      // Clamp to zero so a clock skew between Go and the WebView never shows
      // a negative duration.
      elapsedMs: since == null ? null : Math.max(0, now - since),
      label: s.label ?? '',
    };
  });
  return {
    connected: list.filter((s) => s.connState === 'connected' || s.connState === 'in_use').length,
    saved: list.length,
    errors: list.filter((s) => s.connState === 'error').length,
    running: list.reduce((sum, s) => sum + (s.running ?? 0), 0),
    rows,
    more: visible.length - rows.length,
  };
}

// usesPassword mirrors Connection.usesPassword in app.go.
function usesPassword(conn) {
  return String(conn?.authMethod ?? '').includes('パスワード');
}

function text(value) {
  return String(value ?? '').trim();
}

function num(value) {
  const n = Number(value ?? 0);
  return Number.isFinite(n) ? n : 0;
}

function tagList(value) {
  const items = Array.isArray(value) ? value : String(value ?? '').split(',');
  return items.map((tag) => String(tag ?? '').trim()).filter(Boolean);
}

// editableSnapshot normalizes the user-editable fields of a connection so two
// values can be compared with JSON equality. Fields that do not apply to the
// transport are left out, and so is a stored password, which the backend
// never sends back.
export function editableSnapshot(conn) {
  const type = conn?.type === 'Serial' ? 'Serial' : 'SSH';
  const snap = {
    name: text(conn?.name),
    type,
    port: num(conn?.port),
    connectTimeout: num(conn?.connectTimeout),
    commandTimeout: num(conn?.commandTimeout),
    tags: tagList(conn?.tags),
    description: text(conn?.description),
  };
  if (type === 'Serial') {
    snap.serialPort = text(conn?.serialPort);
    snap.baudRate = num(conn?.baudRate);
  } else {
    snap.host = text(conn?.host);
    snap.user = text(conn?.user);
    snap.authMethod = text(conn?.authMethod);
    if (!usesPassword(conn)) snap.credential = text(conn?.credential);
  }
  return snap;
}

// connectionDirtyState is the saveState axis of design §5.4.
export function connectionDirtyState(draft, saved) {
  if (draft == null) return 'saved';
  if (!draft.id) return 'new';
  if (!saved) return 'modified';
  if (JSON.stringify(editableSnapshot(draft)) !== JSON.stringify(editableSnapshot(saved))) return 'modified';
  // A typed password is always a change: the saved side has no value to compare.
  if ((draft.type ?? 'SSH') !== 'Serial' && usesPassword(draft) && String(draft.credential ?? '') !== '') {
    return 'modified';
  }
  return 'saved';
}

export function saveStateLabelKey(code) {
  return `state.save.${SAVE_STATES.includes(code) ? code : 'saved'}`;
}

// Unknown codes fall back to "Not connected" rather than showing a raw key.
export function connStateLabelKey(code) {
  return `state.conn.${CONN_STATES.includes(code) ? code : 'disconnected'}`;
}

export function connStateSymbol(code) {
  if (code === 'connected' || code === 'idle') return '●';
  if (code === 'error' || code === 'missing') return '▲';
  if (code === 'in_use') return '◆';
  return '○';
}

// requestStateLabelKey keeps unknown codes in the key so callers can detect a
// missing translation (t() returns the key itself) and show the raw code.
export function requestStateLabelKey(code) {
  return `state.req.${code}`;
}

export function isKnownRequestState(code) {
  return REQUEST_STATES.includes(code);
}

// formatElapsed renders a connection age for the sidebar and editor badge.
export function formatElapsed(ms, lang) {
  if (ms == null || !Number.isFinite(Number(ms))) return '';
  const en = lang === 'en';
  const minutes = Math.floor(Math.max(0, Number(ms)) / 60000);
  if (minutes < 1) return en ? '<1m' : '1分未満';
  if (minutes < 60) return en ? `${minutes}m` : `${minutes}分`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) {
    const rest = minutes % 60;
    if (en) return rest ? `${hours}h ${rest}m` : `${hours}h`;
    return rest ? `${hours}時間${rest}分` : `${hours}時間`;
  }
  const days = Math.floor(hours / 24);
  return en ? `${days}d` : `${days}日`;
}

function clockTime(value) {
  const ms = parseTime(value);
  if (ms == null) return '';
  const date = new Date(ms);
  const pad = (n) => String(n).padStart(2, '0');
  return `${pad(date.getHours())}:${pad(date.getMinutes())}`;
}

// lastTestLabel renders "最終テスト: 成功 10:32" from the stored lastTest*
// fields. The time is local wall-clock HH:MM and is dropped if unparsable.
export function lastTestLabel(conn, t) {
  const result = conn?.lastTestResult;
  if (result !== 'ok' && result !== 'failed') {
    return `${t('state.test.label')}: ${t('state.test.never')}`;
  }
  const outcome = t(result === 'ok' ? 'state.test.ok' : 'state.test.failed');
  const time = clockTime(conn?.lastTestAt);
  return `${t('state.test.label')}: ${outcome}${time ? ` ${time}` : ''}`;
}
