// The "Agents" section of the settings page (design §7 phase 3): per-agent
// MCP tokens and approval-bypass grants.
//
// The section is built with DOM calls only (no innerHTML), because agent
// names, previews and connection IDs come from MCP clients and users. It is
// mounted into a placeholder that main.js draws inside #page-root, and is
// rebuilt from the module state below on every mount, so a settings re-render
// keeps the issued token and the half-typed agent name.
//
// The secret returned by IssueAgentToken lives only in `agents.issued` and in
// the DOM of the open section. Closing it (or leaving the settings page via
// forgetIssuedAgentToken) drops it for good; the backend keeps only a hash.

import {
  IssueAgentToken,
  ListAgentApprovalBypasses,
  ListAgentTokens,
  RevokeAgentApprovalBypass,
  RevokeAgentToken,
  RevokeAllAgentApprovalBypasses,
  SetAgentBypassMaxRisk,
} from '../wailsjs/go/main/App';

export const AGENTS_I18N = {
  ja: {
    'agents.title': 'エージェント',
    'agents.subtitle': 'エージェントごとのトークンと承認省略を管理します。承認省略は、そのエージェント専用のトークンで認証した要求にだけ効きます。',
    'agents.reload': '再読み込み',
    'agents.loading': '読み込み中…',
    'agents.loadFailed': 'エージェントの情報を読み込めませんでした: {message}',
    'agents.issue.title': 'トークンを発行',
    'agents.issue.label': 'エージェント名',
    'agents.issue.placeholder': '例: claude-code',
    'agents.issue.help': 'MCP の agent_name と同じ名前にします。発行したトークンで認証すると、要求はこの名前に固定されます。',
    'agents.issue.submit': '発行',
    'agents.issue.empty': 'エージェント名を入力してください',
    'agents.issued.title': '「{name}」のトークン',
    'agents.issued.once': 'このトークンは今だけ表示します。閉じるか設定画面を離れると、二度と表示できません（保存されるのはハッシュだけです）。',
    'agents.issued.copy': 'トークンをコピー',
    'agents.issued.close': '閉じる（再表示できません）',
    'agents.tokens.title': '発行済みのトークン',
    'agents.tokens.empty': 'まだトークンを発行していません。',
    'agents.tokens.agent': 'エージェント',
    'agents.tokens.preview': 'トークン',
    'agents.tokens.createdAt': '発行日時',
    'agents.tokens.bypass': '承認省略',
    'agents.tokens.bypassYes': '使える',
    'agents.tokens.bypassNo': '使えない',
    'agents.tokens.action': '操作',
    'agents.tokens.reissue': '再発行',
    'agents.tokens.revoke': '失効',
    'agents.bypass.title': '承認省略',
    'agents.bypass.empty': '承認省略を許可しているエージェントはありません。',
    'agents.bypass.agent': 'エージェント',
    'agents.bypass.maxRisk': 'リスク上限',
    'agents.bypass.scope': '対象の接続先',
    'agents.bypass.scopeAll': 'すべての接続先',
    'agents.bypass.scopeUnknown': '{id}（削除済み）',
    'agents.bypass.grantedAt': '許可した日時',
    'agents.bypass.expiresAt': '期限',
    'agents.bypass.remaining': '残り {time}',
    'agents.bypass.status': '状態',
    'agents.bypass.active': '有効',
    'agents.bypass.noToken': 'トークン未発行のため無効',
    'agents.bypass.expired': '期限切れ',
    'agents.bypass.inactive': '無効',
    'agents.bypass.revoke': '取り消す',
    'agents.bypass.revokeAll': 'すべて取り消す',
    'agents.risk.Low': 'Low まで',
    'agents.risk.Medium': 'Medium まで',
    'agents.risk.High': 'High まで（強制）',
    'agents.confirm.forceHigh.title': 'High も承認なしで実行しますか？',
    'agents.confirm.forceHigh.body': '「{name}」のコマンドは、削除や再起動を含む High のものまで承認なしで実行されます。sudo・本番・書き込みの承認必須設定も、このエージェントには効かなくなります。期限と対象の接続先はそのままです。',
    'agents.confirm.forceHigh.ok': 'High まで許可する',
    'agents.config.title': '~/.claude.json の設定例',
    'agents.config.help': 'mcpServers に追加します。トークンは発行直後の表示中だけ、例に入ります。',
    'agents.config.placeholder': '<エージェント用トークン>',
    'agents.config.copy': '設定例をコピー',
    'agents.copied': 'コピーしました',
    'agents.confirm.cancel': 'キャンセル',
    'agents.confirm.close': '閉じる',
    'agents.confirm.reissue.title': 'トークンを再発行しますか？',
    'agents.confirm.reissue.body': '「{name}」の今のトークンはすぐに使えなくなります。このトークンを設定しているエージェントは、新しいトークンに差し替えるまで使えません。',
    'agents.confirm.reissue.ok': '再発行する',
    'agents.confirm.revokeToken.title': 'トークンを失効させますか？',
    'agents.confirm.revokeToken.body': '「{name}」のトークンを失効させます。このトークンを使うエージェントは ssh-gete を使えなくなり、承認省略も取り消されます。元には戻せません。',
    'agents.confirm.revokeToken.ok': '失効させる',
    'agents.confirm.revokeBypass.title': '承認省略を取り消しますか？',
    'agents.confirm.revokeBypass.body': '「{name}」の承認省略を取り消します。以後、このエージェントのコマンドはすべて承認待ちに入ります。',
    'agents.confirm.revokeBypass.ok': '取り消す',
    'agents.confirm.revokeAll.title': 'すべての承認省略を取り消しますか？',
    'agents.confirm.revokeAll.body': '{n} 件のエージェントの承認省略を取り消します。以後、どのエージェントのコマンドも承認待ちに入ります。',
    'agents.confirm.revokeAll.ok': 'すべて取り消す',
    'agents.time.hm': '{h}時間{m}分',
    'agents.time.m': '{m}分',
  },
  en: {
    'agents.title': 'Agents',
    'agents.subtitle': 'Per-agent tokens and approval bypass. A bypass only applies to requests authenticated with that agent\'s own token.',
    'agents.reload': 'Reload',
    'agents.loading': 'Loading…',
    'agents.loadFailed': 'Could not load agents: {message}',
    'agents.issue.title': 'Issue a token',
    'agents.issue.label': 'Agent name',
    'agents.issue.placeholder': 'e.g. claude-code',
    'agents.issue.help': 'Use the same name as the MCP agent_name. Requests authenticated with the token are bound to this name.',
    'agents.issue.submit': 'Issue',
    'agents.issue.empty': 'Enter an agent name',
    'agents.issued.title': 'Token for "{name}"',
    'agents.issued.once': 'This token is shown only now. Once you close it or leave Settings it cannot be shown again (only a hash is stored).',
    'agents.issued.copy': 'Copy token',
    'agents.issued.close': 'Close (cannot be shown again)',
    'agents.tokens.title': 'Issued tokens',
    'agents.tokens.empty': 'No tokens issued yet.',
    'agents.tokens.agent': 'Agent',
    'agents.tokens.preview': 'Token',
    'agents.tokens.createdAt': 'Issued',
    'agents.tokens.bypass': 'Approval bypass',
    'agents.tokens.bypassYes': 'Allowed',
    'agents.tokens.bypassNo': 'Not allowed',
    'agents.tokens.action': 'Actions',
    'agents.tokens.reissue': 'Reissue',
    'agents.tokens.revoke': 'Revoke',
    'agents.bypass.title': 'Approval bypass',
    'agents.bypass.empty': 'No agent has approval bypass.',
    'agents.bypass.agent': 'Agent',
    'agents.bypass.maxRisk': 'Max risk',
    'agents.bypass.scope': 'Connections',
    'agents.bypass.scopeAll': 'All connections',
    'agents.bypass.scopeUnknown': '{id} (deleted)',
    'agents.bypass.grantedAt': 'Granted',
    'agents.bypass.expiresAt': 'Expires',
    'agents.bypass.remaining': '{time} left',
    'agents.bypass.status': 'Status',
    'agents.bypass.active': 'Active',
    'agents.bypass.noToken': 'Inactive (no token issued)',
    'agents.bypass.expired': 'Expired',
    'agents.bypass.inactive': 'Inactive',
    'agents.bypass.revoke': 'Revoke',
    'agents.bypass.revokeAll': 'Revoke all',
    'agents.risk.Low': 'Up to Low',
    'agents.risk.Medium': 'Up to Medium',
    'agents.risk.High': 'Up to High (forced)',
    'agents.confirm.forceHigh.title': 'Run High commands without approval?',
    'agents.confirm.forceHigh.body': 'Commands from "{name}" will run without approval, High ones (deletes, reboots, ...) included. The sudo, production and write approval rules no longer apply to this agent. The expiry and connection scope stay as they are.',
    'agents.confirm.forceHigh.ok': 'Allow up to High',
    'agents.config.title': '~/.claude.json example',
    'agents.config.help': 'Add this under mcpServers. The token is filled in only while it is being shown right after issuing.',
    'agents.config.placeholder': '<agent token>',
    'agents.config.copy': 'Copy example',
    'agents.copied': 'Copied',
    'agents.confirm.cancel': 'Cancel',
    'agents.confirm.close': 'Close',
    'agents.confirm.reissue.title': 'Reissue the token?',
    'agents.confirm.reissue.body': 'The current token for "{name}" stops working immediately. Agents configured with it cannot use ssh-gete until they use the new one.',
    'agents.confirm.reissue.ok': 'Reissue',
    'agents.confirm.revokeToken.title': 'Revoke the token?',
    'agents.confirm.revokeToken.body': 'The token for "{name}" will be revoked. Agents using it can no longer use ssh-gete, and its approval bypass is revoked too. This cannot be undone.',
    'agents.confirm.revokeToken.ok': 'Revoke',
    'agents.confirm.revokeBypass.title': 'Revoke approval bypass?',
    'agents.confirm.revokeBypass.body': 'Approval bypass for "{name}" will be revoked. Every command from this agent will wait for approval again.',
    'agents.confirm.revokeBypass.ok': 'Revoke',
    'agents.confirm.revokeAll.title': 'Revoke every approval bypass?',
    'agents.confirm.revokeAll.body': 'Approval bypass will be revoked for {n} agent(s). Every agent\'s commands will wait for approval again.',
    'agents.confirm.revokeAll.ok': 'Revoke all',
    'agents.time.hm': '{h}h {m}m',
    'agents.time.m': '{m}m',
  },
};

