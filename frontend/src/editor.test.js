import { describe, it, expect, vi, beforeEach } from 'vitest';
vi.mock('../wailsjs/go/main/App', () => ({
  SaveConnection: vi.fn(), TestConnectionConfig: vi.fn(), DisconnectConnection: vi.fn(),
  DeleteConnection: vi.fn(), ListSerialPorts: vi.fn(async () => []),
  GetHostKeyInfo: vi.fn(async (id) => ({ connId: id, address: 'h:22', known: false, keys: [] })), ResetHostKey: vi.fn(),
  TestConnectionWithPassphraseSave: vi.fn(),
}));
import * as App from '../wailsjs/go/main/App';
import { mergeI18n, interpolate } from './model.js';
import * as E from './editor.js';

const I18N = mergeI18n(E.EDITOR_I18N, (await import('./model.js')).MODEL_I18N);
const saved = () => [{ id: 'A', name: 'web-01', type: 'SSH', host: 'h', port: 22, user: 'u', authMethod: 'SSH鍵認証', credential: '~/.ssh/id', tags: [], description: '', connectTimeout: 10, commandTimeout: 30, baudRate: 0, serialPort: '' },
  { id: 'B', name: 'db', type: 'SSH', host: 'h2', port: 22, user: 'u', authMethod: 'パスワード保管', credential: '', credentialSet: true, tags: [], description: '', connectTimeout: 10, commandTimeout: 30 }];
