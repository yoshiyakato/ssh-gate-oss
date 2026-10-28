// Signal menu for the terminal toolbar (REQ-017).
//
// SSH tabs get the terminal control characters (Ctrl-C / Ctrl-Z / Ctrl-\ /
// Ctrl-D, sent as input bytes) and, under a separator, the SSH "signal"
// channel requests TERM / KILL / HUP / INT. Serial tabs get BREAK.
// KILL and TERM ask first through the `confirm` function the caller passes
// (main.js passes confirmSignalDialog, whose initial focus is Cancel).
// createSignalSender builds onSend so control characters follow queued input.
//
// This module imports nothing (no Wails bindings, no other modules) and owns
// no global shortcut, so it cannot collide with shortcuts.js or with keys the
// shell needs. All side effects go through the callbacks in `options`.
// Every string is set with textContent; nothing here builds HTML.

export const SIGNALS_I18N = {
  ja: {
    'sig.menu': 'シグナル',
    'sig.menuTitle': 'シグナルを送る',
    'sig.menuDisabled': 'シグナルを送れるセッションがありません',
    'sig.group.control': '実行中のコマンドへ（制御文字）',
    'sig.group.signal': 'シェルへ（SSH シグナル）',
    'sig.group.serial': 'シリアル',
    'sig.ctrlC': '実行中のコマンドを中断',
    'sig.ctrlZ': '実行中のコマンドを一時停止',
    'sig.ctrlBackslash': '実行中のコマンドを終了（QUIT）',
    'sig.ctrlD': '入力の終わり（EOF）',
    'sig.TERM': 'シェルに終了を依頼（対話シェルは無視しがち）',
    'sig.KILL': 'シェルを強制終了（セッションが終わる）',
    'sig.HUP': 'シェルに切断を通知',
    'sig.INT': 'シェルに割り込み（対話シェルはプロンプトを出し直すだけ）',
    'sig.BREAK': 'BREAK を送る',
    'sig.itemAria': '{key}: {label}',
    'sig.confirm.title': '{name} を送りますか',
    'sig.confirm.TERM': 'このセッションのシェル（リモートのプロセス）に TERM シグナルを送り、終了を依頼します。対話中の bash や zsh はふつう TERM を無視するので、何も起きないことがあります。セッションを確実に終えるには HUP か KILL を使ってください。',
    'sig.confirm.KILL': 'このセッションのシェル（リモートのプロセス）に KILL シグナルを送り、すぐに強制終了します。セッションは終わり、保存されていない作業は失われます。',
    'sig.confirm.ok': '{name} を送る',
    'sig.confirm.cancel': 'キャンセル',
    'sig.result.sent': '{name} を送りました',
    'sig.result.sent_shell_may_ignore': '{name} を送りました。対話シェルは {name} を無視することがあります。セッションを終えるには HUP か KILL を使ってください',
    'sig.result.signal_unsupported': 'サーバーが {name} を受け付けませんでした（SSH シグナルに対応していない可能性があります）。Ctrl-C などの制御文字を使ってください',
    'sig.result.signal_failed': '{name} を送れませんでした',
    'sig.result.invalid_signal': '送れないシグナルです（{name}）',
    'sig.result.break_unsupported': 'このシリアルポートは BREAK に対応していません',
    'sig.result.break_failed': 'BREAK を送れませんでした',
    'sig.result.no_session': 'ターミナルが接続されていません',
    'sig.result.not_ready': 'ターミナルはまだ接続中です',
    'sig.result.input_timeout': '入力の送信が詰まっています。しばらくしてからもう一度送ってください',
    'sig.result.signal_busy': '前のシグナルの応答を待っています',
    'sig.result.signal_not_applicable': '{name} はこの種類のターミナルには送れません',
    'sig.result.signal_unavailable': 'このターミナルではシグナルを送れません',
    'sig.result.unknown': '{name} を送れませんでした（{code}）',
  },
  en: {
    'sig.menu': 'Signal',
    'sig.menuTitle': 'Send a signal',
    'sig.menuDisabled': 'No session to send a signal to',
    'sig.group.control': 'To the running command (control characters)',
    'sig.group.signal': 'To the shell (SSH signals)',
    'sig.group.serial': 'Serial',
    'sig.ctrlC': 'Interrupt the running command',
    'sig.ctrlZ': 'Suspend the running command',
    'sig.ctrlBackslash': 'Quit the running command (QUIT)',
    'sig.ctrlD': 'End of input (EOF)',
    'sig.TERM': 'Ask the shell to terminate (interactive shells often ignore it)',
    'sig.KILL': 'Kill the shell (ends the session)',
    'sig.HUP': 'Hang up the shell',
    'sig.INT': 'Interrupt the shell (an interactive shell only redraws its prompt)',
    'sig.BREAK': 'Send BREAK',
    'sig.itemAria': '{key}: {label}',
    'sig.confirm.title': 'Send {name}?',
    'sig.confirm.TERM': 'Sends the TERM signal to this session\'s shell (the remote process) and asks it to exit. An interactive bash or zsh usually ignores TERM, so nothing may happen. To end the session for sure, use HUP or KILL.',
    'sig.confirm.KILL': 'Sends the KILL signal to this session\'s shell (the remote process) and stops it immediately. The session ends and unsaved work is lost.',
    'sig.confirm.ok': 'Send {name}',
    'sig.confirm.cancel': 'Cancel',
    'sig.result.sent': 'Sent {name}',
    'sig.result.sent_shell_may_ignore': 'Sent {name}. An interactive shell may ignore {name}; use HUP or KILL to end the session',
    'sig.result.signal_unsupported': 'The server did not accept {name} (it may not support SSH signals). Use a control character such as Ctrl-C instead',
    'sig.result.signal_failed': 'Could not send {name}',
    'sig.result.invalid_signal': 'This signal cannot be sent ({name})',
    'sig.result.break_unsupported': 'This serial port does not support BREAK',
    'sig.result.break_failed': 'Could not send BREAK',
    'sig.result.no_session': 'The terminal is not connected',
    'sig.result.not_ready': 'The terminal is still connecting',
    'sig.result.input_timeout': 'Input is backed up. Try again in a moment',
    'sig.result.signal_busy': 'Still waiting for the reply to the previous signal',
    'sig.result.signal_not_applicable': '{name} cannot be sent to this kind of terminal',
    'sig.result.signal_unavailable': 'Signals cannot be sent from this terminal',
    'sig.result.unknown': 'Could not send {name} ({code})',
  },
};

