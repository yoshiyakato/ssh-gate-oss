// Connection editor (design-v1 §6.5): the list, the edit form, and the
// delete / leave / disconnect / passphrase modals. The page is always drawn
// from state.editor.draft (when there is one) so polling, toasts and language
// switches never drop what the user typed. Selection and the draft are keyed
// by connection id, never by list index.

import {
  connectionDirtyState,
  connStateLabelKey,
  connStateSymbol,
  escapeHtml,
  formatElapsed,
  lastTestLabel,
  saveStateLabelKey,
} from './model.js';
import {
  DeleteConnection,
  DisconnectConnection,
  ListSerialPorts,
  SaveConnection,
  TestConnectionConfig,
} from '../wailsjs/go/main/App';

export const EDITOR_I18N = {
  ja: {
    'edit.listTitle': '接続先一覧',
    'edit.new': '新規',
    'edit.search': '検索...',
    'edit.searchAria': '接続先を検索',
    'edit.empty': '接続先がありません',
    'edit.noMatch': '一致する接続先がありません',
    'edit.count': '{n} 件を表示',
    'edit.unsavedMark': '未保存の変更あり',
    'edit.newRow': '新規（未保存）',
    'edit.notSelected': '接続先が選択されていません',
    'edit.notSelectedHint': '一覧から選ぶか「新規」で追加してください。',
    'edit.title': '接続先編集',
    'edit.subtitle': 'SSH / シリアル接続先を登録します。',
    'edit.newName': '新規接続先',
    'edit.unnamed': '名前のない接続先',
    'edit.name': '接続先名',
    'edit.type': '接続種類',
    'edit.typeSerial': 'Serial（シリアル）',
    'edit.host': 'ホスト名 / IPアドレス',
    'edit.port': 'ポート',
    'edit.user': 'ユーザー名',
    'edit.authMethod': '認証方式',
    'edit.authKey': 'SSH鍵認証',
    'edit.authPassword': 'パスワード保管',
    'edit.keyPath': '鍵ファイルパス',
    'edit.keyPathHelp': '空欄にすると ssh-agent を使います。',
    'edit.password': 'パスワード',
    'edit.passwordSet': '設定済み',
    'edit.passwordUnset': '未設定',
    'edit.passwordKeep': '空欄のまま保存すると、保存済みのパスワードを維持します。',
    'edit.passwordReplace': '保存すると、入力したパスワードに置き換えます。',
    'edit.passwordClear': 'クリア',
    'edit.passwordClearAria': '入力したパスワードを消す',
    'edit.connectTimeout': '接続タイムアウト（秒）',
    'edit.commandTimeout': 'コマンド実行タイムアウト（秒）',
    'edit.serialPort': 'シリアルポート',
    'edit.rescan': '再スキャン',
    'edit.noPorts': 'ポートが見つかりません',
    'edit.baudRate': 'ボーレート',
    'edit.tags': '許可タグ',
    'edit.description': '説明',
    'edit.calloutSerial': 'シリアル接続先です。「ターミナルを開く」でシリアルコンソールを開きます。',
    'edit.calloutSSH': 'MCP経由のコマンドは読み取り系を含めて承認キューへ送信します。',
    'edit.save': '保存',
    'edit.cancel': 'キャンセル',
    'edit.test': '接続テスト',
    'edit.openTerminal': 'ターミナルを開く',
    'edit.saveFirst': '先に保存してください',
    'edit.disconnect': '接続を切断',
    'edit.notConnected': '接続していません',
    'edit.delete': '削除',
    'edit.deleteAria': '{name} を削除',
    'edit.close': '閉じる',
    'edit.copy': 'コピー',
    'edit.copied': 'コピーしました',
    'edit.testResult': '直近の接続テスト結果',
    'edit.testing': '接続テスト中…',
    'edit.saving': '保存中…',
    'edit.delete.title': '接続先を削除',
    'edit.delete.body': 'を接続先一覧から削除します。',
    'edit.delete.history': 'コマンド履歴は監査用に残します。',
    'edit.deleteWithTabs': '開いているターミナル {k} タブも閉じます。',
    'edit.disconnect.title': '接続を切断',
    'edit.disconnect.body': '{name} への接続を切断します。',
    'edit.disconnectConfirm': 'ターミナル {k} タブと実行中コマンド {j} 件も終了します。',
    'edit.leave.title': '未保存の変更を破棄しますか？',
    'edit.leave.body': '{name} に保存していない変更があります。',
    'edit.leave.discard': '破棄して移動',
    'edit.leave.save': '保存して移動',
    'edit.passphrase.title': 'SSH鍵パスフレーズ',
    'edit.passphrase.body': '{name} の秘密鍵がパスフレーズで保護されています。',
    'edit.passphrase.label': 'パスフレーズ',
    'edit.passphrase.help': 'SQLiteには保存せず、このアプリ起動中のSSH接続だけに使います。',
    'edit.passphrase.submit': '続行',
  },
  en: {
    'edit.listTitle': 'Connection List',
    'edit.new': 'New',
    'edit.search': 'Search...',
    'edit.searchAria': 'Search connections',
    'edit.empty': 'No connections',
    'edit.noMatch': 'No matching connections',
    'edit.count': '{n} shown',
    'edit.unsavedMark': 'Unsaved changes',
    'edit.newRow': 'New (not saved)',
    'edit.notSelected': 'No connection selected',
    'edit.notSelectedHint': 'Pick one from the list or add one with "New".',
    'edit.title': 'Edit Connection',
    'edit.subtitle': 'Register an SSH / serial connection.',
    'edit.newName': 'New Connection',
    'edit.unnamed': 'Unnamed connection',
    'edit.name': 'Connection Name',
    'edit.type': 'Connection Type',
    'edit.typeSerial': 'Serial',
    'edit.host': 'Hostname / IP Address',
    'edit.port': 'Port',
    'edit.user': 'Username',
    'edit.authMethod': 'Authentication',
    'edit.authKey': 'SSH Key',
    'edit.authPassword': 'Stored Password',
    'edit.keyPath': 'Key File Path',
    'edit.keyPathHelp': 'Leave blank to use ssh-agent.',
    'edit.password': 'Password',
    'edit.passwordSet': 'Set',
    'edit.passwordUnset': 'Not set',
    'edit.passwordKeep': 'Saving with this field blank keeps the stored password.',
    'edit.passwordReplace': 'Saving replaces the stored password with the one entered.',
    'edit.passwordClear': 'Clear',
    'edit.passwordClearAria': 'Clear the entered password',
    'edit.connectTimeout': 'Connect Timeout (sec)',
    'edit.commandTimeout': 'Command Timeout (sec)',
    'edit.serialPort': 'Serial Port',
    'edit.rescan': 'Rescan',
    'edit.noPorts': 'No ports found',
    'edit.baudRate': 'Baud Rate',
    'edit.tags': 'Allowed Tags',
    'edit.description': 'Description',
    'edit.calloutSerial': 'This is a serial connection. "Open Terminal" opens a serial console.',
    'edit.calloutSSH': 'Commands via MCP, including read-only ones, are sent to the approval queue.',
    'edit.save': 'Save',
    'edit.cancel': 'Cancel',
    'edit.test': 'Test Connection',
    'edit.openTerminal': 'Open Terminal',
    'edit.saveFirst': 'Save changes first',
    'edit.disconnect': 'Disconnect',
    'edit.notConnected': 'Not connected',
    'edit.delete': 'Delete',
    'edit.deleteAria': 'Delete {name}',
    'edit.close': 'Close',
    'edit.copy': 'Copy',
    'edit.copied': 'Copied',
    'edit.testResult': 'Latest connection test result',
    'edit.testing': 'Testing…',
    'edit.saving': 'Saving…',
    'edit.delete.title': 'Delete Connection',
    'edit.delete.body': 'will be removed from the connection list.',
    'edit.delete.history': 'Command history is kept for auditing.',
    'edit.deleteWithTabs': 'This also closes {k} open terminal tab(s).',
    'edit.disconnect.title': 'Disconnect',
    'edit.disconnect.body': 'Disconnect from {name}.',
    'edit.disconnectConfirm': 'This also closes {k} terminal tab(s) and stops {j} running command(s).',
    'edit.leave.title': 'Discard unsaved changes?',
    'edit.leave.body': '{name} has changes that are not saved.',
    'edit.leave.discard': 'Discard and leave',
    'edit.leave.save': 'Save and leave',
    'edit.passphrase.title': 'SSH Key Passphrase',
    'edit.passphrase.body': "{name}'s private key is protected by a passphrase.",
    'edit.passphrase.label': 'Passphrase',
    'edit.passphrase.help': 'Not stored in SQLite; used only for SSH while this app is running.',
    'edit.passphrase.submit': 'Continue',
  },
};

