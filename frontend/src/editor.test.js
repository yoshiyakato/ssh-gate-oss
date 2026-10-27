import { describe, it, expect, vi, beforeEach } from 'vitest';
vi.mock('../wailsjs/go/main/App', () => ({
  SaveConnection: vi.fn(), TestConnectionConfig: vi.fn(), DisconnectConnection: vi.fn(),
  DeleteConnection: vi.fn(), ListSerialPorts: vi.fn(async () => []),
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