// Items per tab kind. `send` is what onSend receives (plus `kind`).
// control: raw bytes for the pty (the remote tty turns them into signals).
// signal:  an SSH "signal" channel request (RFC 4254 §6.9), name without SIG.
// break:   a serial BREAK.
// wire is the name App.SendTerminalSignal(id, sig) takes (terminal_signal.go).
const SSH_ITEMS = Object.freeze([
  { id: 'ctrl-c', group: 'control', key: 'Ctrl-C', labelKey: 'sig.ctrlC', send: { type: 'control', data: '\x03', wire: 'INT_CHAR' } },
  { id: 'ctrl-z', group: 'control', key: 'Ctrl-Z', labelKey: 'sig.ctrlZ', send: { type: 'control', data: '\x1a', wire: 'TSTP_CHAR' } },
  { id: 'ctrl-backslash', group: 'control', key: 'Ctrl-\\', labelKey: 'sig.ctrlBackslash', send: { type: 'control', data: '\x1c', wire: 'QUIT_CHAR' } },
  { id: 'ctrl-d', group: 'control', key: 'Ctrl-D', labelKey: 'sig.ctrlD', send: { type: 'control', data: '\x04', wire: 'EOF_CHAR' } },
  { id: 'TERM', group: 'signal', key: 'TERM', labelKey: 'sig.TERM', confirm: true, send: { type: 'signal', signal: 'TERM', wire: 'TERM' } },
  { id: 'KILL', group: 'signal', key: 'KILL', labelKey: 'sig.KILL', confirm: true, send: { type: 'signal', signal: 'KILL', wire: 'KILL' } },
  { id: 'HUP', group: 'signal', key: 'HUP', labelKey: 'sig.HUP', send: { type: 'signal', signal: 'HUP', wire: 'HUP' } },
  { id: 'INT', group: 'signal', key: 'INT', labelKey: 'sig.INT', send: { type: 'signal', signal: 'INT', wire: 'INT' } },
]);
const SERIAL_ITEMS = Object.freeze([
  { id: 'BREAK', group: 'serial', key: 'BREAK', labelKey: 'sig.BREAK', send: { type: 'break', wire: 'BREAK' } },
]);