// The mcpServers key written to ~/.claude.json; it matches main.js agentConfig.
export const MCP_SERVER_KEY = 'ssh-gete';

// A mount within this window reuses the lists instead of fetching again, so
// a settings re-render right after an action does not refetch twice.
const RELOAD_AFTER_MS = 4000;

const agents = {
  tokens: [],     // AgentToken[] (no secrets)
  bypasses: [],   // AgentPolicy[] with approvalBypass
  loaded: false,
  loadedAt: 0,
  loading: false,
  error: '',
  loadSeq: 0,     // a load started before the latest action is discarded
  busy: '',       // the data-action running now; buttons are disabled meanwhile
  draftName: '',
  inputError: '',
  issued: null,   // { agentName, token } while the one-time token is shown
};

let mountedRoot = null;
let mountedCtx = null;

// ------------------------------------------------------------------ helpers

function formatTimestamp(value) {
  if (!value) return '-';
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return String(value);
  const pad = (n) => String(n).padStart(2, '0');
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())} ${pad(date.getHours())}:${pad(date.getMinutes())}`;
}

// formatRemaining renders the time left until expiresAt, or '' when it is
// unknown or already past.
export function formatRemaining(expiresAt, t, now = Date.now()) {
  const ms = Date.parse(expiresAt ?? '');
  if (Number.isNaN(ms) || ms <= now) return '';
  const minutes = Math.max(1, Math.floor((ms - now) / 60000));
  const h = Math.floor(minutes / 60);
  const m = minutes % 60;
  return h > 0 ? t('agents.time.hm', { h, m }) : t('agents.time.m', { m });
}

// bypassStatus classifies one grant for display. A grant without a token
// never takes effect, which is the most useful thing to tell the user.
export function bypassStatus(policy, now = Date.now()) {
  if (!policy?.hasToken) return 'noToken';
  if (policy.active) return 'active';
  const expires = Date.parse(policy.expiresAt ?? '');
  if (!Number.isNaN(expires) && expires <= now) return 'expired';
  return 'inactive';
}

// scopeLabels names the connections a grant is limited to, in the order the
// grant lists them. IDs that no longer exist stay visible as deleted.
export function scopeLabels(scopeConnIds, connections, t) {
  const ids = (scopeConnIds ?? []).filter(Boolean);
  if (!ids.length) return [t('agents.bypass.scopeAll')];
  const names = new Map((connections ?? []).map((conn) => [conn.id, conn.name || conn.id]));
  return ids.map((id) => (names.has(id) ? names.get(id) : t('agents.bypass.scopeUnknown', { id })));
}

export function mcpEndpoint(mcp) {
  const settings = mcp ?? {};
  return settings.localURL || `http://127.0.0.1:${settings.port || 8787}${settings.mcpPath || '/mcp'}`;
}

