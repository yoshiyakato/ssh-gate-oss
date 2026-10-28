// askConfirm is the app's replacement for window.confirm. The macOS WebView
// of Wails v2 has no JavaScript confirm panel, so window.confirm returns
// false there without showing anything and every "OK / Cancel" step silently
// cancelled. This draws an in-page modal instead and resolves true for OK.
//
// The message is set as text (never markup) and keeps its line breaks.

let chain = Promise.resolve();

const LABELS = {
  ja: { ok: 'OK', cancel: 'キャンセル' },
  en: { ok: 'OK', cancel: 'Cancel' },
};

function labelsFor(lang) {
  return LABELS[lang] ?? LABELS[document.documentElement.lang] ?? LABELS.ja;
}

export function askConfirm(message, { title = '', okLabel = '', cancelLabel = '', danger = false, lang = '' } = {}) {
  const labels = labelsFor(lang);
  const run = chain.then(() => new Promise((resolve) => {
    const previousFocus = document.activeElement;
    const host = document.createElement('div');
    host.className = 'app-confirm-root';
    host.innerHTML = `
      <div class="modal-backdrop show"></div>
      <div class="modal d-block" tabindex="-1" role="dialog" aria-modal="true" data-confirm-dialog="backdrop">
        <div class="modal-dialog modal-dialog-centered">
          <div class="modal-content">
            <div class="modal-header"><h5 class="modal-title fw-bold"></h5></div>
            <div class="modal-body"><p class="mb-0" style="white-space: pre-wrap"></p></div>
            <div class="modal-footer">
              <button class="btn btn-outline-secondary" type="button" data-confirm-dialog="cancel"></button>
              <button class="btn" type="button" data-confirm-dialog="ok"></button>
            </div>
          </div>
        </div>
      </div>`;
    const header = host.querySelector('.modal-header');
    if (title) header.querySelector('.modal-title').textContent = title;
    else header.remove();
    host.querySelector('.modal-body p').textContent = String(message ?? '');
    host.querySelector('[data-confirm-dialog="cancel"]').textContent = cancelLabel || labels.cancel;
    const ok = host.querySelector('[data-confirm-dialog="ok"]');
    ok.textContent = okLabel || labels.ok;
    ok.classList.add(danger ? 'btn-danger' : 'btn-primary');
    document.body.appendChild(host);

    let done = false;
    const finish = (value) => {
      if (done) return;
      done = true;
      host.remove();
      document.removeEventListener('keydown', onKey, true);
      if (previousFocus?.isConnected && typeof previousFocus.focus === 'function') previousFocus.focus();
      resolve(value);
    };
    const onKey = (event) => {
      if (event.key === 'Escape') {
        event.preventDefault();
        finish(false);
      }
    };
    host.addEventListener('click', (event) => {
      const role = event.target.closest?.('[data-confirm-dialog]')?.dataset.confirmDialog;
      if (role === 'ok') finish(true);
      else if (role === 'cancel' || (role === 'backdrop' && event.target.dataset.confirmDialog === 'backdrop')) finish(false);
    });
    document.addEventListener('keydown', onKey, true);
    ok.focus();
  }));
  chain = run.catch(() => {});
  return run;
}
