import { describe, it, expect, vi } from 'vitest';

// i18n.js pulls in every page module; none of them may touch Wails or xterm
// at import time, but their imports must resolve.
vi.mock('../wailsjs/go/main/App', () => ({}));
vi.mock('../wailsjs/runtime/runtime', () => ({ EventsOn: vi.fn() }));
vi.mock('@xterm/xterm', () => ({ Terminal: class {} }));
vi.mock('@xterm/addon-fit', () => ({ FitAddon: class {} }));

const { interpolate } = await import('./model.js');
const { I18N } = await import('./i18n.js');
const { PAGES, pageTitle, renderNav, renderSkeleton } = await import('./shell.js');

const tFor = (lang) => (key, vars) => interpolate(I18N[lang][key] ?? key, vars);

describe('i18n', () => {
  it('merges every module dictionary with matching ja/en keys', () => {
    expect(Object.keys(I18N.ja).sort()).toEqual(Object.keys(I18N.en).sort());
    for (const prefix of ['nav.', 'tabs.', 'side.', 'edit.', 'state.', 'appr.', 'agents.', 'srvtab.', 'keys.', 'srvfilter.', 'cred.']) {
      expect(Object.keys(I18N.ja).some((key) => key.startsWith(prefix))).toBe(true);
    }
  });

  it('has a title for every page', () => {
    for (const lang of ['ja', 'en']) {
      for (const page of PAGES) {
        const title = pageTitle(page, tFor(lang));
        expect(title).not.toMatch(/^(nav|title)\./);
      }
    }
  });
});

describe('renderSkeleton', () => {
  for (const lang of ['ja', 'en']) {
    it(`renders the fixed containers once (${lang})`, () => {
      const html = renderSkeleton({ t: tFor(lang), logoUrl: 'logo.png' });
      expect(html).toMatchSnapshot();
      document.body.innerHTML = `<div id="app">${html}</div>`;
      for (const id of ['sidebar-nav', 'sidebar-status', 'server-tabs', 'shell-subtabs', 'page-root', 'terminal-dock', 'modal-root', 'page-title', 'header-title']) {
        expect(document.querySelectorAll(`#${id}`)).toHaveLength(1);
      }
      // #terminal-dock must sit outside #page-root so renderPage never replaces it.
      expect(document.querySelector('#page-root #terminal-dock')).toBeNull();
      // The tab bars sit outside #page-root so a page redraw keeps their nodes.
      expect(document.querySelector('#page-root #server-tabs, #page-root #shell-subtabs')).toBeNull();
      expect(document.querySelector('#shell-subtabs').hidden).toBe(true);
      expect(document.querySelector('#sidebar-status').closest('.app-sidebar')).not.toBeNull();
    });
  }

  it('renders nav with the active page and a pending badge', () => {
    const html = renderNav('commands', tFor('en'), { commands: 3 });
    document.body.innerHTML = html;
    expect(document.querySelectorAll('[data-nav]')).toHaveLength(4);
    expect(document.querySelector('[data-nav="commands"]').classList.contains('active')).toBe(true);
    expect(document.querySelector('[data-nav="commands"] .nav-badge').textContent).toBe('3');
    expect(document.querySelector('[data-nav="terminal"] .nav-badge')).toBeNull();
  });
});
