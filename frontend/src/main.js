import 'admin-lte/dist/css/adminlte.min.css';
import 'admin-lte/dist/js/adminlte.min.js';
import 'bootstrap-icons/font/bootstrap-icons.css';
import '@xterm/xterm/css/xterm.css';
import './style.css';

import { Terminal } from '@xterm/xterm';
import { FitAddon } from '@xterm/addon-fit';
import logoUrl from './assets/ssh-gate-logo.png';

import {
  ApproveCommand,
  ApproveCommandAndBypassAgent,
  CloseTerminal,
  DeleteConnection,
  DeleteCommandRequest,
  DeleteMCPSettings,
  DisconnectConnection,
  GetInitialData,
  ListSerialPorts,
  RejectCommand,
  ResizeTerminal,
  SaveConnection,
  SaveMCPSettings,
  SendTerminalInput,
  StartTerminal,
  TestConnectionConfig,
} from '../wailsjs/go/main/App';
import { EventsOn } from '../wailsjs/runtime/runtime';

document.body.className = 'layout-fixed sidebar-expand-lg bg-body-tertiary';

const app = document.querySelector('#app');

const I18N = {
  ja: {
    'sidebar.toggle': 'サイドバー切替',
    'nav.connections': '接続先管理',
    'nav.commands': 'コマンド履歴・承認',
    'nav.terminal': 'ターミナル',
    'nav.settings': 'MCP待受設定',
    'title.settings': 'MCPサーバー待受設定',
    'breadcrumb.root': 'ssh-gete',
    'status.heading': 'ステータス',
    'status.mcpServer': 'MCPサーバー',
    'status.sshSession': 'SSHセッション',
    'status.currentConnection': '現在の接続先',
    'status.listenPort': '待受ポート',
    'status.connectedAgents': '被接続エージェント数',
    'lang.toggleLabel': '言語',

    'ssh.connected': '接続中',
    'ssh.disconnected': '切断中',
    'mcp.status.running': '起動中',
    'mcp.status.error': 'エラー',
    'mcp.status.stopped': '停止中',

    'common.new': '新規',
    'common.save': '保存',
    'common.cancel': 'キャンセル',
    'common.delete': '削除',
    'common.copy': 'コピー',
    'common.close': '閉じる',
    'common.copied': 'コピーしました',
    'common.search': '検索...',

    'conn.listTitle': '接続先一覧',
    'conn.searchAria': '接続先を検索',
    'conn.empty': '接続先がありません',
    'conn.countSuffix': '件を表示',
    'conn.editTitle': '接続先編集',
    'conn.editSubtitle': 'SSH / シリアル接続先を登録します。',
    'conn.reachability': '疎通',
    'conn.name': '接続先名',
    'conn.tags': '許可タグ',
    'conn.description': '説明',
    'conn.calloutSerial': 'シリアル接続先です。ターミナルメニューで選択するとシリアルコンソールが開きます。',
    'conn.calloutSSH': 'MCP経由のコマンドは読み取り系を含めて承認キューへ送信します。',
    'conn.connect': '接続',
    'conn.disconnect': '切断',
    'conn.deleteAria': 'を削除',
    'conn.type': '接続種類',
    'conn.typeSerial': 'Serial（シリアル）',
    'conn.host': 'ホスト名 / IPアドレス',
    'conn.port': 'ポート',
    'conn.user': 'ユーザー名',
    'conn.authMethod': '認証方式',
    'conn.authKey': 'SSH鍵認証',
    'conn.authPassword': 'パスワード保管',
    'conn.credential': '鍵ファイルパス / パスワード',
    'conn.connectTimeout': '接続タイムアウト（秒）',
    'conn.commandTimeout': 'コマンド実行タイムアウト（秒）',
    'conn.serialPort': 'シリアルポート',
    'conn.rescan': '再スキャン',
    'conn.noPorts': 'ポートが見つかりません',
    'conn.baudRate': 'ボーレート',
    'conn.newName': '新規接続先',
    'conn.unsaved': '未保存の接続先',
    'conn.notSelected': '接続先が選択されていません',
    'conn.disconnected': '切断しました',

    'passphrase.title': 'SSH鍵パスフレーズ',
    'passphrase.body1': 'の秘密鍵がパスフレーズで保護されています。',
    'passphrase.label': 'パスフレーズ',
    'passphrase.help': 'SQLiteには保存せず、このアプリ起動中のSSH実行だけに使います。',
    'passphrase.submit': '接続テスト実行',

    'delConn.title': '接続先を削除',
    'delConn.body1pre': 'を接続先一覧から削除します。',
    'delConn.body2': 'コマンド履歴は監査用に残します。',
    'delCmd.title': 'コマンド履歴を削除',
    'delCmd.body1pre': 'をコマンド履歴から削除します。',
    'delCmd.body2': '承認待ちの場合もキューから消えます。',

    'test.lastResult': '直近の接続テスト結果',

    'cmd.queueTitle': '承認キュー',
    'cmd.queueWaiting': '件待機',
    'cmd.queueEmpty': 'MCPからの承認待ちコマンドはありません',
    'cmd.historyTitle': 'コマンド履歴',
    'cmd.historyEmpty': '履歴はまだありません',
    'cmd.unknownAgent': '未申告エージェント',
    'cmd.status.pending': '承認待ち',
    'cmd.approveTitle': '実行承認',
    'cmd.approveSubtitle': 'MCPクライアントが要求したコマンド、理由、出力履歴を確認します。',
    'cmd.detailTitle': 'コマンド詳細',
    'cmd.detailSubtitle': '実行したコマンドの内容と出力（stdout / stderr）を確認します。',
    'cmd.stdoutEmpty': '出力なし',
    'cmd.agent': 'エージェント',
    'cmd.bypassing': '承認省略中',
    'cmd.plannedCommand': '実行予定コマンド',
    'cmd.reason': '実行理由',
    'cmd.reasonEmpty': '理由未入力',
    'cmd.stdoutPlaceholder': '承認後に実行結果を表示',
    'cmd.stderrPlaceholder': 'エラーなし',
    'cmd.deleteHistory': '履歴削除',
    'cmd.reject': '拒否',
    'cmd.approveBypass': 'エージェントは承認省略',
    'cmd.approve': '承認して実行',
    'cmd.emptyTitle': '承認対象はありません',
    'cmd.emptyDesc': 'MCP経由で危険または承認必須のコマンドが要求されると、ここに実行予定コマンドと理由が表示されます。',

    'hist.time': '時刻',
    'hist.agent': 'エージェント',
    'hist.status': '状態',
    'hist.command': 'コマンド',
    'hist.reason': '理由',
    'hist.action': '操作',

    'term.headingSerial': 'シリアルコンソール',
    'term.headingSSH': 'インタラクティブシェル',
    'term.connectSerial': 'シリアル接続',
    'term.connectSSH': 'SSHシェルに接続',
    'term.target': '接続先',
    'term.selectLabel': '接続先（種別は接続先で設定）',
    'term.unnamed': '無名',
    'term.noConnections': '接続先がありません',
    'term.notRegistered': '接続先が登録されていません',
    'term.disconnect': '切断',
    'term.connectedTo': '接続中',
    'term.disconnected': '未接続',
    'term.detail.type': '種別',
    'term.detail.port': 'ポート',
    'term.detail.baudRate': 'ボーレート',
    'term.detail.tags': 'タグ',
    'term.detail.description': '説明',
    'term.detail.host': 'ホスト',
    'term.detail.user': 'ユーザー',
    'term.detail.authMethod': '認証方式',
    'term.detail.state': '状態',

    'mcp.title': 'MCPサーバー待受設定',
    'mcp.subtitle': 'ローカルエージェントがssh-geteへ接続するためのHTTP MCP待受を管理します。',
    'mcp.enable': 'MCP待受を有効化',
    'mcp.bearer': 'Bearer認証',
    'mcp.listenAddress': '待受アドレス',
    'mcp.port': '待受ポート',
    'mcp.transport': 'Transport',
    'mcp.baseURL': 'Base URL',
    'mcp.mcpPath': 'MCPパス',
    'mcp.healthPath': 'Healthパス',
    'mcp.auditPath': 'Auditパス',
    'mcp.tokenName': 'トークン名',
    'mcp.tokenInput': 'Bearer Token（変更時のみ入力）',
    'mcp.tokenPreview': '保存済みトークン',
    'mcp.allowedOrigins': '許可Origin（カンマ区切り）',
    'mcp.tlsMode': 'TLS / HTTPS',
    'mcp.tls.local': 'ローカル平文',
    'mcp.tls.builtin': '内蔵TLS（未実装）',
    'mcp.tls.proxy': 'リバースプロキシ終端',
    'mcp.proxyMode': 'Proxy Mode',
    'mcp.proxy.disabled': '無効',
    'mcp.maxBodyKB': '最大リクエストサイズ（KB）',
    'mcp.maxOutputKB': '最大出力サイズ（KB）',
    'mcp.requestTimeout': 'MCPリクエストタイムアウト（秒）',
    'mcp.defaultConnectTimeout': '既定接続タイムアウト（秒）',
    'mcp.defaultCommandTimeout': '既定コマンドタイムアウト（秒）',
    'mcp.strictHostKey': 'known_hosts厳格検証',
    'mcp.requireApprovalSudo': 'sudoは承認必須',
    'mcp.requireApprovalProd': 'prodタグは承認必須',
    'mcp.requireApprovalWriteOp': '書き込み系は承認必須',
    'mcp.configPath': 'SQLite保存先',
    'mcp.callout': 'チェック変更、ポート変更、Bearer変更は「保存して待受開始/再起動」を押した時点で反映します。',
    'mcp.refresh': '状態更新',
    'mcp.deleteSettings': '設定削除',
    'mcp.saveStart': '保存して待受開始/再起動',
    'mcp.endpointTitle': '公開エンドポイント',
    'mcp.bearerRequired': 'Bearer必須',
    'mcp.noAuth': '認証なし',
    'mcp.connectURL': '接続URL',
    'mcp.usageTitle': 'エージェント用usage',
    'mcp.usage.listHosts': '登録済みSSH接続先を確認',
    'mcp.usage.executeCommand': '承認キューへ登録（自動実行なし）',
    'mcp.usage.requestExec': '理由付きで承認キューへ登録',
    'mcp.usage.getHistory': '承認状態と実行結果を取得',
    'mcp.refreshed': 'MCP状態を更新しました',
    'mcp.deleteConfirm': 'MCPサーバー待受設定を削除し、待受を停止します。よろしいですか？',

    'fatal.init': 'Wailsバックエンドの初期化に失敗しました。',
  },
  en: {
    'sidebar.toggle': 'Toggle sidebar',
    'nav.connections': 'Connections',
    'nav.commands': 'Command History / Approval',
    'nav.terminal': 'Terminal',
    'nav.settings': 'MCP Listener',
    'title.settings': 'MCP Server Listener Settings',
    'breadcrumb.root': 'ssh-gete',
    'status.heading': 'Status',
    'status.mcpServer': 'MCP Server',
    'status.sshSession': 'SSH Session',
    'status.currentConnection': 'Current Connection',
    'status.listenPort': 'Listen Port',
    'status.connectedAgents': 'Connected Agents',
    'lang.toggleLabel': 'Language',

    'ssh.connected': 'Connected',
    'ssh.disconnected': 'Disconnected',
    'mcp.status.running': 'Running',
    'mcp.status.error': 'Error',
    'mcp.status.stopped': 'Stopped',

    'common.new': 'New',
    'common.save': 'Save',
    'common.cancel': 'Cancel',
    'common.delete': 'Delete',
    'common.copy': 'Copy',
    'common.close': 'Close',
    'common.copied': 'Copied',
    'common.search': 'Search...',

    'conn.listTitle': 'Connection List',
    'conn.searchAria': 'Search connections',
    'conn.empty': 'No connections',
    'conn.countSuffix': 'shown',
    'conn.editTitle': 'Edit Connection',
    'conn.editSubtitle': 'Register an SSH / serial connection.',
    'conn.reachability': 'Reachability',
    'conn.name': 'Connection Name',
    'conn.tags': 'Allowed Tags',
    'conn.description': 'Description',
    'conn.calloutSerial': 'This is a serial connection. Selecting it from the Terminal menu opens a serial console.',
    'conn.calloutSSH': 'Commands via MCP, including read-only ones, are sent to the approval queue.',
    'conn.connect': 'Connect',
    'conn.disconnect': 'Disconnect',
    'conn.deleteAria': 'Delete',
    'conn.type': 'Connection Type',
    'conn.typeSerial': 'Serial',
    'conn.host': 'Hostname / IP Address',
    'conn.port': 'Port',
    'conn.user': 'Username',
    'conn.authMethod': 'Authentication',
    'conn.authKey': 'SSH Key',
    'conn.authPassword': 'Stored Password',
    'conn.credential': 'Key File Path / Password',
    'conn.connectTimeout': 'Connect Timeout (sec)',
    'conn.commandTimeout': 'Command Timeout (sec)',
    'conn.serialPort': 'Serial Port',
    'conn.rescan': 'Rescan',
    'conn.noPorts': 'No ports found',
    'conn.baudRate': 'Baud Rate',
    'conn.newName': 'New Connection',
    'conn.unsaved': 'Unsaved connection',
    'conn.notSelected': 'No connection selected',
    'conn.disconnected': 'Disconnected',

    'passphrase.title': 'SSH Key Passphrase',
    'passphrase.body1': "'s private key is protected by a passphrase.",
    'passphrase.label': 'Passphrase',
    'passphrase.help': 'Not stored in SQLite; used only for SSH while this app is running.',
    'passphrase.submit': 'Run Connection Test',

    'delConn.title': 'Delete Connection',
    'delConn.body1pre': 'will be removed from the connection list.',
    'delConn.body2': 'Command history is kept for auditing.',
    'delCmd.title': 'Delete Command History',
    'delCmd.body1pre': 'will be removed from the command history.',
    'delCmd.body2': 'Pending requests are also removed from the queue.',

    'test.lastResult': 'Latest connection test result',

    'cmd.queueTitle': 'Approval Queue',
    'cmd.queueWaiting': 'waiting',
    'cmd.queueEmpty': 'No commands awaiting approval from MCP',
    'cmd.historyTitle': 'Command History',
    'cmd.historyEmpty': 'No history yet',
    'cmd.unknownAgent': 'Unidentified agent',
    'cmd.status.pending': '承認待ち',
    'cmd.approveTitle': 'Execution Approval',
    'cmd.approveSubtitle': 'Review the command, reason, and output history requested by the MCP client.',
    'cmd.detailTitle': 'Command Detail',
    'cmd.detailSubtitle': 'Review the executed command and its output (stdout / stderr).',
    'cmd.stdoutEmpty': 'No output',
    'cmd.agent': 'Agent',
    'cmd.bypassing': 'Approval bypassed',
    'cmd.plannedCommand': 'Command to Execute',
    'cmd.reason': 'Reason',
    'cmd.reasonEmpty': 'No reason provided',
    'cmd.stdoutPlaceholder': 'Output shown after approval',
    'cmd.stderrPlaceholder': 'No errors',
    'cmd.deleteHistory': 'Delete History',
    'cmd.reject': 'Reject',
    'cmd.approveBypass': 'Bypass approval for agent',
    'cmd.approve': 'Approve & Execute',
    'cmd.emptyTitle': 'Nothing to approve',
    'cmd.emptyDesc': 'When a dangerous or approval-required command is requested via MCP, the command and its reason appear here.',

    'hist.time': 'Time',
    'hist.agent': 'Agent',
    'hist.status': 'Status',
    'hist.command': 'Command',
    'hist.reason': 'Reason',
    'hist.action': 'Action',

    'term.headingSerial': 'Serial Console',
    'term.headingSSH': 'Interactive Shell',
    'term.connectSerial': 'Connect Serial',
    'term.connectSSH': 'Connect SSH Shell',
    'term.target': 'Connection',
    'term.selectLabel': 'Connection (type is set per connection)',
    'term.unnamed': 'Unnamed',
    'term.noConnections': 'No connections',
    'term.notRegistered': 'No connections registered',
    'term.disconnect': 'Disconnect',
    'term.connectedTo': 'Connected',
    'term.disconnected': 'Disconnected',
    'term.detail.type': 'Type',
    'term.detail.port': 'Port',
    'term.detail.baudRate': 'Baud Rate',
    'term.detail.tags': 'Tags',
    'term.detail.description': 'Description',
    'term.detail.host': 'Host',
    'term.detail.user': 'User',
    'term.detail.authMethod': 'Authentication',
    'term.detail.state': 'State',

    'mcp.title': 'MCP Server Listener Settings',
    'mcp.subtitle': 'Manage the HTTP MCP listener that local agents use to connect to ssh-gete.',
    'mcp.enable': 'Enable MCP Listener',
    'mcp.bearer': 'Bearer Auth',
    'mcp.listenAddress': 'Listen Address',
    'mcp.port': 'Listen Port',
    'mcp.transport': 'Transport',
    'mcp.baseURL': 'Base URL',
    'mcp.mcpPath': 'MCP Path',
    'mcp.healthPath': 'Health Path',
    'mcp.auditPath': 'Audit Path',
    'mcp.tokenName': 'Token Name',
    'mcp.tokenInput': 'Bearer Token (enter only when changing)',
    'mcp.tokenPreview': 'Saved Token',
    'mcp.allowedOrigins': 'Allowed Origins (comma-separated)',
    'mcp.tlsMode': 'TLS / HTTPS',
    'mcp.tls.local': 'Local plaintext',
    'mcp.tls.builtin': 'Built-in TLS (not implemented)',
    'mcp.tls.proxy': 'Reverse proxy termination',
    'mcp.proxyMode': 'Proxy Mode',
    'mcp.proxy.disabled': 'Disabled',
    'mcp.maxBodyKB': 'Max Request Size (KB)',
    'mcp.maxOutputKB': 'Max Output Size (KB)',
    'mcp.requestTimeout': 'MCP Request Timeout (sec)',
    'mcp.defaultConnectTimeout': 'Default Connect Timeout (sec)',
    'mcp.defaultCommandTimeout': 'Default Command Timeout (sec)',
    'mcp.strictHostKey': 'Strict known_hosts verification',
    'mcp.requireApprovalSudo': 'sudo requires approval',
    'mcp.requireApprovalProd': 'prod tag requires approval',
    'mcp.requireApprovalWriteOp': 'write ops require approval',
    'mcp.configPath': 'SQLite Location',
    'mcp.callout': 'Checkbox, port, and Bearer changes take effect when you press "Save & Start/Restart Listener".',
    'mcp.refresh': 'Refresh Status',
    'mcp.deleteSettings': 'Delete Settings',
    'mcp.saveStart': 'Save & Start/Restart Listener',
    'mcp.endpointTitle': 'Public Endpoints',
    'mcp.bearerRequired': 'Bearer required',
    'mcp.noAuth': 'No auth',
    'mcp.connectURL': 'Connection URL',
    'mcp.usageTitle': 'Agent Usage',
    'mcp.usage.listHosts': 'List registered SSH connections',
    'mcp.usage.executeCommand': 'Add to approval queue (no auto-execute)',
    'mcp.usage.requestExec': 'Add to approval queue with a reason',
    'mcp.usage.getHistory': 'Get approval status and execution results',
    'mcp.refreshed': 'MCP status refreshed',
    'mcp.deleteConfirm': 'This deletes the MCP server listener settings and stops the listener. Continue?',

    'fatal.init': 'Failed to initialize the Wails backend.',
  },
};

