import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

vi.mock('../wailsjs/go/main/App', () => ({
  IssueAgentToken: vi.fn(),
  ListAgentApprovalBypasses: vi.fn(),
  ListAgentTokens: vi.fn(),
  RevokeAgentApprovalBypass: vi.fn(),
  RevokeAgentToken: vi.fn(),
  RevokeAllAgentApprovalBypasses: vi.fn(),
  SetAgentBypassMaxRisk: vi.fn(),
}));

import * as App from '../wailsjs/go/main/App';
import { MODEL_I18N, interpolate, mergeI18n } from './model.js';
import {
  AGENTS_I18N,
  agentsSectionBusy,
  buildAgentsSection,
  bypassStatus,
  claudeConfigExample,
  forgetIssuedAgentToken,
  formatRemaining,
  getAgentsState,
  handleAgentsChange,
  handleAgentsClick,
  handleAgentsInput,
  handleAgentsSubmit,
  loadAgents,
  mountAgentsSection,
  resetAgentsState,
  scopeLabels,
} from './agents.js';

const I18N = mergeI18n(AGENTS_I18N, MODEL_I18N);
const tFor = (lang) => (key, vars) => interpolate(I18N[lang][key] ?? key, vars);
const NOW = Date.parse('2026-10-28T10:00:00Z');
const SECRET = 'sga_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef';

const flush = async () => {
  for (let i = 0; i < 10; i += 1) await Promise.resolve();
};

function makeCtx(lang = 'ja') {
  return {
    t: tFor(lang),
    lang: () => lang,
    state: {
      data: {
        connections: [{ id: 'c-web', name: 'web-01' }, { id: 'c-db', name: 'db-01' }],
        mcp: { localURL: 'http://127.0.0.1:8787/mcp' },
      },
      settingsMcp: { localURL: 'http://127.0.0.1:8787/mcp' },
    },
    toast: vi.fn(),
    refresh: vi.fn(async () => {}),
    bumpGen: vi.fn(),
  };
}

function policy(agentName, extra = {}) {
  return {
    agentName, approvalBypass: true, maxRisk: 'Medium', scopeConnIds: [],
    grantedAt: '2026-10-28T09:00:00Z', expiresAt: '2026-10-28T17:00:00Z',
    active: true, hasToken: true, ...extra,
  };
}

function token(agentName, extra = {}) {
  return { agentName, bypass: true, createdAt: '2026-10-28T08:00:00Z', preview: 'sga_…abcd', ...extra };
}

function click(selector) {
  const target = document.querySelector(selector);
  if (!target) throw new Error(`no element ${selector}`);
  return { target };
}

// Answers the open confirm dialog with OK or Cancel.
async function answerDialog(ok) {
  await flush();
  const button = document.querySelector(`#agents-modal-root [data-agents-dialog="${ok ? 'ok' : 'cancel'}"]`);
  if (!button) throw new Error('no dialog');
  button.click();
}

async function mount(ctx, { tokens = [], bypasses = [] } = {}) {
  App.ListAgentTokens.mockResolvedValue(tokens);
  App.ListAgentApprovalBypasses.mockResolvedValue(bypasses);
  document.body.innerHTML = '<div id="agents-root"></div>';
  mountAgentsSection(document.querySelector('#agents-root'), ctx);
  await flush();
}

beforeEach(() => {
  vi.clearAllMocks();
  resetAgentsState();
  document.body.innerHTML = '';
  Object.defineProperty(navigator, 'clipboard', { value: { writeText: vi.fn(async () => {}) }, configurable: true });
});

afterEach(() => {
  resetAgentsState();
});

describe('AGENTS_I18N', () => {
  it('merges with the other dictionaries and keeps the agents. prefix', () => {
    expect(() => mergeI18n(AGENTS_I18N, MODEL_I18N)).not.toThrow();
    expect(Object.keys(AGENTS_I18N.ja).every((key) => key.startsWith('agents.'))).toBe(true);
    expect(Object.keys(AGENTS_I18N.ja).sort()).toEqual(Object.keys(AGENTS_I18N.en).sort());
  });
});

