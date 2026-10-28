// Keyboard shortcuts for the terminal screen (design-v1 §6.4, phase 4).
//
// macOS: Cmd + key.  Windows/Linux: Ctrl+Shift + key.
// The returned handler is meant for xterm's attachCustomKeyEventHandler:
// it returns false for every key event it claims (keydown, keypress and keyup),
// so the keystroke never reaches the shell. Unclaimed events return true.
//
// This module imports nothing (no Wails bindings, no other modules). All side
// effects go through the `actions` callbacks the Integrator passes in.

export const SHORTCUTS_I18N = {
  ja: {
    'keys.newShell': '新しいシェル',
    'keys.closeTab': 'タブを閉じる',
    'keys.nextTab': '次のタブ',
    'keys.prevTab': '前のタブ',
    'keys.selectTab': '{n}番目のタブ',
    'keys.nextServer': '次のサーバー',
    'keys.prevServer': '前のサーバー',
    'keys.allServers': 'すべてのサーバー',
  },
  en: {
    'keys.newShell': 'New shell',
    'keys.closeTab': 'Close tab',
    'keys.nextTab': 'Next tab',
    'keys.prevTab': 'Previous tab',
    'keys.selectTab': 'Tab {n}',
    'keys.nextServer': 'Next server',
    'keys.prevServer': 'Previous server',
    'keys.allServers': 'All servers',
  },
};

/** @typedef {'mac'|'other'} ShortcutPlatform */

/**
 * @typedef {{type:'newShell'} | {type:'closeTab'} | {type:'nextTab'} | {type:'prevTab'}
 *   | {type:'selectTab', index:number} | {type:'nextServer'} | {type:'prevServer'}
 *   | {type:'allServers'}} ShortcutAction
 */

/**
 * Detects the shortcut platform from a navigator-like object.
 * @param {{platform?:string, userAgent?:string, userAgentData?:{platform?:string}}} [nav]
 * @returns {ShortcutPlatform}
 */
export function detectPlatform(nav = typeof navigator !== 'undefined' ? navigator : undefined) {
  if (!nav) return 'other';
  const p = (nav.userAgentData && nav.userAgentData.platform) || nav.platform || nav.userAgent || '';
  return /mac|iphone|ipad|ipod/i.test(p) ? 'mac' : 'other';
}

// Digits, letters, arrows and PageUp/Down come from the physical key (event.code)
// first, so Shift does not change the meaning ("Shift+1" gives key "!" but code
// "Digit1"). Brackets are the exception: event.code names the US key position,
// and on a JIS keyboard the key printed "[" reports BracketRight, "]" reports
// Backslash and "@" reports BracketLeft. So "[" / "]" come from event.key (the
// printed character; Shift gives "{" / "}"), and event.code is only used when
// event.key carries no printable character.
function keyId(ev) {
  const code = ev.code || '';
  const key = ev.key || '';
  if (/^Digit[0-9]$/.test(code)) return code.slice(5);
  if (/^Numpad[0-9]$/.test(code)) return code.slice(6);
  if (/^Key[A-Z]$/.test(code)) return code.slice(3).toLowerCase();
  switch (key) {
    case '[': case '{': return '[';
    case ']': case '}': return ']';
    default: break;
  }
  const printable = key.length === 1;
  switch (code) {
    case 'BracketLeft': return printable ? '' : '[';
    case 'BracketRight': return printable ? '' : ']';
    case 'ArrowLeft': return 'left';
    case 'ArrowRight': return 'right';
    case 'PageUp': return 'pageup';
    case 'PageDown': return 'pagedown';
    default: break;
  }
  switch (key) {
    case 'ArrowLeft': return 'left';
    case 'ArrowRight': return 'right';
    case 'PageUp': return 'pageup';
    case 'PageDown': return 'pagedown';
    default: break;
  }
  if (/^[0-9]$/.test(key)) return key;
  if (/^[a-zA-Z]$/.test(key)) return key.toLowerCase();
  return '';
}

