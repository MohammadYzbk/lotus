# How Lotus works

A walk through the application as it actually runs: what happens at launch,
what happens on every keystroke, and how each subsystem hangs off that spine.

This describes the code as it is, not as it is planned. For what is coming
next, see [the project plan](project-plan.md).

---

## 1. What Lotus is

An offline-first desktop LaTeX editor. It does not implement TeX — it
orchestrates an external engine (Tectonic) and wraps it in an editing
experience: instant preview, legible errors, bidirectional source ↔ PDF
navigation, and an opinionated slice of Git and GitHub.

One codebase runs on macOS, Windows and Linux. There is no second
platform-specific implementation; the handful of genuinely platform-dependent
functions sit behind Go build tags in the `_unix` / `_windows` / `_portable`
file pattern.

---

## 2. The shape

Four layers in **one process**:

```
┌─────────────────────────────────────────────────────────────┐
│  Webview (TypeScript)                                       │
│  CodeMirror editor · PDF.js preview · file tree · palette   │
└───────────────┬───────────────────────────┬─────────────────┘
                │ Wails bridge              │ HTTP (same process)
                │ (bound Go methods)        │ /pdf/… and /project/…
┌───────────────▼───────────────────────────▼─────────────────┐
│  App (package main) — app*.go                               │
│  the only mutable state, one mutex, all bound methods       │
└───────────────┬─────────────────────────────────────────────┘
                │
┌───────────────▼─────────────────────────────────────────────┐
│  Domain packages (internal/…)                               │
│  project · tex · texlog · synctex · vcs · forge · secrets   │
│  pure-ish, no Wails, individually testable                  │
└───────────────┬─────────────────────────────────────────────┘
                │ subprocess / network / filesystem
┌───────────────▼─────────────────────────────────────────────┐
│  Outside world: tectonic, git, the GitHub API, the keychain │
└─────────────────────────────────────────────────────────────┘
```

The important property: **`internal/…` knows nothing about Wails**. Every
package there is exercised by tests that construct real repositories, real
`.synctex.gz` files and real engine logs — no mocking of the app shell.
`package main` is the only place that holds mutable state.

---

## 3. Startup, traced

### 3.1 The Go side comes up first

`main()` ([`main.go`](../main.go)) constructs the `App`, embeds the built
frontend with `//go:embed all:frontend/dist`, and hands Wails a configuration:

- **`Middleware: app.assetMiddleware()`** — claims `/pdf/…` and `/project/…`
  before the asset chain sees them (§5).
- **`Mac: &mac.Options{}`** — an empty struct, but required: Wails only reads
  its `zoomable` flag when Mac options are present and otherwise defaults it to
  false, which disables the green button and fullscreen with it.
- **`Bind: []any{app}`** — every exported method on `*App` becomes callable
  from TypeScript. This is the entire API surface between the halves.
- **`OnStartup: app.startup`**.

### 3.2 `app.startup` restores where you were

1. Resolve the settings path: `~/Library/Application Support/lotus/settings.json`
   on macOS (`appDir()` in [`settings.go`](../settings.go)).
2. Load it. A missing or corrupt file means "no preferences yet", never an
   error — that is a perfectly good state to start in.
3. Try to reopen `LastProject`. If the directory is gone — moved, unmounted, on
   a disconnected drive — fall through.
4. Otherwise open the **scratch project** under the app directory, creating
   `main.tex` from a starter document if absent.

`openLocked` is where a project actually becomes current:

```
project.Open(dir)          → walk the tree, detect the root .tex
p.SetRootFile(remembered)  → a remembered choice beats re-detecting
buildDir(root)             → ~/Library/Caches/lotus/build/<hash of path>
a.pdfPath = ""             → the previous project's artifacts must not show
a.openFile = …             → remembered file, else the root document
recordOpenedLocked()       → note mtime+size for the stale-write guard (§4.2)
persistLocked()            → write settings back
```

**Build artifacts live outside the project** — keyed by a hash of the project
path, under the OS cache directory. A repository full of untracked `.pdf` and
`.synctex.gz` is noise the writer has to keep ignoring, and Lotus opens real
Git working copies.

### 3.3 The webview boots