const BAUD_RATES = [9600, 19200, 38400, 57600, 115200, 230400, 460800, 921600];
const AUTH_KEY = 'SSH鍵認証';
const AUTH_PASSWORD = 'パスワード保管';
// connState codes that get their own badge class; anything else renders as
// disconnected so an unexpected backend value never lands in a class name.
const CONN_STATES = ['connected', 'connecting', 'disconnected', 'error', 'in_use'];
const DISCONNECTABLE = ['connected', 'in_use', 'connecting'];

function emptyTest() {
  return { ok: null, message: '', target: '', connId: '' };
}

export function createEditorState() {
  return {
    selectedId: '',
    draft: null,
    returnId: '',
    test: emptyTest(),
    alert: '',
    serialPorts: [],
    filter: '',
    busy: '', // '' | 'save' | 'test' — disables the matching buttons while awaiting
    keyPathStash: null, // { id, value }: key path set aside while password auth is chosen
  };
}

function savedConnections(state) {
  return state.data?.connections ?? [];
}

function savedById(state, id) {
  if (!id) return undefined;
  return savedConnections(state).find((connection) => connection.id === id);
}

function hasNewDraft(editor) {
  return Boolean(editor.draft && editor.draft.id === '');
}

// The connection the form shows: the draft when it belongs to the selection,
// otherwise the saved value.
function currentConnection(state) {
  const editor = state.editor;
  if (editor.draft && editor.draft.id === editor.selectedId) return editor.draft;
  return savedById(state, editor.selectedId);
}

function currentId(state) {
  return currentConnection(state)?.id ?? '';
}

function dirtyStateOf(state) {
  const draft = state.editor.draft;
  if (!draft) return 'saved';
  return connectionDirtyState(draft, savedById(state, draft.id));
}

export function isEditorDirty(state) {
  return dirtyStateOf(state) !== 'saved';
}

// saveState of the form currently shown (a draft for another row does not count).
function currentSaveState(state) {
  const editor = state.editor;
  if (!editor.draft || editor.draft.id !== editor.selectedId) return 'saved';
  return dirtyStateOf(state);
}

function rowIsDirty(state, connection) {
  const draft = state.editor.draft;
  return Boolean(draft && draft.id === connection.id && isEditorDirty(state));
}

function connStateFor(ctx, id) {
  const code = (id && ctx.sessionFor?.(id)?.connState) || 'disconnected';
  return CONN_STATES.includes(code) ? code : 'disconnected';
}

function isPassword(connection) {
  return String(connection?.authMethod || '').includes('パスワード');
}

function displayName(connection, t) {
  return String(connection?.name || '').trim() || connection?.host || t('edit.unnamed');
}

function testTarget(connection) {
  if (connection.type === 'Serial') return `${connection.serialPort || '-'} @ ${connection.baudRate || 115200}`;
  return `${connection.user}@${connection.host}:${connection.port}`;
}

function newDraft(t) {
  return {
    id: '',
    name: t('edit.newName'),
    type: 'SSH',
    host: '127.0.0.1',
    port: 22,
    user: '',
    authMethod: AUTH_KEY,
    credential: '~/.ssh/id_ed25519',
    serialPort: '',
    baudRate: 115200,
    connectTimeout: 10,
    commandTimeout: 30,
    tags: [],
    description: '',
    credentialSet: false,
    mcpFirstTrust: false,
    lastTestResult: '',
    lastTestAt: '',
    lastTestMessage: '',
  };
}

// Copies the form into the draft. Fields not in the DOM (the other transport's
// fields) keep the draft / saved value, so switching SSH <-> Serial never drops
// them. The form is tagged with the id it was drawn for; a stale form is ignored.
function syncFormToDraft(state) {
  const form = document.querySelector('#connection-form');
  if (!form) return null;
  const editor = state.editor;
  const base = currentConnection(state);
  if (!base || form.dataset.connId !== base.id) return null;
  const data = Object.fromEntries(new FormData(form));
  const pick = (key) => (key in data ? String(data[key]) : (base[key] ?? ''));
  const pickNum = (key, fallback) => (key in data ? Number(data[key] || fallback) : (base[key] ?? fallback));
  editor.draft = {
    ...base,
    name: pick('name'),
    type: pick('type') === 'Serial' ? 'Serial' : 'SSH',
    host: pick('host'),
    port: pickNum('port', 22),
    user: pick('user'),
    authMethod: pick('authMethod') || AUTH_KEY,
    credential: pick('credential'),
    serialPort: pick('serialPort'),
    baudRate: pickNum('baudRate', 115200),
    connectTimeout: pickNum('connectTimeout', 10),
    commandTimeout: pickNum('commandTimeout', 30),
    tags: ('tags' in data ? String(data.tags) : (base.tags ?? []).join(', '))
      .split(',').map((tag) => tag.trim()).filter(Boolean),
    description: pick('description'),
  };
  return editor.draft;
}

// Ensures there is a draft for the current selection and returns it.
function ensureDraft(state) {
  const synced = syncFormToDraft(state);
  if (synced) return synced;
  const base = currentConnection(state);
  if (!base) return null;
  if (state.editor.draft !== base) state.editor.draft = { ...base, tags: [...(base.tags ?? [])] };
  return state.editor.draft;
}