// claudeConfigExample is the ~/.claude.json snippet for one agent. The token
// goes in only when the caller has it (while it is on screen); otherwise a
// placeholder keeps the shape.
export function claudeConfigExample(mcp, token, t) {
  return mcpConfigJSON(mcp, `Bearer ${token || t('agents.config.placeholder')}`);
}

// mcpConfigJSON is the one shape of the ~/.claude.json snippet; the settings
// page's shared-Bearer example and the per-agent example both use it, so the
// two cannot drift. An empty authorization leaves headers out.
export function mcpConfigJSON(mcp, authorization) {
  const server = { type: 'http', url: mcpEndpoint(mcp) };
  if (authorization) server.headers = { Authorization: authorization };
  return JSON.stringify({ mcpServers: { [MCP_SERVER_KEY]: server } }, null, 2);
}

function settingsMcp(ctx) {
  return ctx?.state?.settingsMcp ?? ctx?.state?.data?.mcp ?? {};
}

// el builds an element. String children become text nodes, so nothing passed
// here is ever parsed as HTML.
function el(tag, attrs = {}, ...children) {
  const node = document.createElement(tag);
  for (const [name, value] of Object.entries(attrs)) {
    if (value == null || value === false) continue;
    if (name === 'className') node.className = value;
    else if (name === 'value') node.value = value;
    else node.setAttribute(name, value === true ? '' : String(value));
  }
  for (const child of children.flat()) {
    if (child == null || child === false) continue;
    node.append(typeof child === 'string' || typeof child === 'number' ? document.createTextNode(String(child)) : child);
  }
  return node;
}

