// Package engine finds the TeX engine the app should run, and installs one
// when there is none.
//
// Three places are searched, in this order:
//
//  1. Bundled — shipped inside the installed application. This is the engine
//     the release was built and tested against, so it wins.
//  2. Managed — downloaded by the app into its own data directory, which only
//     happens because someone asked for it on the first-run screen.
//  3. PATH — whatever the machine already had. Last, because it is the one
//     version nobody here chose.
//
// The distinction matters beyond ordering: the status screen says which of the
// three answered, so "it compiles on my machine but not theirs" has a visible
// cause rather than being a mystery.
package engine

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// Source says where a located engine came from.
type Source string

const (
	SourceBundled Source = "bundled"
	SourceManaged Source = "managed"
	SourcePath    Source = "path"
	// SourceNone is the zero value: nothing was found.
	SourceNone Source = ""
)

// Found is the outcome of a search.
type Found struct {
	// Path is the executable, empty when nothing was found.
	Path string
	// Source is where it came from.
	Source Source
}

// OK reports whether an engine was found.
func (f Found) OK() bool { return f.Path != "" }

// ExecutableName is the engine binary's filename on this platform.
func ExecutableName() string { return executableNameFor(runtime.GOOS) }

// executableNameFor is ExecutableName for a platform that may not be this one,
// which a build host assembling an application for elsewhere needs.
func executableNameFor(goos string) string {
	if goos == "windows" {
		return "tectonic.exe"
	}
	return "tectonic"
}

// Candidate is a directory to look in, and what finding it there would mean.
type Candidate struct {
	Dir    string
	Source Source
}

// Candidates lists the directories to search, in order, for an app whose
// executable is at exePath and whose managed engine lives in managedDir.
//
// Either may be empty — a test binary has no bundle, and a machine with no
// writable config directory has no managed engine — and an empty one is simply
// not searched.
func Candidates(exePath, managedDir string) []Candidate {
	var out []Candidate

	if exePath != "" {
		dir := filepath.Dir(exePath)

		// A macOS app is a directory: the executable sits in Contents/MacOS and
		// everything shipped beside it belongs in Contents/Resources. Anywhere
		// else in the bundle is either ignored by the OS or signed separately.
		if runtime.GOOS == "darwin" && filepath.Base(dir) == "MacOS" {
			resources := filepath.Join(filepath.Dir(dir), "Resources")
			out = append(out, Candidate{Dir: filepath.Join(resources, "engine"), Source: SourceBundled})
		}

		// Windows and Linux ship a directory, so the engine goes in a
		// subdirectory of it. The executable's own directory is searched too,
		// which is what a hand-assembled portable copy tends to look like.
		out = append(out,
			Candidate{Dir: filepath.Join(dir, "engine"), Source: SourceBundled},
			Candidate{Dir: dir, Source: SourceBundled},
		)
	}

	if managedDir != "" {
		out = append(out, Candidate{Dir: managedDir, Source: SourceManaged})
	}
	return out
}

// Locate returns the first engine among the candidates, falling back to PATH.
func Locate(candidates []Candidate) Found {
	name := ExecutableName()
	for _, c := range candidates {
		if c.Dir == "" {
			continue
		}
		path := filepath.Join(c.Dir, name)
		if isExecutable(path) {
			return Found{Path: path, Source: c.Source}
		}
	}

	if path, err := exec.LookPath(name); err == nil {
		return Found{Path: path, Source: SourcePath}
	}
	return Found{}
}

// isExecutable reports whether path is a file this process could run.
//
// Windows has no execute bit — runnability is decided by the extension, which
// ExecutableName has already applied — so existence is the whole test there.
func isExecutable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return info.Mode()&0o111 != 0
}
