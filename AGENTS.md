# Lotus agent guidance

The user's current request and any issue or specification they name define
the work scope and take precedence over repository and skill guidance.

## Execution

- Complete authorized implementation and affected verification before handoff.
  State routine assumptions and proceed. Ask when missing input would change
  scope or product behavior, and continue independent work while waiting.
- Treat follow-up messages as steering for the active task unless the user
  cancels or replaces it.
- If repository or skill guidance causes a pause, link the exact file and
  quote the instruction. Distinguish required approval from your interpretation,
  and account for authorization already given in the conversation.
- Write terse, plain English. Lead with the outcome and material trade-offs;
  use lists when they make the result easier to scan.

## Guardrails

- Preserve unrelated tracked and untracked work, personal settings, caches,
  credentials, and private paths.
- Keep secrets, user content, and machine-private values out of source, logs,
  and evidence.
- Do not commit, push, create or update a pull request, merge, or close an
  issue unless the user explicitly asks for that publication action. Loading
  a skill alone is not authorization.
- At handoff, name changed files, executed checks and results, unavailable or
  manual evidence, blockers, and accepted exceptions. Never call an unrun,
  unavailable, or owner-only check passed.

## MVP accessibility timing

- Owner decision, 2026-09-04: defer dedicated accessibility work and formal
  validation to final pre-launch hardening, unless explicitly requested earlier.
  These checks do not gate current UI review or implementation.
- Preserve platform-default controls, existing accessibility support, and
  ordinary keyboard behavior. Deferred checks remain open and required before
  MVP launch; never mark them passed or waived.

## Work routing

- Lotus is a single cross-platform Wails v2 application: a Go backend at the
  repository root with a TypeScript frontend in [`frontend/`](frontend/). Keep
  it building and running on macOS, Windows, and Linux from one codebase, and
  do not add a platform-specific second implementation.
- Put platform-specific Go behind build tags in the existing `_unix` /
  `_windows` / `_portable` file pattern rather than in a separate target.
- For a requested commit, push, or pull request, use the repository
  `commit-push` skill. [`CONTRIBUTING.md`](CONTRIBUTING.md) owns Git rules and
  [the pull-request template](.github/PULL_REQUEST_TEMPLATE.md) owns its body.
- For product language or normative document changes, read
  [`CONTEXT.md`](CONTEXT.md).
- Derive affected checks from repository files and CI. The standard checks are
  `go build ./...`, `go test ./...`, and `npm run build` in
  [`frontend/`](frontend/).
- Complete required checks; broaden or repeat them only for new changes,
  failures, or unresolved concerns. Add tests for meaningful behavior changes,
  rather than tests that merely restate configuration or instructions.