/**
 * Pure key matcher. Returns the action for a key event, or null if the event
 * is not one of ours and must go to the shell.
 *
 *   action        mac                 other (Windows/Linux)
 *   newShell      Cmd+T               Ctrl+Shift+T
 *   closeTab      Cmd+W               Ctrl+Shift+W
 *   nextTab       Cmd+Shift+]         Ctrl+Shift+]   / Ctrl+Shift+PageDown
 *   prevTab       Cmd+Shift+[         Ctrl+Shift+[   / Ctrl+Shift+PageUp
 *   selectTab n   Cmd+1..9            Ctrl+Shift+1..9   (9 = last tab, index -1)
 *   nextServer    Cmd+Alt+Right       Ctrl+Shift+Alt+Right
 *   prevServer    Cmd+Alt+Left        Ctrl+Shift+Alt+Left
 *   allServers    Cmd+Alt+0           Ctrl+Shift+Alt+0
 *
 * @param {{key?:string, code?:string, metaKey?:boolean, ctrlKey?:boolean,
 *          shiftKey?:boolean, altKey?:boolean, isComposing?:boolean, keyCode?:number}} ev
 * @param {ShortcutPlatform} platform
 * @returns {ShortcutAction|null}
 */
export function matchShortcut(ev, platform) {
  if (!ev) return null;
  // IME composition (Japanese input etc.) is never a shortcut.
  if (ev.isComposing || ev.keyCode === 229) return null;
  const meta = !!ev.metaKey;
  const ctrl = !!ev.ctrlKey;
  const shift = !!ev.shiftKey;
  const alt = !!ev.altKey;

  if (platform === 'mac') {
    if (!meta || ctrl) return null;
  } else {
    if (!ctrl || !shift || meta) return null;
  }

  const k = keyId(ev);
  if (!k) return null;

  if (alt) {
    // Server tab movement. On mac Shift must be off; on other it is part of the modifier.
    if (platform === 'mac' && shift) return null;
    if (k === 'right') return { type: 'nextServer' };
    if (k === 'left') return { type: 'prevServer' };
    if (k === '0') return { type: 'allServers' };
    return null;
  }

  // Tab next/prev: mac needs Shift (Cmd+Shift+[ ]), other already has Shift.
  if (k === ']' && (platform !== 'mac' || shift)) return { type: 'nextTab' };
  if (k === '[' && (platform !== 'mac' || shift)) return { type: 'prevTab' };
  if (platform !== 'mac' && k === 'pagedown') return { type: 'nextTab' };
  if (platform !== 'mac' && k === 'pageup') return { type: 'prevTab' };

  // The rest on mac are plain Cmd+key (Cmd+Shift+T etc. stay unclaimed).
  if (platform === 'mac' && shift) return null;
  if (k === 't') return { type: 'newShell' };
  if (k === 'w') return { type: 'closeTab' };
  if (/^[1-9]$/.test(k)) {
    const n = Number(k);
    return { type: 'selectTab', index: n === 9 ? -1 : n - 1 };
  }
  return null;
}

/**
 * Pure helper: the target index when moving through `length` items.
 * delta = +1 / -1 wraps around; returns -1 when length is 0.
 * @param {number} current  current index (-1 = none)
 * @param {number} length
 * @param {number} delta
 */
export function cycleIndex(current, length, delta) {
  if (!length) return -1;
  if (current < 0 || current >= length) return delta >= 0 ? 0 : length - 1;
  return (((current + delta) % length) + length) % length;
}

/**
 * Pure helper: resolves a selectTab index (-1 = last) against a list length.
 * Returns -1 if out of range.
 */
export function resolveTabIndex(index, length) {
  if (!length) return -1;
  if (index === -1) return length - 1;
  return index >= 0 && index < length ? index : -1;
}

/**
 * Pure helper: the next server-tab value. `ids` are the server-tab connIds in
 * display order, '' is the "すべて" tab and sits before the first id.
 * @param {string} current  state.serverFilter ('' = all)
 * @param {string[]} ids
 * @param {number} delta
 * @returns {string}
 */
