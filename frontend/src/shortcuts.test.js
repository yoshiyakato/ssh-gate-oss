import { test } from 'node:test';
import assert from 'node:assert/strict';
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
  assert.equal(detectPlatform({ platform: 'MacIntel' }), 'mac');
  assert.equal(detectPlatform({ userAgentData: { platform: 'macOS' } }), 'mac');
  assert.equal(detectPlatform({ platform: 'Win32' }), 'other');
  assert.equal(detectPlatform({ platform: 'Linux x86_64' }), 'other');
  assert.equal(detectPlatform({}), 'other');
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
    assert.deepEqual(matchShortcut(e, platform), want, `${platform} ${JSON.stringify(e)}`);
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
    assert.equal(matchShortcut(e, platform), null, `${platform} ${JSON.stringify(e)}`);
  }
  assert.equal(matchShortcut(null, 'mac'), null);
});

test('cycleIndex / resolveTabIndex', () => {
  assert.equal(cycleIndex(0, 3, 1), 1);
  assert.equal(cycleIndex(2, 3, 1), 0);
  assert.equal(cycleIndex(0, 3, -1), 2);
  assert.equal(cycleIndex(-1, 3, 1), 0);
  assert.equal(cycleIndex(-1, 3, -1), 2);
  assert.equal(cycleIndex(5, 3, 1), 0);
  assert.equal(cycleIndex(0, 0, 1), -1);
  assert.equal(resolveTabIndex(0, 3), 0);
  assert.equal(resolveTabIndex(-1, 3), 2);
  assert.equal(resolveTabIndex(3, 3), -1);
  assert.equal(resolveTabIndex(0, 0), -1);
  assert.equal(resolveTabIndex(-1, 0), -1);
});

test('cycleServer includes the all tab', () => {
  const ids = ['CONN-a', 'CONN-b'];
  assert.equal(cycleServer('', ids, 1), 'CONN-a');
  assert.equal(cycleServer('CONN-a', ids, 1), 'CONN-b');
  assert.equal(cycleServer('CONN-b', ids, 1), '');
  assert.equal(cycleServer('', ids, -1), 'CONN-b');
  assert.equal(cycleServer('CONN-gone', ids, 1), '');
  assert.equal(cycleServer('', [], 1), '');
});

test('shortcutLabel', () => {
  assert.equal(shortcutLabel('newShell', 'mac'), '⌘T');
  assert.equal(shortcutLabel('nextTab', 'mac'), '⌘⇧]');
  assert.equal(shortcutLabel('nextServer', 'mac'), '⌘⌥→');
  assert.equal(shortcutLabel('selectTab', 'mac', 3), '⌘3');
  assert.equal(shortcutLabel('closeTab', 'other'), 'Ctrl+Shift+W');
  assert.equal(shortcutLabel('prevServer', 'other'), 'Ctrl+Shift+Alt+Left');
  assert.equal(shortcutLabel('bogus', 'other'), '');
});

test('I18N fragment: keys prefix, ja/en match', () => {
  const ja = Object.keys(SHORTCUTS_I18N.ja).sort();
  const en = Object.keys(SHORTCUTS_I18N.en).sort();
  assert.deepEqual(ja, en);
  for (const k of ja) assert.ok(k.startsWith('keys.'), k);
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
  assert.equal(h.platform, 'mac');

  const e = makeEvent('KeyT', MAC);
  assert.equal(h(e), false);
  assert.equal(e.prevented, 1);
  assert.deepEqual(calls, [{ type: 'newShell' }]);

  // same event seen again by the document listener: no second run
  h.onDocumentKeydown(e);
  assert.equal(calls.length, 1);
  // and xterm calling again for the same event still returns false
  assert.equal(h(e), false);
  assert.equal(calls.length, 1);

  // keypress / keyup of the same combo are swallowed without running
  assert.equal(h(makeEvent('KeyT', MAC, { type: 'keypress' })), false);
  assert.equal(h(makeEvent('KeyT', MAC, { type: 'keyup' })), false);
  assert.equal(calls.length, 1);

  // normal keys go to the shell
  const plain = makeEvent('KeyT', {});
  assert.equal(h(plain), true);
  assert.equal(plain.prevented, 0);

  // matched combo without a registered action is not claimed
  assert.equal(h(makeEvent('KeyW', MAC)), true);

  // selectTab
  assert.equal(h(makeEvent('Digit2', MAC)), false);
  assert.deepEqual(calls.at(-1), { type: 'selectTab', index: 1 });
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

  assert.equal(h(makeEvent('KeyW', WIN, { repeat: true })), false);
  assert.deepEqual(calls, []);
  assert.equal(h(makeEvent('PageDown', WIN, { repeat: true })), false);
  assert.deepEqual(calls, ['next']);

  on = false;
  assert.equal(h(makeEvent('KeyT', WIN)), true);
  assert.deepEqual(calls, ['next']);
  on = true;

  const d = makeEvent('KeyT', WIN);
  h.onDocumentKeydown(d);
  assert.deepEqual(calls, ['next', 'new']);
  assert.equal(d.prevented, 1);
  h.onDocumentKeydown(makeEvent('KeyC', WIN));
  assert.deepEqual(calls, ['next', 'new']);
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
  assert.equal(h(makeEvent('KeyT', MAC)), false);
  assert.equal(h(makeEvent('KeyW', MAC)), false);
  await new Promise((r) => setTimeout(r, 0));
  assert.deepEqual(errors, ['sync', 'async']);
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
    assert.deepEqual(matchShortcut(e, platform), expected, `${platform} ${e.code} ${e.key}`);
  }
});
