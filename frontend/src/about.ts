// Version, and whether there is a newer one.
//
// The check is always something the writer asked for. An offline-first editor
// that contacts a server on its own is not offline-first, and nothing here
// runs on a timer.

import { AppVersion, CheckForUpdate } from '../wailsjs/go/main/App';
import { BrowserOpenURL } from '../wailsjs/runtime/runtime';
import type { update } from '../wailsjs/go/models';

function element<T extends HTMLElement>(html: string): T {
  const template = document.createElement('template');
  template.innerHTML = html.trim();
  return template.content.firstElementChild as T;
}

function escape(text: string): string {
  const node = document.createElement('span');
  node.textContent = text;
  return node.innerHTML;
}

/** Opens the about sheet and checks for an update. */
export async function openAboutSheet() {
  const overlay = document.createElement('div');
  overlay.className = 'palette-overlay';
  overlay.innerHTML = `<div class="palette sheet" role="dialog" aria-modal="true" aria-label="About Lotus"></div>`;
  document.body.append(overlay);

  const panel = overlay.querySelector<HTMLDivElement>('.sheet')!;
  const close = () => overlay.remove();
  overlay.addEventListener('mousedown', (event) => {
    if (event.target === overlay) close();
  });
  overlay.addEventListener('keydown', (event) => {
    if (event.key === 'Escape') {
      event.preventDefault();
      close();
    }
  });

  const version = await AppVersion();

  const draw = (result: update.Result | null) => {
    panel.replaceChildren();

    const body = element<HTMLDivElement>(`
      <div class="sheet-body">
        <h2 class="sheet-title">Lotus</h2>
        <p class="sheet-text">Version ${escape(version)}</p>
        <div class="about-update"></div>
      </div>
    `);
    const slot = body.querySelector<HTMLDivElement>('.about-update')!;

    if (result === null) {
      slot.append(element('<p class="sheet-note">Checking for updates…</p>'));
    } else if (result.development) {
      slot.append(
        element(`<p class="sheet-note">
          This is a development build, so there is nothing to update to.
          ${result.latest ? `The latest release is ${escape(result.latest)}.` : ''}
        </p>`),
      );
    } else if (result.error) {
      slot.append(element(`<p class="sheet-note">${escape(result.error)}</p>`));
    } else if (result.newer) {
      slot.append(element(`<p class="sheet-text">Version ${escape(result.latest)} is available.</p>`));
    } else {
      slot.append(element('<p class="sheet-note">This is the latest release.</p>'));
    }
    panel.append(body);

    const actions = element<HTMLDivElement>('<div class="sheet-actions"></div>');
    if (result?.newer && result.url) {
      const get = document.createElement('button');
      get.className = 'btn';
      get.textContent = 'Get it';
      // The browser, not an in-app download: replacing a running, signed
      // application is the business of an installer, not of the app itself.
      get.addEventListener('click', () => BrowserOpenURL(result.url));
      actions.append(get);
    }
    const done = document.createElement('button');
    done.className = 'btn btn-quiet';
    done.textContent = 'Done';
    done.addEventListener('click', close);
    actions.append(done);
    panel.append(actions);
  };

  draw(null);
  try {
    draw(await CheckForUpdate());
  } catch (err) {
    draw({ current: version, latest: '', url: '', newer: false, development: false, error: String(err) });
  }
}
