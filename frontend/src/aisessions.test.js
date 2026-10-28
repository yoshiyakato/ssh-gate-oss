import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

// xterm needs a real layout engine; jsdom gets a recording stand-in.
const terminals = [];
vi.mock('@xterm/xterm', () => ({
  Terminal: class {
    constructor(options) {
      this.options = { ...options };
      this.written = [];
      this.onDataCalls = 0;
      this.disposed = false;
      this.oscHandlers = [];
      this.parser = { registerOscHandler: (code, fn) => { this.oscHandlers.push(code); return { dispose() {}, fn }; } };
      terminals.push(this);
    }
    loadAddon(addon) { addon.activate?.(this); }
    open(el) { this.element = document.createElement('div'); this.element.className = 'xterm'; el.appendChild(this.element); }
    onData() { this.onDataCalls += 1; return { dispose() {} }; }
    write(data) { this.written.push(typeof data === 'string' ? data : new TextDecoder().decode(data)); }
    attachCustomKeyEventHandler(fn) { this.keyHandler = fn; }
    dispose() { this.disposed = true; }
  },
}));
vi.mock('@xterm/addon-fit', () => ({
  FitAddon: class { activate(t) { this.t = t; } fit() { this.fitted = (this.fitted ?? 0) + 1; } },
}));

import { MODEL_I18N, interpolate, mergeI18n } from './model.js';
import {
  AI_SESSIONS_I18N,
  aiSessionCloseReasonKey,
  aiSessionReasonLabel,
  aiSessionStateLabelKey,
  aiSessionsBusy,
  buildAISessionsPage,
  disposeAISessionViewer,
  displayEnvValue,
  formatAge,
  getAISessionViewer,
  getAISessionsState,
  handleAISessionsClick,
  mountAISessionViewer,
  normalizeAISession,
  normalizeAISessionSettings,
  onAISessionData,
  onAISessionsChanged,
  renderAILineContext,
  renderAISessionsPage,
  renderSidebarAISessions,
  resetAISessionsState,
  setAISessionsApi,
  shortSessionId,
  sortAISessions,
  summarizeAISessions,
  validateAISessionSettings,
} from './aisessions.js';

const I18N = mergeI18n(AI_SESSIONS_I18N, MODEL_I18N);
const tFor = (lang) => (key, vars) => {
  if (!Object.hasOwn(I18N[lang], key)) throw new Error(`missing i18n key ${key} (${lang})`);
  return interpolate(I18N[lang][key], vars);
};
const NOW = Date.parse('2026-10-28T10:00:00Z');
const ID_A = 'AIS-7QABCDEFGHIJKLMNOPQRSTUVWX';
const ID_B = 'AIS-K2ABCDEFGHIJKLMNOPQRSTUVWX';
const ID_C = 'AIS-ABCDEFGHIJKLMNOPQRSTUVWXYZ';

const flush = async () => {
  for (let i = 0; i < 20; i += 1) await Promise.resolve();
};
const b64 = (text) => btoa(text);

function session(id, extra = {}) {
  return {
    id, agentName: 'claude-code', connId: 'c-web', connName: 'web-01', mode: 'exec', shell: 'sh',
    state: 'idle', cwd: '/var/www', tainted: false, reason: 'nginx の設定を調べる',
    openedAt: '2026-10-28T09:48:00Z', lastActivityAt: '2026-10-28T09:58:00Z',
    expiresAt: '2026-10-28T10:48:00Z', lineCount: 8, redactions: 0, ...extra,
  };
}

const LIST = [
  session(ID_A, { state: 'pending_approval', pendingRequestId: 'REQ-8' }),
  session(ID_B, { connId: 'c-db', connName: 'db-stg', agentName: 'codex', mode: 'shell', tainted: true, lineCount: 2, openedAt: '2026-10-28T09:57:00Z' }),
  session(ID_C, { state: 'closed', closeReason: 'idle_timeout', closedAt: '2026-10-28T09:02:00Z', lineCount: 3 }),
];