function detectLang() {
  try {
    const saved = localStorage.getItem('sshgate-lang');
    if (saved === 'ja' || saved === 'en') return saved;
  } catch {
    // localStorage unavailable; fall through to navigator.
  }
  return String(navigator.language || '').toLowerCase().startsWith('en') ? 'en' : 'ja';
}

function t(key) {
  return I18N[state.lang]?.[key] ?? I18N.ja[key] ?? key;
}

function setLang(lang) {
  state.lang = lang;
  try {
    localStorage.setItem('sshgate-lang', lang);
  } catch {
    // Persisting language is best-effort.
  }
  render();
}

const state = {
  lang: detectLang(),
  active: 'connections',
  selectedConnection: 0,
  selectedRequestId: '',
  data: {
    connections: [],
    requests: [],
    agentPolicies: [],
    mcp: {},
  },
  dirty: {
    connections: false,
    settings: false,
  },
  passphrase: {
    open: false,
    connectionName: '',
    value: '',
    connection: null,
  },
  deleteConnection: {
    open: false,
    connectionName: '',
  },
  deleteCommand: {
    open: false,
    requestId: '',
  },
  connectionTest: {
    ok: null,
    message: '',
    target: '',
  },
  terminal: {
    connected: false,
    label: '',
  },
  serialPorts: [],
  toast: '',
};