export function cycleServer(current, ids, delta) {
  const all = ['', ...(ids || [])];
  const i = all.indexOf(current || '');
  return all[cycleIndex(i, all.length, delta)];
}

/**
 * Display label for a shortcut, for tooltips (`title` attributes).
 * @param {ShortcutAction['type']} type
 * @param {ShortcutPlatform} platform
 * @param {number} [n] 1-based tab number for selectTab
 */
export function shortcutLabel(type, platform, n = 1) {
  const mac = platform === 'mac';
  const key = {
    newShell: 'T',
    closeTab: 'W',
    nextTab: ']',
    prevTab: '[',
    selectTab: String(n),
    nextServer: mac ? '→' : 'Right',
    prevServer: mac ? '←' : 'Left',
    allServers: '0',
  }[type];
  if (!key) return '';
  const server = type === 'nextServer' || type === 'prevServer' || type === 'allServers';
  const tabMove = type === 'nextTab' || type === 'prevTab';
  if (mac) return '⌘' + (server ? '⌥' : '') + (tabMove ? '⇧' : '') + key;
  return 'Ctrl+Shift+' + (server ? 'Alt+' : '') + key;
}

/**
 * Builds the key handler.
 *
 * @param {{
 *   platform?: ShortcutPlatform,
 *   enabled?: () => boolean,            // false = never intercept (e.g. a modal is open)
 *   actions: Partial<Record<ShortcutAction['type'], (action: ShortcutAction) => void>>,
 *   onError?: (err: unknown) => void,
 * }} opts
 * @returns {((ev: KeyboardEvent) => boolean) & { onDocumentKeydown: (ev: KeyboardEvent) => void, platform: ShortcutPlatform }}
 *
 * handler(ev) -> false when claimed (xterm will not send it to the shell), true otherwise.
 * Only keydown runs the action; keypress/keyup of a claimed combo are swallowed too.
 * Auto-repeat runs only the tab/server movement actions, never newShell/closeTab.
 * Only shortcuts that have a callback in `actions` are claimed.
 *
 * handler.onDocumentKeydown is for document.addEventListener('keydown', …) so the
 * shortcuts also work while focus is outside xterm. It skips events the xterm
 * handler already processed (xterm does not stop propagation), so nothing fires twice.
 */
export function createShortcutHandler(opts) {
  const platform = (opts && opts.platform) || detectPlatform();
  const actions = (opts && opts.actions) || {};
  const enabled = (opts && opts.enabled) || (() => true);
  const onError = (opts && opts.onError) || ((err) => { console.error('[shortcuts]', err); });
  const seen = new WeakSet();
  const REPEATABLE = new Set(['nextTab', 'prevTab', 'nextServer', 'prevServer']);

  function handle(ev) {
    if (!ev || !enabled()) return true;
    const action = matchShortcut(ev, platform);
    if (!action || typeof actions[action.type] !== 'function') return true;
    if (ev.type && ev.type !== 'keydown') return false; // keypress / keyup of our combo
    if (typeof ev.preventDefault === 'function') ev.preventDefault();
    if (typeof ev.stopPropagation === 'function') ev.stopPropagation();
    if (ev.repeat && !REPEATABLE.has(action.type)) return false;
    try {
      const r = actions[action.type](action);
      if (r && typeof r.catch === 'function') r.catch(onError);
    } catch (err) {
      onError(err);
    }
    return false;
  }

  const handler = (ev) => {
    if (ev && typeof ev === 'object') {
      if (seen.has(ev)) {
        const a = matchShortcut(ev, platform);
        return !(a && typeof actions[a.type] === 'function');
      }
      seen.add(ev);
    }
    return handle(ev);
  };
  handler.onDocumentKeydown = (ev) => {
    if (!ev || seen.has(ev)) return;
    seen.add(ev);
    handle(ev);
  };
  handler.platform = platform;
  return handler;
}
