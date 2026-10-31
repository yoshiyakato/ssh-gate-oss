import { describe, it, expect, vi } from 'vitest';
import { MODEL_I18N, interpolate, mergeI18n } from './model.js';
import { SIDEBAR_I18N, handleSidebarClick, renderSidebarStatus, sessionRowAriaLabel, updateSidebarStatus } from './sidebar.js';

const I18N = mergeI18n(SIDEBAR_I18N, MODEL_I18N);
const tFor = (lang) => (key, vars) => interpolate(I18N[lang][key] ?? key, vars);
const NOW = Date.parse('2026-10-27T10:00:00Z');
const ago = (minutes) => new Date(NOW - minutes * 60000).toISOString();

function session(connId, extra = {}) {
  return {
    connId, connName: connId, kind: 'ssh', connState: 'disconnected',
    shells: 0, connecting: 0, running: 0, pending: 0, ...extra,
  };
}

const baseData = (sessions, extra = {}) => ({
  connections: [], requests: [], mcp: { status: '起動中', listenAddress: '127.0.0.1', port: 8787, connectedAgents: 1 },
  storageError: '', sessions, terminals: [], runningTotal: 0, ...extra,
});

describe('sessionRowAriaLabel', () => {
  const row = { name: 'web-01', kind: 'ssh', connState: 'connected', shells: 2, running: 1, elapsedMs: 12 * 60000 };
  it('reads naturally in JA and EN, omitting zero counts', () => {
    expect(sessionRowAriaLabel(row, tFor('ja'), 'ja')).toBe('web-01、接続中、12分、シェル2、実行中1件');
    expect(sessionRowAriaLabel(row, tFor('en'), 'en')).toBe('web-01, Connected, 12m, 2 shells, 1 running');
    expect(sessionRowAriaLabel({ ...row, running: 0 }, tFor('en'), 'en')).toBe('web-01, Connected, 12m, 2 shells');
  });
});

describe('renderSidebarStatus', () => {
  it('shows the empty state', () => {
    for (const lang of ['ja', 'en']) {
      document.body.innerHTML = renderSidebarStatus(baseData([session('a')]), { t: tFor(lang), lang, now: NOW });
      expect(document.querySelector('.side-empty').textContent).toBe(tFor(lang)('side.empty'));
      expect(document.querySelectorAll('.side-session-row')).toHaveLength(0);
    }
  });

  it('caps rows at five and links the rest', () => {
    const sessions = Array.from({ length: 6 }, (_, i) => session(`web-0${i}`, { connState: 'connected', shells: 1, connectedSince: ago(i + 1) }));
    document.body.innerHTML = renderSidebarStatus(baseData(sessions), { t: tFor('ja'), lang: 'ja', now: NOW });
    expect(document.querySelectorAll('.side-session-row')).toHaveLength(5);
    expect(document.querySelector('[data-action="side-more"]').textContent).toContain('他 1 件');
  });

  it('shows the version just above the company name', () => {
    const render = (version) => {
      document.body.innerHTML = renderSidebarStatus(baseData([], { version }), { t: tFor('ja'), lang: 'ja', now: NOW });
      return document.querySelector('.app-version');
    };
    const row = render('20261031-02');
    expect(row.textContent).toBe('Ver.20261031-02');
    expect(row.nextElementSibling.className).toBe('company-name');
    expect(render('v1.02').textContent).toBe('Ver.1.02');
    expect(render('')).toBeNull();
  });

  it('always pairs the symbol with a state label', () => {
    const data = baseData([session('router-01', { connState: 'error', lastError: 'boom' }), session('web-01', { connState: 'connected', shells: 1, connectedSince: ago(3) })]);
    document.body.innerHTML = renderSidebarStatus(data, { t: tFor('ja'), lang: 'ja', now: NOW });
    for (const row of document.querySelectorAll('.side-session-row')) {
      expect(row.querySelector('.side-symbol').textContent.trim()).not.toBe('');
      expect(row.getAttribute('aria-label')).toMatch(/接続エラー|接続中/);
    }
    expect(document.querySelector('.side-session-row.is-error').textContent).toContain('接続エラー');
  });

  it('shows the storage error only when present', () => {
    document.body.innerHTML = renderSidebarStatus(baseData([], { storageError: 'disk full' }), { t: tFor('en'), lang: 'en', now: NOW });
    expect(document.querySelector('.side-storage-error').textContent).toContain('Storage error');
    document.body.innerHTML = renderSidebarStatus(baseData([]), { t: tFor('en'), lang: 'en', now: NOW });
    expect(document.querySelector('.side-storage-error')).toBeNull();
  });
});

