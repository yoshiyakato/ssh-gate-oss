import { describe, it, expect } from 'vitest';
import { interpolate } from './model.js';
import { RESULT_I18N, resultMessage } from './result-message.js';

const opts = (lang) => ({ lang, dict: RESULT_I18N.en, t: (k, v) => interpolate(RESULT_I18N[lang][k] ?? k, v) });

describe('resultMessage (E6)', () => {
  it('Japanese mode shows the backend message as is', () => {
    expect(resultMessage({ ok: false, code: 'not_found', message: '承認対象のリクエストが見つかりません' }, opts('ja'))).toBe('承認対象のリクエストが見つかりません');
  });
  it('English mode uses the code text when the dictionary has it', () => {
    expect(resultMessage({ ok: false, code: 'not_found', message: '承認対象のリクエストが見つかりません' }, opts('en'))).toBe('Not found');
  });
  it('English mode never shows Japanese: unknown code, no code, success', () => {
    expect(resultMessage({ ok: false, code: 'name_required', message: '接続先名は必須です' }, opts('en'))).toBe('The operation failed (name_required)');
    expect(resultMessage({ ok: false, message: '接続先名は必須です' }, opts('en'))).toBe('The operation failed');
    expect(resultMessage({ ok: true, message: '保存しました' }, opts('en'))).toBe('Done');
  });
  it('English mode keeps a message that has no Japanese (an OS / SSH error)', () => {
    expect(resultMessage({ ok: false, message: 'dial tcp 10.0.0.1:22: i/o timeout' }, opts('en'))).toBe('dial tcp 10.0.0.1:22: i/o timeout');
    expect(resultMessage({ ok: true, message: '' }, opts('en'))).toBe('');
  });
  it('has English and Japanese text for every ActionResult code the backend sends', () => {
    expect(Object.keys(RESULT_I18N.en).sort()).toEqual(Object.keys(RESULT_I18N.ja).sort());
    const codes = ['required', 'duplicate_name', 'not_found', 'storage', 'state_storage', 'in_use', 'shutting_down',
      'exec_failed', 'connect_failed', 'restart_failed', 'invalid_listen', 'token_from_env', 'token_failed',
      'not_password_auth', 'password_required', 'keyring_unavailable', 'resize_failed', 'port_unavailable', 'target_unknown'];
    for (const code of codes) {
      expect(resultMessage({ ok: false, code, message: '日本語のメッセージ' }, opts('en'))).not.toMatch(/日本語|failed \(/);
    }
  });
});
