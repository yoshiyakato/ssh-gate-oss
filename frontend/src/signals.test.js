import { describe, it, expect, vi, beforeEach } from 'vitest';
import { interpolate, mergeI18n } from './model.js';
import {
  SIGNALS_I18N, SSH_SIGNAL_NAMES, confirmSignalDialog, createSignalSender, normalizeSignalResult, renderSignalMenu,
  signalItems, signalResultMessage, signalResultToast,
} from './signals.js';

const I18N = mergeI18n(SIGNALS_I18N);
const tFor = (lang) => (key, vars) => interpolate(I18N[lang][key] ?? key, vars);
const t = tFor('ja');

function key(target, k, extra = {}) {
  const ev = new KeyboardEvent('keydown', { key: k, bubbles: true, cancelable: true, ...extra });
  target.dispatchEvent(ev);
  return ev;
}
const flush = () => new Promise((r) => setTimeout(r, 0));

function setup(extra = {}) {
  const container = document.createElement('div');
  document.body.replaceChildren(container);
  const onSend = vi.fn(async () => undefined);
  const focusTerminal = vi.fn();
  const confirm = vi.fn(async () => true);
  const onResult = vi.fn();
  const menu = renderSignalMenu(container, { kind: 'ssh', disabled: false, t, onSend, focusTerminal, confirm, onResult, ...extra });
  const toggle = container.querySelector('[data-signal-toggle]');
  const list = container.querySelector('[role="menu"]');
  const item = (id) => list.querySelector(`[data-signal-id="${id}"]`);
  return { container, menu, toggle, list, item, onSend, focusTerminal, confirm, onResult };
}

describe('SIGNALS_I18N', () => {
  it('has matching ja/en keys under the sig. prefix', () => {
    expect(Object.keys(SIGNALS_I18N.ja).sort()).toEqual(Object.keys(SIGNALS_I18N.en).sort());
    for (const k of Object.keys(SIGNALS_I18N.ja)) expect(k.startsWith('sig.')).toBe(true);
  });
  it('covers every item label, group and confirm text', () => {
    for (const kind of ['ssh', 'serial']) {
      for (const item of signalItems(kind)) {
        expect(SIGNALS_I18N.ja[item.labelKey]).toBeTruthy();
        expect(SIGNALS_I18N.ja[`sig.group.${item.group}`]).toBeTruthy();
        if (item.confirm) expect(SIGNALS_I18N.ja[`sig.confirm.${item.id}`]).toBeTruthy();
      }
    }
  });
});

describe('signalItems', () => {
  it('ssh: control characters, then TERM KILL HUP INT', () => {
    const items = signalItems('ssh');
    expect(items.map((i) => i.id)).toEqual(['ctrl-c', 'ctrl-z', 'ctrl-backslash', 'ctrl-d', 'TERM', 'KILL', 'HUP', 'INT']);
    expect(items.slice(0, 4).map((i) => i.send.data)).toEqual(['\x03', '\x1a', '\x1c', '\x04']);
    expect(items.slice(4).map((i) => i.send.signal)).toEqual(SSH_SIGNAL_NAMES);
    expect(items.filter((i) => i.confirm).map((i) => i.id).sort()).toEqual(['KILL', 'TERM']);
  });
  it('wire names match App.SendTerminalSignal', () => {
    expect(signalItems('ssh').map((i) => i.send.wire)).toEqual(['INT_CHAR', 'TSTP_CHAR', 'QUIT_CHAR', 'EOF_CHAR', 'TERM', 'KILL', 'HUP', 'INT']);
    expect(signalItems('serial')[0].send.wire).toBe('BREAK');
  });
  it('backend ActionResult { ok: true, code: \'\' } is a success', () => {
    expect(normalizeSignalResult({ ok: true, code: '', message: 'Ctrl-C を送信しました' }).code).toBe('ok');
  });
  it('serial: BREAK only', () => {
    expect(signalItems('serial').map((i) => [i.id, i.send.type])).toEqual([['BREAK', 'break']]);
  });
});

