import { test, expect } from 'vitest';
import {
  SHORTCUTS_I18N,
  detectPlatform,
  matchShortcut,
  cycleIndex,
  resolveTabIndex,
  cycleServer,
  shortcutLabel,
  createShortcutHandler,
} from './shortcuts.js';

const ev = (code, mods = {}, extra = {}) => ({ type: 'keydown', code, key: '', ...mods, ...extra });
const MAC = { metaKey: true };
const WIN = { ctrlKey: true, shiftKey: true };

test('detectPlatform', () => {
  expect(detectPlatform({ platform: 'MacIntel' })).toBe('mac');
  expect(detectPlatform({ userAgentData: { platform: 'macOS' } })).toBe('mac');
  expect(detectPlatform({ platform: 'Win32' })).toBe('other');
  expect(detectPlatform({ platform: 'Linux x86_64' })).toBe('other');
  expect(detectPlatform({})).toBe('other');
});

test('matchShortcut table', () => {
  const rows = [
    // [platform, event, expected]
    ['mac', ev('KeyT', MAC), { type: 'newShell' }],
    ['mac', ev('KeyW', MAC), { type: 'closeTab' }],
    ['mac', ev('BracketRight', { ...MAC, shiftKey: true }), { type: 'nextTab' }],
    ['mac', ev('BracketLeft', { ...MAC, shiftKey: true }), { type: 'prevTab' }],
    ['mac', ev('Digit1', MAC), { type: 'selectTab', index: 0 }],
    ['mac', ev('Digit3', MAC), { type: 'selectTab', index: 2 }],
    ['mac', ev('Digit9', MAC), { type: 'selectTab', index: -1 }],
    ['mac', ev('ArrowRight', { ...MAC, altKey: true }), { type: 'nextServer' }],
    ['mac', ev('ArrowLeft', { ...MAC, altKey: true }), { type: 'prevServer' }],
    ['mac', ev('Digit0', { ...MAC, altKey: true }), { type: 'allServers' }],
    ['other', ev('KeyT', WIN), { type: 'newShell' }],
    ['other', ev('KeyW', WIN), { type: 'closeTab' }],
    ['other', ev('BracketRight', WIN), { type: 'nextTab' }],
    ['other', ev('BracketLeft', WIN), { type: 'prevTab' }],
    ['other', ev('PageDown', WIN), { type: 'nextTab' }],
    ['other', ev('PageUp', WIN), { type: 'prevTab' }],
    ['other', ev('Digit2', WIN), { type: 'selectTab', index: 1 }],
    ['other', ev('Numpad4', WIN), { type: 'selectTab', index: 3 }],
    ['other', ev('ArrowRight', { ...WIN, altKey: true }), { type: 'nextServer' }],
    ['other', ev('ArrowLeft', { ...WIN, altKey: true }), { type: 'prevServer' }],
    ['other', ev('Digit0', { ...WIN, altKey: true }), { type: 'allServers' }],
    // key fallback when code is missing (Shift changes key to upper case / symbol)
    ['other', { type: 'keydown', key: 'T', ...WIN }, { type: 'newShell' }],
    ['other', { type: 'keydown', key: '}', ...WIN }, { type: 'nextTab' }],
    ['mac', { type: 'keydown', key: 't', ...MAC }, { type: 'newShell' }],
  ];
  for (const [platform, e, want] of rows) {
    expect(matchShortcut(e, platform), `${platform} ${JSON.stringify(e)}`).toStrictEqual(want);
  }
});

