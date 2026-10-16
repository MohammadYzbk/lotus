# lotus

Lotus is an offline-first, cross-platform desktop LaTeX editor focused on a
polished writing, compilation, diagnostics, and PDF-preview workflow. It
orchestrates an external TeX engine rather than implementing one.

## Current status

One application: a [Wails v2](https://wails.io) desktop app with a Go backend
and a TypeScript/CodeMirror frontend, running on macOS, Windows, and Linux from
a single codebase.

- Opens a project folder, edits `.tex` sources, and compiles with Tectonic.
- Parses engine logs into structured diagnostics.
- Renders the PDF preview with `pdf.js`.
- Supports bidirectional SyncTeX navigation: the preview follows the cursor,
  `Cmd-J` jumps to the current source location, and clicking the preview jumps
  back to source. The parser lives in [`internal/synctex`](internal/synctex/)
  with tests against real engine output.
- Completes LaTeX commands, environments, and math snippets, plus `\ref` and
  `\cite` targets scanned from the whole project rather than the open buffer.
- Has a command palette (`Cmd-K`), a document outline, and light/dark theming
  that follows the system or an explicit choice, with the main colours
  adjustable per theme.
- Gives the whole window to the preview for reading.
- Connects a GitHub account, clones a repository into a managed working copy,
  and opens it as a project, showing the current branch and whether there is
  uncommitted work.
- Branches, commits, pushes, and opens a pull request without leaving the app.
- Fetches, reports how far a branch has drifted from its remote, and pulls by
  fast-forward or rebase, surfacing conflicts for resolution in the editor.
- Publishes a folder that was never under version control as a new repository.
- Ships with its own TeX engine, and offers to fetch one when a build has none,
  so a fresh install compiles without a terminal.

Against the phased build plan in
[`docs/project-plan.md`](docs/project-plan.md), Phases 0-8 are in place:
foundations, the vertical slice, the live-ish compile loop, projects as folders,
bidirectional SyncTeX, editor UX polish, connecting to GitHub, the branch and
pull-request workflow, and sync robustness. Publishing a plain folder as a new
repository is there too, which the plan never listed. Phase 9, packaging and
distribution, is largely built — a bundled engine, the first-run screen, an
update check, and release workflows for all three platforms — but unproven:
nothing has run in CI, nothing has been signed, and only macOS on Apple silicon
has been built end to end. The OAuth device flow and pull-request creation are
covered by tests against a stubbed server but have never run against real
GitHub. [`docs/packaging.md`](docs/packaging.md) and
[`docs/architecture.md`](docs/architecture.md) list those gaps and the rest.

Collaboration, plugins, AI, and telemetry remain out of scope.

## Installing

Download the build for your platform from the
[releases page](https://github.com/MohammadYzbk/lotus/releases), open it, and
start writing. The TeX engine is inside: nothing else to install, and no
terminal.

Releases are not signed yet, so the first launch needs a nudge — on macOS,
right-click the app and choose **Open**; on Windows, **More info → Run anyway**.
Both warnings are expected, and [`docs/packaging.md`](docs/packaging.md)
explains what would remove them.

Building from source instead? See [Development](#development). A build made
that way has no engine inside it, and offers to fetch one the first time it
opens.

## Shortcuts

`Cmd` throughout; `Ctrl` works in its place on Windows and Linux, since the
handler accepts either modifier.

### The palette

| Key | Does |
| --- | --- |
| `Cmd-K`, `Cmd-P` | Open a file |
| `Cmd-Shift-P` | Run a command |
| `Cmd-Shift-O` | Go to a heading |
| `Cmd-G` | Go to a line |

Once it is open, `↑`/`↓` or `Ctrl-P`/`Ctrl-N` move, `Enter` chooses, `Esc`
dismisses. Typing `>`, `@`, or `:` as the first character switches mode without
reopening, and the command list shows each command's own key beside it.

### Writing

| Key | Does |
| --- | --- |
| `Cmd-S` | Save and compile |
| `Cmd-Space` | Complete a command, environment, `\ref`, or `\cite` |
| `Enter` | Accept the highlighted completion |
| `Tab` | Next snippet field, or indent |
| `Cmd-Z`, `Cmd-Shift-Z` | Undo, redo |

### Preview

| Key | Does |
| --- | --- |
| `Cmd-J` | Show the cursor's line in the PDF |
| Click the page | Jump back to that spot in the source |
| `Cmd-+`, `Cmd-−` | Zoom in, out |
| `Cmd-0` | Fit to width |
| Pinch on a trackpad | Zoom |
| `Cmd-Shift-Return` | Give the whole window to the preview, and back |

### Window and dialogs

| Key | Does |
| --- | --- |
| `Ctrl-Cmd-F` | Full screen, on the macOS convention |
| `Cmd-,` | Colour settings |
| `Esc` | Dismiss the open sheet |
| `Enter` | Confirm a field: a branch name, a token, a repository search |
| `Cmd-Enter` | Commit, from the message box |
| Double-click a divider | Reset that pane split |

## Repository layout

```text
main.go, app.go           Wails entrypoint and the bound app API
app_engine.go             Finding a TeX engine, and installing one
app_update.go             The version, and whether a newer one exists
app_github.go             Sign-in, the account, the repository list
app_git.go                Branch, commit, push, pull request
app_sync.go               Fetch, divergence, pull, rebase, conflicts
app_publish.go            Turning a plain folder into a new repository
app_manifest_*.go         Per-platform paths, behind build tags
pdfserver.go              Serves compiled PDFs to the frontend
settings.go               Persisted user settings
internal/
  tex/                    TeX engine invocation
  texlog/                 Engine log parsing into diagnostics
  synctex/                SyncTeX parser for source/preview navigation
  project/                Project folder and file handling
  forge/                  GitHub sign-in, repositories, pull requests
  vcs/                    Working-copy state and Git operations, over go-git
  secrets/                The GitHub token, in the OS keychain
  engine/                 Locating, downloading and unpacking Tectonic
  update/                 Asking GitHub whether there is a newer Lotus
build/cmd/bundle-engine/  Puts the engine inside a built application
frontend/                 Vite + TypeScript UI (CodeMirror editor, pdf.js preview)
  src/latex/              LaTeX vocabulary, completion sources, outline parsing
  src/palette.ts          Command palette (commands, files, headings, go-to-line)
  src/theme.ts            Light/dark tokens shared by the chrome and the editor
  src/settings.ts         The adjustable colours, per theme
  src/preview.ts          pdf.js rendering, zoom, the SyncTeX highlight
  src/git.ts, github.ts   The Git and GitHub sheets
  src/engine.ts           The first-run engine screen
  src/about.ts            Version and the update check
build/                    Wails packaging inputs for darwin and windows
.github/workflows/        Checks on every push, and the release build
```

## Development

Requirements: Go, Node, [Wails v2](https://wails.io), and `tectonic` on `PATH`.

```sh
go install github.com/wailsapp/wails/v2/cmd/wails@v2.13.0
wails generate module   # regenerates frontend/wailsjs bindings
wails dev               # run with live reload
go test ./...           # Go tests
npm --prefix frontend test   # frontend unit tests (vitest)
wails build             # produce a platform binary

# Put the TeX engine inside that build, which the release workflow also does.
go run ./build/cmd/bundle-engine -app build/bin/lotus.app -platform darwin/arm64
```

[`docs/packaging.md`](docs/packaging.md) covers releases: what ships, how the
engine is pinned, and what signing needs.

JetBrains users can run the checked-in **Lotus (Wails)** configuration in
[`.run/`](.run/), which is equivalent to `wails dev`.

### GitHub sign-in

Browser sign-in uses the OAuth device flow, which needs a registered OAuth app.
Register one at **Settings → Developer settings → OAuth Apps**, enable device
flow, and pass its client ID at build time:

```sh
wails build -ldflags "-X main.githubClientID=Iv1.your-client-id"
```

`LOTUS_GITHUB_CLIENT_ID` works too, for trying it without a rebuild. A client ID
is public, so it is safe to commit in a build script; the device flow exists
precisely because a desktop app has nowhere to keep a secret.

Without one, browser sign-in is unavailable and the app asks for a personal
access token with the `repo` scope instead. That path also covers organisations
that block OAuth apps. Either way the token goes to the OS keychain, never to
disk.

`frontend/wailsjs` is generated and untracked. Keep `frontend/dist/.gitkeep`;
`main.go` embeds that directory, so a fresh checkout must contain it.

## Contributor entrypoints

- [`AGENTS.md`](AGENTS.md) for universal guardrails and task routing. Claude
  Code reaches the same entrypoint through [`CLAUDE.md`](CLAUDE.md).
- [`CONTRIBUTING.md`](CONTRIBUTING.md) for branch, commit, and pull-request
  format.
- [`docs/architecture.md`](docs/architecture.md) for how the application
  actually runs: startup, the compile loop, and how each subsystem hangs off it.
- [`docs/packaging.md`](docs/packaging.md) for how a release is built, what the
  engine pin means, and what is and is not signed.
- [`docs/project-plan.md`](docs/project-plan.md) for the vision, architecture,
  stack decisions, and the phased build plan.
- [`CONTEXT.md`](CONTEXT.md) for product vocabulary.
- [`docs/phase-0-spikes.md`](docs/phase-0-spikes.md) for the Phase 0 spike
  results that verified the stack choices.

## License

MIT — see [LICENSE](LICENSE).