describe('normalizeSignalResult / signalResultMessage', () => {
  it('normalizes the shapes onSend may return', () => {
    expect(normalizeSignalResult(undefined)).toEqual({ ok: true, code: 'ok', message: '' });
    expect(normalizeSignalResult(true).ok).toBe(true);
    expect(normalizeSignalResult(false)).toEqual({ ok: false, code: 'signal_failed', message: '' });
    expect(normalizeSignalResult('signal_unsupported')).toEqual({ ok: false, code: 'signal_unsupported', message: '' });
    expect(normalizeSignalResult({ ok: true })).toEqual({ ok: true, code: 'ok', message: '' });
    expect(normalizeSignalResult({ ok: false, code: 'signal_rejected', message: 'x' })).toEqual({ ok: false, code: 'signal_rejected', message: 'x' });
    expect(normalizeSignalResult({ ok: false })).toEqual({ ok: false, code: 'signal_failed', message: '' });
    expect(normalizeSignalResult({ code: 'session_closed' })).toEqual({ ok: false, code: 'session_closed', message: '' });
  });
  it('turns codes into text in both languages', () => {
    expect(signalResultMessage('signal_unsupported', t, { name: 'KILL' })).toContain('KILL');
    expect(signalResultMessage('signal_unsupported', t, { name: 'KILL' })).toContain('対応していない可能性があります');
    expect(signalResultMessage('signal_unsupported', tFor('en'), { name: 'KILL' })).toContain('may not support');
    expect(signalResultMessage('ok', t, { name: 'Ctrl-C' })).toBe('Ctrl-C を送りました');
    expect(signalResultMessage('break_unsupported', tFor('en'))).toBe('This serial port does not support BREAK');
    expect(signalResultMessage('cancelled', t)).toBe('');
    expect(signalResultMessage('weird_code', tFor('en'), { name: 'HUP' })).toBe('Could not send HUP (weird_code)');
  });
  it('never returns a raw key for a listed code', () => {
    // the failure codes App.SendTerminalSignal returns (terminal_signal.go)
    for (const code of ['signal_unsupported', 'signal_failed', 'invalid_signal', 'break_unsupported',
      'break_failed', 'no_session', 'not_ready',
      'input_timeout', 'signal_busy', 'signal_not_applicable', 'signal_unavailable']) {
      for (const lang of ['ja', 'en']) {
        const text = signalResultMessage(code, tFor(lang), { name: 'X' });
        expect(text).not.toMatch(/^sig\./);
        expect(text).not.toBe(signalResultMessage('weird', tFor(lang), { name: 'X' }).replace('weird', code));
      }
    }
  });
});

