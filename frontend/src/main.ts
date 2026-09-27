// The app shell: file tree, editor, preview.
//
// The document that gets compiled is the project's root file, not whichever file
// is open — editing chapter3.tex has to compile the root, or the preview would
// show a fragment with no preamble.
//
// SyncTeX ties the two panes together in both directions: the marker follows the
// cursor into the PDF, and clicking the PDF moves the cursor.

import './style.css';
import { mountEditor, type EditorHandle } from './editor';
import { mountPreview } from './preview';
import { FileTree } from './tree';
import { mountPalette, type PaletteAction } from './palette';
import { mountPanes } from './panes';
import { gestureZoomFactor, wheelZoomFactor } from './zoom';
import { outline, type OutlineItem } from './latex/outline';
import { cycleTheme, initTheme, onThemeChange, themePreference } from './theme';
import { connectGitHub, openGitHubRepository } from './github';
import { openGitSheet, publishSheet } from './git';
import {
  Compile,
  ForwardSearch,
  InverseSearch,
  ConfirmDelete,
  CreateEntry,
  CurrentProject,
  DeleteEntry,
  EngineVersion,
  OpenFile,
  OpenProjectDialog,
  ProjectSymbols,
  RenameEntry,
  RepositoryState,
  SaveAndCompile,
  SetRootFile,
  ToggleFullscreen,
} from '../wailsjs/go/main/App';
import type { main, project, synctex, texlog, vcs } from '../wailsjs/go/models';

// How long to wait after the last keystroke before compiling.
//
// A warm compile takes 80–250 ms, so the delay — not the engine — is what the
// pause feels like. Too short and it fires mid-word, wasting runs and flashing
// errors for half-typed commands; too long and it stops feeling live. 600 ms
// sits just past a normal inter-word pause.
const IDLE_COMPILE_MS = 600;

document.querySelector('#app')!.innerHTML = `
  <header class="bar">
    <button class="btn btn-quiet" id="open-project" title="Open a different folder">
      <span id="project-name">…</span>
    </button>
    <span class="sep">/</span>
    <span class="path"><span class="dir" id="path-dir"></span><span class="file" id="path-file"></span></span>
    <button class="git" id="git" hidden title="Git"></button>
    <span class="spacer"></span>
    <span class="status" id="status">loading…</span>
    <button class="btn" id="compile" title="Compile now (Cmd-S)">Compile</button>
    <button class="btn btn-quiet" id="sync" title="Show this line in the PDF (Cmd-J)">Find in PDF</button>
    <button class="icon-btn" id="palette" title="Command palette (Cmd-K)">⌘K</button>
    <button class="icon-btn" id="theme" title="Theme">◐</button>
    <span class="engine" id="engine"></span>
  </header>
  <main class="panes">
    <aside class="sidebar">
      <div class="sidebar-head">
        <span>Files</span>
        <span class="spacer"></span>
        <button class="icon-btn" id="new-file" title="New file">＋</button>
        <button class="icon-btn" id="new-folder" title="New folder">＋▸</button>
      </div>
      <div class="tree" id="tree"></div>
      <div class="sidebar-head outline-head">
        <span>Outline</span>
        <span class="spacer"></span>
        <span class="sidebar-hint">⌘⇧O</span>
      </div>
      <div class="outline" id="outline"></div>
      <div class="root-note" id="root-note"></div>
    </aside>
    <div class="gutter" id="gutter-sidebar" role="separator" aria-orientation="vertical"
         title="Drag to resize · double-click to reset"></div>
    <section class="pane" id="editor-pane"></section>
    <div class="gutter" id="gutter-preview" role="separator" aria-orientation="vertical"
         title="Drag to resize · double-click to reset"></div>
    <section class="pane preview">
      <div class="pdf-scroll" id="pdf"><p class="empty">Compiling…</p></div>
      <div class="zoom">
        <button class="zoom-btn" id="zoom-out" title="Zoom out (Cmd−)">−</button>
        <button class="zoom-btn zoom-level" id="zoom-level" title="Fit to width (Cmd-0)">fit</button>
        <button class="zoom-btn" id="zoom-in" title="Zoom in (Cmd+)">+</button>
        <button class="zoom-btn zoom-expand" id="preview-focus" title="Give the window to the preview (⌘⇧↵)">⤢</button>
      </div>
    </section>
  </main>
  <footer class="drawer">
    <div class="drawer-head">
      <span id="drawer-title">No problems</span>
      <span class="spacer"></span>
      <button class="btn btn-quiet" id="toggle-log">Show raw log</button>
    </div>
    <div class="drawer-body" id="drawer-body" hidden>
      <ul class="problems" id="problems"></ul>
      <pre class="rawlog" id="log" hidden></pre>
    </div>
  </footer>
`;

