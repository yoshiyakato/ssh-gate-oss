import { describe, it, expect, vi, beforeAll, beforeEach } from 'vitest';
vi.mock('./dialog.js', () => ({ askConfirm: vi.fn() }));
import { askConfirm } from './dialog.js';
beforeEach(() => vi.mocked(askConfirm).mockReset());

// End-to-end wiring of main.js against mocked bindings: the phase 2
// acceptance points that live in the integration (not in one module).

const handlers = vi.hoisted(() => ({}));
const backend = vi.hoisted(() => ({
  data: null,
  calls: [],
  // When set, the next GetInitialData answers with the data as it was when
  // called, but only once the promise resolves (an in-flight poll).
  gate: null,
  inFlight: 0,
  maxInFlight: 0,
  save: null,
  approve: [],
}));

vi.mock('admin-lte/dist/js/adminlte.min.js', () => ({}));
vi.mock('@xterm/xterm', () => ({
  Terminal: class {
    constructor() { this.cols = 80; this.rows = 24; this.options = {}; this.written = []; }
    loadAddon(addon) { addon.activate?.(this); }
    open(el) { this.element = document.createElement('div'); this.element.className = 'xterm'; el.appendChild(this.element); }
    onData() { return { dispose() {} }; }
    write(data) { this.written.push(typeof data === 'string' ? data : new TextDecoder().decode(data)); }
    focus() {}
    blur() {}
    attachCustomKeyEventHandler(fn) { this.keyHandler = fn; }
    dispose() {}
  },
}));
vi.mock('@xterm/addon-fit', () => ({ FitAddon: class { activate() {} fit() {} } }));
vi.mock('../wailsjs/runtime/runtime', () => ({
  EventsOn: (name, cb) => { handlers[name] = cb; },
}));
vi.mock('../wailsjs/go/main/App', () => {
  const ok = (extra = {}) => async (...args) => ({ ok: true, message: '', ...extra, args });
  return {
    GetInitialData: async () => {
      backend.calls.push('GetInitialData');
      const snapshot = structuredClone(backend.data);
      backend.inFlight += 1;
      backend.maxInFlight = Math.max(backend.maxInFlight, backend.inFlight);
      try {
        const gate = backend.gate;
        backend.gate = null;
        if (gate) await gate;
        return snapshot;
      } finally {
        backend.inFlight -= 1;
      }
    },
    ListTerminalSessions: async () => [],
    StartTerminal: async (id) => {
      backend.calls.push(`StartTerminal:${id}`);
      return { ok: true, sessionId: id, message: '' };
    },
    SendTerminalInput: ok(),
    ResizeTerminal: ok(),
    CloseTerminal: ok(),
    TestConnectionWithPassphrase: ok(),
    SaveConnection: async (conn) => (backend.save ? backend.save(conn) : { ok: true, id: 'c1', message: '' }),
    TestConnectionConfig: ok(),
    DisconnectConnection: ok(),
    DeleteConnection: ok(),
    ListSerialPorts: async () => [],
    GetApprovalTarget: async (id) => ({ requestId: id, found: true, executable: true, fpChanged: false }),
    PlaintextBackups: async () => [],
    ApproveCommand: async (id, command) => {
      backend.approve.push([id, command]);
      return { ok: true, message: '' };
    },
    ApproveCommandAndBypassAgent: ok(),
    ApproveCommandConfirmed: ok(),
    DeleteCommandRequest: ok(),
    DeleteMCPSettings: ok(),
    GenerateMCPToken: ok(),
    RejectCommand: ok(),
    RevokeAgentApprovalBypass: ok(),
    SaveMCPSettings: ok(),
  };
});

const conn = (id, name) => ({
  id, name, type: 'SSH', host: '10.0.0.1', port: 22, user: 'u', authMethod: 'SSH鍵認証',
  credential: '~/.ssh/id', tags: [], description: '', connectTimeout: 10, commandTimeout: 30,
});
const session = (connId, connName, extra = {}) => ({
  connId, connName, kind: 'ssh', connState: 'disconnected', shells: 0, connecting: 0, running: 0, pending: 0, ...extra,
});