describe('renderSignalMenu', () => {
  beforeEach(() => { document.body.replaceChildren(); });

  it('renders a closed menu button with ARIA wiring', () => {
    const { toggle, list } = setup();
    expect(toggle.getAttribute('aria-haspopup')).toBe('menu');
    expect(toggle.getAttribute('aria-expanded')).toBe('false');
    expect(toggle.getAttribute('aria-controls')).toBe(list.id);
    expect(toggle.textContent).toBe('シグナル');
    expect(list.hidden).toBe(true);
    expect(list.querySelectorAll('[role="menuitem"]')).toHaveLength(8);
    expect(list.querySelectorAll('[role="separator"]')).toHaveLength(1);
  });

  it('puts the separator between the control characters and the SSH signals', () => {
    const { list } = setup();
    const ids = [...list.children].map((li) => li.getAttribute('role') === 'separator' ? '|' : li.querySelector('[data-signal-id]')?.dataset.signalId ?? 'h');
    expect(ids).toEqual(['h', 'ctrl-c', 'ctrl-z', 'ctrl-backslash', 'ctrl-d', '|', 'h', 'TERM', 'KILL', 'HUP', 'INT']);
  });

  it('serial shows BREAK only', async () => {
    const { list, item, toggle, onSend } = setup({ kind: 'serial' });
    expect([...list.querySelectorAll('[role="menuitem"]')].map((b) => b.dataset.signalId)).toEqual(['BREAK']);
    toggle.click();
    item('BREAK').click();
    await flush();
    expect(onSend).toHaveBeenCalledWith({ kind: 'serial', id: 'BREAK', type: 'break', wire: 'BREAK' });
  });

  it('uses textContent (labels are not parsed as HTML)', () => {
    const evil = (k) => (k === 'sig.ctrlC' ? '<img src=x onerror=alert(1)>' : t(k));
    const { list, item } = setup({ t: evil });
    expect(list.querySelector('img')).toBeNull();
    expect(item('ctrl-c').textContent).toContain('<img src=x onerror=alert(1)>');
  });

  it('click opens; clicking an item sends the control byte and refocuses xterm', async () => {
    const { toggle, list, item, onSend, focusTerminal, confirm, onResult, menu } = setup();
    toggle.click();
    expect(menu.isOpen()).toBe(true);
    expect(list.hidden).toBe(false);
    expect(toggle.getAttribute('aria-expanded')).toBe('true');
    item('ctrl-c').click();
    await flush();
    expect(onSend).toHaveBeenCalledWith({ kind: 'ssh', id: 'ctrl-c', type: 'control', data: '\x03', wire: 'INT_CHAR' });
    expect(confirm).not.toHaveBeenCalled();
    expect(menu.isOpen()).toBe(false);
    expect(focusTerminal).toHaveBeenCalled();
    expect(onResult).toHaveBeenCalledWith({ ok: true, code: 'ok', message: '' }, { id: 'ctrl-c', name: 'Ctrl-C', type: 'control' });
  });

  it('mousedown on the button and items is prevented so xterm keeps focus', () => {
    const { toggle, item } = setup();
    const down = (target) => {
      const ev = new MouseEvent('mousedown', { bubbles: true, cancelable: true, button: 0 });
      target.dispatchEvent(ev);
      return ev.defaultPrevented;
    };
    expect(down(toggle)).toBe(true);
    expect(down(item('HUP'))).toBe(true);
  });

  it('HUP and INT are sent without confirmation', async () => {
    const { menu, onSend, confirm } = setup();
    await menu.activate('HUP');
    await menu.activate('INT');
    expect(confirm).not.toHaveBeenCalled();
    expect(onSend.mock.calls.map((c) => c[0].signal)).toEqual(['HUP', 'INT']);
  });

  it('KILL asks first and sends only when confirmed', async () => {
    const { menu, onSend, confirm, focusTerminal } = setup();
    confirm.mockResolvedValueOnce(false);
    const cancelled = await menu.activate('KILL');
    expect(cancelled.code).toBe('cancelled');
    expect(onSend).not.toHaveBeenCalled();
    expect(focusTerminal).toHaveBeenCalledTimes(1);
    expect(confirm.mock.calls[0][0]).toEqual({
      title: 'KILL を送りますか',
      message: SIGNALS_I18N.ja['sig.confirm.KILL'],
      okLabel: 'KILL を送る',
      signal: 'KILL',
    });
    confirm.mockResolvedValueOnce(true);
    const sent = await menu.activate('KILL');
    expect(sent.ok).toBe(true);
    expect(onSend).toHaveBeenCalledWith({ kind: 'ssh', id: 'KILL', type: 'signal', signal: 'KILL', wire: 'KILL' });
  });

  it('TERM asks first too', async () => {
    const { menu, confirm, onSend } = setup();
    await menu.activate('TERM');
    expect(confirm).toHaveBeenCalledTimes(1);
    expect(confirm.mock.calls[0][0].signal).toBe('TERM');
    expect(onSend).toHaveBeenCalledWith({ kind: 'ssh', id: 'TERM', type: 'signal', signal: 'TERM', wire: 'TERM' });
  });

  it('never sends KILL/TERM without a confirm function', async () => {
    const { menu, onSend } = setup({ confirm: undefined });
    expect((await menu.activate('KILL')).code).toBe('cancelled');
    expect((await menu.activate('TERM')).code).toBe('cancelled');
    expect(onSend).not.toHaveBeenCalled();
  });

  it('a throwing confirm counts as no', async () => {
    const { menu, onSend } = setup({ confirm: vi.fn(async () => { throw new Error('modal gone'); }) });
    expect((await menu.activate('KILL')).code).toBe('cancelled');
    expect(onSend).not.toHaveBeenCalled();
  });

  it('does not open a second confirm while one is pending', async () => {
    let resolve;
    const confirm = vi.fn(() => new Promise((r) => { resolve = r; }));
    const { menu, onSend } = setup({ confirm });
    const first = menu.activate('KILL');
    expect(await menu.activate('TERM')).toBeNull();
    expect(confirm).toHaveBeenCalledTimes(1);
    resolve(true);
    await first;
    expect(onSend).toHaveBeenCalledTimes(1);
  });

  it('does not send if the session went away while confirming', async () => {
    let resolve;
    const { menu, onSend } = setup({ confirm: () => new Promise((r) => { resolve = r; }) });
    const p = menu.activate('KILL');
    menu.update({ disabled: true });
    resolve(true);
    expect((await p).code).toBe('cancelled');
    expect(onSend).not.toHaveBeenCalled();
  });

  it('passes the target tab id to onSend', async () => {
    const { menu, onSend } = setup({ target: 'tab-1' });
    await menu.activate('ctrl-c');
    expect(onSend.mock.calls[0][0]).toMatchObject({ target: 'tab-1', wire: 'INT_CHAR', kind: 'ssh' });
  });

  it('does not send KILL to another tab if the active tab changed while confirming', async () => {
    let resolve;
    const { menu, onSend, focusTerminal } = setup({ target: 'tab-1', confirm: () => new Promise((r) => { resolve = r; }) });
    const p = menu.activate('KILL');
    menu.update({ target: 'tab-2' });
    resolve(true);
    expect((await p).code).toBe('cancelled');
    expect(onSend).not.toHaveBeenCalled();
    expect(focusTerminal).toHaveBeenCalled();
  });

  it('does not send if the tab kind changed while confirming', async () => {
    let resolve;
    const { menu, onSend } = setup({ target: 'tab-1', confirm: () => new Promise((r) => { resolve = r; }) });
    const p = menu.activate('TERM');
    menu.update({ kind: 'serial' });
    resolve(true);
    expect((await p).code).toBe('cancelled');
    expect(onSend).not.toHaveBeenCalled();
  });

  it('isBusy() covers open menu, pending confirm and pending send', async () => {
    let resolveConfirm;
    let resolveSend;
    const { menu } = setup({
      confirm: () => new Promise((r) => { resolveConfirm = r; }),
      onSend: () => new Promise((r) => { resolveSend = r; }),
    });
    expect(menu.isBusy()).toBe(false);
    menu.open();
    expect(menu.isBusy()).toBe(true);
    menu.close();
    expect(menu.isBusy()).toBe(false);
    const p = menu.activate('KILL');
    expect(menu.isBusy()).toBe(true);
    resolveConfirm(true);
    await flush();
    expect(menu.isBusy()).toBe(true);
    resolveSend({ ok: true, code: '' });
    expect((await p).ok).toBe(true);
    expect(menu.isBusy()).toBe(false);
  });

  it('update({ lang }) redraws labels with the same t function', () => {
    let lang = 'ja';
    const tt = (k, v) => tFor(lang)(k, v);
    const { menu, toggle, item } = setup({ t: tt, lang: 'ja' });
    expect(toggle.textContent).toBe('シグナル');
    lang = 'en';
    menu.update({ t: tt, lang: 'en' });
    expect(toggle.textContent).toBe('Signal');
    expect(item('KILL').textContent).toContain('Kill');
  });

  it('mount() moves the same menu into a new slot', () => {
    const { menu } = setup();
    const slot = document.createElement('div');
    document.body.append(slot);
    menu.mount(slot);
    expect(slot.firstChild).toBe(menu.element);
    menu.mount(slot);
    expect(slot.children).toHaveLength(1);
  });

  it('a menu whose slot was redrawn away closes on the next pointer press', () => {
    const { menu, container } = setup();
    menu.open();
    container.replaceChildren();
    menu.element.dispatchEvent(new MouseEvent('pointerdown', { bubbles: true }));
    document.body.dispatchEvent(new MouseEvent('pointerdown', { bubbles: true }));
    expect(menu.isOpen()).toBe(false);
  });

  it('reports backend result codes and a throw from onSend', async () => {
    const onSend = vi.fn()
      .mockResolvedValueOnce({ ok: false, code: 'signal_unsupported' })
      .mockRejectedValueOnce(new Error('boom'));
    const { menu, onResult } = setup({ onSend });
    expect(await menu.activate('HUP')).toEqual({ ok: false, code: 'signal_unsupported', message: '' });
    expect(await menu.activate('INT')).toEqual({ ok: false, code: 'signal_failed', message: 'boom' });
    expect(onResult.mock.calls.map((c) => c[0].code)).toEqual(['signal_unsupported', 'signal_failed']);
  });

  it('a BREAK throw maps to break_failed', async () => {
    const { menu } = setup({ kind: 'serial', onSend: vi.fn(async () => { throw new Error('x'); }) });
    expect((await menu.activate('BREAK')).code).toBe('break_failed');
  });

  it('items of the other kind are ignored', async () => {
    const { menu, onSend } = setup({ kind: 'serial' });
    expect(await menu.activate('KILL')).toBeNull();
    expect(onSend).not.toHaveBeenCalled();
  });

  it('disabled: button disabled, nothing opens or sends', async () => {
    const { menu, toggle, onSend } = setup({ disabled: true });
    expect(toggle.disabled).toBe(true);
    expect(toggle.title).toBe(SIGNALS_I18N.ja['sig.menuDisabled']);
    menu.open();
    expect(menu.isOpen()).toBe(false);
    expect(await menu.activate('ctrl-c')).toBeNull();
    expect(onSend).not.toHaveBeenCalled();
  });

  it('update() switches kind, language and disabled state', () => {
    const { menu, toggle, list } = setup();
    menu.open();
    menu.update({ disabled: true });
    expect(menu.isOpen()).toBe(false);
    expect(toggle.disabled).toBe(true);
    menu.update({ disabled: false, kind: 'serial', t: tFor('en') });
    expect(toggle.disabled).toBe(false);
    expect(toggle.textContent).toBe('Signal');
    expect([...list.querySelectorAll('[role="menuitem"]')].map((b) => b.dataset.signalId)).toEqual(['BREAK']);
    expect(list.querySelector('[data-signal-id="BREAK"]').textContent).toContain('Send BREAK');
  });

  describe('keyboard', () => {
    it('ArrowDown on the button opens and focuses the first item; arrows wrap', () => {
      const { toggle, item, menu } = setup();
      toggle.focus();
      const ev = key(toggle, 'ArrowDown');
      expect(ev.defaultPrevented).toBe(true);
      expect(menu.isOpen()).toBe(true);
      expect(document.activeElement).toBe(item('ctrl-c'));
      key(document.activeElement, 'ArrowDown');
      expect(document.activeElement).toBe(item('ctrl-z'));
      key(document.activeElement, 'ArrowUp');
      key(document.activeElement, 'ArrowUp');
      expect(document.activeElement).toBe(item('INT'));
      key(document.activeElement, 'ArrowDown');
      expect(document.activeElement).toBe(item('ctrl-c'));
      key(document.activeElement, 'End');
      expect(document.activeElement).toBe(item('INT'));
      key(document.activeElement, 'Home');
      expect(document.activeElement).toBe(item('ctrl-c'));
    });

    it('roving tabindex: only the focused item is tabbable', () => {
      const { toggle, list } = setup();
      key(toggle, 'ArrowDown');
      key(document.activeElement, 'ArrowDown');
      const tabbable = [...list.querySelectorAll('[role="menuitem"]')].filter((b) => b.getAttribute('tabindex') === '0');
      expect(tabbable).toEqual([document.activeElement]);
    });

    it('ArrowUp on the button opens at the last item', () => {
      const { toggle, item } = setup();
      key(toggle, 'ArrowUp');
      expect(document.activeElement).toBe(item('INT'));
    });

    it('Enter activates the focused item', async () => {
      const { toggle, onSend, focusTerminal } = setup();
      key(toggle, 'Enter');
      key(document.activeElement, 'ArrowDown');
      key(document.activeElement, 'Enter');
      await flush();
      expect(onSend).toHaveBeenCalledWith({ kind: 'ssh', id: 'ctrl-z', type: 'control', data: '\x1a', wire: 'TSTP_CHAR' });
      expect(focusTerminal).toHaveBeenCalled();
    });

    it('Escape closes and returns focus to the button', () => {
      const { toggle, menu } = setup();
      key(toggle, 'ArrowDown');
      const ev = key(document.activeElement, 'Escape');
      expect(ev.defaultPrevented).toBe(true);
      expect(menu.isOpen()).toBe(false);
      expect(document.activeElement).toBe(toggle);
    });

    it('menu keys do not bubble to document shortcut handlers', () => {
      const { toggle } = setup();
      const seen = vi.fn();
      document.addEventListener('keydown', seen);
      key(toggle, 'ArrowDown');
      key(document.activeElement, 'ArrowDown');
      document.removeEventListener('keydown', seen);
      expect(seen).not.toHaveBeenCalled();
    });

    it('Tab closes the menu', () => {
      const { toggle, menu } = setup();
      key(toggle, 'ArrowDown');
      key(document.activeElement, 'Tab');
      expect(menu.isOpen()).toBe(false);
    });
  });

  it('a pointer press outside closes the menu', () => {
    const { menu } = setup();
    menu.open();
    const outside = document.createElement('div');
    document.body.append(outside);
    outside.dispatchEvent(new MouseEvent('pointerdown', { bubbles: true }));
    expect(menu.isOpen()).toBe(false);
  });

  it('clicking the open button closes it and refocuses xterm', () => {
    const { toggle, menu, focusTerminal } = setup();
    toggle.click();
    toggle.click();
    expect(menu.isOpen()).toBe(false);
    expect(focusTerminal).toHaveBeenCalledTimes(1);
  });

  it('destroy() removes the element and stops sending', async () => {
    const { menu, container, onSend } = setup();
    menu.destroy();
    expect(container.children).toHaveLength(0);
    expect(await menu.activate('ctrl-c')).toBeNull();
    expect(onSend).not.toHaveBeenCalled();
  });

  it('two menus get distinct ids', () => {
    const a = document.createElement('div');
    const b = document.createElement('div');
    renderSignalMenu(a, { t });
    renderSignalMenu(b, { t });
    expect(a.querySelector('[role="menu"]').id).not.toBe(b.querySelector('[role="menu"]').id);
  });
});