let state, ctx, sessions;
function render() { document.querySelector('#page-root').innerHTML = E.renderConnectionsPage(state, ctx); }
function fire(el, type) { const ev = new Event(type, { bubbles: true, cancelable: true }); el.dispatchEvent(ev); return ev; }
async function click(sel) { const el = document.querySelector(sel); return E.handleEditorClick({ target: el }, ctx); }
function type(name, value) { const el = document.querySelector('#connection-form [name="' + name + '"]'); el.value = value; return E.handleEditorInput({ target: el }, ctx); }
const tick = () => new Promise((r) => setTimeout(r, 0));
async function modalClick(sel) { await tick(); document.querySelector('#editor-modal-root ' + sel).click(); }
beforeEach(() => {
  document.body.innerHTML = '<div id="app"><div id="page-root"></div></div>';
  sessions = [];
  state = { lang: 'ja', data: { connections: saved(), sessions }, editor: E.createEditorState() };
  ctx = { state, t: (k, v) => interpolate(I18N[state.lang][k] ?? k, v), lang: () => state.lang, toast: vi.fn(), renderPage: render, bumpGen: vi.fn(),
    refresh: vi.fn(async () => { E.reconcileEditor(state); }), openTerminal: vi.fn(), sessionFor: (id) => sessions.find((s) => s.connId === id) };
  E.reconcileEditor(state); render();
});
describe('editor', () => {
  it('keeps input across rerender and lang switch', () => {
    type('name', 'web-02'); expect(E.isEditorDirty(state)).toBe(true);
    state.lang = 'en'; render();
    expect(document.querySelector('[name=name]').value).toBe('web-02');
    expect(document.querySelector('.dirty-mark')).toBeTruthy();
    expect(document.querySelector('[data-action=edit-open-terminal]').disabled).toBe(true);
    expect(document.querySelector('[data-action=edit-open-terminal]').title).toBe('Save changes first');
    expect(document.querySelector('.badge-save-modified')).toBeTruthy();
  });
  it('save success moves selection and rename does not add rows', async () => {
    type('name', 'web-02');
    App.SaveConnection.mockResolvedValue({ ok: true, id: 'A', message: 'saved' });
    ctx.refresh = vi.fn(async () => { state.data.connections = state.data.connections.map((c) => c.id === 'A' ? { ...c, name: 'web-02' } : c); E.reconcileEditor(state); });
    await E.handleEditorSubmit({ target: document.querySelector('#connection-form'), preventDefault() {} }, ctx);
    expect(App.SaveConnection.mock.calls[0][0].name).toBe('web-02');
    expect(state.editor.draft).toBe(null); expect(state.editor.selectedId).toBe('A');
    expect(document.querySelectorAll('[data-action=edit-select]').length).toBe(2);
  });
  it('save failure keeps draft and alert', async () => {
    type('name', 'x'); App.SaveConnection.mockResolvedValue({ ok: false, message: 'dup' });
    await E.handleEditorSubmit({ target: document.querySelector('#connection-form'), preventDefault() {} }, ctx);
    expect(state.editor.draft.name).toBe('x'); expect(document.querySelector('.editor-alert').textContent).toContain('dup');
  });
  it('new twice -> one draft; cancel returns', async () => {
    await click('[data-action=edit-new]'); await click('[data-action=edit-new]');
    expect(document.querySelectorAll('[data-action=edit-select-new]').length).toBe(1);
    expect(state.editor.selectedId).toBe('');
    await click('[data-action=edit-cancel]');
    expect(state.editor.selectedId).toBe('A'); expect(state.editor.draft).toBe(null);
  });
  it('leave dialog discard / cancel / save', async () => {
    type('name', 'zz');
    let p = E.selectConnection('B', ctx); await modalClick('[data-modal-dismiss=button]');
    expect(await p).toBe(false); expect(state.editor.selectedId).toBe('A');
    p = E.selectConnection('B', ctx); await modalClick('[data-modal-value=discard]');
    expect(await p).toBe(true); expect(state.editor.selectedId).toBe('B'); expect(state.editor.draft).toBe(null);
    type('host', 'h3'); App.SaveConnection.mockResolvedValue({ ok: false, message: 'err' });
    p = E.confirmLeaveEditor(ctx); await modalClick('[data-modal-value=save]');
    expect(await p).toBe(false);
  });
  it('disconnect dialog has k and j', async () => {
    sessions.push({ connId: 'A', connState: 'connected', shells: 2, connecting: 1, running: 4, connectedSince: new Date(Date.now() - 600000).toISOString() });
    render();
    expect(document.querySelector('#editor-badges .badge-conn-connected').textContent).toContain('10');
    App.DisconnectConnection.mockResolvedValue({ ok: true, message: 'bye' });
    const p = click('[data-action=edit-disconnect]'); await tick();
    expect(document.querySelector('#editor-modal-root').textContent).toContain('ターミナル 3 タブと実行中コマンド 4 件');
    document.querySelector('#editor-modal-root [data-modal-value=ok]').click(); await p;
    expect(App.DisconnectConnection).toHaveBeenCalledWith('A');
  });
  it('passphrase flow on test', async () => {
    App.TestConnectionConfig.mockResolvedValueOnce({ ok: false, code: 'needs_passphrase', message: 'pp' }).mockResolvedValueOnce({ ok: true, message: 'ok!' });
    const p = click('[data-action=edit-test]'); await tick(); await tick();
    document.querySelector('#editor-modal-input').value = 'secret';
    fire(document.querySelector('#editor-modal-root form'), 'submit'); await p;
    expect(App.TestConnectionConfig.mock.calls[1][1]).toBe('secret');
    expect(document.querySelector('[role=status]').textContent).toContain('ok!');
  });
  it('passphrase dialog offers 「キーチェーンに保存」 (off by default) and saves only when checked', async () => {
    App.TestConnectionConfig.mockReset();
    App.TestConnectionWithPassphraseSave.mockReset();
    App.TestConnectionConfig.mockResolvedValueOnce({ ok: false, code: 'needs_passphrase', message: 'pp' }).mockResolvedValueOnce({ ok: true, message: 'ok!' });
    let p = click('[data-action=edit-test]'); await tick(); await tick();
    const box = document.querySelector('#editor-modal-save-passphrase');
    expect(box).not.toBeNull();
    expect(box.checked).toBe(false);
    document.querySelector('#editor-modal-input').value = 'secret';
    fire(document.querySelector('#editor-modal-root form'), 'submit'); await p;
    expect(App.TestConnectionWithPassphraseSave).not.toHaveBeenCalled();

    App.TestConnectionConfig.mockResolvedValueOnce({ ok: false, code: 'needs_passphrase', message: 'pp' }).mockResolvedValueOnce({ ok: true, message: 'ok!' });
    App.TestConnectionWithPassphraseSave.mockResolvedValue({ ok: true, message: 'kept in keychain' });
    p = click('[data-action=edit-test]'); await tick(); await tick();
    document.querySelector('#editor-modal-save-passphrase').checked = true;
    document.querySelector('#editor-modal-input').value = 'secret';
    fire(document.querySelector('#editor-modal-root form'), 'submit'); await p;
    expect(App.TestConnectionWithPassphraseSave).toHaveBeenCalledWith('A', 'secret', true);
    expect(ctx.toast).toHaveBeenCalledWith('kept in keychain');
  });
  it('needs_password on test goes through ctx.recoverCredential and retries once', async () => {
    App.TestConnectionConfig.mockReset();
    await E.selectConnection('B', ctx);
    App.TestConnectionConfig.mockResolvedValueOnce({ ok: false, code: 'needs_password', message: 'gone' }).mockResolvedValueOnce({ ok: true, message: 'ok!' });
    ctx.recoverCredential = vi.fn(async () => true);
    await click('[data-action=edit-test]');
    expect(ctx.recoverCredential).toHaveBeenCalledTimes(1);
    expect(ctx.recoverCredential.mock.calls[0][0].id).toBe('B');
    expect(App.TestConnectionConfig).toHaveBeenCalledTimes(2);
    expect(state.editor.test.ok).toBe(true);
  });
  it('a password the keychain refused is offered for this session', async () => {
    await E.selectConnection('B', ctx);
    type('credential', 'new-pw');
    App.SaveConnection.mockResolvedValue({ ok: false, code: 'keyring_unavailable', message: 'no keychain' });
    ctx.recoverCredential = vi.fn(async () => true);
    await E.handleEditorSubmit({ target: document.querySelector('#connection-form'), preventDefault() {} }, ctx);
    expect(ctx.recoverCredential).toHaveBeenCalledWith(expect.objectContaining({ id: 'B' }), expect.objectContaining({ code: 'keyring_unavailable' }), { password: 'new-pw' });
    expect(state.editor.draft).not.toBe(null);
  });
  it('password set badge and clear', async () => {
    await E.selectConnection('B', ctx);
    expect(document.querySelector('#connection-form').textContent).toContain('設定済み');
    type('credential', 'pw'); expect(E.isEditorDirty(state)).toBe(true);
    await click('[data-action=edit-clear-password]'); expect(E.isEditorDirty(state)).toBe(false);
  });
  it('promptPassphrase cancel returns null', async () => {
    const p = E.promptPassphrase({ name: 'x' }, ctx); await tick();
    fire(document.querySelector('#editor-modal-root .modal'), 'keydown'); // not escape
    document.querySelector('#editor-modal-root [data-modal-dismiss=close]').click();
    expect(await p).toBe(null);
  });
  it('search box counts as focus so polls leave it alone', () => {
    const search = document.querySelector('[data-action=edit-search]');
    search.focus();
    expect(E.editorHasFocus()).toBe(true);
    search.blur();
    expect(E.editorHasFocus()).toBe(false);
  });
  it('a finished connection test keeps the focused input and caret', async () => {
    const d = (() => { let r; const p = new Promise((res) => { r = res; }); return { p, r }; })();
    App.TestConnectionConfig.mockReturnValueOnce(d.p);
    const p = click('[data-action=edit-test]'); await tick();
    const description = document.querySelector('#connection-form [name=description]');
    description.focus();
    description.value = 'typing';
    E.handleEditorInput({ target: description }, ctx);
    d.r({ ok: false, message: 'timeout' }); await p;
    expect(document.querySelector('#connection-form [name=description]')).toBe(description);
    expect(document.activeElement).toBe(description);
    expect(document.querySelector('#editor-test-result [role=status]').textContent).toContain('timeout');
    expect(document.querySelector('[data-action=edit-test]').disabled).toBe(false);
    description.blur();
  });
  it('a finished connection test without focus redraws the page', async () => {
    App.TestConnectionConfig.mockResolvedValueOnce({ ok: true, message: 'fine' });
    const form = document.querySelector('#connection-form');
    await click('[data-action=edit-test]');
    expect(document.querySelector('#connection-form')).not.toBe(form);
    expect(document.querySelector('[role=status]').textContent).toContain('fine');
  });
  it('toggling key -> password -> key restores the key path', async () => {
    const auth = () => document.querySelector('#connection-form [name=authMethod]');
    const change = async (value) => { auth().value = value; await E.handleEditorChange({ target: auth() }, ctx); };
    await change('パスワード保管');
    expect(document.querySelector('#connection-form [name=credential]').type).toBe('password');
    expect(document.querySelector('#connection-form [name=credential]').value).toBe('');
    await change('SSH鍵認証');
    expect(document.querySelector('#connection-form [name=credential]').value).toBe('~/.ssh/id');
    expect(E.isEditorDirty(state)).toBe(false);
    // A path typed but not saved survives the round trip too.
    type('credential', '~/.ssh/other');
    await change('パスワード保管');
    await change('SSH鍵認証');
    expect(document.querySelector('#connection-form [name=credential]').value).toBe('~/.ssh/other');
  });
  it('refreshEditorChrome follows sessions without replacing inputs', () => {
    const name = document.querySelector('#connection-form [name=name]');
    expect(document.querySelector('[data-action=edit-disconnect]').disabled).toBe(true);
    sessions.push({ connId: 'A', connState: 'connected', shells: 1, connecting: 0, running: 0 });
    E.refreshEditorChrome(ctx);
    expect(document.querySelector('[data-action=edit-disconnect]').disabled).toBe(false);
    expect(document.querySelector('#editor-badges .badge-conn-connected')).toBeTruthy();
    expect(document.querySelector('#connection-form [name=name]')).toBe(name);
  });
  it('disconnect and delete drop the frontend tabs of that connection', async () => {
    ctx.closeTabsForConnection = vi.fn();
    sessions.push({ connId: 'A', connState: 'connecting', shells: 0, connecting: 1, running: 0 });
    render();
    App.DisconnectConnection.mockResolvedValue({ ok: true, message: 'bye' });
    let p = click('[data-action=edit-disconnect]'); await tick();
    document.querySelector('#editor-modal-root [data-modal-value=ok]').click(); await p;
    expect(ctx.closeTabsForConnection).toHaveBeenCalledWith('A');
    ctx.closeTabsForConnection.mockClear();
    App.DeleteConnection.mockResolvedValue({ ok: true, message: 'deleted' });
    p = click('[data-action=edit-delete][data-conn-id=B]'); await tick();
    document.querySelector('#editor-modal-root [data-modal-value=ok]').click(); await p;
    expect(ctx.closeTabsForConnection).toHaveBeenCalledWith('B');
  });
});