// What SaveConnection / TestConnectionConfig receive. lastTest* and
// credentialSet are ignored by the backend; mcpFirstTrust must be sent back
// as-is because the update writes it.
function connectionPayload(connection) {
  return {
    id: connection.id ?? '',
    name: connection.name ?? '',
    type: connection.type === 'Serial' ? 'Serial' : 'SSH',
    host: connection.host ?? '',
    port: Number(connection.port) || 22,
    user: connection.user ?? '',
    authMethod: connection.authMethod || AUTH_KEY,
    credential: connection.credential ?? '',
    serialPort: connection.serialPort ?? '',
    baudRate: Number(connection.baudRate) || 115200,
    connectTimeout: Number(connection.connectTimeout) || 10,
    commandTimeout: Number(connection.commandTimeout) || 30,
    tags: [...(connection.tags ?? [])],
    description: connection.description ?? '',
    mcpFirstTrust: Boolean(connection.mcpFirstTrust),
  };
}

function resetTransient(editor) {
  editor.test = emptyTest();
  editor.alert = '';
  editor.keyPathStash = null;
}

// Drops the draft. A discarded new draft returns to the row that was selected
// before "New" was pressed.
function discardDraft(state) {
  const editor = state.editor;
  if (hasNewDraft(editor) && editor.selectedId === '') editor.selectedId = editor.returnId;
  editor.draft = null;
  editor.returnId = '';
  resetTransient(editor);
}

export function reconcileEditor(state) {
  const editor = state.editor;
  if (!editor) return;
  const connections = savedConnections(state);
  if (editor.draft && editor.draft.id && editor.draft.id !== editor.selectedId) {
    // A draft for a row that is no longer selected only survives while dirty.
    if (!isEditorDirty(state)) editor.draft = null;
  }
  if (editor.selectedId) {
    const exists = connections.some((connection) => connection.id === editor.selectedId);
    // A dirty draft of a connection deleted elsewhere is kept so the user can
    // still read (and re-save) it; otherwise fall back to the first row.
    if (!exists && !(editor.draft && editor.draft.id === editor.selectedId)) {
      editor.selectedId = connections[0]?.id ?? '';
      resetTransient(editor);
    }
  } else if (!hasNewDraft(editor)) {
    editor.selectedId = connections[0]?.id ?? '';
  }
  if (editor.returnId && !connections.some((connection) => connection.id === editor.returnId)) {
    editor.returnId = connections[0]?.id ?? '';
  }
}

// True while the user is typing on the connections page: in the form or in
// the list's search box (which sits outside the form).
export function editorHasFocus() {
  const active = document.activeElement;
  return Boolean(active && active.closest && active.closest('#connection-form, [data-action="edit-search"]'));
}

// ---------------------------------------------------------------- rendering

export function renderConnectionsPage(state, ctx) {
  const { t } = ctx;
  const editor = state.editor;
  return `
    <div class="row g-3">
      <div class="col-12 col-xl-4">
        <div class="card card-outline card-primary h-100">
          <div class="card-header">
            <div class="d-flex align-items-center justify-content-between">
              <h3 class="card-title fw-bold">${escapeHtml(t('edit.listTitle'))}</h3>
              <button class="btn btn-primary btn-sm" type="button" data-action="edit-new">
                <i class="bi bi-plus-lg me-1"></i>${escapeHtml(t('edit.new'))}
              </button>
            </div>
          </div>
          <div class="card-body p-0">
            <div class="p-3">
              <div class="input-group">
                <span class="input-group-text"><i class="bi bi-search"></i></span>
                <input class="form-control" type="search" data-action="edit-search" value="${escapeHtml(editor.filter)}" placeholder="${escapeHtml(t('edit.search'))}" aria-label="${escapeHtml(t('edit.searchAria'))}" autocapitalize="none" autocorrect="off" autocomplete="off" spellcheck="false">
              </div>
            </div>
            <div class="list-group list-group-flush ssh-list" id="editor-list">${renderList(state, ctx)}</div>
          </div>
          <div class="card-footer text-secondary" id="editor-list-count">${escapeHtml(t('edit.count', { n: visibleConnections(state).length }))}</div>
        </div>
      </div>
      <div class="col-12 col-xl-8">
        ${currentConnection(state) ? renderForm(state, ctx) : renderNoSelection(t)}
      </div>
    </div>
  `;
}

function visibleConnections(state) {
  const query = state.editor.filter.trim().toLowerCase();
  const connections = savedConnections(state);
  if (!query) return connections;
  return connections.filter((connection) => [connection.name, connection.host, connection.user, connection.serialPort, ...(connection.tags ?? [])]
    .some((value) => String(value ?? '').toLowerCase().includes(query)));
}

function renderList(state, ctx) {
  const { t } = ctx;
  const editor = state.editor;
  const rows = visibleConnections(state).map((connection) => connectionRow(state, ctx, connection));
  if (hasNewDraft(editor)) rows.push(newDraftRow(state, ctx));
  if (rows.length) return rows.join('');
  const message = savedConnections(state).length ? t('edit.noMatch') : t('edit.empty');
  return `<div class="p-4 text-center text-secondary">${escapeHtml(message)}</div>`;
}

function dirtyMark(t) {
  return `<span class="dirty-mark me-1" title="${escapeHtml(t('edit.unsavedMark'))}" aria-label="${escapeHtml(t('edit.unsavedMark'))}">●</span>`;
}

function rowSubtitle(connection) {
  return connection.type === 'Serial'
    ? `${escapeHtml(connection.serialPort || '-')} @ ${escapeHtml(connection.baudRate || 115200)}`
    : `${escapeHtml(connection.host)}:${escapeHtml(connection.port)} / ${escapeHtml(connection.user)}`;
}

function typeBadge(connection) {
  const isSerial = connection.type === 'Serial';
  return `<span class="badge ${isSerial ? 'text-bg-info' : 'text-bg-primary'} me-1">${isSerial ? 'Serial' : 'SSH'}</span>`;
}

function testBadgeClass(connection) {
  if (connection.lastTestResult === 'ok') return 'text-bg-success';
  if (connection.lastTestResult === 'failed') return 'text-bg-danger';
  return 'text-bg-secondary';
}

function testShortKey(connection) {
  if (connection.lastTestResult === 'ok') return 'state.test.ok';
  if (connection.lastTestResult === 'failed') return 'state.test.failed';
  return 'state.test.never';
}

// The row's right-hand badge: the live connection state while there is one,
// otherwise the last test result.
function rowStatusBadge(ctx, connection) {
  const { t } = ctx;
  const code = connStateFor(ctx, connection.id);
  if (code !== 'disconnected') {
    return `<span class="badge badge-conn-${code}">${escapeHtml(connStateSymbol(code))} ${escapeHtml(t(connStateLabelKey(code)))}</span>`;
  }
  return `<span class="badge ${testBadgeClass(connection)}" title="${escapeHtml(lastTestLabel(connection, t))}">${escapeHtml(t(testShortKey(connection)))}</span>`;
}