test('matchShortcut lets shell keys through', () => {
  const rows = [
    // plain typing and normal terminal control keys
    ['mac', ev('KeyT')],
    ['mac', ev('KeyC', { ctrlKey: true })],       // Ctrl+C on mac goes to shell
    ['mac', ev('KeyW', { ctrlKey: true })],       // Ctrl+W (delete word)
    ['mac', ev('KeyT', { ...MAC, ctrlKey: true })],
    ['mac', ev('KeyT', { ...MAC, shiftKey: true })],
    ['mac', ev('BracketRight', MAC)],             // Cmd+] without Shift
    ['mac', ev('ArrowRight', MAC)],               // Cmd+Right (no Alt)
    ['mac', ev('ArrowRight', { ...MAC, altKey: true, shiftKey: true })],
    ['mac', ev('Digit0', MAC)],
    ['mac', ev('PageDown', { ...MAC, shiftKey: true })],
    ['other', ev('KeyC', { ctrlKey: true })],     // Ctrl+C
    ['other', ev('KeyW', { ctrlKey: true })],     // Ctrl+W
    ['other', ev('KeyT', { ctrlKey: true })],     // Ctrl+T (transpose)
    ['other', ev('KeyT', { shiftKey: true })],
    ['other', ev('KeyT', { ...WIN, metaKey: true })],
    ['other', ev('KeyT', MAC)],                   // Cmd on a non-mac platform
    ['other', ev('KeyC', WIN)],                   // Ctrl+Shift+C (copy) is not ours
    ['other', ev('KeyV', WIN)],
    ['other', ev('Digit0', WIN)],
    ['other', ev('KeyT', { ...WIN, altKey: true })],
    ['mac', ev('KeyT', MAC, { isComposing: true })],
    ['other', ev('KeyT', WIN, { keyCode: 229 })],
    ['mac', ev('', MAC)],
  ];
  for (const [platform, e] of rows) {
    expect(matchShortcut(e, platform), `${platform} ${JSON.stringify(e)}`).toBe(null);
  }
  expect(matchShortcut(null, 'mac')).toBe(null);
});

test('cycleIndex / resolveTabIndex', () => {
  expect(cycleIndex(0, 3, 1)).toBe(1);
  expect(cycleIndex(2, 3, 1)).toBe(0);
  expect(cycleIndex(0, 3, -1)).toBe(2);
  expect(cycleIndex(-1, 3, 1)).toBe(0);
  expect(cycleIndex(-1, 3, -1)).toBe(2);
  expect(cycleIndex(5, 3, 1)).toBe(0);
  expect(cycleIndex(0, 0, 1)).toBe(-1);
  expect(resolveTabIndex(0, 3)).toBe(0);
  expect(resolveTabIndex(-1, 3)).toBe(2);
  expect(resolveTabIndex(3, 3)).toBe(-1);
  expect(resolveTabIndex(0, 0)).toBe(-1);
  expect(resolveTabIndex(-1, 0)).toBe(-1);
});

test('cycleServer includes the all tab', () => {
  const ids = ['CONN-a', 'CONN-b'];
  expect(cycleServer('', ids, 1)).toBe('CONN-a');
  expect(cycleServer('CONN-a', ids, 1)).toBe('CONN-b');
  expect(cycleServer('CONN-b', ids, 1)).toBe('');
  expect(cycleServer('', ids, -1)).toBe('CONN-b');
  expect(cycleServer('CONN-gone', ids, 1)).toBe('');
  expect(cycleServer('', [], 1)).toBe('');
});

test('shortcutLabel', () => {
  expect(shortcutLabel('newShell', 'mac')).toBe('⌘T');
  expect(shortcutLabel('nextTab', 'mac')).toBe('⌘⇧]');
  expect(shortcutLabel('nextServer', 'mac')).toBe('⌘⌥→');
  expect(shortcutLabel('selectTab', 'mac', 3)).toBe('⌘3');
  expect(shortcutLabel('closeTab', 'other')).toBe('Ctrl+Shift+W');
  expect(shortcutLabel('prevServer', 'other')).toBe('Ctrl+Shift+Alt+Left');
  expect(shortcutLabel('bogus', 'other')).toBe('');
});

test('I18N fragment: keys prefix, ja/en match', () => {
  const ja = Object.keys(SHORTCUTS_I18N.ja).sort();
  const en = Object.keys(SHORTCUTS_I18N.en).sort();
  expect(ja).toStrictEqual(en);
  for (const k of ja) expect(k.startsWith('keys.'), k).toBeTruthy();
});

function makeEvent(code, mods, extra = {}) {
  const e = ev(code, mods, extra);
  e.prevented = 0;
  e.stopped = 0;
  e.preventDefault = () => { e.prevented++; };
  e.stopPropagation = () => { e.stopped++; };
  return e;
}

