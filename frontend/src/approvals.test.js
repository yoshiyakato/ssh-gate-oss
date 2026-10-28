import { describe, it, expect, vi, beforeEach } from 'vitest';

const backend = vi.hoisted(() => ({
  calls: [],
  target: null,
  targetError: false,
  approve: null,
  confirmed: null,
  bypass: null,
}));

vi.mock('../wailsjs/go/main/App', () => ({
  GetApprovalTarget: async (id) => {
    backend.calls.push(['GetApprovalTarget', id]);
    if (backend.targetError) throw new Error('boom');
    return typeof backend.target === 'function' ? backend.target(id) : { ...(backend.target ?? {}), requestId: id };
  },
  ApproveCommand: async (id, command) => {
    backend.calls.push(['ApproveCommand', id, command]);
    return backend.approve ? backend.approve(id, command) : { ok: true, message: `${id} を承認し、実行しました` };
  },
  ApproveCommandConfirmed: async (id, command) => {
    backend.calls.push(['ApproveCommandConfirmed', id, command]);
    return backend.confirmed ? backend.confirmed(id, command) : { ok: true, message: 'confirmed' };
  },
  ApproveCommandAndBypassAgent: async (id, command) => {
    backend.calls.push(['ApproveCommandAndBypassAgent', id, command]);
    return backend.bypass ? backend.bypass(id, command) : { ok: true, message: 'bypassed' };
  },
  RejectCommand: async (id) => {
    backend.calls.push(['RejectCommand', id]);
    return { ok: true, message: `${id} を拒否しました` };
  },
  RevokeAgentApprovalBypass: async (name) => {
    backend.calls.push(['RevokeAgentApprovalBypass', name]);
    return { ok: true, message: 'revoked' };
  },
}));

import { MODEL_I18N, interpolate, mergeI18n } from './model.js';
import { BASE_I18N } from './i18n.js';
import { RESULT_I18N, resultMessage } from './result-message.js';
import {
  APPROVALS_I18N,
  APPROVAL_CODES,
  approvalBlock,
  approvalCommandToSend,
  approvalResultMessage,
  hasHiddenChars,
  plainDisplayChar,
  bypassAvailability,
  bypassConfirmText,
  commandsView,
  createApprovalsState,
  handleApprovalsClick,
  handleApprovalsInput,
  renderCommandsPage,
} from './approvals.js';

const I18N = mergeI18n(BASE_I18N, MODEL_I18N, APPROVALS_I18N);
const tFor = (lang) => (key, vars) => interpolate(I18N[lang][key] ?? key, vars);

const flush = async () => {
  for (let i = 0; i < 5; i += 1) await new Promise((resolve) => setTimeout(resolve, 0));
};

const conn = (id, name) => ({ id, name, type: 'SSH', host: '10.0.0.1', port: 22, user: 'u' });
const request = (id, extra = {}) => ({
  id, connectionId: 'c1', host: 'web-01', user: 'u', command: `echo ${id}`, executedCommand: '', reason: 'why',
  requestedBy: 'agent', requestedAt: '2026-10-27T10:00:00Z', risk: 'Low', status: 'pending', stdout: '', stderr: '',
  targetFp: 'u@10.0.0.1:22', ...extra,
});
const okTarget = (extra = {}) => ({
  found: true, resolvedBy: 'id', connId: 'c1', name: 'web-01', type: 'SSH',
  requestedHost: 'web-01', requestedFp: 'u@10.0.0.1:22', currentFp: 'u@10.0.0.1:22', fpChanged: false, executable: true,
  hostKey: { connId: 'c1', address: '10.0.0.1:22', known: true, keys: [{ type: 'ssh-ed25519', fingerprint: 'SHA256:abc' }] },
  message: '', ...extra,
});

let state;
let ctx;
let root;