function connectionRow(state, ctx, saved) {
  const { t } = ctx;
  const editor = state.editor;
  const dirty = rowIsDirty(state, saved);
  // The row reflects the draft's name while editing so a rename is visible
  // before saving; it is still the same row (same id).
  const shown = dirty ? editor.draft : saved;
  const active = saved.id === editor.selectedId ? 'active' : '';
  return `
    <div class="list-group-item connection-item ${active}">
      <button class="connection-select" type="button" data-action="edit-select" data-conn-id="${escapeHtml(saved.id)}" aria-current="${active ? 'true' : 'false'}">
        <div>
          <div class="fw-bold">
            ${dirty ? dirtyMark(t) : ''}${typeBadge(shown)}${escapeHtml(shown.name)}
          </div>
          <small>${rowSubtitle(shown)}</small>
        </div>
        ${rowStatusBadge(ctx, saved)}
      </button>
      <button class="btn btn-sm connection-trash" type="button" data-action="edit-delete" data-conn-id="${escapeHtml(saved.id)}" aria-label="${escapeHtml(t('edit.deleteAria', { name: saved.name }))}" title="${escapeHtml(t('edit.delete'))}">
        <i class="bi bi-trash"></i>
      </button>
    </div>
  `;
}

function newDraftRow(state, ctx) {
  const { t } = ctx;
  const draft = state.editor.draft;
  const active = state.editor.selectedId === '' ? 'active' : '';
  return `
    <div class="list-group-item connection-item ${active}">
      <button class="connection-select" type="button" data-action="edit-select-new" aria-current="${active ? 'true' : 'false'}">
        <div>
          <div class="fw-bold">${dirtyMark(t)}${typeBadge(draft)}${escapeHtml(draft.name || t('edit.newName'))}</div>
          <small>${rowSubtitle(draft)}</small>
        </div>
        <span class="badge badge-save-new">${escapeHtml(t(saveStateLabelKey('new')))}</span>
      </button>
      <button class="btn btn-sm connection-trash" type="button" data-action="edit-delete" data-conn-id="" aria-label="${escapeHtml(t('edit.deleteAria', { name: draft.name || t('edit.newName') }))}" title="${escapeHtml(t('edit.delete'))}">
        <i class="bi bi-trash"></i>
      </button>
    </div>
  `;
}

function renderNoSelection(t) {
  return `
    <div class="card card-outline card-primary h-100">
      <div class="card-body d-flex flex-column align-items-center justify-content-center text-center text-secondary gap-2">
        <div class="fw-bold">${escapeHtml(t('edit.notSelected'))}</div>
        <div class="small">${escapeHtml(t('edit.notSelectedHint'))}</div>
        <button class="btn btn-primary btn-sm mt-2" type="button" data-action="edit-new">
          <i class="bi bi-plus-lg me-1"></i>${escapeHtml(t('edit.new'))}
        </button>
      </div>
    </div>
  `;
}

function renderBadges(state, ctx) {
  const { t } = ctx;
  const connection = currentConnection(state);
  const saveState = currentSaveState(state);
  const code = connStateFor(ctx, connection?.id);
  const session = connection?.id ? ctx.sessionFor?.(connection.id) : undefined;
  let connLabel = t(connStateLabelKey(code));
  if ((code === 'connected' || code === 'in_use') && session?.connectedSince) {
    const since = Date.parse(session.connectedSince);
    if (!Number.isNaN(since)) connLabel += ` ${formatElapsed(Math.max(0, Date.now() - since), ctx.lang())}`;
  }
  const connTitle = code === 'error' ? (session?.lastError || '') : (session?.label || '');
  return `
    <span class="badge badge-save-${saveState}">${escapeHtml(t(saveStateLabelKey(saveState)))}</span>
    <span class="badge badge-conn-${code}" title="${escapeHtml(connTitle)}">${escapeHtml(connStateSymbol(code))} ${escapeHtml(connLabel)}</span>
  `;
}

function renderActions(state, ctx) {
  const { t } = ctx;
  const editor = state.editor;
  const connection = currentConnection(state);
  const saveState = currentSaveState(state);
  const dirty = saveState !== 'saved';
  const code = connStateFor(ctx, connection?.id);
  const canOpen = saveState === 'saved' && Boolean(connection?.id);
  const canDisconnect = Boolean(connection?.id) && DISCONNECTABLE.includes(code);
  const saveFirst = canOpen ? '' : escapeHtml(t('edit.saveFirst'));
  const notConnected = canDisconnect ? '' : escapeHtml(t('edit.notConnected'));
  const busy = editor.busy;
  return `
    <button class="btn btn-primary" type="submit" data-action="edit-save" ${dirty && !busy ? '' : 'disabled'}>${escapeHtml(busy === 'save' ? t('edit.saving') : t('edit.save'))}</button>
    <button class="btn btn-outline-secondary" type="button" data-action="edit-cancel" ${dirty && !busy ? '' : 'disabled'}>${escapeHtml(t('edit.cancel'))}</button>
    <div class="ms-auto d-flex gap-2 flex-wrap">
      <button class="btn btn-outline-primary" type="button" data-action="edit-test" ${busy ? 'disabled' : ''}>
        <i class="bi bi-activity me-1"></i>${escapeHtml(busy === 'test' ? t('edit.testing') : t('edit.test'))}
      </button>
      <span class="d-inline-block" title="${saveFirst}">
        <button class="btn btn-outline-dark" type="button" data-action="edit-open-terminal" title="${saveFirst}" ${canOpen ? '' : 'disabled'}>
          <i class="bi bi-terminal-fill me-1"></i>${escapeHtml(t('edit.openTerminal'))}
        </button>
      </span>
      <span class="d-inline-block" title="${notConnected}">
        <button class="btn btn-outline-danger" type="button" data-action="edit-disconnect" title="${notConnected}" ${canDisconnect ? '' : 'disabled'}>
          <i class="bi bi-plug me-1"></i>${escapeHtml(t('edit.disconnect'))}
        </button>
      </span>
    </div>
  `;
}

function renderLastTest(state, ctx) {
  const { t } = ctx;
  const connection = currentConnection(state);
  const saved = savedById(state, connection?.id);
  if (!saved) return '';
  const message = saved.lastTestResult === 'failed' && saved.lastTestMessage ? ` — ${saved.lastTestMessage}` : '';
  return `<span class="${saved.lastTestResult === 'failed' ? 'text-danger' : 'text-secondary'}">${escapeHtml(lastTestLabel(saved, t))}${escapeHtml(message)}</span>`;
}

function renderTestResult(state, ctx) {
  const { t } = ctx;
  const test = state.editor.test;
  if (!test.message || test.connId !== currentId(state)) return '';
  return `
    <div class="alert ${test.ok ? 'alert-success' : 'alert-danger'} mt-3 mb-0" role="status">
      <div class="d-flex align-items-start justify-content-between gap-3">
        <div>
          <div class="fw-bold mb-1">${escapeHtml(t('edit.testResult'))}${test.target ? `: ${escapeHtml(test.target)}` : ''}</div>
          <div class="small">${escapeHtml(test.message)}</div>
        </div>
        <button class="btn btn-sm btn-outline-secondary flex-shrink-0" type="button" data-action="edit-copy-test">
          <i class="bi bi-copy me-1"></i>${escapeHtml(t('edit.copy'))}
        </button>
      </div>
    </div>
  `;
}

