import { describe, it, expect, beforeEach } from 'vitest';
import { askConfirm } from './dialog.js';

const press = (role) => document.querySelector(`[data-confirm-dialog="${role}"]`).click();

describe('askConfirm', () => {
  beforeEach(() => {
    document.body.innerHTML = '';
  });

  it('shows the message as text with its line breaks and resolves true on OK', async () => {
    const answer = askConfirm('<b>x</b>\nline 2', { title: 'T', okLabel: 'Go', lang: 'en' });
    await Promise.resolve();
    await Promise.resolve();
    const body = document.querySelector('.app-confirm-root .modal-body p');
    expect(body.textContent).toBe('<b>x</b>\nline 2');
    expect(body.querySelector('b')).toBeNull();
    expect(document.querySelector('.modal-title').textContent).toBe('T');
    expect(document.querySelector('[data-confirm-dialog="cancel"]').textContent).toBe('Cancel');
    press('ok');
    await expect(answer).resolves.toBe(true);
    expect(document.querySelector('.app-confirm-root')).toBeNull();
  });

  it('resolves false on Cancel and on Escape', async () => {
    let answer = askConfirm('a', { lang: 'ja' });
    await Promise.resolve();
    await Promise.resolve();
    expect(document.querySelector('[data-confirm-dialog="cancel"]').textContent).toBe('キャンセル');
    press('cancel');
    await expect(answer).resolves.toBe(false);

    answer = askConfirm('b');
    await Promise.resolve();
    await Promise.resolve();
    document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }));
    await expect(answer).resolves.toBe(false);
  });
});