const el = <T extends HTMLElement>(id: string) => document.getElementById(id) as T;
const statusEl = el('status');
const problemsEl = el('problems');
const logEl = el('log');
const pdfEl = el('pdf');
const editorPane = el('editor-pane');

// SyncTeX, both directions. A click on the page resolves to a source line; the
// cursor's line resolves to a place on the page.
const preview = mountPreview(pdfEl, {
  onPointClicked: (page, x, y) => void jumpToSource(page, x, y),
  onZoomChanged: (zoom, scale) => {
    // "fit" is a mode, not a number, and saying so is more useful than showing
    // whatever percentage the pane width happens to imply.
    const label = el('zoom-level');
    label.textContent = zoom === 'fit' ? 'fit' : `${Math.round(scale * 100)}%`;
    label.title = zoom === 'fit' ? 'Fitting the pane width — click for 100%' : 'Fit to width (Cmd-0)';
  },
});

let syncAvailable = false;
let syncTimer: number | undefined;

type Status = 'clean' | 'dirty' | 'compiling' | 'failed' | 'broken';

function setStatus(kind: Status, text: string) {
  statusEl.className = `status ${kind}`;
  statusEl.textContent = text;
}

// --- state -------------------------------------------------------------------

let editor: EditorHandle | null = null;
let openFile = '';
// Diagnostics from the last compile, across the whole project. The editor only
// ever shows the ones belonging to the file on screen.
let diagnostics: texlog.Diagnostic[] = [];

let compiling = false;
let pending = false;
let idleTimer: number | undefined;

const tree = new FileTree(el('tree'), {
  onOpen: (path) => void openPath(path),
  onCreate: (parent, name, isDir) => void applyProject(CreateEntry(parent, name, isDir), { reopen: !isDir }),
  onRename: (path, name) => void applyProject(RenameEntry(path, name), { reopen: true }),
  onDelete: (path, isDir) => void confirmAndDelete(path, isDir),
  onSetRoot: (path) => void applyProject(SetRootFile(path), { recompile: true }),
});

// --- the project index -------------------------------------------------------

// What \ref, \cite and \input complete against, and what the palette lists.
// Both are refreshed from the backend rather than derived from the open buffer,
// because they describe the project, not the file on screen.
let symbols: project.Symbols | null = null;
let fileList: string[] = [];

const completionSources = {
  symbols: () => symbols,
  files: () => fileList,
};

async function refreshSymbols() {
  try {
    symbols = await ProjectSymbols();
  } catch (err) {
    // Completion degrades to the built-in vocabulary, which is still useful.
    console.error(err);
    symbols = null;
  }
}

/** Flattens the tree into the project-relative paths of every file. */
function collectFiles(node: main.ProjectInfo['tree']): string[] {
  const out: string[] = [];
  const walk = (n: typeof node) => {
    if (!n) return;
    if (n.isDir) {
      for (const child of n.children ?? []) walk(child);
      return;
    }
    if (n.path) out.push(n.path);
  };
  walk(node);
  return out.sort();
}

// --- git state ---------------------------------------------------------------

// Read on demand rather than folded into ProjectInfo: it walks the working
// tree, and the file tree re-renders far more often than a branch changes.
async function renderGitState() {
  const badge = el('git');
  let state: vcs.State;
  try {
    state = await RepositoryState();
  } catch (err) {
    console.error(err);
    badge.hidden = true;
    return;
  }

  // A plain folder is a perfectly normal project, so the badge disappears
  // rather than announcing the absence of Git.
  if (!state.repository) {
    badge.hidden = true;
    return;
  }

  const name = state.branch || (state.unborn ? 'no commits yet' : state.detached ? 'detached' : '');
  badge.hidden = false;
  badge.className = `git${state.dirty ? ' dirty' : ''}`;
  badge.textContent = state.dirty ? `${name} •` : name;
  badge.title = [
    state.remote ? `Remote: ${state.remote}` : 'No remote',
    state.dirty ? 'Uncommitted changes' : 'No uncommitted changes',
    state.error,
  ]
    .filter(Boolean)
    .join('\n');
}