Wails loads the embedded `index.html`, which loads the bundle. `init()` in
[`frontend/src/main.ts`](../frontend/src/main.ts) runs:

```
mountPanes(…)        restore dragged column widths from localStorage
wirePinchZoom(pdfEl) trackpad zoom over the preview
initTheme()          apply light/dark before first paint
renderSyncState()    the SyncTeX on/off switch
EngineVersion()      fire-and-forget; fills the status bar when it returns
await afterProjectChange(await CurrentProject())
```

`afterProjectChange` renders the tree, kicks off a symbol scan and a Git-state
read (both un-awaited — neither blocks the first paint), opens the file, and
calls `compileNow()`. **First pixels to first PDF is one round trip plus one
compile.**

---

## 4. The compile loop

The spine of the application. Everything else hangs off it.

```
keystroke
   │
   ├─ onChange → setStatus('dirty') and scheduleCompile()
   │                                    └─ 600 ms debounce (IDLE_COMPILE_MS)
   │
   └─ Cmd-S → compileNow() immediately
                 │
                 ▼
        SaveAndCompile(openFile, buffer)        ── Go, holding a.mu
                 │
                 ├─ writeOpenFileLocked  → two guards (§4.2)
                 ├─ compiler.Compile(…)  → tectonic subprocess
                 ├─ texlog.Parse(rawLog) → structured diagnostics
                 └─ returns CompileResult { pdfUrl, diagnostics, log, … }
                 │
                 ▼
        frontend: preview.load(pdfUrl) · editor.setProblems() · drawer
                  refreshSymbols() · renderGitState() · syncToCursor()
```

### 4.1 Why 600 ms

A warm compile takes 80–250 ms, so the debounce — not the engine — is what the
pause feels like. Too short and it fires mid-word, wasting runs and flashing
errors for half-typed commands; too long and it stops feeling live. 600 ms sits
just past a normal inter-word pause.

Overlapping compiles are collapsed rather than queued: if one is running, a
`pending` flag is set and exactly one more runs when it finishes.

### 4.2 Two guards before anything is written

`writeOpenFileLocked` refuses in two cases, both about not destroying work:

1. **Wrong file.** If the editor holds `a.tex` but asks to write `b.tex`, the
   save is refused. A desynchronised frontend must not be able to write one
   file's contents into another.
2. **Changed underneath.** The mtime and size recorded when the file was opened
   are compared against disk. If they differ, the write is refused with
   *"nothing was overwritten"* — an edit made outside Lotus is never silently
   clobbered by a stale buffer.

### 4.3 Running the engine

[`internal/tex`](../internal/tex) builds a fixed Tectonic 0.17 argument vector
(`Invocation.Arguments()`):

```
tectonic -X compile --untrusted --outdir <cache dir>
         --synctex --keep-logs --print <main.tex>
```

`--synctex` and `--keep-logs` are what make the artifacts survive the run;
without them the engine cleans up after itself. The working directory is the
main file's directory, so `\input` and `\includegraphics` resolve as the author
wrote them. A run is bounded by `DefaultTimeout` (3 minutes — generous, because
a cold first run downloads support files).

The result distinguishes two kinds of failure, which is the distinction the
whole UI is built around:

| | meaning |
|---|---|
| `Success: false`, `err == nil` | the engine ran; **the document** is broken. Normal while writing. |
| `err != nil` | no verdict at all — engine missing, timeout, cancelled. |

`PDFStale` is computed by noting the PDF's mtime *before* the run: a failed
compile that leaves an older PDF in place is reported as stale rather than as
this run's work.

### 4.4 Turning chatter into diagnostics

[`internal/texlog`](../internal/texlog) aims at the common cases and no
further: undefined control sequences, missing maths mode, missing packages,
unclosed braces, undefined references. Each `Diagnostic` carries severity,
file, line, cleaned message, an optional plain-English `Hint`, the offending
`Token`, and `Approximate` when the line is a best guess.

Anything unrecognised still reaches the writer as the raw log, always one click
away in the drawer. **The parser's job is to put a marker on the right line
when it can, never to be the only way to see what the engine said.**

Severity drives presentation: errors and warnings become inline editor markers;
`info` (overfull boxes) stays in the problems list, because a gutter that is
always lit is a gutter nobody reads.