function formTitle(state, t) {
  const connection = currentConnection(state);
  return connection.id ? displayName(savedById(state, connection.id) ?? connection, t) : t('edit.title');
}

function renderAlert(state) {
  const alert = state.editor.alert;
  return alert ? `<div class="alert alert-danger editor-alert mb-3" role="alert">${escapeHtml(alert)}</div>` : '';
}

function renderForm(state, ctx) {
  const { t } = ctx;
  const connection = currentConnection(state);
  const isSerial = connection.type === 'Serial';
  return `
    <form class="card card-outline card-primary h-100" id="connection-form" data-conn-id="${escapeHtml(connection.id)}" novalidate>
      <div class="card-header editor-header">
        <div class="d-flex align-items-start justify-content-between gap-3 flex-wrap">
          <div>
            <h3 class="card-title fw-bold mb-1" id="editor-title">${escapeHtml(formTitle(state, t))}</h3>
            <div class="text-secondary small">${escapeHtml(t('edit.subtitle'))}</div>
          </div>
          <div class="d-flex align-items-center gap-1 flex-wrap" id="editor-badges">${renderBadges(state, ctx)}</div>
        </div>
      </div>
      <div class="card-body">
        <div id="editor-alert">${renderAlert(state)}</div>
        <div class="row g-3">
          ${inputCol(t('edit.name'), 'name', connection.name)}
          ${typeCol(t, connection.type)}
          ${isSerial ? serialFields(state, t, connection) : sshFields(state, t, connection)}
          ${inputCol(t('edit.tags'), 'tags', (connection.tags ?? []).join(', '), 'text', 'col-12')}
          <div class="col-12">
            <label class="form-label fw-bold">${escapeHtml(t('edit.description'))}</label>
            <textarea class="form-control" name="description" rows="4" autocapitalize="none" autocorrect="off" autocomplete="off" spellcheck="false">${escapeHtml(connection.description)}</textarea>
          </div>
        </div>
        <div class="small mt-3" id="editor-last-test">${renderLastTest(state, ctx)}</div>
        <div class="callout callout-info mt-3 mb-0">${escapeHtml(isSerial ? t('edit.calloutSerial') : t('edit.calloutSSH'))}</div>
        <div id="editor-test-result">${renderTestResult(state, ctx)}</div>
      </div>
      <div class="card-footer d-flex gap-2 flex-wrap editor-actions" id="editor-actions">${renderActions(state, ctx)}</div>
    </form>
  `;
}

function inputCol(label, name, value, type = 'text', col = 'col-12 col-md-6') {
  return `
    <div class="${col}">
      <label class="form-label fw-bold">${escapeHtml(label)}</label>
      <input class="form-control" name="${name}" type="${type}" value="${escapeHtml(value)}" autocapitalize="none" autocorrect="off" autocomplete="off" spellcheck="false">
    </div>
  `;
}

function typeCol(t, type) {
  const value = type === 'Serial' ? 'Serial' : 'SSH';
  return `
    <div class="col-12 col-md-6">
      <label class="form-label fw-bold">${escapeHtml(t('edit.type'))}</label>
      <select class="form-select" name="type" data-action="edit-type">
        <option value="SSH" ${value === 'SSH' ? 'selected' : ''}>SSH</option>
        <option value="Serial" ${value === 'Serial' ? 'selected' : ''}>${escapeHtml(t('edit.typeSerial'))}</option>
      </select>
    </div>
  `;
}

function sshFields(state, t, connection) {
  // The option value stays the raw backend string; only the label is localized.
  const auth = isPassword(connection) ? AUTH_PASSWORD : AUTH_KEY;
  const authOptions = [[AUTH_KEY, t('edit.authKey')], [AUTH_PASSWORD, t('edit.authPassword')]];
  return `
    ${inputCol(t('edit.host'), 'host', connection.host)}
    ${inputCol(t('edit.port'), 'port', connection.port, 'number')}
    ${inputCol(t('edit.user'), 'user', connection.user)}
    <div class="col-12 col-md-6">
      <label class="form-label fw-bold">${escapeHtml(t('edit.authMethod'))}</label>
      <select class="form-select" name="authMethod" data-action="edit-auth">
        ${authOptions.map(([value, label]) => `<option value="${escapeHtml(value)}" ${value === auth ? 'selected' : ''}>${escapeHtml(label)}</option>`).join('')}
      </select>
    </div>
    ${isPassword(connection) ? passwordCol(state, t, connection) : keyPathCol(t, connection)}
    ${inputCol(t('edit.connectTimeout'), 'connectTimeout', connection.connectTimeout, 'number')}
    ${inputCol(t('edit.commandTimeout'), 'commandTimeout', connection.commandTimeout, 'number')}
  `;
}

function keyPathCol(t, connection) {
  return `
    <div class="col-12 col-md-6">
      <label class="form-label fw-bold">${escapeHtml(t('edit.keyPath'))}</label>
      <input class="form-control" name="credential" type="text" value="${escapeHtml(connection.credential)}" autocapitalize="none" autocorrect="off" autocomplete="off" spellcheck="false">
      <div class="form-text">${escapeHtml(t('edit.keyPathHelp'))}</div>
    </div>
  `;
}

// Passwords are write-only: the backend never returns them, so the field
// starts empty and a blank value on save keeps the stored password. "Clear"
// only empties what was typed here (back to "keep the stored one").
function passwordCol(state, t, connection) {
  const saved = savedById(state, connection.id);
  const isSet = Boolean(saved?.credentialSet && isPassword(saved));
  return `
    <div class="col-12 col-md-6">
      <label class="form-label fw-bold d-flex align-items-center gap-2">
        ${escapeHtml(t('edit.password'))}
        <span class="badge ${isSet ? 'text-bg-success' : 'text-bg-secondary'}">${isSet ? '✓ ' : ''}${escapeHtml(isSet ? t('edit.passwordSet') : t('edit.passwordUnset'))}</span>
      </label>
      <div class="input-group">
        <input class="form-control" name="credential" type="password" value="${escapeHtml(connection.credential || '')}" placeholder="${escapeHtml(isSet ? '••••••' : '')}" autocomplete="new-password" autocapitalize="none" autocorrect="off" spellcheck="false">
        <button class="btn btn-outline-secondary" type="button" data-action="edit-clear-password" aria-label="${escapeHtml(t('edit.passwordClearAria'))}" ${connection.credential ? '' : 'disabled'}>${escapeHtml(t('edit.passwordClear'))}</button>
      </div>
      <div class="form-text" id="editor-password-help">${escapeHtml(passwordHelp(t, connection, isSet))}</div>
    </div>
  `;
}

function passwordHelp(t, connection, isSet) {
  if (connection.credential) return t('edit.passwordReplace');
  return isSet ? t('edit.passwordKeep') : '';
}