function makeApi() {
  return {
    ListAISessions: vi.fn(async () => LIST),
    GetAISession: vi.fn(async (id) => ({
      ...session(id),
      history: [
        { seq: 1, requestId: 'REQ-1', line: 'cd /var/www', status: 'auto_succeeded', exitCode: 0 },
        { seq: 2, requestId: 'REQ-2', line: 'ls', status: 'succeeded', exitCode: 0 },
        { seq: 8, requestId: 'REQ-8', line: 'nginx -t', status: 'pending' },
      ],
    })),
    ReadAISessionRaw: vi.fn(async () => ({ data: b64('hello\r\n'), startCursor: 0, endCursor: 7, missedBytes: 0 })),
    CloseAISession: vi.fn(async () => ({ ok: true, message: '終了しました' })),
    CloseAllAISessions: vi.fn(async () => ({ ok: true, message: 'すべて終了しました' })),
    GetAISessionSettings: vi.fn(async () => ({ enabled: true, idleMinutes: 10, maxMinutes: 60, perAgent: 3, total: 16, bypassMaxRisk: 'Low', shellConnIds: ['c-db'], ringKB: 1024, auditKB: 1024 })),
    SaveAISessionSettings: vi.fn(async () => ({ ok: true, message: '保存しました' })),
  };
}

function makeCtx(lang = 'ja') {
  return {
    t: tFor(lang),
    lang: () => lang,
    state: {
      data: { connections: [{ id: 'c-web', name: 'web-01' }, { id: 'c-db', name: 'db-stg' }, { id: 'c-ser', name: 'usb', kind: 'serial' }] },
      selectedRequestId: '',
    },
    toast: vi.fn(),
    refresh: vi.fn(async () => {}),
    bumpGen: vi.fn(),
    navigate: vi.fn(async () => true),
  };
}

function clickEvent(selector) {
  const target = document.querySelector(selector);
  if (!target) throw new Error(`no element ${selector}`);
  return { target };
}

async function answerDialog(ok) {
  await flush();
  const button = document.querySelector(`#aisessions-modal-root [data-ais-dialog="${ok ? 'ok' : 'cancel'}"]`);
  if (!button) throw new Error('no dialog');
  button.click();
}

// rowsText is the list as compact text: the "snapshot" of the page.
function rowsText(root) {
  return [...root.querySelectorAll('.ais-table tbody tr')].map((tr) => [...tr.children].map((td) => td.textContent.trim().replace(/\s+/g, ' ')).join(' | '));
}

let api;

beforeEach(() => {
  resetAISessionsState();
  terminals.length = 0;
  api = makeApi();
  setAISessionsApi(api);
  document.body.innerHTML = '<div id="page-root"></div>';
  vi.useFakeTimers({ toFake: ['Date'] });
  vi.setSystemTime(NOW);
});

afterEach(() => {
  vi.useRealTimers();
  resetAISessionsState();
});

describe('i18n', () => {
  it('has the same keys in JA and EN and merges with the base dictionary', () => {
    expect(Object.keys(AI_SESSIONS_I18N.ja).sort()).toEqual(Object.keys(AI_SESSIONS_I18N.en).sort());
    expect(() => mergeI18n(AI_SESSIONS_I18N, MODEL_I18N)).not.toThrow();
  });

  const states = ['opening', 'idle', 'pending_approval', 'busy', 'closing', 'closed'];
  const reasons = ['agent_closed', 'human_closed', 'idle_timeout', 'max_lifetime', 'disconnected', 'connection_deleted',
    'target_changed', 'token_revoked', 'ai_session_disabled', 'remote_exit', 'conn_lost', 'app_shutdown', 'app_restart', 'open_failed'];

  it.each(['ja', 'en'])('labels every session state and close reason (%s)', (lang) => {
    const t = tFor(lang);
    for (const code of states) expect(t(aiSessionStateLabelKey(code))).not.toMatch(/^ais\./);
    for (const code of reasons) {
      expect(aiSessionCloseReasonKey(code)).toBe(`ais.close.${code}`);
      expect(t(aiSessionCloseReasonKey(code))).not.toMatch(/^ais\./);
    }
    expect(aiSessionCloseReasonKey('weird')).toBe('ais.close.unknown');
    expect(t(aiSessionCloseReasonKey('weird'), { code: 'weird' })).toContain('weird');
    expect(aiSessionStateLabelKey('nope')).toBe('ais.state.idle');
  });

  it('translates classification reasons, including base:<risk>', () => {
    const t = tFor('en');
    expect(aiSessionReasonLabel('env_overlay_unsafe', t)).toBe('An environment variable with a non-safe name is set');
    expect(aiSessionReasonLabel('base:Medium', t)).toBe('Command on its own: Medium');
    expect(aiSessionReasonLabel('future_code', t)).toBe('future_code');
  });
});