Engine-reported paths are resolved against a **diagnostic file manifest** — a
bounded, canonicalising walk of the project — so a file reported in a different
case on a case-insensitive filesystem still maps to the right editor buffer.

---

## 5. Getting the PDF on screen

The PDF is **not** sent across the Wails bridge. It is served over HTTP from
the same process:

```go
/pdf/main.pdf?rev=N     → the most recent compiled PDF
/project/<rel>?rev=N    → a file from the open project (figures)
```

Three decisions worth knowing, all in [`pdfserver.go`](../pdfserver.go):

**Middleware, not the asset server's `Handler`.** `Handler` is a *fallback*,
consulted only for requests the asset chain declines — and under `wails dev`
the chain never declines anything: it proxies to Vite, whose SPA fallback
answers unknown paths with `index.html` and HTTP 200. The preview was being fed
HTML and PDF.js reported *"Invalid PDF structure"*. Middleware wraps the whole
chain, so dev and production behave identically.

**A revision counter, not a cache header alone.** `a.revision++` on every
successful compile; the query parameter defeats the webview cache, which
otherwise keeps showing a figure that has been replaced on disk.

**Whole files, not `http.ServeFile`.** `ServeFile` negotiates Range and
conditional requests, and the webview's custom-scheme transport does not handle
the resulting 206/304 responses — the preview's fetch simply never settled,
leaving the pane stuck on a placeholder with no error. These files are small
enough that one plain response is the better trade.

Path safety: `/project/` URLs go through `proj.Abs(rel)`, which refuses
anything resolving outside the project root.

### 5.1 Rendering

[`preview.ts`](../frontend/src/preview.ts) renders each page to its own canvas
inside a positioned wrapper. That structure is what makes both SyncTeX
directions possible: a click resolves to a page and a point in PDF coordinates,
and a highlight can be positioned over a page without redrawing it.

**One scale, used everywhere.** `scale` (CSS pixels per PDF point) drives
rendering, highlight placement and click hit-testing. The tempting shortcut —
`max-width` on the canvas — makes CSS shrink the bitmap so the rendered page no
longer matches the scale the SyncTeX maths uses, putting every highlight and
every click in the wrong place.

Zoom is a *mode*: `'fit'` tracks the pane width via a debounced
`ResizeObserver`, or an explicit scale between 0.25× and 6×. A recompile
deliberately does **not** reset it. Pages render at device pixel ratio and lay
out in CSS pixels, so text stays crisp on a Retina display.

The legacy PDF.js build is used deliberately: the modern one calls
`Map.prototype.getOrInsertComputed`, which the macOS WKWebView does not
implement.

---

## 6. SyncTeX, both directions

[`internal/synctex`](../internal/synctex) parses the `.synctex.gz` the engine
emits. Parsing is **lazy** — a long document's SyncTeX data runs to megabytes
and most compiles are never followed by a search, so `syncLocked()` parses on
first use and caches until the next compile invalidates it.

### Forward: cursor → PDF

`onCursorLine` → 180 ms debounce → `ForwardSearch(file, line)` → highlight.

The debounce is shorter than the compile's: long enough that arrow-keying
through a document does not fire a search per line, short enough that the
marker feels like it is following along.

`Forward` resolves a source line to **every row of output it produced** on each
page. Two problems pull against each other here:

- **Spillover.** When a file is `\input`, TeX first finishes the paragraph
  already in progress and attributes those broken-off lines to the *new* file's
  first line. Pointing at all of it sends the writer inches above the heading
  they asked for. So boxes are clustered by vertical gaps and only the last
  cluster is kept.
- **Extent.** Within that cluster the writer wants the whole area, not just
  where it starts. Every row is returned, each widened across the boxes beside
  it, so the marks read like a text selection.

A line with no output of its own (a comment, a blank line, a macro definition)
falls forward to the nearest line that does have material, preferring later
over earlier — the writer is usually about to type there.

`center` distinguishes *following along* from *being asked to go there*: while
typing the marker moves but the page only scrolls if the spot is off-screen;
`Cmd-J` always scrolls.

### Inverse: click → cursor

