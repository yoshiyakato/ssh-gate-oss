// Keychain recovery and notices (phase5-keychain.md §7, §8, §10; REQ-014).
//
// - recoverCredential: the common dialog for needs_password (the saved item is
//   gone: enter it again and save it, or use it for this session only) and
//   keyring_unavailable (the keychain cannot be used now: this session only).
//   Saving the password in plaintext to the database is never offered.
// - mountKeychainNotices: on the settings page, the passwords the startup
//   migration could not move and the old backups that still hold plaintext
//   passwords, with a [Delete] button.
//
// This is a main.js helper (like serverview.js): it is wired through ctx and
// is not imported by the page modules. Paths, names and backend messages go
// into the DOM through textContent only.

import {
  DeletePlaintextBackups,
  PlaintextBackups,
  SaveConnection,
  UseCredentialForSession,
} from '../wailsjs/go/main/App';
import { askConfirm } from './dialog.js';

export const CREDENTIALS_I18N = {
  ja: {
    'cred.needsPassword.title': 'パスワードを入力し直してください',
    'cred.needsPassword.body': '「{name}」のパスワードがキーチェーンに見つかりません（キーチェーンから削除されたか、別のPCから設定を移した可能性があります）。',
    'cred.unavailable.title': 'キーチェーンを利用できません',
    'cred.unavailable.body': '「{name}」のパスワードをキーチェーンで扱えません（ロック中、または利用できない環境です）。このセッションの間だけ使うパスワードを入力できます。パスワードをデータベースに平文で保存することはありません。',
    'cred.saveUnavailable.body': 'キーチェーンにパスワードを保存できませんでした。入力したパスワードは「このセッションだけ使う」ことができます（アプリを終了すると消えます）。',
    'cred.password': 'パスワード',
    'cred.sessionHelp': '「このセッションだけ使う」はメモリにだけ保持し、終了すると消えます。',
    'cred.save': 'キーチェーンに保存',
    'cred.session': 'このセッションだけ使う',
    'cred.cancel': 'キャンセル',
    'cred.close': '閉じる',
    'cred.empty': 'パスワードを入力してください',
    'cred.pending': '{n} 件のパスワードをキーチェーンへ移せませんでした（データベースに残しています）。次回の起動で再試行します。',
    'cred.backups.title': '古いバックアップに平文のパスワードが残っています',
    'cred.backups.body': '移行前に作られたバックアップです。不要なら削除してください（Time Machine やクラウド同期にある複製は消えません）。',
    'cred.backups.count': 'パスワード {n} 件',
    'cred.backups.unreadable': '中身を確認できません（パスワードを含む可能性があります）',
    'cred.backups.delete': '削除',
    'cred.backups.confirm': '次の {n} 個のファイルを削除します。元に戻せません。\n\n{paths}',
    'cred.sessionUsed': '{name} のパスワードをこのセッションだけ使います（アプリを終了すると消えます）',
    'cred.sessionFailed': 'このセッション用のパスワードを保持できませんでした',
    'cred.saved': '{name} のパスワードをキーチェーンに保存しました',
    'cred.saveFailed': 'パスワードを保存できませんでした',
    'cred.backups.deleted': '平文のパスワードを含むバックアップを {n} 件削除しました',
    'cred.backups.deleteFailed': '{n} 件を削除しましたが、削除できないファイルがあります',
  },
  en: {
    'cred.needsPassword.title': 'Enter the password again',
    'cred.needsPassword.body': 'The password of "{name}" is not in the keychain (it was removed from the keychain, or the settings came from another computer).',
    'cred.unavailable.title': 'Keychain unavailable',
    'cred.unavailable.body': 'The password of "{name}" cannot be handled by the keychain (it is locked or not available here). You can enter a password to use for this session only. Passwords are never stored in the database in plaintext.',
    'cred.saveUnavailable.body': 'The password could not be saved in the keychain. You can use the entered password for this session only (it is gone when the app quits).',
    'cred.password': 'Password',
    'cred.sessionHelp': '"Use for this session only" keeps it in memory until the app quits.',
    'cred.save': 'Save in keychain',
    'cred.session': 'Use for this session only',
    'cred.cancel': 'Cancel',
    'cred.close': 'Close',
    'cred.empty': 'Enter the password',
    'cred.pending': '{n} password(s) could not be moved to the keychain (kept in the database). This is retried at the next start.',
    'cred.backups.title': 'Old backups still contain plaintext passwords',
    'cred.backups.body': 'These backups were made before the migration. Delete them if you no longer need them (copies in Time Machine or cloud sync are not removed).',
    'cred.backups.count': '{n} password(s)',
    'cred.backups.unreadable': 'Cannot be inspected (may contain passwords)',
    'cred.backups.delete': 'Delete',
    'cred.backups.confirm': 'Delete these {n} file(s)? This cannot be undone.\n\n{paths}',
    'cred.sessionUsed': 'The password of {name} is used for this session only (it is gone when the app quits)',
    'cred.sessionFailed': 'The password could not be kept for this session',
    'cred.saved': 'Saved the password of {name} in the keychain',
    'cred.saveFailed': 'The password could not be saved',
    'cred.backups.deleted': 'Deleted {n} backup(s) containing plaintext passwords',
    'cred.backups.deleteFailed': 'Deleted {n} file(s), but some files could not be deleted',
  },
};

