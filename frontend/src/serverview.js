// Server filter view (phase4-ui-integration.md §1.2, §2.1). Pure functions
// main.js calls with its `state` and getTerminalState(): every screen reads
// the same derived view, so the tab badges, the approval queue, the history
// and the shell sub-tabs can never disagree about what a filter shows.
//
// Nothing here touches the DOM, Wails or module state. The only side effect is
// saveServerFilter writing the Storage it is given.
//
// Matching rules come from serverfilter.js (recordConnId): a record belongs to
// the connection its connectionId names; a connectionId that is no longer
// saved is 「不明な接続先」; a record with no connectionId (pre-v2 row) is
// matched by connection name only when exactly one SSH connection has it.

import { ALL_SERVERS, buildServerTabs, buildShellSubtabs, normalizeServerFilter } from './servertabs.js';
import { connectionIndex, filterByServer, recordConnId } from './serverfilter.js';
import { connStateSymbol } from './model.js';

export const SERVER_FILTER_STORAGE_KEY = 'sshgate-server-filter';

// loadServerFilter returns the saved connId, or '' (「すべて」) when nothing
// usable is stored or the Storage throws (private mode, blocked site data).
// The value is not validated here: the connection list is not loaded yet at
// startup, so main.js normalizes it after the first refresh.
export function loadServerFilter(storage) {
  try {
    const value = storage?.getItem?.(SERVER_FILTER_STORAGE_KEY);
    return typeof value === 'string' ? value.trim() : ALL_SERVERS;
  } catch (error) {
    return ALL_SERVERS;
  }
}

// saveServerFilter stores the connId; 「すべて」 is stored as '' (the key is
// kept). Storage errors are swallowed, like setLang in main.js.
export function saveServerFilter(storage, connId) {
  try {
    storage?.setItem?.(SERVER_FILTER_STORAGE_KEY, typeof connId === 'string' ? connId : ALL_SERVERS);
  } catch (error) {
    // Storage unavailable; the filter just is not remembered.
  }
}

// countPendingByServer counts pending requests per connection with the same
// rule as filterByServer, so a tab's ⏳ always equals its queue length.
export function countPendingByServer(requests, connections) {
  const index = connections && connections.byId instanceof Map ? connections : connectionIndex(connections);
  const byConn = {};
  let unknown = 0;
  let total = 0;
  for (const request of requests ?? []) {
    if (request?.status !== 'pending') continue;
    total += 1;
    const id = recordConnId(request, index);
    if (id === null) unknown += 1;
    else byConn[id] = (byConn[id] ?? 0) + 1;
  }
  return { byConn, unknown, total };
}

// effectiveFilter accepts the filter only when the connection is both in
// sessions (what the tabs are built from) and in connections (what records
// are matched against). The two lists normally hold the same set; right
// after a save or delete one can lag, and a filter only one side knows would
// make the tab say "web-01" while filterByServer showed everything.
function effectiveFilter(serverFilter, sessions, index) {
  const filter = normalizeServerFilter(serverFilter, sessions);
  return filter && index.byId.has(filter) ? filter : ALL_SERVERS;
}

function pickActiveTermId(terminalState, items) {
  if (!items.length) return '';
  const activeId = terminalState?.activeId ?? '';
  if (activeId && items.some((item) => item.id === activeId)) return activeId;
  const tabs = terminalState?.tabs instanceof Map ? terminalState.tabs : new Map();
  // Most recently used (F1 usedSeq); ties and tabs never used go to the last
  // in display order.
  let best = items[items.length - 1];
  let bestSeq = Number(tabs.get(best.id)?.usedSeq) || 0;
  for (const item of items) {
    const seq = Number(tabs.get(item.id)?.usedSeq) || 0;
    if (seq > bestSeq) {
      best = item;
      bestSeq = seq;
    }
  }
  return best.id;
}

/**
 * deriveServerView builds everything the screens draw for state.serverFilter.
 * @param state main.js state ({ serverFilter, active, selectedRequestId, data })
 * @param terminalState getTerminalState() of terminal-tabs.js
 * @returns {{
 *   filter: string, filterName: string,
 *   tabs: ReturnType<typeof buildServerTabs>,
 *   queue: object[], history: object[], selectedRequest: object | null,
 *   hiddenPending: number, unknownIds: Set<string>,
 *   subtabs: ReturnType<typeof buildShellSubtabs>,
 *   stackVisible: boolean, activeTermId: string,
 * }}
 */
export function deriveServerView(state, terminalState) {
  const data = state?.data ?? {};
  const sessions = (data.sessions ?? []).filter(Boolean);
  const connections = data.connections ?? [];
  const requests = (data.requests ?? []).filter(Boolean);
  const index = connectionIndex(connections);
  const filter = effectiveFilter(state?.serverFilter, sessions, index);

  const counts = countPendingByServer(requests, index);
  // SessionSummary.pending can lag the request list (sessions:changed is not
  // sent on approval decisions), so the badge is recounted from requests.
  const sessionsWithPending = sessions.map((s) => ({ ...s, pending: counts.byConn[s.connId] ?? 0 }));
  const tabs = buildServerTabs(sessionsWithPending, {
    selected: filter,
    extraPending: counts.unknown,
    symbolOf: connStateSymbol,
  });

  const history = filterByServer(requests, filter, index);
  const queue = history.filter((request) => request.status === 'pending');
  // Never select a request outside the filter: the approve button acts on
  // the selection, and a hidden request must not be approved by mistake.
  const selectedId = state?.selectedRequestId ?? '';
  const selectedRequest = (selectedId && history.find((request) => request.id === selectedId)) || queue[0] || null;

  const unknownIds = new Set(
    requests.filter((request) => recordConnId(request, index) === null).map((request) => request.id),
  );

  const subtabs = buildShellSubtabs(terminalState, {
    serverFilter: filter,
    sessions,
    terminals: data.terminals ?? [],
  });
  const activeTermId = pickActiveTermId(terminalState, subtabs.items);

  let filterName = '';
  if (filter) {
    filterName = sessions.find((s) => s.connId === filter)?.connName
      || index.byId.get(filter)?.name
      || filter;
  }

  return {
    filter,
    filterName: String(filterName),
    tabs,
    queue,
    history,
    selectedRequest,
    hiddenPending: counts.total - queue.length,
    unknownIds,
    subtabs,
    stackVisible: state?.active === 'terminal' && activeTermId !== '',
    activeTermId,
  };
}