describe('updateSidebarStatus', () => {
  it('keeps unchanged row nodes across polls', () => {
    const root = document.createElement('div');
    document.body.replaceChildren(root);
    const data = baseData([session('web-01', { connState: 'connected', shells: 1, connectedSince: ago(3) })]);
    updateSidebarStatus(root, data, { t: tFor('ja'), lang: 'ja', now: NOW });
    const row = root.querySelector('.side-session-row');
    expect(updateSidebarStatus(root, data, { t: tFor('ja'), lang: 'ja', now: NOW })).toBe(false);
    data.sessions[0].shells = 2;
    expect(updateSidebarStatus(root, data, { t: tFor('ja'), lang: 'ja', now: NOW })).toBe(true);
    expect(root.querySelector('.side-session-row')).toBe(row);
    expect(row.textContent).toContain('シェル2');
  });
});

describe('handleSidebarClick', () => {
  it('routes row, more and approvals clicks through ctx', async () => {
    const ctx = { showConnection: vi.fn(async () => {}), navigate: vi.fn(async () => true) };
    document.body.innerHTML = '<button data-action="side-open-conn" data-conn-id="c1"></button><button data-action="side-more"></button><button data-action="side-approvals"></button><button data-lang="en"></button>';
    const [row, more, approvals, lang] = document.querySelectorAll('button');
    expect(await handleSidebarClick({ target: row }, ctx)).toBe(true);
    expect(ctx.showConnection).toHaveBeenCalledWith('c1');
    await handleSidebarClick({ target: more }, ctx);
    await handleSidebarClick({ target: approvals }, ctx);
    expect(ctx.navigate.mock.calls.map(([page]) => page)).toEqual(['connections', 'commands']);
    expect(await handleSidebarClick({ target: lang }, ctx)).toBe(false);
  });
});

describe('AI sessions line (phase7 §8.1)', () => {
  const T = (key, vars) => interpolate(({ 'side.aiSessions': 'AI sessions {n} ({p} awaiting approval)', 'side.aiSessionsOnly': 'AI sessions {n}' })[key] ?? tFor('en')(key, vars), vars);
  it('shows the counts and hides the line when off and empty', () => {
    document.body.innerHTML = renderSidebarStatus(baseData([], { aiSessions: { open: 2, pendingApproval: 1, enabled: true } }), { t: T, lang: 'en', now: NOW });
    const line = document.querySelector('[data-action="side-aisessions"]');
    expect(line.textContent).toContain('AI sessions 2 (1 awaiting approval)');
    document.body.innerHTML = renderSidebarStatus(baseData([], { aiSessions: { open: 0, pendingApproval: 0, enabled: false } }), { t: T, lang: 'en', now: NOW });
    expect(document.querySelector('[data-action="side-aisessions"]')).toBeNull();
  });
  it('navigates to the AI sessions page', async () => {
    document.body.innerHTML = '<button data-action="side-aisessions">x</button>';
    const navigate = vi.fn(async () => true);
    const event = { target: document.querySelector('button') };
    expect(await handleSidebarClick(event, { navigate })).toBe(true);
    expect(navigate).toHaveBeenCalledWith('aisessions');
  });
});
