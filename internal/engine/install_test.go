package engine

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const engineBody = "#!/bin/sh\necho Tectonic 0.17.0\n"

// tarGz packs the named files into a gzipped tarball.
func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		header := &tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func zipped(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func writeFile(t *testing.T, dir, name string, body []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestExtractTarGzTakesTheEngine(t *testing.T) {
	dir := t.TempDir()
	src := writeFile(t, dir, "a.tar.gz", tarGz(t, map[string]string{
		ExecutableName(): engineBody,
		"README.md":      "not the engine",
	}))

	dst := filepath.Join(dir, "out", ExecutableName())
	if err := extractEngine(src, dst, ArchiveTarGz, ExecutableName()); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != engineBody {
		t.Errorf("content: got %q", got)
	}
	if !isExecutable(dst) {
		t.Error("the extracted engine is not executable")
	}
}

func TestExtractZipTakesTheEngine(t *testing.T) {
	dir := t.TempDir()
	src := writeFile(t, dir, "a.zip", zipped(t, map[string]string{
		"tectonic-0.17.0/" + ExecutableName(): engineBody,
		"tectonic-0.17.0/LICENSE":             "text",
	}))

	dst := filepath.Join(dir, "out", ExecutableName())
	if err := extractEngine(src, dst, ArchiveZip, ExecutableName()); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != engineBody {
		t.Errorf("content: got %q", got)
	}
}

// An archive that unpacks but holds no engine has to say so, rather than
// leaving an empty file that fails later as a baffling exec error.
func TestExtractReportsAnArchiveWithNoEngine(t *testing.T) {
	dir := t.TempDir()
	src := writeFile(t, dir, "a.tar.gz", tarGz(t, map[string]string{"README.md": "text"}))

	err := extractEngine(src, filepath.Join(dir, ExecutableName()), ArchiveTarGz, ExecutableName())
	if !errors.Is(err, ErrNotInArchive) {
		t.Errorf("got %v, want ErrNotInArchive", err)
	}
}

// Nothing in an archive may decide where a file lands. An entry that tries to
// escape is simply not the engine, so it is never matched.
func TestExtractIgnoresAnEscapingEntry(t *testing.T) {
	dir := t.TempDir()
	src := writeFile(t, dir, "a.tar.gz", tarGz(t, map[string]string{
		"../../../../tmp/" + ExecutableName(): "escaped",
		"inner/" + ExecutableName():           engineBody,
	}))

	dst := filepath.Join(dir, "out", ExecutableName())
	if err := extractEngine(src, dst, ArchiveTarGz, ExecutableName()); err != nil {
		t.Fatal(err)
	}
	// Whichever entry matched, it was written where we said, not where the
	// archive said.
	if _, err := os.Stat(dst); err != nil {
		t.Fatalf("the engine did not land at the path we chose: %v", err)
	}
	outside := filepath.Join(dir, "..", "..", "..", "..", "tmp", ExecutableName())
	if _, err := os.Stat(filepath.Clean(outside)); err == nil {
		t.Error("a file was written outside the destination")
	}
}

// serveAsset stands in for GitHub, returning body for any request.
func serveAsset(t *testing.T, body []byte, status int) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Length", itoa(len(body)))
		w.Write(body)
	}))
	t.Cleanup(server.Close)
	return server
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

func TestDownloadVerifiesTheChecksum(t *testing.T) {
	body := tarGz(t, map[string]string{ExecutableName(): engineBody})
	server := serveAsset(t, body, http.StatusOK)
	dir := t.TempDir()

	asset := Asset{Name: "engine.tar.gz", SHA256: digest(body), Archive: ArchiveTarGz}
	if err := download(t.Context(), assetAt(asset, server.URL), filepath.Join(dir, "a"), nil); err != nil {
		t.Fatalf("a correct download was rejected: %v", err)
	}
}

// The digest is the only thing standing between a corrupted or swapped
// download and an executable we then run.
func TestDownloadRejectsAWrongChecksum(t *testing.T) {
	body := tarGz(t, map[string]string{ExecutableName(): engineBody})
	server := serveAsset(t, body, http.StatusOK)
	dir := t.TempDir()

	asset := Asset{
		Name:    "engine.tar.gz",
		SHA256:  "0000000000000000000000000000000000000000000000000000000000000000",
		Archive: ArchiveTarGz,
	}
	err := download(t.Context(), assetAt(asset, server.URL), filepath.Join(dir, "a"), nil)
	if err == nil {
		t.Fatal("a download that did not match its checksum was accepted")
	}
	if !strings.Contains(err.Error(), "checksum") {
		t.Errorf("unhelpful error: %v", err)
	}
}