function serialFields(state, t, connection) {
  const current = connection.serialPort || '';
  // Include the configured port even if it is unplugged / not scanned yet.
  const options = [...new Set([current, ...(state.editor.serialPorts ?? [])].filter(Boolean))];
  const baud = Number(connection.baudRate) || 115200;
  return `
    <div class="col-12 col-md-6">
      <div class="d-flex justify-content-between align-items-center">
        <label class="form-label fw-bold mb-0">${escapeHtml(t('edit.serialPort'))}</label>
        <button class="btn btn-link btn-sm p-0" type="button" data-action="edit-rescan">
          <i class="bi bi-arrow-clockwise me-1"></i>${escapeHtml(t('edit.rescan'))}
        </button>
      </div>
      <select class="form-select" name="serialPort">
        ${options.length
          ? options.map((port) => `<option value="${escapeHtml(port)}" ${port === current ? 'selected' : ''}>${escapeHtml(port)}</option>`).join('')
          : `<option value="">${escapeHtml(t('edit.noPorts'))}</option>`}
      </select>
    </div>
    <div class="col-12 col-md-6">
      <label class="form-label fw-bold">${escapeHtml(t('edit.baudRate'))}</label>
      <select class="form-select" name="baudRate">
        ${[...new Set([...BAUD_RATES, baud])].sort((x, y) => x - y).map((rate) => `<option value="${rate}" ${rate === baud ? 'selected' : ''}>${rate}</option>`).join('')}
      </select>
    </div>
  `;
}

// Updates the parts that depend on dirtiness without replacing the inputs, so
// the caret and IME composition survive typing.
function refreshChrome(ctx) {
  const { state, t } = ctx;
  const badges = document.querySelector('#editor-badges');
  if (badges) badges.innerHTML = renderBadges(state, ctx);
  const actions = document.querySelector('#editor-actions');
  if (actions) actions.innerHTML = renderActions(state, ctx);
  refreshList(ctx);
  const connection = currentConnection(state);
  const clear = document.querySelector('#connection-form [data-action="edit-clear-password"]');
  if (clear && connection) clear.disabled = !connection.credential;
  const help = document.querySelector('#editor-password-help');
  if (help && connection) {
    const saved = savedById(state, connection.id);
    help.textContent = passwordHelp(t, connection, Boolean(saved?.credentialSet && isPassword(saved)));
  }
}

// Exported for sessions:changed: connection badges and the disconnect button
// follow the live sessions without touching the inputs.
export function refreshEditorChrome(ctx) {
  refreshChrome(ctx);
}

// After an async action (test, save, disconnect): a full renderPage unless
// the user is typing in the same form meanwhile, in which case only the
// non-input parts are updated so focus, caret and IME composition survive.
function renderAfterAction(ctx) {
  const { state, t } = ctx;
  const form = document.querySelector('#connection-form');
  const connection = currentConnection(state);
  if (!editorHasFocus() || !form || !connection || form.dataset.connId !== connection.id) {
    ctx.renderPage();
    return;
  }
  refreshChrome(ctx);
  const set = (selector, html) => {
    const element = document.querySelector(selector);
    if (element) element.innerHTML = html;
  };
  set('#editor-alert', renderAlert(state));
  set('#editor-test-result', renderTestResult(state, ctx));
  set('#editor-last-test', renderLastTest(state, ctx));
  const title = document.querySelector('#editor-title');
  if (title) title.textContent = formTitle(state, t);
}

function refreshList(ctx) {
  const list = document.querySelector('#editor-list');
  if (list) list.innerHTML = renderList(ctx.state, ctx);
  const count = document.querySelector('#editor-list-count');
  if (count) count.textContent = ctx.t('edit.count', { n: visibleConnections(ctx.state).length });
}

// ------------------------------------------------------------------- modals

let modalChain = Promise.resolve();

function modalRoot() {
  let root = document.querySelector('#editor-modal-root');
  if (!root) {
    root = document.createElement('div');
    root.id = 'editor-modal-root';
    document.body.appendChild(root);
  }
  return root;
}

// Shows one modal in #editor-modal-root (outside #app, so page re-renders do
// not touch it). Modals are queued: a second request waits for the first to
// close. Resolves with { value, input } for a button / submit, or null for
// close / Escape / backdrop.
function showModal({ title, bodyHtml, buttons, input = null }) {
  const run = modalChain.then(() => new Promise((resolve) => {
    const root = modalRoot();
    const host = document.createElement('div');
    const previousFocus = document.activeElement;
    const inputHtml = input
      ? `
        <label class="form-label fw-bold" for="editor-modal-input">${escapeHtml(input.label)}</label>
        <input class="form-control" id="editor-modal-input" name="modalInput" type="${input.type || 'text'}" autocomplete="${input.autocomplete || 'off'}" autocapitalize="none" autocorrect="off" spellcheck="false">
        ${input.help ? `<div class="form-text">${escapeHtml(input.help)}</div>` : ''}
      `
      : '';
    host.innerHTML = `
      <div class="modal-backdrop show"></div>
      <div class="modal d-block" tabindex="-1" role="dialog" aria-modal="true" aria-labelledby="editor-modal-title" data-modal-dismiss="backdrop">
        <div class="modal-dialog modal-dialog-centered">
          <form class="modal-content" novalidate>
            <div class="modal-header">
              <h5 class="modal-title fw-bold" id="editor-modal-title">${escapeHtml(title)}</h5>
              <button class="btn-close" type="button" data-modal-dismiss="close" aria-label="${escapeHtml(input?.closeLabel || buttons.find((b) => b.dismiss)?.label || '')}"></button>
            </div>
            <div class="modal-body">${bodyHtml}${inputHtml}</div>
            <div class="modal-footer">
              ${buttons.map((button) => `<button class="btn ${button.className || 'btn-outline-secondary'}" type="${button.submit ? 'submit' : 'button'}" ${button.dismiss ? 'data-modal-dismiss="button"' : `data-modal-value="${escapeHtml(button.value)}"`}>${button.icon ? `<i class="bi ${button.icon} me-1"></i>` : ''}${escapeHtml(button.label)}</button>`).join('')}
            </div>
          </form>
        </div>
      </div>
    `;
    root.appendChild(host);
    let done = false;
    const finish = (result) => {
      if (done) return;
      done = true;
      host.remove();
      if (previousFocus && previousFocus.isConnected && typeof previousFocus.focus === 'function') previousFocus.focus();
      resolve(result);
    };
    const inputValue = () => host.querySelector('#editor-modal-input')?.value ?? '';
    host.addEventListener('click', (event) => {
      const dismiss = event.target.closest('[data-modal-dismiss]');
      if (dismiss && (dismiss.dataset.modalDismiss !== 'backdrop' || event.target === dismiss)) {
        finish(null);
        return;
      }
      const valued = event.target.closest('[data-modal-value]');
      if (valued && valued.type !== 'submit') finish({ value: valued.dataset.modalValue, input: inputValue() });
    });
    host.addEventListener('submit', (event) => {
      event.preventDefault();
      const submit = buttons.find((button) => button.submit);
      finish({ value: submit?.value ?? 'submit', input: inputValue() });
    });
    host.addEventListener('keydown', (event) => {
      if (event.key === 'Escape') {
        event.preventDefault();
        finish(null);
      }
    });
    const focusTarget = host.querySelector('#editor-modal-input')
      ?? host.querySelector('[data-modal-value]')
      ?? host.querySelector('[data-modal-dismiss="button"]');
    focusTarget?.focus();
  }));
  modalChain = run.catch(() => {});
  return run;
}