`Inverse(page, x, y)` decides by vertical distance first, then horizontal, then
the smaller box. Straight-line distance is the obvious choice and the wrong
one: a click in the gap between two lines is a few points from both, but an
enclosing box contains it outright — so plain distance answers with whichever
source line happens to own the page's vbox.

After jumping, the app re-runs the forward search for the resolved line, so
both directions show the same area.

The whole feature can be switched off from the palette; the toggle gates the UI
only, and the choice is remembered.

---

## 7. Editor intelligence

### Completion

The hard part is not the word list — it is knowing what the cursor is asking
for. [`latex/complete.ts`](../frontend/src/latex/complete.ts) decides from the
text before the cursor:

| context | offers |
|---|---|
| `\ref{`, `\eqref{`, `\cref{`, … | labels from the whole project |
| `\cite{`, `\citep{`, … | bib keys, with author and year |
| `\begin{` | environments, completing the whole block including `\end` |
| `\end{` | the enclosing environment first |
| `\input{`, `\includegraphics{` | project files |
| bare `\…` | commands, your own macros ranked above built-ins |

Cross-references come from the **backend**, not the buffer: a `\ref` in
`chapters/method.tex` routinely points at a `\label` in `main.tex`, and a
`\cite` points into a `.bib` nobody has open.
[`internal/project/symbols.go`](../internal/project/symbols.go) scans every
`.tex` and `.bib` in the project, tagging each label with the nearest heading
above it, and re-scans after each compile — a stale list that omits the label
you just wrote is worse than the scan.

Math commands are ranked *down* outside maths rather than hidden: the context
guess is a heuristic, and a wrong guess that hides `\alpha` is worse than one
that merely reorders it.

### Outline

Parsed from the **live buffer**, not from disk. An outline that only updates
when you save is an outline that disagrees with the document in front of you —
and the moment you notice it lying, you stop trusting it.

### Command palette

One overlay, four modes chosen by a prefix: `>` commands, `@` headings, `:`
line, bare = files. Fuzzy-scored with contiguity and word-boundary bonuses.

---

## 8. Git and GitHub

Three packages, split by what they talk to:

- **[`internal/forge`](../internal/forge)** — the GitHub API over plain
  `net/http`: OAuth device flow, the signed-in user, repository listing, pull
  requests. Hand-rolled rather than an SDK; the whole surface is a handful of
  requests.
- **[`internal/vcs`](../internal/vcs)** — Git itself, over go-git.
- **[`internal/secrets`](../internal/secrets)** — the token, in the OS keychain
  and nowhere else.

### The one place Lotus shells out

go-git covers cloning, status, branching, committing, fetching and
fast-forward pulls. It **cannot rebase**: its `Merge` accepts only
fast-forward and there is no `Rebase` or `CherryPick` at all. Divergence — two
machines that both moved — is exactly the case that needs one, so that single
operation goes to the `git` binary
([`internal/vcs/gitcli.go`](../internal/vcs/gitcli.go)).

The split has a useful consequence: **fetching stays in go-git, so the token
stays in this process** and is never passed to a subprocess or written where a
credential helper might cache it. Everything behind the CLI works on objects
that are already downloaded, so it needs no credentials and no network. The
subprocess environment is pinned (`GIT_TERMINAL_PROMPT=0`, `GIT_EDITOR=true`)
so nothing can hang forever behind a prompt nobody can see.

go-git also does not model unmerged index stages — during a conflict it reports
the file as plainly modified — so conflicted paths are read with
`git diff --name-only --diff-filter=U`.

### The workflow

```
clone      → ~/Library/Application Support/lotus/repos/<owner>/<name>
branch     → create (carries work in progress) · switch (refuses while dirty)
commit     → stages everything dirty; refuses without an identity
push       → publishes only the named branch, records it as tracking
fetch      → safe: changes nothing visible
pull       → fast-forward only; refuses on divergence rather than guessing
rebase     → replays local commits onto origin; conflicts edited in place,
             then continue or abort
pull req   → reuses one already open rather than failing
```

Conflicts are resolved **in the editor**: the files carry the standard
`<<<<<<<` markers, and "continue" checks that the markers are gone rather than
that the index is staged, because editing the file is the whole model.

---

## 9. Where state lives

