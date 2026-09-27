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

Against the phased build plan in
[`docs/project-plan.md`](docs/project-plan.md), Phases 0-8 are in place:
foundations, the vertical slice, the live-ish compile loop, projects as folders,
bidirectional SyncTeX, editor UX polish, connecting to GitHub, the branch and
pull-request workflow, and sync robustness. Publishing a plain folder as a new
repository is there too, which the plan never listed. Phase 9, packaging and
distribution, is not started. The OAuth device flow and pull-request creation
are covered by tests against a stubbed server but have never run against real
GitHub; [`docs/architecture.md`](docs/architecture.md) lists that and the other
known gaps.

Collaboration, plugins, AI, and telemetry remain out of scope.

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
frontend/                 Vite + TypeScript UI (CodeMirror editor, pdf.js preview)
  src/latex/              LaTeX vocabulary, completion sources, outline parsing
  src/palette.ts          Command palette (commands, files, headings, go-to-line)
  src/theme.ts            Light/dark tokens shared by the chrome and the editor
  src/settings.ts         The adjustable colours, per theme
  src/preview.ts          pdf.js rendering, zoom, the SyncTeX highlight
  src/git.ts, github.ts   The Git and GitHub sheets
build/                    Wails packaging inputs for darwin and windows
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
```

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
- [`docs/project-plan.md`](docs/project-plan.md) for the vision, architecture,
  stack decisions, and the phased build plan.
- [`CONTEXT.md`](CONTEXT.md) for product vocabulary.
- [`docs/phase-0-spikes.md`](docs/phase-0-spikes.md) for the Phase 0 spike
  results that verified the stack choices.

## License

MIT — see [LICENSE](LICENSE).