// --- the outline -------------------------------------------------------------

let outlineItems: OutlineItem[] = [];

/** Rebuilds the outline from the buffer, not from disk, so it tracks typing. */
function renderOutline() {
  const host = el('outline');
  outlineItems = editor ? outline(editor.getDoc()) : [];

  if (outlineItems.length === 0) {
    host.replaceChildren();
    const empty = document.createElement('p');
    empty.className = 'outline-empty';
    empty.textContent = editor ? 'No headings in this file.' : '';
    host.append(empty);
    return;
  }

  const list = document.createElement('ul');
  list.className = 'outline-list';

  // Headings are indented by their own level rather than by nesting depth, so a
  // document that jumps from \section to \subsubsection still reads correctly.
  const shallowest = Math.min(...outlineItems.map((i) => i.level));

  for (const item of outlineItems) {
    const row = document.createElement('li');
    const button = document.createElement('button');
    button.className = `outline-item level-${item.level}`;
    button.style.paddingLeft = `${8 + (item.level - shallowest) * 12}px`;
    button.textContent = item.title;
    button.title = `${item.title} — line ${item.line}`;
    button.addEventListener('click', () => editor?.goToLine(item.line));
    row.append(button);
    list.append(row);
  }

  host.replaceChildren(list);
}

// --- project and files -------------------------------------------------------

function showPath(path: string) {
  const cut = path.lastIndexOf('/');
  el('path-dir').textContent = cut < 0 ? '' : path.slice(0, cut + 1);
  el('path-file').textContent = cut < 0 ? path : path.slice(cut + 1);
}

function renderProject(info: main.ProjectInfo) {
  el('project-name').textContent = info.name || 'No project';
  tree.render({ tree: info.tree, rootFile: info.rootFile, openFile });
  fileList = collectFiles(info.tree);

  // Say plainly which document is compiled, since it is usually not this one.
  const note = el('root-note');
  if (!info.rootFile) {
    note.textContent = 'No \\documentclass found — nothing to compile yet.';
    note.className = 'root-note warn';
  } else if (info.rootFile !== openFile) {
    note.textContent = `Compiling ${info.rootFile}`;
    note.className = 'root-note';
  } else {
    note.textContent = '';
    note.className = 'root-note';
  }

  if (info.error) setStatus('broken', info.error);
}

/** Runs a project-mutating call, then refreshes the tree. */
async function applyProject(
  call: Promise<main.ProjectInfo>,
  opts: { reopen?: boolean; recompile?: boolean } = {},
) {
  const info = await call;
  if (info.error) {
    setStatus('broken', info.error);
  }
  // The backend decides what is open now: a create selects the new file, a
  // delete falls back to the root document.
  if (opts.reopen && info.openFile && info.openFile !== openFile) {
    await openPath(info.openFile, info);
    return;
  }
  renderProject(info);
  if (opts.recompile) await compileNow();
}

async function confirmAndDelete(path: string, isDir: boolean) {
  if (!(await ConfirmDelete(path, isDir))) return;
  await applyProject(DeleteEntry(path), { reopen: true, recompile: true });
}

/** Opens a file in the editor, or shows a figure in place of it. */
async function openPath(path: string, info?: main.ProjectInfo) {
  const file = await OpenFile(path);
  if (file.error) {
    setStatus('broken', file.error);
    return;
  }

  openFile = path;
  showPath(path);

  if (file.kind === 'image') {
    showImage(file.url, path);
  } else if (file.kind === 'binary') {
    showMessage(`${path} is not a text file, so there is nothing to edit.`);
  } else {
    showEditor(file.content);
  }

  renderProject(info ?? (await CurrentProject()));
  applyDiagnosticsToEditor();
}

function showEditor(content: string) {
  if (editor) {
    editor.setDoc(content);
    editor.focus();
    renderOutline();
    return;
  }
  editorPane.replaceChildren();
  editor = mountEditor(editorPane, {
    doc: content,
    sources: completionSources,
    onSave: () => void compileNow(),
    onChange: () => {
      if (!compiling) setStatus('dirty', 'editing…');
      scheduleCompile();
      scheduleOutline();
    },
    onCursorLine: (line) => scheduleSync(line),
  });
  editor.focus();
  renderOutline();
}

// Rebuilding the outline parses the whole buffer, so it waits for a pause
// rather than running on every keystroke.
let outlineTimer: number | undefined;

