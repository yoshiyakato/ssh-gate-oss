// App skeleton (phase2-contract §2.2). renderSkeleton() is written into #app
// exactly once at startup; afterwards main.js only rewrites the parts it owns
// (#header-title, #page-title, #breadcrumb-*, #sidebar-nav, #sidebar-status,
// #server-tabs, #shell-subtabs, #page-root, #modal-root). #server-tabs and
// #shell-subtabs sit outside #page-root so the 5s redraw keeps their nodes
// and focus (phase4-ui-integration §0, §4.2). #terminal-dock is never rewritten, so the xterm
// panes F1 mounts there survive every render and page switch.

import { escapeHtml } from './model.js';

export const PAGES = ['connections', 'commands', 'terminal', 'aisessions', 'settings'];

const NAV_ICONS = {
  connections: 'bi-hdd-network',
  commands: 'bi-terminal',
  terminal: 'bi-terminal-fill',
  aisessions: 'bi-robot',
  settings: 'bi-broadcast-pin',
};

export function pageTitle(active, t) {
  if (active === 'connections') return t('nav.connections');
  if (active === 'commands') return t('nav.commands');
  if (active === 'terminal') return t('nav.terminal');
  if (active === 'aisessions') return t('nav.aisessions');
  return t('title.settings');
}

// renderNav returns the innerHTML of #sidebar-nav. badges maps a page to the
// count shown next to it (0 hides the badge).
export function renderNav(active, t, badges = {}) {
  const item = (id) => {
    const badge = badges[id] ?? 0;
    return `
      <li class="nav-item">
        <button class="nav-link ${active === id ? 'active' : ''}" type="button" data-nav="${id}"${active === id ? ' aria-current="page"' : ''}>
          <i class="nav-icon bi ${NAV_ICONS[id]}"></i>
          <p>${escapeHtml(t(`nav.${id}`))}${badge ? `<span class="nav-badge badge text-bg-warning ms-2">${escapeHtml(badge)}</span>` : ''}</p>
        </button>
      </li>`;
  };
  return `<ul class="nav sidebar-menu flex-column" role="menu">${PAGES.map(item).join('')}
    </ul>`;
}

export function renderSkeleton({ t, logoUrl = '' }) {
  return `
    <div class="app-wrapper">
      <nav class="app-header navbar navbar-expand bg-body">
        <div class="container-fluid">
          <ul class="navbar-nav">
            <li class="nav-item">
              <button class="nav-link btn btn-link" id="sidebar-toggle" data-lte-toggle="sidebar" type="button" aria-label="${escapeHtml(t('sidebar.toggle'))}">
                <i class="bi bi-list"></i>
              </button>
            </li>
            <li class="nav-item d-none d-md-block">
              <span class="nav-link fw-bold" id="header-title"></span>
            </li>
          </ul>
          <ul class="navbar-nav ms-auto">
            <li class="nav-item">
              <span class="nav-link">
                <i class="bi bi-person-circle me-1"></i>
                admin
              </span>
            </li>
          </ul>
        </div>
      </nav>

      <aside class="app-sidebar bg-primary-subtle shadow" data-bs-theme="dark">
        <div class="sidebar-brand">
          <button class="brand-link btn btn-link text-start" type="button" data-nav="connections">
            <span class="brand-image d-inline-flex align-items-center justify-content-center">
              <img src="${escapeHtml(logoUrl)}" alt="SSH GATE" style="width:30px;height:30px;border-radius:7px;object-fit:cover;" />
            </span>
            <span class="brand-text fw-semibold">SSH GATE</span>
          </button>
        </div>
        <div class="sidebar-wrapper">
          <nav class="mt-2" id="sidebar-nav"></nav>
        </div>
        <div class="sidebar-status" id="sidebar-status"></div>
      </aside>

      <main class="app-main">
        <div class="app-content-header">
          <div class="container-fluid">
            <div class="row">
              <div class="col-sm-6">
                <h3 class="mb-0" id="page-title"></h3>
              </div>
              <div class="col-sm-6">
                <ol class="breadcrumb float-sm-end">
                  <li class="breadcrumb-item" id="breadcrumb-root">${escapeHtml(t('breadcrumb.root'))}</li>
                  <li class="breadcrumb-item active" aria-current="page" id="breadcrumb-current"></li>
                </ol>
              </div>
            </div>
          </div>
        </div>
        <div class="app-content">
          <div class="container-fluid">
            <div id="server-tabs" class="server-tabs-bar"></div>
            <div id="shell-subtabs" class="shell-subtabs-bar" hidden></div>
            <div id="page-root"></div>
            <div id="terminal-dock"></div>
          </div>
        </div>
      </main>
      <div id="modal-root"></div>
    </div>
  `;
}
