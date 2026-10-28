import { describe, expect, it } from 'vitest';
import {
  MODEL_I18N,
  connStateLabelKey,
  connStateSymbol,
  connectionDirtyState,
  editableSnapshot,
  escapeHtml,
  formatElapsed,
  interpolate,
  isKnownRequestState,
  lastTestLabel,
  mergeI18n,
  requestStateLabelKey,
  saveStateLabelKey,
  sortSessionsForSidebar,
  summarizeSessions,
} from './model.js';
import * as model from './model.js';
import * as serverfilter from './serverfilter.js';
import { serverTabs } from './serverfilter.js';
import { serverStateSymbol } from './servertabs.js';

const NOW = Date.parse('2026-10-27T12:00:00Z');
const ago = (minutes) => new Date(NOW - minutes * 60000).toISOString();

function session(connName, fields = {}) {
  return {
    connId: `id-${connName}`,
    connName,
    kind: 'ssh',
    connState: 'disconnected',
    shells: 0,
    connecting: 0,
    running: 0,
    pending: 0,
    ...fields,
  };
}

describe('summarizeSessions', () => {
  const cases = [
    {
      name: 'empty',
      sessions: [],
      want: { connected: 0, saved: 0, errors: 0, running: 0, rows: [], more: 0 },
    },
    {
      name: 'null is treated as empty',
      sessions: null,
      want: { connected: 0, saved: 0, errors: 0, running: 0, rows: [], more: 0 },
    },
    {
      name: 'all disconnected',
      sessions: [session('a'), session('b'), session('c')],
      want: { connected: 0, saved: 3, errors: 0, running: 0, rows: [], more: 0 },
    },
    {
      name: 'error comes first',
      sessions: [
        session('web', { connState: 'connected', shells: 1, connectedSince: ago(1) }),
        session('router', { connState: 'error', lastError: 'boom' }),
      ],
      order: ['router', 'web'],
      want: { connected: 1, saved: 2, errors: 1, running: 0, more: 0 },
    },
    {
      name: 'running comes after errors',
      sessions: [
        session('fresh', { connState: 'connected', shells: 1, connectedSince: ago(1) }),
        session('busy', { connState: 'connected', shells: 1, running: 2, connectedSince: ago(30) }),
        session('router', { connState: 'error' }),
      ],
      order: ['router', 'busy', 'fresh'],
      want: { connected: 2, errors: 1, running: 2 },
    },
    {
      name: 'newest connectedSince first, then name',
      sessions: [
        session('old', { connState: 'connected', shells: 1, connectedSince: ago(50) }),
        session('new', { connState: 'connected', shells: 1, connectedSince: ago(2) }),
        session('zz-connecting', { connState: 'connecting', connecting: 1 }),
        session('aa-connecting', { connState: 'connecting', connecting: 1 }),
      ],
      order: ['new', 'old', 'aa-connecting', 'zz-connecting'],
      want: { connected: 2 },
    },
    {
      name: 'six visible rows are cut to five with more=1',
      sessions: [1, 2, 3, 4, 5, 6].map((n) => session(`h${n}`, { connState: 'connected', shells: 1, connectedSince: ago(n) })),
      order: ['h1', 'h2', 'h3', 'h4', 'h5'],
      want: { connected: 6, saved: 6, more: 1 },
    },
    {
      name: 'serial in_use counts as connected',
      sessions: [
        session('usb', { kind: 'serial', connState: 'in_use', shells: 1, connectedSince: ago(8), label: '/dev/cu.usbserial-10 @115200' }),
        session('web'),
      ],
      order: ['usb'],
      want: { connected: 1, saved: 2 },
    },
    {
      name: 'running row is listed even when not connected',
      sessions: [session('batch', { running: 1 }), session('idle-host')],
      order: ['batch'],
      want: { connected: 0, running: 1 },
    },
  ];

  it.each(cases)('$name', ({ sessions, order, want }) => {
    const view = summarizeSessions(sessions, { now: NOW });
    expect(view).toMatchObject(want);
    if (order) expect(view.rows.map((r) => r.name)).toEqual(order);
  });

  it('fills row fields, symbol and elapsed time', () => {
    const view = summarizeSessions([
      session('usb', { kind: 'serial', connState: 'in_use', shells: 1, connectedSince: ago(8), label: '/dev/cu.usbserial-10 @115200' }),
      session('web', { connState: 'connected', shells: 2, running: 1, pending: 3, connectedSince: ago(12) }),
      session('router', { connState: 'error' }),
    ], { now: NOW });
    expect(view.rows).toEqual([
      { connId: 'id-router', name: 'router', kind: 'ssh', connState: 'error', symbol: '▲', shells: 0, connecting: 0, running: 0, pending: 0, elapsedMs: null, label: '' },
      { connId: 'id-web', name: 'web', kind: 'ssh', connState: 'connected', symbol: '●', shells: 2, connecting: 0, running: 1, pending: 3, elapsedMs: 12 * 60000, label: '' },
      { connId: 'id-usb', name: 'usb', kind: 'serial', connState: 'in_use', symbol: '◆', shells: 1, connecting: 0, running: 0, pending: 0, elapsedMs: 8 * 60000, label: '/dev/cu.usbserial-10 @115200' },
    ]);
  });

  it('honors limit and clamps negative elapsed time', () => {
    const sessions = [
      session('future', { connState: 'connected', shells: 1, connectedSince: new Date(NOW + 5000).toISOString() }),
      session('b', { connState: 'error' }),
    ];
    const view = summarizeSessions(sessions, { now: NOW, limit: 1 });
    expect(view.rows.map((r) => r.name)).toEqual(['b']);
    expect(view.more).toBe(1);
    expect(summarizeSessions(sessions, { now: NOW }).rows[1].elapsedMs).toBe(0);
  });
});