export const SSH_SIGNAL_NAMES = Object.freeze(['TERM', 'KILL', 'HUP', 'INT']);

/** signalItems returns the menu items for a tab kind ('ssh' | 'serial'). */
export function signalItems(kind) {
  return kind === 'serial' ? SERIAL_ITEMS : SSH_ITEMS;
}

function itemById(kind, id) {
  const item = signalItems(kind).find((i) => i.id === id);
  return item ? { ...item, kind: kind === 'serial' ? 'serial' : 'ssh' } : null;
}

const RESULT_CODES = new Set([
  // every failure code App.SendTerminalSignal returns (terminal_signal.go)
  'signal_unsupported', 'signal_failed', 'invalid_signal', 'break_unsupported', 'break_failed',
  'no_session', 'not_ready', 'input_timeout', 'signal_busy', 'signal_not_applicable', 'signal_unavailable',
]);

/**
 * normalizeSignalResult turns whatever onSend returned (undefined, true,
 * false, a string code, or { ok, code, message }) into { ok, code, message }.
 */
export function normalizeSignalResult(value) {
  if (value === undefined || value === null || value === true) return { ok: true, code: 'ok', message: '' };
  if (value === false) return { ok: false, code: 'signal_failed', message: '' };
  if (typeof value === 'string') {
    return value === '' || value === 'ok' ? { ok: true, code: 'ok', message: '' } : { ok: false, code: value, message: '' };
  }
  if (typeof value === 'object') {
    const code = typeof value.code === 'string' && value.code ? value.code : (value.ok === false ? 'signal_failed' : 'ok');
    const ok = value.ok === undefined ? code === 'ok' : !!value.ok;
    return { ok, code: ok ? 'ok' : code, message: typeof value.message === 'string' ? value.message : '' };
  }
  return { ok: true, code: 'ok', message: '' };
}

/**
 * signalResultMessage turns a result code into UI text.
 * `name` is the human name of what was sent (e.g. "KILL", "Ctrl-C", "BREAK").
 * 'cancelled' (the user said no in the confirm dialog) gives ''.
 * Unknown codes fall back to sig.result.unknown so a new backend code still
 * reads as a failure instead of a raw key.
 */
export function signalResultMessage(code, t, { name = '' } = {}) {
  const c = String(code ?? '') || 'ok';
  if (c === 'cancelled') return '';
  if (c === 'ok') return t('sig.result.sent', { name });
  if (RESULT_CODES.has(c)) return t(`sig.result.${c}`, { name });
  return t('sig.result.unknown', { name, code: c });
}

// SSH signals an interactive login shell (bash / zsh) ignores or only
// catches: a successful send of these looks like nothing happened.
const SHELL_MAY_IGNORE = new Set(['TERM', 'INT']);

/**
 * signalResultToast picks the toast for onResult's (result, item), or null
 * for none. Failures give a warning. A successful SSH signal also gives an
 * info toast, so the user knows it was delivered even when the shell does not
 * react (TERM / INT add that an interactive shell may ignore them and point to
 * HUP / KILL). Successful control characters and BREAK stay silent, as does
 * 'cancelled'.
 *
 * returns null | { text, variant: 'warning' | 'info' }
 */