function makeCtx(lang = 'ja') {
  state = {
    lang,
    active: 'commands',
    dataGen: 0,
    data: {
      connections: [conn('c1', 'web-01'), conn('c2', 'db-01')],
      requests: [],
      agentPolicies: [],
      agentTokens: [],
      mcp: { bypassAllowed: true },
      sessions: [],
    },
    selectedRequestId: '',
    dirty: { settings: false, approval: false },
    approvals: createApprovalsState(),
  };
  ctx = {
    state,
    t: tFor(lang),
    lang: () => lang,
    toast: vi.fn(),
    renderPage: vi.fn(() => renderCommandsPage(root, ctx, ctx.view)),
    refresh: vi.fn(async () => true),
    bumpGen: vi.fn(() => ++state.dataGen),
    copyText: vi.fn(async () => {}),
    view: undefined,
  };
  return ctx;
}

async function render(view) {
  ctx.view = view;
  renderCommandsPage(root, ctx, view);
  await flush();
}

async function click(selector) {
  const element = root.querySelector(selector);
  const event = new MouseEvent('click', { bubbles: true });
  Object.defineProperty(event, 'target', { value: element });
  const handled = await handleApprovalsClick(event, ctx, ctx.view);
  await flush();
  return handled;
}

beforeEach(() => {
  document.body.innerHTML = '<div id="page-root"></div>';
  root = document.getElementById('page-root');
  backend.calls = [];
  backend.target = okTarget();
  backend.targetError = false;
  backend.approve = null;
  backend.confirmed = null;
  backend.bypass = null;
  makeCtx('ja');
  vi.restoreAllMocks();
});

describe('APPROVALS_I18N', () => {
  it('has the same keys in JA and EN, all under appr., and merges with BASE_I18N', () => {
    expect(Object.keys(APPROVALS_I18N.ja).sort()).toEqual(Object.keys(APPROVALS_I18N.en).sort());
    for (const key of Object.keys(APPROVALS_I18N.ja)) expect(key.startsWith('appr.')).toBe(true);
    expect(() => mergeI18n(BASE_I18N, MODEL_I18N, APPROVALS_I18N)).not.toThrow();
    for (const code of APPROVAL_CODES) {
      expect(APPROVALS_I18N.ja[`appr.code.${code}`]).toBeTruthy();
      expect(APPROVALS_I18N.en[`appr.code.${code}`]).toBeTruthy();
    }
  });
});

