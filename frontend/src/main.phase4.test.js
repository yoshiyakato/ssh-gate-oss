import { describe, it, expect, vi, beforeAll } from 'vitest';
import { phase4Dashboard, TERMINALS } from './__fixtures__/phase4.js';

// Phase 4 wiring of main.js (phase4-ui-integration.md §5.2): the server tabs
// filter every screen, unknown connections stay under 「すべて」, the filter
// survives a reload, and the shortcuts reach the filtered sub-tabs.

const handlers = vi.hoisted(() => ({}));
const backend = vi.hoisted(() => ({ data: null, approve: [], terminals: [], closed: [] }));

vi.mock('admin-lte/dist/js/adminlte.min.js', () => ({}));
vi.mock('@xterm/xterm', () => ({
  Terminal: class {
    constructor() { this.cols = 80; this.rows = 24; this.options = {}; this.written = []; this.keyHandlers = []; }
    loadAddon(addon) { addon.activate?.(this); }
    open(el) { this.element = document.createElement('div'); this.element.className = 'xterm'; el.appendChild(this.element); }
    onData() { return { dispose() {} }; }
    write(data) { this.written.push(typeof data === 'string' ? data : new TextDecoder().decode(data)); }
    attachCustomKeyEventHandler(fn) { this.keyHandlers.push(fn); }
    focus() {}
    blur() {}
    dispose() {}
  },
}));
vi.mock('@xterm/addon-fit', () => ({ FitAddon: class { activate() {} fit() {} } }));
vi.mock('../wailsjs/runtime/runtime', () => ({
  EventsOn: (name, cb) => { handlers[name] = cb; },
  Environment: async () => ({ platform: 'linux' }),
}));
vi.mock('../wailsjs/go/main/App', () => {
  const ok = async () => ({ ok: true, message: '' });
  return {
    GetInitialData: async () => structuredClone(backend.data),
    ListTerminalSessions: async () => structuredClone(backend.terminals),
    StartTerminal: async (id) => ({ ok: true, sessionId: id, message: '' }),
    SendTerminalInput: ok,
    ResizeTerminal: ok,
    CloseTerminal: async (id) => {
      backend.closed.push(id);
      return { ok: true, message: '' };
    },
    TestConnectionWithPassphrase: ok,
    TestConnectionWithPassphraseSave: ok,
    SaveConnection: async (conn) => ({ ok: true, id: conn.id, message: '' }),
    TestConnectionConfig: ok,
    DisconnectConnection: ok,
    DeleteConnection: ok,
    ListSerialPorts: async () => [],
    GetHostKeyInfo: async () => ({ keys: [] }),
    ResetHostKey: ok,
    GetApprovalTarget: async (id) => ({ requestId: id, found: true, executable: true, fpChanged: false }),
    ApproveCommand: async (id, command) => {
      backend.approve.push([id, command]);
      return { ok: true, message: '' };
    },
    ApproveCommandAndBypassAgent: ok,
    ApproveCommandConfirmed: ok,
    RejectCommand: ok,
    RevokeAgentApprovalBypass: ok,
    DeleteCommandRequest: ok,
    DeleteMCPSettings: ok,
    GenerateMCPToken: ok,
    SaveMCPSettings: ok,
    ListAgentTokens: async () => [],
    ListAgentApprovalBypasses: async () => [],
    PlaintextBackups: async () => [],
    DeletePlaintextBackups: ok,
    UseCredentialForSession: ok,
  };
});

const flush = async () => {
  for (let i = 0; i < 8; i += 1) await new Promise((resolve) => setTimeout(resolve, 0));
};
const click = async (selector) => {
  const element = document.querySelector(selector);
  if (!element) throw new Error(`no element for ${selector}`);
  element.dispatchEvent(new MouseEvent('click', { bubbles: true }));
  await flush();
};
const poll = async () => {
  vi.advanceTimersByTime(5000);
  await flush();
};
const typeInto = async (selector, value) => {
  const element = document.querySelector(selector);
  element.value = value;
  element.dispatchEvent(new Event('input', { bubbles: true }));
  await flush();
};
const ids = (selector) => [...document.querySelectorAll(selector)].map((el) => el.dataset.selectRequest);
const queueIds = () => ids('.ssh-list [data-select-request]');
const historyIds = () => ids('tbody tr[data-select-request]');
const unknownIds = (selector) => [...document.querySelectorAll(selector)]
  .filter((el) => el.querySelector('.unknown-conn'))
  .map((el) => el.dataset.selectRequest);
