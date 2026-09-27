// Settings: the colours.
//
// Overrides are CSS custom properties set on the root element, which is the
// same mechanism the built-in palettes use — so a change reaches the chrome
// and the editor together, with no second definition to keep in step. The
// CodeMirror theme reads `var(--…)` for every colour it draws, and inherits
// these for free.
//
// Light and dark are stored separately. A colour chosen against a dark ground
// is rarely the one you want on a light one, and sharing a single value would
// make one of the two themes worse every time the other was adjusted.

import { isDark, onThemeChange } from './theme';

/** One adjustable colour. */
interface Token {
  /** The CSS custom property, without the leading dashes. */
  name: string;
  label: string;
  hint: string;
}

// A curated set rather than every token. These are the ones people actually
// want to change, and the omissions — borders, selection, the faint greys —
// are derived closely enough from these that exposing them invites a palette
// where text and its background are the same colour.
const TOKENS: Token[] = [
  { name: 'accent', label: 'Accent', hint: 'Selection, focus rings, the caret' },
  { name: 'bg', label: 'Background', hint: 'The editor and the window' },
  { name: 'bg-raised', label: 'Panels', hint: 'Toolbar, sidebar, dialogs' },
  { name: 'bg-sunken', label: 'Recessed', hint: 'Behind the PDF, hovered rows' },
  { name: 'fg', label: 'Text', hint: 'Body text and source' },
  { name: 'syn-command', label: 'Commands', hint: '\\section, \\textbf' },
  { name: 'syn-env', label: 'Environments', hint: 'itemize, equation' },
  { name: 'syn-string', label: 'Strings', hint: 'Quoted values' },
];

type Overrides = Record<string, string>;

const STORAGE_KEY = 'lotus.colors';

let overrides: { light: Overrides; dark: Overrides } = read();

function read(): { light: Overrides; dark: Overrides } {
  try {
    const raw = localStorage.getItem(STORAGE_KEY);
    if (raw) {
      const parsed = JSON.parse(raw) as { light?: Overrides; dark?: Overrides };
      return { light: parsed.light ?? {}, dark: parsed.dark ?? {} };
    }
  } catch {
    // Private windows and blocked site data both throw, and a corrupt value
    // should cost the customisation rather than the app.
  }
  return { light: {}, dark: {} };
}

function save() {
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(overrides));
  } catch {
    // See read(): not worth failing a colour change over.
  }
}

/** The override set for whichever theme is showing. */
function active(): Overrides {
  return isDark() ? overrides.dark : overrides.light;
}

/**
 * Writes the active overrides onto the root element.
 *
 * Every managed property is cleared first, so removing an override actually
 * restores the stylesheet's value instead of leaving the last one stuck.
 */
export function applyColors() {
  const root = document.documentElement;
  for (const token of TOKENS) {
    root.style.removeProperty(`--${token.name}`);
  }
  root.style.removeProperty('--accent-fg');

  const set = active();
  for (const [name, value] of Object.entries(set)) {
    root.style.setProperty(`--${name}`, value);
  }

  // Text drawn *on* the accent has to stay legible whatever accent is chosen —
  // a pale accent with white-on-accent labels is unreadable, and it is not
  // something anyone should have to think about while picking a colour.
  if (set.accent) {
    root.style.setProperty('--accent-fg', readableOn(set.accent));
  }
}

/** Black or white, whichever reads better on the given colour. */
function readableOn(color: string): string {
  const rgb = parseHex(color);
  if (!rgb) return '#ffffff';
  // Relative luminance, sRGB coefficients. The 0.55 threshold is a shade above
  // the usual midpoint because text on a coloured ground reads as lighter than
  // the same value in a flat swatch.
  const luminance = (0.2126 * rgb[0] + 0.7152 * rgb[1] + 0.0722 * rgb[2]) / 255;
  return luminance > 0.55 ? '#11131a' : '#ffffff';
}

function parseHex(value: string): [number, number, number] | null {
  const match = /^#?([0-9a-f]{6})$/i.exec(value.trim());
  if (!match) return null;
  const n = Number.parseInt(match[1], 16);
  return [(n >> 16) & 255, (n >> 8) & 255, n & 255];
}