describe('pure helpers', () => {
  it('classifies grants', () => {
    expect(bypassStatus(policy('a'), NOW)).toBe('active');
    expect(bypassStatus(policy('a', { hasToken: false, active: false }), NOW)).toBe('noToken');
    expect(bypassStatus(policy('a', { active: false, expiresAt: '2026-10-28T09:30:00Z' }), NOW)).toBe('expired');
    expect(bypassStatus(policy('a', { active: false }), NOW)).toBe('inactive');
  });

  it('formats remaining time', () => {
    expect(formatRemaining('2026-10-28T13:05:00Z', tFor('ja'), NOW)).toBe('3時間5分');
    expect(formatRemaining('2026-10-28T10:20:00Z', tFor('en'), NOW)).toBe('20m');
    expect(formatRemaining('2026-10-28T09:00:00Z', tFor('en'), NOW)).toBe('');
    expect(formatRemaining('', tFor('en'), NOW)).toBe('');
  });

  it('names scope connections and keeps deleted IDs visible', () => {
    const conns = [{ id: 'c-web', name: 'web-01' }];
    expect(scopeLabels([], conns, tFor('ja'))).toEqual(['すべての接続先']);
    expect(scopeLabels(['c-web', 'c-gone'], conns, tFor('en'))).toEqual(['web-01', 'c-gone (deleted)']);
  });

  it('builds the ~/.claude.json example with a placeholder or the token', () => {
    const mcp = { port: 9000, mcpPath: '/mcp' };
    const placeholder = JSON.parse(claudeConfigExample(mcp, '', tFor('en')));
    expect(placeholder).toEqual({
      mcpServers: { 'ssh-gete': { type: 'http', url: 'http://127.0.0.1:9000/mcp', headers: { Authorization: 'Bearer <agent token>' } } },
    });
    const filled = JSON.parse(claudeConfigExample(mcp, SECRET, tFor('ja')));
    expect(filled.mcpServers['ssh-gete'].headers.Authorization).toBe(`Bearer ${SECRET}`);
  });
});

describe('rendering', () => {
  it('lists tokens and bypasses in JA and EN', async () => {
    for (const lang of ['ja', 'en']) {
      resetAgentsState();
      const ctx = makeCtx(lang);
      await mount(ctx, {
        tokens: [token('claude-code')],
        bypasses: [
          policy('claude-code', { scopeConnIds: ['c-web', 'c-gone'] }),
          policy('codex', { hasToken: false, active: false, maxRisk: 'Low' }),
        ],
      });
      const t = tFor(lang);
      expect(document.querySelector('#agents-section .card-title').textContent).toBe(t('agents.title'));
      expect(document.querySelector('[data-agent-row="claude-code"]').textContent).toContain('sga_…abcd');
      const rows = document.querySelectorAll('[data-bypass-row]');
      expect(rows).toHaveLength(2);
      expect(rows[0].querySelector('.agents-bypass-status').dataset.status).toBe('active');
      expect(rows[0].querySelectorAll('.agents-scope li')).toHaveLength(2);
      expect(rows[0].textContent).toContain('web-01');
      expect(rows[1].querySelector('.agents-bypass-status').textContent).toBe(t('agents.bypass.noToken'));
      expect(rows[1].textContent).toContain(t('agents.risk.Low'));
      expect(document.querySelector('[data-action="agents-revoke-all"]')).not.toBeNull();
    }
  });

  it('shows empty states and hides revoke-all', async () => {
    await mount(makeCtx('en'));
    expect(document.querySelector('.agents-tokens-empty').textContent).toBe('No tokens issued yet.');
    expect(document.querySelector('.agents-bypass-empty')).not.toBeNull();
    expect(document.querySelector('[data-action="agents-revoke-all"]')).toBeNull();
  });

  it('puts user-supplied names in as text, never as markup', async () => {
    const evil = '<img src=x onerror="window.__pwned=1">';
    await mount(makeCtx(), { tokens: [token(evil, { preview: '<b>p</b>' })], bypasses: [policy(evil, { scopeConnIds: ['<i>x</i>'] })] });
    expect(document.querySelector('#agents-section img')).toBeNull();
    expect(document.querySelector('#agents-section b')).toBeNull();
    expect(document.querySelector('#agents-section .agents-scope i')).toBeNull();
    expect(document.querySelector('#agents-section').textContent).toContain(evil);
    expect(window.__pwned).toBeUndefined();
  });

  it('shows a load error', async () => {
    App.ListAgentTokens.mockRejectedValue(new Error('db locked'));
    App.ListAgentApprovalBypasses.mockResolvedValue([]);
    document.body.innerHTML = '<div id="agents-root"></div>';
    mountAgentsSection(document.querySelector('#agents-root'), makeCtx('en'));
    await flush();
    expect(document.querySelector('.agents-error').textContent).toBe('Could not load agents: db locked');
  });

  it('does not refetch on a remount right after a load', async () => {
    const ctx = makeCtx();
    await mount(ctx);
    expect(App.ListAgentTokens).toHaveBeenCalledTimes(1);
    document.body.innerHTML = '<div id="agents-root"></div>';
    mountAgentsSection(document.querySelector('#agents-root'), ctx);
    await flush();
    expect(App.ListAgentTokens).toHaveBeenCalledTimes(1);
  });

  it('drops a load that an action overtook', async () => {
    const ctx = makeCtx();
    await mount(ctx, { bypasses: [policy('a')] });
    let resolveStale;
    App.ListAgentTokens.mockReturnValueOnce(new Promise((resolve) => { resolveStale = resolve; }));
    App.ListAgentApprovalBypasses.mockResolvedValueOnce([policy('a')]);
    const stale = loadAgents();
    App.ListAgentTokens.mockResolvedValue([]);
    App.ListAgentApprovalBypasses.mockResolvedValue([]);
    await loadAgents();
    resolveStale([token('stale')]);
    await stale;
    expect(getAgentsState().tokens).toEqual([]);
    expect(getAgentsState().bypasses).toEqual([]);
  });
});

