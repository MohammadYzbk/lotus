package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/MohammadYzbk/lotus/internal/engine"
	"github.com/MohammadYzbk/lotus/internal/tex"
)

// engineApp is an app whose engine search is confined to directories this test
// made, so nothing depends on what the developer's machine has installed.
func engineApp(t *testing.T) (*App, string) {
	t.Helper()
	managed := t.TempDir()
	// An empty PATH, or the developer's own Tectonic answers every question
	// here and the tests stop meaning anything.
	t.Setenv("PATH", t.TempDir())
	return &App{
		compiler:     tex.NewCompiler(),
		settingsFile: filepath.Join(t.TempDir(), settingsFileName),
		engineDir:    managed,
	}, managed
}

// fakeEngine writes a script that answers --version the way Tectonic does.
func fakeEngine(t *testing.T, dir, reply string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in engine is a shell script")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, engine.ExecutableName())
	script := "#!/bin/sh\necho '" + reply + "'\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// The case this whole feature exists for: a fresh machine with no TeX.
func TestEngineStatusWithNoEngine(t *testing.T) {
	app, _ := engineApp(t)

	info := app.EngineStatus()
	if info.Available {
		t.Fatalf("an engine was reported on an empty machine: %+v", info)
	}
	if info.Error == "" {
		t.Error("no explanation was offered")
	}
	// And the writer is told something can be done about it.
	if !info.Installable {
		t.Errorf("not installable on %s/%s, which the tests run on", runtime.GOOS, runtime.GOARCH)
	}
	if info.InstallVersion != engine.Version {
		t.Errorf("install version: got %q, want %q", info.InstallVersion, engine.Version)
	}
}

func TestEngineStatusFindsTheManagedEngine(t *testing.T) {
	app, managed := engineApp(t)
	want := fakeEngine(t, managed, "Tectonic 0.17.0")

	info := app.EngineStatus()
	if !info.Available {
		t.Fatalf("the installed engine was not found: %+v", info)
	}
	if info.Path != want {
		t.Errorf("path: got %q, want %q", info.Path, want)
	}
	if info.Source != string(engine.SourceManaged) {
		t.Errorf("source: got %q, want %q", info.Source, engine.SourceManaged)
	}
	if info.Version != "Tectonic 0.17.0" {
		t.Errorf("version: got %q", info.Version)
	}
}

// Finding a file is not the same as having an engine. A truncated download is
// on disk and executable, and only running it proves anything.
func TestEngineStatusRejectsAnEngineThatCannotRun(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a non-executable file is not a meaningful case on Windows")
	}
	app, managed := engineApp(t)
	// Executable, but not a program: exactly what half a download looks like.
	path := filepath.Join(managed, engine.ExecutableName())
	if err := os.WriteFile(path, []byte("\x7fELF truncated"), 0o755); err != nil {
		t.Fatal(err)
	}

	info := app.EngineStatus()
	if info.Available {
		t.Fatal("a broken engine was reported as available")
	}
	if !strings.Contains(info.Error, "could not run") {
		t.Errorf("unhelpful error: %q", info.Error)
	}
	// The path is still reported, because that is what has to be repaired.
	if info.Path != path {
		t.Errorf("path: got %q, want %q", info.Path, path)
	}
}

// Resolving must point the compiler at what was found, or the app would go on
// running whatever happens to be called "tectonic" on PATH.
func TestResolveEnginePointsTheCompilerAtTheEngine(t *testing.T) {
	app, managed := engineApp(t)
	want := fakeEngine(t, managed, "Tectonic 0.17.0")

	app.mu.Lock()
	found := app.resolveEngineLocked()
	binary := app.compiler.Binary
	app.mu.Unlock()

	if !found.OK() {
		t.Fatal("nothing was found")
	}
	if binary != want {
		t.Errorf("compiler binary: got %q, want %q", binary, want)
	}
}

// With no engine anywhere, the compiler is left on its default name so the
// failure reads as "tectonic not found" rather than as an empty path.
func TestResolveEngineLeavesTheDefaultWhenThereIsNone(t *testing.T) {
	app, _ := engineApp(t)

	app.mu.Lock()
	found := app.resolveEngineLocked()
	binary := app.compiler.Binary
	app.mu.Unlock()

	if found.OK() {
		t.Fatalf("found an engine that should not exist: %+v", found)
	}
	if binary != "" {
		t.Errorf("compiler binary: got %q, want it cleared", binary)
	}
	// Which the compiler then reports under the default name.
	if _, err := app.compiler.Version(t.Context()); err == nil {
		t.Error("a missing engine reported a version")
	}
}

func TestEngineVersionSaysWhenThereIsNoEngine(t *testing.T) {
	app, _ := engineApp(t)
	app.mu.Lock()
	app.resolveEngineLocked()
	app.mu.Unlock()

	if got := app.EngineVersion(); !strings.Contains(got, "no engine") {
		t.Errorf("got %q", got)
	}
}

// Two installs at once would race over the same destination.
func TestInstallEngineRefusesASecondInstall(t *testing.T) {
	app, _ := engineApp(t)
	app.engine.mu.Lock()
	app.engine.installing = true
	app.engine.mu.Unlock()

	info := app.InstallEngine()
	if !strings.Contains(info.Error, "already being installed") {
		t.Errorf("got %q", info.Error)
	}
}

func TestInstallEngineNeedsSomewhereToPutIt(t *testing.T) {
	app := &App{compiler: tex.NewCompiler(), engineDir: ""}
	// appDir() answers for the real machine, so only the explicitly empty case
	// is asserted here — see managedEngineDir's comment.
	if app.managedEngineDir() == "" {
		if info := app.InstallEngine(); info.Error == "" {
			t.Error("an install with nowhere to go reported success")
		}
	}
}

func TestAppVersionDefaultsToDevelopment(t *testing.T) {
	app := &App{}
	if got := app.AppVersion(); got != "dev" {
		t.Errorf("an unstamped build reports %q, want %q", got, "dev")
	}
}