function scheduleOutline() {
  window.clearTimeout(outlineTimer);
  outlineTimer = window.setTimeout(renderOutline, 250);
}

/** Replaces the editor with a non-editable view; the editor is rebuilt on the
 * next text file, which also discards its history for the old buffer. */
function replaceEditorWith(node: HTMLElement) {
  editor = null;
  editorPane.replaceChildren(node);
  renderOutline();
}

function showImage(url: string, path: string) {
  const wrap = document.createElement('div');
  wrap.className = 'figure-view';
  const img = document.createElement('img');
  img.src = url;
  img.alt = path;
  const caption = document.createElement('p');
  caption.className = 'empty';
  caption.textContent = path;
  wrap.append(img, caption);
  replaceEditorWith(wrap);
}

function showMessage(text: string) {
  const p = document.createElement('p');
  p.className = 'empty';
  p.textContent = text;
  replaceEditorWith(p);
}

// --- the compile loop --------------------------------------------------------

function scheduleCompile() {
  window.clearTimeout(idleTimer);
  idleTimer = window.setTimeout(() => void compileNow(), IDLE_COMPILE_MS);
}

async function compileNow(): Promise<void> {
  window.clearTimeout(idleTimer);
  if (compiling) {
    pending = true;
    return;
  }
  compiling = true;
  setStatus('compiling', 'compiling…');

  try {
    // Only a text buffer has anything to save; a figure on screen must not
    // overwrite the file it came from.
    const res: main.CompileResult = editor
      ? await SaveAndCompile(openFile, editor.getDoc())
      : await Compile();

    // The buffer is on disk now, so a \label written a moment ago is only
    // completable from here on. Deliberately not awaited: completion catching
    // up a few milliseconds late is invisible, a slower compile is not.
    void refreshSymbols();
    // Saving is also what turns a clean working copy dirty.
    void renderGitState();

    if (res.error) {
      setStatus('broken', res.error);
      diagnostics = res.diagnostics ?? [];
      renderDrawer(diagnostics, res.log);
      applyDiagnosticsToEditor();
      pinnedView = 'log';
      showView('log', true);
      return;
    }

    diagnostics = res.diagnostics ?? [];
    applyDiagnosticsToEditor();
    renderDrawer(diagnostics, res.log);

    const counts = tally(diagnostics);
    if (res.compiled) {
      setStatus('clean', `compiled ${res.rootFile} in ${res.durationMs} ms${counts ? ` · ${counts}` : ''}`);
    } else {
      const stale = res.pdfStale ? ' · preview is from the last good compile' : '';
      const explained = diagnostics.some((d) => d.severity === 'error');
      if (explained) {
        setStatus('failed', `${counts} · ${res.durationMs} ms${stale}`);
      } else {
        // The parser covers the common cases, and this is not one of them — a
        // broken image, a bad font, something from a tool downstream of TeX.
        // Reporting "1 warning" here would imply the document was fine, so say
        // it failed and open the log, which is the only thing that can explain
        // why.
        setStatus('failed', `compile failed for a reason we could not parse · ${res.durationMs} ms${stale}`);
        pinnedView = 'log';
        showView('log', true);
      }
    }

    syncAvailable = res.hasSynctex;
    if (res.pdfUrl) {
      try {
        await preview.load(res.pdfUrl);
        // Positions from the previous compile are gone with the old render, so
        // put the marker back where the cursor now is.
        if (editor) syncToCursor(editor.cursorLine(), false);
      } catch (err) {
        setStatus('broken', `preview failed: ${err}`);
        console.error(err);
      }
    } else {
      // Never compiled successfully yet, so there is nothing to show. Saying so
      // beats leaving "Compiling…" on screen as though it were still working.
      preview.showMessage(
        res.compiled
          ? 'The engine produced no pages.'
          : 'No preview yet — fix the errors below and it will appear.',
      );
    }
  } catch (err) {
    setStatus('broken', String(err));
    console.error(err);
  } finally {
    compiling = false;
    if (pending) {
      pending = false;
      void compileNow();
    }
  }
}

/** Markers belong to one file; showing another file's errors on these lines
 * would point at innocent code. */
function applyDiagnosticsToEditor() {
  editor?.setProblems(diagnostics.filter((d) => d.file === openFile));
}

// --- preview focus -----------------------------------------------------------

// Giving the whole window to the PDF, for reading rather than writing.
//
// Deliberately not persisted: it is a mode you enter to read a few pages, and
// reopening the app into a hidden editor would be a puzzle rather than a
// convenience.
let previewFocused = false;