function dashboard(seq, sessions) {
  return {
    connections: [conn('c1', 'web-01'), conn('c2', 'db-01')],
    requests: [],
    agentPolicies: [],
    mcp: { status: '停止中', port: 8787 },
    storageError: '',
    sessions,
    terminals: [],
    runningTotal: 0,
    sessionsSeq: seq,
    version: '20261101-02',
  };
}

const flush = async () => {
  for (let i = 0; i < 5; i += 1) await new Promise((resolve) => setTimeout(resolve, 0));
};
const deferred = () => { let resolve; const promise = new Promise((r) => { resolve = r; }); return { promise, resolve }; };
const getInitialDataCalls = () => backend.calls.filter((call) => call === 'GetInitialData').length;
const typeInto = async (selector, value) => {
  const element = document.querySelector(selector);
  element.value = value;
  element.dispatchEvent(new Event('input', { bubbles: true }));
  await flush();
};
const poll = async () => {
  vi.advanceTimersByTime(5000);
  await flush();
};
const click = async (selector) => {
  document.querySelector(selector).dispatchEvent(new MouseEvent('click', { bubbles: true }));
  await flush();
};

beforeAll(async () => {
  vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] });
  localStorage.setItem('sshgate-lang', 'ja');
  document.body.innerHTML = '<div id="app"></div>';
  backend.data = dashboard(1, [session('c1', 'web-01'), session('c2', 'db-01')]);
  await import('./main.js');
  await flush();
});