/** The colour a picker should show: the override, else the stylesheet's own. */
function currentValue(name: string): string {
  const set = active();
  if (set[name]) return set[name];

  const computed = getComputedStyle(document.documentElement).getPropertyValue(`--${name}`).trim();
  return normaliseToHex(computed) ?? '#000000';
}

/**
 * `<input type="color">` accepts only #rrggbb, while the stylesheet may hold
 * any CSS colour. The browser is asked to do the conversion rather than
 * reimplementing colour parsing here.
 */
function normaliseToHex(value: string): string | null {
  if (!value) return null;
  if (/^#[0-9a-f]{6}$/i.test(value)) return value.toLowerCase();

  const probe = document.createElement('span');
  probe.style.color = value;
  document.body.append(probe);
  const resolved = getComputedStyle(probe).color;
  probe.remove();

  const match = /^rgba?\((\d+),\s*(\d+),\s*(\d+)/.exec(resolved);
  if (!match) return null;
  const hex = (n: string) => Number(n).toString(16).padStart(2, '0');
  return `#${hex(match[1])}${hex(match[2])}${hex(match[3])}`;
}

// Re-apply on a theme change: the two themes carry different overrides, and
// switching must not leave the previous theme's colours on the root.
onThemeChange(() => applyColors());

// --- the sheet ----------------------------------------------------------------

function element<T extends HTMLElement>(html: string): T {
  const template = document.createElement('template');
  template.innerHTML = html.trim();
  return template.content.firstElementChild as T;
}

/** Opens the settings sheet. */
export function openSettings() {
  const overlay = document.createElement('div');
  overlay.className = 'palette-overlay';
  overlay.innerHTML = `<div class="palette sheet" role="dialog" aria-modal="true" aria-label="Settings"></div>`;
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

  const draw = () => {
    panel.replaceChildren();
    const theme = isDark() ? 'dark' : 'light';

    const body = element<HTMLDivElement>(`
      <div class="sheet-body">
        <h2 class="sheet-title">Colours</h2>
        <p class="sheet-text">Editing the <strong>${theme}</strong> palette. Light and dark are
           kept separately, so switching themes keeps each one as you set it.</p>
        <div class="color-grid"></div>
      </div>
    `);
    const grid = body.querySelector<HTMLDivElement>('.color-grid')!;

    for (const token of TOKENS) {
      const row = element<HTMLDivElement>(`
        <div class="color-row">
          <input class="color-swatch" type="color" id="c-${token.name}" />
          <label class="color-label" for="c-${token.name}">
            <span class="color-name">${token.label}</span>
            <span class="color-hint">${token.hint}</span>
          </label>
          <button class="btn btn-quiet color-reset" title="Back to the default">Reset</button>
        </div>
      `);

      const input = row.querySelector<HTMLInputElement>('input')!;
      const reset = row.querySelector<HTMLButtonElement>('button')!;
      input.value = currentValue(token.name);

      const overridden = Boolean(active()[token.name]);
      reset.disabled = !overridden;
      row.classList.toggle('overridden', overridden);

      // `input` rather than `change`: the point of a colour picker is watching
      // the app change while you drag.
      input.addEventListener('input', () => {
        active()[token.name] = input.value;
        applyColors();
        save();
        reset.disabled = false;
        row.classList.add('overridden');
      });
      reset.addEventListener('click', () => {
        delete active()[token.name];
        applyColors();
        save();
        draw();
      });

      grid.append(row);
    }
    panel.append(body);

    const actions = element<HTMLDivElement>('<div class="sheet-actions"></div>');
    const resetAll = document.createElement('button');
    resetAll.className = 'btn btn-quiet';
    resetAll.textContent = `Reset ${theme}`;
    resetAll.disabled = Object.keys(active()).length === 0;
    resetAll.addEventListener('click', () => {
      if (isDark()) overrides.dark = {};
      else overrides.light = {};
      applyColors();
      save();
      draw();
    });

    const done = document.createElement('button');
    done.className = 'btn';
    done.textContent = 'Done';
    done.addEventListener('click', close);

    actions.append(resetAll, done);
    panel.append(actions);
  };

  draw();
}