func TestDownloadReportsAServerError(t *testing.T) {
	server := serveAsset(t, nil, http.StatusNotFound)
	asset := Asset{Name: "engine.tar.gz", SHA256: digest(nil), Archive: ArchiveTarGz}

	err := download(t.Context(), assetAt(asset, server.URL), filepath.Join(t.TempDir(), "a"), nil)
	if err == nil {
		t.Fatal("a 404 was accepted")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("the error does not carry the status: %v", err)
	}
}

func TestDownloadReportsProgress(t *testing.T) {
	body := tarGz(t, map[string]string{ExecutableName(): engineBody})
	server := serveAsset(t, body, http.StatusOK)
	asset := Asset{Name: "engine.tar.gz", SHA256: digest(body), Archive: ArchiveTarGz}

	var last, total int64
	err := download(t.Context(), assetAt(asset, server.URL), filepath.Join(t.TempDir(), "a"),
		func(done, size int64) { last, total = done, size })
	if err != nil {
		t.Fatal(err)
	}
	if last != int64(len(body)) {
		t.Errorf("final progress: got %d, want %d", last, len(body))
	}
	if total != int64(len(body)) {
		t.Errorf("reported total: got %d, want %d", total, len(body))
	}
}

// assetAt points an asset at a test server by overriding its URL.
func assetAt(a Asset, base string) Asset {
	a.urlOverride = base + "/" + a.Name
	return a
}

func TestInstallLeavesNoEngineWhenTheDownloadIsRejected(t *testing.T) {
	dir := t.TempDir()
	body := []byte("corrupt")
	server := serveAsset(t, body, http.StatusOK)

	asset := Asset{Name: "engine.tar.gz", SHA256: digest([]byte("something else")), Archive: ArchiveTarGz}
	_, err := install(t.Context(), dir, assetAt(asset, server.URL), nil)
	if err == nil {
		t.Fatal("a corrupt download was installed")
	}
	if _, statErr := os.Stat(filepath.Join(dir, ExecutableName())); statErr == nil {
		t.Error("an engine was left behind by a failed install")
	}
	// The workspace is cleaned up too, so a retry does not accumulate debris.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the failed install left %d entries behind: %v", len(entries), entries)
	}
}

func TestInstallWritesARunnableEngine(t *testing.T) {
	dir := t.TempDir()
	body := tarGz(t, map[string]string{"tectonic-0.17.0/" + ExecutableName(): engineBody})
	server := serveAsset(t, body, http.StatusOK)

	asset := Asset{Name: "engine.tar.gz", SHA256: digest(body), Archive: ArchiveTarGz}
	path, err := install(t.Context(), dir, assetAt(asset, server.URL), nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, ExecutableName()); path != want {
		t.Errorf("path: got %q, want %q", path, want)
	}
	if !isExecutable(path) {
		t.Error("the installed engine is not executable")
	}

	// And it is where Locate will look for a managed engine.
	if found := Locate([]Candidate{{Dir: dir, Source: SourceManaged}}); found.Path != path {
		t.Errorf("Locate did not find the installed engine: %+v", found)
	}
}

// Installing over an engine that is already there replaces it, which is what
// repairing a broken install looks like.
func TestInstallReplacesAnExistingEngine(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, ExecutableName())
	if err := os.WriteFile(existing, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}

	body := tarGz(t, map[string]string{ExecutableName(): engineBody})
	server := serveAsset(t, body, http.StatusOK)
	asset := Asset{Name: "engine.tar.gz", SHA256: digest(body), Archive: ArchiveTarGz}

	if _, err := install(t.Context(), dir, assetAt(asset, server.URL), nil); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(existing)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != engineBody {
		t.Errorf("the old engine is still in place: %q", got)
	}
}

func TestInstallNeedsADirectory(t *testing.T) {
	if _, err := Install(t.Context(), "", nil); err == nil {
		t.Error("an empty destination was accepted")
	}
}