const BAUD_RATES = [9600, 19200, 38400, 57600, 115200, 230400, 460800, 921600];

// xterm lives outside the render() innerHTML cycle: the instance is kept here and
// its DOM element is re-attached after each render so the live session survives.
let term = null;
let fitAddon = null;
let termWired = false;

function pageTitle(active) {
  if (active === 'connections') return t('nav.connections');
  if (active === 'commands') return t('nav.commands');
  if (active === 'terminal') return t('nav.terminal');
  return t('title.settings');
}

function escapeHtml(value) {
  return String(value ?? '')
    .replaceAll('&', '&amp;')
    .replaceAll('<', '&lt;')
    .replaceAll('>', '&gt;')
    .replaceAll('"', '&quot;')
    .replaceAll("'", '&#039;');
}

function field(object, key, fallback = '') {
  return object?.[key] ?? fallback;
}

function boolAttr(value) {
  return value ? 'checked' : '';
}

function mcpStatusClass(status) {
  if (status === '起動中') return 'text-bg-success';
  if (status === 'エラー') return 'text-bg-danger';
  return 'text-bg-secondary';
}

function mcpStatusLabel(status) {
  if (status === '起動中') return t('mcp.status.running');
  if (status === 'エラー') return t('mcp.status.error');
  return t('mcp.status.stopped');
}

function mcpStatusBadge(status, className = '') {
  const value = status || '停止中';
  return `<span class="badge ${mcpStatusClass(value)} ${className}">${escapeHtml(mcpStatusLabel(value))}</span>`;
}

function sshStatusClass(status) {
  if (status === 'Online') return 'text-bg-success';
  if (status === 'Error') return 'text-bg-danger';
  return 'text-bg-secondary';
}

function sshStatusLabel(status) {
  if (status === 'Online') return t('ssh.connected');
  return t('ssh.disconnected');
}

// Request status is a backend-provided string. Only the pending value has an
// EN translation; other statuses (executed, rejected, ...) pass through as-is.
function requestStatusLabel(status) {
  if (status === '承認待ち') return t('cmd.status.pending');
  return status;
}

function sshStatusBadge(status, className = '') {
  return `<span class="badge ${sshStatusClass(status)} ${className}">${escapeHtml(sshStatusLabel(status))}</span>`;
}

function mcpEndpoint(mcp) {
  const base = String(mcp.baseURL || `http://${mcp.listenAddress || '127.0.0.1'}:${mcp.port || 8787}`).replace(/\/+$/, '');
  const path = String(mcp.mcpPath || '/mcp');
  return `${base}${path.startsWith('/') ? path : `/${path}`}`;
}

function agentConfig(mcp) {
  const server = { url: mcpEndpoint(mcp) };
  if (mcp.bearerEnabled) {
    server.headers = { Authorization: 'Bearer ${SSH_GETE_TOKEN}' };
  }
  return JSON.stringify({ mcpServers: { 'ssh-gete': server } }, null, 2);
}

async function copyText(text) {
  const value = String(text ?? '');
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
  showToast(t('common.copied'));
}

function showToast(message) {
  state.toast = message;
  render();
  window.clearTimeout(showToast.timer);
  showToast.timer = window.setTimeout(() => {
    state.toast = '';
    render();
  }, 3200);
}

async function refreshData({ silent = false } = {}) {
  try {
    const next = await GetInitialData();
    const protectConnections = state.active === 'connections' && state.dirty.connections;
    const protectSettings = state.active === 'settings' && state.dirty.settings;
    const protectModal = state.passphrase.open || state.deleteConnection.open || state.deleteCommand.open;
    // The live xterm must not be torn down by the periodic poll.
    const protectTerminal = state.active === 'terminal';
    state.data = {
      connections: protectConnections ? state.data.connections : next.connections,
      requests: next.requests,
      agentPolicies: next.agentPolicies ?? [],
      mcp: protectSettings ? state.data.mcp : next.mcp,
    };
    if (state.selectedConnection >= state.data.connections.length) state.selectedConnection = 0;
    if (!protectConnections && !protectSettings && !protectModal && !protectTerminal) render();
  } catch (error) {
    if (!silent) {
      app.innerHTML = `<div class="fatal-error">${escapeHtml(t('fatal.init'))}<pre>${escapeHtml(error)}</pre></div>`;
    }
  }
}

function shell(content) {
  const waitCount = state.data.requests.filter((request) => request.status === '承認待ち').length;
  const mcp = state.data.mcp ?? {};
  const currentConnection = state.data.connections[state.selectedConnection] ?? state.data.connections[0] ?? {};
  return `
    <div class="app-wrapper">
      <nav class="app-header navbar navbar-expand bg-body">
        <div class="container-fluid">
          <ul class="navbar-nav">
            <li class="nav-item">
              <button class="nav-link btn btn-link" data-lte-toggle="sidebar" type="button" aria-label="${escapeHtml(t('sidebar.toggle'))}">
                <i class="bi bi-list"></i>
              </button>
            </li>
            <li class="nav-item d-none d-md-block">
              <span class="nav-link fw-bold">${pageTitle(state.active)}</span>
            </li>
          </ul>
          <ul class="navbar-nav ms-auto">
            <li class="nav-item">
              <span class="nav-link">
                <i class="bi bi-person-circle me-1"></i>
                admin
              </span>
            </li>
          </ul>
        </div>
      </nav>

      <aside class="app-sidebar bg-primary-subtle shadow" data-bs-theme="dark">
        <div class="sidebar-brand">
          <button class="brand-link btn btn-link text-start" type="button" data-nav="connections">
            <span class="brand-image d-inline-flex align-items-center justify-content-center">
              <img src="${logoUrl}" alt="SSH GATE" style="width:30px;height:30px;border-radius:7px;object-fit:cover;" />
            </span>
            <span class="brand-text fw-semibold">SSH GATE</span>
          </button>
        </div>
        <div class="sidebar-wrapper">
          <nav class="mt-2">
            <ul class="nav sidebar-menu flex-column" role="menu">
              ${navItem('connections', 'bi-hdd-network', t('nav.connections'))}
              ${navItem('commands', 'bi-terminal', t('nav.commands'), waitCount)}
              ${navItem('terminal', 'bi-terminal-fill', t('nav.terminal'))}
              ${navItem('settings', 'bi-broadcast-pin', t('nav.settings'))}
            </ul>
          </nav>
        </div>
        <div class="sidebar-status">
          <div class="d-flex align-items-center justify-content-between mb-2">
            <span class="status-heading mb-0">${escapeHtml(t('lang.toggleLabel'))}</span>
            <div class="btn-group btn-group-sm" role="group" aria-label="${escapeHtml(t('lang.toggleLabel'))}">
              <button class="btn ${state.lang === 'ja' ? 'btn-primary' : 'btn-outline-secondary'}" type="button" data-lang="ja">JA</button>
              <button class="btn ${state.lang === 'en' ? 'btn-primary' : 'btn-outline-secondary'}" type="button" data-lang="en">EN</button>
            </div>
          </div>
          <div class="status-heading">${escapeHtml(t('status.heading'))}</div>
          <div class="status-line">
            <span>${escapeHtml(t('status.mcpServer'))}</span>
            ${mcpStatusBadge(mcp.status, 'status-badge')}
          </div>
          <div class="status-line">
            <span>${escapeHtml(t('status.sshSession'))}</span>
            ${sshStatusBadge(currentConnection.status, 'status-badge')}
          </div>
          <div class="status-line">
            <span>${escapeHtml(t('status.currentConnection'))}</span>
            <strong class="status-value" title="${escapeHtml(currentConnection.name || '-')}">${escapeHtml(currentConnection.name || '-')}</strong>
          </div>
          <div class="status-line">
            <span>${escapeHtml(t('status.listenPort'))}</span>
            <strong class="status-value">${escapeHtml(mcp.port || '-')}</strong>
          </div>
          <div class="status-line">
            <span>${escapeHtml(t('status.connectedAgents'))}</span>
            <strong class="status-value">${escapeHtml(mcp.connectedAgents ?? 0)}</strong>
          </div>
          <div class="company-name">QuickIterate Co., Ltd.</div>
        </div>
      </aside>

      <main class="app-main">
        <div class="app-content-header">
          <div class="container-fluid">
            <div class="row">
              <div class="col-sm-6">
                <h3 class="mb-0">${pageTitle(state.active)}</h3>
              </div>
              <div class="col-sm-6">
                <ol class="breadcrumb float-sm-end">
                  <li class="breadcrumb-item">${escapeHtml(t('breadcrumb.root'))}</li>
                  <li class="breadcrumb-item active" aria-current="page">${pageTitle(state.active)}</li>
                </ol>
              </div>
            </div>
          </div>
        </div>
        <div class="app-content">
          <div class="container-fluid">
            ${content}
          </div>
        </div>
      </main>
      ${state.toast ? `<div class="toast show align-items-center text-bg-dark border-0 ssh-toast"><div class="d-flex"><div class="toast-body">${escapeHtml(state.toast)}</div></div></div>` : ''}
      ${state.passphrase.open ? passphraseModal() : ''}
      ${state.deleteConnection.open ? deleteConnectionModal() : ''}
      ${state.deleteCommand.open ? deleteCommandModal() : ''}
    </div>
  `;
}