export function signalResultToast(result, item, t) {
  const r = normalizeSignalResult(result);
  const name = item?.name ?? '';
  if (!r.ok) {
    const text = signalResultMessage(r.code, t, { name });
    return text ? { text, variant: 'warning' } : null;
  }
  if (item?.type !== 'signal') return null;
  const key = SHELL_MAY_IGNORE.has(item.id) ? 'sig.result.sent_shell_may_ignore' : 'sig.result.sent';
  return { text: t(key, { name }), variant: 'info' };
}

function el(tag, attrs = {}, text) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v === undefined || v === null || v === false) continue;
    node.setAttribute(k, v === true ? '' : String(v));
  }
  if (text !== undefined) node.textContent = text;
  return node;
}

let menuSeq = 0;

/**
 * renderSignalMenu draws the toolbar's signal menu into `container`
 * (replacing its children) and returns a controller.
 *
 * options:
 *   kind          'ssh' | 'serial'
 *   target        the terminal tab id the menu acts on (the active tab). It
 *                 is captured when an item is chosen and passed to onSend as
 *                 `target`; if it changes while KILL / TERM is being
 *                 confirmed, nothing is sent (result 'cancelled').
 *   disabled      no active session (or it is not running)
 *   t             (key, vars) => string
 *   lang          optional; a change (via update) redraws the labels even
 *                 when `t` is the same function
 *   onSend        async ({ kind, target, type, wire, data?, signal?, id }) => result
 *                 result: undefined / true (sent), a code string, or
 *                 { ok, code, message }. A throw counts as signal_failed.
 *   confirm       async ({ title, message, okLabel, signal }) => boolean
 *                 used for KILL / TERM. Without it those items are refused
 *                 (never sent unconfirmed).
 *   focusTerminal () => void, called after every activation / dismissal by
 *                 pointer so focus goes back to xterm.
 *   onResult      (result, item) => void, e.g. a toast via signalResultMessage.
 *
 * controller: { element, update(opts), mount(container), open(focus), close(),
 *               isOpen(), isBusy(), activate(id), destroy() }
 * isBusy() is true while the open menu holds keyboard focus, a confirm is
 * pending or onSend has not settled; a caller that redraws on a timer should
 * skip the redraw then. mount() moves the same menu into a freshly rendered
 * slot and keeps keyboard focus where it was. While open, a key pressed or
 * focus moved outside the menu closes it (Escape is not passed on).
 */