describe('sortSessionsForSidebar', () => {
  it('does not mutate its input', () => {
    const input = [session('b'), session('a')];
    const sorted = sortSessionsForSidebar(input);
    expect(sorted.map((s) => s.connName)).toEqual(['a', 'b']);
    expect(input.map((s) => s.connName)).toEqual(['b', 'a']);
  });
});

describe('connectionDirtyState', () => {
  const sshSaved = {
    id: 'c1', name: 'web-01', type: 'SSH', host: '10.0.0.1', port: 22, user: 'ops',
    authMethod: 'SSH鍵認証', credential: '~/.ssh/id_ed25519', serialPort: '', baudRate: 0,
    connectTimeout: 10, commandTimeout: 60, tags: ['prod', 'web'], description: 'front',
  };
  const pwSaved = { ...sshSaved, authMethod: 'パスワード保管', credential: '', credentialSet: true };
  const serialSaved = {
    id: 's1', name: 'usb', type: 'Serial', host: '', port: 0, user: '', authMethod: 'SSH鍵認証',
    credential: '', serialPort: '/dev/cu.usbserial-10', baudRate: 115200,
    connectTimeout: 10, commandTimeout: 60, tags: [], description: '',
  };

  const cases = [
    { name: 'draft null', draft: null, saved: sshSaved, want: 'saved' },
    { name: 'empty id is new', draft: { ...sshSaved, id: '' }, saved: undefined, want: 'new' },
    { name: 'identical copy', draft: { ...sshSaved }, saved: sshSaved, want: 'saved' },
    {
      name: 'numeric strings and tag spacing are equal',
      draft: { ...sshSaved, port: '22', connectTimeout: '10', commandTimeout: '60', tags: [' prod ', '', 'web '] },
      saved: sshSaved,
      want: 'saved',
    },
    { name: 'tags as comma string', draft: { ...sshSaved, tags: 'prod, web,' }, saved: sshSaved, want: 'saved' },
    { name: 'surrounding whitespace is ignored', draft: { ...sshSaved, name: ' web-01 ' }, saved: sshSaved, want: 'saved' },
    { name: 'renamed', draft: { ...sshSaved, name: 'web-02' }, saved: sshSaved, want: 'modified' },
    { name: 'port changed', draft: { ...sshSaved, port: '2222' }, saved: sshSaved, want: 'modified' },
    { name: 'tag order matters', draft: { ...sshSaved, tags: ['web', 'prod'] }, saved: sshSaved, want: 'modified' },
    { name: 'SSH ignores serialPort', draft: { ...sshSaved, serialPort: '/dev/ttyUSB0', baudRate: 9600 }, saved: sshSaved, want: 'saved' },
    { name: 'Serial ignores host/user/auth', draft: { ...serialSaved, host: 'x', user: 'y', authMethod: 'パスワード保管', credential: 'p' }, saved: serialSaved, want: 'saved' },
    { name: 'Serial baud changed', draft: { ...serialSaved, baudRate: '9600' }, saved: serialSaved, want: 'modified' },
    { name: 'type switched', draft: { ...sshSaved, type: 'Serial' }, saved: sshSaved, want: 'modified' },
    { name: 'password auth untouched', draft: { ...pwSaved }, saved: pwSaved, want: 'saved' },
    { name: 'password typed', draft: { ...pwSaved, credential: 'secret' }, saved: pwSaved, want: 'modified' },
    { name: 'key path changed', draft: { ...sshSaved, credential: '~/.ssh/other' }, saved: sshSaved, want: 'modified' },
    { name: 'saved deleted', draft: { ...sshSaved }, saved: undefined, want: 'modified' },
  ];

  it.each(cases)('$name → $want', ({ draft, saved, want }) => {
    expect(connectionDirtyState(draft, saved)).toBe(want);
  });

  it('editableSnapshot drops non-editable fields', () => {
    const snap = editableSnapshot({ ...sshSaved, lastTestResult: 'ok', credentialSet: true, mcpFirstTrust: true });
    expect(Object.keys(snap).sort()).toEqual(
      ['authMethod', 'commandTimeout', 'connectTimeout', 'credential', 'description', 'host', 'name', 'port', 'tags', 'type', 'user'],
    );
  });
});