function icon(name, extra = 'me-1') {
  return el('i', { className: `bi ${name} ${extra}`.trim(), 'aria-hidden': 'true' });
}

function actionButton(action, label, { className = 'btn-outline-secondary btn-sm', iconName = '', data = {}, type = 'button' } = {}) {
  const attrs = { className: `btn ${className}`, type, 'data-action': action, disabled: Boolean(agents.busy) };
  for (const [key, value] of Object.entries(data)) attrs[`data-${key}`] = value;
  return el('button', attrs, iconName ? icon(iconName) : null, label);
}

async function copyText(text, ctx) {
  const value = String(text ?? '');
  if (!value) return;
  try {
    await navigator.clipboard.writeText(value);
  } catch {
    const textarea = el('textarea', { readonly: true });
    textarea.value = value;
    textarea.style.position = 'fixed';
    textarea.style.opacity = '0';
    document.body.appendChild(textarea);
    textarea.select();
    try {
      document.execCommand('copy');
    } finally {
      textarea.remove();
    }
  }
  ctx.toast(ctx.t('agents.copied'));
}

// ------------------------------------------------------------------ render

function renderIssued(ctx) {
  const { t } = ctx;
  const { agentName, token } = agents.issued;
  const tokenInput = el('input', {
    className: 'form-control font-monospace',
    id: 'agents-issued-token',
    readonly: true,
    autocomplete: 'off',
    spellcheck: 'false',
    'aria-label': t('agents.issued.title', { name: agentName }),
    value: token,
  });
  return el('div', { className: 'alert alert-success agents-issued', role: 'status' },
    el('div', { className: 'fw-bold mb-1 agents-issued-title' }, t('agents.issued.title', { name: agentName })),
    el('div', { className: 'small mb-2' }, t('agents.issued.once')),
    el('div', { className: 'input-group' },
      tokenInput,
      actionButton('agents-copy-token', t('agents.issued.copy'), { className: 'btn-outline-secondary', iconName: 'bi-copy' }),
    ),
    el('div', { className: 'mt-2 text-end' },
      actionButton('agents-hide-token', t('agents.issued.close'), { className: 'btn-outline-danger btn-sm', iconName: 'bi-eye-slash' }),
    ),
  );
}

function renderIssueForm(ctx) {
  const { t } = ctx;
  const input = el('input', {
    className: `form-control${agents.inputError ? ' is-invalid' : ''}`,
    id: 'agents-issue-name',
    name: 'agentName',
    type: 'text',
    placeholder: t('agents.issue.placeholder'),
    autocomplete: 'off',
    autocapitalize: 'none',
    autocorrect: 'off',
    spellcheck: 'false',
    'aria-describedby': 'agents-issue-help',
    value: agents.draftName,
  });
  return el('form', { className: 'agents-issue-form', 'data-agents-form': 'issue', novalidate: true },
    el('label', { className: 'form-label fw-bold', for: 'agents-issue-name' }, t('agents.issue.label')),
    el('div', { className: 'input-group has-validation' },
      input,
      el('button', { className: 'btn btn-primary', type: 'submit', 'data-action': 'agents-issue', disabled: Boolean(agents.busy) },
        icon('bi-key'), t('agents.issue.submit')),
      agents.inputError ? el('div', { className: 'invalid-feedback' }, agents.inputError) : null,
    ),
    el('div', { className: 'form-text', id: 'agents-issue-help' }, t('agents.issue.help')),
  );
}

function renderTokens(ctx) {
  const { t } = ctx;
  if (!agents.tokens.length) return el('p', { className: 'text-secondary mb-0 agents-tokens-empty' }, t('agents.tokens.empty'));
  return el('div', { className: 'table-responsive' },
    el('table', { className: 'table table-sm align-middle mb-0 agents-tokens' },
      el('thead', {}, el('tr', {},
        el('th', {}, t('agents.tokens.agent')),
        el('th', {}, t('agents.tokens.preview')),
        el('th', {}, t('agents.tokens.createdAt')),
        el('th', {}, t('agents.tokens.bypass')),
        el('th', { className: 'text-end' }, t('agents.tokens.action')),
      )),
      el('tbody', {}, agents.tokens.map((token) => el('tr', { 'data-agent-row': token.agentName },
        el('td', { className: 'fw-bold' }, token.agentName),
        el('td', {}, el('code', {}, token.preview || '-')),
        el('td', { className: 'text-nowrap' }, formatTimestamp(token.createdAt)),
        el('td', {}, token.bypass ? t('agents.tokens.bypassYes') : t('agents.tokens.bypassNo')),
        el('td', { className: 'text-end text-nowrap' },
          actionButton('agents-reissue', t('agents.tokens.reissue'), { iconName: 'bi-arrow-repeat', data: { 'agent-name': token.agentName } }),
          ' ',
          actionButton('agents-revoke-token', t('agents.tokens.revoke'), { className: 'btn-outline-danger btn-sm', iconName: 'bi-x-octagon', data: { 'agent-name': token.agentName } }),
        ),
      ))),
    ),
  );
}