export function renderSignalMenu(container, options = {}) {
  let opts = { kind: 'ssh', disabled: false, ...options };
  const t = (key, vars) => (opts.t ? opts.t(key, vars) : key);
  const menuId = `sig-menu-${++menuSeq}`;
  let isMenuOpen = false;
  let confirming = false;
  let sending = 0;
  let destroyed = false;

  const root = el('div', { class: 'term-signal dropdown' });
  const toggle = el('button', {
    type: 'button',
    class: 'btn btn-sm btn-outline-secondary term-signal-toggle',
    'aria-haspopup': 'menu',
    'aria-expanded': 'false',
    'aria-controls': menuId,
    'data-signal-toggle': '',
  });
  const icon = el('i', { class: 'bi bi-lightning-charge', 'aria-hidden': 'true' });
  const toggleText = el('span', { class: 'term-signal-label' });
  toggle.append(icon, toggleText);
  const menu = el('ul', { id: menuId, class: 'dropdown-menu term-signal-menu', role: 'menu', hidden: true });
  root.append(toggle, menu);

  function menuItems() {
    return [...menu.querySelectorAll('[role="menuitem"]')];
  }

  function buildItems() {
    menu.replaceChildren();
    menu.setAttribute('aria-label', t('sig.menuTitle'));
    let lastGroup = '';
    for (const item of signalItems(opts.kind)) {
      if (item.group !== lastGroup) {
        if (lastGroup) menu.append(el('li', { role: 'separator', class: 'dropdown-divider' }));
        menu.append(el('li', { role: 'presentation', class: 'dropdown-header' }, t(`sig.group.${item.group}`)));
        lastGroup = item.group;
      }
      const li = el('li', { role: 'none' });
      const label = t(item.labelKey);
      const button = el('button', {
        type: 'button',
        role: 'menuitem',
        tabindex: '-1',
        class: `dropdown-item term-signal-item${item.confirm ? ' text-danger' : ''}`,
        'data-signal-id': item.id,
        'aria-label': t('sig.itemAria', { key: item.key, label }),
      });
      button.append(el('kbd', { class: 'term-signal-key' }, item.key), el('span', { class: 'term-signal-text' }, label));
      li.append(button);
      menu.append(li);
    }
  }

  function applyState() {
    toggleText.textContent = t('sig.menu');
    toggle.title = opts.disabled ? t('sig.menuDisabled') : t('sig.menuTitle');
    toggle.disabled = !!opts.disabled;
    if (opts.disabled) close({ restore: false });
  }

  function focusTerminal() {
    try { opts.focusTerminal?.(); } catch { /* focus is best effort */ }
  }

  function focusItem(index) {
    const items = menuItems();
    if (!items.length) return;
    const i = ((index % items.length) + items.length) % items.length;
    items.forEach((b, n) => b.setAttribute('tabindex', n === i ? '0' : '-1'));
    items[i].focus();
  }

  function onOutsidePointer(event) {
    if (!root.isConnected || !root.contains(event.target)) close({ restore: false });
  }

  // A menu opened by pointer leaves focus in xterm (keepFocus), so its keys
  // never reach onMenuKeydown. Any key pressed outside the menu closes it;
  // Escape is used up here (capture phase, before xterm sees it) so it does
  // not leak to the shell. Other keys go on to their target as usual.
  function onOutsideKeydown(event) {
    if (root.contains(event.target)) return;
    if (event.key === 'Escape') {
      event.preventDefault();
      event.stopPropagation();
      close({ restore: false });
      return;
    }
    close({ restore: false });
  }

  // Focus moving elsewhere (Tab out, a modal taking focus) closes the menu, so
  // it never stays painted over a dialog that is meant to block the page.
  function onOutsideFocus(event) {
    if (!root.contains(event.target)) close({ restore: false });
  }

  function focusInside() {
    const active = document.activeElement;
    return !!active && active !== document.body && root.contains(active);
  }

  function open(focus = 'first') {
    if (destroyed || opts.disabled || confirming) return;
    if (!isMenuOpen) {
      isMenuOpen = true;
      menu.hidden = false;
      menu.classList.add('show');
      toggle.setAttribute('aria-expanded', 'true');
      document.addEventListener('pointerdown', onOutsidePointer, true);
      document.addEventListener('keydown', onOutsideKeydown, true);
      document.addEventListener('focusin', onOutsideFocus, true);
    }
    if (focus === 'first') focusItem(0);
    else if (focus === 'last') focusItem(-1);
  }

  // restore: 'toggle' (keyboard Escape), true/'terminal' (back to xterm), false.
  function close({ restore = false } = {}) {
    if (!isMenuOpen) return;
    isMenuOpen = false;
    menu.hidden = true;
    menu.classList.remove('show');
    toggle.setAttribute('aria-expanded', 'false');
    document.removeEventListener('pointerdown', onOutsidePointer, true);
    document.removeEventListener('keydown', onOutsideKeydown, true);
    document.removeEventListener('focusin', onOutsideFocus, true);
    if (restore === 'toggle') toggle.focus();
    else if (restore) focusTerminal();
  }

  async function activate(id) {
    const item = itemById(opts.kind, id);
    close({ restore: false });
    if (!item || opts.disabled || destroyed) return null;
    const name = item.key;
    const target = opts.target;
    if (item.confirm) {
      if (confirming) return null;
      if (typeof opts.confirm !== 'function') {
        // Never send KILL / TERM without asking.
        const result = { ok: false, code: 'cancelled', message: '' };
        focusTerminal();
        return result;
      }
      confirming = true;
      let ok = false;
      try {
        ok = !!(await opts.confirm({
          title: t('sig.confirm.title', { name }),
          message: t(`sig.confirm.${item.id}`),
          okLabel: t('sig.confirm.ok', { name }),
          signal: item.id,
        }));
      } catch {
        ok = false;
      } finally {
        confirming = false;
      }
      if (!ok || destroyed || opts.disabled || opts.target !== target || opts.kind !== item.kind) {
        focusTerminal();
        return { ok: false, code: 'cancelled', message: '' };
      }
    }
    let result;
    sending += 1;
    try {
      result = normalizeSignalResult(await opts.onSend?.({ kind: item.kind, target, id: item.id, ...item.send }));
    } catch (error) {
      result = { ok: false, code: item.send.type === 'break' ? 'break_failed' : 'signal_failed', message: String(error?.message ?? error ?? '') };
    } finally {
      sending -= 1;
    }
    try { opts.onResult?.(result, { id: item.id, name, type: item.send.type }); } catch { /* caller's problem */ }
    focusTerminal();
    return result;
  }

  // Pointer presses must not take focus away from xterm.
  function keepFocus(event) {
    if (event.button === 0 || event.button === undefined) event.preventDefault();
  }

  function onToggleClick() {
    if (isMenuOpen) close({ restore: true });
    else open('none');
  }

  function onToggleKeydown(event) {
    if (opts.disabled) return;
    if (event.key === 'ArrowDown' || event.key === 'Enter' || event.key === ' ') {
      event.preventDefault();
      event.stopPropagation();
      open('first');
    } else if (event.key === 'ArrowUp') {
      event.preventDefault();
      event.stopPropagation();
      open('last');
    } else if (event.key === 'Escape' && isMenuOpen) {
      event.preventDefault();
      event.stopPropagation();
      close({ restore: 'toggle' });
    }
  }

  function onMenuKeydown(event) {
    const items = menuItems();
    const current = items.indexOf(document.activeElement);
    const handled = () => { event.preventDefault(); event.stopPropagation(); };
    switch (event.key) {
      case 'ArrowDown': handled(); focusItem(current + 1); break;
      case 'ArrowUp': handled(); focusItem(current < 0 ? -1 : current - 1); break;
      case 'Home': handled(); focusItem(0); break;
      case 'End': handled(); focusItem(-1); break;
      case 'Escape': handled(); close({ restore: 'toggle' }); break;
      case 'Tab': close({ restore: false }); break;
      case 'Enter':
      case ' ': {
        handled();
        const id = document.activeElement?.dataset?.signalId;
        if (id) activate(id);
        break;
      }
      default: break;
    }
  }

  function onMenuClick(event) {
    const button = event.target?.closest?.('[data-signal-id]');
    if (!button || !menu.contains(button)) return;
    event.stopPropagation();
    activate(button.dataset.signalId);
  }

  toggle.addEventListener('mousedown', keepFocus);
  toggle.addEventListener('click', onToggleClick);
  toggle.addEventListener('keydown', onToggleKeydown);
  menu.addEventListener('mousedown', keepFocus);
  menu.addEventListener('click', onMenuClick);
  menu.addEventListener('keydown', onMenuKeydown);

  buildItems();
  applyState();
  container.replaceChildren(root);

  return {
    element: root,
    update(next = {}) {
      const kindChanged = next.kind !== undefined && next.kind !== opts.kind;
      const tChanged = (next.t !== undefined && next.t !== opts.t)
        || (next.lang !== undefined && next.lang !== opts.lang);
      opts = { ...opts, ...next };
      if (kindChanged || tChanged) {
        const wasOpen = isMenuOpen;
        const hadFocus = wasOpen && menu.contains(document.activeElement);
        close({ restore: false });
        buildItems();
        if (wasOpen && !opts.disabled) open(hadFocus ? 'first' : 'none');
      }
      applyState();
    },
    mount(next) {
      if (destroyed || !next || root.parentNode === next) return;
      // Moving a node drops focus from it; put it back so a keyboard user
      // keeps driving the open menu (or the button) after a redraw.
      const active = focusInside() ? document.activeElement : null;
      next.replaceChildren(root);
      if (active && active.isConnected && document.activeElement !== active) {
        try { active.focus({ preventScroll: true }); } catch { active.focus(); }
      }
    },
    open,
    close,
    isOpen: () => isMenuOpen,
    // An open menu only counts while it holds keyboard focus: a pointer-opened
    // menu (focus in xterm) survives redraws through mount(), and must not
    // keep the toolbar from showing that the session exited.
    isBusy: () => (isMenuOpen && focusInside()) || confirming || sending > 0,
    activate,
    destroy() {
      destroyed = true;
      close({ restore: false });
      root.remove();
    },
  };
}