describe('pure helpers', () => {
  it('commandsView without a view shows everything and selects by id, else the first pending', () => {
    state.data.requests = [request('R2'), request('R1', { status: 'succeeded' })];
    expect(commandsView(state).queue.map((r) => r.id)).toEqual(['R2']);
    expect(commandsView(state).selectedRequest.id).toBe('R2');
    state.selectedRequestId = 'R1';
    expect(commandsView(state).selectedRequest.id).toBe('R1');
  });

  it('commandsView with a view uses only the view (never selects outside the filter)', () => {
    state.data.requests = [request('R1'), request('R3', { connectionId: 'c2' })];
    state.selectedRequestId = 'R3';
    const view = { filter: 'c1', queue: [state.data.requests[0]], history: [state.data.requests[0]], selectedRequest: state.data.requests[0], hiddenPending: 1, unknownIds: new Set() };
    const model = commandsView(state, view);
    expect(model.selectedRequest.id).toBe('R1');
    expect(model.filterName).toBe('web-01');
    expect(model.hiddenPending).toBe(1);
  });

  it('approvalBlock blocks a changed or serial target, not a missing one', () => {
    expect(approvalBlock(null).blocked).toBe(false);
    expect(approvalBlock(okTarget()).blocked).toBe(false);
    expect(approvalBlock(okTarget({ fpChanged: true }))).toEqual({ blocked: true, reasonKey: 'appr.blocked.changed' });
    expect(approvalBlock(okTarget({ executable: false, type: 'Serial' }))).toEqual({ blocked: true, reasonKey: 'appr.blocked.notExecutable' });
    expect(approvalBlock({ found: false }).blocked).toBe(false);
    // N21: an older request without a recorded target cannot be approved.
    expect(approvalBlock(okTarget({ targetUnknown: true, fpChanged: true }))).toEqual({ blocked: true, reasonKey: 'appr.blocked.unknown' });
  });

  it('bypassAvailability needs an agent name and the shared or the agent token', () => {
    expect(bypassAvailability(state, request('R', { requestedBy: '' })).reasonKey).toBe('appr.bypass.needsAgent');
    expect(bypassAvailability(state, request('R')).allowed).toBe(true);
    state.data.mcp.bypassAllowed = false;
    expect(bypassAvailability(state, request('R'))).toEqual({ allowed: false, reasonKey: 'cmd.bypassNeedsBearer' });
    state.data.agentTokens = [{ agentName: 'agent', bypass: true }];
    expect(bypassAvailability(state, request('R')).allowed).toBe(true);
  });

  it('approvalResultMessage localizes known codes and passes other messages through', () => {
    const tEn = tFor('en');
    expect(approvalResultMessage({ ok: false, code: 'expired', message: 'JA' }, tEn)).toBe(I18N.en['appr.code.expired']);
    expect(approvalResultMessage({ ok: false, code: 'target_changed', message: 'JA' }, tEn)).toContain('Reject it and have the agent ask again');
    expect(approvalResultMessage({ ok: true, message: 'done' }, tEn)).toBe('done');
    expect(approvalResultMessage({ ok: false, code: 'other', message: 'raw' }, tEn)).toBe('raw');
  });

  // Final audit #8: in English, success and non-approval codes go through
  // resultMessage instead of showing the backend's Japanese text.
  it('approvalResultMessage falls back to resultMessage for other results', () => {
    const tEn = tFor('en');
    const dict = { ...I18N.en, ...RESULT_I18N.en };
    const tr = (key, vars) => interpolate(dict[key] ?? key, vars);
    const fallback = (result) => resultMessage(result, { lang: 'en', t: tr, dict });
    const cases = [
      [{ ok: true, message: 'R1 を承認し、実行しました' }, RESULT_I18N.en['result.done']],
      [{ ok: true, message: 'R3 を拒否しました' }, RESULT_I18N.en['result.done']],
      [{ ok: false, code: 'exec_failed', message: 'R1 の実行に失敗しました: x' }, RESULT_I18N.en['result.code.exec_failed']],
      [{ ok: false, code: 'not_found', message: '拒否対象のリクエストが見つかりません' }, RESULT_I18N.en['result.code.not_found']],
      [{ ok: false, code: 'state_storage', message: 'R1 の状態を更新できません' }, RESULT_I18N.en['result.code.state_storage']],
    ];
    for (const [result, want] of cases) {
      const got = approvalResultMessage(result, tEn, fallback);
      expect(got).toBe(want);
      expect(got).not.toMatch(/[\u3040-\u30ff\u4e00-\u9fff]/);
    }
    expect(approvalResultMessage({ ok: true, message: 'R1 を承認し、実行しました、agent の承認省略を許可しました', bypass: 'granted' }, tEn, fallback))
      .toBe(`${RESULT_I18N.en['result.done']} / ${I18N.en['appr.bypassResult.granted']}`);
    // Japanese mode keeps the backend text (it already has the bypass note).
    const ja = (result) => resultMessage(result, { lang: 'ja', t: tr, dict });
    expect(approvalResultMessage({ ok: true, message: 'R1 を承認し、実行しました', bypass: 'granted' }, tFor('ja'), ja)).toBe('R1 を承認し、実行しました');
  });

  // Final audit #7: a textarea turns CR into LF; an unedited approval sends
  // the requested command.
  it('approvalCommandToSend keeps the requested command when the form is unedited', () => {
    const textarea = document.createElement('textarea');
    const requested = 'echo hi\rrm -rf ~';
    textarea.value = requested;
    expect(textarea.value).not.toBe(requested); // the normalization the fix guards against
    expect(approvalCommandToSend(requested, textarea.value)).toBe(requested);
    expect(approvalCommandToSend('a\r\nb', 'a\nb')).toBe('a\r\nb');
    expect(approvalCommandToSend('ls', 'ls -la')).toBe('ls -la');
    expect(approvalCommandToSend('ls', undefined)).toBe('ls');
  });

  // Final audit #9: combining marks in the CJK symbols block are badged.
  it('plainDisplayChar badges combining marks in U+3000-U+303F', () => {
    for (const cp of [0x302a, 0x302b, 0x302c, 0x302d, 0x302e, 0x302f, 0x303f, 0x3099, 0x309a]) {
      expect(plainDisplayChar(cp)).toBe(false);
    }
    for (const cp of [0x3001, 0x3002, 0x300c, 0x3029, 0x3030, 0x303e, 0x3042, 0x30a2, 0x65e5]) {
      expect(plainDisplayChar(cp)).toBe(true);
    }
    expect(hasHiddenChars('rm -rf /tmp/a\u302a')).toBe(true);
    expect(hasHiddenChars('ログ「エラー」を確認')).toBe(false);
  });

  it('bypassConfirmText states 8 hours, the Medium cap and the token requirement in both languages', () => {
    const req = request('R');
    const ja = bypassConfirmText(state, req, tFor('ja'));
    expect(ja).toContain('8時間');
    expect(ja).toContain('Medium');
    expect(ja).toContain('トークン');
    expect(ja).toContain('agent 専用のエージェントトークンはまだありません');
    const en = bypassConfirmText(state, req, tFor('en'));
    expect(en).toContain('8 hours');
    expect(en).toContain('up to Medium');
    expect(en).toContain('own agent token');
    state.data.agentTokens = [{ agentName: 'agent' }];
    expect(bypassConfirmText(state, req, tFor('en'))).not.toContain('has no agent token yet');
  });
});