const KNOWN_RISKS = new Set(['Low', 'Medium', 'High']);
const STATUS_BADGE = { active: 'text-bg-success', noToken: 'text-bg-warning', expired: 'text-bg-secondary', inactive: 'text-bg-secondary' };

function renderBypasses(ctx, now) {
  const { t } = ctx;
  if (!agents.bypasses.length) return el('p', { className: 'text-secondary mb-0 agents-bypass-empty' }, t('agents.bypass.empty'));
  const connections = ctx.state?.data?.connections ?? [];
  return el('div', { className: 'table-responsive' },
    el('table', { className: 'table table-sm align-middle mb-0 agents-bypasses' },
      el('thead', {}, el('tr', {},
        el('th', {}, t('agents.bypass.agent')),
        el('th', {}, t('agents.bypass.status')),
        el('th', {}, t('agents.bypass.maxRisk')),
        el('th', {}, t('agents.bypass.scope')),
        el('th', {}, t('agents.bypass.grantedAt')),
        el('th', {}, t('agents.bypass.expiresAt')),
        el('th', { className: 'text-end' }, ''),
      )),
      el('tbody', {}, agents.bypasses.map((policy) => {
        const status = bypassStatus(policy, now);
        const remaining = status === 'active' ? formatRemaining(policy.expiresAt, t, now) : '';
        const risk = policy.maxRisk || 'Medium';
        return el('tr', { 'data-bypass-row': policy.agentName },
          el('td', { className: 'fw-bold' }, policy.agentName),
          el('td', {}, el('span', { className: `badge ${STATUS_BADGE[status]} agents-bypass-status`, 'data-status': status }, t(`agents.bypass.${status}`))),
          el('td', {}, maxRiskSelect(policy.agentName, risk, t)),
          el('td', {}, el('ul', { className: 'list-unstyled mb-0 agents-scope' },
            scopeLabels(policy.scopeConnIds, connections, t).map((label) => el('li', {}, label)))),
          el('td', { className: 'text-nowrap' }, formatTimestamp(policy.grantedAt)),
          el('td', { className: 'text-nowrap' },
            formatTimestamp(policy.expiresAt),
            remaining ? el('div', { className: 'small text-secondary' }, t('agents.bypass.remaining', { time: remaining })) : null),
          el('td', { className: 'text-end' },
            actionButton('agents-revoke-bypass', t('agents.bypass.revoke'), { className: 'btn-outline-danger btn-sm', data: { 'agent-name': policy.agentName } })),
        );
      })),
    ),
  );
}

// maxRiskSelect lets the human change the grant's max risk; High is the
// forced bypass and asks for confirmation (setMaxRisk).
function maxRiskSelect(agentName, risk, t) {
  const options = [...KNOWN_RISKS].map((value) => el('option', { value, selected: value === risk }, t(`agents.risk.${value}`)));
  if (!KNOWN_RISKS.has(risk)) options.push(el('option', { value: risk, selected: true }, risk));
  return el('select', {
    className: `form-select form-select-sm agents-max-risk${risk === 'High' ? ' border-danger text-danger' : ''}`,
    'data-agents-max-risk': agentName,
    'aria-label': t('agents.bypass.maxRisk'),
    disabled: Boolean(agents.busy),
  }, ...options);
}

function renderConfig(ctx) {
  const { t } = ctx;
  const config = claudeConfigExample(settingsMcp(ctx), agents.issued?.token ?? '', t);
  return el('div', { className: 'agents-config' },
    el('div', { className: 'd-flex align-items-center justify-content-between gap-2 mb-1' },
      el('span', { className: 'fw-bold' }, t('agents.config.title')),
      actionButton('agents-copy-config', t('agents.config.copy'), { iconName: 'bi-copy' }),
    ),
    el('div', { className: 'form-text mt-0 mb-2' }, t('agents.config.help')),
    el('pre', { className: 'command-block mb-0' }, el('code', { id: 'agents-config-code' }, config)),
  );
}