describe('editor phase 3-4', () => {
  const serial = () => ({ id: 'S', name: 'tty', type: 'Serial', host: '', port: 22, user: '', authMethod: 'SSH鍵認証', credential: '', tags: [], description: '', connectTimeout: 10, commandTimeout: 30, baudRate: 9600, serialPort: '/dev/ttyUSB0', dataBits: 8, parity: 'none', stopBits: '1' });
  const form = () => document.querySelector('#connection-form');
  const change = async (name, value) => { const el = form().querySelector(`[name="${name}"]`); el.value = value; await E.handleEditorChange({ target: el }, ctx); };
  const submit = () => E.handleEditorSubmit({ target: form(), preventDefault() {} }, ctx);
  const known = (id, address = 'h:22') => ({ connId: id, address, known: true, keys: [{ type: 'ssh-ed25519', fingerprint: 'SHA256:abc', recordedAt: '2026-10-01T09:00:00Z' }] });
  beforeEach(() => { App.SaveConnection.mockReset(); App.ResetHostKey.mockReset(); App.GetHostKeyInfo.mockClear(); });

  it('serial line selects feed the draft, the dirty state and the payload', async () => {
    state.data.connections.push(serial()); E.reconcileEditor(state);
    await E.selectConnection('S', ctx);
    expect(form().querySelector('[name=dataBits]').value).toBe('8');
    expect(form().querySelector('[name=parity]').value).toBe('none');
    expect(form().textContent).toContain('RTS/CTS');
    await change('parity', 'even');
    expect(E.isEditorDirty(state)).toBe(true);
    await change('stopBits', '2');
    App.SaveConnection.mockResolvedValue({ ok: true, id: 'S', message: 'saved' });
    await submit();
    const sent = App.SaveConnection.mock.calls[0][0];
    expect(sent).toMatchObject({ type: 'Serial', dataBits: 8, parity: 'even', stopBits: '2', host: '', user: '', credential: '', mcpFirstTrust: false });
  });

  it('an SSH payload carries no serial leftovers (backend sameTarget)', () => {
    const p = E.connectionPayload({ ...saved()[0], baudRate: 115200, serialPort: '/dev/x', dataBits: 7, parity: 'odd', stopBits: '2' });
    expect(p).toMatchObject({ type: 'SSH', host: 'h', serialPort: '', baudRate: 0, dataBits: 0, parity: '', stopBits: '' });
  });

  it('save-time check shows the field error and does not call SaveConnection', async () => {
    ctx.platform = 'windows';
    state.data.connections.push({ ...serial(), dataBits: 5, stopBits: '2' }); E.reconcileEditor(state);
    await E.selectConnection('S', ctx);
    await change('parity', 'odd');
    await submit();
    expect(App.SaveConnection).not.toHaveBeenCalled();
    const sel = form().querySelector('[name=stopBits]');
    expect(sel.classList.contains('is-invalid')).toBe(true);
    expect(document.querySelector('[data-field-error=stopBits]').textContent).toContain('1.5');
    expect(document.querySelector('.editor-alert')).toBeTruthy();
    await change('stopBits', '1.5');
    expect(document.querySelector('[data-field-error=stopBits]')).toBe(null);
    expect(document.querySelector('.editor-alert')).toBe(null);
  });

  it('a backend field error is shown next to the field', async () => {
    state.data.connections.push(serial()); E.reconcileEditor(state);
    await E.selectConnection('S', ctx);
    await change('parity', 'mark');
    App.SaveConnection.mockResolvedValue({ ok: false, code: 'invalid_serial_mode', field: 'parity', message: 'この OS ではマーク/スペースパリティを使えません' });
    await submit();
    expect(document.querySelector('[data-field-error=parity]').textContent).toContain('マーク');
    expect(state.editor.draft.parity).toBe('mark');
  });

  it('a backend serial error without a field shows only the top alert and keeps the draft', async () => {
    state.data.connections.push(serial()); E.reconcileEditor(state);
    await E.selectConnection('S', ctx);
    await change('parity', 'even');
    App.SaveConnection.mockResolvedValue({ ok: false, code: 'invalid_serial_mode', message: 'この組み合わせは使えません' });
    await submit();
    expect(document.querySelector('[data-field-error]')).toBe(null);
    expect(document.querySelector('.editor-alert').textContent).toContain('この組み合わせは使えません');
    expect(state.editor.draft.parity).toBe('even');
  });

  it('macOS disables mark / space parity and 1.5 stop bits', async () => {
    ctx.platform = () => 'darwin';
    state.data.connections.push(serial()); E.reconcileEditor(state);
    await E.selectConnection('S', ctx);
    expect(form().querySelector('[name=parity] option[value=mark]').disabled).toBe(true);
    expect(form().querySelector('[name=parity] option[value=odd]').disabled).toBe(false);
    expect(form().querySelector('[name=stopBits] option[value="1.5"]').disabled).toBe(true);
    expect(E.serialLineIssue({ dataBits: 8, parity: 'space', stopBits: '1' }, 'darwin').field).toBe('parity');
    expect(E.serialLineIssue({ dataBits: 8, parity: 'space', stopBits: '1' }, 'linux')).toBe(null);
    expect(E.serialLineIssue({ dataBits: 5, parity: 'none', stopBits: '1.5' }, 'windows')).toBe(null);
    expect(E.serialLineIssue({ dataBits: 9 }, '').field).toBe('dataBits');
  });

  it('switching an SSH connection to Serial warns that SSH credentials go away', async () => {
    expect(document.querySelector('.editor-serial-drops-ssh')).toBe(null);
    await change('type', 'Serial');
    expect(document.querySelector('.editor-serial-drops-ssh').textContent).toContain('削除されます');
    await change('type', 'SSH');
    expect(form().querySelector('[name=host]').value).toBe('h');
  });

  it('host key block shows fingerprint, date, scope and other connections with textContent', async () => {
    state.data.connections.push({ ...saved()[0], id: 'C', name: '<img src=x onerror=alert(1)>', host: 'H' });
    App.GetHostKeyInfo.mockResolvedValue(known('A'));
    state.editor.hostKey = E.createEditorState().hostKey; render(); await tick(); await tick();
    const block = document.querySelector('#editor-hostkey');
    expect(block.textContent).toContain('SHA256:abc');
    expect(block.textContent).toContain('記録');
    expect(block.textContent).toContain('h:22');
    expect(block.textContent).toContain('<img src=x onerror=alert(1)>');
    expect(block.querySelector('img')).toBe(null);
    expect(block.querySelector('[data-action=edit-reset-hostkey]').disabled).toBe(false);
    expect(App.GetHostKeyInfo).toHaveBeenCalledWith('A');
    // Cached: a rerender does not ask again.
    App.GetHostKeyInfo.mockClear(); render(); await tick();
    expect(App.GetHostKeyInfo).not.toHaveBeenCalled();
    expect(document.querySelector('#editor-hostkey').textContent).toContain('SHA256:abc');
  });

  it('reset host key confirms with every affected connection, then reloads', async () => {
    state.data.connections.push({ ...saved()[0], id: 'C', name: 'web-01-alias', host: 'H' });
    App.GetHostKeyInfo.mockResolvedValue(known('A'));
    render(); await tick(); await tick();
    // Cancel: nothing happens.
    let p = click('[data-action=edit-reset-hostkey]'); await tick();
    const modal = document.querySelector('#editor-modal-root');
    expect(modal.textContent).toContain('同じホストとポートを使う接続先すべて');
    expect([...modal.querySelectorAll('.editor-hostkey-affected li')].map((li) => li.textContent)).toEqual(['web-01', 'web-01-alias']);
    modal.querySelector('[data-modal-dismiss=button]').click(); await p;
    expect(App.ResetHostKey).not.toHaveBeenCalled();
    // OK: resets and reloads.
    App.ResetHostKey.mockResolvedValue({ ok: true, id: 'A', message: 'removed' });
    App.GetHostKeyInfo.mockResolvedValue({ connId: 'A', address: 'h:22', known: false, keys: [] });
    p = click('[data-action=edit-reset-hostkey]'); await tick();
    document.querySelector('#editor-modal-root [data-modal-value=ok]').click(); await p; await tick(); await tick();
    expect(App.ResetHostKey).toHaveBeenCalledWith('A');
    expect(ctx.toast).toHaveBeenCalledWith('removed');
    expect(document.querySelector('#editor-hostkey').textContent).toContain('未記録');
    expect(document.querySelector('[data-action=edit-reset-hostkey]').disabled).toBe(true);
  });

  it('a connection test reloads the host key (TOFU records it)', async () => {
    render(); await tick(); await tick();
    App.GetHostKeyInfo.mockClear();
    App.GetHostKeyInfo.mockResolvedValue(known('A'));
    App.TestConnectionConfig.mockResolvedValueOnce({ ok: true, message: 'fine' });
    await click('[data-action=edit-test]'); await tick(); await tick();
    expect(App.GetHostKeyInfo).toHaveBeenCalledWith('A');
    expect(document.querySelector('#editor-hostkey').textContent).toContain('SHA256:abc');
  });

  it('new drafts and serial rows have no host key lookup', async () => {
    App.GetHostKeyInfo.mockClear();
    await click('[data-action=edit-new]'); await tick();
    expect(document.querySelector('#editor-hostkey')).toBe(null);
    expect(form().textContent).toContain('保存して接続テストすると記録します');
    expect(App.GetHostKeyInfo).not.toHaveBeenCalled();
  });

  it('onSelectionChange follows user selection only', async () => {
    ctx.onSelectionChange = vi.fn();
    await click('[data-action="edit-select"][data-conn-id="B"]');
    expect(ctx.onSelectionChange).toHaveBeenCalledWith('B', { previousId: 'A', reason: 'select' });
    ctx.onSelectionChange.mockClear();
    await E.selectConnection('A', ctx, { notify: false });
    expect(state.editor.selectedId).toBe('A');
    expect(ctx.onSelectionChange).not.toHaveBeenCalled();
    await click('[data-action=edit-new]');
    expect(ctx.onSelectionChange).toHaveBeenCalledWith('', { previousId: 'A', reason: 'new' });
    ctx.onSelectionChange.mockClear();
    App.SaveConnection.mockResolvedValue({ ok: true, id: 'N', message: 'ok' });
    ctx.refresh = vi.fn(async () => { state.data.connections.push({ ...saved()[0], id: 'N', name: 'new' }); E.reconcileEditor(state); });
    type('user', 'me');
    await submit();
    expect(ctx.onSelectionChange).toHaveBeenCalledWith('N', { previousId: '', reason: 'save' });
    ctx.onSelectionChange.mockClear();
    state.data.connections = state.data.connections.filter((c) => c.id !== 'N');
    E.reconcileEditor(state);
    expect(ctx.onSelectionChange).not.toHaveBeenCalled();
  });
});

