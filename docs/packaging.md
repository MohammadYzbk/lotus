# Packaging and distribution

How a Lotus release is built, what goes inside it, and what is signed.

The target this exists to hit: someone who has never opened a terminal
downloads Lotus, opens it, and compiles a document. Everything below is in
service of that, and the places where it is not yet true are named at the end.

## What ships

An installed Lotus is the application plus a TeX engine. Nothing else is
required of the machine — no TeX distribution, no package manager, no `PATH`
to arrange.

| Platform | Artifact | The engine lives at |
| --- | --- | --- |
| macOS | `lotus-macos-{arm64,amd64}.dmg` | `Lotus.app/Contents/Resources/engine/tectonic` |
| Windows | `lotus-windows-amd64-installer.exe` | `engine\tectonic.exe`, beside `lotus.exe` |
| Linux | `lotus-linux-amd64.tar.gz` | `engine/tectonic`, beside `lotus` |

Those paths are not arbitrary: they are the first places
[`internal/engine`](../internal/engine/engine.go) looks. The two have to agree,
which is why the build-time bundler and the runtime search import the same
package rather than each hard-coding a path.

## The engine

Tectonic, pinned in [`internal/engine/release.go`](../internal/engine/release.go)
to one version with one SHA-256 digest per platform.

Pinned rather than "latest" on purpose. A document that compiled yesterday
should compile today, and an engine that moves under the writer without anyone
choosing it is how that stops being true. Moving the pin means editing
`Version` and re-deriving every digest in the same commit.

The digests come from GitHub's own asset metadata. They are not an independent
attestation — the same host serves the file and the digest — but they catch the
failure that actually happens, which is a truncated or interrupted download,
and they pin the bytes so a file replaced behind the URL stops going unnoticed.

### Three places an engine can come from

Searched in this order, with the winner reported in the engine sheet:

1. **Bundled** — shipped in the installer. The version this release was built
   and tested against, so it wins.
2. **Managed** — downloaded by Lotus into its own data directory, which only
   happens because someone pressed the button on the first-run screen.
3. **PATH** — whatever the machine already had. Last, because it is the one
   version nobody here chose.

A build made with `wails build` alone has no bundled engine. That is not a
broken build: it falls through to the first-run screen, which offers to fetch
the pinned release. Bundling is what makes the *installer* zero-config.

## Building

```sh
# The application, with the version stamped in.
wails build -platform darwin/arm64 -ldflags "-X main.version=0.1.0"

# The engine, into the application that was just built.
go run ./build/cmd/bundle-engine -app build/bin/lotus.app -platform darwin/arm64
```

On Windows and Linux the bundler takes the directory holding the executable
rather than a bundle:

```sh
go run ./build/cmd/bundle-engine -app build/bin -platform linux/amd64
```

Pass `-cache <dir>` to keep the download between builds. The cached archive is
re-checked against its digest before use, so a corrupted cache is caught rather
than baked into a release.

These are two commands rather than a Wails `postBuildHooks` entry deliberately:
the hook would also fire on `wails dev`, and a twenty-megabyte download on the
way into a dev loop is not a trade anyone wants.

### Stamping the version

`main.version` is set at link time and defaults to `dev`. It is the only place
a version is written down at build time, so the About box and the update check
cannot disagree with the tag that produced the build. A `dev` build is never
told it is out of date — it is not on the release line at all.

## The release workflow

[`.github/workflows/release.yml`](../.github/workflows/release.yml) builds all
four artifacts on a tag push (`v*`), or on a manual run that builds without
publishing, so a release can be rehearsed.

Order matters in one place: **the engine is bundled before anything is signed.**
A signature seals the bundle's contents, and a file added afterwards invalidates
it. On macOS the nested engine is signed first and the app second, because
`codesign` seals inner code into the outer signature.

### Signing

Every signing step is skipped when its secrets are absent, so the workflow
produces working — if unsigned — artifacts in a fork or before certificates
exist, rather than failing.

| Secret | For |
| --- | --- |
| `MACOS_CERTIFICATE`, `MACOS_CERTIFICATE_PASSWORD` | A base64 Developer ID `.p12` and its password |
| `MACOS_SIGNING_IDENTITY` | e.g. `Developer ID Application: Name (TEAMID)` |
| `APPLE_ID`, `APPLE_TEAM_ID`, `APPLE_APP_PASSWORD` | Notarisation, using an app-specific password |
| `WINDOWS_CERTIFICATE`, `WINDOWS_CERTIFICATE_PASSWORD` | A base64 code-signing `.pfx` and its password |

An unsigned build works, but macOS Gatekeeper and Windows SmartScreen will both
warn about it, and on macOS the first launch needs right-click → Open. If
releases go out unsigned, say so in the release notes: a writer should know the
warning is expected rather than a sign that something is wrong.

## Updates

Lotus checks for a newer release only when asked — from the palette, via
**About Lotus**. Nothing runs on a timer and nothing contacts a server on
launch: an offline-first editor that phones home on start is not offline-first.

The check reports; it does not replace anything. Installing an update in place
means writing over a running, signed application, which is the business of a
framework with its own update signing (Sparkle, Squirrel), and getting it wrong
turns a working install into one the OS refuses to launch. Until there are
signed releases to feed such a framework, the honest half of the job is telling
the writer there is a new version and opening the page.

## Known gaps

- **Nothing here has been run in CI.** Both workflows were written and checked
  for syntax, but no run exists: that needs a push, and pushing is the owner's
  call. Expect the usual first-run friction from a new workflow.
- **No signing has been performed or verified**, because no certificates exist
  yet. The signing steps are written against Apple's and Microsoft's documented
  tools and are skipped without secrets, so the first real run of them will be
  their first test.
- **Only macOS/arm64 has been built and verified end to end** — built, engine
  bundled, engine located from inside the bundle and executed. The other three
  platforms are exercised only by the cross-platform code paths and their unit
  tests.
- **No crash or error reporting.** The project plan lists opt-in crash
  reporting under this phase, while the README has telemetry out of scope. That
  is a contradiction only the owner can settle, so nothing was built; see the
  note in [`docs/project-plan.md`](project-plan.md).
- **No auto-update**, as above: the check is implemented, the replacement is
  not.
- **No Linux arm64 artifact.** The engine supports it and `AssetFor` covers it;
  only the CI matrix omits it, for want of a runner.
