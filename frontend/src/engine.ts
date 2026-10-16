// The TeX engine: what the writer sees when there isn't one.
//
// This is the first thing a new install can get wrong, and the one failure a
// writer cannot work around. Everything here exists so that the answer to "it
// says it can't compile" is a button, not a terminal.

import { EngineStatus, InstallEngine } from '../wailsjs/go/main/App';
import { EventsOff, EventsOn } from '../wailsjs/runtime/runtime';
import type { main } from '../wailsjs/go/models';

/** Fired whenever the engine situation changes, so the status bar can follow. */
type Listener = (info: main.EngineInfo) => void;

const listeners = new Set<Listener>();

export function onEngineChange(fn: Listener) {
  listeners.add(fn);
}

function announce(info: main.EngineInfo) {
  for (const fn of listeners) fn(info);
}

/** Asks the backend where things stand, and tells anyone listening. */
export async function refreshEngine(): Promise<main.EngineInfo> {
  const info = await EngineStatus();
  announce(info);
  return info;
}

/** A short description for the status bar. */
export function engineLabel(info: main.EngineInfo): string {
  if (info.available) return info.version;
  if (info.installing) return 'Installing the engine…';
  return 'No engine';
}

function element<T extends HTMLElement>(html: string): T {
  const template = document.createElement('template');
  template.innerHTML = html.trim();
  return template.content.firstElementChild as T;
}

/** Text as text: a path or an engine's own output is not markup. */
function escape(text: string): string {
  const node = document.createElement('span');
  node.textContent = text;
  return node.innerHTML;
}

/** Megabytes, for a progress line people can read. */
function megabytes(bytes: number): string {
  return (bytes / (1024 * 1024)).toFixed(1);
}

/**
 * Opens the engine sheet.
 *
 * `dismissable` is false on first run: with no engine the app cannot do the
 * one thing it is for, so the sheet is the app until that is resolved.
 */
export function openEngineSheet(info: main.EngineInfo, dismissable = true) {
  const overlay = document.createElement('div');
  overlay.className = 'palette-overlay';
  overlay.innerHTML = `<div class="palette sheet" role="dialog" aria-modal="true" aria-label="TeX engine"></div>`;
  document.body.append(overlay);

  const panel = overlay.querySelector<HTMLDivElement>('.sheet')!;
  const close = () => {
    EventsOff('engine:progress');
    overlay.remove();
  };

  if (dismissable) {
    overlay.addEventListener('mousedown', (event) => {
      if (event.target === overlay) close();
    });
    overlay.addEventListener('keydown', (event) => {
      if (event.key === 'Escape') {
        event.preventDefault();
        close();
      }
    });
  }

  let current = info;
  let installing = false;

  const draw = () => {
    panel.replaceChildren();

    const body = element<HTMLDivElement>(`
      <div class="sheet-body">
        <h2 class="sheet-title">TeX engine</h2>
        <div class="engine-detail"></div>
      </div>
    `);
    const detail = body.querySelector<HTMLDivElement>('.engine-detail')!;

    if (current.available) {
      detail.append(
        element(`<p class="sheet-text">${escape(current.version)} is ready.</p>`),
        element(`<p class="sheet-note">${sourceExplanation(current)}</p>`),
        element(`<p class="sheet-note engine-path">${escape(current.path)}</p>`),
      );
    } else if (installing) {
      detail.append(
        element(`<p class="sheet-text">Downloading Tectonic ${escape(current.installVersion)}…</p>`),
        element(`
          <div class="engine-progress">
            <div class="engine-bar"><div class="engine-bar-fill" id="engine-bar-fill"></div></div>
            <span class="sheet-note" id="engine-progress-text">Starting…</span>
          </div>
        `),
      );
    } else {
      detail.append(
        element(`<p class="sheet-text">${escape(current.error || 'No TeX engine found.')}</p>`),
        element(`<p class="sheet-note">
            Lotus compiles with Tectonic, which it does not include in this build.
            It can fetch the version it was tested against — about 20&nbsp;MB, kept
            inside Lotus's own folder, and removed when you remove Lotus.
          </p>`),
      );
    }
    panel.append(body);

    const actions = element<HTMLDivElement>('<div class="sheet-actions"></div>');

    if (!current.available && !installing && current.installable) {
      const install = document.createElement('button');
      install.className = 'btn';
      install.textContent = `Install Tectonic ${current.installVersion}`;
      install.addEventListener('click', () => void run());
      actions.append(install);
    }

    if (!installing) {
      const recheck = document.createElement('button');
      recheck.className = 'btn btn-quiet';
      recheck.textContent = 'Check again';
      recheck.addEventListener('click', () => {
        void refreshEngine().then((next) => {
          current = next;
          draw();
        });
      });
      actions.append(recheck);
    }

    if (dismissable || current.available) {
      const done = document.createElement('button');
      done.className = current.available ? 'btn' : 'btn btn-quiet';
      done.textContent = 'Done';
      done.disabled = installing;
      done.addEventListener('click', close);
      actions.append(done);
    }

    panel.append(actions);
  };

  const run = async () => {
    installing = true;
    draw();

    // Progress arrives from the download itself; the bar is indeterminate
    // until the server says how large the file is.
    EventsOn('engine:progress', (p: { done: number; total: number }) => {
      const fill = document.getElementById('engine-bar-fill');
      const text = document.getElementById('engine-progress-text');
      if (!fill || !text) return;
      if (p.total > 0) {
        const pct = Math.min(100, Math.round((p.done / p.total) * 100));
        fill.style.width = `${pct}%`;
        text.textContent = `${megabytes(p.done)} of ${megabytes(p.total)} MB`;
      } else {
        text.textContent = `${megabytes(p.done)} MB`;
      }
    });

    try {
      current = await InstallEngine();
    } finally {
      EventsOff('engine:progress');
      installing = false;
    }
    announce(current);
    draw();
  };

  draw();
}

/** Why this engine and not another — the thing that explains a support case. */
function sourceExplanation(info: main.EngineInfo): string {
  switch (info.source) {
    case 'bundled':
      return 'Shipped with Lotus.';
    case 'managed':
      return 'Installed by Lotus.';
    case 'path':
      return 'Found on this machine, not installed by Lotus.';
    default:
      return '';
  }
}