describe('editor audit fixes', () => {
  const auth = () => document.querySelector('#connection-form [name=authMethod]');
  const cred = () => document.querySelector('#connection-form [name=credential]');
  const change = async (value) => { auth().value = value; await E.handleEditorChange({ target: auth() }, ctx); };

  // #35 / N7: the credential is never carried over by guessing its shape.
  it('a password that looks like a path never lands in the key path field', async () => {
    await click('[data-action="edit-select"][data-conn-id="B"]');
    type('credential', '/Tr0ub4dor&3');
    await change('SSH鍵認証');
    expect(cred().type).toBe('text');
    expect(cred().value).toBe('');
    expect(state.editor.draft.credential).toBe('');
  });
  it('a password typed after key -> password is dropped; the key path comes back', async () => {
    await change('パスワード保管');
    type('credential', '~secret%pw');
    await change('SSH鍵認証');
    expect(cred().value).toBe('~/.ssh/id');
  });
  it('a key path of any shape never stays as the password', async () => {
    for (const path of ['C:/keys/id_rsa', 'id_rsa', 'keys/id_ed25519']) {
      type('credential', path);
      await change('パスワード保管');
      expect(cred().type).toBe('password');
      expect(cred().value).toBe('');
      await change('SSH鍵認証');
      expect(cred().value).toBe(path);
    }
  });

  // N23: the delete confirm says running commands will be stopped.
  it('the delete confirm counts running commands it will stop', async () => {
    sessions.push({ connId: 'B', connState: 'disconnected', shells: 1, connecting: 0, running: 3 });
    render();
    App.DeleteConnection.mockClear();
    App.DeleteConnection.mockResolvedValue({ ok: true, message: 'deleted' });
    const p = click('[data-action=edit-delete][data-conn-id=B]'); await tick();
    const modal = document.querySelector('#editor-modal-root');
    expect(modal.querySelector('[data-delete-running]').textContent).toBe('実行中のコマンド 3 件も中止します。');
    expect(modal.textContent).toContain('ターミナル 1 タブ');
    modal.querySelector('[data-modal-value=ok]').click(); await p;
    expect(App.DeleteConnection).toHaveBeenCalledWith('B');
  });

  // E5: SaveConnection names name/host/user; the error sits next to the input.
  it('a required or duplicate-name error is shown next to that text field', async () => {
    App.SaveConnection.mockReset();
    for (const [field, code] of [['name', 'duplicate_name'], ['host', 'required'], ['user', 'required']]) {
      type(field, field === 'name' ? 'db' : '');
      App.SaveConnection.mockResolvedValue({ ok: false, code, field, message: `${field} のエラー` });
      await E.handleEditorSubmit({ target: document.querySelector('#connection-form'), preventDefault() {} }, ctx);
      const input = document.querySelector(`#connection-form [name="${field}"]`);
      expect(input.classList.contains('is-invalid')).toBe(true);
      expect(input.getAttribute('aria-invalid')).toBe('true');
      expect(document.querySelector(`[data-field-error="${field}"]`).textContent).toBe(`${field} のエラー`);
      expect(document.querySelectorAll('[data-field-error]').length).toBe(1);
    }
    state.lang = 'en'; render();
    expect(document.querySelector('[data-field-error="user"]')).toBeTruthy();
  });

  // N4: running commands alone (connState disconnected) can be cut.
  it('a connection with only running commands can be disconnected', async () => {
    sessions.push({ connId: 'A', connState: 'disconnected', shells: 0, connecting: 0, running: 2 });
    render();
    expect(document.querySelector('[data-action=edit-disconnect]').disabled).toBe(false);
    App.DisconnectConnection.mockClear();
    App.DisconnectConnection.mockResolvedValue({ ok: true, message: 'bye' });
    const p = click('[data-action=edit-disconnect]'); await tick();
    expect(document.querySelector('#editor-modal-root').textContent).toContain('実行中コマンド 2 件');
    document.querySelector('#editor-modal-root [data-modal-value=ok]').click(); await p;
    expect(App.DisconnectConnection).toHaveBeenCalledWith('A');
  });

  // N5: an action that finishes after the user left the page does not
  // redraw the page they are on now.
  it('a connection test that finishes on another page leaves that page alone', async () => {
    state.active = 'connections';
    const d = (() => { let r; const p = new Promise((res) => { r = res; }); return { p, r }; })();
    App.TestConnectionConfig.mockReturnValueOnce(d.p);
    const p = click('[data-action=edit-test]'); await tick();
    state.active = 'commands';
    ctx.renderPage = vi.fn();
    d.r({ ok: true, message: 'fine' }); await p;
    expect(ctx.renderPage).not.toHaveBeenCalled();
    expect(state.editor.test.message).toBe('fine');
  });

  // E6: the editor shows ctx.resultMessage, not the raw backend message.
  it('save failure text goes through ctx.resultMessage', async () => {
    ctx.resultMessage = (result) => `EN:${result.code ?? '-'}`;
    type('name', '');
    App.SaveConnection.mockResolvedValue({ ok: false, code: 'name_required', message: '接続先名は必須です' });
    await E.handleEditorSubmit({ target: document.querySelector('#connection-form'), preventDefault() {} }, ctx);
    expect(ctx.toast).toHaveBeenCalledWith('EN:name_required');
    expect(document.querySelector('.editor-alert').textContent).toContain('EN:name_required');
    expect(document.querySelector('.editor-alert').textContent).not.toContain('接続先名');
  });
});