| What | Where | Survives |
|---|---|---|
| Last project, root file, open file | `~/Library/Application Support/lotus/settings.json` | restart |
| Scratch project | `…/Application Support/lotus/scratch/` | restart |
| Cloned repositories | `…/Application Support/lotus/repos/<owner>/<name>/` | restart |
| GitHub token | OS keychain, service `lotus` | restart |
| Compile artifacts | `~/Library/Caches/lotus/build/<hash>/` | regenerable |
| Theme, pane widths, SyncTeX toggle, zoom | `localStorage` (`lotus.*`) | restart, per machine |
| Open document, diagnostics, SyncTeX map | memory only | no |

Every `localStorage` access is wrapped in `try`/`catch` — a private window or
blocked site data throws, and losing a preference must never stop the app
starting.

**The token is the one value with a rule of its own:** it is never in
`settings.json`, never in a project directory, never in a log line, never in
`ProjectInfo`, and it is stripped from error text before display.

---

## 10. Concurrency

One mutex, `App.mu`, guards all mutable app state: the open project, the output
directory, the PDF path and revision, the open-file identity and the SyncTeX
cache. It is needed because three things run concurrently:

- bound methods, each invoked on its own goroutine by Wails;
- the asset middleware, serving from the webview's own goroutine;
- overlapping compiles (the idle timer firing while one is still running).

The GitHub half carries **its own** mutex (`githubState.mu`) so a slow network
call never blocks the compile loop.

Methods named `…Locked` assume the caller already holds `a.mu`. A device-flow
sign-in deliberately outlives the call that started it, using a context
detached from the request with its own 20-minute deadline.

---

## 11. Safety rules the code is shaped by

These recur throughout and explain most of the refusals:

1. **Never overwrite work that changed underneath you.** Saves are checked
   against the file's identity at open time.
2. **Never switch branches or pull over uncommitted changes.** The app blocks
   and names the files in the way.
3. **Never push without an explicit action.** Opening a pull request does not
   push for you; it tells you to.
4. **Never guess on divergence.** A refused pull says what happened and what to
   do next.
5. **Never lose the raw log.** However good the parser gets, the engine's own
   words stay one click away.
6. **Treat the token as radioactive.** Keychain only, minimum scope, never
   serialised, redacted from errors.

---

## 12. How this is tested

Go tests build **real artifacts** rather than mocking:

- `internal/vcs` creates real repositories, a real bare remote, real
  divergence, real rebase conflicts — the whole two-machine story runs offline.
- `internal/synctex` runs against `.synctex.gz` files produced by a real engine.
- `internal/texlog` parses real engine output.
- `internal/forge` runs against an `httptest` server; **the device flow and PR
  creation have never touched real GitHub.**
- App-level tests inject a stub GitHub client, an in-memory keychain and a
  temporary clone directory, so no test touches the real keychain, the network,
  or the developer's support directory.

Frontend tests (vitest) cover the logic that is hard to verify by hand:
completion context detection, outline parsing, and the pinch-zoom direction
mapping — a pinch cannot be synthesised, so the sign is pinned by tests.

Standard checks: `go build ./...`, `go test ./...`, and `npm run build` in
`frontend/`.

---

## 13. Seams, and what is not done

Deliberate seams, each placed where a swap is foreseeable:

- **The Git layer** is small enough that a full CLI backend could replace
  go-git; one operation already does.
- **`secrets.Store`** is a two-method interface with a memory implementation.
- **The language mode** is the legacy `stex` stream parser. A Lezer grammar
  would buy structural selection and smarter indentation; completion is
  supplied separately and would survive the swap.
- **`forge.Client`** takes configurable base URLs, so GitHub Enterprise and
  tests both work.

Known gaps:

- **The OAuth device flow and pull-request creation are unverified against real
  GitHub.** Device flow additionally needs a client ID supplied at build time
  (`-X main.githubClientID=…`); without one the app falls back to a pasted
  token.
- **Every page re-renders on zoom and on compile.** Fine for a paper, wasteful
  for a hundred-page thesis; page virtualisation is the obvious fix.
- **No page-level conflict UI.** Conflicts are surfaced and edited as markers;
  a three-way merge view is out of scope.
- Phase 9 — packaging, signed installers, first-run experience and auto-update
  — has not started.