describe('issuing a token', () => {
  async function issue(ctx, name) {
    const input = document.querySelector('#agents-issue-name');
    input.value = name;
    handleAgentsInput({ target: input });
    const form = document.querySelector('[data-agents-form="issue"]');
    return handleAgentsSubmit({ target: form, preventDefault: vi.fn() }, ctx);
  }

  it('shows the token once with a copy button, fills the example, and forgets it on close', async () => {
    const ctx = makeCtx('ja');
    await mount(ctx);
    App.IssueAgentToken.mockResolvedValue({ ok: true, message: 'claude-code 専用のトークンを発行しました', token: SECRET });
    App.ListAgentTokens.mockResolvedValue([token('claude-code')]);
    expect(await issue(ctx, '  claude-code ')).toBe(true);
    await flush();
    expect(App.IssueAgentToken).toHaveBeenCalledWith('claude-code');
    expect(ctx.bumpGen).toHaveBeenCalled();
    expect(ctx.refresh).toHaveBeenCalledWith({ silent: true });
    expect(ctx.toast).toHaveBeenCalledWith('claude-code 専用のトークンを発行しました');
    expect(document.querySelector('#agents-issued-token').value).toBe(SECRET);
    expect(document.querySelector('.agents-issued-title').textContent).toBe('「claude-code」のトークン');
    expect(document.querySelector('#agents-config-code').textContent).toContain(`Bearer ${SECRET}`);
    expect(document.querySelector('#agents-issue-name').value).toBe('');

    await handleAgentsClick(click('[data-action="agents-copy-token"]'), ctx);
    expect(navigator.clipboard.writeText).toHaveBeenCalledWith(SECRET);
    expect(ctx.toast).toHaveBeenLastCalledWith('コピーしました');

    // Survives a settings re-render while still shown.
    document.body.innerHTML = '<div id="agents-root"></div>';
    mountAgentsSection(document.querySelector('#agents-root'), ctx);
    expect(document.querySelector('#agents-issued-token').value).toBe(SECRET);

    await handleAgentsClick(click('[data-action="agents-hide-token"]'), ctx);
    expect(document.querySelector('#agents-issued-token')).toBeNull();
    expect(document.body.innerHTML).not.toContain(SECRET);
    expect(document.querySelector('#agents-config-code').textContent).toContain('<エージェント用トークン>');
    expect(getAgentsState().issued).toBeNull();

    document.body.innerHTML = '<div id="agents-root"></div>';
    mountAgentsSection(document.querySelector('#agents-root'), ctx);
    expect(document.body.innerHTML).not.toContain(SECRET);
  });

  it('forgets the token when the page is left', async () => {
    const ctx = makeCtx('en');
    await mount(ctx);
    App.IssueAgentToken.mockResolvedValue({ ok: true, message: 'ok', token: SECRET });
    await issue(ctx, 'codex');
    await flush();
    expect(document.querySelector('#agents-issued-token')).not.toBeNull();
    forgetIssuedAgentToken();
    expect(document.body.innerHTML).not.toContain(SECRET);
  });

  it('copies the example without a token when none is shown', async () => {
    const ctx = makeCtx('en');
    await mount(ctx);
    await handleAgentsClick(click('[data-action="agents-copy-config"]'), ctx);
    const copied = navigator.clipboard.writeText.mock.calls[0][0];
    expect(JSON.parse(copied).mcpServers['ssh-gete'].headers.Authorization).toBe('Bearer <agent token>');
  });

  it('rejects an empty name without calling the backend', async () => {
    const ctx = makeCtx('en');
    await mount(ctx);
    await issue(ctx, '   ');
    expect(App.IssueAgentToken).not.toHaveBeenCalled();
    expect(document.querySelector('.invalid-feedback').textContent).toBe('Enter an agent name');
  });

  it('asks before replacing an existing token and does nothing on cancel', async () => {
    const ctx = makeCtx('ja');
    await mount(ctx, { tokens: [token('claude-code')] });
    const pending = handleAgentsClick(click('[data-action="agents-reissue"][data-agent-name="claude-code"]'), ctx);
    await flush();
    expect(document.querySelector('#agents-modal-root .modal-body').textContent).toContain('「claude-code」の今のトークン');
    await answerDialog(false);
    await pending;
    expect(App.IssueAgentToken).not.toHaveBeenCalled();
    expect(document.querySelector('#agents-modal-root .modal')).toBeNull();
  });

  it('shows the backend message and no token on failure', async () => {
    const ctx = makeCtx('ja');
    await mount(ctx);
    App.IssueAgentToken.mockResolvedValue({ ok: false, message: '保存できません' });
    await issue(ctx, 'x');
    await flush();
    expect(ctx.toast).toHaveBeenCalledWith('保存できません');
    expect(document.querySelector('#agents-issued-token')).toBeNull();
  });

  it('keeps the typed name across a re-render and reports busy while typing', async () => {
    const ctx = makeCtx('ja');
    await mount(ctx);
    const input = document.querySelector('#agents-issue-name');
    input.value = 'half';
    handleAgentsInput({ target: input });
    expect(agentsSectionBusy()).toBe(true);
    document.body.innerHTML = '<div id="agents-root"></div>';
    mountAgentsSection(document.querySelector('#agents-root'), ctx);
    expect(document.querySelector('#agents-issue-name').value).toBe('half');
  });
});