function navItem(id, icon, label, badge = 0) {
  return `
    <li class="nav-item">
      <button class="nav-link ${state.active === id ? 'active' : ''}" type="button" data-nav="${id}">
        <i class="nav-icon bi ${icon}"></i>
        <p>${label}${badge ? `<span class="nav-badge badge text-bg-warning ms-2">${badge}</span>` : ''}</p>
      </button>
    </li>
  `;
}

function renderConnections() {
  const connections = state.data.connections ?? [];
  const selected = connections[state.selectedConnection] ?? connections[0] ?? {};
  return `
    <div class="row g-3">
      <div class="col-12 col-xl-4">
        <div class="card card-outline card-primary h-100">
          <div class="card-header">
            <div class="d-flex align-items-center justify-content-between">
              <h3 class="card-title fw-bold">${escapeHtml(t('conn.listTitle'))}</h3>
              <button class="btn btn-primary btn-sm" type="button" data-action="new-connection">
                <i class="bi bi-plus-lg me-1"></i>${escapeHtml(t('common.new'))}
              </button>
            </div>
          </div>
          <div class="card-body p-0">
            <div class="p-3">
              <div class="input-group">
                <span class="input-group-text"><i class="bi bi-search"></i></span>
                <input class="form-control" type="search" placeholder="${escapeHtml(t('common.search'))}" aria-label="${escapeHtml(t('conn.searchAria'))}">
              </div>
            </div>
            <div class="list-group list-group-flush ssh-list">
              ${connections.length ? connections.map((connection, index) => connectionRow(connection, index)).join('') : emptyBlock(t('conn.empty'))}
            </div>
          </div>
          <div class="card-footer text-secondary">${connections.length} ${escapeHtml(t('conn.countSuffix'))}</div>
        </div>
      </div>
      <div class="col-12 col-xl-8">
        <form class="card card-outline card-primary h-100" id="connection-form">
          <div class="card-header">
            <div class="d-flex align-items-start justify-content-between gap-3">
              <div>
                <h3 class="card-title fw-bold mb-1">${escapeHtml(t('conn.editTitle'))}</h3>
                <div class="text-secondary small">${escapeHtml(t('conn.editSubtitle'))}</div>
              </div>
              <span class="badge ${selected.status === 'Online' ? 'text-bg-success' : selected.status === 'Error' ? 'text-bg-danger' : 'text-bg-secondary'}">${escapeHtml(t('conn.reachability'))} ${escapeHtml(selected.status || 'Unknown')}</span>
            </div>
          </div>
          <div class="card-body">
            <div class="row g-3">
              ${inputCol(t('conn.name'), 'name', selected.name)}
              ${connectionTypeCol(selected.type)}
              ${(selected.type === 'Serial') ? connectionSerialFields(selected) : connectionSSHFields(selected)}
              ${inputCol(t('conn.tags'), 'tags', (selected.tags ?? []).join(', '), 'text', 'col-12')}
              <div class="col-12">
                <label class="form-label fw-bold">${escapeHtml(t('conn.description'))}</label>
                <textarea class="form-control" name="description" rows="4" autocapitalize="none" autocorrect="off" autocomplete="off" spellcheck="false">${escapeHtml(selected.description)}</textarea>
              </div>
            </div>
            <div class="callout callout-info mt-3 mb-0">
              ${(selected.type === 'Serial')
                ? escapeHtml(t('conn.calloutSerial'))
                : escapeHtml(t('conn.calloutSSH'))}
            </div>
            ${connectionTestResult()}
          </div>
          <div class="card-footer d-flex gap-2">
            ${(selected.type === 'Serial') ? '' : `
            <button class="btn btn-primary" type="button" data-action="connect-ssh">
              <i class="bi bi-plug-fill me-1"></i>${escapeHtml(t('conn.connect'))}
            </button>
            <button class="btn btn-outline-secondary" type="button" data-action="disconnect-ssh">
              <i class="bi bi-plug me-1"></i>${escapeHtml(t('conn.disconnect'))}
            </button>`}
            <div class="ms-auto d-flex gap-2">
              <button class="btn btn-outline-secondary" type="button">${escapeHtml(t('common.cancel'))}</button>
              <button class="btn btn-primary" type="submit">${escapeHtml(t('common.save'))}</button>
            </div>
          </div>
        </form>
      </div>
    </div>
  `;
}

function passphraseModal() {
  return `
    <div class="modal-backdrop show"></div>
    <div class="modal d-block" tabindex="-1" role="dialog" aria-modal="true">
      <div class="modal-dialog modal-dialog-centered">
        <form class="modal-content" id="passphrase-form">
          <div class="modal-header">
            <h5 class="modal-title fw-bold">${escapeHtml(t('passphrase.title'))}</h5>
            <button class="btn-close" type="button" data-action="close-passphrase" aria-label="${escapeHtml(t('common.close'))}"></button>
          </div>
          <div class="modal-body">
            <p class="text-secondary mb-3">${escapeHtml(state.passphrase.connectionName)} ${escapeHtml(t('passphrase.body1'))}</p>
            <label class="form-label fw-bold">${escapeHtml(t('passphrase.label'))}</label>
            <input class="form-control" name="passphrase" type="password" autocomplete="current-password" autocapitalize="none" autocorrect="off" spellcheck="false" value="${escapeHtml(state.passphrase.value)}" autofocus>
            <div class="form-text">${escapeHtml(t('passphrase.help'))}</div>
          </div>
          <div class="modal-footer">
            <button class="btn btn-outline-secondary" type="button" data-action="close-passphrase">${escapeHtml(t('common.cancel'))}</button>
            <button class="btn btn-primary" type="submit">${escapeHtml(t('passphrase.submit'))}</button>
          </div>
        </form>
      </div>
    </div>
  `;
}

function connectionRow(connection, index) {
  const statusClass = connection.status === 'Online' ? 'success' : connection.status === 'Error' ? 'danger' : 'secondary';
  const activeClass = index === state.selectedConnection ? 'active' : '';
  const isSerial = connection.type === 'Serial';
  const subtitle = isSerial
    ? `${escapeHtml(connection.serialPort || '-')} @ ${escapeHtml(connection.baudRate || 115200)}`
    : `${escapeHtml(connection.host)}:${escapeHtml(connection.port)} / ${escapeHtml(connection.user)}`;
  return `
    <div class="list-group-item connection-item ${activeClass}">
      <button class="connection-select" type="button" data-select-connection="${index}">
        <div>
          <div class="fw-bold">
            <span class="badge ${isSerial ? 'text-bg-info' : 'text-bg-primary'} me-1">${isSerial ? 'Serial' : 'SSH'}</span>
            ${escapeHtml(connection.name)}
          </div>
          <small>${subtitle}</small>
        </div>
        <span class="badge text-bg-${statusClass}">${escapeHtml(connection.status || 'Unknown')}</span>
      </button>
      <button class="btn btn-sm connection-trash" type="button" data-action="delete-connection" data-connection-name="${escapeHtml(connection.name)}" aria-label="${escapeHtml(connection.name)} ${escapeHtml(t('conn.deleteAria'))}">
        <i class="bi bi-trash"></i>
      </button>
    </div>
  `;
}

function deleteConnectionModal() {
  return `
    <div class="modal-backdrop show"></div>
    <div class="modal d-block" tabindex="-1" role="dialog" aria-modal="true">
      <div class="modal-dialog modal-dialog-centered">
        <div class="modal-content">
          <div class="modal-header">
            <h5 class="modal-title fw-bold">${escapeHtml(t('delConn.title'))}</h5>
            <button class="btn-close" type="button" data-action="close-delete-connection" aria-label="${escapeHtml(t('common.close'))}"></button>
          </div>
          <div class="modal-body">
            <p class="mb-2"><strong>${escapeHtml(state.deleteConnection.connectionName)}</strong> ${escapeHtml(t('delConn.body1pre'))}</p>
            <p class="text-secondary mb-0">${escapeHtml(t('delConn.body2'))}</p>
          </div>
          <div class="modal-footer">
            <button class="btn btn-outline-secondary" type="button" data-action="close-delete-connection">${escapeHtml(t('common.cancel'))}</button>
            <button class="btn btn-danger" type="button" data-action="confirm-delete-connection">
              <i class="bi bi-trash me-1"></i>${escapeHtml(t('common.delete'))}
            </button>
          </div>
        </div>
      </div>
    </div>
  `;
}

function deleteCommandModal() {
  return `
    <div class="modal-backdrop show"></div>
    <div class="modal d-block" tabindex="-1" role="dialog" aria-modal="true">
      <div class="modal-dialog modal-dialog-centered">
        <div class="modal-content">
          <div class="modal-header">
            <h5 class="modal-title fw-bold">${escapeHtml(t('delCmd.title'))}</h5>
            <button class="btn-close" type="button" data-action="close-delete-command" aria-label="${escapeHtml(t('common.close'))}"></button>
          </div>
          <div class="modal-body">
            <p class="mb-2"><strong>${escapeHtml(state.deleteCommand.requestId)}</strong> ${escapeHtml(t('delCmd.body1pre'))}</p>
            <p class="text-secondary mb-0">${escapeHtml(t('delCmd.body2'))}</p>
          </div>
          <div class="modal-footer">
            <button class="btn btn-outline-secondary" type="button" data-action="close-delete-command">${escapeHtml(t('common.cancel'))}</button>
            <button class="btn btn-danger" type="button" data-action="confirm-delete-command">
              <i class="bi bi-trash me-1"></i>${escapeHtml(t('common.delete'))}
            </button>
          </div>
        </div>
      </div>
    </div>
  `;
}