describe('model helpers', () => {
  it.each([
    [[], { open: 0, pendingApproval: 0 }],
    [LIST, { open: 2, pendingApproval: 1 }],
    [[session('AIS-1', { state: 'busy' }), session('AIS-2', { pendingRequestId: 'REQ-1' })], { open: 2, pendingApproval: 1 }],
    [[session('AIS-1', { closedAt: '2026-10-28T09:00:00Z', state: '' })], { open: 0, pendingApproval: 0 }],
    [{ open: 3, pendingApproval: 2 }, { open: 3, pendingApproval: 2 }],
    [{ open: 0, pendingApproval: 0, enabled: true }, { open: 0, pendingApproval: 0, enabled: true }],
    [null, { open: 0, pendingApproval: 0 }],
  ])('summarizeAISessions(%j)', (input, expected) => {
    expect(summarizeAISessions(input)).toEqual(expected);
  });

  it('normalizes snake_case list_sessions rows too', () => {
    const s = normalizeAISession({ session_id: ID_A, connection: 'web-01', close_reason: 'agent_closed', lines: 4 });
    expect(s).toMatchObject({ id: ID_A, connName: 'web-01', state: 'closed', closeReason: 'agent_closed', lines: 4 });
  });

  it('treats Go zero times as unset (an open session stays open)', () => {
    const s = normalizeAISession({ id: ID_A, connectionId: 'c-web', hostName: 'web-01', state: 'idle', closedAt: '0001-01-01T00:00:00Z', openedAt: '2026-10-28T09:00:00Z' });
    expect(s).toMatchObject({ connId: 'c-web', connName: 'web-01', closedAt: '', state: 'idle' });
    expect(summarizeAISessions([{ id: ID_A, closedAt: '0001-01-01T00:00:00Z' }])).toEqual({ open: 1, pendingApproval: 0 });
  });

  it('sorts open sessions (newest first) before closed ones', () => {
    const sorted = sortAISessions(LIST.map(normalizeAISession)).map((s) => s.id);
    expect(sorted).toEqual([ID_B, ID_A, ID_C]);
  });

  it('formats ages and short ids', () => {
    const t = tFor('ja');
    expect(formatAge('2026-10-28T09:48:00Z', t, NOW)).toBe('12分');
    expect(formatAge('2026-10-28T08:30:00Z', t, NOW)).toBe('1時間30分');
    expect(formatAge('2026-10-28T09:59:30Z', t, NOW)).toBe('1分未満');
    expect(formatAge('', t, NOW)).toBe('-');
    expect(shortSessionId(ID_A)).toBe('AIS-7QAB…');
  });
});

describe('sidebar line', () => {
  it('shows the count and the pending approvals', () => {
    const html = renderSidebarAISessions({ open: 2, pendingApproval: 1 }, tFor('ja'));
    expect(html).toContain('data-action="side-aisessions"');
    expect(html).toContain('AI セッション 2（承認待ち 1）');
    expect(renderSidebarAISessions({ open: 2, pendingApproval: 1 }, tFor('en'))).toContain('AI sessions 2 (1 awaiting approval)');
  });

  it('drops the approval part when nothing waits', () => {
    expect(renderSidebarAISessions({ open: 1, pendingApproval: 0 }, tFor('en'))).toContain('AI sessions 1 <');
  });

  it('is hidden with no session unless the feature is known to be on', () => {
    expect(renderSidebarAISessions({ open: 0, pendingApproval: 0 }, tFor('ja'))).toBe('');
    expect(renderSidebarAISessions({ open: 0, pendingApproval: 0, enabled: false }, tFor('ja'))).toBe('');
    expect(renderSidebarAISessions(undefined, tFor('ja'))).toBe('');
    expect(renderSidebarAISessions({ open: 0, pendingApproval: 0, enabled: true }, tFor('ja'))).toContain('AI セッション 0');
  });

  it('navigates to the page from the sidebar action', async () => {
    document.body.innerHTML = renderSidebarAISessions({ open: 1, pendingApproval: 0 }, tFor('ja'));
    const ctx = makeCtx();
    expect(await handleAISessionsClick(clickEvent('[data-action="side-aisessions"]'), ctx)).toBe(true);
    expect(ctx.navigate).toHaveBeenCalledWith('aisessions');
  });
});