const selectedTab = () => document.querySelector('#server-tabs [aria-selected="true"]')?.dataset.connId;
const tabButton = (connId) => `#server-tabs [data-conn-id="${connId}"]`;
const subtabIds = () => [...document.querySelectorAll('#shell-subtabs .shell-subtab')].map((el) => el.dataset.termId);
const toastText = () => document.querySelector('#toast-root')?.textContent ?? '';

async function boot({ saved, lang = 'ja' } = {}) {
  vi.clearAllTimers();
  vi.resetModules();
  localStorage.clear();
  localStorage.setItem('sshgate-lang', lang);
  if (saved !== undefined) localStorage.setItem('sshgate-server-filter', saved);
  document.body.innerHTML = '<div id="app"></div>';
  backend.data = phase4Dashboard(1);
  backend.terminals = structuredClone(TERMINALS);
  await import('./main.js');
  await flush();
}

beforeAll(() => {
  vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] });
});

describe('phase 4 server filter across the screens', () => {
  beforeAll(async () => {
    await boot();
    // T3 (db-stg) has ended; it stays as an exited tab.
    handlers['terminal:exit']({ id: 'TERM-T3', reason: 'remote_exit' });
    await flush();
  });

  it('filters every screen by the selected tab and keeps unknown connections under 「すべて」', async () => {
    // 1. Approval queue under 「すべて」.
    await click('[data-nav="commands"]');
    expect(selectedTab()).toBe('');
    expect(queueIds()).toEqual(['R1', 'R3', 'R4']);
    expect(unknownIds('.ssh-list [data-select-request]')).toEqual(['R4']);
    expect(historyIds()).toEqual(['R1', 'R2', 'R3', 'R4', 'R5']);
    expect(unknownIds('tbody tr[data-select-request]')).toEqual(['R4', 'R5']);
    // 「すべて」 counts the unknown pending one too; the nav badge never filters.
    expect(document.querySelector(`${tabButton('')} .server-tab-pending`).textContent).toContain('3');

    // 2. web-01 tab.
    await click(tabButton('c-web'));
    expect(selectedTab()).toBe('c-web');
    expect(queueIds()).toEqual(['R1']);
    expect(historyIds()).toEqual(['R1', 'R2']);
    expect(document.querySelector('.approvals-hidden-pending').textContent).toContain('2');
    expect(document.querySelector('[data-nav="commands"] .nav-badge').textContent).toBe('3');

    // 3. Connections: the list follows the tab but is not filtered.
    await click('[data-nav="connections"]');
    expect(document.querySelector('[data-action="edit-select"][aria-current="true"]').dataset.connId).toBe('c-web');
    expect(document.querySelectorAll('[data-action="edit-select"]')).toHaveLength(3);

    // 4. List → tab.
    await click('[data-action="edit-select"][data-conn-id="c-db"]');
    expect(selectedTab()).toBe('c-db');

    // 5. Terminal: only db-stg's tab.
    await click('[data-nav="terminal"]');
    expect(document.querySelector('#shell-subtabs').hidden).toBe(false);
    expect(subtabIds()).toEqual(['TERM-T3']);
    expect(document.querySelector('#terminal-stack').hidden).toBe(false);
    // The ended tab offers a reconnect in the toolbar.
    expect(document.querySelector('.term-active-actions [data-action="term-reconnect"]')).not.toBeNull();

    // 6. router-01 has no shell: the stack is hidden, only 「＋新しいシェル」.
    await click(tabButton('c-rt'));
    expect(subtabIds()).toEqual([]);
    expect(document.querySelector('#shell-subtabs .shell-subtab-new')).not.toBeNull();
    expect(document.querySelector('#terminal-stack').hidden).toBe(true);
    expect(document.querySelector('.term-empty').textContent).toContain('router-01 のシェルはありません');
    // Other servers' panes are hidden too.
    expect([...document.querySelectorAll('.term-pane')].every((pane) => pane.hidden)).toBe(true);

    // 7. 「すべて」: every tab labelled name #n; R4 is back in the queue.
    await click(tabButton(''));
    expect(subtabIds()).toHaveLength(3);
    for (const el of document.querySelectorAll('#shell-subtabs .shell-subtab')) {
      expect(el.textContent).toMatch(/^(web-01|db-stg) #\d/);
    }
    await click('[data-nav="commands"]');
    expect(queueIds()).toContain('R4');
  });

  it('never approves a request outside the filter', async () => {
    await click('[data-nav="commands"]');
    await click(tabButton(''));
    await click('.ssh-list [data-select-request="R3"]');
    expect(document.querySelector('#approval-command').dataset.requestId).toBe('R3');
    // R3 stays selected internally, but it is not on screen under web-01.
    await click(tabButton('c-web'));
    expect(document.querySelector('#approval-command').dataset.requestId).toBe('R1');
    backend.approve = [];
    await click('[data-action="approve-command"]');
    expect(backend.approve.map(([id]) => id)).toEqual(['R1']);
    await click(tabButton(''));
  });

  it('keeps the server tab nodes and focus through a poll', async () => {
    const button = document.querySelector(tabButton('c-db'));
    button.focus();
    await poll();
    expect(document.querySelector(tabButton('c-db'))).toBe(button);
    expect(document.activeElement).toBe(button);
    button.blur();
  });

  it('keeps the filter and the edit when the unsaved-changes dialog is cancelled', async () => {
    await click('[data-nav="connections"]');
    await click(tabButton('c-web'));
    expect(document.querySelector('[data-action="edit-select"][aria-current="true"]').dataset.connId).toBe('c-web');
    await typeInto('#connection-form [name="name"]', 'web-01-edited');
    await click(tabButton('c-db'));
    expect(document.querySelector('#editor-modal-root .modal')).not.toBeNull();
    await click('#editor-modal-root [data-modal-dismiss="button"]');
    expect(selectedTab()).toBe('c-web');
    expect(document.querySelector('[data-action="edit-select"][aria-current="true"]').dataset.connId).toBe('c-web');
    expect(document.querySelector('#connection-form [name="name"]').value).toBe('web-01-edited');
    expect(localStorage.getItem('sshgate-server-filter')).toBe('c-web');
    await click('[data-action="edit-cancel"]');
  });

  it('runs the shortcuts on the filtered sub-tabs, once per key press', async () => {
    await click('[data-nav="terminal"]');
    await click(tabButton('c-web'));
    expect(subtabIds()).toEqual(['TERM-T1', 'TERM-T2']);

    // Ctrl+Shift+2 (not mac in jsdom): the 2nd sub-tab of web-01.
    const second = new KeyboardEvent('keydown', { key: '2', code: 'Digit2', ctrlKey: true, shiftKey: true, bubbles: true });
    document.dispatchEvent(second);
    await flush();
    expect(document.querySelector('#shell-subtabs .shell-subtab.is-active').dataset.termId).toBe('TERM-T2');

    // Ctrl+Shift+Alt+→ through the xterm handler, then the same event on
    // document: the server moves once (web-01 → db-stg).
    const next = new KeyboardEvent('keydown', { key: 'ArrowRight', code: 'ArrowRight', ctrlKey: true, shiftKey: true, altKey: true, bubbles: true });
    const pane = document.querySelector('.term-pane[data-term-id="TERM-T2"]');
    expect(pane).not.toBeNull();
    // Every xterm got the one shared handler.
    const { getTerminalState } = await import('./terminal-tabs.js');
    const xterm = getTerminalState().tabs.get('TERM-T2').xterm;
    expect(xterm.keyHandlers).toHaveLength(1);
    expect(xterm.keyHandlers[0](next)).toBe(false);
    document.dispatchEvent(next);
    await flush();
    expect(selectedTab()).toBe('c-db');

    // Ctrl+Shift+Alt+0: 「すべて」.
    document.dispatchEvent(new KeyboardEvent('keydown', { key: '0', code: 'Digit0', ctrlKey: true, shiftKey: true, altKey: true, bubbles: true }));
    await flush();
    expect(selectedTab()).toBe('');
  });

  it('does not take server shortcuts from a text input on other pages', async () => {
    await click('[data-nav="connections"]');
    const search = document.querySelector('[data-action="edit-search"]');
    search.focus();
    const event = new KeyboardEvent('keydown', { key: 'ArrowRight', code: 'ArrowRight', ctrlKey: true, shiftKey: true, altKey: true, bubbles: true, cancelable: true });
    search.dispatchEvent(event);
    await flush();
    expect(selectedTab()).toBe('');
    expect(event.defaultPrevented).toBe(false);
    search.blur();
    // Without focus in an input it moves.
    document.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowRight', code: 'ArrowRight', ctrlKey: true, shiftKey: true, altKey: true, bubbles: true }));
    await flush();
    expect(selectedTab()).toBe('c-web');
    await click(tabButton(''));
  });

  it('falls back to 「すべて」 with a toast when the selected connection is deleted', async () => {
    await click('[data-nav="commands"]');
    await click(tabButton('c-db'));
    expect(localStorage.getItem('sshgate-server-filter')).toBe('c-db');
    const next = phase4Dashboard(50);
    next.connections = next.connections.filter((c) => c.id !== 'c-db');
    next.sessions = next.sessions.filter((s) => s.connId !== 'c-db');
    backend.data = next;
    await poll();
    expect(selectedTab()).toBe('');
    expect(localStorage.getItem('sshgate-server-filter')).toBe('');
    expect(toastText()).toContain('選んでいた接続先が無くなったため');
    // R3 now belongs to no connection: it stays under 「すべて」 as unknown.
    expect(queueIds()).toContain('R3');
    expect(unknownIds('.ssh-list [data-select-request]')).toContain('R3');
  });
});

describe('phase 4 server filter at startup', () => {
  it('restores the saved filter', async () => {
    await boot({ saved: 'c-db' });
    expect(selectedTab()).toBe('c-db');
    await click('[data-nav="commands"]');
    expect(queueIds()).toEqual(['R3']);
  });

  it('drops a saved filter whose connection is gone, silently', async () => {
    await boot({ saved: 'c-gone' });
    expect(selectedTab()).toBe('');
    expect(localStorage.getItem('sshgate-server-filter')).toBe('');
    expect(toastText()).toBe('');
  });
});

describe('phase 4 terminal toolbar, closeTab and the sidebar', () => {
  beforeAll(async () => {
    await boot();
  });

  const ctrlShiftW = () => new KeyboardEvent('keydown', { key: 'W', code: 'KeyW', ctrlKey: true, shiftKey: true, bubbles: true, cancelable: true });

  it('closeTab never closes a hidden tab of another server', async () => {
    const { getTerminalState } = await import('./terminal-tabs.js');
    await click('[data-nav="terminal"]');
    await click(tabButton('c-web'));
    expect(subtabIds()).toEqual(['TERM-T1', 'TERM-T2']);
    // router-01 has no shells; the raw active id stays on web-01's tab.
    await click(tabButton('c-rt'));
    expect(subtabIds()).toEqual([]);
    expect(getTerminalState().activeId).toMatch(/^TERM-T[12]$/);
    backend.closed = [];
    document.dispatchEvent(ctrlShiftW());
    await flush();
    expect(backend.closed).toEqual([]);
    expect(getTerminalState().tabs.size).toBe(3);

    // On db-stg the shown tab is the one closed.
    await click(tabButton('c-db'));
    expect(subtabIds()).toEqual(['TERM-T3']);
    document.dispatchEvent(ctrlShiftW());
    await flush();
    expect(backend.closed).toEqual(['TERM-T3']);
    expect(getTerminalState().tabs.has('TERM-T1')).toBe(true);
    expect(getTerminalState().tabs.has('TERM-T2')).toBe(true);
    await click(tabButton(''));
  });

  it('a pulled USB device says 「デバイスが外れました」 in the toolbar, with a reconnect', async () => {
    await click('[data-nav="terminal"]');
    await click(tabButton('c-web'));
    await click('#shell-subtabs .shell-subtab[data-term-id="TERM-T1"]');
    handlers['terminal:exit']({ id: 'TERM-T1', reason: 'device_removed' });
    await flush();
    const actions = document.querySelector('.term-active-actions');
    expect(actions.classList.contains('is-device-removed')).toBe(true);
    expect(document.querySelector('.term-active-note').textContent).toContain('デバイスが外れました');
    expect(actions.querySelector('[data-action="term-reconnect"]')).not.toBeNull();
    await click(tabButton(''));
  });

  it('cancelling the discard-edit dialog from a sidebar row keeps the commands page and the edit', async () => {
    await click('[data-nav="commands"]');
    await click(tabButton(''));
    await click('.ssh-list [data-select-request="R1"]');
    await typeInto('#approval-command', 'echo edited');
    const confirm = vi.spyOn(window, 'confirm').mockReturnValue(false);
    await click('[data-action="side-open-conn"][data-conn-id="c-db"]');
    expect(confirm).toHaveBeenCalled();
    confirm.mockRestore();
    expect(selectedTab()).toBe('');
    expect(document.querySelector('#approval-command')).not.toBeNull();
    expect(document.querySelector('#approval-command').value).toBe('echo edited');
    expect(document.querySelector('#approval-command').dataset.requestId).toBe('R1');
  });
});

describe('phase 4 terminal toolbar in English', () => {
  it('a pulled USB device says "Device removed"', async () => {
    await boot({ lang: 'en' });
    await click('[data-nav="terminal"]');
    await click(tabButton('c-db'));
    handlers['terminal:exit']({ id: 'TERM-T3', reason: 'device_removed' });
    await flush();
    expect(document.querySelector('.term-active-actions').classList.contains('is-device-removed')).toBe(true);
    expect(document.querySelector('.term-active-note').textContent).toContain('Device removed');
  });
});
