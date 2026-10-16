// Command bundle-engine puts a Tectonic binary inside a built application, so
// the installed app has an engine without the writer installing one.
//
// It runs as a Wails post-build hook, and separately in CI before packaging.
// Written in Go rather than as a shell script because it has to do the same
// thing on all three platforms, and because it reuses internal/engine — the
// pinned version, the digests, and the unpacking are then defined once, and
// the app and the installer cannot disagree about which engine that is.
//
//	go run ./build/cmd/bundle-engine -app build/bin/lotus.app -platform darwin/arm64
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/MohammadYzbk/lotus/internal/engine"
)

func main() {
	app := flag.String("app", "", "the built application: a .app bundle, or the directory holding the executable")
	platform := flag.String("platform", runtime.GOOS+"/"+runtime.GOARCH, "GOOS/GOARCH to bundle for")
	cache := flag.String("cache", "", "directory to keep downloads in, so repeat builds do not refetch")
	flag.Parse()

	if err := run(*app, *platform, *cache); err != nil {
		fmt.Fprintln(os.Stderr, "bundle-engine:", err)
		os.Exit(1)
	}
}

func run(app, platform, cache string) error {
	if app == "" {
		return errors.New("-app is required")
	}
	goos, goarch, ok := strings.Cut(platform, "/")
	if !ok {
		return fmt.Errorf("-platform %q is not GOOS/GOARCH", platform)
	}

	dest, err := engineDir(app, goos)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dest, err)
	}

	installed, err := engine.BundleInto(context.Background(), dest, goos, goarch, cache, progress)
	if err != nil {
		return err
	}
	fmt.Printf("bundle-engine: Tectonic %s -> %s\n", engine.Version, installed)
	return nil
}

// engineDir is where a bundled engine belongs for a platform, and has to agree
// with engine.Candidates — that pairing is what makes the bundle findable.
func engineDir(app, goos string) (string, error) {
	switch {
	case goos == "darwin" && strings.HasSuffix(app, ".app"):
		return filepath.Join(app, "Contents", "Resources", "engine"), nil
	case goos == "darwin":
		return "", fmt.Errorf("on darwin, -app must be the .app bundle, got %q", app)
	default:
		// Windows and Linux ship a directory; the engine goes beside the
		// executable in an "engine" subdirectory.
		return filepath.Join(app, "engine"), nil
	}
}

// reportEvery is how often progress is printed. A CI log wants to show the job
// moving, not a progress bar's worth of redrawn lines.
const reportEvery = 10

var lastReported = -reportEvery

// progress prints a line every reportEvery percent.
func progress(done, total int64) {
	if total <= 0 {
		return
	}
	pct := int(done * 100 / total)
	if pct/reportEvery > lastReported/reportEvery || done == total {
		lastReported = pct
		fmt.Printf("bundle-engine: %d%%\n", pct)
	}
}
