package engine

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// installEngine writes a stand-in engine into dir and returns its path.
func installEngine(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ExecutableName())
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// emptyPath points PATH at a directory with nothing in it, so a test that is
// about the bundle does not quietly pass on the developer's own Tectonic.
func emptyPath(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
}

// The bundled engine is the one the release was tested with, so it has to win
// even when the machine has others.
func TestLocatePrefersBundledOverManagedAndPath(t *testing.T) {
	emptyPath(t)
	root := t.TempDir()
	bundled := installEngine(t, filepath.Join(root, "bundle"))
	installEngine(t, filepath.Join(root, "managed"))

	found := Locate([]Candidate{
		{Dir: filepath.Join(root, "bundle"), Source: SourceBundled},
		{Dir: filepath.Join(root, "managed"), Source: SourceManaged},
	})
	if found.Path != bundled {
		t.Errorf("path: got %q, want %q", found.Path, bundled)
	}
	if found.Source != SourceBundled {
		t.Errorf("source: got %q, want %q", found.Source, SourceBundled)
	}
}

func TestLocateFallsBackToTheManagedEngine(t *testing.T) {
	emptyPath(t)
	root := t.TempDir()
	managed := installEngine(t, filepath.Join(root, "managed"))

	found := Locate([]Candidate{
		{Dir: filepath.Join(root, "bundle"), Source: SourceBundled},
		{Dir: filepath.Join(root, "managed"), Source: SourceManaged},
	})
	if found.Path != managed || found.Source != SourceManaged {
		t.Errorf("got %+v, want the managed engine at %q", found, managed)
	}
}

// Whatever is already on the machine is the last resort, and saying so is the
// point: the status screen names the source.
func TestLocateFallsBackToPath(t *testing.T) {
	dir := t.TempDir()
	onPath := installEngine(t, dir)
	t.Setenv("PATH", dir)

	found := Locate([]Candidate{{Dir: filepath.Join(t.TempDir(), "absent"), Source: SourceBundled}})
	if found.Source != SourcePath {
		t.Fatalf("source: got %q, want %q", found.Source, SourcePath)
	}
	// LookPath may return the path in a different but equivalent spelling.
	if filepath.Base(found.Path) != filepath.Base(onPath) {
		t.Errorf("path: got %q, want something named %q", found.Path, filepath.Base(onPath))
	}
}

func TestLocateFindsNothingWhenThereIsNothing(t *testing.T) {
	emptyPath(t)
	found := Locate([]Candidate{{Dir: t.TempDir(), Source: SourceBundled}})
	if found.OK() {
		t.Errorf("found %+v in an empty tree", found)
	}
}

// A file that happens to be named tectonic but cannot be run is not an engine.
// Reporting it as one turns a clear "no engine" into a failed compile.
func TestLocateIgnoresAFileItCannotRun(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows decides runnability by extension, not by a mode bit")
	}
	emptyPath(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ExecutableName()), []byte("not executable"), 0o644); err != nil {
		t.Fatal(err)
	}

	if found := Locate([]Candidate{{Dir: dir, Source: SourceBundled}}); found.OK() {
		t.Errorf("a non-executable file was accepted: %+v", found)
	}
}

func TestLocateIgnoresADirectoryOfTheRightName(t *testing.T) {
	emptyPath(t)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ExecutableName()), 0o755); err != nil {
		t.Fatal(err)
	}

	if found := Locate([]Candidate{{Dir: dir, Source: SourceBundled}}); found.OK() {
		t.Errorf("a directory was accepted as an engine: %+v", found)
	}
}

// On macOS the only place a bundled file may live is Contents/Resources.
func TestCandidatesLookInsideAMacApplicationBundle(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the Contents/Resources layout is macOS-only")
	}
	exe := "/Applications/Lotus.app/Contents/MacOS/lotus"

	want := "/Applications/Lotus.app/Contents/Resources/engine"
	for _, c := range Candidates(exe, "") {
		if c.Dir == want {
			if c.Source != SourceBundled {
				t.Errorf("source for %q: got %q", want, c.Source)
			}
			return
		}
	}
	t.Errorf("Resources/engine is not searched; got %+v", Candidates(exe, ""))
}

func TestCandidatesSkipWhatIsNotThere(t *testing.T) {
	if got := Candidates("", ""); len(got) != 0 {
		t.Errorf("with no executable and no managed directory, got %+v", got)
	}

	got := Candidates("", "/data/engine")
	if len(got) != 1 || got[0].Source != SourceManaged {
		t.Errorf("got %+v, want only the managed directory", got)
	}
}

func TestCandidateOrderIsBundledThenManaged(t *testing.T) {
	got := Candidates(filepath.Join("app", "lotus"), filepath.Join("data", "engine"))
	if len(got) == 0 {
		t.Fatal("no candidates")
	}
	if got[0].Source != SourceBundled {
		t.Errorf("first candidate: got %q, want %q", got[0].Source, SourceBundled)
	}
	if last := got[len(got)-1]; last.Source != SourceManaged {
		t.Errorf("last candidate: got %q, want %q", last.Source, SourceManaged)
	}
}

func TestAssetForEverySupportedPlatform(t *testing.T) {
	for _, platform := range SupportedPlatforms() {
		goos, goarch, ok := strings.Cut(platform, "/")
		if !ok {
			t.Fatalf("malformed platform key %q", platform)
		}
		asset, err := AssetFor(goos, goarch)
		if err != nil {
			t.Fatalf("%s: %v", platform, err)
		}
		if len(asset.SHA256) != 64 {
			t.Errorf("%s: digest %q is not a sha256", platform, asset.SHA256)
		}
		if !strings.Contains(asset.Name, Version) {
			t.Errorf("%s: %q is not the pinned version %s", platform, asset.Name, Version)
		}
		// The tag's "@" has to reach the URL encoded, or GitHub serves a 404.
		if url := asset.URL(); !strings.Contains(url, "tectonic%40"+Version) {
			t.Errorf("%s: url %q does not carry the escaped tag", platform, url)
		}
	}
}

// Someone on a platform Tectonic does not publish for should be told that, not
// handed a download that cannot work.
func TestAssetForAnUnsupportedPlatform(t *testing.T) {
	_, err := AssetFor("plan9", "mips")
	if err == nil {
		t.Fatal("plan9/mips was accepted")
	}
	if !strings.Contains(err.Error(), "plan9/mips") {
		t.Errorf("the error does not name the platform: %v", err)
	}
}

func TestExecutableNameMatchesThePlatform(t *testing.T) {
	got := ExecutableName()
	want := "tectonic"
	if runtime.GOOS == "windows" {
		want = "tectonic.exe"
	}
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
