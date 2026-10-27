import { describe, it, expect, vi, beforeAll } from 'vitest';

// Startup restore when the first GetInitialData fails (WebView reload while
// the bridge is not ready): the first poll that succeeds must still rebuild
// the tabs of the live backend terminals.

const backend = vi.hoisted(() => ({ failNext: true, listCalls: 0 }));

vi.mock('admin-lte/dist/js/adminlte.min.js', () => ({}));
vi.mock('@xterm/xterm', () => ({
  Terminal: class {
    constructor() { this.cols = 80; this.rows = 24; this.options = {}; this.written = []; }
    loadAddon(addon) { addon.activate?.(this); }
    open(el) { this.element = document.createElement('div'); this.element.className = 'xterm'; el.appendChild(this.element); }
    onData() { return { dispose() {} }; }
    write(data) { this.written.push(typeof data === 'string' ? data : new TextDecoder().decode(data)); }
    focus() {}
    dispose() {}
  },
}));
vi.mock('@xterm/addon-fit', () => ({ FitAddon: class { activate() {} fit() {} } }));
vi.mock('../wailsjs/runtime/runtime', () => ({ EventsOn: () => {} }));
vi.mock('../wailsjs/go/main/App', () => {
  const ok = async () => ({ ok: true, message: '' });
  const terminals = [
    { id: 'TERM-a', connId: 'c1', connName: 'web-01', kind: 'ssh', startedAt: '2026-10-27T10:00:00Z', status: 'open' },
    { id: 'TERM-b', connId: 'c1', connName: 'web-01', kind: 'ssh', startedAt: '2026-10-27T10:01:00Z', status: 'open' },
  ];
  return {
    GetInitialData: async () => {
      if (backend.failNext) {
        backend.failNext = false;
        throw new Error('runtime not ready');
      }
      return {
        connections: [{ id: 'c1', name: 'web-01', type: 'SSH', host: 'h', port: 22, user: 'u', authMethod: 'SSH鍵認証', credential: '~/.ssh/id', tags: [] }],
        requests: [],
        agentPolicies: [],
        mcp: { status: '停止中', port: 8787 },
        storageError: '',
        sessions: [{ connId: 'c1', connName: 'web-01', kind: 'ssh', connState: 'connected', shells: 2, connecting: 0, running: 0, pending: 0 }],
        terminals,
        runningTotal: 0,
        sessionsSeq: 1,
      };
    },
    ListTerminalSessions: async () => {
      backend.listCalls += 1;
      return terminals;
    },
    StartTerminal: ok, SendTerminalInput: ok, ResizeTerminal: ok, CloseTerminal: ok,
    TestConnectionWithPassphrase: ok, SaveConnection: ok, TestConnectionConfig: ok,
    DisconnectConnection: ok, DeleteConnection: ok, ListSerialPorts: async () => [],
    ApproveCommand: ok, ApproveCommandAndBypassAgent: ok, ApproveCommandConfirmed: ok,
    DeleteCommandRequest: ok, DeleteMCPSettings: ok, GenerateMCPToken: ok, RejectCommand: ok,
    RevokeAgentApprovalBypass: ok, SaveMCPSettings: ok,
  };
});

const flush = async () => {
  for (let i = 0; i < 5; i += 1) await new Promise((resolve) => setTimeout(resolve, 0));
};

beforeAll(async () => {
  vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] });
  localStorage.setItem('sshgate-lang', 'ja');
  document.body.innerHTML = '<div id="app"></div>';
  await import('./main.js');
  await flush();
});

describe('startup restore after a failed first refresh', () => {
  it('restores tabs on the first poll that succeeds, once', async () => {
    expect(document.querySelector('.fatal-error')).not.toBeNull();
    expect(document.querySelectorAll('.term-pane')).toHaveLength(0);

    vi.advanceTimersByTime(5000);
    await flush();
    expect(document.querySelectorAll('.term-pane')).toHaveLength(2);
    expect(backend.listCalls).toBe(1);

    vi.advanceTimersByTime(5000);
    await flush();
    expect(document.querySelectorAll('.term-pane')).toHaveLength(2);
    expect(backend.listCalls).toBe(1);
  });
});
