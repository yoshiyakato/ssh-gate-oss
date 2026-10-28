import { describe, it, test, assert } from 'vitest';
import {
  ALL_SERVERS,
  SERVERFILTER_I18N,
  connectionIndex,
  recordConnId,
  normalizeServerFilter,
  filterByServer,
  isUnknownConnection,
  serverTabs,
} from './serverfilter.js';

const CONNS = [
  { id: 'c-web', name: 'web-01', type: 'SSH' },
  { id: 'c-db', name: 'db-stg', type: 'SSH' },
  { id: 'c-usb', name: 'usb-serial', type: 'Serial' },
  { id: 'c-dup1', name: 'dup', type: 'SSH' },
  { id: 'c-dup2', name: 'dup', type: 'SSH' },
];

const REQS = [
  { id: 'r1', connectionId: 'c-web', host: 'web-01', status: 'pending' },
  { id: 'r2', connectionId: 'c-db', host: 'db-stg', status: 'succeeded' },
  { id: 'r3', connectionId: 'c-gone', host: 'old-box', status: 'pending' },
  { id: 'r4', connectionId: '', host: 'web-01', status: 'failed' },
  { id: 'r5', connectionId: '', host: 'usb-serial', status: 'pending' },
  { id: 'r6', connectionId: '', host: 'dup', status: 'rejected' },
  { id: 'r7', host: 'nowhere', status: 'expired' },
  { id: 'r8', connectionId: 'c-gone', host: 'web-01', status: 'pending' },
  { id: 'r9', connectionId: 'c-web', host: 'renamed-later', status: 'pending' },
];

describe('recordConnId', () => {
  const cases = [
    ['by connectionId', REQS[0], 'c-web'],
    ['deleted connectionId stays unknown', REQS[2], null],
    ['deleted id does not fall back to name', REQS[7], null],
    ['id wins over stale host name', REQS[8], 'c-web'],
    ['legacy row by name', REQS[3], 'c-web'],
    ['serial excluded from name match', REQS[4], null],
    ['ambiguous name is unknown', REQS[5], null],
    ['no id no match', REQS[6], null],
    ['trims whitespace', { connectionId: ' c-db ' }, 'c-db'],
    ['empty record', {}, null],
    ['null record', null, null],
  ];
  for (const [name, record, want] of cases) {
    it(name, () => {
      assert.strictEqual(recordConnId(record, CONNS), want);
      assert.strictEqual(recordConnId(record, connectionIndex(CONNS)), want);
      assert.strictEqual(isUnknownConnection(record, CONNS), want === null);
    });
  }
});

test('recordConnId: serial matched by connectionId still resolves', () => {
  assert.strictEqual(recordConnId({ connectionId: 'c-usb', host: 'usb-serial' }, CONNS), 'c-usb');
});

describe('normalizeServerFilter', () => {
  const cases = [
    ['', ALL_SERVERS],
    [null, ALL_SERVERS],
    [undefined, ALL_SERVERS],
    ['c-web', 'c-web'],
    [' c-db ', 'c-db'],
    ['c-gone', ALL_SERVERS],
    ['web-01', ALL_SERVERS],
  ];
  for (const [input, want] of cases) {
    it(JSON.stringify(input), () => {
      assert.strictEqual(normalizeServerFilter(input, CONNS), want);
    });
  }
});

describe('filterByServer', () => {
  const ids = (list) => list.map((r) => r.id);
  const cases = [
    ['all keeps everything incl. unknown', ALL_SERVERS, REQS.map((r) => r.id)],
    ['web-01', 'c-web', ['r1', 'r4', 'r9']],
    ['db-stg', 'c-db', ['r2']],
    ['serial has none by name', 'c-usb', []],
    ['ambiguous name owners get nothing', 'c-dup1', []],
    ['stale filter shows all', 'c-gone', REQS.map((r) => r.id)],
  ];
  for (const [name, filter, want] of cases) {
    it(name, () => {
      assert.deepStrictEqual(ids(filterByServer(REQS, filter, CONNS)), want);
    });
  }
});

test('filterByServer: unknown records appear only under all', () => {
  const unknown = REQS.filter((r) => isUnknownConnection(r, CONNS)).map((r) => r.id);
  assert.deepStrictEqual(unknown, ['r3', 'r5', 'r6', 'r7', 'r8']);
  const all = filterByServer(REQS, ALL_SERVERS, CONNS).map((r) => r.id);
  for (const id of unknown) assert.ok(all.includes(id));
  for (const conn of CONNS) {
    const got = filterByServer(REQS, conn.id, CONNS).map((r) => r.id);
    for (const id of unknown) assert.ok(!got.includes(id), `${id} leaked into ${conn.id}`);
  }
});

