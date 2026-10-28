// Shared fixture of phase4-ui-integration.md §5 (main.phase4.test.js).
// Three connections, one with two shells; requests that cover a matched
// pending (R1), a matched finished (R2), another server (R3), a legacy row
// without connectionId (R4) and a deleted connection (R5).

export const conn = (id, name, extra = {}) => ({
  id, name, type: 'SSH', host: `${name}.local`, port: 22, user: 'u', authMethod: 'SSH鍵認証',
  credential: '~/.ssh/id', tags: [], description: '', connectTimeout: 10, commandTimeout: 30, ...extra,
});

export const session = (connId, connName, extra = {}) => ({
  connId, connName, kind: 'ssh', connState: 'disconnected', shells: 0, connecting: 0, running: 0, pending: 0, ...extra,
});

export const request = (id, connectionId, status, extra = {}) => ({
  id, connectionId, status, host: connectionId ? `${connectionId}-host` : 'legacy-host', user: 'u',
  command: `echo ${id}`, executedCommand: '', reason: '', requestedBy: 'agent',
  requestedAt: '2026-10-27T10:00:00Z', risk: 'low', ...extra,
});

export const TERMINALS = [
  { id: 'TERM-T1', connId: 'c-web', connName: 'web-01', kind: 'ssh', startedAt: '2026-10-27T10:00:00Z', status: 'open' },
  { id: 'TERM-T3', connId: 'c-db', connName: 'db-stg', kind: 'ssh', startedAt: '2026-10-27T10:01:00Z', status: 'open' },
  { id: 'TERM-T2', connId: 'c-web', connName: 'web-01', kind: 'ssh', startedAt: '2026-10-27T10:02:00Z', status: 'open' },
];

export function phase4Dashboard(seq = 1) {
  return {
    connections: [conn('c-web', 'web-01'), conn('c-db', 'db-stg'), conn('c-rt', 'router-01')],
    requests: [
      request('R1', 'c-web', 'pending', { host: 'web-01.local' }),
      request('R2', 'c-web', 'succeeded'),
      request('R3', 'c-db', 'pending'),
      request('R4', '', 'pending', { host: 'legacy-host' }),
      request('R5', 'c-gone', 'failed'),
    ],
    agentPolicies: [],
    agentTokens: [],
    mcp: { status: '停止中', port: 8787 },
    storageError: '',
    keychainPending: 0,
    sessions: [
      session('c-web', 'web-01', { connState: 'connected', shells: 2, connectedSince: '2026-10-27T10:00:00Z' }),
      session('c-db', 'db-stg', { connState: 'connected', shells: 1 }),
      session('c-rt', 'router-01'),
    ],
    terminals: TERMINALS,
    runningTotal: 0,
    sessionsSeq: seq,
  };
}