describe('renderCommandsPage', () => {
  it('puts request strings in as text, never as markup', async () => {
    const evil = '<img src=x onerror="window.__pwned=1">';
    state.data.requests = [request('R1', { command: evil, reason: evil, requestedBy: evil, host: evil })];
    backend.target = okTarget({ name: evil, currentFp: evil, requestedFp: evil });
    await render();
    expect(root.querySelector('img')).toBeNull();
    expect(root.querySelector('#approval-command').value).toBe(evil);
    expect(root.querySelector('.reason-box').textContent).toBe(evil);
    expect(root.querySelector('#approval-target').textContent).toContain(evil);
  });

  it('shows the resolved target, both user@host:port values and the host key', async () => {
    state.data.requests = [request('R1')];
    await render();
    expect(backend.calls).toContainEqual(['GetApprovalTarget', 'R1']);
    const panel = root.querySelector('#approval-target');
    expect(panel.dataset.requestId).toBe('R1');
    expect(panel.textContent).toContain('web-01 (SSH)');
    expect(panel.textContent).toContain('接続先ID（要求時に記録）');
    expect(panel.querySelector('.approval-fp-requested').textContent).toBe('u@10.0.0.1:22');
    expect(panel.querySelector('.approval-fp-current').textContent).toBe('u@10.0.0.1:22');
    expect(panel.querySelector('.approval-hostkey').textContent).toContain('SHA256:abc');
    expect(root.querySelector('[data-action="approve-command"]').disabled).toBe(false);
  });

  it('says the host key is not trusted yet when none is recorded', async () => {
    state.data.requests = [request('R1')];
    backend.target = okTarget({ resolvedBy: 'name', hostKey: { known: false, keys: [] } });
    await render();
    expect(root.querySelector('.approval-hostkey-unknown')).not.toBeNull();
    expect(root.querySelector('#approval-target').textContent).toContain('IDの無い古い要求');
  });

  it('disables approve for an older request without a recorded target (N21)', async () => {
    for (const lang of ['ja', 'en']) {
      makeCtx(lang);
      state.data.requests = [request('R1')];
      backend.target = okTarget({ targetUnknown: true, fpChanged: true, requestedFp: '' });
      await render();
      expect(root.querySelector('.approval-target-unknown').textContent).toBe(tFor(lang)('appr.target.unknown'));
      expect(root.querySelector('.approval-target-changed')).toBeNull();
      expect(root.querySelector('[data-action="approve-command"]').disabled).toBe(true);
      expect(root.querySelector('[data-action="approve-command"]').title).toBe(tFor(lang)('appr.blocked.unknown'));
      expect(root.querySelector('[data-action="reject-command"]').disabled).toBe(false);
    }
  });

  it('disables approve and bypass when the target changed, and tells the user to reject (REQ-011)', async () => {
    for (const lang of ['ja', 'en']) {
      makeCtx(lang);
      state.data.requests = [request('R1')];
      backend.target = okTarget({ fpChanged: true, currentFp: 'root@10.0.0.9:22' });
      await render();
      const alert = root.querySelector('.approval-target-changed');
      expect(alert.textContent).toContain('u@10.0.0.1:22 → root@10.0.0.9:22');
      expect(alert.textContent).toContain(tFor(lang)('appr.target.changedAction'));
      expect(root.querySelector('[data-action="approve-command"]').disabled).toBe(true);
      expect(root.querySelector('[data-action="approve-command"]').title).toBe(tFor(lang)('appr.blocked.changed'));
      expect(root.querySelector('[data-action="approve-bypass-agent"]').disabled).toBe(true);
      expect(root.querySelector('[data-action="reject-command"]').disabled).toBe(false);
    }
  });

  it('blocks a serial target and warns about a missing one without blocking it', async () => {
    state.data.requests = [request('R1')];
    backend.target = okTarget({ type: 'Serial', executable: false, currentFp: '' });
    await render();
    expect(root.querySelector('.approval-target-serial')).not.toBeNull();
    expect(root.querySelector('[data-action="approve-command"]').disabled).toBe(true);

    backend.target = { found: false, message: 'gone' };
    await render();
    expect(root.querySelector('.approval-target-missing')).not.toBeNull();
    expect(root.querySelector('[data-action="approve-command"]').disabled).toBe(false);
  });

  it('patches the target in place without redrawing an edited command', async () => {
    state.data.requests = [request('R1')];
    let release;
    backend.target = () => new Promise((resolve) => { release = () => resolve({ ...okTarget({ fpChanged: true, currentFp: 'x@y:22' }), requestId: 'R1' }); });
    ctx.view = undefined;
    renderCommandsPage(root, ctx);
    expect(root.querySelector('.approval-target-loading')).not.toBeNull();
    const textarea = root.querySelector('#approval-command');
    textarea.value = 'echo edited';
    handleApprovalsInput({ target: textarea }, ctx);
    expect(state.dirty.approval).toBe(true);
    release();
    await flush();
    expect(root.querySelector('#approval-command')).toBe(textarea);
    expect(textarea.value).toBe('echo edited');
    expect(root.querySelector('.approval-target-changed')).not.toBeNull();
    expect(ctx.renderPage).not.toHaveBeenCalled();
  });

  it('shows the hidden-pending notice, unknown-connection badges and filtered empty texts', async () => {
    const r4 = request('R4', { connectionId: 'gone', host: 'old' });
    state.data.requests = [r4];
    await render({ filter: '', queue: [r4], history: [r4], selectedRequest: r4, hiddenPending: 0, unknownIds: new Set(['R4']) });
    expect(root.querySelectorAll('.unknown-conn')).toHaveLength(2);
    expect(root.querySelector('.unknown-conn').textContent).toBe('不明な接続先');
    expect(root.querySelector('.approvals-hidden-pending')).toBeNull();

    await render({ filter: 'c2', queue: [], history: [], selectedRequest: null, hiddenPending: 1, unknownIds: new Set() });
    const notice = root.querySelector('.approvals-hidden-pending');
    expect(notice.textContent).toContain('1 件');
    expect(notice.querySelector('[data-action="srvfilter-show-all"]')).not.toBeNull();
    expect(root.textContent).toContain('db-01 の承認待ちはありません');
    expect(root.textContent).toContain('db-01 の履歴はありません');
    expect(root.querySelector('#approval-command')).toBeNull();
    // srvfilter-show-all belongs to main.js.
    expect(await click('[data-action="srvfilter-show-all"]')).toBe(false);
  });

  it('keeps the edited/executed command view for decided requests', async () => {
    const done = request('R1', { status: 'succeeded', executedCommand: 'echo changed', stdout: 'out' });
    state.data.requests = [done];
    state.selectedRequestId = 'R1';
    await render();
    expect(root.querySelector('#approval-command')).toBeNull();
    expect(root.querySelector('#approval-target')).toBeNull();
    expect(root.querySelector('.command-block').textContent).toBe('echo changed');
    expect(root.textContent).toContain('修正あり');
    expect(root.querySelector('[data-action="approve-command"]')).toBeNull();
  });

  it('shows the bypass state with its expiry and a revoke button', async () => {
    state.data.requests = [request('R1')];
    state.data.agentPolicies = [{ agentName: 'agent', approvalBypass: true, expiresAt: '2026-10-27T18:00:00Z', active: true, hasToken: true }];
    await render();
    expect(root.querySelector('.command-agent em').textContent).toContain('承認省略中');
    expect(await click('[data-action="revoke-bypass"]')).toBe(true);
    expect(backend.calls).toContainEqual(['RevokeAgentApprovalBypass', 'agent']);
  });
});