function setPreviewFocus(on: boolean) {
  previewFocused = on;
  document.querySelector('.panes')!.classList.toggle('preview-focus', on);

  const button = el('preview-focus');
  button.textContent = on ? '⤡' : '⤢';
  button.title = on ? 'Restore the editor (⌘⇧↵)' : 'Give the window to the preview (⌘⇧↵)';

  if (!on && editor) {
    // CodeMirror measures lazily and was display:none while focused, so its
    // scroller has stale geometry until it is asked to look again.
    editor.view.requestMeasure();
    editor.focus();
  }
}

// --- pinch to zoom -----------------------------------------------------------

/**
 * WebKit's pinch events, which is what a trackpad produces inside WKWebView.
 *
 * They are not in lib.dom, because they have never been standardised — every
 * other engine reports a pinch as a wheel event with ctrlKey set instead.
 */
interface WebKitGestureEvent extends UIEvent {
  scale: number;
}

function wirePinchZoom(target: HTMLElement) {
  // Both families are wired rather than feature-detected. Detection looked
  // tidier, but `ongesturechange` existing does not guarantee it fires, and the
  // failure mode there is a pinch that silently does nothing. Instead the
  // native gesture takes precedence when it actually arrives, and ctrl+wheel —
  // which is how every other engine, and a Windows precision touchpad, reports
  // a pinch — covers the rest.
  let lastGesture = 0;
  // `scale` is cumulative across a gesture, so each step is the ratio against
  // the previous reading.
  let previous = 1;

  // Long enough to span the wheel events WebKit emits alongside a gesture,
  // short enough that a deliberate ctrl+scroll straight after one still works.
  const GESTURE_HOLD_MS = 400;

  target.addEventListener('gesturestart', (event) => {
    event.preventDefault();
    previous = 1;
    lastGesture = Date.now();
  });
  target.addEventListener('gesturechange', (event) => {
    // Without this the webview zooms the whole page instead of the document.
    event.preventDefault();
    lastGesture = Date.now();
    const scale = (event as WebKitGestureEvent).scale;
    if (!scale) return;
    preview.zoomBy(gestureZoomFactor(scale, previous));
    previous = scale;
  });
  target.addEventListener('gestureend', (event) => {
    event.preventDefault();
    lastGesture = Date.now();
  });

  target.addEventListener(
    'wheel',
    (event) => {
      // An ordinary scroll must still scroll.
      if (!event.ctrlKey) return;
      event.preventDefault();
      // The native gesture is already driving this pinch; WebKit sends both.
      if (Date.now() - lastGesture < GESTURE_HOLD_MS) return;
      preview.zoomBy(wheelZoomFactor(event.deltaY));
    },
    { passive: false },
  );
}

// --- SyncTeX: source <-> PDF -------------------------------------------------

// TEMPORARY: a switch for turning SyncTeX off.
//
// Everything this controls is in this block and the two `syncEnabled` guards
// below, so removing it later means deleting them and the palette entry that
// calls setSyncEnabled. It gates the UI only — the backend still parses the
// .synctex.gz, which costs nothing while nothing asks it for a search.
//
// The choice is remembered, because the reason to turn the feature off is
// usually to work without it for a while, not for a single session.
const SYNC_STORAGE_KEY = 'lotus.synctex.enabled';

let syncEnabled = readSyncEnabled();

function readSyncEnabled(): boolean {
  try {
    return localStorage.getItem(SYNC_STORAGE_KEY) !== 'off';
  } catch {
    // Private windows and blocked site data both throw. Defaulting to on keeps
    // the feature available rather than silently disabling it.
    return true;
  }
}

function setSyncEnabled(next: boolean) {
  syncEnabled = next;
  try {
    localStorage.setItem(SYNC_STORAGE_KEY, next ? 'on' : 'off');
  } catch {
    // See readSyncEnabled: losing the preference is better than not applying it.
  }

  renderSyncState();
  if (!next) {
    // Leaving the last marker on screen would imply the preview is still
    // following the cursor.
    window.clearTimeout(syncTimer);
    preview.clearHighlight();
    return;
  }
  if (editor) syncToCursor(editor.cursorLine(), false);
}

/** Keeps the Find in PDF button honest about whether it will do anything. */
function renderSyncState() {
  const button = el<HTMLButtonElement>('sync');
  button.disabled = !syncEnabled;
  button.title = syncEnabled
    ? 'Show this line in the PDF (Cmd-J)'
    : 'SyncTeX is off — turn it back on from the command palette (Cmd-K)';
}
// END TEMPORARY