test('handler claims keys, runs action once, blocks shell', () => {
  const calls = [];
  const h = createShortcutHandler({
    platform: 'mac',
    actions: {
      newShell: (a) => calls.push(a),
      selectTab: (a) => calls.push(a),
    },
  });
  expect(h.platform).toBe('mac');

  const e = makeEvent('KeyT', MAC);
  expect(h(e)).toBe(false);
  expect(e.prevented).toBe(1);
  expect(calls).toStrictEqual([{ type: 'newShell' }]);

  // same event seen again by the document listener: no second run
  h.onDocumentKeydown(e);
  expect(calls.length).toBe(1);
  // and xterm calling again for the same event still returns false
  expect(h(e)).toBe(false);
  expect(calls.length).toBe(1);

  // keypress / keyup of the same combo are swallowed without running
  expect(h(makeEvent('KeyT', MAC, { type: 'keypress' }))).toBe(false);
  expect(h(makeEvent('KeyT', MAC, { type: 'keyup' }))).toBe(false);
  expect(calls.length).toBe(1);

  // normal keys go to the shell
  const plain = makeEvent('KeyT', {});
  expect(h(plain)).toBe(true);
  expect(plain.prevented).toBe(0);

  // matched combo without a registered action is not claimed
  expect(h(makeEvent('KeyW', MAC))).toBe(true);

  // selectTab
  expect(h(makeEvent('Digit2', MAC))).toBe(false);
  expect(calls.at(-1)).toStrictEqual({ type: 'selectTab', index: 1 });
});

test('handler: repeat only for movement, enabled() gate, document listener', () => {
  const calls = [];
  let on = true;
  const h = createShortcutHandler({
    platform: 'other',
    enabled: () => on,
    actions: {
      newShell: () => calls.push('new'),
      closeTab: () => calls.push('close'),
      nextTab: () => calls.push('next'),
    },
  });

  expect(h(makeEvent('KeyW', WIN, { repeat: true }))).toBe(false);
  expect(calls).toStrictEqual([]);
  expect(h(makeEvent('PageDown', WIN, { repeat: true }))).toBe(false);
  expect(calls).toStrictEqual(['next']);

  on = false;
  expect(h(makeEvent('KeyT', WIN))).toBe(true);
  expect(calls).toStrictEqual(['next']);
  on = true;

  const d = makeEvent('KeyT', WIN);
  h.onDocumentKeydown(d);
  expect(calls).toStrictEqual(['next', 'new']);
  expect(d.prevented).toBe(1);
  h.onDocumentKeydown(makeEvent('KeyC', WIN));
  expect(calls).toStrictEqual(['next', 'new']);
});

test('handler reports action errors without throwing', async () => {
  const errors = [];
  const h = createShortcutHandler({
    platform: 'mac',
    onError: (e) => errors.push(e.message),
    actions: {
      newShell: () => { throw new Error('sync'); },
      closeTab: () => Promise.reject(new Error('async')),
    },
  });
  expect(h(makeEvent('KeyT', MAC))).toBe(false);
  expect(h(makeEvent('KeyW', MAC))).toBe(false);
  await new Promise((r) => setTimeout(r, 0));
  expect(errors).toStrictEqual(['sync', 'async']);
});

test('brackets follow the printed key, so a JIS keyboard is not inverted', () => {
  // JIS: key printed "[" is code BracketRight, "]" is Backslash, "@" is BracketLeft.
  const jis = [
    ['mac', ev('BracketRight', { ...MAC, shiftKey: true }, { key: '{' }), { type: 'prevTab' }],
    ['mac', ev('Backslash', { ...MAC, shiftKey: true }, { key: '}' }), { type: 'nextTab' }],
    ['mac', ev('BracketLeft', { ...MAC, shiftKey: true }, { key: '`' }), null],
    ['other', ev('BracketRight', WIN, { key: '{' }), { type: 'prevTab' }],
    ['other', ev('Backslash', WIN, { key: '}' }), { type: 'nextTab' }],
    ['other', ev('BracketLeft', WIN, { key: '`' }), null],
    // US layout with key present still works.
    ['mac', ev('BracketLeft', { ...MAC, shiftKey: true }, { key: '{' }), { type: 'prevTab' }],
    ['mac', ev('BracketRight', { ...MAC, shiftKey: true }, { key: '}' }), { type: 'nextTab' }],
    // No printable key (e.g. 'Unidentified') falls back to code.
    ['other', ev('BracketLeft', WIN, { key: 'Unidentified' }), { type: 'prevTab' }],
  ];
  for (const [platform, e, expected] of jis) {
    expect(matchShortcut(e, platform), `${platform} ${e.code} ${e.key}`).toStrictEqual(expected);
  }
});