// buildAgentsSection returns the section's root element for the current
// module state. Exported for tests; main.js uses mountAgentsSection.
export function buildAgentsSection(ctx, { now = Date.now() } = {}) {
  const { t } = ctx;
  const header = el('div', { className: 'card-header' },
    el('div', { className: 'd-flex align-items-start justify-content-between gap-3' },
      el('div', {},
        el('h3', { className: 'card-title fw-bold mb-1', id: 'agents-section-title' }, t('agents.title')),
        el('div', { className: 'text-secondary small' }, t('agents.subtitle')),
      ),
      actionButton('agents-reload', t('agents.reload'), { iconName: 'bi-arrow-clockwise' }),
    ),
  );
  const body = el('div', { className: 'card-body' });
  if (agents.error) body.append(el('div', { className: 'alert alert-danger agents-error' }, agents.error));
  if (agents.issued) body.append(renderIssued(ctx));
  body.append(
    el('h4', { className: 'h6 fw-bold mt-1' }, t('agents.issue.title')),
    renderIssueForm(ctx),
    el('hr'),
    el('h4', { className: 'h6 fw-bold' }, t('agents.tokens.title')),
    !agents.loaded && agents.loading ? el('p', { className: 'text-secondary mb-0' }, t('agents.loading')) : renderTokens(ctx),
    el('hr'),
    el('div', { className: 'd-flex align-items-center justify-content-between gap-2 mb-2' },
      el('h4', { className: 'h6 fw-bold mb-0' }, t('agents.bypass.title')),
      agents.bypasses.length
        ? actionButton('agents-revoke-all', t('agents.bypass.revokeAll'), { className: 'btn-outline-danger btn-sm', iconName: 'bi-shield-x' })
        : null,
    ),
    !agents.loaded && agents.loading ? el('p', { className: 'text-secondary mb-0' }, t('agents.loading')) : renderBypasses(ctx, now),
    el('hr'),
    renderConfig(ctx),
  );
  return el('section', { className: 'card card-outline card-warning agents-section', id: 'agents-section', 'aria-labelledby': 'agents-section-title' },
    header, body);
}

function rerender() {
  if (!mountedRoot || !mountedRoot.isConnected || !mountedCtx) return;
  const active = document.activeElement;
  const refocus = active && mountedRoot.contains(active) && active.id ? active.id : '';
  const selection = refocus && typeof active.selectionStart === 'number' ? [active.selectionStart, active.selectionEnd] : null;
  mountedRoot.replaceChildren(buildAgentsSection(mountedCtx));
  if (refocus) {
    const next = mountedRoot.querySelector(`#${refocus}`);
    next?.focus();
    if (selection && typeof next?.setSelectionRange === 'function') next.setSelectionRange(...selection);
  }
}

// mountAgentsSection draws the section into root (a placeholder main.js puts
// on the settings page) and fetches the lists unless they are fresh.
export function mountAgentsSection(root, ctx) {
  if (!root) return;
  mountedRoot = root;
  mountedCtx = ctx;
  rerender();
  if (!agents.loading && (!agents.loaded || Date.now() - agents.loadedAt > RELOAD_AFTER_MS)) {
    loadAgents();
  }
}

// ------------------------------------------------------------------ data

// loadAgents fetches both lists. A load that started before a later load or
// action is dropped, so it cannot bring back a revoked row.
export async function loadAgents() {
  const seq = ++agents.loadSeq;
  agents.loading = true;
  rerender();
  try {
    const [tokens, bypasses] = await Promise.all([ListAgentTokens(), ListAgentApprovalBypasses()]);
    if (seq !== agents.loadSeq) return;
    agents.tokens = Array.isArray(tokens) ? tokens : [];
    agents.bypasses = (Array.isArray(bypasses) ? bypasses : []).filter((policy) => policy?.approvalBypass !== false);
    agents.loaded = true;
    agents.loadedAt = Date.now();
    agents.error = '';
  } catch (error) {
    if (seq !== agents.loadSeq) return;
    agents.error = mountedCtx ? mountedCtx.t('agents.loadFailed', { message: String(error?.message ?? error) }) : String(error);
  } finally {
    if (seq === agents.loadSeq) {
      agents.loading = false;
      rerender();
    }
  }
}

// ------------------------------------------------------------------ dialog

let dialogChain = Promise.resolve();

function dialogRoot() {
  let root = document.querySelector('#agents-modal-root');
  if (!root) {
    root = el('div', { id: 'agents-modal-root' });
    document.body.appendChild(root);
  }
  return root;
}