// Long enough that moving through a document with the arrow keys does not fire a
// search per line, short enough that it feels like the marker is following along.
const SYNC_IDLE_MS = 180;

function scheduleSync(line: number) {
  window.clearTimeout(syncTimer);
  syncTimer = window.setTimeout(() => syncToCursor(line, false), SYNC_IDLE_MS);
}

/**
 * Forward search: mark where the cursor's line ended up in the PDF.
 *
 * `center` distinguishes following along from being asked to go there. While
 * typing, the marker moves but the page only scrolls if the spot is off-screen —
 * a preview that re-centres itself on every cursor movement is exhausting to
 * write against. Cmd-J means "take me there", so it always scrolls.
 */
function syncToCursor(line: number, center: boolean) {
  if (!syncEnabled || !syncAvailable || !editor || !openFile) return;

  void (async () => {
    let rects: synctex.Rect[];
    try {
      rects = await ForwardSearch(openFile, line);
    } catch (err) {
      console.error(err);
      return;
    }
    if (!rects || rects.length === 0) {
      // Perfectly normal: comments, blank preamble lines and macro definitions
      // never appear on a page. Clear the marker rather than leaving a stale one
      // implying this line is somewhere it is not.
      preview.clearHighlight();
      if (center) setStatus('clean', `line ${line} produces no output in the PDF`);
      return;
    }
    preview.highlight(rects);
    preview.reveal(rects[0], center);
  })();
}

/** Inverse search: a click on the page takes the cursor to the line behind it. */
async function jumpToSource(page: number, x: number, y: number) {
  if (!syncEnabled || !syncAvailable) return;

  let loc: main.SourceLocation;
  try {
    loc = await InverseSearch(page, x, y);
  } catch (err) {
    console.error(err);
    return;
  }
  if (loc.error) {
    setStatus('broken', loc.error);
    return;
  }

  // Show what was matched, so a surprising jump is explainable rather than
  // just wrong-feeling.
  preview.highlight([loc.rect]);

  if (loc.file !== openFile) {
    await openPath(loc.file);
  }
  editor?.goToLine(loc.line);
}

// --- the problems drawer -----------------------------------------------------

type View = 'problems' | 'log';

let pinnedView: View | null = null;
let currentView: View = 'log';
let bodyOpen = false;

function renderDrawer(diags: texlog.Diagnostic[], rawLog: string) {
  problemsEl.replaceChildren();

  for (const d of diags) {
    const item = document.createElement('li');
    item.className = `problem ${d.severity}`;

    const where = document.createElement('button');
    where.className = 'where';
    // The file matters now that a project spans many: an error in chapter 3 is
    // not the same as one in the root.
    const place = d.file && d.file !== openFile ? shortName(d.file) : '';
    where.textContent = d.line
      ? `${place ? `${place}:` : 'line '}${d.line}${d.approximate ? '?' : ''}`
      : place || '—';
    if (d.line) {
      where.title = d.approximate
        ? 'The engine gave no location; this is our best guess'
        : `Jump to ${d.file || openFile} line ${d.line}`;
      where.addEventListener('click', () => void jumpTo(d));
    } else {
      where.disabled = true;
      where.title = 'The engine reported no location for this problem';
    }

    const text = document.createElement('span');
    text.className = 'problem-text';
    text.append(d.message);
    if (d.hint) {
      const hint = document.createElement('span');
      hint.className = 'hint';
      hint.textContent = d.approximate ? `Probably: ${d.hint}` : d.hint;
      text.append(hint);
    }

    item.append(where, text);
    problemsEl.append(item);
  }

  logEl.textContent = rawLog.trim();
  el('drawer-title').textContent = tally(diags) || 'No problems';

  const view = pinnedView ?? (diags.length > 0 ? 'problems' : 'log');
  showView(view, diags.length > 0 || pinnedView === 'log');
}

/** Jumping to a problem in another file has to open that file first. */
async function jumpTo(d: texlog.Diagnostic) {
  if (d.file && d.file !== openFile) {
    await openPath(d.file);
  }
  editor?.goToLine(d.line);
}

function shortName(path: string): string {
  const cut = path.lastIndexOf('/');
  return cut < 0 ? path : path.slice(cut + 1);
}