/**
 * createSignalSender builds the menu's onSend.
 *
 * Control characters (Ctrl-C / Ctrl-Z / Ctrl-\ / Ctrl-D) go through the tab's
 * own input path, `entry.xterm.input(data, true)`: xterm fires onData, so the
 * byte joins terminal-tabs.js's queueInput / flushInput behind keystrokes and
 * paste chunks that have not reached the backend yet. Calling
 * SendTerminalSignal for them instead would put the byte on the backend queue
 * between two chunks of a paste (an EOF in the middle of a heredoc).
 * SSH signals and BREAK go to the backend (`sendSignal(id, wire)`, i.e.
 * App.SendTerminalSignal).
 *
 *   getEntry(id)          the terminal-tabs.js tab entry ({ status, xterm })
 *   sendSignal(id, wire)  App.SendTerminalSignal
 */
export function createSignalSender({ getEntry, sendSignal }) {
  return async (request) => {
    const { target, type, data, wire } = request ?? {};
    if (type === 'control') {
      const entry = getEntry?.(target);
      if (!entry || entry.status !== 'open') return { ok: false, code: 'no_session', message: '' };
      if (typeof entry.xterm?.input === 'function') {
        entry.xterm.input(data, true);
        return { ok: true, code: '', message: '' };
      }
      // No xterm.input (older xterm): the backend queue still keeps the order
      // with input that already reached it.
    }
    return sendSignal(target, wire);
  };
}