// confirmDialog shows a modal outside #app (page re-renders and main.js's
// click handler never reach it) and resolves true only for the OK button.
// Close, Cancel, the backdrop and Escape resolve false.
export function confirmDialog(ctx, { title, body, okLabel }) {
  const { t } = ctx;
  const run = dialogChain.then(() => new Promise((resolve) => {
    const previousFocus = document.activeElement;
    const titleId = 'agents-modal-title';
    const cancel = el('button', { className: 'btn btn-outline-secondary', type: 'button', 'data-agents-dialog': 'cancel' }, t('agents.confirm.cancel'));
    const ok = el('button', { className: 'btn btn-danger', type: 'button', 'data-agents-dialog': 'ok' }, okLabel);
    const modal = el('div', { className: 'modal d-block', tabindex: '-1', role: 'dialog', 'aria-modal': 'true', 'aria-labelledby': titleId, 'data-agents-dialog': 'backdrop' },
      el('div', { className: 'modal-dialog modal-dialog-centered' },
        el('div', { className: 'modal-content' },
          el('div', { className: 'modal-header' },
            el('h5', { className: 'modal-title fw-bold', id: titleId }, title),
            el('button', { className: 'btn-close', type: 'button', 'data-agents-dialog': 'close', 'aria-label': t('agents.confirm.close') }),
          ),
          el('div', { className: 'modal-body' }, el('p', { className: 'mb-0' }, body)),
          el('div', { className: 'modal-footer' }, cancel, ok),
        ),
      ),
    );
    const host = el('div', {}, el('div', { className: 'modal-backdrop show' }), modal);
    dialogRoot().appendChild(host);
    let done = false;
    const finish = (value) => {
      if (done) return;
      done = true;
      host.remove();
      if (previousFocus && previousFocus.isConnected && typeof previousFocus.focus === 'function') previousFocus.focus();
      resolve(value);
    };
    host.addEventListener('click', (event) => {
      const target = event.target.closest('[data-agents-dialog]');
      if (!target) return;
      const kind = target.dataset.agentsDialog;
      if (kind === 'backdrop' && event.target !== target) return;
      finish(kind === 'ok');
    });
    host.addEventListener('keydown', (event) => {
      if (event.key === 'Escape') {
        event.preventDefault();
        finish(false);
      }
    });
    // Destructive dialogs start on Cancel so Enter does not confirm by accident.
    cancel.focus();
  }));
  dialogChain = run.catch(() => {});
  return run;
}

// ------------------------------------------------------------------ actions

function knownToken(agentName) {
  return agents.tokens.some((token) => token.agentName === agentName);
}

// runAction marks the section busy, calls the backend, toasts its message,
// and reloads both the lists and main.js's data (agentPolicies feeds the
// approval screen).
async function runAction(ctx, action, call) {
  agents.busy = action;
  agents.loadSeq += 1;
  rerender();
  ctx.bumpGen();
  let result;
  try {
    result = await call();
  } catch (error) {
    result = { ok: false, message: String(error?.message ?? error) };
  } finally {
    agents.busy = '';
  }
  const message = ctx.resultMessage ? ctx.resultMessage(result) : String(result?.message ?? '');
  if (message) ctx.toast(message);
  await loadAgents();
  await ctx.refresh({ silent: true });
  return result ?? { ok: false, message: '' };
}

async function issueToken(ctx, rawName) {
  const { t } = ctx;
  const agentName = String(rawName ?? '').trim();
  if (!agentName) {
    agents.inputError = t('agents.issue.empty');
    rerender();
    mountedRoot?.querySelector('#agents-issue-name')?.focus();
    return false;
  }
  agents.inputError = '';
  if (knownToken(agentName)) {
    const ok = await confirmDialog(ctx, {
      title: t('agents.confirm.reissue.title'),
      body: t('agents.confirm.reissue.body', { name: agentName }),
      okLabel: t('agents.confirm.reissue.ok'),
    });
    if (!ok) return false;
  }
  // The previous token (if shown) is now dead or for another agent.
  agents.issued = null;
  const result = await runAction(ctx, 'agents-issue', () => IssueAgentToken(agentName));
  if (result.ok && result.token) {
    agents.issued = { agentName, token: result.token };
    if (agents.draftName.trim() === agentName) agents.draftName = '';
    rerender();
    mountedRoot?.querySelector('#agents-issued-token')?.select?.();
  }
  return Boolean(result.ok);
}

async function revokeToken(ctx, agentName) {
  const { t } = ctx;
  const ok = await confirmDialog(ctx, {
    title: t('agents.confirm.revokeToken.title'),
    body: t('agents.confirm.revokeToken.body', { name: agentName }),
    okLabel: t('agents.confirm.revokeToken.ok'),
  });
  if (!ok) return false;
  const result = await runAction(ctx, 'agents-revoke-token', () => RevokeAgentToken(agentName));
  if (result.ok && agents.issued?.agentName === agentName) {
    agents.issued = null;
    rerender();
  }
  return Boolean(result.ok);
}

async function revokeBypass(ctx, agentName) {
  const { t } = ctx;
  const ok = await confirmDialog(ctx, {
    title: t('agents.confirm.revokeBypass.title'),
    body: t('agents.confirm.revokeBypass.body', { name: agentName }),
    okLabel: t('agents.confirm.revokeBypass.ok'),
  });
  if (!ok) return false;
  const result = await runAction(ctx, 'agents-revoke-bypass', () => RevokeAgentApprovalBypass(agentName));
  return Boolean(result.ok);
}

