import { describe, expect, it } from 'vitest';
import {
  SERVER_FILTER_STORAGE_KEY,
  countPendingByServer,
  deriveServerView,
  loadServerFilter,
  saveServerFilter,
} from './serverview.js';

// Fixture of phase4-ui-integration.md §5.
const connections = () => [
  { id: 'c-web', name: 'web-01', type: 'SSH' },
  { id: 'c-db', name: 'db-stg', type: 'SSH' },
  { id: 'c-rt', name: 'router-01', type: 'SSH' },
];
const sessions = () => [
  { connId: 'c-web', connName: 'web-01', kind: 'ssh', connState: 'connected', shells: 2, pending: 1 },
  { connId: 'c-db', connName: 'db-stg', kind: 'ssh', connState: 'disconnected', shells: 0, pending: 1 },
  { connId: 'c-rt', connName: 'router-01', kind: 'ssh', connState: 'disconnected', shells: 0, pending: 0 },
];
const requests = () => [
  { id: 'R1', connectionId: 'c-web', status: 'pending', host: 'web-01.local' },
  { id: 'R2', connectionId: 'c-web', status: 'succeeded' },
  { id: 'R3', connectionId: 'c-db', status: 'pending' },
  { id: 'R4', connectionId: '', status: 'pending', host: 'legacy-host' }, // pre-v2 row
  { id: 'R5', connectionId: 'c-gone', status: 'failed' }, // deleted connection
];
const terminalState = () => ({
  tabs: new Map([
    ['T1', { id: 'T1', connId: 'c-web', label: 'web-01 #1', status: 'open', usedSeq: 1 }],
    ['T2', { id: 'T2', connId: 'c-web', label: 'web-01 #2', status: 'open', usedSeq: 2 }],
    ['T3', { id: 'T3', connId: 'c-db', label: 'db-stg #1', status: 'exited', usedSeq: 3 }],
  ]),
  order: ['T1', 'T3', 'T2'],
  activeId: 'T3',
});

function makeState(fields = {}) {
  return {
    serverFilter: '',
    active: 'terminal',
    selectedRequestId: '',
    data: { connections: connections(), sessions: sessions(), requests: requests(), terminals: [] },
    ...fields,
  };
}

const ids = (list) => list.map((r) => r.id);
const tabOf = (view, connId) => view.tabs.tabs.find((tab) => tab.connId === connId);