export async function promptPassphrase(conn, ctx) {
  const { t } = ctx;
  const result = await showModal({
    title: t('edit.passphrase.title'),
    bodyHtml: `<p class="text-secondary mb-3">${escapeHtml(t('edit.passphrase.body', { name: displayName(conn, t) }))}</p>`,
    input: {
      label: t('edit.passphrase.label'),
      type: 'password',
      autocomplete: 'current-password',
      help: t('edit.passphrase.help'),
      closeLabel: t('edit.close'),
    },
    buttons: [
      { dismiss: true, label: t('edit.cancel') },
      { value: 'submit', submit: true, label: t('edit.passphrase.submit'), className: 'btn-primary' },
    ],
  });
  return result ? result.input : null;
}

function confirmModal(ctx, { title, bodyHtml, okLabel, okClass = 'btn-danger', icon = '' }) {
  const { t } = ctx;
  return showModal({
    title,
    bodyHtml,
    buttons: [
      { dismiss: true, label: t('edit.cancel') },
      { value: 'ok', label: okLabel, className: okClass, icon },
    ],
  }).then((result) => result?.value === 'ok');
}

// ------------------------------------------------------------------ actions

async function saveDraft(ctx) {
  const { state } = ctx;
  const editor = state.editor;
  syncFormToDraft(state);
  const draft = editor.draft;
  if (!draft || !isEditorDirty(state)) return true;
  editor.busy = 'save';
  refreshChrome(ctx);
  ctx.bumpGen();
  let result;
  try {
    result = await SaveConnection(connectionPayload(draft));
  } catch (error) {
    result = { ok: false, message: String(error) };
  } finally {
    editor.busy = '';
  }
  if (result?.message) ctx.toast(result.message);
  if (!result?.ok) {
    editor.alert = result?.message || '';
    renderAfterAction(ctx);
    return false;
  }
  editor.selectedId = result.id || draft.id;
  editor.draft = null;
  editor.returnId = '';
  resetTransient(editor);
  await ctx.refresh({ silent: true });
  renderAfterAction(ctx);
  return true;
}

export async function confirmLeaveEditor(ctx) {
  const { state, t } = ctx;
  syncFormToDraft(state);
  if (!isEditorDirty(state)) {
    // An unchanged draft carries nothing worth keeping.
    if (state.editor.draft) state.editor.draft = null;
    return true;
  }
  const draft = state.editor.draft;
  const result = await showModal({
    title: t('edit.leave.title'),
    bodyHtml: `<p class="mb-0">${escapeHtml(t('edit.leave.body', { name: displayName(draft, t) }))}</p>`,
    buttons: [
      { dismiss: true, label: t('edit.cancel') },
      { value: 'save', label: t('edit.leave.save'), className: 'btn-primary' },
      { value: 'discard', label: t('edit.leave.discard'), className: 'btn-outline-danger' },
    ],
  });
  if (result?.value === 'discard') {
    discardDraft(state);
    return true;
  }
  if (result?.value === 'save') return saveDraft(ctx);
  return false;
}

export async function selectConnection(connId, ctx) {
  const { state } = ctx;
  const editor = state.editor;
  if (connId === editor.selectedId && !(connId === '' && !hasNewDraft(editor))) return true;
  if (!(await confirmLeaveEditor(ctx))) return false;
  // confirmLeaveEditor left either no draft or a non-dirty one; neither may
  // follow the selection to another row.
  if (editor.draft && editor.draft.id !== connId) editor.draft = null;
  editor.selectedId = connId;
  editor.returnId = '';
  resetTransient(editor);
  ctx.renderPage();
  return true;
}

async function startNewDraft(ctx) {
  const { state, t } = ctx;
  const editor = state.editor;
  if (hasNewDraft(editor)) {
    // Only one new draft: "New" just brings it back into view.
    if (editor.selectedId !== '') {
      editor.selectedId = '';
      ctx.renderPage();
    }
    return;
  }
  const returnId = editor.selectedId;
  if (!(await confirmLeaveEditor(ctx))) return;
  editor.draft = newDraft(t);
  editor.selectedId = '';
  editor.returnId = returnId;
  resetTransient(editor);
  ctx.renderPage();
}

async function cancelEdit(ctx) {
  const { state } = ctx;
  if (!isEditorDirty(state)) return;
  discardDraft(state);
  reconcileEditor(state);
  ctx.renderPage();
}

async function runConnectionTest(ctx) {
  const { state } = ctx;
  const editor = state.editor;
  const connection = ensureDraft(state) ?? currentConnection(state);
  if (!connection) return;
  const tested = connectionPayload(connection);
  const connId = connection.id;
  editor.busy = 'test';
  refreshChrome(ctx);
  let result;
  try {
    result = await TestConnectionConfig(tested, '');
    if (!result?.ok && result?.code === 'needs_passphrase') {
      editor.busy = '';
      refreshChrome(ctx);
      const passphrase = await promptPassphrase(tested, ctx);
      if (!passphrase) return;
      editor.busy = 'test';
      refreshChrome(ctx);
      result = await TestConnectionConfig(tested, passphrase);
    }
  } catch (error) {
    result = { ok: false, message: String(error) };
  } finally {
    editor.busy = '';
  }
  // Only the test panel changes; connState is the backend's business.
  editor.test = { ok: Boolean(result?.ok), message: result?.message || '', target: testTarget(tested), connId };
  if (result?.message) ctx.toast(result.message);
  // The backend records lastTest when the tested values equal the saved ones.
  await ctx.refresh({ silent: true });
  renderAfterAction(ctx);
}

async function openTerminal(ctx) {
  const { state, t } = ctx;
  const connection = currentConnection(state);
  if (!connection?.id || currentSaveState(state) !== 'saved') {
    ctx.toast(t('edit.saveFirst'));
    return;
  }
  await ctx.openTerminal(connection.id);
}

async function disconnect(ctx) {
  const { state, t } = ctx;
  const connection = currentConnection(state);
  if (!connection?.id) return;
  const session = ctx.sessionFor?.(connection.id);
  if (!DISCONNECTABLE.includes(connStateFor(ctx, connection.id))) return;
  const k = (session?.shells ?? 0) + (session?.connecting ?? 0);
  const j = session?.running ?? 0;
  const name = savedById(state, connection.id)?.name ?? connection.name;
  const ok = await confirmModal(ctx, {
    title: t('edit.disconnect.title'),
    bodyHtml: `<p class="mb-2">${escapeHtml(t('edit.disconnect.body', { name }))}</p><p class="text-secondary mb-0">${escapeHtml(t('edit.disconnectConfirm', { k, j }))}</p>`,
    okLabel: t('edit.disconnect'),
    icon: 'bi-plug',
  });
  if (!ok) return;
  ctx.bumpGen();
  let result;
  try {
    result = await DisconnectConnection(connection.id);
  } catch (error) {
    result = { ok: false, message: String(error) };
  }
  if (result?.message) ctx.toast(result.message);
  // Open tabs also leave on terminal:exit(user); this catches a tab that was
  // reconnecting (StartTerminal just returns canceled) and exited tabs.
  if (result?.ok) ctx.closeTabsForConnection?.(connection.id);
  await ctx.refresh({ silent: true });
  renderAfterAction(ctx);
}