async function setMaxRisk(ctx, agentName, risk) {
  const { t } = ctx;
  if (risk === 'High') {
    const ok = await confirmDialog(ctx, {
      title: t('agents.confirm.forceHigh.title'),
      body: t('agents.confirm.forceHigh.body', { name: agentName }),
      okLabel: t('agents.confirm.forceHigh.ok'),
    });
    if (!ok) {
      rerender(); // put the select back to the stored value
      return false;
    }
  }
  const result = await runAction(ctx, 'agents-max-risk', () => SetAgentBypassMaxRisk(agentName, risk));
  return Boolean(result.ok);
}

async function revokeAllBypasses(ctx) {
  const { t } = ctx;
  const ok = await confirmDialog(ctx, {
    title: t('agents.confirm.revokeAll.title'),
    body: t('agents.confirm.revokeAll.body', { n: agents.bypasses.length }),
    okLabel: t('agents.confirm.revokeAll.ok'),
  });
  if (!ok) return false;
  const result = await runAction(ctx, 'agents-revoke-all', () => RevokeAllAgentApprovalBypasses());
  return Boolean(result.ok);
}

// forgetIssuedAgentToken drops the one-time token. main.js calls it when the
// user leaves the settings page; the close button calls it too.
export function forgetIssuedAgentToken() {
  if (!agents.issued) return;
  agents.issued = null;
  rerender();
}

// agentsSectionBusy is true while the page should not be redrawn under the
// user: an action is running, or the name field has focus or unsent text.
export function agentsSectionBusy() {
  if (agents.busy) return true;
  if (agents.draftName.trim()) return true;
  const active = document.activeElement;
  return Boolean(active && active.id === 'agents-issue-name');
}

// handleAgentsClick handles agents-* actions. The issue button is a submit
// button and is left to handleAgentsSubmit.
export async function handleAgentsClick(event, ctx) {
  const target = event?.target?.closest?.('[data-action^="agents-"]');
  if (!target) return false;
  const action = target.dataset.action;
  const agentName = target.dataset.agentName ?? '';
  if (action === 'agents-issue') return true;
  if (agents.busy && action !== 'agents-copy-token' && action !== 'agents-copy-config' && action !== 'agents-hide-token') return true;
  if (action === 'agents-copy-token') {
    if (agents.issued) await copyText(agents.issued.token, ctx);
    return true;
  }
  if (action === 'agents-hide-token') {
    forgetIssuedAgentToken();
    return true;
  }
  if (action === 'agents-copy-config') {
    await copyText(claudeConfigExample(settingsMcp(ctx), agents.issued?.token ?? '', ctx.t), ctx);
    return true;
  }
  if (action === 'agents-reload') {
    await loadAgents();
    return true;
  }
  if (action === 'agents-reissue') {
    if (agentName) await issueToken(ctx, agentName);
    return true;
  }
  if (action === 'agents-revoke-token') {
    if (agentName) await revokeToken(ctx, agentName);
    return true;
  }
  if (action === 'agents-revoke-bypass') {
    if (agentName) await revokeBypass(ctx, agentName);
    return true;
  }
  if (action === 'agents-revoke-all') {
    await revokeAllBypasses(ctx);
    return true;
  }
  return false;
}

export async function handleAgentsSubmit(event, ctx) {
  const form = event?.target?.closest?.('[data-agents-form="issue"]');
  if (!form) return false;
  event.preventDefault?.();
  if (agents.busy) return true;
  const name = form.querySelector('#agents-issue-name')?.value ?? agents.draftName;
  agents.draftName = name;
  await issueToken(ctx, name);
  return true;
}

export async function handleAgentsChange(event, ctx) {
  const select = event?.target?.closest?.('[data-agents-max-risk]');
  if (!select) return false;
  if (agents.busy) return true;
  await setMaxRisk(ctx, select.dataset.agentsMaxRisk, select.value);
  return true;
}

export function handleAgentsInput(event) {
  if (event?.target?.id !== 'agents-issue-name') return false;
  agents.draftName = event.target.value;
  if (agents.inputError) {
    agents.inputError = '';
    event.target.classList.remove('is-invalid');
    event.target.parentElement?.querySelector('.invalid-feedback')?.remove();
  }
  return true;
}

// getAgentsState is for tests and the Integrator's checks; do not mutate.
export function getAgentsState() {
  return agents;
}

// resetAgentsState restores the initial module state (tests only).
export function resetAgentsState() {
  Object.assign(agents, {
    tokens: [], bypasses: [], loaded: false, loadedAt: 0, loading: false, error: '',
    loadSeq: 0, busy: '', draftName: '', inputError: '', issued: null,
  });
  mountedRoot = null;
  mountedCtx = null;
  dialogChain = Promise.resolve();
  document.querySelector('#agents-modal-root')?.remove();
}