describe('page', () => {
  async function mount(lang = 'ja') {
    const ctx = makeCtx(lang);
    const root = document.getElementById('page-root');
    renderAISessionsPage(root, ctx);
    await flush();
    return { ctx, root };
  }

  it('renders the list (JA)', async () => {
    const { root } = await mount('ja');
    expect(api.ListAISessions).toHaveBeenCalledTimes(1);
    expect(rowsText(root)).toEqual([
      '● 待機中 | AIS-K2AB… | db-stg | codex | shell ⚠ 汚染 | /var/www | 3分 | 2 | 閲覧 強制終了',
      '⏳ 承認待ち | AIS-7QAB… | web-01 | claude-code | exec | /var/www | 12分 | 8 承認待ち 1 | 閲覧 強制終了',
      expect.stringMatching(/^○ 終了（アイドルで終了） \| AIS-ABCD… \| web-01 \| claude-code \| exec \| \/var\/www \| \d\d:\d\d \| 3 \| 閲覧$/),
    ]);
    expect(root.querySelector('.ais-tainted').getAttribute('title')).toContain('以後の行はすべて承認が必要です');
    expect(root.querySelector('[data-action="ais-close-all"]')).not.toBeNull();
    // The closed tombstone has no force-close button.
    expect(root.querySelector(`[data-ais-row="${ID_C}"] [data-action="ais-close"]`)).toBeNull();
  });

  it('renders the list (EN)', async () => {
    const { root } = await mount('en');
    const rows = rowsText(root);
    expect(rows[0]).toBe('● Idle | AIS-K2AB… | db-stg | codex | shell ⚠ Tainted | /var/www | 3m | 2 | View Force close');
    expect(rows[1]).toBe('⏳ Awaiting approval | AIS-7QAB… | web-01 | claude-code | exec | /var/www | 12m | 8 1 awaiting approval | View Force close');
    expect(rows[2]).toMatch(/^○ Closed \(Idle timeout\) \|/);
  });

  it('shows the empty and disabled notices', async () => {
    api.ListAISessions.mockResolvedValue([]);
    api.GetAISessionSettings.mockResolvedValue({ enabled: false });
    const { root } = await mount('en');
    expect(root.querySelector('.ais-empty').textContent).toBe('No AI sessions are open.');
    expect(root.querySelector('.ais-disabled').textContent).toBe('AI sessions are disabled. Enable them in settings.');
    expect(root.querySelector('[data-action="ais-close-all"]')).toBeNull();
  });

  it('shows a load error', async () => {
    api.ListAISessions.mockRejectedValue(new Error('boom'));
    const { root } = await mount('ja');
    expect(root.querySelector('.ais-error').textContent).toBe('AI セッションを読み込めませんでした: boom');
  });

  it('builds text only: agent-supplied strings are never parsed as HTML', async () => {
    api.ListAISessions.mockResolvedValue([session(ID_A, { cwd: '<img src=x onerror=alert(1)>', agentName: '<b>x</b>' })]);
    const { root } = await mount('ja');
    expect(root.querySelector('img')).toBeNull();
    expect(root.querySelector('b')).toBeNull();
    expect(root.textContent).toContain('<img src=x onerror=alert(1)>');
  });

  it('does not refetch on a quick re-render', async () => {
    const { root, ctx } = await mount();
    renderAISessionsPage(root, ctx);
    await flush();
    expect(api.ListAISessions).toHaveBeenCalledTimes(1);
  });

  it('applies aisessions:changed with a list, and refetches without one', async () => {
    const { root } = await mount();
    onAISessionsChanged({ sessions: [session(ID_A, { state: 'busy' })] });
    expect(rowsText(root)).toHaveLength(1);
    expect(root.querySelector('.ais-state').dataset.state).toBe('busy');
    onAISessionsChanged({});
    await flush();
    expect(api.ListAISessions).toHaveBeenCalledTimes(2);
    expect(rowsText(root)).toHaveLength(3);
  });

  it('only marks the list stale when the page is not on screen', async () => {
    const { root } = await mount();
    root.remove();
    onAISessionsChanged({});
    await flush();
    expect(api.ListAISessions).toHaveBeenCalledTimes(1);
    expect(getAISessionsState().loadedAt).toBe(0);
  });

  describe('force close', () => {
    it('does not call CloseAISession when the dialog is cancelled', async () => {
      const { ctx } = await mount();
      const done = handleAISessionsClick(clickEvent(`[data-ais-row="${ID_A}"] [data-action="ais-close"]`), ctx);
      await flush();
      const dialog = document.querySelector('#aisessions-modal-root .modal-body');
      expect(dialog.textContent).toBe('このセッションを終了します。承認待ちの行は取り下げられ、実行中の行は中止されます。');
      expect(document.activeElement?.dataset.aisDialog).toBe('cancel');
      await answerDialog(false);
      expect(await done).toBe(true);
      expect(api.CloseAISession).not.toHaveBeenCalled();
      expect(document.querySelector('#aisessions-modal-root .modal')).toBeNull();
    });

    it('closes on Escape without calling the backend', async () => {
      const { ctx } = await mount();
      const done = handleAISessionsClick(clickEvent(`[data-ais-row="${ID_A}"] [data-action="ais-close"]`), ctx);
      await flush();
      document.querySelector('#aisessions-modal-root .modal').dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
      await done;
      expect(api.CloseAISession).not.toHaveBeenCalled();
    });

    it('calls CloseAISession on OK, toasts and reloads', async () => {
      const { ctx } = await mount();
      const done = handleAISessionsClick(clickEvent(`[data-ais-row="${ID_A}"] [data-action="ais-close"]`), ctx);
      await answerDialog(true);
      await done;
      await flush();
      expect(api.CloseAISession).toHaveBeenCalledWith(ID_A);
      expect(ctx.toast).toHaveBeenCalledWith('終了しました');
      expect(ctx.bumpGen).toHaveBeenCalled();
      expect(api.ListAISessions).toHaveBeenCalledTimes(2);
    });

    it('force closes all after confirming with the open count', async () => {
      const { ctx } = await mount('en');
      const done = handleAISessionsClick(clickEvent('[data-action="ais-close-all"]'), ctx);
      await flush();
      expect(document.querySelector('#aisessions-modal-root .modal-body').textContent).toContain('2 open session(s)');
      await answerDialog(true);
      await done;
      expect(api.CloseAllAISessions).toHaveBeenCalledTimes(1);
    });
  });

  it('jumps to the approval screen for a pending line', async () => {
    const { ctx } = await mount();
    await handleAISessionsClick(clickEvent('[data-action="ais-open-request"]'), ctx);
    expect(ctx.state.selectedRequestId).toBe('REQ-8');
    expect(ctx.navigate).toHaveBeenCalledWith('commands');
  });

  describe('viewer', () => {
    async function view(id = ID_A) {
      const env = await mount();
      await handleAISessionsClick(clickEvent(`[data-ais-row="${id}"] [data-action="ais-view"]`), env.ctx);
      await flush();
      return env;
    }

    it('creates a read-only xterm with no input handler and fills it from the ring', async () => {
      const { root } = await view();
      expect(terminals).toHaveLength(1);
      const term = terminals[0];
      expect(term.options.disableStdin).toBe(true);
      expect(term.onDataCalls).toBe(0);
      expect(term.oscHandlers).toContain(52);
      expect(api.ReadAISessionRaw).toHaveBeenCalledWith(ID_A, 0);
      expect(term.written.join('')).toBe('hello\r\n');
      expect(root.querySelector('.ais-viewer .ais-readonly').textContent).toBe('読み取り専用');
      expect(root.querySelector('#ais-viewer-slot .ais-viewer-term .xterm')).not.toBeNull();
      expect(root.querySelector('.ais-viewer-reason').textContent).toBe('目的: nginx の設定を調べる');
    });

    it('lists the sent lines with their states and a link to the approval', async () => {
      const { root } = await view();
      const items = [...root.querySelectorAll('.ais-history-item')].map((li) => li.textContent.replace(/\s+/g, ' ').trim());
      expect(items).toEqual([
        '#1 cd /var/www ✓ 成功（承認省略） (0)',
        '#2 ls ✓ 成功 (0)',
        '#8 nginx -t ⏳ 承認待ち 承認画面へ →',
      ]);
    });

    it('writes aisession:data only for the viewed session', async () => {
      await view();
      const term = terminals[0];
      onAISessionData({ id: ID_B, data: b64('other'), cursor: 7 });
      onAISessionData({ id: ID_A, data: b64('more'), cursor: 7 });
      expect(term.written.join('')).toBe('hello\r\nmore');
    });

    it('skips and trims chunks the snapshot already covered, and marks gaps', async () => {
      await view();
      const term = terminals[0];
      onAISessionData({ id: ID_A, data: b64('lo\r\n'), cursor: 3 }); // fully covered
      onAISessionData({ id: ID_A, data: b64('\r\nAB'), cursor: 5 }); // overlaps by 2
      onAISessionData({ id: ID_A, data: b64('Z'), cursor: 20 }); // 11 bytes missing
      const text = term.written.join('');
      expect(text).toBe('hello\r\nAB\r\n\x1b[2m[以前の出力は破棄されました（11 バイト）]\x1b[0m\r\nZ');
    });

    it('queues chunks that arrive while the ring is being read', async () => {
      let resolveRead;
      api.ReadAISessionRaw.mockImplementation(() => new Promise((r) => { resolveRead = r; }));
      await mount();
      const env = makeCtx();
      const slot = document.createElement('div');
      document.body.appendChild(slot);
      const pending = mountAISessionViewer(slot, ID_A, { t: env.t });
      onAISessionData({ id: ID_A, data: b64('abc'), cursor: 0 }); // inside the snapshot
      onAISessionData({ id: ID_A, data: b64('d'), cursor: 3 });
      resolveRead({ data: b64('abc'), startCursor: 0, endCursor: 3 });
      await pending;
      expect(terminals.at(-1).written.join('')).toBe('abcd');
    });

    it('shows discarded output before the ring start', async () => {
      api.ReadAISessionRaw.mockResolvedValue({ data: b64('tail'), startCursor: 4096, endCursor: 4100, missedBytes: 4096 });
      await view();
      expect(terminals[0].written.join('')).toBe('\r\n\x1b[2m[以前の出力は破棄されました（4096 バイト）]\x1b[0m\r\ntail');
    });

    it('reports a read failure inside the viewer', async () => {
      api.ReadAISessionRaw.mockRejectedValue(new Error('gone'));
      await view();
      expect(terminals[0].written.join('')).toContain('出力を読み込めませんでした: gone');
    });

    it('notes once that the viewed session ended', async () => {
      await view();
      onAISessionsChanged({ sessions: [session(ID_A, { state: 'closed', closeReason: 'human_closed', closedAt: '2026-10-28T10:00:00Z' })] });
      onAISessionsChanged({ sessions: [session(ID_A, { state: 'closed', closeReason: 'human_closed', closedAt: '2026-10-28T10:00:00Z' })] });
      await flush();
      const notes = terminals[0].written.filter((w) => w.includes('セッションは終了しました'));
      expect(notes).toEqual(['\r\n\x1b[2m[セッションは終了しました（人が強制終了）]\x1b[0m\r\n']);
    });

    it('keeps the same xterm across re-renders', async () => {
      const { root, ctx } = await view();
      renderAISessionsPage(root, ctx);
      onAISessionsChanged({ sessions: LIST });
      expect(terminals).toHaveLength(1);
      expect(root.querySelector('#ais-viewer-slot .ais-viewer-term')).toBe(getAISessionViewer().host);
    });

    it('disposes on stop viewing and on leaving the page, and reads again on return', async () => {
      const { root, ctx } = await view();
      await handleAISessionsClick(clickEvent('[data-action="ais-viewer-close"]'), ctx);
      expect(terminals[0].disposed).toBe(true);
      expect(root.querySelector('.ais-viewer')).toBeNull();
      expect(getAISessionViewer()).toBeNull();

      await handleAISessionsClick(clickEvent(`[data-ais-row="${ID_B}"] [data-action="ais-view"]`), ctx);
      await flush();
      expect(terminals).toHaveLength(2);
      disposeAISessionViewer();
      expect(terminals[1].disposed).toBe(true);
      onAISessionData({ id: ID_B, data: b64('late') });
      expect(terminals[1].written.join('')).not.toContain('late');

      renderAISessionsPage(root, ctx);
      await flush();
      expect(terminals).toHaveLength(3);
      expect(api.ReadAISessionRaw).toHaveBeenLastCalledWith(ID_B, 0);
    });

    it('does not write a stale ring read after switching sessions', async () => {
      let resolveFirst;
      api.ReadAISessionRaw.mockImplementationOnce(() => new Promise((r) => { resolveFirst = r; }));
      const { ctx } = await mount();
      await handleAISessionsClick(clickEvent(`[data-ais-row="${ID_A}"] [data-action="ais-view"]`), ctx);
      await handleAISessionsClick(clickEvent(`[data-ais-row="${ID_B}"] [data-action="ais-view"]`), ctx);
      await flush();
      resolveFirst({ data: b64('STALE'), startCursor: 0, endCursor: 5 });
      await flush();
      expect(terminals[0].written.join('')).toBe('');
      expect(terminals[1].written.join('')).toBe('hello\r\n');
    });
  });

  describe('settings', () => {
    async function openSettings() {
      const env = await mount('ja');
      await handleAISessionsClick(clickEvent('[data-action="ais-settings-toggle"]'), env.ctx);
      await flush();
      return env;
    }

    it('shows the loaded values and lists only non-serial connections for shell mode', async () => {
      const { root } = await openSettings();
      expect(root.querySelector('#ais-set-enabled').checked).toBe(true);
      expect(root.querySelector('#ais-set-idleMinutes').value).toBe('10');
      expect(root.querySelector('#ais-set-bypassMaxRisk').value).toBe('Low');
      expect([...root.querySelector('#ais-set-bypassMaxRisk').options].map((o) => o.value)).toEqual(['Low', 'Medium']);
      expect(root.querySelector('#ais-set-shell-c-db').checked).toBe(true);
      expect(root.querySelector('#ais-set-shell-c-web').checked).toBe(false);
      expect(root.querySelector('#ais-set-shell-c-ser')).toBeNull();
      expect(root.querySelector('#ais-set-enabled-help').textContent).toContain('High の行は常に人の承認が必要です');
      expect(root.querySelector('[data-action="ais-settings-save"]').disabled).toBe(true);
      expect(aiSessionsBusy()).toBe(false);
    });

    it('tracks edits, blocks redraws, validates and saves', async () => {
      const { root, ctx } = await openSettings();
      const idle = root.querySelector('#ais-set-idleMinutes');
      idle.value = '500';
      idle.dispatchEvent(new Event('input', { bubbles: true }));
      expect(aiSessionsBusy()).toBe(true);
      expect(root.querySelector('#ais-set-idleMinutes').classList.contains('is-invalid')).toBe(true);
      expect(root.querySelector('[data-action="ais-settings-save"]').disabled).toBe(true);

      const fixed = root.querySelector('#ais-set-idleMinutes');
      fixed.value = '20';
      fixed.dispatchEvent(new Event('input', { bubbles: true }));
      const web = root.querySelector('#ais-set-shell-c-web');
      web.checked = true;
      web.dispatchEvent(new Event('change', { bubbles: true }));
      const risk = root.querySelector('#ais-set-bypassMaxRisk');
      risk.value = 'Medium';
      risk.dispatchEvent(new Event('change', { bubbles: true }));
      expect(root.querySelector('[data-action="ais-settings-save"]').disabled).toBe(false);

      await handleAISessionsClick(clickEvent('[data-action="ais-settings-save"]'), ctx);
      await flush();
      expect(api.SaveAISessionSettings).toHaveBeenCalledTimes(1);
      const saved = api.SaveAISessionSettings.mock.calls[0][0];
      expect(saved).toMatchObject({ enabled: true, idleMinutes: 20, bypassMaxRisk: 'Medium' });
      expect(saved.shellConnIds.sort()).toEqual(['c-db', 'c-web']);
      expect(ctx.toast).toHaveBeenCalledWith('保存しました');
      expect(aiSessionsBusy()).toBe(false);
      expect(api.GetAISessionSettings).toHaveBeenCalledTimes(2);
    });

    it('discards edits on cancel', async () => {
      const { root, ctx } = await openSettings();
      const box = root.querySelector('#ais-set-enabled');
      box.checked = false;
      box.dispatchEvent(new Event('change', { bubbles: true }));
      await handleAISessionsClick(clickEvent('[data-action="ais-settings-cancel"]'), ctx);
      expect(root.querySelector('#ais-set-enabled').checked).toBe(true);
      expect(aiSessionsBusy()).toBe(false);
      expect(api.SaveAISessionSettings).not.toHaveBeenCalled();
    });

    it('keeps a failed save as a draft', async () => {
      api.SaveAISessionSettings.mockResolvedValue({ ok: false, message: 'storage' });
      const { root, ctx } = await openSettings();
      const total = root.querySelector('#ais-set-total');
      total.value = '20';
      total.dispatchEvent(new Event('input', { bubbles: true }));
      await handleAISessionsClick(clickEvent('[data-action="ais-settings-save"]'), ctx);
      expect(ctx.toast).toHaveBeenCalledWith('storage');
      expect(aiSessionsBusy()).toBe(true);
      expect(root.querySelector('#ais-set-total').value).toBe('20');
    });

    it('validates ranges and normalizes unknown values', () => {
      const t = tFor('en');
      expect(validateAISessionSettings(normalizeAISessionSettings({}), t)).toEqual({});
      const errors = validateAISessionSettings({ ...normalizeAISessionSettings({}), idleMinutes: 0, ringKB: 9000, perAgent: 1.5, total: '' }, t);
      expect(Object.keys(errors).sort()).toEqual(['idleMinutes', 'perAgent', 'ringKB', 'total']);
      expect(errors.idleMinutes).toBe('Enter a whole number from 1 to 120');
      expect(normalizeAISessionSettings({ bypassMaxRisk: 'High', extra: 1 })).toMatchObject({ bypassMaxRisk: 'Low', extra: 1, idleMinutes: 10 });
    });
  });

  it('ignores clicks that are not its own', async () => {
    const { ctx } = await mount();
    const other = document.createElement('button');
    other.dataset.action = 'agents-reload';
    document.body.appendChild(other);
    expect(await handleAISessionsClick({ target: other }, ctx)).toBe(false);
  });

  it('builds without a mount for tests and other hosts', () => {
    const page = buildAISessionsPage(makeCtx('en'), { now: NOW });
    expect(page.querySelector('#ais-title').textContent).toBe('AI sessions');
  });
});