function connectionTestResult() {
  if (!state.connectionTest.message) return '';
  return `
    <div class="alert ${state.connectionTest.ok ? 'alert-success' : 'alert-danger'} mt-3 mb-0">
      <div class="d-flex align-items-start justify-content-between gap-3">
        <div>
          <div class="fw-bold mb-1">${escapeHtml(t('test.lastResult'))}${state.connectionTest.target ? `: ${escapeHtml(state.connectionTest.target)}` : ''}</div>
          <div class="small">${escapeHtml(state.connectionTest.message)}</div>
        </div>
        <button class="btn btn-sm btn-outline-secondary flex-shrink-0" type="button" data-action="copy-connection-test">
          <i class="bi bi-copy me-1"></i>${escapeHtml(t('common.copy'))}
        </button>
      </div>
    </div>
  `;
}

// Selection is tracked by request ID, not by list index: the poll prepends new
// (auto-executed) entries to state.data.requests, so indexes shift under the user.
function selectedCommandRequest() {
  const requests = state.data.requests ?? [];
  return requests.find((request) => request.id === state.selectedRequestId)
    ?? requests.find((request) => request.status === '承認待ち')
    ?? null;
}

function renderCommands() {
  const requests = state.data.requests ?? [];
  const queueRequests = requests.filter((request) => request.status === '承認待ち');
  const selected = selectedCommandRequest();
  const selectedId = selected?.id ?? '';
  return `
    <div class="row g-3">
      <div class="col-12 col-xl-4">
        <div class="card card-outline card-warning h-100">
          <div class="card-header">
            <div class="d-flex align-items-center justify-content-between">
              <h3 class="card-title fw-bold">${escapeHtml(t('cmd.queueTitle'))}</h3>
              <span class="badge text-bg-warning">${queueRequests.length}${escapeHtml(t('cmd.queueWaiting'))}</span>
            </div>
          </div>
          <div class="card-body p-0">
            <div class="list-group list-group-flush ssh-list">
              ${queueRequests.length ? queueRequests.map((request) => requestRow(request, selectedId)).join('') : emptyBlock(t('cmd.queueEmpty'))}
            </div>
          </div>
        </div>
      </div>
      <div class="col-12 col-xl-8">
        ${selected ? commandDetail(selected) : emptyCommandDetail()}
      </div>
      <div class="col-12">
        <div class="card">
          <div class="card-header">
            <h3 class="card-title fw-bold">${escapeHtml(t('cmd.historyTitle'))}</h3>
          </div>
          <div class="card-body p-0">
            ${requests.length ? historyTable(requests, selectedId) : emptyBlock(t('cmd.historyEmpty'))}
          </div>
        </div>
      </div>
    </div>
  `;
}

function requestRow(request, selectedId) {
  return `
    <button class="list-group-item list-group-item-action ${request.id === selectedId ? 'active' : ''}" type="button" data-select-request="${escapeHtml(request.id)}">
      <div class="d-flex align-items-start justify-content-between gap-2">
        <div>
          <div class="fw-bold">${escapeHtml(request.id)}</div>
          <small>${escapeHtml(request.requestedBy || t('cmd.unknownAgent'))} / ${escapeHtml(request.requestedAt)}</small>
        </div>
        <span class="badge text-bg-secondary">${escapeHtml(requestStatusLabel(request.status))}</span>
      </div>
      <code class="d-block mt-2 small text-truncate">${escapeHtml(request.command)}</code>
    </button>
  `;
}

function commandDetail(selected) {
  const bypassEnabled = agentBypassEnabled(selected.requestedBy);
  const pending = selected.status === '承認待ち';
  return `
    <div class="card card-outline ${pending ? 'card-warning' : 'card-secondary'} h-100">
      <div class="card-header">
        <div class="d-flex align-items-start justify-content-between gap-3">
          <div>
            <h3 class="card-title fw-bold mb-1">${escapeHtml(pending ? t('cmd.approveTitle') : t('cmd.detailTitle'))}</h3>
            <div class="text-secondary small">${escapeHtml(pending ? t('cmd.approveSubtitle') : t('cmd.detailSubtitle'))}</div>
          </div>
          <span class="badge ${pending ? 'text-bg-warning' : 'text-bg-secondary'}">${escapeHtml(requestStatusLabel(selected.status))}</span>
        </div>
      </div>
      <div class="card-body">
        <div class="command-agent mb-3">
          <span>${escapeHtml(t('cmd.agent'))}</span>
          <strong>${escapeHtml(selected.requestedBy || t('cmd.unknownAgent'))}</strong>
          ${bypassEnabled ? `<em>${escapeHtml(t('cmd.bypassing'))}</em>` : ''}
        </div>

        <div class="mb-3">
          <div class="d-flex justify-content-between align-items-center mb-2">
            <h5 class="fw-bold mb-0">${escapeHtml(t('cmd.plannedCommand'))}</h5>
            <button class="btn btn-outline-secondary btn-sm" type="button" data-action="copy-command">${escapeHtml(t('common.copy'))}</button>
          </div>
          <pre class="command-block"><code>${escapeHtml(selected.command)}</code></pre>
        </div>

        <div class="mb-3">
          <h5 class="fw-bold">${escapeHtml(t('cmd.reason'))}</h5>
          <div class="reason-box">${escapeHtml(selected.reason || t('cmd.reasonEmpty'))}</div>
        </div>

        <div class="row g-3">
          ${terminalPanel('stdout', selected.stdout || (pending ? t('cmd.stdoutPlaceholder') : t('cmd.stdoutEmpty')))}
          ${terminalPanel('stderr', selected.stderr || t('cmd.stderrPlaceholder'), true)}
        </div>
      </div>
      <div class="card-footer d-flex gap-2">
        <button class="btn btn-outline-danger" type="button" data-action="delete-command-request" data-command-request-id="${escapeHtml(selected.id)}">
          <i class="bi bi-trash me-1"></i>${escapeHtml(t('cmd.deleteHistory'))}
        </button>
        ${pending ? `
        <button class="btn btn-danger" type="button" data-action="reject-command">${escapeHtml(t('cmd.reject'))}</button>
        <button class="btn btn-outline-primary" type="button" data-action="approve-bypass-agent">
          ${escapeHtml(t('cmd.approveBypass'))}
        </button>
        <button class="btn btn-primary ms-auto" type="button" data-action="approve-command">${escapeHtml(t('cmd.approve'))}</button>
        ` : ''}
      </div>
    </div>
  `;
}

function agentBypassEnabled(agentName) {
  const name = String(agentName ?? '').trim();
  return Boolean(name && (state.data.agentPolicies ?? []).some((policy) => policy.agentName === name && policy.approvalBypass));
}

function emptyCommandDetail() {
  return `
    <div class="card h-100">
      <div class="card-body empty-state">
        <i class="bi bi-terminal fs-1 text-secondary"></i>
        <h4>${escapeHtml(t('cmd.emptyTitle'))}</h4>
        <p>${escapeHtml(t('cmd.emptyDesc'))}</p>
      </div>
    </div>
  `;
}

function renderTerminal() {
  const connections = state.data.connections ?? [];
  const selected = connections[state.selectedConnection] ?? connections[0] ?? {};
  const connected = state.terminal.connected;
  const isSerial = selected.type === 'Serial';
  const heading = isSerial ? t('term.headingSerial') : t('term.headingSSH');
  const connectLabel = isSerial ? t('term.connectSerial') : t('term.connectSSH');
  const connectIcon = isSerial ? 'bi-usb-symbol' : 'bi-terminal-fill';
  return `
    <div class="row g-3">
      <div class="col-12 col-xl-4">
        <div class="card card-outline card-primary h-100">
          <div class="card-header">
            <h3 class="card-title fw-bold mb-0">${escapeHtml(t('term.target'))}</h3>
          </div>
          <div class="card-body">
            <div class="mb-3">
              <label class="form-label small fw-bold text-secondary">${escapeHtml(t('term.selectLabel'))}</label>
              <select class="form-select" data-action="terminal-select-connection" ${connected ? 'disabled' : ''}>
                ${connections.length
                  ? connections.map((connection, index) => `
                    <option value="${index}" ${index === state.selectedConnection ? 'selected' : ''}>[${connection.type === 'Serial' ? 'Serial' : 'SSH'}] ${escapeHtml(connection.name || t('term.unnamed'))}</option>
                  `).join('')
                  : `<option>${escapeHtml(t('term.noConnections'))}</option>`}
              </select>
            </div>
            ${connections.length ? terminalConnectionDetail(selected) : emptyBlock(t('term.notRegistered'))}
            <div class="d-grid gap-2 mt-3">
              ${connected
                ? `<button class="btn btn-danger" type="button" data-action="terminal-disconnect"><i class="bi bi-plug me-1"></i>${escapeHtml(t('term.disconnect'))}</button>`
                : `<button class="btn btn-primary" type="button" data-action="terminal-connect" ${connections.length ? '' : 'disabled'}><i class="bi ${connectIcon} me-1"></i>${escapeHtml(connectLabel)}</button>`}
            </div>
          </div>
        </div>
      </div>
      <div class="col-12 col-xl-8">
        <div class="card card-outline card-dark h-100">
          <div class="card-header">
            <div class="d-flex align-items-center justify-content-between gap-2">
              <h3 class="card-title fw-bold mb-0">${escapeHtml(heading)}</h3>
              <span class="badge ${connected ? 'text-bg-success' : 'text-bg-secondary'}">
                ${connected ? `${escapeHtml(t('term.connectedTo'))}: ${escapeHtml(state.terminal.label || '')}` : escapeHtml(t('term.disconnected'))}
              </span>
            </div>
          </div>
          <div class="card-body p-2">
            <div class="xterm-host" id="xterm-host"></div>
          </div>
        </div>
      </div>
    </div>
  `;
}

function terminalConnectionDetail(connection) {
  const rows = connection.type === 'Serial'
    ? [
      [t('term.detail.type'), 'Serial'],
      [t('term.detail.port'), connection.serialPort],
      [t('term.detail.baudRate'), connection.baudRate ? `${connection.baudRate} bps` : '-'],
      [t('term.detail.tags'), (connection.tags ?? []).join(', ')],
      [t('term.detail.description'), connection.description],
    ]
    : [
      [t('term.detail.type'), 'SSH'],
      [t('term.detail.host'), connection.host],
      [t('term.detail.port'), connection.port],
      [t('term.detail.user'), connection.user],
      [t('term.detail.authMethod'), connection.authMethod],
      [t('term.detail.tags'), (connection.tags ?? []).join(', ')],
      [t('term.detail.state'), connection.status],
      [t('term.detail.description'), connection.description],
    ];
  return `
    <dl class="terminal-detail mb-0">
      ${rows.map(([label, value]) => `
        <dt>${escapeHtml(label)}</dt>
        <dd>${escapeHtml(value || '-')}</dd>
      `).join('')}
    </dl>
  `;
}

