// Light and dark, for the chrome and the editor together.
//
// The editor's colours are written as `var(--…)` rather than literals, so both
// themes are defined once in style.css and CodeMirror simply inherits them.
// That removes the failure mode where the chrome switches to light and the
// editor stays dark because someone updated one palette and not the other.

import { Compartment, type Extension } from '@codemirror/state';
import { EditorView } from '@codemirror/view';
import { HighlightStyle, syntaxHighlighting } from '@codemirror/language';
import { tags as t } from '@lezer/highlight';

export type ThemePreference = 'system' | 'light' | 'dark';

const STORAGE_KEY = 'lotus.theme';

/** Only the `dark` flag actually changes; the colours are CSS variables. */
export const themeCompartment = new Compartment();

let preference: ThemePreference = read();
const listeners = new Set<(dark: boolean) => void>();

const media = window.matchMedia('(prefers-color-scheme: dark)');
media.addEventListener('change', () => {
  if (preference === 'system') apply();
});

function read(): ThemePreference {
  try {
    const saved = localStorage.getItem(STORAGE_KEY);
    if (saved === 'light' || saved === 'dark' || saved === 'system') return saved;
  } catch {
    // Private windows and blocked site data both throw here. A theme that
    // cannot be remembered is a much smaller problem than an app that will not
    // start, so fall through to the default.
  }
  return 'system';
}

export function themePreference(): ThemePreference {
  return preference;
}

export function isDark(): boolean {
  return preference === 'system' ? media.matches : preference === 'dark';
}

export function setTheme(next: ThemePreference) {
  preference = next;
  try {
    localStorage.setItem(STORAGE_KEY, next);
  } catch {
    // See read(): losing the preference across restarts is acceptable.
  }
  apply();
}

/** Cycles system → light → dark → system, which is what a single button wants. */
export function cycleTheme(): ThemePreference {
  const order: ThemePreference[] = ['system', 'light', 'dark'];
  const next = order[(order.indexOf(preference) + 1) % order.length];
  setTheme(next);
  return next;
}

export function onThemeChange(fn: (dark: boolean) => void) {
  listeners.add(fn);
}

function apply() {
  const dark = isDark();
  // An explicit choice stamps the root; "system" leaves it to the media query,
  // so the OS switching at sunset is picked up without a restart.
  if (preference === 'system') document.documentElement.removeAttribute('data-theme');
  else document.documentElement.setAttribute('data-theme', preference);

  for (const fn of listeners) fn(dark);
}

export function initTheme() {
  apply();
}

// --- the editor's half -------------------------------------------------------

const highlight = HighlightStyle.define([
  { tag: t.comment, color: 'var(--syn-comment)', fontStyle: 'italic' },
  // In stex the control sequences land on these; covering the family rather
  // than one tag keeps the theme working if the language mode is ever swapped
  // for a real Lezer grammar.
  { tag: [t.keyword, t.controlKeyword, t.moduleKeyword], color: 'var(--syn-command)' },
  { tag: [t.tagName, t.typeName, t.className], color: 'var(--syn-env)' },
  { tag: [t.atom, t.bool, t.special(t.variableName)], color: 'var(--syn-atom)' },
  { tag: [t.number, t.literal], color: 'var(--syn-number)' },
  { tag: [t.string, t.regexp], color: 'var(--syn-string)' },
  { tag: [t.variableName, t.propertyName], color: 'var(--syn-name)' },
  { tag: [t.bracket, t.brace, t.punctuation, t.separator], color: 'var(--syn-bracket)' },
  { tag: [t.operator, t.derefOperator], color: 'var(--syn-operator)' },
  { tag: [t.meta, t.processingInstruction], color: 'var(--syn-meta)' },
  { tag: t.escape, color: 'var(--syn-atom)' },
  { tag: t.link, color: 'var(--accent)', textDecoration: 'underline' },
  { tag: t.heading, color: 'var(--syn-command)', fontWeight: '600' },
  { tag: t.strong, fontWeight: '600' },
  { tag: t.emphasis, fontStyle: 'italic' },
  { tag: t.invalid, color: 'var(--bad)' },
]);