describe('renderAILineContext', () => {
  const lineCtx = {
    sessionId: ID_A,
    agentName: 'claude-code',
    mode: 'shell',
    seq: 8,
    cwd: '/var/www',
    env: { LANG: 'C', API_TOKEN: 'abcd', LONG: 'x'.repeat(80) },
    envUnset: ['FOO'],
    tainted: true,
    previousLines: [
      { seq: 4, line: 'echo 4', status: 'succeeded', exitCode: 0 },
      { seq: 5, line: 'echo 5', status: 'succeeded', exitCode: 0 },
      { seq: 6, line: 'false', status: 'failed', exitCode: 1 },
      { seq: 7, line: '<script>alert(1)</script>', status: 'succeeded', exitCode: 0 },
    ],
    outputTail: Array.from({ length: 25 }, (_, i) => `out ${i}`).join('\n') + '\n',
    reasons: ['session_tainted', 'base:Low'],
    bypassMaxRisk: 'Low',
  };

  it('shows the context, escaped, with the last 3 lines and 20 output lines', () => {
    const box = renderAILineContext(lineCtx, tFor('ja'));
    document.body.appendChild(box);
    expect(box.querySelector('#ai-line-context-title').textContent).toBe('AI セッション内の行');
    expect(box.textContent).toContain(`${ID_A} · claude-code · shell · 8 行目`);
    expect(box.querySelector('.ai-line-cwd').textContent).toBe('/var/www');
    expect(box.querySelector('.ai-line-tainted')).not.toBeNull();
    expect(box.querySelector('.ai-line-shell-note')).not.toBeNull();
    const env = [...box.querySelectorAll('.ai-line-env li')].map((li) => li.textContent);
    expect(env).toEqual(['API_TOKEN=［伏せています］', 'FOO=（削除）', 'LANG=C', `LONG=${'x'.repeat(64)}…`]);
    const prev = [...box.querySelectorAll('.ai-line-prev li')].map((li) => li.textContent);
    expect(prev).toHaveLength(3);
    expect(prev[0]).toBe('#5 echo 5 成功 (0)');
    expect(prev[2]).toContain('<script>alert(1)</script>');
    expect(box.querySelector('script')).toBeNull();
    const out = box.querySelector('.ai-line-output').textContent.split('\n');
    expect(out).toHaveLength(20);
    expect(out[0]).toBe('out 5');
    expect([...box.querySelectorAll('.ai-line-reasons li')].map((li) => li.textContent)).toEqual(['汚染されたセッション', 'コマンド単体の判定: Low']);
    expect(box.querySelector('.ai-line-readonly').textContent).toContain('修正して承認できません');
    expect(box.querySelector('.ai-line-bypass').textContent).toContain('Low を超える行');
  });

  it('accepts the env overlay as [{name, value, unset}] rows', () => {
    const box = renderAILineContext({ sessionId: ID_A, env: [{ name: 'TZ', value: 'UTC' }, { name: 'FOO', unset: true }] }, tFor('en'));
    expect([...box.querySelectorAll('.ai-line-env li')].map((li) => li.textContent)).toEqual(['FOO=(unset)', 'TZ=UTC']);
  });

  it('handles a first line with no context (EN)', () => {
    const box = renderAILineContext({ sessionId: ID_A, mode: 'exec', seq: 1, cwd: '/home/u' }, tFor('en'));
    expect(box.textContent).toContain('This is the first line of the session.');
    expect(box.querySelector('.ai-line-env').textContent).toBe('None');
    expect(box.querySelector('.ai-line-output')).toBeNull();
    expect(box.querySelector('.ai-line-tainted')).toBeNull();
    expect(box.querySelector('.ai-line-shell-note')).toBeNull();
  });

  it('masks secret-looking names even if the backend did not', () => {
    const t = tFor('en');
    for (const name of ['GITHUB_TOKEN', 'db_password', 'AWS_SECRET_ACCESS_KEY', 'SSH_KEY']) {
      expect(displayEnvValue(name, 'v', t)).toBe('[hidden]');
    }
    expect(displayEnvValue('LANG', 'C', t)).toBe('C');
  });
});