function b64ToBytes(b64) {
  const bin = atob(b64);
  const bytes = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i += 1) bytes[i] = bin.charCodeAt(i);
  return bytes;
}

function ensureTerminal() {
  if (term) return;
  term = new Terminal({
    fontSize: 13,
    fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Consolas, monospace',
    cursorBlink: true,
    theme: { background: '#0b1120', foreground: '#d8e2f2' },
  });
  fitAddon = new FitAddon();
  term.loadAddon(fitAddon);
}

// Re-attach the persistent xterm element into the freshly rendered host.
function mountTerminal() {
  const host = document.querySelector('#xterm-host');
  if (!host) return;
  ensureTerminal();
  if (!term.element) {
    term.open(host);
  } else if (term.element.parentElement !== host) {
    host.appendChild(term.element);
  }
  if (!termWired) {
    term.onData((data) => {
      if (state.terminal.connected) SendTerminalInput(data);
    });
    termWired = true;
  }
  // クリックでターミナルにフォーカス（再描画でフォーカスが外れても拾えるように）。
  if (!host.dataset.focusWired) {
    host.addEventListener('mousedown', () => { if (term) term.focus(); });
    host.dataset.focusWired = '1';
  }
  fitTerminal();
  if (state.terminal.connected) {
    // 再アタッチ直後はDOMが落ち着く前なので、次フレームで確実にフォーカスする。
    setTimeout(() => { if (term) term.focus(); }, 0);
  }
}

function fitTerminal() {
  if (!term || !fitAddon || !term.element) return;
  try {
    fitAddon.fit();
    if (state.terminal.connected) ResizeTerminal(term.cols, term.rows);
  } catch (error) {
    // host not laid out yet; ignored.
  }
}

async function refreshSerialPorts() {
  try {
    state.serialPorts = (await ListSerialPorts()) ?? [];
  } catch (error) {
    state.serialPorts = [];
  }
}

async function teardownTerminal() {
  if (state.terminal.connected) {
    await CloseTerminal();
  }
  state.terminal.connected = false;
  state.terminal.label = '';
  if (term) {
    term.dispose();
    term = null;
    fitAddon = null;
    termWired = false;
  }
}

EventsOn('terminal:data', (payload) => {
  if (term) term.write(b64ToBytes(payload));
});

EventsOn('terminal:exit', (message) => {
  if (term) term.write(`\r\n\x1b[33m[${message}]\x1b[0m\r\n`);
  state.terminal.connected = false;
  state.terminal.label = '';
  if (state.active === 'terminal') render();
});

window.addEventListener('resize', () => {
  if (state.active === 'terminal') fitTerminal();
});

function renderSettings() {
  const mcp = state.data.mcp ?? {};
  const endpoint = mcpEndpoint(mcp);
  const config = agentConfig(mcp);
  return `
    <form class="row g-3" id="mcp-form">
      <div class="col-12 col-xl-8">
        <div class="card card-outline card-primary">
          <div class="card-header">
            <div class="d-flex align-items-start justify-content-between gap-3">
              <div>
                <h3 class="card-title fw-bold mb-1">${escapeHtml(t('mcp.title'))}</h3>
                <div class="text-secondary small">${escapeHtml(t('mcp.subtitle'))}</div>
              </div>
              ${mcpStatusBadge(mcp.status)}
            </div>
          </div>
          <div class="card-body">
            <div class="row g-3">
              ${checkCol(t('mcp.enable'), 'enabled', mcp.enabled)}
              ${checkCol(t('mcp.bearer'), 'bearerEnabled', mcp.bearerEnabled)}
              ${inputCol(t('mcp.listenAddress'), 'listenAddress', mcp.listenAddress)}
              ${inputCol(t('mcp.port'), 'port', mcp.port, 'number')}
              ${selectCol(t('mcp.transport'), 'transport', mcp.transport, ['Streamable HTTP'])}
              ${inputCol(t('mcp.baseURL'), 'baseURL', mcp.baseURL)}
              ${inputCol(t('mcp.mcpPath'), 'mcpPath', mcp.mcpPath)}
              ${inputCol(t('mcp.healthPath'), 'healthPath', mcp.healthPath)}
              ${inputCol(t('mcp.auditPath'), 'auditPath', mcp.auditPath)}
              ${inputCol(t('mcp.tokenName'), 'tokenName', mcp.tokenName)}
              ${inputCol(t('mcp.tokenInput'), 'tokenInput', '', 'password')}
              ${readonlyCol(t('mcp.tokenPreview'), mcp.tokenPreview)}
              ${inputCol(t('mcp.allowedOrigins'), 'allowedOrigins', (mcp.allowedOrigins ?? []).join(', '), 'text', 'col-12')}
              ${selectCol(t('mcp.tlsMode'), 'tlsMode', mcp.tlsMode, ['ローカル平文', '内蔵TLS（未実装）', 'リバースプロキシ終端'], 'col-12 col-md-6', { 'ローカル平文': t('mcp.tls.local'), '内蔵TLS（未実装）': t('mcp.tls.builtin'), 'リバースプロキシ終端': t('mcp.tls.proxy') })}
              ${selectCol(t('mcp.proxyMode'), 'proxyMode', mcp.proxyMode, ['無効', 'nginx', 'Caddy', 'Cloudflare Tunnel'], 'col-12 col-md-6', { '無効': t('mcp.proxy.disabled') })}
              ${inputCol(t('mcp.maxBodyKB'), 'maxBodyKB', mcp.maxBodyKB, 'number')}
              ${inputCol(t('mcp.maxOutputKB'), 'maxOutputKB', mcp.maxOutputKB, 'number')}
              ${inputCol(t('mcp.requestTimeout'), 'requestTimeout', mcp.requestTimeout, 'number')}
              ${inputCol(t('mcp.defaultConnectTimeout'), 'defaultConnectTimeout', mcp.defaultConnectTimeout, 'number')}
              ${inputCol(t('mcp.defaultCommandTimeout'), 'defaultCommandTimeout', mcp.defaultCommandTimeout, 'number')}
              ${checkCol(t('mcp.strictHostKey'), 'strictHostKey', mcp.strictHostKey)}
              ${checkCol(t('mcp.requireApprovalSudo'), 'requireApprovalSudo', mcp.requireApprovalSudo)}
              ${checkCol(t('mcp.requireApprovalProd'), 'requireApprovalProd', mcp.requireApprovalProd)}
              ${checkCol(t('mcp.requireApprovalWriteOp'), 'requireApprovalWriteOp', mcp.requireApprovalWriteOp)}
              ${readonlyCol(t('mcp.configPath'), mcp.configPath, 'col-12')}
            </div>
            <div class="callout callout-info mt-3 mb-0">${escapeHtml(t('mcp.callout'))}</div>
            ${mcp.lastError ? `<div class="alert alert-danger mt-3 mb-0">${escapeHtml(mcp.lastError)}</div>` : ''}
          </div>
          <div class="card-footer d-flex gap-2">
            <button class="btn btn-outline-primary" type="button" data-action="refresh-mcp"><i class="bi bi-arrow-clockwise me-1"></i>${escapeHtml(t('mcp.refresh'))}</button>
            <button class="btn btn-outline-danger" type="button" data-action="delete-mcp"><i class="bi bi-trash me-1"></i>${escapeHtml(t('mcp.deleteSettings'))}</button>
            <button class="btn btn-primary ms-auto" type="submit">${escapeHtml(t('mcp.saveStart'))}</button>
          </div>
        </div>
      </div>
      <div class="col-12 col-xl-4">
        <div class="card card-outline card-info">
          <div class="card-header">
            <h3 class="card-title fw-bold">${escapeHtml(t('mcp.endpointTitle'))}</h3>
          </div>
          <div class="card-body">
            ${endpointRow('MCP', mcp.mcpPath, mcp.bearerEnabled ? t('mcp.bearerRequired') : t('mcp.noAuth'))}
            ${endpointRow('Health', mcp.healthPath, t('mcp.noAuth'))}
            ${endpointRow('Audit API', mcp.auditPath, mcp.bearerEnabled ? t('mcp.bearerRequired') : t('mcp.noAuth'))}
            <div class="mt-3">
              <label class="form-label fw-bold">${escapeHtml(t('mcp.connectURL'))}</label>
              <div class="input-group">
                <input class="form-control" value="${escapeHtml(endpoint)}" readonly>
                <button class="btn btn-outline-secondary" type="button" data-action="copy-mcp-endpoint"><i class="bi bi-copy"></i></button>
              </div>
            </div>
          </div>
        </div>
        <div class="card">
          <div class="card-header">
            <div class="d-flex align-items-center justify-content-between">
              <h3 class="card-title fw-bold">${escapeHtml(t('mcp.usageTitle'))}</h3>
              <button class="btn btn-outline-secondary btn-sm" type="button" data-action="copy-agent-config"><i class="bi bi-copy me-1"></i>${escapeHtml(t('common.copy'))}</button>
            </div>
          </div>
          <div class="card-body">
            <pre class="command-block"><code>${escapeHtml(config)}</code></pre>
            <div class="usage-list mt-3">
              ${usageRow('list_hosts', t('mcp.usage.listHosts'))}
              ${usageRow('execute_command', t('mcp.usage.executeCommand'))}
              ${usageRow('request_command_execution', t('mcp.usage.requestExec'))}
              ${usageRow('get_command_history', t('mcp.usage.getHistory'))}
            </div>
          </div>
        </div>
      </div>
    </form>
  `;
}

