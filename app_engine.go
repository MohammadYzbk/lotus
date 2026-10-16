package main

// The TeX engine: finding one, and installing one when there is none.
//
// This is the first-run problem. Everything else in Lotus assumes a working
// engine; without one the app opens, shows a project, and fails at the only
// thing it exists to do. A writer who has never used a terminal cannot be sent
// to install Tectonic themselves, so the app finds a bundled engine if the
// installer shipped one, and otherwise offers to fetch the pinned release.

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/MohammadYzbk/lotus/internal/engine"
	wailsRuntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// EngineInfo is what the UI needs to explain the engine situation.
type EngineInfo struct {
	// Available is true when an engine was found and answered --version.
	Available bool `json:"available"`
	// Path is the executable in use.
	Path string `json:"path"`
	// Source is "bundled", "managed", or "path" — which of the three places
	// answered. Worth showing: it is the difference between "the app shipped
	// this" and "this machine happened to have one".
	Source string `json:"source"`
	// Version is the engine's own self-identification.
	Version string `json:"version"`
	// Installable is true when this platform has a release we could fetch.
	Installable bool `json:"installable"`
	// InstallVersion is the Tectonic release that would be installed.
	InstallVersion string `json:"installVersion"`
	// Installing is true while a download is in flight.
	Installing bool `json:"installing"`
	// Error explains the absence of an engine, in the writer's terms.
	Error string `json:"error"`
}

// engineState guards the install, which must not run twice at once.
type engineState struct {
	mu         sync.Mutex
	installing bool
}

// managedEngineDir is where an app-installed engine lives: inside the app's
// own directory, never a system location, so installing needs no privileges
// and uninstalling Lotus takes the engine with it.
//
// Injectable for the same reason settingsFile is: a test that searched — or
// worse, installed into — the real application-support directory would be
// reading the developer's own machine instead of a fixture.
func (a *App) managedEngineDir() string {
	if a.engineDir != "" {
		return a.engineDir
	}
	dir, err := appDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "engine")
}

// engineCandidates lists where to look on this machine.
func (a *App) engineCandidates() []engine.Candidate {
	exe, err := os.Executable()
	if err != nil {
		exe = ""
	} else if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		// A symlinked launcher would otherwise send the search to the wrong
		// directory, and on macOS out of the bundle entirely.
		exe = resolved
	}
	return engine.Candidates(exe, a.managedEngineDir())
}

// resolveEngineLocked points the compiler at the best engine available.
//
// Called at startup and after an install. Nothing fails here: an app with no
// engine still opens, and EngineStatus is what explains the situation.
func (a *App) resolveEngineLocked() engine.Found {
	found := engine.Locate(a.engineCandidates())
	if found.OK() {
		a.compiler.Binary = found.Path
	} else {
		// Leave the default name in place. It will not resolve, which is the
		// correct outcome, and it keeps error messages talking about
		// "tectonic" rather than an empty string.
		a.compiler.Binary = ""
	}
	return found
}

// EngineStatus reports what the app found, and what it could do about it.
func (a *App) EngineStatus() EngineInfo {
	a.engine.mu.Lock()
	installing := a.engine.installing
	a.engine.mu.Unlock()

	a.mu.Lock()
	found := a.resolveEngineLocked()
	a.mu.Unlock()

	info := EngineInfo{
		Path:           found.Path,
		Source:         string(found.Source),
		Installing:     installing,
		InstallVersion: engine.Version,
	}
	if _, err := engine.HostAsset(); err == nil {
		info.Installable = true
	} else {
		info.Error = err.Error()
	}

	if !found.OK() {
		if info.Installable {
			info.Error = "No TeX engine found."
		}
		return info
	}

	// Found is not the same as working: a truncated download or a binary built
	// for another architecture is on disk and executable, and only answering
	// --version proves otherwise.
	version, err := a.compiler.Version(a.context())
	if err != nil {
		info.Error = fmt.Sprintf("The engine at %s could not run: %v", found.Path, err)
		return info
	}
	info.Available = true
	info.Version = version
	return info
}

// InstallEngine downloads the pinned engine and puts the app on it.
//
// Progress is emitted as "engine:progress" so the first-run screen can show a
// bar; on a slow connection this is twenty megabytes, and a button that sits
// there for nine minutes with no sign of life reads as a hang.
func (a *App) InstallEngine() EngineInfo {
	a.engine.mu.Lock()
	if a.engine.installing {
		a.engine.mu.Unlock()
		info := a.EngineStatus()
		info.Error = "an engine is already being installed"
		return info
	}
	a.engine.installing = true
	a.engine.mu.Unlock()

	defer func() {
		a.engine.mu.Lock()
		a.engine.installing = false
		a.engine.mu.Unlock()
	}()

	dir := a.managedEngineDir()
	if dir == "" {
		return EngineInfo{Error: "could not find a directory to install the engine into"}
	}

	// Deliberately not holding a.mu: this runs for minutes, and holding the
	// app lock would freeze every compile, save, and tree operation behind it.
	_, err := engine.Install(a.context(), dir, func(done, total int64) {
		wailsRuntime.EventsEmit(a.context(), "engine:progress", map[string]int64{
			"done": done, "total": total,
		})
	})
	if err != nil {
		info := a.EngineStatus()
		info.Error = err.Error()
		return info
	}
	return a.EngineStatus()
}