export const CREDENTIAL_CODES = Object.freeze(['needs_password', 'keyring_unavailable']);

export function isCredentialCode(code) {
  return CREDENTIAL_CODES.includes(code);
}

function el(tag, attrs = {}, ...children) {
  const node = document.createElement(tag);
  for (const [key, value] of Object.entries(attrs)) {
    if (value === undefined || value === null || value === false) continue;
    if (key === 'text') node.textContent = String(value);
    else if (key === 'class') node.className = value;
    else if (key === 'dataset') Object.assign(node.dataset, value);
    else node.setAttribute(key, value === true ? '' : String(value));
  }
  for (const child of children.flat()) {
    if (child === null || child === undefined || child === false) continue;
    node.append(child instanceof Node ? child : document.createTextNode(String(child)));
  }
  return node;
}

function modalRoot() {
  let root = document.getElementById('credential-modal-root');
  if (!root) {
    root = document.createElement('div');
    root.id = 'credential-modal-root';
    document.body.appendChild(root);
  }
  return root;
}

let modalChain = Promise.resolve();

// promptCredential shows one dialog and resolves with { action, password }
// (action 'save' | 'session') or null when it is dismissed. mode:
//   needs_password      → input + [save] [session]
//   keyring_unavailable → input + [session]
//   save_unavailable    → the typed password is reused: [session] only
export function promptCredential(ctx, { name, mode, password = '' }) {
  const { t } = ctx;
  const run = modalChain.then(() => new Promise((resolve) => {
    const previousFocus = document.activeElement;
    const titleKey = mode === 'needs_password' ? 'cred.needsPassword.title' : 'cred.unavailable.title';
    const bodyKey = mode === 'needs_password' ? 'cred.needsPassword.body'
      : mode === 'save_unavailable' ? 'cred.saveUnavailable.body' : 'cred.unavailable.body';
    const input = mode === 'save_unavailable' ? null : el('input', {
      class: 'form-control', id: 'credential-modal-input', type: 'password',
      autocomplete: 'current-password', autocapitalize: 'none', spellcheck: 'false',
    });
    const error = el('div', { class: 'invalid-feedback d-block', hidden: true });
    const buttons = [el('button', { class: 'btn btn-outline-secondary', type: 'button', dataset: { credDismiss: '1' }, text: t('cred.cancel') })];
    buttons.push(el('button', { class: `btn ${mode === 'needs_password' ? 'btn-outline-primary' : 'btn-primary'}`, type: 'button', dataset: { credAction: 'session' }, text: t('cred.session') }));
    if (mode === 'needs_password') {
      buttons.push(el('button', { class: 'btn btn-primary', type: 'submit', dataset: { credAction: 'save' }, text: t('cred.save') }));
    }
    const form = el('form', { class: 'modal-content', novalidate: true },
      el('div', { class: 'modal-header' },
        el('h5', { class: 'modal-title fw-bold', id: 'credential-modal-title', text: t(titleKey) }),
        el('button', { class: 'btn-close', type: 'button', dataset: { credDismiss: '1' }, 'aria-label': t('cred.close') })),
      el('div', { class: 'modal-body' },
        el('p', { class: 'mb-3', text: t(bodyKey, { name }) }),
        input ? el('label', { class: 'form-label fw-bold', for: 'credential-modal-input', text: t('cred.password') }) : null,
        input,
        error,
        el('div', { class: 'form-text', text: t('cred.sessionHelp') })),
      el('div', { class: 'modal-footer' }, buttons));
    const host = el('div', {},
      el('div', { class: 'modal-backdrop show' }),
      el('div', { class: 'modal d-block', tabindex: '-1', role: 'dialog', 'aria-modal': 'true', 'aria-labelledby': 'credential-modal-title' },
        el('div', { class: 'modal-dialog modal-dialog-centered' }, form)));
    modalRoot().appendChild(host);

    let done = false;
    const finish = (result) => {
      if (done) return;
      done = true;
      host.remove();
      if (previousFocus?.isConnected && typeof previousFocus.focus === 'function') previousFocus.focus();
      resolve(result);
    };
    const pick = (action) => {
      const value = input ? input.value : password;
      if (!value) {
        error.textContent = t('cred.empty');
        error.hidden = false;
        input?.focus();
        return;
      }
      finish({ action, password: value });
    };
    host.addEventListener('click', (event) => {
      if (event.target.closest('[data-cred-dismiss]')) {
        finish(null);
        return;
      }
      const button = event.target.closest('[data-cred-action]');
      if (button && button.type !== 'submit') pick(button.dataset.credAction);
    });
    form.addEventListener('submit', (event) => {
      event.preventDefault();
      pick(mode === 'needs_password' ? 'save' : 'session');
    });
    host.addEventListener('keydown', (event) => {
      if (event.key === 'Escape') {
        event.preventDefault();
        finish(null);
      }
    });
    (input ?? form.querySelector('[data-cred-action]'))?.focus();
  }));
  modalChain = run.catch(() => {});
  return run;
}