function inputCol(label, name, value, type = 'text', col = 'col-12 col-md-6') {
  return `
    <div class="${col}">
      <label class="form-label fw-bold">${label}</label>
      <input class="form-control" name="${name}" type="${type}" value="${escapeHtml(value)}" autocapitalize="none" autocorrect="off" autocomplete="off" spellcheck="false">
    </div>
  `;
}

function readonlyCol(label, value, col = 'col-12 col-md-6') {
  return `
    <div class="${col}">
      <label class="form-label fw-bold">${label}</label>
      <input class="form-control" value="${escapeHtml(value)}" readonly>
    </div>
  `;
}

function connectionTypeCol(type) {
  const value = type === 'Serial' ? 'Serial' : 'SSH';
  return `
    <div class="col-12 col-md-6">
      <label class="form-label fw-bold">${escapeHtml(t('conn.type'))}</label>
      <select class="form-select" name="type" data-action="connection-type">
        <option value="SSH" ${value === 'SSH' ? 'selected' : ''}>SSH</option>
        <option value="Serial" ${value === 'Serial' ? 'selected' : ''}>${escapeHtml(t('conn.typeSerial'))}</option>
      </select>
    </div>
  `;
}

function connectionSSHFields(selected) {
  return `
    ${inputCol(t('conn.host'), 'host', selected.host)}
    ${inputCol(t('conn.port'), 'port', selected.port, 'number')}
    ${inputCol(t('conn.user'), 'user', selected.user)}
    ${selectCol(t('conn.authMethod'), 'authMethod', selected.authMethod, ['SSH鍵認証', 'パスワード保管'], 'col-12 col-md-6', { 'SSH鍵認証': t('conn.authKey'), 'パスワード保管': t('conn.authPassword') })}
    ${inputCol(t('conn.credential'), 'credential', selected.credential)}
    ${inputCol(t('conn.connectTimeout'), 'connectTimeout', selected.connectTimeout, 'number')}
    ${inputCol(t('conn.commandTimeout'), 'commandTimeout', selected.commandTimeout, 'number')}
  `;
}

function connectionSerialFields(selected) {
  const ports = state.serialPorts ?? [];
  const current = selected.serialPort || '';
  // Include the saved port even if it is currently unplugged / not scanned.
  const options = [...new Set([current, ...ports].filter(Boolean))];
  const baud = selected.baudRate || 115200;
  return `
    <div class="col-12 col-md-6">
      <div class="d-flex justify-content-between align-items-center">
        <label class="form-label fw-bold mb-0">${escapeHtml(t('conn.serialPort'))}</label>
        <button class="btn btn-link btn-sm p-0" type="button" data-action="rescan-serial-ports">
          <i class="bi bi-arrow-clockwise me-1"></i>${escapeHtml(t('conn.rescan'))}
        </button>
      </div>
      <select class="form-select" name="serialPort">
        ${options.length
          ? options.map((port) => `<option value="${escapeHtml(port)}" ${port === current ? 'selected' : ''}>${escapeHtml(port)}</option>`).join('')
          : `<option value="">${escapeHtml(t('conn.noPorts'))}</option>`}
      </select>
    </div>
    <div class="col-12 col-md-6">
      <label class="form-label fw-bold">${escapeHtml(t('conn.baudRate'))}</label>
      <select class="form-select" name="baudRate">
        ${BAUD_RATES.map((rate) => `<option value="${rate}" ${rate === baud ? 'selected' : ''}>${rate}</option>`).join('')}
      </select>
    </div>
  `;
}

function selectCol(label, name, selected, options, col = 'col-12 col-md-6', labels = {}) {
  // The option value stays the raw (backend) string so the saved form value is
  // unchanged; only the visible text is localized via the optional labels map.
  return `
    <div class="${col}">
      <label class="form-label fw-bold">${label}</label>
      <select class="form-select" name="${name}">
        ${options.map((option) => `<option value="${escapeHtml(option)}" ${option === selected ? 'selected' : ''}>${escapeHtml(labels[option] ?? option)}</option>`).join('')}
      </select>
    </div>
  `;
}

function checkCol(label, name, value, col = 'col-12 col-md-6') {
  return `
    <div class="${col}">
      <div class="form-check form-switch mt-4">
        <input class="form-check-input" type="checkbox" role="switch" id="${name}" name="${name}" ${boolAttr(value)}>
        <label class="form-check-label fw-bold" for="${name}">${label}</label>
      </div>
    </div>
  `;
}

function terminalPanel(title, content, error = false) {
  return `
    <div class="col-12 col-lg-6">
      <div class="terminal-panel ${error ? 'terminal-error' : ''}">
        <div class="terminal-title">${title}</div>
        <pre>${escapeHtml(content)}</pre>
      </div>
    </div>
  `;
}

function historyTable(requests, selectedId) {
  return `
    <div class="table-responsive">
      <table class="table table-hover align-middle mb-0">
        <thead>
          <tr>
            <th>${escapeHtml(t('hist.time'))}</th>
            <th>${escapeHtml(t('hist.agent'))}</th>
            <th>${escapeHtml(t('hist.status'))}</th>
            <th>${escapeHtml(t('hist.command'))}</th>
            <th>${escapeHtml(t('hist.reason'))}</th>
            <th class="text-end">${escapeHtml(t('hist.action'))}</th>
          </tr>
        </thead>
        <tbody>
          ${requests.map((request) => `
            <tr class="${request.id === selectedId ? 'table-active' : ''}" data-select-request="${escapeHtml(request.id)}" style="cursor:pointer">
              <td>${escapeHtml(request.requestedAt)}</td>
              <td>${escapeHtml(request.requestedBy || t('cmd.unknownAgent'))}</td>
              <td><span class="badge text-bg-secondary">${escapeHtml(requestStatusLabel(request.status))}</span></td>
              <td><code>${escapeHtml(request.command)}</code></td>
              <td>${escapeHtml(request.reason || '-')}</td>
              <td class="text-end">
                <button class="btn btn-outline-danger btn-sm" type="button" data-action="delete-command-request" data-command-request-id="${escapeHtml(request.id)}" title="${escapeHtml(t('cmd.deleteHistory'))}">
                  <i class="bi bi-trash"></i>
                </button>
              </td>
            </tr>
          `).join('')}
        </tbody>
      </table>
    </div>
  `;
}

function endpointRow(label, path, auth) {
  return `
    <div class="endpoint-row">
      <span>${label}</span>
      <code>${escapeHtml(path)}</code>
      <strong>${auth}</strong>
    </div>
  `;
}

function usageRow(name, description) {
  return `
    <div class="usage-row">
      <code>${escapeHtml(name)}</code>
      <span>${escapeHtml(description)}</span>
    </div>
  `;
}

function emptyBlock(message) {
  return `<div class="p-4 text-center text-secondary">${escapeHtml(message)}</div>`;
}

function collectConnectionForm() {
  const form = document.querySelector('#connection-form');
  const data = Object.fromEntries(new FormData(form));
  const current = state.data.connections[state.selectedConnection] ?? {};
  // Mode-specific fields are only in the DOM for the active type; fall back to
  // the stored value so switching SSH <-> Serial never drops the other side.
  const pick = (key, fallback) => (key in data ? field(data, key) : (current[key] ?? fallback));
  const pickNum = (key, fallback) => (key in data ? Number(field(data, key, fallback)) : (current[key] ?? fallback));
  return {
    name: pick('name', ''),
    type: pick('type', 'SSH'),
    host: pick('host', ''),
    port: pickNum('port', 22),
    user: pick('user', ''),
    authMethod: pick('authMethod', 'SSH鍵認証'),
    credential: pick('credential', ''),
    serialPort: pick('serialPort', ''),
    baudRate: pickNum('baudRate', 115200),
    connectTimeout: pickNum('connectTimeout', 10),
    commandTimeout: pickNum('commandTimeout', 30),
    tags: ('tags' in data ? field(data, 'tags') : (current.tags ?? []).join(', '))
      .split(',').map((tag) => tag.trim()).filter(Boolean),
    description: pick('description', ''),
    status: current.status ?? 'Unknown',
    lastChecked: current.lastChecked ?? '-',
  };
}

function collectMCPForm() {
  const form = document.querySelector('#mcp-form');
  const data = Object.fromEntries(new FormData(form));
  const checked = (name) => Boolean(form.querySelector(`[name="${name}"]`)?.checked);
  return {
    enabled: checked('enabled'),
    listenAddress: field(data, 'listenAddress'),
    port: Number(field(data, 'port', 8787)),
    transport: field(data, 'transport'),
    baseURL: field(data, 'baseURL'),
    mcpPath: field(data, 'mcpPath'),
    healthPath: field(data, 'healthPath'),
    auditPath: field(data, 'auditPath'),
    bearerEnabled: checked('bearerEnabled'),
    tokenName: field(data, 'tokenName'),
    tokenInput: field(data, 'tokenInput'),
    allowedOrigins: field(data, 'allowedOrigins').split(',').map((origin) => origin.trim()).filter(Boolean),
    tlsMode: field(data, 'tlsMode'),
    proxyMode: field(data, 'proxyMode'),
    maxBodyKB: Number(field(data, 'maxBodyKB', 256)),
    maxOutputKB: Number(field(data, 'maxOutputKB', 128)),
    requestTimeout: Number(field(data, 'requestTimeout', 30)),
    defaultConnectTimeout: Number(field(data, 'defaultConnectTimeout', 10)),
    defaultCommandTimeout: Number(field(data, 'defaultCommandTimeout', 30)),
    strictHostKey: checked('strictHostKey'),
    autoExecuteLowRisk: false,
    requireApprovalSudo: checked('requireApprovalSudo'),
    requireApprovalProd: checked('requireApprovalProd'),
    requireApprovalWriteOp: checked('requireApprovalWriteOp'),
    configPath: state.data.mcp?.configPath ?? '',
  };
}

function render() {
  const content = state.active === 'connections'
    ? renderConnections()
    : state.active === 'commands'
      ? renderCommands()
      : state.active === 'terminal'
        ? renderTerminal()
        : renderSettings();

  app.innerHTML = shell(content);

  if (state.active === 'terminal') {
    mountTerminal();
  }
}

