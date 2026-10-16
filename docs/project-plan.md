# Project Plan — Modern Offline LaTeX Editor (working title: TBD)

A single-author, offline-first desktop LaTeX editor whose entire reason to exist is
that writing LaTeX *feels good*: instant preview, legible errors, and painless
version control against the user's own GitHub repo. It does not reimplement TeX —
it orchestrates an existing engine and wraps it in a modern editing experience.

---

## 1. Vision & scope

**What it is (v1):**
- A desktop app for writing LaTeX solo, offline.
- Zero-config first run — no separate multi-gigabyte TeX install required.
- A fast edit → compile → preview loop that feels close to live.
- Bidirectional source ↔ PDF navigation (SyncTeX).
- Human-readable compile errors shown inline on the offending lines.
- Optional sync of a project to a user-chosen GitHub repo, with branch-based edits
  and pull-request creation.

**Explicit non-goals for v1 (defer, don't build):**
- Real-time collaboration (CRDT/OT, presence, multi-cursor).
- A custom TeX engine of any kind.
- A full CTAN package-manager UI.
- Mobile / web versions.
- Rich merge-conflict resolution UI (v1 detects and surfaces conflicts; full 3-way
  merge editing is a later milestone).

The differentiator is **taste and polish**, not a unique algorithm. That means the
bar is a hundred small frictions removed, which only reveals itself through
dogfooding — so plan to write real documents in the tool as early as Phase 2.

---

## 2. Architecture at a glance

Four cooperating layers:

1. **Editor (frontend)** — code editor component with LaTeX awareness, plus the PDF
   preview pane and all UI. Built in web tech inside the desktop shell.
2. **Orchestration (Go backend)** — spawns the TeX engine, watches files, parses
   logs, handles SyncTeX lookups, and manages projects on disk.
3. **Engine** — a bundled TeX engine invoked as a subprocess. It is never modified.
4. **Version control** — Git operations against a local working copy plus GitHub API
   calls for repo discovery, branches, and pull requests.

Data flows: editor buffer → Go writes to disk → Go runs engine → Go reads back PDF +
`.synctex.gz` + log → frontend renders PDF and error markers.

---

## 3. Tech stack decisions

| Concern | Choice | Rationale / tradeoff |
|---|---|---|
| Desktop shell | **Wails** (Go + web frontend) | Keeps the backend in Go, where the project's momentum already is. Alternatives: Tauri (Rust — bigger community, more polish, but new language to learn) and Electron (heaviest, most batteries-included). Confirm the current major version and its docs before starting — Wails moves fast. |
| TeX engine | **Tectonic** (bundled), with optional fallback to a system `latexmk`/TeX Live | Self-contained; fetches only the packages a document needs, on demand, and caches them. This is what enables the zero-config first run. Power users can point at a full local TeX install. |
| Editor component | **CodeMirror 6** | Syntax highlighting, bracket matching, and an extensible completion system out of the box; you supply LaTeX-specific completions. |
| PDF rendering | **PDF.js** | Mature in-browser PDF renderer; integrates cleanly with the web frontend and supports the coordinate access SyncTeX needs. |
| Git operations | **go-git** (pure Go), with a documented fallback to shelling out to the system `git` | go-git needs no external dependency, matching the zero-config ethos. Tradeoff: it lags the `git` CLI on some edge cases and large-repo performance. Shelling out to `git` is more battle-tested but adds a dependency. Start with go-git; keep the interface abstract so swapping is cheap. |
| GitHub API | **go-github** | For repo listing, branch creation, and pull requests. (Git *transport* — clone/commit/push/pull — goes through go-git; repo *discovery and PRs* go through the API.) |
| Secret storage | OS keychain via a Go keyring library | Never store the OAuth token in plaintext. Use the platform credential store (Keychain / Credential Manager / Secret Service). |

Confirm current versions and APIs of each library at implementation time rather than
trusting this table — several of these evolve quickly.

---

## 4. GitHub sync — detailed design

This is the most involved subsystem after the compile loop, so it gets its own
design section. The mental model: **a project is a local working copy of a Git repo;
the app is a friendly front-end over a small, opinionated slice of Git + GitHub.**

### 4.1 Authentication
- Use the **GitHub OAuth device flow**, which is designed for desktop/CLI apps: the
  app shows a short code, the user authorizes in their browser, and the app polls for
  the token. No hosted redirect server required (only a registered OAuth app client
  ID).
- Request the **minimum scope** needed (repo access). Least privilege.
- Store the resulting token in the **OS keychain**, never on disk in plaintext.
- Provide a clear "Disconnect GitHub" action that deletes the stored token.

### 4.2 Choosing a repo
- After auth, list the user's repositories via the API so they can pick one, and also
  accept a manual clone URL for repos not surfaced (org repos, etc.).
- On selection, **clone** into a managed working directory. The `.tex` files, `.bib`,
  images, and any class files live in this working copy — this *is* the project.

### 4.3 The branch-based edit workflow
This is the core of the feature request. Keep the surface small and legible:
- **Show the current branch** prominently in the UI at all times.
- **Create a branch** from the current HEAD with one action (validated name).
- **Switch branches** (with a guard: warn/stash or block if there are uncommitted
  changes, so work is never silently lost).
- **Commit** — manual, with a message. (Autosave writes to disk continuously;
  commits are deliberate, so history isn't polluted with keystroke noise.)
- **Push** the current branch to the remote.
- **Open a pull request** from the current branch via the API, prefilled with a
  title/body, opening the resulting PR URL in the browser.

### 4.4 Getting remote changes (even solo)
Solo does not mean single-machine — a user editing from a laptop and a desktop will
diverge. So:
- **Fetch/pull** to bring in remote commits.
- **Detect divergence** (local and remote both moved) and surface it clearly rather
  than failing cryptically.
- v1 conflict handling: **detect and surface** conflicts, offer safe options (pull
  with rebase, or open the conflicted file with standard conflict markers for manual
  fix-up in the editor). A polished 3-way merge UI is deferred (see Phase 8).

### 4.5 Safety rules (bake these in from the start)
- Never auto-push without an explicit user action.
- Never switch branches or pull over uncommitted changes without an explicit choice.
- Treat the token as radioactive: keychain only, minimum scope, easy revoke.
- Always show the user what branch they're on and whether there are unsynced changes.

---

## 5. Phased build plan

Each phase ends in something usable, so there's always a working artifact. Rough
sequencing only — adjust as dogfooding reveals priorities.

### Phase 0 — Foundations
- Initialize the repo, license, and a Wails skeleton that opens a window.
- Set up CI (build on the target OSes, run tests/linters).
- Spike each risky dependency in isolation: run Tectonic from Go on a hello-world
  `.tex`; render a PDF with PDF.js; mount a CodeMirror 6 instance.
- **Done when:** the empty shell builds and launches on your primary OS, and each
  dependency has a proven minimal example.

### Phase 1 — Vertical slice (the spine)
- Two panes: CodeMirror editor on the left, PDF.js preview on the right.
- A Go function that: writes the buffer to a temp `.tex`, runs Tectonic, returns the
  PDF path (and captures the log + `.synctex.gz`).
- Compile on **save**; load the resulting PDF into the preview.
- **Done when:** you can type a document, hit save, and see the compiled PDF. This is
  the whole app in miniature — probably a weekend.

### Phase 2 — The "feels live" loop
- **Debounced auto-compile** on edit (not every keystroke — settle on an idle delay).
- Smooth PDF refresh (preserve scroll position; avoid flashing).
- **Compile-error parsing**: turn Tectonic/TeX log output into structured errors and
  render them as inline markers on the offending source lines. Target the common 80%
  first: undefined control sequence, missing `$`, missing package, unbalanced braces.
- **Done when:** editing feels near-live and errors appear where the mistake is, in
  plain language. Start dogfooding — write something real in it now.

### Phase 3 — Projects as folders
- Introduce the **project = a directory** concept (real LaTeX is multi-file).
- File tree UI; open/create/rename/delete files; handle images and `.bib`.
- Identify the **main/root file** to compile (the one with `\documentclass`), since
  the entry point isn't always the file being edited.
- **Done when:** you can open a multi-file project, edit any file, and compile the
  root correctly.

### Phase 4 — SyncTeX (bidirectional)
- Parse the `.synctex.gz` the engine emits.
- **Forward:** cursor in source → scroll/highlight the matching spot in the PDF.
- **Inverse:** click in the PDF → jump to the matching source line.
- **Done when:** both directions work reliably on a multi-page document. This is the
  feature that most separates "professional" from "clunky."

### Phase 5 — Editor UX polish
- LaTeX completions: environments, `\commands`, `\ref`/`\cite` targets pulled from the
  project, math snippets.
- Command palette; document outline/structure navigation.
- Good default typography and theming (light/dark); font choices.
- **Done when:** the editor feels modern to *you* in daily use — this phase is judged
  by taste, so trust the dogfooding.

### Phase 6 — GitHub: connect & open
- Implement OAuth device flow; store token in the keychain.
- Repo listing + manual clone URL; clone into the managed working dir.
- Open a cloned repo as a project (reuses Phase 3 machinery).
- Show current branch and dirty/clean state in the UI.
- **Done when:** a user can connect GitHub, pick a repo, and edit it as a project.

### Phase 7 — GitHub: branch, commit, push, PR
- Branch: create / switch (with uncommitted-changes guard).
- Commit (manual, with message) and push the current branch.
- Create a pull request from the branch via the API; open it in the browser.
- **Done when:** the full request is satisfied — edit on a branch, commit, push, open
  a PR, without leaving the app.

### Phase 8 — Sync robustness
- Fetch/pull; detect divergence; safe pull-with-rebase option.
- Surface conflicts with standard markers for in-editor resolution.
- Handle the multi-machine round trip cleanly (edit on machine A, continue on B).
- **Done when:** a realistic two-machine solo workflow doesn't lose or corrupt work.

### Phase 9 — Packaging & distribution
- Bundle Tectonic; produce signed installers for the target OSes.
- First-run experience (the zero-config moment — verify it truly "just works").
- Auto-update mechanism; crash/error reporting (opt-in).
- **Done when:** a non-technical user can download, install, and compile a document
  without touching a terminal.

> **Open question, raised while building this phase.** Crash and error reporting
> is listed here as opt-in, while the README has kept telemetry out of scope
> since Phase 0. The two cannot both hold. Nothing was built either way: an
> opt-in reporter is a small piece of work, but it is the first thing in Lotus
> that would send anything anywhere, and that is an owner's decision rather than
> an implementation detail. A local error log with a "copy this into an issue"
> button would satisfy most of the intent without a server, if that is the line.

---

## 6. State & data model (sketch)

- **Project**: a directory on disk (optionally a Git working copy). Tracks the root
  `.tex`, open files, and last-compile artifacts.
- **Compile job**: input files → engine invocation → { PDF path, log, synctex map }.
- **Git state** (when applicable): current branch, ahead/behind counts, dirty flag,
  remote URL.
- **Settings**: engine choice (bundled vs system), auto-compile delay, theme, keymap.
- **Secrets**: GitHub token — keychain only, referenced by handle, never serialized
  into project or settings files.

---

## 7. Key risks & mitigations

- **SyncTeX fiddliness** — the format and coordinate mapping are finicky. Mitigate by
  isolating it behind a small Go module with its own tests on known-good documents.
- **go-git edge cases** — may stumble on some real-world repos. Mitigate by keeping
  the Git layer behind an interface so a `git`-CLI backend can be swapped in.
- **Log-parsing coverage** — TeX errors are endlessly varied. Accept 80% coverage in
  v1 and always fall back to showing the raw log for the unmatched cases.
- **Scope creep toward collaboration** — the moment "just a little sharing" appears,
  the CRDT tar pit opens. Hold the line: v1 is solo.
- **Dependency version drift** — Wails, go-git, go-github, Tectonic, CodeMirror all
  move. Re-verify APIs at implementation time rather than trusting any snapshot.

---

## 8. A realistic MVP cut

If you want something usable fast and are willing to trim: **Phases 0–4** already
constitute a genuinely nice offline LaTeX editor (zero-config compile, live-ish
preview, readable errors, SyncTeX). Ship or dogfood that, then treat **Phases 6–7**
(the GitHub workflow you asked for) as the headline of a second milestone, with
Phase 5 polish woven throughout. Phase 8 robustness and Phase 9 distribution convert
it from "my tool" into "other people's tool."
