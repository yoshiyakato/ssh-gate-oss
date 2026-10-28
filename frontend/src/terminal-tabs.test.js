import { describe, it, expect, vi, beforeEach } from 'vitest';

const handlers = {};
const api = vi.hoisted(() => ({
  StartTerminal: vi.fn(), SendTerminalInput: vi.fn(), ResizeTerminal: vi.fn(),
  CloseTerminal: vi.fn(), ListTerminalSessions: vi.fn(), TestConnectionWithPassphrase: vi.fn(),
}));
const eventsOn = vi.hoisted(() => vi.fn());

vi.mock('../wailsjs/go/main/App', () => api);
vi.mock('../wailsjs/runtime/runtime', () => ({ EventsOn: eventsOn }));
vi.mock('./model', () => ({
  escapeHtml: (v) => String(v ?? '').replace(/[&<>"']/g, (c) => `&#${c.charCodeAt(0)};`),
}));
// xterm needs a real layout engine; jsdom gets a recording stand-in.
vi.mock('@xterm/xterm', () => ({
  Terminal: class {
    constructor() { this.cols = 80; this.rows = 24; this._c = 80; this.options = {}; this.written = []; this.cb = null; }
    loadAddon(a) { a.activate(this); }
    open(el) { this.element = document.createElement('div'); el.appendChild(this.element); }
    onData(cb) { this.cb = cb; return { dispose: () => { this.cb = null; } }; }
    write(d) { this.written.push(typeof d === 'string' ? d : new TextDecoder().decode(d)); }
    focus() { this.focused = (this.focused ?? 0) + 1; }
    blur() { this.blurred = (this.blurred ?? 0) + 1; }
    attachCustomKeyEventHandler(fn) { this.keyHandlers = [...(this.keyHandlers ?? []), fn]; }
    dispose() { this.disposed = true; }
  },
}));
vi.mock('@xterm/addon-fit', () => ({
  FitAddon: class { activate(t) { this.t = t; } fit() { this.t.cols = this.t._c; this.t.rows = 24; } },
}));
let mod;
let ctx;
const deferred = () => { let r; const p = new Promise((res) => { r = res; }); return { p, r }; };
const tick = () => new Promise((r) => setTimeout(r, 0));

beforeEach(async () => {
  vi.resetModules();
  Object.values(api).forEach((f) => f.mockReset());
  eventsOn.mockReset();
  eventsOn.mockImplementation((n, cb) => { handlers[n] = cb; });
  api.ResizeTerminal.mockResolvedValue({ ok: true });
  api.CloseTerminal.mockResolvedValue({ ok: true });
  document.body.innerHTML = '<div id="page-root"></div><div id="terminal-dock"></div>';
  mod = await import('./terminal-tabs.js');
  ctx = {
    state: { active: 'terminal', data: { connections: [{ id: 'c1', name: 'web-01', type: 'SSH' }, { id: 'c2', name: 'db', type: 'SSH' }] } },
    t: (k, v) => (v ? `${k}:${JSON.stringify(v)}` : k),
    toast: vi.fn(), renderPage: vi.fn(), navigate: vi.fn(async () => true), promptPassphrase: vi.fn(),
  };
  mod.initTerminalTabs(ctx);
  mod.setTerminalPageVisible(true);
});

describe('terminal-tabs', () => {
  it('registers events once and creates one stack', () => {
    mod.initTerminalTabs(ctx);
    expect(eventsOn.mock.calls.filter(([n]) => n === 'terminal:data')).toHaveLength(1);
    expect(eventsOn.mock.calls.filter(([n]) => n === 'terminal:exit')).toHaveLength(1);
    expect(document.querySelectorAll('#terminal-dock #terminal-stack')).toHaveLength(1);
  });

  it('labels and isolates data, ignores unknown ids, keeps pane on renders', async () => {
    api.StartTerminal.mockResolvedValue({ ok: true });
    const a = await mod.openTerminalTab('c1');
    const b = await mod.openTerminalTab('c1');
    expect(a.ok && b.ok).toBe(true);
    const { tabs } = mod.getTerminalState();
    expect(tabs.get(a.id).label).toBe('web-01 #1');
    expect(tabs.get(b.id).label).toBe('web-01 #2');
    expect(a.id).toMatch(/^TERM-[A-Za-z0-9-]{1,64}$/);
    const pane = tabs.get(a.id).pane;
    handlers['terminal:data']({ id: a.id, data: btoa('hello') });
    handlers['terminal:data']({ id: 'TERM-nope', data: btoa('x') });
    handlers['terminal:data']({ id: b.id, data: btoa('B'), dropped: 5 });
    expect(tabs.get(a.id).xterm.written.join('')).toBe('hello');
    expect(tabs.get(b.id).xterm.written.join('')).toContain('tabs.dropped');
    document.getElementById('page-root').innerHTML = mod.renderTerminalPage(ctx.state, ctx.t);
    expect(document.querySelector(`[data-term-id="${a.id}"].term-pane`)).toBe(pane);
    expect(document.querySelectorAll('.term-tab')).toHaveLength(2);
  });

  it('send queue is serial and ordered', async () => {
    api.StartTerminal.mockResolvedValue({ ok: true });
    const { id } = await mod.openTerminalTab('c1');
    const x = mod.getTerminalState().tabs.get(id).xterm;
    const d1 = deferred();
    const sent = [];
    api.SendTerminalInput.mockImplementation(async (_id, data) => { sent.push(data); if (sent.length === 1) await d1.p; return { ok: true }; });
    x.cb('a'); await tick();
    x.cb('b'); x.cb('c');
    await tick();
    expect(sent).toEqual(['a']);
    d1.r(); await tick(); await tick();
    expect(sent).toEqual(['a', 'bc']);
  });

  it('exit user removes, conn_lost keeps exited read-only with reconnect', async () => {
    api.StartTerminal.mockResolvedValue({ ok: true });
    const a = await mod.openTerminalTab('c1');
    const b = await mod.openTerminalTab('c2');
    handlers['terminal:exit']({ id: a.id, reason: 'user' });
    const { tabs } = mod.getTerminalState();
    expect(tabs.has(a.id)).toBe(false);
    handlers['terminal:exit']({ id: b.id, reason: 'conn_lost', message: 'eof' });
    expect(tabs.get(b.id).status).toBe('exited');
    expect(tabs.get(b.id).xterm.options.disableStdin).toBe(true);
    const html = mod.renderTerminalPage(ctx.state, ctx.t);
    expect(html).toContain('term-reconnect');
    // reconnect keeps xterm and order position
    const x = tabs.get(b.id).xterm;
    await mod.reconnectTerminalTab(b.id);
    const st = mod.getTerminalState();
    expect(st.order).toHaveLength(1);
    expect(st.order[0]).not.toBe(b.id);
    expect(st.tabs.get(st.order[0]).xterm).toBe(x);
    expect(st.tabs.get(st.order[0]).status).toBe('open');
  });

  it('exit before StartTerminal resolves stays exited', async () => {
    const d = deferred();
    api.StartTerminal.mockReturnValue(d.p);
    const pr = mod.openTerminalTab('c1');
    await tick();
    const id = mod.getTerminalState().order[0];
    handlers['terminal:exit']({ id, reason: 'remote_exit', exitCode: 3 });
    d.r({ ok: true });
    const res = await pr;
    expect(res.ok).toBe(true);
    expect(mod.getTerminalState().tabs.get(id).status).toBe('exited');
    expect(mod.getTerminalState().tabs.get(id).xterm.written.join('')).toContain('(3)');
  });

  it('output that arrives before StartTerminal resolves (the MOTD) is shown', async () => {
    const d = deferred();
    api.StartTerminal.mockReturnValue(d.p);
    const pr = mod.openTerminalTab('c1');
    await tick();
    const id = mod.getTerminalState().order[0];
    expect(mod.getTerminalState().tabs.get(id).status).toBe('starting');
    handlers['terminal:data']({ id, data: btoa('welcome') });
    d.r({ ok: true });
    expect((await pr).ok).toBe(true);
    expect(mod.getTerminalState().tabs.get(id).xterm.written.join('')).toContain('welcome');
  });

  it('closeTabsForConnection drops a reconnecting tab and exited tabs of that connection only', async () => {
    api.StartTerminal.mockResolvedValue({ ok: true });
    const a = await mod.openTerminalTab('c1');
    const b = await mod.openTerminalTab('c1');
    const other = await mod.openTerminalTab('c2');
    handlers['terminal:exit']({ id: a.id, reason: 'conn_lost' });
    handlers['terminal:exit']({ id: b.id, reason: 'conn_lost' });
    // b reconnects; Disconnect cancels it in the backend (no terminal:exit).
    const d = deferred();
    api.StartTerminal.mockReturnValue(d.p);
    const reconnecting = mod.reconnectTerminalTab(b.id);
    await tick();
    expect(mod.closeTabsForConnection('c1')).toBe(2);
    d.r({ ok: false, code: 'canceled', message: '接続を中止しました' });
    await reconnecting;
    const { order, tabs } = mod.getTerminalState();
    expect(order).toEqual([other.id]);
    expect(tabs.has(other.id)).toBe(true);
    expect(document.querySelectorAll('.term-pane')).toHaveLength(1);
  });

  it('needs_passphrase prompts and retries once', async () => {
    api.StartTerminal.mockResolvedValueOnce({ ok: false, code: 'needs_passphrase', message: 'pp' }).mockResolvedValueOnce({ ok: true });
    ctx.promptPassphrase.mockResolvedValue('secret');
    api.TestConnectionWithPassphrase.mockResolvedValue({ ok: true });
    const res = await mod.openTerminalTab('c1');
    expect(ctx.promptPassphrase).toHaveBeenCalledTimes(1);
    expect(api.TestConnectionWithPassphrase).toHaveBeenCalledWith('c1', 'secret');
    expect(api.StartTerminal).toHaveBeenCalledTimes(2);
    expect(res.ok).toBe(true);
  });

  it('needs_password / keyring_unavailable go through ctx.recoverCredential once', async () => {
    api.StartTerminal.mockResolvedValueOnce({ ok: false, code: 'needs_password', message: 'gone' }).mockResolvedValueOnce({ ok: true });
    ctx.recoverCredential = vi.fn(async () => true);
    const res = await mod.openTerminalTab('c1');
    expect(ctx.recoverCredential).toHaveBeenCalledTimes(1);
    expect(ctx.recoverCredential.mock.calls[0][1]).toMatchObject({ code: 'needs_password' });
    expect(api.StartTerminal).toHaveBeenCalledTimes(2);
    expect(res.ok).toBe(true);

    // Dismissed: the tab goes away without a toast; no third start.
    api.StartTerminal.mockReset();
    api.StartTerminal.mockResolvedValue({ ok: false, code: 'keyring_unavailable', message: 'locked' });
    ctx.recoverCredential = vi.fn(async () => false);
    ctx.toast.mockClear();
    const dismissed = await mod.openTerminalTab('c1');
    expect(dismissed).toMatchObject({ ok: false, code: 'canceled' });
    expect(api.StartTerminal).toHaveBeenCalledTimes(1);
    expect(ctx.toast).not.toHaveBeenCalled();
    expect(mod.terminalTabCount('c1')).toBe(1);
  });

  it('keeps the exit reason for the toolbar and clears it on reconnect', async () => {
    api.StartTerminal.mockResolvedValue({ ok: true });
    const { id } = await mod.openTerminalTab('c1');
    handlers['terminal:exit']({ id, reason: 'device_removed' });
    const entry = mod.getTerminalState().tabs.get(id);
    expect(entry.status).toBe('exited');
    expect(entry.exitReason).toBe('device_removed');
    await mod.reconnectTerminalTab(id);
    expect(entry.exitReason).toBe('');
    expect(entry.id).not.toBe(id);
  });

  it('failure removes tab and toasts', async () => {
    api.StartTerminal.mockResolvedValue({ ok: false, code: 'connect_failed', message: 'boom' });
    const res = await mod.openTerminalTab('c1');
    expect(res.ok).toBe(false);
    expect(mod.terminalTabCount()).toBe(0);
    expect(ctx.toast).toHaveBeenCalledWith('boom');
  });

  it('resize only on change', async () => {
    api.StartTerminal.mockResolvedValue({ ok: true });
    const { id } = await mod.openTerminalTab('c1');
    expect(api.StartTerminal).toHaveBeenCalledWith(id, 'c1', 80, 24);
    const entry = mod.getTerminalState().tabs.get(id);
    api.ResizeTerminal.mockClear();
    mod.setTerminalPageVisible(false); mod.setTerminalPageVisible(true);
    expect(api.ResizeTerminal).not.toHaveBeenCalled();
    entry.xterm._c = 120;
    mod.setTerminalPageVisible(false); mod.setTerminalPageVisible(true);
    expect(api.ResizeTerminal).toHaveBeenCalledWith(id, 120, 24);
  });

  it('restore creates tabs from ListTerminalSessions and reconcile marks lost ones', async () => {
    api.ListTerminalSessions.mockResolvedValue([
      { id: 'TERM-a', connId: 'c1', connName: 'web-01', kind: 'ssh', startedAt: '2026-01-01T00:00:00Z', status: 'open' },
      { id: 'TERM-b', connId: 'c1', connName: 'web-01', kind: 'ssh', startedAt: '2026-01-01T00:01:00Z', status: 'open' },
    ]);
    await mod.restoreTerminalTabs();
    await mod.restoreTerminalTabs();
    const { tabs, order } = mod.getTerminalState();
    expect(order).toEqual(['TERM-a', 'TERM-b']);
    expect(tabs.get('TERM-b').label).toBe('web-01 #2');
    expect(tabs.get('TERM-a').xterm.written[0]).toContain('tabs.restored');
    mod.reconcileTerminalTabs([{ id: 'TERM-a', connName: 'web-02' }]);
    mod.reconcileTerminalTabs([{ id: 'TERM-a', connName: 'web-02' }]);
    expect(tabs.get('TERM-b').status).toBe('exited');
    expect(tabs.get('TERM-a').label).toBe('web-02 #1');
  });

  it('click handler routes term-* actions', async () => {
    api.StartTerminal.mockResolvedValue({ ok: true });
    document.getElementById('page-root').innerHTML = mod.renderTerminalPage(ctx.state, ctx.t);
    document.querySelector('[data-action="term-new-select"]').value = 'c2';
    expect(mod.handleTerminalChange({ target: document.querySelector('[data-action="term-new-select"]') })).toBe(true);
    expect(await mod.handleTerminalClick({ target: document.querySelector('[data-action="term-new"]') }, ctx)).toBe(true);
    expect(api.StartTerminal.mock.calls[0][1]).toBe('c2');
    const id = mod.getTerminalState().order[0];
    document.getElementById('page-root').innerHTML = mod.renderTerminalPage(ctx.state, ctx.t);
    expect(await mod.handleTerminalClick({ target: document.querySelector('[data-action="term-close"]') }, ctx)).toBe(true);
    expect(api.CloseTerminal).toHaveBeenCalledWith(id);
    expect(mod.terminalTabCount()).toBe(0);
    expect(await mod.handleTerminalClick({ target: document.body }, ctx)).toBe(false);
  });

  it('attaches ctx.terminalKeyHandler to each new xterm once, and keeps it on reconnect', async () => {
    api.StartTerminal.mockResolvedValue({ ok: true });
    const handler = vi.fn(() => true);
    ctx.terminalKeyHandler = handler;
    const a = await mod.openTerminalTab('c1');
    const b = await mod.openTerminalTab('c2');
    const { tabs } = mod.getTerminalState();
    expect(tabs.get(a.id).xterm.keyHandlers).toEqual([handler]);
    expect(tabs.get(b.id).xterm.keyHandlers).toEqual([handler]);
    handlers['terminal:exit']({ id: a.id, reason: 'conn_lost' });
    const x = tabs.get(a.id).xterm;
    await mod.reconnectTerminalTab(a.id);
    expect(x.keyHandlers).toEqual([handler]);
  });

  it('without ctx.terminalKeyHandler nothing is attached', async () => {
    api.StartTerminal.mockResolvedValue({ ok: true });
    const { id } = await mod.openTerminalTab('c1');
    expect(mod.getTerminalState().tabs.get(id).xterm.keyHandlers).toBeUndefined();
  });

  it('renderTerminalNewShell is the .term-new part only and stays wired to term-new', async () => {
    api.StartTerminal.mockResolvedValue({ ok: true });
    const root = document.getElementById('page-root');
    root.innerHTML = mod.renderTerminalNewShell(ctx.state, ctx.t);
    expect(root.querySelectorAll('.term-new')).toHaveLength(1);
    expect(root.querySelector('.term-tabs')).toBeNull();
    expect([...root.querySelectorAll('option')].map((o) => o.value)).toEqual(['c1', 'c2']);
    root.querySelector('[data-action="term-new-select"]').value = 'c2';
    mod.handleTerminalChange({ target: root.querySelector('[data-action="term-new-select"]') });
    expect(await mod.handleTerminalClick({ target: root.querySelector('[data-action="term-new"]') }, ctx)).toBe(true);
    expect(api.StartTerminal.mock.calls[0][1]).toBe('c2');
    // A user-controlled name reaches the page escaped.
    const evil = { data: { connections: [{ id: 'cx', name: '<img src=x onerror=alert(1)>', type: 'SSH' }] } };
    root.innerHTML = mod.renderTerminalNewShell(evil, ctx.t);
    expect(root.querySelector('img')).toBeNull();
    expect(root.querySelector('option').textContent).toContain('<img src=x onerror=alert(1)>');
  });

  it('renderTerminalPage with serverFilter lists that connection only and drops .term-new', async () => {
    api.StartTerminal.mockResolvedValue({ ok: true });
    const a = await mod.openTerminalTab('c1');
    const b = await mod.openTerminalTab('c2');
    const root = document.getElementById('page-root');
    root.innerHTML = mod.renderTerminalPage(ctx.state, ctx.t, { serverFilter: 'c1' });
    expect([...root.querySelectorAll('[data-action="term-activate"]')].map((el) => el.dataset.termId)).toEqual([a.id]);
    expect(root.querySelector('.term-new')).toBeNull();
    root.innerHTML = mod.renderTerminalPage(ctx.state, ctx.t);
    expect([...root.querySelectorAll('[data-action="term-activate"]')].map((el) => el.dataset.termId)).toEqual([a.id, b.id]);
    expect(root.querySelector('.term-new')).not.toBeNull();
    mod.closeTabsForConnection('c1');
    root.innerHTML = mod.renderTerminalPage(ctx.state, ctx.t, { serverFilter: 'c1' });
    expect(root.querySelector('.term-empty').textContent).toContain('tabs.emptyFiltered:{"name":"web-01"}');
    expect(mod.renderTerminalEmpty(ctx.state, ctx.t)).toContain('tabs.emptyHint');
  });

  it('setTerminalServerFilter hides and blurs other panes; close stays on the same server', async () => {
    api.StartTerminal.mockResolvedValue({ ok: true });
    const a1 = await mod.openTerminalTab('c1');
    const b1 = await mod.openTerminalTab('c2');
    const a2 = await mod.openTerminalTab('c1');
    const { tabs } = mod.getTerminalState();
    mod.activateTerminalTab(b1.id);
    expect(mod.setTerminalServerFilter('c1')).toBe(true);
    expect(mod.setTerminalServerFilter('c1')).toBe(false);
    expect(mod.getTerminalServerFilter()).toBe('c1');
    expect(tabs.get(a1.id).pane.hidden).toBe(false);
    expect(tabs.get(a2.id).pane.hidden).toBe(false);
    expect(tabs.get(b1.id).pane.hidden).toBe(true);
    expect(tabs.get(b1.id).xterm.blurred).toBe(1);
    // A hidden active pane is never focused or measured.
    const before = tabs.get(b1.id).xterm.focused ?? 0;
    mod.activateTerminalTab(b1.id);
    await tick();
    expect(tabs.get(b1.id).xterm.focused ?? 0).toBe(before);
    expect(tabs.get(b1.id).needsFit).toBe(true);
    // New tabs of another connection start hidden.
    const b2 = await mod.openTerminalTab('c2');
    expect(tabs.get(b2.id).pane.hidden).toBe(true);
    // Closing the active c1 tab picks the other c1 tab, not the c2 neighbour.
    mod.activateTerminalTab(a1.id);
    await mod.closeTerminalTab(a1.id);
    expect(mod.getTerminalState().activeId).toBe(a2.id);
    // Back to all: every pane may show again.
    expect(mod.setTerminalServerFilter('')).toBe(true);
    for (const entry of tabs.values()) expect(entry.pane.hidden).toBe(false);
  });
  // Final audit #12: in English the toast detail is not the backend's
  // Japanese message.
  it('input_timeout toast detail goes through resultMessage', async () => {
    api.StartTerminal.mockResolvedValue({ ok: true });
    const { id } = await mod.openTerminalTab('c1');
    const entry = mod.getTerminalState().tabs.get(id);
    ctx.resultMessage = vi.fn(() => 'Sending input is stuck');
    ctx.toast.mockClear?.();
    api.SendTerminalInput.mockImplementation(async () => ({ ok: false, code: 'input_timeout', message: '入力の送信が詰まっています' }));
    entry.xterm.cb('x'); await tick(); await tick();
    expect(entry.exitReason).toBe('input_dropped');
    expect(ctx.resultMessage).toHaveBeenCalledWith(expect.objectContaining({ code: 'input_timeout' }));
    const last = ctx.toast.mock.calls.at(-1)[0];
    expect(last).toContain('Sending input is stuck');
    expect(last).not.toContain('入力の送信が詰まっています');
    delete ctx.resultMessage;
    api.SendTerminalInput.mockReset?.();
  });
  // audit-2 N6: lost input must not be followed by later keys.
  it('input_timeout makes the tab read-only, closes the session and offers reconnect', async () => {
    api.StartTerminal.mockResolvedValue({ ok: true });
    const { id } = await mod.openTerminalTab('c1');
    const entry = mod.getTerminalState().tabs.get(id);
    const x = entry.xterm;
    const d1 = deferred();
    const sent = [];
    api.SendTerminalInput.mockImplementation(async (_id, data) => {
      sent.push(data);
      if (sent.length === 1) {
        await d1.p;
        return { ok: false, code: 'input_timeout', message: '入力の送信が詰まっています' };
      }
      return { ok: true };
    });
    x.cb('rm -rf /var/'); await tick();
    x.cb('tmp/cache'); // pending while the first call hangs
    d1.r(); await tick(); await tick();
    expect(sent).toEqual(['rm -rf /var/']);
    expect(entry.status).toBe('exited');
    expect(entry.exitReason).toBe('input_dropped');
    expect(x.options.disableStdin).toBe(true);
    expect(x.written.join('')).toContain('tabs.inputDropped');
    expect(ctx.toast).toHaveBeenCalledWith(expect.stringContaining('tabs.inputDropped'));
    await tick();
    expect(api.CloseTerminal).toHaveBeenCalledWith(id);
    // Later keys go nowhere.
    x.cb('\r'); await tick(); await tick();
    expect(sent).toEqual(['rm -rf /var/']);
    // The backend's exit for the closed session keeps the tab.
    handlers['terminal:exit']({ id, reason: 'user' });
    expect(mod.getTerminalState().tabs.get(id)).toBe(entry);
    expect(entry.exitReason).toBe('input_dropped');
    expect(mod.renderTerminalPage(ctx.state, ctx.t)).toContain('term-reconnect');
    // Reconnect brings input back on a new session.
    await mod.reconnectTerminalTab(id);
    expect(entry.status).toBe('open');
    expect(entry.exitReason).toBe('');
    expect(x.options.disableStdin).toBe(false);
    x.cb('ls\r'); await tick(); await tick();
    expect(sent).toEqual(['rm -rf /var/', 'ls\r']);
  });

  it('a rejected SendTerminalInput call also blocks input', async () => {
    api.StartTerminal.mockResolvedValue({ ok: true });
    const { id } = await mod.openTerminalTab('c1');
    const entry = mod.getTerminalState().tabs.get(id);
    api.SendTerminalInput.mockRejectedValue(new Error('ipc down'));
    entry.xterm.cb('a'); await tick(); await tick();
    expect(entry.exitReason).toBe('input_dropped');
    expect(ctx.toast).toHaveBeenCalledWith(expect.stringContaining('ipc down'));
  });

  it('no_session does not block or toast (terminal:exit tells the user)', async () => {
    api.StartTerminal.mockResolvedValue({ ok: true });
    const { id } = await mod.openTerminalTab('c1');
    const entry = mod.getTerminalState().tabs.get(id);
    api.SendTerminalInput.mockResolvedValue({ ok: false, code: 'no_session', message: 'gone' });
    entry.xterm.cb('a'); await tick(); await tick();
    expect(entry.status).toBe('open');
    expect(entry.exitReason).toBe('');
    expect(ctx.toast).not.toHaveBeenCalled();
    expect(api.CloseTerminal).not.toHaveBeenCalled();
  });

  it('has JA and EN text for input_dropped', () => {
    for (const lang of ['ja', 'en']) {
      expect(mod.TERMINAL_I18N[lang]['tabs.exit.input_dropped']).toBeTruthy();
      expect(mod.TERMINAL_I18N[lang]['tabs.inputDropped']).toBeTruthy();
    }
  });

  // #38 / N22: one resize in flight per tab; the last one sent is the latest.
  it('resizes are sent one at a time and collapse to the latest size', async () => {
    api.StartTerminal.mockResolvedValue({ ok: true });
    const { id } = await mod.openTerminalTab('c1');
    const entry = mod.getTerminalState().tabs.get(id);
    const d1 = deferred();
    const calls = [];
    api.ResizeTerminal.mockReset();
    api.ResizeTerminal.mockImplementation(async (_id, cols) => { calls.push(cols); if (calls.length === 1) await d1.p; return { ok: true }; });
    const resizeTo = (cols) => { entry.xterm._c = cols; mod.setTerminalPageVisible(false); mod.setTerminalPageVisible(true); };
    resizeTo(100);
    resizeTo(110);
    resizeTo(120);
    expect(calls).toEqual([100]);
    d1.r(); await tick(); await tick();
    expect(calls).toEqual([100, 120]);
    expect(entry.resizing).toBe(false);
    resizeTo(130);
    expect(calls).toEqual([100, 120, 130]);
    await tick();
    expect(api.ResizeTerminal).toHaveBeenLastCalledWith(id, 130, 24);
  });
  // A failed resize (rejected or ok:false) must not leave lastSize at the
  // size the PTY never took, or resizing back to it would be skipped.
  it.each([
    ['ok:false', () => Promise.resolve({ ok: false, message: 'x' })],
    ['rejection', () => Promise.reject(new Error('x'))],
  ])('resends a size whose resize failed (%s)', async (_label, fail) => {
    api.StartTerminal.mockResolvedValue({ ok: true });
    const { id } = await mod.openTerminalTab('c1');
    const entry = mod.getTerminalState().tabs.get(id);
    const calls = [];
    api.ResizeTerminal.mockReset();
    api.ResizeTerminal.mockImplementation((_id, cols) => { calls.push(cols); return calls.length === 1 ? fail() : Promise.resolve({ ok: true }); });
    const resizeTo = (cols) => { entry.xterm._c = cols; mod.setTerminalPageVisible(false); mod.setTerminalPageVisible(true); };
    resizeTo(120);
    await tick(); await tick();
    expect(entry.lastSize).toBeNull();
    expect(entry.resizing).toBe(false);
    resizeTo(120);
    await tick();
    expect(calls).toEqual([120, 120]);
    expect(entry.lastSize).toEqual({ cols: 120, rows: 24 });
  });
});