describe('deriveServerView: approval queue and history', () => {
  it.each([
    ['', { queue: ['R1', 'R3', 'R4'], history: ['R1', 'R2', 'R3', 'R4', 'R5'], hidden: 0 }],
    ['c-web', { queue: ['R1'], history: ['R1', 'R2'], hidden: 2 }],
    ['c-db', { queue: ['R3'], history: ['R3'], hidden: 2 }],
    ['c-rt', { queue: [], history: [], hidden: 3 }],
    // A filter whose connection is gone behaves as 「すべて」.
    ['c-gone', { queue: ['R1', 'R3', 'R4'], history: ['R1', 'R2', 'R3', 'R4', 'R5'], hidden: 0 }],
  ])('filter %j', (serverFilter, want) => {
    const view = deriveServerView(makeState({ serverFilter }), terminalState());
    expect(view.filter).toBe(serverFilter === 'c-gone' ? '' : serverFilter);
    expect(ids(view.queue)).toEqual(want.queue);
    expect(ids(view.history)).toEqual(want.history);
    expect(view.hiddenPending).toBe(want.hidden);
    expect(view.tabs.selected).toBe(view.filter);
  });

  it('「すべて」 keeps unknown records, labels them, and counts them on the All tab', () => {
    const view = deriveServerView(makeState(), terminalState());
    expect([...view.unknownIds].sort()).toEqual(['R4', 'R5']);
    expect(view.tabs.tabs[0].connId).toBe('');
    expect(view.tabs.tabs[0].pending).toBe(3);
    expect(view.tabs.tabs[0].selected).toBe(true);
  });

  it('a specific server shows only its records, never unknown ones', () => {
    const view = deriveServerView(makeState({ serverFilter: 'c-web' }), terminalState());
    expect(ids(view.history)).not.toContain('R4');
    expect(ids(view.history)).not.toContain('R5');
    expect(tabOf(view, 'c-web').pending).toBe(1);
    expect(tabOf(view, 'c-web').selected).toBe(true);
    expect(view.tabs.selected).toBe('c-web');
    expect(view.filterName).toBe('web-01');
  });

  it('selectedRequest never points outside the filter', () => {
    const outside = deriveServerView(makeState({ serverFilter: 'c-web', selectedRequestId: 'R3' }), terminalState());
    expect(outside.selectedRequest.id).toBe('R1');
    const inside = deriveServerView(makeState({ serverFilter: 'c-web', selectedRequestId: 'R2' }), terminalState());
    expect(inside.selectedRequest.id).toBe('R2');
    const unknown = deriveServerView(makeState({ selectedRequestId: 'R4' }), terminalState());
    expect(unknown.selectedRequest.id).toBe('R4');
    const none = deriveServerView(makeState({ serverFilter: 'c-rt', selectedRequestId: 'R1' }), terminalState());
    expect(none.selectedRequest).toBeNull();
  });

  it('tab pending is recounted from requests, not SessionSummary.pending', () => {
    const state = makeState();
    state.data.sessions[0].pending = 9; // stale backend count
    state.data.sessions[2].pending = 4;
    const view = deriveServerView(state, terminalState());
    expect(tabOf(view, 'c-web').pending).toBe(1);
    expect(tabOf(view, 'c-db').pending).toBe(1);
    expect(tabOf(view, 'c-rt').pending).toBe(0);
    expect(tabOf(view, 'c-rt').badges.pending).toBe('');
    expect(view.tabs.tabs[0].pending).toBe(3);
  });

  it('tab symbols come from model.js connStateSymbol', () => {
    const view = deriveServerView(makeState(), terminalState());
    expect(tabOf(view, 'c-web').badges.symbol).toBe('●');
    expect(tabOf(view, 'c-db').badges.symbol).toBe('○');
  });

  it('a connection known to sessions but not connections is not used as a filter', () => {
    const state = makeState({ serverFilter: 'c-new' });
    state.data.sessions.push({ connId: 'c-new', connName: 'new', connState: 'disconnected' });
    const view = deriveServerView(state, terminalState());
    expect(view.filter).toBe('');
    expect(ids(view.history)).toHaveLength(5);
  });

  it('copes with empty state', () => {
    const view = deriveServerView({}, null);
    expect(view.filter).toBe('');
    expect(view.queue).toEqual([]);
    expect(view.history).toEqual([]);
    expect(view.selectedRequest).toBeNull();
    expect(view.subtabs.items).toEqual([]);
    expect(view.activeTermId).toBe('');
    expect(view.stackVisible).toBe(false);
    expect(view.tabs.tabs).toHaveLength(1);
  });
});