const base = EditorView.theme({
  '&': {
    height: '100%',
    fontSize: 'var(--editor-size)',
    backgroundColor: 'var(--bg)',
    color: 'var(--fg)',
  },
  '.cm-scroller': {
    fontFamily: 'var(--mono)',
    lineHeight: 'var(--editor-leading)',
  },
  '.cm-content': { caretColor: 'var(--accent)' },
  '&.cm-focused .cm-cursor': { borderLeftColor: 'var(--accent)', borderLeftWidth: '2px' },
  '.cm-gutters': {
    backgroundColor: 'var(--bg)',
    color: 'var(--fg-faint)',
    border: 'none',
    paddingRight: '4px',
  },
  '.cm-activeLineGutter': { backgroundColor: 'var(--bg-raised)', color: 'var(--fg-dim)' },
  '.cm-activeLine': { backgroundColor: 'var(--active-line)' },
  '&.cm-focused .cm-selectionBackground, .cm-selectionBackground, ::selection': {
    backgroundColor: 'var(--selection)',
  },
  '.cm-matchingBracket, &.cm-focused .cm-matchingBracket': {
    backgroundColor: 'var(--bracket-match)',
    outline: '1px solid var(--border-strong)',
  },
  '.cm-foldPlaceholder': {
    backgroundColor: 'var(--bg-sunken)',
    border: '1px solid var(--border)',
    color: 'var(--fg-dim)',
    borderRadius: '4px',
    padding: '0 6px',
  },
  // Completion. The default popup is a browser list; this one has to sit in the
  // same visual language as the rest of the app.
  '.cm-tooltip': {
    backgroundColor: 'var(--bg-raised)',
    border: '1px solid var(--border)',
    borderRadius: '8px',
    boxShadow: 'var(--shadow)',
    overflow: 'hidden',
  },
  '.cm-tooltip.cm-tooltip-autocomplete > ul': {
    fontFamily: 'var(--mono)',
    fontSize: '12.5px',
    maxHeight: '18em',
  },
  '.cm-tooltip.cm-tooltip-autocomplete > ul > li': {
    padding: '4px 10px',
    display: 'flex',
    alignItems: 'baseline',
    gap: '8px',
  },
  '.cm-tooltip.cm-tooltip-autocomplete > ul > li[aria-selected]': {
    backgroundColor: 'var(--accent)',
    color: 'var(--accent-fg)',
  },
  '.cm-completionLabel': { flex: '0 0 auto' },
  '.cm-completionDetail': {
    marginLeft: 'auto',
    fontStyle: 'normal',
    fontSize: '11px',
    color: 'var(--fg-dim)',
    fontFamily: 'var(--sans)',
    whiteSpace: 'nowrap',
    overflow: 'hidden',
    textOverflow: 'ellipsis',
    maxWidth: '16em',
  },
  'li[aria-selected] .cm-completionDetail': { color: 'var(--accent-fg)', opacity: '0.85' },
  '.cm-completionMatchedText': { textDecoration: 'none', color: 'inherit', fontWeight: '700' },
  '.cm-completionInfo': {
    backgroundColor: 'var(--bg-raised)',
    border: '1px solid var(--border)',
    borderRadius: '8px',
    padding: '8px 10px',
    fontFamily: 'var(--sans)',
    fontSize: '12px',
    maxWidth: '22em',
  },
  '.cm-tooltip.cm-tooltip-lint': { fontFamily: 'var(--sans)', fontSize: '12px', padding: '2px' },
  '.cm-diagnostic': { borderLeftWidth: '3px', padding: '4px 8px' },
  '.cm-diagnostic-error': { borderLeftColor: 'var(--bad)' },
  '.cm-diagnostic-warning': { borderLeftColor: 'var(--dirty)' },
  '.cm-snippetFieldPosition': { borderLeft: '1.4px solid var(--accent)' },
  '.cm-snippetField': { backgroundColor: 'var(--selection)' },
  '.cm-searchMatch': { backgroundColor: 'var(--bracket-match)' },
});

/** The extension set the editor installs; reconfigured when the theme flips. */
export function editorTheme(dark: boolean): Extension {
  // CodeMirror uses this flag for the handful of decisions it makes itself,
  // such as which built-in selection blending to use.
  return [base, EditorView.theme({}, { dark }), syntaxHighlighting(highlight)];
}