// Regression tests for review findings (REQ-017 review round).
describe('review findings', () => {
  beforeEach(() => { document.body.replaceChildren(); });

  function withTerminal() {
    const term = document.createElement('textarea');
    term.className = 'xterm-helper-textarea';
    document.body.append(term);
    const onTermKey = vi.fn();
    term.addEventListener('keydown', onTermKey);
    term.focus();
    return { term, onTermKey };
  }

  // #2: control characters must not overtake typed input still queued in
  // the frontend (entry.pendingInput / sendQueue).
  describe('createSignalSender', () => {
    it('sends control characters through the tab input path (xterm.input), not the backend signal call', async () => {
      const order = [];
      const entry = { id: 'T1', status: 'open', xterm: { input: vi.fn((d, user) => order.push(['input', d, user])) } };
      const sendSignal = vi.fn(async (id, wire) => { order.push(['signal', id, wire]); return { ok: true, code: '' }; });
      const onSend = createSignalSender({ getEntry: (id) => (id === 'T1' ? entry : null), sendSignal });
      const r = await onSend({ kind: 'ssh', target: 'T1', id: 'ctrl-d', type: 'control', data: '\x04', wire: 'EOF_CHAR' });
      expect(normalizeSignalResult(r).ok).toBe(true);
      expect(order).toEqual([['input', '\x04', true]]);
      expect(sendSignal).not.toHaveBeenCalled();
    });

    it('a paste still being flushed is followed, not split, by the control character', async () => {
      // Model of terminal-tabs.js: onData -> queueInput -> chunks sent one by one.
      const wire = [];
      let release;
      const entry = { id: 'T1', status: 'open', pendingInput: '', sendQueue: Promise.resolve(), sending: false };
      const queueInput = (data) => {
        entry.pendingInput += data;
        if (entry.sending) return;
        entry.sending = true;
        entry.sendQueue = entry.sendQueue.then(async () => {
          while (entry.pendingInput) {
            const chunk = entry.pendingInput.slice(0, 4);
            entry.pendingInput = entry.pendingInput.slice(4);
            await new Promise((r) => { release = r; });
            wire.push(chunk);
          }
          entry.sending = false;
        });
      };
      entry.xterm = { input: (d) => queueInput(d) };
      const sendSignal = vi.fn(async (_id, w) => { wire.push(`<${w}>`); return { ok: true, code: '' }; });
      const onSend = createSignalSender({ getEntry: () => entry, sendSignal });
      queueInput('cat <<E\nab\nE\n');
      await flush();
      await onSend({ target: 'T1', type: 'control', data: '\x04', wire: 'EOF_CHAR' });
      while (entry.sending) { release?.(); await flush(); }
      expect(wire.join('')).toBe('cat <<E\nab\nE\n\x04');
      expect(sendSignal).not.toHaveBeenCalled();
    });

    it('SSH signals and BREAK go to the backend with the tab id', async () => {
      const entry = { id: 'T1', status: 'open', xterm: { input: vi.fn() } };
      const sendSignal = vi.fn(async () => ({ ok: true, code: '' }));
      const onSend = createSignalSender({ getEntry: () => entry, sendSignal });
      await onSend({ target: 'T1', type: 'signal', signal: 'KILL', wire: 'KILL' });
      await onSend({ target: 'T1', type: 'break', wire: 'BREAK' });
      expect(sendSignal.mock.calls).toEqual([['T1', 'KILL'], ['T1', 'BREAK']]);
      expect(entry.xterm.input).not.toHaveBeenCalled();
    });

    it('a control character for a missing or closed tab is not sent', async () => {
      const sendSignal = vi.fn();
      const closed = { id: 'T2', status: 'exited', xterm: { input: vi.fn() } };
      const onSend = createSignalSender({ getEntry: (id) => (id === 'T2' ? closed : null), sendSignal });
      expect(normalizeSignalResult(await onSend({ target: 'T1', type: 'control', data: '\x03', wire: 'INT_CHAR' })).code).toBe('no_session');
      expect(normalizeSignalResult(await onSend({ target: 'T2', type: 'control', data: '\x03', wire: 'INT_CHAR' })).code).toBe('no_session');
      expect(closed.xterm.input).not.toHaveBeenCalled();
      expect(sendSignal).not.toHaveBeenCalled();
    });
  });

  // #3: the KILL/TERM confirm must not be one Enter away.
  describe('confirmSignalDialog', () => {
    const open = (extra = {}) => {
      const p = confirmSignalDialog({ title: 'KILL を送りますか', message: '本文', okLabel: 'KILL を送る', t, ...extra });
      const dialog = document.querySelector('[data-signal-confirm]');
      return {
        p, dialog,
        ok: dialog.querySelector('[data-signal-confirm-ok]'),
        cancel: dialog.querySelector('[data-signal-confirm-cancel]'),
      };
    };

    it('focuses Cancel, not the danger button', () => {
      const { cancel, ok, dialog } = open();
      expect(document.activeElement).toBe(cancel);
      expect(ok.className).toContain('btn-danger');
      expect(dialog.getAttribute('role')).toBe('dialog');
      expect(dialog.getAttribute('aria-modal')).toBe('true');
      expect(dialog.textContent).toContain('本文');
    });

    it('Enter on the focused Cancel resolves false', async () => {
      const { p, cancel } = open();
      key(cancel, 'Enter');
      cancel.click();
      expect(await p).toBe(false);
      expect(document.querySelector('[data-signal-confirm]')).toBeNull();
    });

    it('a held (repeating) Enter or Space never activates a button', () => {
      const { ok, cancel } = open();
      ok.focus();
      expect(key(ok, 'Enter', { repeat: true }).defaultPrevented).toBe(true);
      expect(key(cancel, ' ', { repeat: true }).defaultPrevented).toBe(true);
    });

    it('a keyboard click on OK without a fresh key press in the dialog is ignored', async () => {
      const { p, ok } = open();
      // e.g. the Space keyup that opened the menu item lands on the dialog
      ok.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true, detail: 0 }));
      expect(document.querySelector('[data-signal-confirm]')).not.toBeNull();
      key(ok, 'Enter');
      ok.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true, detail: 0 }));
      expect(await p).toBe(true);
    });

    it('a pointer click on OK resolves true', async () => {
      const { p, ok } = open();
      ok.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true, detail: 1 }));
      expect(await p).toBe(true);
    });

    it('Escape and the backdrop cancel', async () => {
      let d = open();
      key(d.cancel, 'Escape');
      expect(await d.p).toBe(false);
      d = open();
      d.dialog.dispatchEvent(new MouseEvent('click', { bubbles: true, detail: 1 }));
      expect(await d.p).toBe(false);
    });

    it('Tab stays inside the dialog', () => {
      const { cancel, ok } = open();
      key(cancel, 'Tab');
      expect(document.activeElement).toBe(ok);
      key(ok, 'Tab');
      expect(document.activeElement).not.toBe(document.body);
      expect(document.querySelector('[data-signal-confirm]').contains(document.activeElement)).toBe(true);
    });

    it('text is set with textContent; EN cancel label', () => {
      const { dialog, cancel } = open({ message: '<img src=x>', t: tFor('en') });
      expect(dialog.querySelector('img')).toBeNull();
      expect(cancel.textContent).toBe('Cancel');
    });

    it('Enter on a KILL menu item opens the dialog with Cancel focused', async () => {
      const { toggle, item, onSend } = setup({ confirm: (o) => confirmSignalDialog({ ...o, t }) });
      key(toggle, 'ArrowDown');
      item('KILL').focus();
      key(item('KILL'), 'Enter');
      const cancel = document.querySelector('[data-signal-confirm-cancel]');
      expect(document.activeElement).toBe(cancel);
      cancel.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true, detail: 0 }));
      expect(document.querySelector('[data-signal-confirm]')).not.toBeNull();
      key(cancel, 'Enter');
      cancel.click();
      await flush();
      expect(onSend).not.toHaveBeenCalled();
    });
  });

  // #4: a pointer-opened menu (focus stays in xterm) must not swallow or leak keys.
  describe('pointer-opened menu and keys typed in xterm', () => {
    it('Escape in xterm closes the menu and is not sent to the shell', () => {
      const { toggle, menu } = setup();
      const { onTermKey, term } = withTerminal();
      toggle.click();
      expect(menu.isOpen()).toBe(true);
      expect(document.activeElement).toBe(term);
      const ev = key(term, 'Escape');
      expect(menu.isOpen()).toBe(false);
      expect(ev.defaultPrevented).toBe(true);
      expect(onTermKey).not.toHaveBeenCalled();
    });

    it('typing in xterm closes the menu and the key still reaches the shell', () => {
      const { toggle, menu } = setup();
      const { onTermKey, term } = withTerminal();
      toggle.click();
      key(term, 'l');
      expect(menu.isOpen()).toBe(false);
      expect(onTermKey).toHaveBeenCalledTimes(1);
    });

    it('a merely open menu without focus inside does not block redraws', () => {
      const { toggle, menu } = setup();
      withTerminal();
      toggle.click();
      expect(menu.isOpen()).toBe(true);
      expect(menu.isBusy()).toBe(false);
      menu.open('first');
      expect(menu.isBusy()).toBe(true);
    });
  });

  // #5: moving the menu into a new slot keeps keyboard focus.
  it('mount() keeps keyboard focus on the focused item', () => {
    const { toggle, menu, item } = setup();
    key(toggle, 'ArrowDown');
    key(document.activeElement, 'ArrowDown');
    expect(document.activeElement).toBe(item('ctrl-z'));
    const slot = document.createElement('div');
    document.body.append(slot);
    menu.mount(slot);
    expect(menu.isOpen()).toBe(true);
    expect(document.activeElement).toBe(item('ctrl-z'));
    key(document.activeElement, 'ArrowDown');
    expect(document.activeElement).toBe(item('ctrl-backslash'));
  });

  it('a kind change while the menu has keyboard focus keeps focus in the menu', () => {
    const { toggle, menu, list } = setup();
    key(toggle, 'ArrowDown');
    menu.update({ kind: 'serial' });
    expect(menu.isOpen()).toBe(true);
    expect(list.contains(document.activeElement)).toBe(true);
  });

  // #6: a dialog that takes focus while the menu is open closes the menu.
  it('focus moving to something outside (e.g. a modal) closes the menu', () => {
    const { toggle, menu } = setup();
    withTerminal();
    toggle.click();
    const modalButton = document.createElement('button');
    document.body.append(modalButton);
    modalButton.focus();
    expect(menu.isOpen()).toBe(false);
  });

  // #7: every item has its own label, and the labels say what they act on.
  it('labels are distinct per language and say command vs shell', () => {
    for (const lang of ['ja', 'en']) {
      const tt = tFor(lang);
      const labels = signalItems('ssh').map((i) => tt(i.labelKey));
      expect(new Set(labels).size).toBe(labels.length);
      for (const a of labels) for (const b of labels) if (a !== b) expect(b.includes(a)).toBe(false);
    }
    expect(tFor('ja')('sig.KILL')).toContain('シェル');
    expect(tFor('ja')('sig.ctrlBackslash')).toContain('コマンド');
    expect(tFor('en')('sig.INT')).toMatch(/shell/i);
    expect(tFor('en')('sig.ctrlC')).toMatch(/command/i);
  });
});