function tally(diags: texlog.Diagnostic[]): string {
  const n = (s: string) => diags.filter((d) => d.severity === s).length;
  const plural = (count: number, word: string) => `${count} ${word}${count === 1 ? '' : 's'}`;
  const parts: string[] = [];
  if (n('error')) parts.push(plural(n('error'), 'error'));
  if (n('warning')) parts.push(plural(n('warning'), 'warning'));
  if (n('info')) parts.push(plural(n('info'), 'note'));
  return parts.join(' · ');
}

function showView(view: View, open: boolean) {
  currentView = view;
  bodyOpen = open;
  el('drawer-body').hidden = !open;
  logEl.hidden = view !== 'log';
  problemsEl.hidden = view !== 'problems';

  // The button says what clicking it will do. While the drawer is shut that is
  // "open this view"; once open, it is "switch to the other one".
  const target: View = open ? (view === 'log' ? 'problems' : 'log') : view;
  el('toggle-log').textContent = target === 'log' ? 'Show raw log' : 'Show problems';
}

el('toggle-log').addEventListener('click', () => {
  pinnedView = bodyOpen ? (currentView === 'log' ? 'problems' : 'log') : currentView;
  showView(pinnedView, true);
});

// --- theme -------------------------------------------------------------------

const THEME_GLYPH = { system: '◐', light: '☀', dark: '☾' } as const;

function renderThemeButton() {
  const preference = themePreference();
  const button = el('theme');
  button.textContent = THEME_GLYPH[preference];
  button.title = `Theme: ${preference} — click to change`;
}

// The editor carries its own copy of the theme, so it has to be told.
onThemeChange((dark) => {
  editor?.setDark(dark);
  renderThemeButton();
});

// --- the command palette -----------------------------------------------------

function openProjectFolder() {
  void (async () => {
    const info = await OpenProjectDialog();
    openFile = '';
    await afterProjectChange(info);
  })();
}

function paletteActions(): PaletteAction[] {
  return [
    { id: 'compile', title: 'Compile now', hint: '⌘S', run: () => void compileNow() },
    {
      id: 'sync',
      title: 'Find this line in the PDF',
      hint: '⌘J',
      run: () => editor && syncToCursor(editor.cursorLine(), true),
    },
    { id: 'open-project', title: 'Open project folder…', run: openProjectFolder },
    { id: 'new-file', title: 'New file', run: () => tree.beginCreate(false) },
    { id: 'new-folder', title: 'New folder', run: () => tree.beginCreate(true) },
    { id: 'problems', title: 'Show problems', run: () => { pinnedView = 'problems'; showView('problems', true); } },
    { id: 'log', title: 'Show raw log', run: () => { pinnedView = 'log'; showView('log', true); } },
    { id: 'theme', title: 'Switch theme (system, light, dark)', run: () => cycleTheme() },
    { id: 'fullscreen', title: 'Toggle full screen', hint: '⌃⌘F', run: () => void ToggleFullscreen() },
    {
      id: 'synctex',
      title: syncEnabled ? 'Turn SyncTeX off' : 'Turn SyncTeX on',
      hint: syncEnabled ? 'on' : 'off',
      run: () => setSyncEnabled(!syncEnabled),
    },
    {
      id: 'preview-focus',
      title: previewFocused ? 'Restore the editor' : 'Give the window to the preview',
      hint: '⌘⇧↵',
      run: () => setPreviewFocus(!previewFocused),
    },
    { id: 'zoom-in', title: 'Zoom in', hint: '⌘+', run: () => preview.zoomIn() },
    { id: 'zoom-out', title: 'Zoom out', hint: '⌘−', run: () => preview.zoomOut() },
    { id: 'zoom-fit', title: 'Fit the preview to the pane', hint: '⌘0', run: () => preview.fitWidth() },
    { id: 'zoom-actual', title: 'Preview at actual size', run: () => preview.actualSize() },
    { id: 'git', title: 'Branch, commit, push…', run: () => openGit() },
    { id: 'github-publish', title: 'Publish this project to GitHub…', run: () => void publishSheet(() => void renderGitState()) },
    { id: 'github-open', title: 'Open a GitHub repository…', run: () => void openRepository() },
    { id: 'github-connect', title: 'Connect a GitHub account…', run: () => void connectAccount() },
    { id: 'goto-file', title: 'Go to file…', hint: '⌘P', run: () => palette.open('') },
    { id: 'goto-heading', title: 'Go to heading…', hint: '⌘⇧O', run: () => palette.open('@') },
    { id: 'goto-line', title: 'Go to line…', hint: '⌘G', run: () => palette.open(':') },
  ];
}