/**
 * confirmSignalDialog asks before KILL / TERM. Resolves true only for a
 * deliberate OK:
 *   - Cancel has the initial focus, so Enter (or a held Enter that repeats
 *     into the dialog) cancels rather than sends;
 *   - a repeating Enter / Space never activates a button;
 *   - a keyboard-made click (detail 0) counts only after a fresh Enter / Space
 *     press inside the dialog, so the Space keyup that chose the menu item does
 *     not land on a button;
 *   - Escape and a click on the backdrop cancel; Tab stays inside.
 * It is not part of editor.js's modal chain; it uses the same Bootstrap
 * classes and is mounted in `root` (default #editor-modal-root, created on
 * body the same way editor.js does when it does not exist yet). That root is
 * one of main.js's MODAL_ROOTS, so modalOpen() is true while the dialog is up
 * and the terminal shortcuts (Cmd+W / Cmd+T / Cmd+1..9 ...) stay off. On top
 * of that, no keydown leaves the dialog (stopPropagation), so a document-level
 * shortcut listener never sees keys typed into it, whatever `root` is.
 * All text is set with textContent.
 *
 * options: { title, message, okLabel, t, root }
 */
// Same as editor.js modalRoot(): the dialog must live in a root main.js's
// modalOpen() checks, even before any editor modal has been shown.
function editorModalRoot() {
  let node = document.getElementById('editor-modal-root');
  if (!node) {
    node = document.createElement('div');
    node.id = 'editor-modal-root';
    document.body.appendChild(node);
  }
  return node;
}