app.addEventListener('click', async (event) => {
  const langButton = event.target.closest('[data-lang]');
  if (langButton) {
    const lang = langButton.dataset.lang;
    if (lang && lang !== state.lang) setLang(lang);
    return;
  }

  const nav = event.target.closest('[data-nav]');
  if (nav) {
    const target = nav.dataset.nav;
    if (state.active === 'terminal' && target !== 'terminal') {
      await teardownTerminal();
    }
    state.active = target;
    render();
    return;
  }

  const actionElement = event.target.closest('[data-action]');
  const action = actionElement?.dataset.action;

  if (action === 'terminal-connect') {
    const connection = state.data.connections[state.selectedConnection];
    if (!connection?.name) {
      showToast(t('conn.notSelected'));
      return;
    }
    const result = await StartTerminal(connection.name);
    if (result.ok) {
      state.terminal.connected = true;
      state.terminal.label = connection.name;
      render();
      if (term) term.focus();
    }
    showToast(result.message);
    return;
  }

  if (action === 'terminal-disconnect') {
    await teardownTerminal();
    render();
    showToast(t('conn.disconnected'));
    return;
  }

  if (action === 'rescan-serial-ports') {
    const form = collectConnectionForm();
    state.data.connections[state.selectedConnection] = { ...state.data.connections[state.selectedConnection], ...form };
    state.dirty.connections = true;
    await refreshSerialPorts();
    render();
    return;
  }

  if (action) {
    const selectedConnection = state.data.connections[state.selectedConnection];
    // Must resolve the same request the detail pane renders (selectedCommandRequest),
    // never by list index: history entries prepend as agents run and indexes shift.
    const selectedRequest = selectedCommandRequest();

    if (action === 'new-connection') {
      state.data.connections.push({
        name: t('conn.newName'),
        type: 'SSH',
        host: '127.0.0.1',
        port: 22,
        user: '',
        authMethod: 'SSH鍵認証',
        credential: '~/.ssh/id_ed25519',
        serialPort: '',
        baudRate: 115200,
        connectTimeout: 10,
        commandTimeout: 30,
        tags: [],
        description: '',
        status: 'Unknown',
        lastChecked: '-',
      });
      state.selectedConnection = state.data.connections.length - 1;
      state.dirty.connections = true;
      render();
      return;
  }

  if (action === 'connect-ssh') {
    const connectionToTest = collectConnectionForm();
    const result = await TestConnectionConfig(connectionToTest, '');
    if (!result.ok && result.message.includes('passphrase protected')) {
      state.passphrase = {
        open: true,
        connectionName: connectionToTest.name || connectionToTest.host || t('conn.unsaved'),
        value: '',
        connection: connectionToTest,
      };
      render();
      return;
    }
    state.connectionTest = {
      ok: result.ok,
      message: result.message,
      target: `${connectionToTest.user}@${connectionToTest.host}:${connectionToTest.port}`,
    };
    showToast(result.message);
    await refreshData({ silent: true });
    return;
  }

  if (action === 'disconnect-ssh') {
    const result = await DisconnectConnection(collectConnectionForm().name);
    state.connectionTest = {
      ok: result.ok,
      message: result.message,
      target: '',
    };
    showToast(result.message);
    await refreshData({ silent: true });
    return;
  }

  if (action === 'delete-connection') {
    const targetName = actionElement?.dataset.connectionName || selectedConnection?.name || '';
    if (!targetName) return;
    state.deleteConnection = { open: true, connectionName: targetName };
    render();
    return;
  }

  if (action === 'close-delete-connection') {
    state.deleteConnection = { open: false, connectionName: '' };
    render();
    return;
  }

  if (action === 'confirm-delete-connection') {
    const targetName = state.deleteConnection.connectionName;
    if (!targetName) return;
    const result = await DeleteConnection(targetName);
    if (result.ok) {
      const deletedIndex = state.data.connections.findIndex((connection) => connection.name === targetName);
      state.data.connections = state.data.connections.filter((connection) => connection.name !== targetName);
      if (deletedIndex >= 0 && state.selectedConnection >= deletedIndex) {
        state.selectedConnection = Math.max(0, state.selectedConnection - 1);
      }
      if (state.selectedConnection >= state.data.connections.length) {
        state.selectedConnection = Math.max(0, state.data.connections.length - 1);
      }
      state.dirty.connections = false;
    }
    state.deleteConnection = { open: false, connectionName: '' };
    showToast(result.message);
    await refreshData({ silent: true });
    return;
  }

  if (action === 'delete-command-request') {
    const requestId = actionElement?.dataset.commandRequestId || selectedRequest?.id || '';
    if (!requestId) return;
    state.deleteCommand = { open: true, requestId };
    render();
    return;
  }

  if (action === 'close-delete-command') {
    state.deleteCommand = { open: false, requestId: '' };
    render();
    return;
  }

  if (action === 'confirm-delete-command') {
    const requestId = state.deleteCommand.requestId;
    if (!requestId) return;
    const result = await DeleteCommandRequest(requestId);
    if (result.ok) {
      state.data.requests = state.data.requests.filter((request) => request.id !== requestId);
      if (state.selectedRequestId === requestId) {
        state.selectedRequestId = '';
      }
    }
    state.deleteCommand = { open: false, requestId: '' };
    showToast(result.message);
    await refreshData({ silent: true });
    return;
  }

  if (action === 'close-passphrase') {
    state.passphrase = { open: false, connectionName: '', value: '', connection: null };
    render();
    return;
  }

  if (action === 'copy-command') {
    await copyText(selectedRequest?.command ?? '');
    return;
  }

  if (action === 'copy-connection-test') {
    await copyText(state.connectionTest.message);
    return;
  }

  if (action === 'copy-mcp-endpoint') {
    await copyText(mcpEndpoint(state.data.mcp ?? {}));
    return;
  }

  if (action === 'copy-agent-config') {
    await copyText(agentConfig(state.data.mcp ?? {}));
    return;
  }

  if (action === 'approve-command') {
    if (!selectedRequest) return;
    const command = document.querySelector('#approval-command')?.value ?? selectedRequest.command ?? '';
    const result = await ApproveCommand(selectedRequest.id ?? '', command);
    // Keep the approved request selected so its stdout/stderr shows in the pane.
    state.selectedRequestId = selectedRequest.id ?? '';
    showToast(result.message);
    await refreshData({ silent: true });
    return;
  }

  if (action === 'approve-bypass-agent') {
    if (!selectedRequest) return;
    const command = selectedRequest.command ?? '';
    const result = await ApproveCommandAndBypassAgent(selectedRequest.id ?? '', command);
    state.selectedRequestId = selectedRequest.id ?? '';
    showToast(result.message);
    await refreshData({ silent: true });
    return;
  }

  if (action === 'reject-command') {
    if (!selectedRequest) return;
    const result = await RejectCommand(selectedRequest.id ?? '');
    showToast(result.message);
    await refreshData({ silent: true });
    return;
  }

  if (action === 'refresh-mcp') {
    await refreshData({ silent: true });
    showToast(t('mcp.refreshed'));
  }

  if (action === 'delete-mcp') {
    if (!window.confirm(t('mcp.deleteConfirm'))) {
      return;
    }
    const result = await DeleteMCPSettings();
    showToast(result.message);
    await refreshData({ silent: true });
    return;
  }
  }

  const connection = event.target.closest('[data-select-connection]');
  if (connection) {
    state.selectedConnection = Number(connection.dataset.selectConnection);
    state.dirty.connections = false;
    render();
    return;
  }

  const request = event.target.closest('[data-select-request]');
  if (request) {
    state.selectedRequestId = request.dataset.selectRequest;
    render();
  }
});

app.addEventListener('submit', async (event) => {
  event.preventDefault();
  if (event.target.id === 'connection-form') {
    const result = await SaveConnection(collectConnectionForm());
    state.dirty.connections = false;
    showToast(result.message);
    await refreshData({ silent: true });
  }
  if (event.target.id === 'mcp-form') {
    const result = await SaveMCPSettings(collectMCPForm());
    state.dirty.settings = false;
    showToast(result.message);
    await refreshData({ silent: true });
  }
  if (event.target.id === 'passphrase-form') {
    const data = Object.fromEntries(new FormData(event.target));
    const connectionToTest = state.passphrase.connection || collectConnectionForm();
    const result = await TestConnectionConfig(connectionToTest, field(data, 'passphrase', state.passphrase.value));
    state.connectionTest = {
      ok: result.ok,
      message: result.message,
      target: `${connectionToTest.user}@${connectionToTest.host}:${connectionToTest.port}`,
    };
    state.passphrase = { open: false, connectionName: '', value: '', connection: null };
    showToast(result.message);
    await refreshData({ silent: true });
  }
});

app.addEventListener('input', (event) => {
  if (event.target.closest('#passphrase-form') && event.target.name === 'passphrase') {
    state.passphrase.value = event.target.value;
    return;
  }
  if (event.target.closest('#connection-form')) state.dirty.connections = true;
  if (event.target.closest('#mcp-form')) state.dirty.settings = true;
});

app.addEventListener('change', (event) => {
  const action = event.target.closest('[data-action]')?.dataset.action;
  if (action === 'terminal-select-connection') {
    // The selector is disabled while connected, so this only fires before connecting.
    state.selectedConnection = Number(event.target.value);
    render();
    return;
  }
  if (action === 'connection-type') {
    // Preserve current edits, switch type, and re-render the matching fields.
    const form = collectConnectionForm();
    form.type = event.target.value;
    state.data.connections[state.selectedConnection] = { ...state.data.connections[state.selectedConnection], ...form };
    state.dirty.connections = true;
    if (form.type === 'Serial' && (state.serialPorts ?? []).length === 0) {
      refreshSerialPorts().then(render);
    } else {
      render();
    }
    return;
  }
  if (event.target.closest('#connection-form')) state.dirty.connections = true;
  if (event.target.closest('#mcp-form')) state.dirty.settings = true;
});

refreshData();
window.setInterval(() => refreshData({ silent: true }), 5000);
