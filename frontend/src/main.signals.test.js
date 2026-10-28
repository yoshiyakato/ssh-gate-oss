import { describe, it, expect, vi, beforeAll } from 'vitest';
import { phase4Dashboard, TERMINALS } from './__fixtures__/phase4.js';
import { matchShortcut } from './shortcuts.js';

// REQ-017 wiring of the signal menu into main.js (req017-signals-wiring.md
// §5): the menu sits in the active tab's toolbar, survives polls, sends
// through SendTerminalSignal (SSH signals) or the xterm input path (control
// characters), confirms KILL with the terminal shortcuts off, and none of its
// keys is a terminal shortcut (desktop or browser).

const handlers = vi.hoisted(() => ({}));
const backend = vi.hoisted(() => ({ data: null, approve: [], terminals: [], closed: [], signals: [], inputs: [] }));

vi.mock('admin-lte/dist/js/adminlte.min.js', () => ({}));
vi.mock('@xterm/xterm', () => ({
  Terminal: class {
    constructor() { this.cols = 80; this.rows = 24; this.options = {}; this.written = []; this.keyHandlers = []; }
    loadAddon(addon) { addon.activate?.(this); }
    open(el) { this.element = document.createElement('div'); this.element.className = 'xterm'; el.appendChild(this.element); }
    onData(cb) { this.dataCb = cb; return { dispose() {} }; }
    // xterm 6: input() fires onData like a keystroke (signals.js relies on it).
    input(data) { this.dataCb?.(data); }
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
    SendTerminalInput: async (id, data) => {
      backend.inputs.push([id, data]);
      return { ok: true, message: '' };
    },
    SendTerminalSignal: async (id, sig) => {
      backend.signals.push([id, sig]);
      return { ok: true, code: '', message: '', sessionId: id };
    },
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

const toggle = () => document.querySelector('.term-active-actions .term-signal-slot [data-signal-toggle]');
const item = (id) => document.querySelector(`.term-signal-menu [data-signal-id="${id}"]`);
const keydown = (target, key, extra = {}) => {
  const ev = new KeyboardEvent('keydown', { key, bubbles: true, cancelable: true, ...extra });
  target.dispatchEvent(ev);
  return ev;
};

describe('REQ-017 signal menu in the terminal toolbar', () => {
  beforeAll(async () => {
    await boot();
    await click('[data-nav="terminal"]');
    await click(tabButton('c-web'));
    await click('#shell-subtabs .shell-subtab[data-term-id="TERM-T1"]');
  });

  it('sits before the close button and is the same node after a poll', async () => {
    const slot = document.querySelector('.term-active-actions .term-signal-slot');
    expect(slot).not.toBeNull();
    expect(slot.nextElementSibling?.dataset.action).toBe('term-close');
    const menuRoot = slot.querySelector('.term-signal');
    expect(toggle().disabled).toBe(false);
    expect(toggle().textContent).toContain('シグナル');
    await poll();
    expect(document.querySelector('.term-signal-slot .term-signal')).toBe(menuRoot);
  });

  it('keeps a pointer-opened menu open through a poll and sends HUP to the shown tab', async () => {
    backend.signals = [];
    toggle().dispatchEvent(new MouseEvent('click', { bubbles: true }));
    expect(document.querySelector('.term-signal-menu').hidden).toBe(false);
    await poll();
    expect(document.querySelector('.term-signal-menu').hidden).toBe(false);
    await click('.term-signal-menu [data-signal-id="HUP"]');
    expect(backend.signals).toEqual([['TERM-T1', 'HUP']]);
    expect(toastText()).toContain('HUP');
  });

  it('sends Ctrl-C through the xterm input path, not SendTerminalSignal', async () => {
    backend.signals = [];
    backend.inputs = [];
    toggle().dispatchEvent(new MouseEvent('click', { bubbles: true }));
    await click('.term-signal-menu [data-signal-id="ctrl-c"]');
    await flush();
    expect(backend.signals).toEqual([]);
    expect(backend.inputs).toContainEqual(['TERM-T1', '\x03']);
  });

  it('asks before KILL, with the terminal shortcuts off while asking', async () => {
    backend.signals = [];
    backend.closed = [];
    toggle().dispatchEvent(new MouseEvent('click', { bubbles: true }));
    await click('.term-signal-menu [data-signal-id="KILL"]');
    const dialog = document.querySelector('#editor-modal-root .modal');
    expect(dialog).not.toBeNull();
    // Ctrl+Shift+W must not close the tab under the dialog.
    document.dispatchEvent(new KeyboardEvent('keydown', { key: 'W', code: 'KeyW', ctrlKey: true, shiftKey: true, bubbles: true, cancelable: true }));
    await flush();
    expect(backend.closed).toEqual([]);
    // A poll while confirming does not redraw the toolbar away.
    const menuRoot = document.querySelector('.term-signal-slot .term-signal');
    await poll();
    expect(document.querySelector('.term-signal-slot .term-signal')).toBe(menuRoot);
    keydown(document.activeElement ?? dialog, 'Escape');
    await flush();
    expect(document.querySelector('#editor-modal-root .modal')).toBeNull();
    expect(backend.signals).toEqual([]);
  });

  it('keyboard use of the menu never triggers a terminal shortcut', async () => {
    backend.closed = [];
    toggle().focus();
    const down = keydown(toggle(), 'ArrowDown');
    expect(down.defaultPrevented).toBe(true);
    expect(document.activeElement).toBe(item('ctrl-c'));
    await poll();
    // Redraw is held off while the menu has the keyboard focus.
    expect(document.activeElement).toBe(item('ctrl-c'));
    keydown(document.activeElement, 'End');
    keydown(document.activeElement, 'Escape');
    expect(document.querySelector('.term-signal-menu').hidden).toBe(true);
    expect(document.activeElement).toBe(toggle());
    toggle().blur();
    expect(backend.closed).toEqual([]);
  });

  it('is disabled for an exited tab', async () => {
    handlers['terminal:exit']({ id: 'TERM-T1', reason: 'remote_exit' });
    await flush();
    expect(toggle().disabled).toBe(true);
    await click(tabButton(''));
  });
});

describe('REQ-017 menu keys and terminal shortcuts do not overlap', () => {
  // Keys the signal menu reacts to (signals.js onToggleKeydown / onMenuKeydown).
  const menuKeys = [
    { key: 'ArrowDown', code: 'ArrowDown' }, { key: 'ArrowUp', code: 'ArrowUp' },
    { key: 'Home', code: 'Home' }, { key: 'End', code: 'End' },
    { key: 'Enter', code: 'Enter' }, { key: ' ', code: 'Space' },
    { key: 'Escape', code: 'Escape' }, { key: 'Tab', code: 'Tab' },
  ];
  // The control characters the menu sends, as typed into xterm.
  const controlKeys = [
    { key: 'c', code: 'KeyC', ctrlKey: true }, { key: 'z', code: 'KeyZ', ctrlKey: true },
    { key: '\\', code: 'Backslash', ctrlKey: true }, { key: 'd', code: 'KeyD', ctrlKey: true },
  ];

  for (const platform of ['mac', 'other', 'browser']) {
    it(`no menu key or control character is a ${platform} shortcut`, () => {
      for (const ev of [...menuKeys, ...controlKeys]) {
        expect(matchShortcut(ev, platform), JSON.stringify(ev)).toBeNull();
        expect(matchShortcut({ ...ev, shiftKey: true }, platform), JSON.stringify(ev)).toBeNull();
      }
    });
  }
});