export function confirmSignalDialog({ title = '', message = '', okLabel = '', t, root } = {}) {
  const tr = (key, vars) => (t ? t(key, vars) : key);
  const mountRoot = root ?? editorModalRoot();
  const previousFocus = document.activeElement;
  const titleId = `sig-confirm-title-${++menuSeq}`;

  const host = el('div', { class: 'term-signal-confirm' });
  const backdrop = el('div', { class: 'modal-backdrop show' });
  const dialog = el('div', {
    class: 'modal d-block', tabindex: '-1', role: 'dialog', 'aria-modal': 'true',
    'aria-labelledby': titleId, 'data-signal-confirm': '',
  });
  const box = el('div', { class: 'modal-dialog modal-dialog-centered' });
  const content = el('div', { class: 'modal-content' });
  const header = el('div', { class: 'modal-header' });
  header.append(el('h5', { class: 'modal-title fw-bold', id: titleId }, title));
  const body = el('div', { class: 'modal-body' });
  body.append(el('p', { class: 'mb-0' }, message));
  const footer = el('div', { class: 'modal-footer' });
  const cancel = el('button', { type: 'button', class: 'btn btn-outline-secondary', 'data-signal-confirm-cancel': '' }, tr('sig.confirm.cancel'));
  const ok = el('button', { type: 'button', class: 'btn btn-danger', 'data-signal-confirm-ok': '' });
  ok.append(el('i', { class: 'bi bi-lightning-charge me-1', 'aria-hidden': 'true' }), document.createTextNode(okLabel));
  footer.append(cancel, ok);
  content.append(header, body, footer);
  box.append(content);
  dialog.append(box);
  host.append(backdrop, dialog);

  return new Promise((resolve) => {
    let done = false;
    let armed = false; // a fresh Enter / Space was pressed inside the dialog
    const finish = (value) => {
      if (done) return;
      done = true;
      host.remove();
      if (previousFocus && previousFocus.isConnected && typeof previousFocus.focus === 'function') {
        try { previousFocus.focus(); } catch { /* best effort */ }
      }
      resolve(value);
    };
    dialog.addEventListener('keydown', (event) => {
      // The dialog is modal: no key reaches document-level shortcut handlers
      // (main.js closes / opens / switches terminal tabs on Cmd+W / Cmd+T /
      // Cmd+1..9). Default actions (Enter / Space on a button) still run.
      event.stopPropagation();
      const k = event.key;
      if (k === 'Escape') {
        event.preventDefault();
        event.stopPropagation();
        finish(false);
      } else if (k === 'Enter' || k === ' ') {
        if (event.repeat) {
          event.preventDefault();
          event.stopPropagation();
        } else {
          armed = true;
        }
      } else if (k === 'Tab') {
        event.preventDefault();
        event.stopPropagation();
        const order = [cancel, ok];
        const i = order.indexOf(document.activeElement);
        const next = event.shiftKey ? (i <= 0 ? order.length - 1 : i - 1) : (i + 1) % order.length;
        order[next].focus();
      }
    });
    const keyboardClickUnarmed = (event) => event.detail === 0 && !armed;
    cancel.addEventListener('click', (event) => {
      event.stopPropagation();
      if (keyboardClickUnarmed(event)) { event.preventDefault(); return; }
      finish(false);
    });
    ok.addEventListener('click', (event) => {
      event.stopPropagation();
      if (keyboardClickUnarmed(event)) { event.preventDefault(); return; }
      finish(true);
    });
    dialog.addEventListener('click', (event) => {
      if (event.target === dialog) finish(false);
    });
    mountRoot.append(host);
    cancel.focus();
  });
}