function savedConnection(ctx, connId) {
  return (ctx.state.data.connections ?? []).find((conn) => conn.id === connId) ?? null;
}

// localized picks the backend message in JA (it can name details) and a
// fixed text in EN: the backend messages are Japanese only.
function localized(ctx, result, enText) {
  if (ctx.state?.lang === 'en') return enText;
  return String(result?.message ?? '') || enText;
}

async function useForSession(ctx, connId, name, password) {
  let result;
  try {
    result = await UseCredentialForSession(connId, password);
  } catch (error) {
    result = { ok: false, message: String(error) };
  }
  ctx.toast(localized(ctx, result, ctx.t(result?.ok ? 'cred.sessionUsed' : 'cred.sessionFailed', { name })));
  return Boolean(result?.ok);
}

// recoverCredential handles a needs_password / keyring_unavailable result
// for the saved connection conn. It resolves true when a password is now
// available (saved again, or kept for this session) and the caller may retry.
// opts.password: a password the user already typed (a save that failed with
// keyring_unavailable); it is offered for this session without asking again.
export async function recoverCredential(ctx, conn, result, { password = '' } = {}) {
  const connId = conn?.id ?? '';
  if (!connId || !isCredentialCode(result?.code)) return false;
  const name = savedConnection(ctx, connId)?.name || conn.name || connId;
  const mode = password ? 'save_unavailable' : result.code;
  const choice = await promptCredential(ctx, { name, mode, password });
  if (!choice) return false;
  if (choice.action === 'session') return useForSession(ctx, connId, name, choice.password);

  const saved = savedConnection(ctx, connId);
  if (!saved) return false;
  ctx.bumpGen();
  let save;
  try {
    save = await SaveConnection({ ...saved, credential: choice.password });
  } catch (error) {
    save = { ok: false, message: String(error) };
  }
  if (save?.ok) {
    ctx.toast(localized(ctx, save, ctx.t('cred.saved', { name })));
    await ctx.refresh({ silent: true });
    return true;
  }
  if (save?.code === 'keyring_unavailable') {
    // The keychain refused the write: offer the typed password for this
    // session instead (never the database).
    return recoverCredential(ctx, conn, save, { password: choice.password });
  }
  ctx.toast(localized(ctx, save, ctx.t('cred.saveFailed')));
  return false;
}