const palette = mountPalette({
  actions: paletteActions,
  files: () => fileList,
  sections: () => outlineItems,
  openFile: (path) => void openPath(path),
  goToLine: (line) => editor?.goToLine(line),
});

// --- GitHub ------------------------------------------------------------------

function connectAccount() {
  void connectGitHub(() => void renderGitState());
}

function openGit() {
  void openGitSheet(() => void renderGitState());
}

function openRepository() {
  void openGitHubRepository((info) => {
    openFile = '';
    void afterProjectChange(info);
  });
}

// --- wiring ------------------------------------------------------------------

el('compile').addEventListener('click', () => void compileNow());
el('sync').addEventListener('click', () => {
  if (editor) syncToCursor(editor.cursorLine(), true);
});
el('new-file').addEventListener('click', () => tree.beginCreate(false));
el('new-folder').addEventListener('click', () => tree.beginCreate(true));
el('open-project').addEventListener('click', openProjectFolder);
el('palette').addEventListener('click', () => palette.open('>'));
el('zoom-in').addEventListener('click', () => preview.zoomIn());
el('zoom-out').addEventListener('click', () => preview.zoomOut());
// The readout doubles as the control: clicking it swaps between fitting the
// pane and 100%, which is the pair of zoom levels anyone actually wants.
el('zoom-level').addEventListener('click', () =>
  preview.zoom() === 'fit' ? preview.actualSize() : preview.fitWidth(),
);
el('preview-focus').addEventListener('click', () => setPreviewFocus(!previewFocused));
// The branch badge is the obvious place to look for anything Git-related.
el('git').addEventListener('click', () => openGit());
el('theme').addEventListener('click', () => cycleTheme());

// Shortcuts that must work wherever focus is — the tree, the preview, or the
// editor — so they live on the window rather than in the editor's keymap.
window.addEventListener('keydown', (event) => {
  if (!(event.metaKey || event.ctrlKey)) return;
  const key = event.key.toLowerCase();

  // Zoom first: the shifted forms ('+' and '_') would otherwise be swallowed by
  // the palette's shift branch below.
  if (key === '+' || key === '=') {
    event.preventDefault();
    preview.zoomIn();
    return;
  }
  if (key === '-' || key === '_') {
    event.preventDefault();
    preview.zoomOut();
    return;
  }
  if (key === '0') {
    event.preventDefault();
    preview.fitWidth();
    return;
  }

  // Cmd-Shift-P / Cmd-Shift-O address the palette's other modes.
  if (event.shiftKey) {
    if (key === 'enter') {
      event.preventDefault();
      setPreviewFocus(!previewFocused);
      return;
    }
    if (key === 'p') {
      event.preventDefault();
      palette.open('>');
    }
    if (key === 'o') {
      event.preventDefault();
      palette.open('@');
    }
    return;
  }

  // Ctrl-Cmd-F is the macOS convention; it reaches here on every platform.
  if (event.ctrlKey && event.metaKey && key === 'f') {
    event.preventDefault();
    void ToggleFullscreen();
    return;
  }

  switch (key) {
    case 's':
      event.preventDefault();
      void compileNow();
      break;
    // Cmd-J: take me to this line in the PDF.
    case 'j':
      event.preventDefault();
      if (editor) syncToCursor(editor.cursorLine(), true);
      break;
    case 'k':
    case 'p':
      event.preventDefault();
      palette.open('');
      break;
    case 'g':
      event.preventDefault();
      palette.open(':');
      break;
  }
});

async function afterProjectChange(info: main.ProjectInfo) {
  renderProject(info);
  // A different project means different labels, citations and macros.
  void refreshSymbols();
  void renderGitState();
  const toOpen = info.openFile || info.rootFile;
  if (toOpen) {
    await openPath(toOpen, info);
  } else {
    showMessage('This folder has no document yet. Use ＋ to create one.');
  }
  await compileNow();
}

async function init() {
  mountPanes(document.querySelector<HTMLElement>('.panes')!);
  wirePinchZoom(pdfEl);
  initTheme();
  renderThemeButton();
  setPreviewFocus(false);
  renderSyncState();

  EngineVersion()
    .then((v) => (el('engine').textContent = v))
    .catch((err) => console.error(err));

  await afterProjectChange(await CurrentProject());
}

void init();