test('filterByServer: every record lands in exactly one server or unknown', () => {
  const index = connectionIndex(CONNS);
  const buckets = CONNS.map((c) => filterByServer(REQS, c.id, index).length);
  const unknown = REQS.filter((r) => isUnknownConnection(r, index)).length;
  assert.strictEqual(buckets.reduce((a, b) => a + b, 0) + unknown, REQS.length);
});

test('filterByServer: returns a new array with the same objects, input untouched', () => {
  const input = [...REQS];
  const out = filterByServer(input, ALL_SERVERS, CONNS);
  assert.notStrictEqual(out, input);
  assert.strictEqual(out[0], input[0]);
  assert.deepStrictEqual(input, REQS);
});

test('filterByServer: nullish inputs', () => {
  assert.deepStrictEqual(filterByServer(null, 'c-web', CONNS), []);
  assert.deepStrictEqual(filterByServer(REQS, 'c-web', null).length, REQS.length);
  assert.deepStrictEqual(filterByServer(undefined, undefined, undefined), []);
});

describe('serverTabs', () => {
  const sessions = [
    { connId: 'c-web', connName: 'web-01', kind: 'ssh', connState: 'connected', shells: 2, connecting: 1, pending: 99 },
    { connId: 'c-usb', connName: 'usb-serial', kind: 'serial', connState: 'in_use', shells: 1 },
    { connId: 'c-db', connName: 'db-stg', kind: 'ssh', connState: 'error' },
  ];
  const tabs = serverTabs(CONNS, sessions, REQS, 'c-web');
  const byId = Object.fromEntries(tabs.map((tab) => [tab.connId, tab]));

  it('all tab first with totals', () => {
    assert.deepStrictEqual(tabs[0], {
      connId: ALL_SERVERS,
      labelKey: 'server.all',
      shells: 3,
      pending: 5,
      unknownPending: 3,
      active: false,
    });
  });

  it('order follows connections', () => {
    assert.deepStrictEqual(tabs.slice(1).map((tab) => tab.connId), CONNS.map((c) => c.id));
  });

  const cases = [
    ['c-web', { name: 'web-01', kind: 'ssh', connState: 'connected', symbol: '●', shells: 2, connecting: 1, pending: 2, active: true }],
    ['c-db', { name: 'db-stg', kind: 'ssh', connState: 'error', symbol: '▲', shells: 0, connecting: 0, pending: 0, active: false }],
    ['c-usb', { name: 'usb-serial', kind: 'serial', connState: 'in_use', symbol: '◆', shells: 1, connecting: 0, pending: 0, active: false }],
    ['c-dup1', { name: 'dup', kind: 'ssh', connState: 'disconnected', symbol: '○', shells: 0, connecting: 0, pending: 0, active: false }],
  ];
  for (const [connId, want] of cases) {
    it(connId, () => {
      assert.deepStrictEqual(byId[connId], { connId, ...want });
    });
  }

  it('pending badge equals filtered queue length', () => {
    for (const tab of tabs.slice(1)) {
      const queue = filterByServer(REQS, tab.connId, CONNS).filter((r) => r.status === 'pending');
      assert.strictEqual(tab.pending, queue.length, tab.connId);
    }
    const allQueue = filterByServer(REQS, ALL_SERVERS, CONNS).filter((r) => r.status === 'pending');
    assert.strictEqual(tabs[0].pending, allQueue.length);
  });
});

test('serverTabs: stale or empty filter activates all', () => {
  for (const filter of ['c-gone', '', undefined]) {
    const tabs = serverTabs(CONNS, [], [], filter);
    assert.strictEqual(tabs[0].active, true);
    assert.ok(tabs.slice(1).every((tab) => !tab.active));
  }
});

test('serverTabs: empty and malformed inputs', () => {
  assert.deepStrictEqual(serverTabs(null, null, null), [{
    connId: ALL_SERVERS, labelKey: 'server.all', shells: 0, pending: 0, unknownPending: 0, active: true,
  }]);
  const tabs = serverTabs([{ id: 'a', name: 'x' }, { id: 'a', name: 'dup-id' }, { name: 'no-id' }], [], []);
  assert.deepStrictEqual(tabs.slice(1).map((tab) => tab.name), ['x']);
});

test('SERVERFILTER_I18N has matching ja/en keys', () => {
  assert.deepStrictEqual(Object.keys(SERVERFILTER_I18N.ja).sort(), Object.keys(SERVERFILTER_I18N.en).sort());
  assert.strictEqual(SERVERFILTER_I18N.ja['server.unknown'], '不明な接続先');
});