// ------------------------------------------------------------- settings

const BACKUPS_RELOAD_MS = 10000;
const backups = { list: [], loadedAt: 0, loading: false, busy: false };
let mounted = null; // { root, ctx }

export function resetCredentialNotices() {
  backups.list = [];
  backups.loadedAt = 0;
  backups.loading = false;
  backups.busy = false;
  mounted = null;
}

async function loadBackups() {
  backups.loading = true;
  try {
    backups.list = (await PlaintextBackups()) ?? [];
  } catch {
    backups.list = [];
  } finally {
    backups.loading = false;
    backups.loadedAt = Date.now();
  }
  if (mounted?.root?.isConnected) drawNotices(mounted.root, mounted.ctx);
}

function drawNotices(root, ctx) {
  const { t, state } = ctx;
  const children = [];
  const pending = Number(state.data.keychainPending ?? 0);
  if (pending > 0) {
    children.push(el('div', { class: 'alert alert-warning mb-3 keychain-pending', role: 'alert', text: t('cred.pending', { n: pending }) }));
  }
  if (backups.list.length) {
    children.push(el('div', { class: 'alert alert-warning mb-3 plaintext-backups', role: 'alert' },
      el('div', { class: 'd-flex align-items-start justify-content-between gap-3' },
        el('div', {},
          el('div', { class: 'fw-bold', text: t('cred.backups.title') }),
          el('div', { class: 'small mb-2', text: t('cred.backups.body') })),
        el('button', {
          class: 'btn btn-sm btn-danger flex-shrink-0', type: 'button',
          dataset: { action: 'cred-delete-backups' }, disabled: backups.busy, text: t('cred.backups.delete'),
        })),
      el('ul', { class: 'mb-0 small plaintext-backup-list' },
        backups.list.map((b) => el('li', {},
          el('code', { class: 'plaintext-backup-path', text: b.path }),
          ' — ',
          b.unreadable ? t('cred.backups.unreadable') : t('cred.backups.count', { n: b.passwords }))))));
  }
  root.replaceChildren(...children);
  root.hidden = children.length === 0;
}

// mountKeychainNotices draws the settings notices into root and refreshes
// the backup list unless it is fresh.
export function mountKeychainNotices(root, ctx) {
  if (!root) return;
  mounted = { root, ctx };
  drawNotices(root, ctx);
  if (!backups.loading && Date.now() - backups.loadedAt > BACKUPS_RELOAD_MS) loadBackups();
}

export async function handleCredentialsClick(event, ctx) {
  const button = event.target.closest?.('[data-action="cred-delete-backups"]');
  if (!button) return false;
  if (backups.busy || !backups.list.length) return true;
  const paths = backups.list.map((b) => b.path);
  if (!(await askConfirm(ctx.t('cred.backups.confirm', { n: paths.length, paths: paths.join('\n') }), { danger: true, lang: ctx.lang?.() }))) return true;
  backups.busy = true;
  if (mounted?.root?.isConnected) drawNotices(mounted.root, ctx);
  let result;
  try {
    result = await DeletePlaintextBackups(paths);
  } catch (error) {
    result = { ok: false, message: String(error) };
  } finally {
    backups.busy = false;
  }
  await loadBackups();
  // Removed = asked for and no longer reported.
  const left = new Set(backups.list.map((b) => b.path));
  const n = paths.filter((path) => !left.has(path)).length;
  ctx.toast(localized(ctx, result, ctx.t(result?.ok ? 'cred.backups.deleted' : 'cred.backups.deleteFailed', { n })));
  return true;
}