describe('deriveServerView: shell sub-tabs', () => {
  it('filter c-web: T1, T2 with the name on the first only; active moves off T3', () => {
    const view = deriveServerView(makeState({ serverFilter: 'c-web' }), terminalState());
    expect(view.subtabs.items.map((i) => i.id)).toEqual(['T1', 'T2']);
    expect(view.subtabs.items.map((i) => i.text)).toEqual(['web-01 #1', '#2']);
    expect(['T1', 'T2']).toContain(view.activeTermId);
    expect(view.activeTermId).toBe('T2'); // most recently used
    expect(view.stackVisible).toBe(true);
    expect(view.subtabs.newShell).toEqual({ connId: 'c-web', name: 'web-01' });
  });

  it('filter 「すべて」: every tab as "name #n", active stays T3', () => {
    const view = deriveServerView(makeState(), terminalState());
    expect(view.subtabs.items.map((i) => i.id)).toEqual(['T1', 'T3', 'T2']);
    expect(view.subtabs.items.map((i) => i.text)).toEqual(['web-01 #1', 'db-stg #1', 'web-01 #2']);
    expect(view.activeTermId).toBe('T3');
    expect(view.subtabs.newShell).toBeNull();
  });

  it('filter c-rt: no tabs, nothing active, stack hidden', () => {
    const view = deriveServerView(makeState({ serverFilter: 'c-rt' }), terminalState());
    expect(view.subtabs.items).toEqual([]);
    expect(view.activeTermId).toBe('');
    expect(view.stackVisible).toBe(false);
    expect(view.subtabs.newShell).toEqual({ connId: 'c-rt', name: 'router-01' });
  });

  it('stack stays hidden off the terminal page', () => {
    const view = deriveServerView(makeState({ serverFilter: 'c-web', active: 'commands' }), terminalState());
    expect(view.activeTermId).toBe('T2');
    expect(view.stackVisible).toBe(false);
  });

  it('follows a rename through sessions, filter stays c-web', () => {
    const state = makeState({ serverFilter: 'c-web' });
    state.data.sessions[0].connName = 'web-01b';
    const view = deriveServerView(state, terminalState());
    expect(view.filter).toBe('c-web');
    expect(view.subtabs.items[0].text).toBe('web-01b #1');
    expect(tabOf(view, 'c-web').name).toBe('web-01b');
    expect(view.filterName).toBe('web-01b');
  });

  it('without usedSeq the last tab in display order wins', () => {
    const ts = terminalState();
    for (const entry of ts.tabs.values()) delete entry.usedSeq;
    const view = deriveServerView(makeState({ serverFilter: 'c-web' }), ts);
    expect(view.activeTermId).toBe('T2');
  });
});

describe('countPendingByServer', () => {
  it('splits pending into connections and unknown', () => {
    expect(countPendingByServer(requests(), connections())).toEqual({
      byConn: { 'c-web': 1, 'c-db': 1 },
      unknown: 1,
      total: 3,
    });
    expect(countPendingByServer(null, null)).toEqual({ byConn: {}, unknown: 0, total: 0 });
  });
});

describe('loadServerFilter / saveServerFilter', () => {
  const memory = (init = {}) => {
    const map = new Map(Object.entries(init));
    return { getItem: (k) => (map.has(k) ? map.get(k) : null), setItem: (k, v) => map.set(k, String(v)), map };
  };
  const throwing = {
    getItem() { throw new Error('denied'); },
    setItem() { throw new Error('denied'); },
  };

  it.each([
    ['stored value', memory({ [SERVER_FILTER_STORAGE_KEY]: 'c-db' }), 'c-db'],
    ['nothing stored', memory(), ''],
    ['stored 「すべて」', memory({ [SERVER_FILTER_STORAGE_KEY]: '' }), ''],
    ['storage throws', throwing, ''],
    ['no storage', null, ''],
    ['non-string value', { getItem: () => 42 }, ''],
  ])('load: %s', (_name, storage, want) => {
    expect(loadServerFilter(storage)).toBe(want);
  });

  it('save writes the key, keeps it for 「すべて」, and swallows errors', () => {
    const storage = memory();
    saveServerFilter(storage, 'c-web');
    expect(storage.map.get('sshgate-server-filter')).toBe('c-web');
    saveServerFilter(storage, '');
    expect(storage.map.has('sshgate-server-filter')).toBe(true);
    expect(storage.map.get('sshgate-server-filter')).toBe('');
    saveServerFilter(storage, null);
    expect(storage.map.get('sshgate-server-filter')).toBe('');
    expect(() => saveServerFilter(throwing, 'c-web')).not.toThrow();
    expect(() => saveServerFilter(null, 'c-web')).not.toThrow();
  });
});