describe('revoking', () => {
  it('revokes a token after confirmation and drops its shown secret', async () => {
    const ctx = makeCtx('en');
    await mount(ctx, { tokens: [token('codex')] });
    App.IssueAgentToken.mockResolvedValue({ ok: true, message: 'issued', token: SECRET });
    const reissue = handleAgentsClick(click('[data-action="agents-reissue"]'), ctx);
    await answerDialog(true);
    await reissue;
    await flush();
    expect(document.querySelector('#agents-issued-token').value).toBe(SECRET);

    App.RevokeAgentToken.mockResolvedValue({ ok: true, message: 'revoked' });
    App.ListAgentTokens.mockResolvedValue([]);
    const pending = handleAgentsClick(click('[data-action="agents-revoke-token"][data-agent-name="codex"]'), ctx);
    await flush();
    expect(document.querySelector('#agents-modal-title').textContent).toBe('Revoke the token?');
    await answerDialog(true);
    await pending;
    await flush();
    expect(App.RevokeAgentToken).toHaveBeenCalledWith('codex');
    expect(ctx.toast).toHaveBeenCalledWith('revoked');
    expect(document.body.innerHTML).not.toContain(SECRET);
    expect(document.querySelector('[data-agent-row="codex"]')).toBeNull();
  });

  it('does not revoke when the dialog is dismissed with Escape', async () => {
    const ctx = makeCtx('en');
    await mount(ctx, { tokens: [token('codex')] });
    const pending = handleAgentsClick(click('[data-action="agents-revoke-token"]'), ctx);
    await flush();
    document.querySelector('#agents-modal-root .modal').dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
    await pending;
    expect(App.RevokeAgentToken).not.toHaveBeenCalled();
  });

  it('revokes one bypass after confirmation', async () => {
    const ctx = makeCtx('ja');
    await mount(ctx, { bypasses: [policy('claude-code'), policy('codex')] });
    App.RevokeAgentApprovalBypass.mockResolvedValue({ ok: true, message: '取り消しました' });
    App.ListAgentApprovalBypasses.mockResolvedValue([policy('codex')]);
    const pending = handleAgentsClick(click('[data-action="agents-revoke-bypass"][data-agent-name="claude-code"]'), ctx);
    await flush();
    expect(document.querySelector('#agents-modal-root .modal-body').textContent).toContain('「claude-code」の承認省略');
    await answerDialog(true);
    await pending;
    await flush();
    expect(App.RevokeAgentApprovalBypass).toHaveBeenCalledWith('claude-code');
    expect(document.querySelectorAll('[data-bypass-row]')).toHaveLength(1);
    expect(ctx.refresh).toHaveBeenCalled();
  });

  it('lowers the max risk without asking', async () => {
    const ctx = makeCtx('ja');
    await mount(ctx, { bypasses: [policy('codex')] });
    App.SetAgentBypassMaxRisk.mockResolvedValue({ ok: true, message: 'ok' });
    const select = document.querySelector('[data-agents-max-risk="codex"]');
    expect(select.value).toBe('Medium');
    select.value = 'Low';
    await handleAgentsChange({ target: select }, ctx);
    expect(App.SetAgentBypassMaxRisk).toHaveBeenCalledWith('codex', 'Low');
    expect(document.querySelector('#agents-modal-root .modal')).toBeNull();
  });

  it('forces High only after confirmation', async () => {
    const ctx = makeCtx('ja');
    await mount(ctx, { bypasses: [policy('codex')] });
    App.SetAgentBypassMaxRisk.mockResolvedValue({ ok: true, message: 'ok' });
    let select = document.querySelector('[data-agents-max-risk="codex"]');
    select.value = 'High';
    let pending = handleAgentsChange({ target: select }, ctx);
    await flush();
    expect(document.querySelector('#agents-modal-root .modal-body').textContent).toContain('「codex」のコマンドは');
    await answerDialog(false);
    await pending;
    expect(App.SetAgentBypassMaxRisk).not.toHaveBeenCalled();
    expect(document.querySelector('[data-agents-max-risk="codex"]').value).toBe('Medium');

    App.ListAgentApprovalBypasses.mockResolvedValue([policy('codex', { maxRisk: 'High' })]);
    select = document.querySelector('[data-agents-max-risk="codex"]');
    select.value = 'High';
    pending = handleAgentsChange({ target: select }, ctx);
    await answerDialog(true);
    await pending;
    await flush();
    expect(App.SetAgentBypassMaxRisk).toHaveBeenCalledWith('codex', 'High');
    expect(document.querySelector('[data-agents-max-risk="codex"]').value).toBe('High');
  });

  it('revokes every bypass after confirmation with the count', async () => {
    const ctx = makeCtx('en');
    await mount(ctx, { bypasses: [policy('a'), policy('b')] });
    App.RevokeAllAgentApprovalBypasses.mockResolvedValue({ ok: true, message: 'done' });
    App.ListAgentApprovalBypasses.mockResolvedValue([]);
    const pending = handleAgentsClick(click('[data-action="agents-revoke-all"]'), ctx);
    await flush();
    expect(document.querySelector('#agents-modal-root .modal-body').textContent).toContain('2 agent(s)');
    await answerDialog(true);
    await pending;
    await flush();
    expect(App.RevokeAllAgentApprovalBypasses).toHaveBeenCalledTimes(1);
    expect(document.querySelector('.agents-bypass-empty')).not.toBeNull();
  });

  it('cancel leaves the bypass in place', async () => {
    const ctx = makeCtx('en');
    await mount(ctx, { bypasses: [policy('a')] });
    const pending = handleAgentsClick(click('[data-action="agents-revoke-all"]'), ctx);
    await answerDialog(false);
    await pending;
    expect(App.RevokeAllAgentApprovalBypasses).not.toHaveBeenCalled();
    expect(ctx.bumpGen).not.toHaveBeenCalled();
  });
});

describe('handleAgentsClick routing', () => {
  it('ignores clicks outside its namespace', async () => {
    document.body.innerHTML = '<button data-action="copy-token">x</button>';
    expect(await handleAgentsClick({ target: document.querySelector('button') }, makeCtx())).toBe(false);
    expect(await handleAgentsSubmit({ target: document.body }, makeCtx())).toBe(false);
    expect(handleAgentsInput({ target: document.body })).toBe(false);
  });

  it('builds without a mounted root', () => {
    const section = buildAgentsSection(makeCtx('en'), { now: NOW });
    expect(section.id).toBe('agents-section');
  });
});