describe('main.js integration', () => {
  it('renders the skeleton once with every container', () => {
    expect(document.querySelectorAll('#page-root')).toHaveLength(1);
    expect(document.querySelectorAll('#terminal-dock #terminal-stack')).toHaveLength(1);
    expect(document.querySelector('#sidebar-status .side-empty')).not.toBeNull();
  });

  it('shows the version from the dashboard data in the sidebar', () => {
    expect(document.querySelector('#sidebar-status .app-version').textContent).toBe('Ver.20261101-02');
  });

  it('keeps tabs and xterm nodes across navigation and polling, while the sidebar updates', async () => {
    await click('[data-nav="terminal"]');
    // No tab yet: the stack stays hidden (phase4 §2.4 stackVisible).
    expect(document.querySelector('#terminal-stack').hidden).toBe(true);
    const select = document.querySelector('[data-action="term-new-select"]');
    select.value = 'c1';
    await click('[data-action="term-new"]');
    await click('[data-action="term-new"]');
    select.value = 'c2';
    document.querySelector('[data-action="term-new-select"]').value = 'c2';
    await click('[data-action="term-new"]');
    const panes = [...document.querySelectorAll('.term-pane')];
    const xterms = [...document.querySelectorAll('.term-pane .xterm')];
    expect(panes).toHaveLength(3);
    expect(document.querySelector('#terminal-stack').hidden).toBe(false);
    // The tab strip is #shell-subtabs (servertabs.js) since phase 4.
    expect([...document.querySelectorAll('#shell-subtabs .shell-subtab')].map((tab) => tab.textContent.trim().split(/\s/)[0])).toEqual(['web-01', 'web-01', 'db-01']);

    // Navigate away and back: nothing is torn down.
    await click('[data-nav="connections"]');
    expect(document.querySelector('#terminal-stack').hidden).toBe(true);
    await click('[data-nav="terminal"]');
    expect(document.querySelectorAll('.term-pane')).toHaveLength(3);

    // A poll on the terminal page updates the sidebar but keeps the xterm DOM.
    const ids = panes.map((pane) => pane.dataset.termId);
    backend.data = dashboard(5, [
      session('c1', 'web-01', { connState: 'connected', shells: 2, connectedSince: new Date().toISOString() }),
      session('c2', 'db-01', { connState: 'connected', shells: 1, connectedSince: new Date().toISOString() }),
    ]);
    backend.data.terminals = ids.map((id, i) => ({ id, connId: i < 2 ? 'c1' : 'c2', connName: i < 2 ? 'web-01' : 'db-01', kind: 'ssh', startedAt: new Date().toISOString(), status: 'open' }));
    vi.advanceTimersByTime(5000);
    await flush();
    expect(document.querySelectorAll('#sidebar-status .side-session-row')).toHaveLength(2);
    expect(document.querySelector('#sidebar-status').textContent).toContain('接続中 2 / 登録 2');
    // Identity, not structure: toEqual on DOM nodes would pass for clones.
    const panesAfter = document.querySelectorAll('.term-pane');
    const xtermsAfter = document.querySelectorAll('.term-pane .xterm');
    expect(panesAfter).toHaveLength(panes.length);
    panes.forEach((pane, i) => expect(panesAfter[i]).toBe(pane));
    xterms.forEach((xterm, i) => expect(xtermsAfter[i]).toBe(xterm));
  });

  it('drops a sessions snapshot older than the one applied', async () => {
    handlers['sessions:changed']({ sessions: [], terminals: [], runningTotal: 0, sessionsSeq: 2 });
    expect(document.querySelectorAll('#sidebar-status .side-session-row')).toHaveLength(2);
    handlers['sessions:changed']({ sessions: [session('c1', 'web-01', { connState: 'error', lastError: 'x' })], terminals: [], runningTotal: 0, sessionsSeq: 9 });
    expect(document.querySelector('#sidebar-status .side-session-row.is-error')).not.toBeNull();
  });

  it('closing one tab keeps the others', async () => {
    const before = document.querySelectorAll('.term-pane').length;
    await click('.term-active-actions [data-action="term-close"]');
    expect(document.querySelectorAll('.term-pane')).toHaveLength(before - 1);
  });

  // N9: the terminal toolbar is innerHTML; a poll or sessions:changed must
  // not replace it while the user is on it.
  it('keeps the new-shell picker, its focus and value through a poll and sessions:changed', async () => {
    const select = document.querySelector('[data-action="term-new-select"]');
    expect(select).not.toBeNull();
    select.focus();
    select.value = 'c2';
    await poll();
    handlers['sessions:changed']({ sessions: [session('c1', 'web-01', { connState: 'error', lastError: 'x' })], terminals: [], runningTotal: 0, sessionsSeq: 12 });
    await flush();
    expect(document.querySelector('[data-action="term-new-select"]')).toBe(select);
    expect(document.activeElement).toBe(select);
    expect(select.value).toBe('c2');
    select.blur();
    await poll();
    expect(document.querySelector('[data-action="term-new-select"]')).not.toBe(select);
  });

  it('switches language without rebuilding the terminal dock', async () => {
    const dock = document.querySelector('#terminal-dock');
    const panes = [...document.querySelectorAll('.term-pane')];
    await click('[data-lang="en"]');
    expect(document.querySelector('#page-title').textContent).toBe('Terminal');
    expect(document.querySelector('[data-action="term-new"]').textContent).toContain('New shell');
    expect(document.querySelector('#terminal-dock')).toBe(dock);
    const panesAfter = document.querySelectorAll('.term-pane');
    expect(panesAfter).toHaveLength(panes.length);
    panes.forEach((pane, i) => expect(panesAfter[i]).toBe(pane));
    await click('[data-lang="ja"]');
  });

  it('asks before leaving an edited connection through the nav', async () => {
    await click('[data-nav="connections"]');
    await typeInto('#connection-form [name="name"]', 'web-01-edited');
    await click('[data-nav="terminal"]');
    expect(document.querySelector('#editor-modal-root .modal')).not.toBeNull();
    expect(document.querySelector('#page-title').textContent).toBe('接続先管理');
    await click('#editor-modal-root [data-modal-dismiss="button"]');
    expect(document.querySelector('#page-title').textContent).toBe('接続先管理');
    expect(document.querySelector('#connection-form [name="name"]').value).toBe('web-01-edited');
    await click('[data-action="edit-cancel"]');
  });

  it('keeps the search box and its focus through a poll', async () => {
    const search = document.querySelector('[data-action="edit-search"]');
    search.focus();
    await typeInto('[data-action="edit-search"]', 'db');
    await poll();
    expect(document.querySelector('[data-action="edit-search"]')).toBe(search);
    expect(document.activeElement).toBe(search);
    await typeInto('[data-action="edit-search"]', '');
    search.blur();
  });

  it('updates the editor connection badge and disconnect button on sessions:changed', async () => {
    await click('[data-action="edit-select"][data-conn-id="c1"]');
    const name = document.querySelector('#connection-form [name="name"]');
    expect(document.querySelector('[data-action="edit-disconnect"]').disabled).toBe(true);
    handlers['sessions:changed']({ sessions: [session('c1', 'web-01', { connState: 'connected', shells: 1, connectedSince: new Date().toISOString() })], terminals: [], runningTotal: 0, sessionsSeq: 20 });
    expect(document.querySelector('[data-action="edit-disconnect"]').disabled).toBe(false);
    expect(document.querySelector('#editor-badges .badge-conn-connected')).not.toBeNull();
    expect(document.querySelector('#connection-form [name="name"]')).toBe(name);
    handlers['sessions:changed']({ sessions: [session('c1', 'web-01')], terminals: [], runningTotal: 0, sessionsSeq: 21 });
    expect(document.querySelector('[data-action="edit-disconnect"]').disabled).toBe(true);
  });

  it('discards a poll that went out while a save was in progress, and never overlaps fetches', async () => {
    const save = deferred();
    backend.save = () => save.promise;
    await click('[data-action="edit-new"]');
    await typeInto('#connection-form [name="name"]', 'app-03');
    document.querySelector('#connection-form').dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
    await flush();

    // A poll leaves while SaveConnection is pending; it sees the old list.
    const gate = deferred();
    backend.gate = gate.promise;
    backend.maxInFlight = 0;
    const before = getInitialDataCalls();
    await poll();
    await poll(); // skipped: one fetch is already queued or running
    expect(getInitialDataCalls()).toBe(before + 1);

    backend.data = { ...backend.data, connections: [...backend.data.connections, conn('c3', 'app-03')], sessionsSeq: 30 };
    save.resolve({ ok: true, id: 'c3', message: '' });
    await flush();
    gate.resolve();
    await flush();
    backend.save = null;

    expect(document.querySelector('#connection-form').dataset.connId).toBe('c3');
    expect(document.querySelector('[data-action="edit-select"][data-conn-id="c3"]').getAttribute('aria-current')).toBe('true');
    expect(getInitialDataCalls()).toBe(before + 2);
    expect(backend.maxInFlight).toBe(1);
  });

  it('keeps the sidebar MCP block live while the settings form is dirty', async () => {
    await click('[data-nav="settings"]');
    await typeInto('#mcp-form [name="tokenName"]', 'edited');
    backend.data = { ...backend.data, mcp: { status: 'エラー', lastError: 'bind failed', port: 8787, connectedAgents: 3 }, sessionsSeq: 40 };
    await poll();
    const sidebar = document.querySelector('#sidebar-status').textContent;
    expect(sidebar).toContain('エラー');
    expect(sidebar).toContain('3');
    // The form itself is left as the user is editing it.
    expect(document.querySelector('#mcp-form [name="tokenName"]').value).toBe('edited');
  });

  it('approves the request whose form was edited even when a new one arrives first', async () => {
    const request = (id, host, command, requestedAt) => ({
      id, connectionId: 'c1', host, user: 'u', command, executedCommand: '', reason: '', requestedBy: 'agent',
      requestedAt, risk: 'low', status: 'pending',
    });
    backend.data = { ...backend.data, mcp: { status: '停止中', port: 8787 }, requests: [request('REQ-A', 'x', 'echo a', '2026-10-27T10:00:00Z')], sessionsSeq: 50 };
    await click('[data-nav="commands"]');
    await poll();
    expect(document.querySelector('#approval-command').value).toBe('echo a');
    await typeInto('#approval-command', 'echo a-edited');

    // B arrives (prepended); the page is not redrawn because of the edit.
    backend.data = { ...backend.data, requests: [request('REQ-B', 'y', 'echo b', '2026-10-27T10:01:00Z'), ...backend.data.requests], sessionsSeq: 51 };
    await poll();
    backend.approve = [];
    await click('[data-action="approve-command"]');
    expect(backend.approve).toEqual([['REQ-A', 'echo a-edited']]);
  });

  // N25 / N5: a language switch redraws the page in the new language and
  // keeps what the user typed there.
  it('keeps an edited approval command through a language switch and approves the edit', async () => {
    await poll();
    await typeInto('#approval-command', 'echo a-lang');
    await click('[data-lang="en"]');
    expect(document.querySelector('#page-title').textContent).not.toBe('コマンド履歴・承認');
    expect(document.querySelector('#approval-command').value).toBe('echo a-lang');
    backend.approve = [];
    await click('[data-action="approve-command"]');
    expect(backend.approve).toEqual([['REQ-A', 'echo a-lang']]);
    await click('[data-lang="ja"]');
  });

  it('keeps unsaved settings input through a language switch', async () => {
    await click('[data-nav="settings"]');
    await typeInto('#mcp-form [name="listenAddress"]', '0.0.0.0');
    await click('[data-lang="en"]');
    expect(document.querySelector('#page-title').textContent).not.toBe('MCPサーバー待受設定');
    expect(document.querySelector('#mcp-form [name="listenAddress"]').value).toBe('0.0.0.0');
    await click('[data-lang="ja"]');
    expect(document.querySelector('#mcp-form [name="listenAddress"]').value).toBe('0.0.0.0');
  });

  // Final audit #11: a typed shared token survives a language switch.
  it('keeps a typed shared token through a language switch', async () => {
    await click('[data-nav="settings"]');
    await typeInto('#mcp-form [name="tokenInput"]', 'new-shared-token');
    await click('[data-lang="en"]');
    expect(document.querySelector('#mcp-form [name="tokenInput"]').value).toBe('new-shared-token');
    await click('[data-lang="ja"]');
    expect(document.querySelector('#mcp-form [name="tokenInput"]').value).toBe('new-shared-token');
  });

  // Final audit #10: generating a token keeps unsaved MCP form edits.
  it('keeps unsaved MCP form edits across token generation', async () => {
    await click('[data-nav="settings"]');
    await typeInto('#mcp-form [name="port"]', '8799');
    await typeInto('#mcp-form [name="allowedOrigins"]', 'http://a.example');
    await click('[data-action="generate-token"]');
    await flush();
    expect(document.querySelector('#mcp-form [name="port"]').value).toBe('8799');
    expect(document.querySelector('#mcp-form [name="allowedOrigins"]').value).toBe('http://a.example');
  });

  // E7: one shape for both ~/.claude.json examples, labeled, no Japanese
  // placeholder in English.
  it('labels the shared-Bearer example and localizes its placeholder', async () => {
    backend.data = { ...backend.data, mcp: { status: '停止中', port: 8790, bearerEnabled: true }, sessionsSeq: 60 };
    // Drops the unsaved form of the previous case, then reloads.
    const confirm = vi.mocked(askConfirm).mockResolvedValue(true);
    await click('[data-action="delete-mcp"]');
    confirm.mockRestore();
    await click('[data-lang="en"]');
    const usage = document.querySelector('#mcp-usage-note').closest('.card-body');
    const code = usage.querySelector('pre code').textContent;
    expect(code).toContain('<issued token>');
    expect(code).not.toMatch(/[\u3040-\u30ff\u4e00-\u9fff]/);
    expect(JSON.parse(code).mcpServers['ssh-gete'].url).toBe('http://127.0.0.1:8790/mcp');
    expect(document.querySelector('#mcp-usage-note').textContent).toContain('shared Bearer token');
    await click('[data-lang="ja"]');
  });

  // E6: in English a Japanese-only backend message is not shown.
  it('shows English for a backend result without a known code', async () => {
    await click('[data-lang="en"]');
    await click('[data-nav="connections"]');
    backend.save = async () => ({ ok: false, message: '接続先名は必須です' });
    await typeInto('#connection-form [name="name"]', 'x');
    document.querySelector('#connection-form').dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
    await flush();
    backend.save = null;
    const toast = document.querySelector('#toast-root, .ssh-toast')?.textContent ?? '';
    expect(document.body.textContent).not.toContain('接続先名は必須です');
    expect(toast).toContain('The operation failed');
    await click('[data-action="edit-cancel"]');
    await click('[data-lang="ja"]');
  });
});
