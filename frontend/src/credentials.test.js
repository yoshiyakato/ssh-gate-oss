import { describe, it, expect, vi, beforeEach } from 'vitest';
vi.mock('./dialog.js', () => ({ askConfirm: vi.fn() }));
import { askConfirm } from './dialog.js';
beforeEach(() => vi.mocked(askConfirm).mockReset());

const calls = vi.hoisted(() => ({ save: [], session: [], deleted: [], saveResult: null, backups: [] }));

vi.mock('../wailsjs/go/main/App', () => ({
  SaveConnection: async (conn) => {
    calls.save.push(conn);
    return calls.saveResult ?? { ok: true, id: conn.id, message: 'saved' };
  },
  UseCredentialForSession: async (id, secret) => {
    calls.session.push([id, secret]);
    return { ok: true, message: 'session' };
  },
  PlaintextBackups: async () => calls.backups,
  DeletePlaintextBackups: async (paths) => {
    calls.deleted.push(paths);
    return { ok: true, message: `deleted ${paths.length}` };
  },
}));

const { interpolate, mergeI18n } = await import('./model.js');
const {
  CREDENTIALS_I18N,
  handleCredentialsClick,
  isCredentialCode,
  mountKeychainNotices,
  recoverCredential,
  resetCredentialNotices,
} = await import('./credentials.js');

const flush = async () => {
  for (let i = 0; i < 6; i += 1) await new Promise((resolve) => setTimeout(resolve, 0));
};

function makeCtx(lang = 'ja') {
  const toasts = [];
  return {
    toasts,
    state: {
      lang,
      data: {
        connections: [{ id: 'c1', name: 'web-01', type: 'SSH', authMethod: 'パスワード保管', credential: '', credentialSet: true }],
        keychainPending: 0,
      },
    },
    t: (key, vars) => interpolate(CREDENTIALS_I18N[lang][key] ?? key, vars),
    toast: (message) => toasts.push(message),
    bumpGen: vi.fn(),
    refresh: vi.fn(async () => {}),
  };
}

// Fills the dialog and presses one of its buttons.
async function answer({ password = '', action }) {
  await flush();
  const modal = document.querySelector('#credential-modal-root .modal');
  expect(modal).not.toBeNull();
  const input = modal.querySelector('#credential-modal-input');
  if (input) input.value = password;
  const button = action === 'cancel'
    ? modal.querySelector('[data-cred-dismiss]')
    : modal.querySelector(`[data-cred-action="${action}"]`);
  expect(button).not.toBeNull();
  if (button.type === 'submit') button.closest('form').dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  else button.dispatchEvent(new MouseEvent('click', { bubbles: true }));
  await flush();
}

const actions = () => [...document.querySelectorAll('#credential-modal-root [data-cred-action]')].map((b) => b.dataset.credAction);

beforeEach(() => {
  document.body.innerHTML = '';
  calls.save = [];
  calls.session = [];
  calls.deleted = [];
  calls.saveResult = null;
  calls.backups = [];
  resetCredentialNotices();
});

describe('credentials dictionary', () => {
  it('has matching ja/en keys under cred.', () => {
    const merged = mergeI18n(CREDENTIALS_I18N);
    expect(Object.keys(merged.ja).sort()).toEqual(Object.keys(merged.en).sort());
    expect(Object.keys(merged.ja).every((key) => key.startsWith('cred.'))).toBe(true);
  });

  it('knows the two credential codes only', () => {
    expect(isCredentialCode('needs_password')).toBe(true);
    expect(isCredentialCode('keyring_unavailable')).toBe(true);
    expect(isCredentialCode('needs_passphrase')).toBe(false);
  });
});

