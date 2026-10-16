package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// DownloadTimeout bounds a whole install. Tectonic is around 20 MB packed, so
// this allows for a connection far slower than anyone would enjoy rather than
// cutting off a writer on hotel wifi halfway through.
const DownloadTimeout = 30 * time.Minute

// Progress reports how far a download has got. total is -1 when the server
// did not say how large the file is.
type Progress func(downloaded, total int64)

// Install downloads the pinned engine for this platform into dir and returns
// the installed executable's path.
//
// The download is verified against its digest before anything is unpacked, and
// the engine is assembled under a temporary name and renamed into place at the
// end. An install that fails partway therefore leaves either the previous
// engine or no engine, never a half-written one that would fail later with a
// stranger error.
func Install(ctx context.Context, dir string, progress Progress) (string, error) {
	asset, err := HostAsset()
	if err != nil {
		return "", err
	}
	return install(ctx, dir, asset, progress)
}

// BundleInto downloads the engine for an explicit platform into dir, for a
// build host assembling an application for somewhere else.
//
// cacheDir, when set, keeps the downloaded archive so that a repeated build —
// or the next job in a matrix — does not fetch twenty megabytes again. The
// cached copy is still checked against its digest before use, so a corrupted
// cache is caught rather than baked into a release.
func BundleInto(ctx context.Context, dir, goos, goarch, cacheDir string, progress Progress) (string, error) {
	asset, err := AssetFor(goos, goarch)
	if err != nil {
		return "", err
	}
	return fetch(ctx, dir, asset, executableNameFor(goos), cacheDir, progress)
}

// install is Install with the asset chosen by the caller, so tests can supply
// one served locally instead of reaching GitHub.
func install(ctx context.Context, dir string, asset Asset, progress Progress) (string, error) {
	return fetch(ctx, dir, asset, ExecutableName(), "", progress)
}

// fetch is the whole operation: obtain the archive, verify it, unpack the one
// file we want, and put it in place atomically.
func fetch(ctx context.Context, dir string, asset Asset, exeName, cacheDir string, progress Progress) (string, error) {
	if dir == "" {
		return "", errors.New("engine: no directory to install into")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("engine: create directory: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, DownloadTimeout)
	defer cancel()

	// Work inside the destination, so the final rename stays on one
	// filesystem. A rename across filesystems fails, and the system temporary
	// directory is very often on a different one.
	work, err := os.MkdirTemp(dir, ".install-")
	if err != nil {
		return "", fmt.Errorf("engine: create workspace: %w", err)
	}
	defer os.RemoveAll(work)

	archive, err := obtain(ctx, asset, work, cacheDir, progress)
	if err != nil {
		return "", err
	}

	staged := filepath.Join(work, exeName)
	if err := extractEngine(archive, staged, asset.Archive, exeName); err != nil {
		return "", err
	}

	installed := filepath.Join(dir, exeName)
	// Windows refuses to replace a file that is open, and a previous engine
	// may be running. Move the old one aside first; it is removed with the
	// workspace, or on the next install if it is still held.
	if _, err := os.Stat(installed); err == nil {
		_ = os.Rename(installed, filepath.Join(work, "previous-"+exeName))
	}
	if err := os.Rename(staged, installed); err != nil {
		return "", fmt.Errorf("engine: install engine: %w", err)
	}
	return installed, nil
}

// obtain returns a verified copy of the asset's archive, downloading it unless
// an already-good one is in the cache.
func obtain(ctx context.Context, asset Asset, work, cacheDir string, progress Progress) (string, error) {
	if cacheDir != "" {
		cached := filepath.Join(cacheDir, asset.Name)
		if err := verify(cached, asset.SHA256); err == nil {
			return cached, nil
		}
		// A cached file that fails its digest is debris, not a reason to stop.
		if err := os.MkdirAll(cacheDir, 0o755); err != nil {
			return "", fmt.Errorf("engine: create cache: %w", err)
		}
		if err := download(ctx, asset, cached, progress); err != nil {
			return "", err
		}
		return cached, nil
	}

	path := filepath.Join(work, asset.Name)
	if err := download(ctx, asset, path, progress); err != nil {
		return "", err
	}
	return path, nil
}

// download fetches the asset to dst and checks its digest.
func download(ctx context.Context, asset Asset, dst string, progress Progress) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.URL(), nil)
	if err != nil {
		return fmt.Errorf("engine: download: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("engine: download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("engine: download %s: %s", asset.Name, resp.Status)
	}

	f, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("engine: download: %w", err)
	}
	defer f.Close()

	sum := sha256.New()
	// Hash on the way past rather than re-reading the file afterwards: the
	// bytes are already in hand, and a second pass over 20 MB buys nothing.
	body := io.Reader(resp.Body)
	if progress != nil {
		body = &progressReader{r: body, total: resp.ContentLength, report: progress}
	}
	if _, err := io.Copy(io.MultiWriter(f, sum), body); err != nil {
		return fmt.Errorf("engine: download: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("engine: download: %w", err)
	}

	if got := hex.EncodeToString(sum.Sum(nil)); got != asset.SHA256 {
		os.Remove(dst)
		return fmt.Errorf("engine: %s does not match its published checksum "+
			"(expected %s, got %s) — the download was corrupted or the file has changed",
			asset.Name, asset.SHA256, got)
	}
	return nil
}

// verify reports whether the file at path has the expected digest.
func verify(path, want string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != want {
		return fmt.Errorf("engine: %s does not match its checksum", filepath.Base(path))
	}
	return nil
}

// progressReader reports download progress, at most ten times a second.
type progressReader struct {
	r        io.Reader
	total    int64
	done     int64
	last     time.Time
	report   Progress
	reported bool
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.done += int64(n)

	// Throttled: this fires per network read, and every report crosses into the
	// UI. The final one is unconditional so the bar always lands on full.
	if now := time.Now(); err != nil || !p.reported || now.Sub(p.last) > 100*time.Millisecond {
		p.last = now
		p.reported = true
		p.report(p.done, p.total)
	}
	return n, err
}