describe('approve / reject handlers', () => {
  it('approves the request the form was drawn for with the edited command', async () => {
    state.data.requests = [request('R1')];
    await render();
    root.querySelector('#approval-command').value = 'echo edited';
    expect(await click('[data-action="approve-command"]')).toBe(true);
    expect(backend.calls).toContainEqual(['ApproveCommand', 'R1', 'echo edited']);
    expect(ctx.bumpGen).toHaveBeenCalled();
    expect(ctx.refresh).toHaveBeenCalledWith({ silent: true });
    expect(state.selectedRequestId).toBe('R1');
    expect(state.dirty.approval).toBe(false);
  });

  it('refuses with cmd.staleSelection when the form belongs to another request (audit #22/#40)', async () => {
    state.data.requests = [request('R1'), request('R2')];
    state.selectedRequestId = 'R1';
    await render();
    // The selection moved without a redraw.
    state.selectedRequestId = 'R2';
    await click('[data-action="approve-command"]');
    expect(backend.calls.some(([name]) => name === 'ApproveCommand')).toBe(false);
    expect(ctx.toast).toHaveBeenCalledWith(I18N.ja['cmd.staleSelection']);
    expect(ctx.renderPage).toHaveBeenCalled();

    // The redraw drew R2's form; moving the selection again makes it stale.
    expect(root.querySelector('#approval-command').dataset.requestId).toBe('R2');
    state.selectedRequestId = 'R1';
    await click('[data-action="reject-command"]');
    expect(backend.calls.some(([name]) => name === 'RejectCommand')).toBe(false);
  });

  it('refuses when the request stopped being pending under the form', async () => {
    state.data.requests = [request('R1')];
    await render();
    state.data.requests = [request('R1', { status: 'rejected' })];
    await click('[data-action="approve-command"]');
    expect(backend.calls.some(([name]) => name === 'ApproveCommand')).toBe(false);
    expect(ctx.toast).toHaveBeenCalledWith(I18N.ja['cmd.staleSelection']);
  });

  it('never approves a request outside the server filter', async () => {
    const r1 = request('R1');
    const r3 = request('R3', { connectionId: 'c2' });
    state.data.requests = [r1, r3];
    const view = { filter: 'c1', queue: [r1], history: [r1], selectedRequest: r1, hiddenPending: 1, unknownIds: new Set() };
    await render(view);
    state.selectedRequestId = 'R3';
    await click('[data-action="approve-command"]');
    expect(backend.calls.filter(([name]) => name === 'ApproveCommand').map(([, id]) => id)).toEqual(['R1']);
  });

  it('re-checks the target before approving and stops on a changed target', async () => {
    state.data.requests = [request('R1')];
    await render();
    expect(root.querySelector('[data-action="approve-command"]').disabled).toBe(false);
    backend.target = okTarget({ fpChanged: true, currentFp: 'root@h:22' });
    await click('[data-action="approve-command"]');
    expect(backend.calls.some(([name]) => name === 'ApproveCommand')).toBe(false);
    expect(ctx.toast).toHaveBeenCalledWith(I18N.ja['appr.code.target_changed']);
    expect(root.querySelector('.approval-target-changed')).not.toBeNull();
    expect(root.querySelector('[data-action="approve-command"]').disabled).toBe(true);
  });

  it('shows target_changed from the backend and redraws the target', async () => {
    state.data.requests = [request('R1')];
    await render();
    // The connection is edited between the lookup and the approval.
    backend.approve = () => {
      backend.target = okTarget({ fpChanged: true, currentFp: 'root@h:22' });
      return { ok: false, code: 'target_changed', message: '要求後に実行先が変わったため承認できません' };
    };
    await click('[data-action="approve-command"]');
    expect(ctx.toast).toHaveBeenCalledWith(I18N.ja['appr.code.target_changed']);
    expect(root.querySelector('.approval-target-changed')).not.toBeNull();
    expect(root.querySelector('[data-action="approve-command"]').disabled).toBe(true);
    expect(ctx.refresh).toHaveBeenCalled();
  });

  // Final audit #7: approving an unedited request with a bare CR sends the
  // requested bytes, not the textarea's LF version.
  it('approves an unedited CR command as requested', async () => {
    state.data.requests = [request('R1', { command: 'echo hi\rrm -rf ~', risk: 'High' })];
    await render();
    const confirm = vi.spyOn(window, 'confirm');
    await click('[data-action="approve-command"]');
    expect(confirm).not.toHaveBeenCalled();
    confirm.mockRestore();
    expect(backend.calls).toContainEqual(['ApproveCommand', 'R1', 'echo hi\rrm -rf ~']);
    expect(backend.calls).not.toContainEqual(['ApproveCommand', 'R1', 'echo hi\nrm -rf ~']);
  });

  it('asks again on risk_increased and approves with ApproveCommandConfirmed on OK', async () => {
    state.data.requests = [request('R1')];
    await render();
    backend.approve = () => ({ ok: false, code: 'risk_increased', message: '修正したコマンドのリスクが Low → High に上がりました' });
    const confirm = vi.spyOn(window, 'confirm').mockReturnValue(true);
    root.querySelector('#approval-command').value = 'rm -rf /tmp/x';
    await click('[data-action="approve-command"]');
    expect(confirm.mock.calls[0][0]).toContain('Low → High');
    expect(confirm.mock.calls[0][0]).toContain(I18N.ja['cmd.riskConfirm']);
    expect(backend.calls).toContainEqual(['ApproveCommandConfirmed', 'R1', 'rm -rf /tmp/x']);
  });

  it('keeps the edit when risk_increased is declined', async () => {
    makeCtx('en');
    state.data.requests = [request('R1')];
    await render();
    backend.approve = () => ({ ok: false, code: 'risk_increased', message: 'JA message' });
    const confirm = vi.spyOn(window, 'confirm').mockReturnValue(false);
    root.querySelector('#approval-command').value = 'rm -rf /tmp/x';
    await click('[data-action="approve-command"]');
    expect(confirm.mock.calls[0][0]).toContain(I18N.en['appr.code.risk_increased']);
    expect(confirm.mock.calls[0][0]).not.toContain('JA message');
    expect(backend.calls.some(([name]) => name === 'ApproveCommandConfirmed')).toBe(false);
    expect(state.dirty.approval).toBe(true);
    expect(root.querySelector('[data-action="approve-command"]').disabled).toBe(false);
    expect(ctx.toast).toHaveBeenCalledWith(I18N.en['appr.riskCanceled']);
  });

  it.each([
    ['not_pending'],
    ['expired'],
    ['shutting_down'],
  ])('shows the localized text for %s and refreshes', async (code) => {
    makeCtx('en');
    state.data.requests = [request('R1')];
    await render();
    backend.approve = () => ({ ok: false, code, message: '日本語' });
    await click('[data-action="approve-command"]');
    expect(ctx.toast).toHaveBeenCalledWith(I18N.en[`appr.code.${code}`]);
    expect(ctx.refresh).toHaveBeenCalledWith({ silent: true });
  });

  it('confirms the bypass grant (8h, Medium, token) before calling the backend', async () => {
    state.data.requests = [request('R1')];
    await render();
    const confirm = vi.spyOn(window, 'confirm').mockReturnValue(false);
    await click('[data-action="approve-bypass-agent"]');
    expect(confirm).toHaveBeenCalledTimes(1);
    expect(confirm.mock.calls[0][0]).toContain('8時間');
    expect(backend.calls.some(([name]) => name === 'ApproveCommandAndBypassAgent')).toBe(false);
    expect(root.querySelector('[data-action="approve-bypass-agent"]').disabled).toBe(false);

    confirm.mockReturnValue(true);
    await click('[data-action="approve-bypass-agent"]');
    expect(backend.calls).toContainEqual(['ApproveCommandAndBypassAgent', 'R1', 'echo R1']);
  });

  it('keeps the request pending and the edit on bearer_required', async () => {
    state.data.requests = [request('R1')];
    await render();
    vi.spyOn(window, 'confirm').mockReturnValue(true);
    backend.bypass = () => ({ ok: false, code: 'bearer_required', message: 'x' });
    await click('[data-action="approve-bypass-agent"]');
    expect(ctx.toast).toHaveBeenCalledWith(I18N.ja['appr.code.bearer_required']);
    expect(state.dirty.approval).toBe(true);
    expect(ctx.refresh).not.toHaveBeenCalled();
  });

  it('does not approve with bypass when the edit raised the risk', async () => {
    state.data.requests = [request('R1')];
    await render();
    vi.spyOn(window, 'confirm').mockReturnValue(true);
    backend.bypass = () => ({ ok: false, code: 'risk_increased', message: 'x' });
    await click('[data-action="approve-bypass-agent"]');
    expect(backend.calls.some(([name]) => name === 'ApproveCommandConfirmed')).toBe(false);
    expect(ctx.toast).toHaveBeenCalledWith(I18N.ja['appr.bypassRiskIncreased']);
  });

  it('disables bypass without any token and explains why', async () => {
    state.data.mcp.bypassAllowed = false;
    state.data.requests = [request('R1')];
    await render();
    const button = root.querySelector('[data-action="approve-bypass-agent"]');
    expect(button.disabled).toBe(true);
    expect(button.closest('.approve-bypass-wrap').title).toBe(I18N.ja['cmd.bypassNeedsBearer']);
  });

  it('still approves when the target lookup fails (the backend decides)', async () => {
    backend.targetError = true;
    state.data.requests = [request('R1')];
    await render();
    expect(root.querySelector('.approval-target-error')).not.toBeNull();
    await click('[data-action="approve-command"]');
    expect(backend.calls).toContainEqual(['ApproveCommand', 'R1', 'echo R1']);
  });

  it('rejects the pinned request and selects rows by id', async () => {
    state.data.requests = [request('R2'), request('R1', { status: 'succeeded' })];
    await render();
    await click('[data-action="reject-command"]');
    expect(backend.calls).toContainEqual(['RejectCommand', 'R2']);

    state.dirty.approval = true;
    expect(await click('tr[data-select-request="R1"] td')).toBe(true);
    expect(state.selectedRequestId).toBe('R1');
    expect(state.dirty.approval).toBe(false);
    expect(ctx.renderPage).toHaveBeenCalled();
  });

  it('copies the selected command through ctx.copyText and leaves delete to main.js', async () => {
    state.data.requests = [request('R1')];
    await render();
    expect(await click('[data-action="copy-command"]')).toBe(true);
    expect(ctx.copyText).toHaveBeenCalledWith('echo R1');
    expect(await click('.card-footer [data-action="delete-command-request"]')).toBe(false);
  });
});