describe('formatElapsed', () => {
  const s = 1000;
  const m = 60 * s;
  const h = 60 * m;
  const cases = [
    [0, '1分未満', '<1m'],
    [59 * s, '1分未満', '<1m'],
    [60 * s, '1分', '1m'],
    [59 * m, '59分', '59m'],
    [60 * m, '1時間', '1h'],
    [65 * m, '1時間5分', '1h 5m'],
    [23 * h + 59 * m, '23時間59分', '23h 59m'],
    [24 * h, '1日', '1d'],
    [50 * h, '2日', '2d'],
  ];
  it.each(cases)('%i ms → %s / %s', (ms, ja, en) => {
    expect(formatElapsed(ms, 'ja')).toBe(ja);
    expect(formatElapsed(ms, 'en')).toBe(en);
  });

  it('returns empty for missing values', () => {
    expect(formatElapsed(null, 'ja')).toBe('');
    expect(formatElapsed(Number.NaN, 'en')).toBe('');
  });
});

describe('interpolate', () => {
  const cases = [
    ['plain', undefined, 'plain'],
    ['{n} 件', { n: 3 }, '3 件'],
    ['{connected} connected / {saved} saved', { connected: 2, saved: 5 }, '2 connected / 5 saved'],
    ['{n} and {n}', { n: 0 }, '0 and 0'],
    ['keep {missing}', { n: 1 }, 'keep {missing}'],
    ['keep {n}', { n: null }, 'keep {n}'],
  ];
  it.each(cases)('%s', (template, vars, want) => {
    expect(interpolate(template, vars)).toBe(want);
  });
});

describe('mergeI18n', () => {
  it('merges disjoint fragments', () => {
    const merged = mergeI18n({ ja: { a: 'あ' }, en: { a: 'A' } }, { ja: { b: 'い' }, en: { b: 'B' } });
    expect(merged).toEqual({ ja: { a: 'あ', b: 'い' }, en: { a: 'A', b: 'B' } });
  });

  it('throws on duplicate keys', () => {
    expect(() => mergeI18n({ ja: { a: 'あ' }, en: { a: 'A' } }, { ja: { a: 'え' }, en: { a: 'E' } })).toThrow(/duplicate key "a"/);
  });

  it('throws on ja/en mismatch', () => {
    expect(() => mergeI18n({ ja: { a: 'あ', b: 'い' }, en: { a: 'A' } })).toThrow(/mismatch: b/);
    expect(() => mergeI18n({ ja: {}, en: { c: 'C' } })).toThrow(/mismatch: c/);
  });

  it('MODEL_I18N is consistent and uses only the state. prefix', () => {
    const merged = mergeI18n(MODEL_I18N);
    expect(Object.keys(merged.ja).every((key) => key.startsWith('state.'))).toBe(true);
    expect(Object.keys(merged.ja)).toHaveLength(25);
  });
});