describe('REQ-017 review round 2', () => {
  beforeEach(() => {
    document.getElementById('editor-modal-root')?.remove();
  });

  // #1: the confirm dialog must count as a modal for main.js's shortcut gate.
  describe('confirmSignalDialog blocks terminal shortcuts', () => {
    // Same check as main.js modalOpen() for the editor root.
    const MODAL_ROOTS = ['#editor-modal-root', '#agents-modal-root', '#credential-modal-root'];
    const modalOpen = () => MODAL_ROOTS.some((sel) => Boolean(document.querySelector(sel)?.childElementCount));

    it('creates #editor-modal-root when it does not exist yet, so modalOpen() is true', async () => {
      expect(document.getElementById('editor-modal-root')).toBeNull();
      const p = confirmSignalDialog({ title: 'KILL', message: 'm', okLabel: 'KILL', t });
      const root = document.getElementById('editor-modal-root');
      expect(root).not.toBeNull();
      expect(root.parentNode).toBe(document.body);
      expect(root.querySelector('[data-signal-confirm]')).not.toBeNull();
      expect(modalOpen()).toBe(true);
      key(document.querySelector('[data-signal-confirm-cancel]'), 'Escape');
      expect(await p).toBe(false);
      expect(modalOpen()).toBe(false);
    });

    it('reuses an existing #editor-modal-root', async () => {
      const existing = document.createElement('div');
      existing.id = 'editor-modal-root';
      document.body.appendChild(existing);
      const p = confirmSignalDialog({ title: 'KILL', message: 'm', okLabel: 'KILL', t });
      expect(document.querySelectorAll('#editor-modal-root').length).toBe(1);
      expect(existing.querySelector('[data-signal-confirm]')).not.toBeNull();
      key(document.querySelector('[data-signal-confirm-cancel]'), 'Escape');
      expect(await p).toBe(false);
    });

    it('Cmd+W / Cmd+T / Cmd+1 / Cmd+Shift+] / Ctrl+Tab typed in the dialog never reach document', async () => {
      const seen = [];
      const onDoc = (event) => seen.push(event.key);
      document.addEventListener('keydown', onDoc);
      try {
        // A custom root outside MODAL_ROOTS: the keydown guard alone must hold.
        const custom = document.createElement('div');
        document.body.appendChild(custom);
        const p = confirmSignalDialog({ title: 'KILL', message: 'm', okLabel: 'KILL', t, root: custom });
        const cancel = document.querySelector('[data-signal-confirm-cancel]');
        key(cancel, 'w', { metaKey: true });
        key(cancel, 't', { metaKey: true });
        key(cancel, '1', { metaKey: true });
        key(cancel, ']', { metaKey: true, shiftKey: true });
        key(cancel, 'Tab', { ctrlKey: true });
        key(cancel, 'Enter');
        expect(seen).toEqual([]);
        expect(document.querySelector('[data-signal-confirm]')).not.toBeNull();
        key(cancel, 'Escape');
        expect(await p).toBe(false);
        custom.remove();
      } finally {
        document.removeEventListener('keydown', onDoc);
      }
    });
  });

  // #2: TERM / INT to an interactive shell look like no-ops.
  describe('TERM / INT feedback', () => {
    it('labels and the TERM confirm say an interactive shell may ignore them and point to HUP / KILL', () => {
      for (const lang of ['ja', 'en']) {
        const L = SIGNALS_I18N[lang];
        expect(L['sig.TERM']).toMatch(lang === 'ja' ? /対話シェル/ : /interactive shell/);
        expect(L['sig.INT']).toMatch(lang === 'ja' ? /対話シェル/ : /interactive shell/);
        expect(L['sig.confirm.TERM']).toMatch(/HUP/);
        expect(L['sig.confirm.TERM']).toMatch(/KILL/);
        expect(L['sig.confirm.TERM']).toMatch(lang === 'ja' ? /無視/ : /ignores/);
      }
    });

    it('a successful TERM / INT gives an info toast that mentions HUP / KILL', () => {
      for (const id of ['TERM', 'INT']) {
        const toast = signalResultToast({ ok: true, code: 'ok' }, { id, name: id, type: 'signal' }, t);
        expect(toast.variant).toBe('info');
        expect(toast.text).toContain(`${id} を送りました`);
        expect(toast.text).toContain('HUP か KILL');
        const en = signalResultToast(undefined, { id, name: id, type: 'signal' }, tFor('en'));
        expect(en.text).toContain(`Sent ${id}`);
        expect(en.text).toContain('HUP or KILL');
      }
    });

    it('a successful HUP / KILL gives a plain info toast; control characters and BREAK stay silent', () => {
      expect(signalResultToast({ ok: true, code: 'ok' }, { id: 'HUP', name: 'HUP', type: 'signal' }, t))
        .toEqual({ text: 'HUP を送りました', variant: 'info' });
      expect(signalResultToast({ ok: true, code: 'ok' }, { id: 'ctrl-c', name: 'Ctrl-C', type: 'control' }, t)).toBeNull();
      expect(signalResultToast({ ok: true, code: 'ok' }, { id: 'BREAK', name: 'BREAK', type: 'break' }, t)).toBeNull();
    });

    it('failures give a warning; cancelled gives nothing', () => {
      expect(signalResultToast({ ok: false, code: 'signal_unsupported' }, { id: 'TERM', name: 'TERM', type: 'signal' }, t))
        .toEqual({ text: signalResultMessage('signal_unsupported', t, { name: 'TERM' }), variant: 'warning' });
      expect(signalResultToast({ ok: false, code: 'cancelled' }, { id: 'KILL', name: 'KILL', type: 'signal' }, t)).toBeNull();
    });

    it('the menu hands onResult what signalResultToast needs for a sent TERM', async () => {
      const results = [];
      const { item } = setup({
        confirm: async () => true,
        onResult: (result, info) => results.push(signalResultToast(result, info, t)),
      });
      item('TERM').click();
      await flush(); await flush();
      expect(results).toHaveLength(1);
      expect(results[0].variant).toBe('info');
      expect(results[0].text).toContain('HUP か KILL');
    });
  });
});