async function deleteConnection(ctx, connId) {
  const { state, t } = ctx;
  const editor = state.editor;
  if (!connId) {
    // An unsaved new draft is simply discarded.
    if (hasNewDraft(editor)) {
      discardDraft(state);
      reconcileEditor(state);
      ctx.renderPage();
    }
    return;
  }
  const saved = savedById(state, connId);
  if (!saved) return;
  const session = ctx.sessionFor?.(connId);
  const tabs = (session?.shells ?? 0) + (session?.connecting ?? 0);
  const ok = await confirmModal(ctx, {
    title: t('edit.delete.title'),
    bodyHtml: `
      <p class="mb-2"><strong>${escapeHtml(saved.name)}</strong> ${escapeHtml(t('edit.delete.body'))}</p>
      ${tabs > 0 ? `<p class="text-danger mb-2">${escapeHtml(t('edit.deleteWithTabs', { k: tabs }))}</p>` : ''}
      <p class="text-secondary mb-0">${escapeHtml(t('edit.delete.history'))}</p>
    `,
    okLabel: t('edit.delete'),
    icon: 'bi-trash',
  });
  if (!ok) return;
  ctx.bumpGen();
  let result;
  try {
    result = await DeleteConnection(connId);
  } catch (error) {
    result = { ok: false, message: String(error) };
  }
  if (result?.ok) {
    ctx.closeTabsForConnection?.(connId);
    if (editor.draft?.id === connId) editor.draft = null;
    if (editor.selectedId === connId) {
      editor.selectedId = '';
      resetTransient(editor);
    }
    if (editor.returnId === connId) editor.returnId = '';
  }
  if (result?.message) ctx.toast(result.message);
  await ctx.refresh({ silent: true });
  reconcileEditor(state);
  ctx.renderPage();
}

async function rescanSerialPorts(ctx) {
  const { state } = ctx;
  ensureDraft(state);
  await loadSerialPorts(state);
  ctx.renderPage();
}

async function loadSerialPorts(state) {
  try {
    state.editor.serialPorts = (await ListSerialPorts()) ?? [];
  } catch {
    state.editor.serialPorts = [];
  }
}

async function copyTestResult(ctx) {
  const value = ctx.state.editor.test.message;
  if (!value) return;
  try {
    await navigator.clipboard.writeText(value);
  } catch {
    const textarea = document.createElement('textarea');
    textarea.value = value;
    textarea.setAttribute('readonly', '');
    textarea.style.position = 'fixed';
    textarea.style.opacity = '0';
    document.body.appendChild(textarea);
    textarea.select();
    document.execCommand('copy');
    textarea.remove();
  }
  ctx.toast(ctx.t('edit.copied'));
}

// ----------------------------------------------------------------- handlers

export async function handleEditorClick(event, ctx) {
  const element = event.target.closest?.('[data-action^="edit-"]');
  if (!element) return false;
  const action = element.dataset.action;
  // Inputs, selects and the submit button are handled by input/change/submit.
  if (action === 'edit-search' || action === 'edit-type' || action === 'edit-auth' || action === 'edit-save') return false;
  if (element.disabled) return true;
  switch (action) {
    case 'edit-new':
      await startNewDraft(ctx);
      return true;
    case 'edit-select':
      await selectConnection(element.dataset.connId ?? '', ctx);
      return true;
    case 'edit-select-new':
      if (ctx.state.editor.selectedId !== '') {
        // Unreachable in practice (leaving a new draft discards it), but keep
        // the row clickable if a draft somehow survives.
        ctx.state.editor.selectedId = '';
        ctx.renderPage();
      }
      return true;
    case 'edit-cancel':
      await cancelEdit(ctx);
      return true;
    case 'edit-test':
      await runConnectionTest(ctx);
      return true;
    case 'edit-open-terminal':
      await openTerminal(ctx);
      return true;
    case 'edit-disconnect':
      await disconnect(ctx);
      return true;
    case 'edit-delete':
      await deleteConnection(ctx, element.dataset.connId ?? '');
      return true;
    case 'edit-rescan':
      await rescanSerialPorts(ctx);
      return true;
    case 'edit-clear-password': {
      const draft = ensureDraft(ctx.state);
      if (draft) draft.credential = '';
      const input = document.querySelector('#connection-form input[name="credential"]');
      if (input) {
        input.value = '';
        input.focus();
      }
      refreshChrome(ctx);
      return true;
    }
    case 'edit-copy-test':
      await copyTestResult(ctx);
      return true;
    default:
      return false;
  }
}

export function handleEditorInput(event, ctx) {
  const target = event.target;
  if (target.closest?.('[data-action="edit-search"]')) {
    ctx.state.editor.filter = target.value;
    refreshList(ctx);
    return true;
  }
  if (!target.closest?.('#connection-form')) return false;
  if (syncFormToDraft(ctx.state)) refreshChrome(ctx);
  return true;
}

export async function handleEditorChange(event, ctx) {
  const target = event.target;
  if (target.closest?.('[data-action="edit-search"]')) return true;
  if (!target.closest?.('#connection-form')) return false;
  const { state } = ctx;
  const draft = syncFormToDraft(state);
  if (!draft) return true;
  if (target.name === 'authMethod') {
    const toPassword = isPassword(draft);
    const looksLikePath = /^(~|\/|[A-Za-z]:\\|%)/.test(draft.credential);
    const editor = state.editor;
    // Never send a key path as a password (or a password as a key path).
    if (toPassword && looksLikePath) {
      // Remembered so toggling back to key auth does not lose the path.
      editor.keyPathStash = { id: draft.id, value: draft.credential };
      draft.credential = '';
    } else if (!toPassword && !looksLikePath) {
      draft.credential = '';
    }
    if (!toPassword && !draft.credential) {
      const saved = savedById(state, draft.id);
      if (editor.keyPathStash?.id === draft.id) draft.credential = editor.keyPathStash.value;
      else if (saved && !isPassword(saved)) draft.credential = saved.credential ?? '';
    }
    ctx.renderPage();
    return true;
  }
  if (target.name === 'type') {
    if (draft.type === 'Serial' && (state.editor.serialPorts ?? []).length === 0) await loadSerialPorts(state);
    ctx.renderPage();
    return true;
  }
  refreshChrome(ctx);
  return true;
}

export async function handleEditorSubmit(event, ctx) {
  if (event.target?.id !== 'connection-form') return false;
  event.preventDefault();
  if (ctx.state.editor.busy) return true;
  await saveDraft(ctx);
  return true;
}