describe('recoverCredential', () => {
  it('needs_password: saves the re-entered password through SaveConnection', async () => {
    const ctx = makeCtx();
    const done = recoverCredential(ctx, { id: 'c1' }, { code: 'needs_password' });
    await flush();
    expect(actions()).toEqual(['session', 'save']);
    await answer({ password: 'pw', action: 'save' });
    expect(await done).toBe(true);
    expect(calls.save).toHaveLength(1);
    expect(calls.save[0]).toMatchObject({ id: 'c1', name: 'web-01', credential: 'pw' });
    expect(calls.session).toEqual([]);
    expect(ctx.refresh).toHaveBeenCalled();
  });

  it('needs_password: a keychain that refuses the save falls back to this session, not the database', async () => {
    const ctx = makeCtx();
    calls.saveResult = { ok: false, code: 'keyring_unavailable', message: 'x' };
    const done = recoverCredential(ctx, { id: 'c1' }, { code: 'needs_password' });
    await answer({ password: 'pw', action: 'save' });
    // The second dialog reuses the typed password and offers the session only.
    expect(actions()).toEqual(['session']);
    expect(document.querySelector('#credential-modal-input')).toBeNull();
    await answer({ action: 'session' });
    expect(await done).toBe(true);
    expect(calls.session).toEqual([['c1', 'pw']]);
    expect(calls.save).toHaveLength(1);
  });

  it('EN: session-only and save toasts are English (the backend messages are Japanese)', async () => {
    const ctx = makeCtx('en');
    let done = recoverCredential(ctx, { id: 'c1' }, { code: 'needs_password' });
    await answer({ password: 'pw', action: 'session' });
    expect(await done).toBe(true);
    done = recoverCredential(ctx, { id: 'c1' }, { code: 'needs_password' });
    await answer({ password: 'pw', action: 'save' });
    expect(await done).toBe(true);
    expect(ctx.toasts).toEqual([
      'The password of web-01 is used for this session only (it is gone when the app quits)',
      'Saved the password of web-01 in the keychain',
    ]);
    const ja = makeCtx('ja');
    done = recoverCredential(ja, { id: 'c1' }, { code: 'needs_password' });
    await answer({ password: 'pw', action: 'session' });
    await done;
    expect(ja.toasts).toEqual(['session']);
  });

  it('keyring_unavailable: offers only 「このセッションだけ使う」', async () => {
    const ctx = makeCtx('en');
    const done = recoverCredential(ctx, { id: 'c1' }, { code: 'keyring_unavailable' });
    await flush();
    expect(actions()).toEqual(['session']);
    expect(document.querySelector('#credential-modal-root').textContent).toContain('never stored in the database');
    await answer({ password: 'pw', action: 'session' });
    expect(await done).toBe(true);
    expect(calls.session).toEqual([['c1', 'pw']]);
    expect(calls.save).toEqual([]);
  });

  it('requires a password and resolves false when dismissed', async () => {
    const ctx = makeCtx();
    const done = recoverCredential(ctx, { id: 'c1' }, { code: 'keyring_unavailable' });
    await answer({ password: '', action: 'session' });
    expect(document.querySelector('#credential-modal-root .invalid-feedback').hidden).toBe(false);
    await answer({ action: 'cancel' });
    expect(await done).toBe(false);
    expect(calls.session).toEqual([]);
  });

  it('ignores other codes and unsaved connections', async () => {
    const ctx = makeCtx();
    expect(await recoverCredential(ctx, { id: 'c1' }, { code: 'needs_passphrase' })).toBe(false);
    expect(await recoverCredential(ctx, { id: '' }, { code: 'needs_password' })).toBe(false);
    expect(document.querySelector('#credential-modal-root .modal')).toBeNull();
  });
});

describe('keychain notices on the settings page', () => {
  it('shows the passwords left in the database and the backups, with paths as text', async () => {
    const ctx = makeCtx();
    ctx.state.data.keychainPending = 2;
    calls.backups = [
      { path: '/x/ssh-gete.db.bak-v3', passwords: 3, unreadable: false },
      { path: '/x/<img src=x onerror=alert(1)>.bak-v2', passwords: 0, unreadable: true },
    ];
    document.body.innerHTML = '<div id="app"><div id="keychain-notices" hidden></div></div>';
    const root = document.querySelector('#keychain-notices');
    mountKeychainNotices(root, ctx);
    await flush();
    expect(root.hidden).toBe(false);
    expect(root.querySelector('.keychain-pending').textContent).toContain('2 件のパスワード');
    const paths = [...root.querySelectorAll('.plaintext-backup-path')].map((el) => el.textContent);
    expect(paths).toEqual(['/x/ssh-gete.db.bak-v3', '/x/<img src=x onerror=alert(1)>.bak-v2']);
    expect(root.querySelector('img')).toBeNull();
    expect(root.textContent).toContain('パスワード 3 件');
    expect(root.textContent).toContain('中身を確認できません');

    const confirm = vi.mocked(askConfirm).mockResolvedValue(true);
    calls.backups = [];
    const handled = await handleCredentialsClick({ target: root.querySelector('[data-action="cred-delete-backups"]') }, ctx);
    confirm.mockRestore();
    expect(handled).toBe(true);
    expect(calls.deleted).toEqual([['/x/ssh-gete.db.bak-v3', '/x/<img src=x onerror=alert(1)>.bak-v2']]);
    expect(root.querySelector('.plaintext-backups')).toBeNull();
    expect(ctx.toasts).toContain('deleted 2');
  });

  it('EN: the deletion toast is English, not the Japanese backend message', async () => {
    resetCredentialNotices();
    const ctx = makeCtx('en');
    calls.backups = [{ path: '/x/ssh-gete.db.bak-v3', passwords: 3, unreadable: false }];
    calls.deleted = [];
    document.body.innerHTML = '<div id="keychain-notices" hidden></div>';
    const root = document.querySelector('#keychain-notices');
    mountKeychainNotices(root, ctx);
    await flush();
    const confirm = vi.mocked(askConfirm).mockResolvedValue(true);
    calls.backups = [];
    await handleCredentialsClick({ target: root.querySelector('[data-action="cred-delete-backups"]') }, ctx);
    confirm.mockRestore();
    expect(ctx.toasts).toEqual(['Deleted 1 backup(s) containing plaintext passwords']);
  });

  it('stays hidden when there is nothing to say', async () => {
    const ctx = makeCtx();
    document.body.innerHTML = '<div id="keychain-notices" hidden></div>';
    const root = document.querySelector('#keychain-notices');
    mountKeychainNotices(root, ctx);
    await flush();
    expect(root.hidden).toBe(true);
    expect(root.childElementCount).toBe(0);
  });
});
