// Pure helpers for the global server tabs (design §6.4, §7 phase 4). Nothing
// here touches the DOM, Wails, or module state; callers pass
// state.data.connections / requests / sessions and state.serverFilter.

// ALL_SERVERS is the serverFilter value of the 「すべて」 tab.
export const ALL_SERVERS = '';

export const SERVERFILTER_I18N = {
  ja: {
    'server.all': 'すべて',
    'server.unknown': '不明な接続先',
  },
  en: {
    'server.all': 'All',
    'server.unknown': 'Unknown connection',
  },
};

function text(value) {
  return String(value ?? '').trim();
}

// connectionIndex builds the lookups recordConnId needs. Serial connections
// are left out of the name lookup: they cannot run commands, so a record can
// never belong to one by name (same rule as the backend's Host match). A name
// shared by two SSH connections maps to null so it is never guessed.
export function connectionIndex(connections) {
  const byId = new Map();
  const byName = new Map();
  for (const conn of connections ?? []) {
    const id = text(conn?.id);
    if (id) byId.set(id, conn);
    const name = text(conn?.name);
    if (!id || !name || conn?.type === 'Serial') continue;
    byName.set(name, byName.has(name) ? null : id);
  }
  return { byId, byName };
}

function asIndex(connections) {
  return connections && connections.byId instanceof Map ? connections : connectionIndex(connections);
}

// recordConnId resolves which saved connection a command request belongs to,
// or null for 「不明な接続先」. A connectionId that is set but no longer saved
// stays unknown instead of falling back to the name, so a new connection
// reusing a deleted one's name does not inherit its history. Only records
// with no connectionId (pre-v2 rows the migration could not match) are
// matched by host name. `connections` is an array or a connectionIndex().
export function recordConnId(record, connections) {
  const index = asIndex(connections);
  const id = text(record?.connectionId);
  if (id) return index.byId.has(id) ? id : null;
  return index.byName.get(text(record?.host)) ?? null;
}

// normalizeServerFilter turns a stale filter (the tab's connection was
// deleted) back into 「すべて」 so no screen ends up filtered to nothing.
export function normalizeServerFilter(serverFilter, connections) {
  const id = text(serverFilter);
  if (!id) return ALL_SERVERS;
  return asIndex(connections).byId.has(id) ? id : ALL_SERVERS;
}

// filterByServer narrows approval-queue / history records to the selected
// server. 「すべて」 returns every record, including those whose connection
// is unknown; a specific server returns only records that resolve to it, so
// unknown records are never hidden from 「すべて」. Order is preserved and
// the records themselves are returned unchanged.
export function filterByServer(records, serverFilter, connections) {
  const index = asIndex(connections);
  const list = [...(records ?? [])];
  const filter = normalizeServerFilter(serverFilter, index);
  if (filter === ALL_SERVERS) return list;
  return list.filter((record) => recordConnId(record, index) === filter);
}

// isUnknownConnection is true when the 「すべて」 view should label a record
// with 'server.unknown'.
export function isUnknownConnection(record, connections) {
  return recordConnId(record, connections) === null;
}

function serverSymbol(code) {
  // Same mapping as connStateSymbol in model.js (design §6.1).
  if (code === 'connected' || code === 'idle') return '●';
  if (code === 'error' || code === 'missing') return '▲';
  if (code === 'in_use') return '◆';
  return '○';
}

// serverTabs builds the top tab strip: the 「すべて」 tab first, then one tab
// per saved connection in the given order. Badges are connState (symbol),
// open shells (⧉) and pending approvals (⏳). Pending is counted from
// `requests` with the same rule as filterByServer, so a badge always equals
// the length of that tab's filtered approval queue; the 「すべて」 tab also
// counts pending records of unknown connections (`unknownPending`).
// `sessions` is DashboardData.sessions; a connection without a session row is
// reported as disconnected.
export function serverTabs(connections, sessions, requests, serverFilter = ALL_SERVERS) {
  const conns = connections ?? [];
  const index = connectionIndex(conns);
  const active = normalizeServerFilter(serverFilter, index);
  const sessionById = new Map((sessions ?? []).map((s) => [text(s?.connId), s]));

  const pendingById = new Map();
  let unknownPending = 0;
  let totalPending = 0;
  for (const request of requests ?? []) {
    if (request?.status !== 'pending') continue;
    totalPending += 1;
    const id = recordConnId(request, index);
    if (id === null) unknownPending += 1;
    else pendingById.set(id, (pendingById.get(id) ?? 0) + 1);
  }

  const tabs = [];
  const seen = new Set();
  let totalShells = 0;
  for (const conn of conns) {
    const connId = text(conn?.id);
    if (!connId || seen.has(connId)) continue;
    seen.add(connId);
    const session = sessionById.get(connId);
    const connState = session?.connState || 'disconnected';
    const shells = session?.shells ?? 0;
    totalShells += shells;
    tabs.push({
      connId,
      name: text(conn?.name) || text(session?.connName),
      kind: conn?.type === 'Serial' ? 'serial' : 'ssh',
      connState,
      symbol: serverSymbol(connState),
      shells,
      connecting: session?.connecting ?? 0,
      pending: pendingById.get(connId) ?? 0,
      active: active === connId,
    });
  }

  return [{
    connId: ALL_SERVERS,
    labelKey: 'server.all',
    shells: totalShells,
    pending: totalPending,
    unknownPending,
    active: active === ALL_SERVERS,
  }, ...tabs];
}