describe('state labels', () => {
  it.each([
    ['connected', '●'],
    ['connecting', '○'],
    ['disconnected', '○'],
    ['error', '▲'],
    ['in_use', '◆'],
    ['idle', '●'],
    ['available', '○'],
    ['missing', '○'],
    ['weird', '○'],
  ])('connStateSymbol(%s) = %s', (code, symbol) => {
    expect(connStateSymbol(code)).toBe(symbol);
  });

  it('serverfilter.js and servertabs.js fallback use the same symbols', () => {
    for (const code of ['connected', 'connecting', 'disconnected', 'error', 'in_use', 'available', 'missing']) {
      const [, tab] = serverTabs([{ id: 'c', name: 'n' }], [{ connId: 'c', connState: code }], []);
      expect(tab.symbol, code).toBe(connStateSymbol(code));
      expect(serverStateSymbol(code), code).toBe(connStateSymbol(code));
    }
  });

  it('label keys exist in MODEL_I18N for every known code', () => {
    for (const code of ['saved', 'modified', 'new']) {
      expect(MODEL_I18N.ja).toHaveProperty([saveStateLabelKey(code)]);
    }
    for (const code of ['connected', 'connecting', 'idle', 'disconnected', 'error', 'in_use', 'available', 'missing']) {
      expect(MODEL_I18N.en).toHaveProperty([connStateLabelKey(code)]);
    }
    for (const code of ['pending', 'running', 'succeeded', 'failed', 'rejected', 'expired', 'interrupted', 'auto_running', 'auto_succeeded', 'auto_failed']) {
      expect(isKnownRequestState(code)).toBe(true);
      expect(MODEL_I18N.ja).toHaveProperty([requestStateLabelKey(code)]);
    }
  });

  it('unknown codes fall back safely', () => {
    expect(connStateLabelKey('bogus')).toBe('state.conn.disconnected');
    expect(saveStateLabelKey(undefined)).toBe('state.save.saved');
    expect(requestStateLabelKey('withdrawn')).toBe('state.req.withdrawn');
    expect(isKnownRequestState('withdrawn')).toBe(false);
  });

  it('§6.3 wording', () => {
    expect(MODEL_I18N.ja['state.conn.disconnected']).toBe('未接続');
    expect(MODEL_I18N.en['state.save.new']).toBe('Not saved yet');
    expect(MODEL_I18N.ja['state.req.interrupted']).toBe('中断（結果不明）');
  });
});

describe('lastTestLabel', () => {
  const at = new Date(2026, 9, 27, 10, 32, 5).toISOString();
  const cases = [
    ['ja', { lastTestResult: 'ok', lastTestAt: at }, '最終テスト: 成功 10:32'],
    ['ja', { lastTestResult: 'failed', lastTestAt: at }, '最終テスト: 失敗 10:32'],
    ['ja', { lastTestResult: '' }, '最終テスト: 未確認'],
    ['en', { lastTestResult: 'ok', lastTestAt: at }, 'Last test: OK 10:32'],
    ['en', { lastTestResult: 'failed', lastTestAt: 'garbage' }, 'Last test: Failed'],
    ['en', {}, 'Last test: Not checked'],
  ];
  it.each(cases)('%s %j', (lang, conn, want) => {
    const t = (key) => MODEL_I18N[lang][key] ?? key;
    expect(lastTestLabel(conn, t)).toBe(want);
  });
});

describe('escapeHtml', () => {
  it.each([
    [`<a href="x">'&'</a>`, '&lt;a href=&quot;x&quot;&gt;&#039;&amp;&#039;&lt;/a&gt;'],
    [null, ''],
    [undefined, ''],
    [0, '0'],
  ])('%j', (input, want) => {
    expect(escapeHtml(input)).toBe(want);
  });
});

describe('server filter re-exports', () => {
  it('model.js re-exports the serverfilter.js functions unchanged', () => {
    for (const name of ['filterByServer', 'isUnknownConnection', 'recordConnId', 'connectionIndex']) {
      expect(model[name], name).toBe(serverfilter[name]);
    }
    const conns = [{ id: 'c-web', name: 'web-01' }];
    const reqs = [{ id: 'R1', connectionId: 'c-web' }, { id: 'R2', connectionId: 'c-gone' }];
    expect(model.filterByServer(reqs, 'c-web', conns).map((r) => r.id)).toEqual(['R1']);
    expect(model.filterByServer(reqs, '', conns).map((r) => r.id)).toEqual(['R1', 'R2']);
  });
});
