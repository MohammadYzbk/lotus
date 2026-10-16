package engine

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
)

// ErrNotInArchive means the download unpacked, but the engine was not in it.
var ErrNotInArchive = errors.New("engine: the archive contains no engine binary")

// maxEngineSize bounds what we will write out of an archive.
//
// Tectonic is around 60 MB unpacked. The cap is well clear of that and still
// well short of filling a disk, which is what an archive claiming an enormous
// member would otherwise do — the size in the header is the archive's claim,
// not something we can check before writing.
const maxEngineSize = 512 << 20

// extractEngine writes the engine binary from the archive at src to dst.
//
// Only the one member whose base name is the engine's is taken, and dst is a
// path this package constructed. Nothing in the archive influences where
// anything lands, so an entry named "../../bin/sh" is simply not matched.
func extractEngine(src, dst string, kind Archive, want string) error {
	switch kind {
	case ArchiveTarGz:
		return extractTarGz(src, dst, want)
	case ArchiveZip:
		return extractZip(src, dst, want)
	default:
		return fmt.Errorf("engine: unknown archive kind %q", kind)
	}
}

func extractTarGz(src, dst, want string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("engine: read archive: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return ErrNotInArchive
		}
		if err != nil {
			return fmt.Errorf("engine: read archive: %w", err)
		}
		if header.Typeflag != tar.TypeReg || path.Base(header.Name) != want {
			continue
		}
		return writeExecutable(dst, tr)
	}
}

func extractZip(src, dst, want string) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return fmt.Errorf("engine: read archive: %w", err)
	}
	defer r.Close()

	for _, entry := range r.File {
		if entry.FileInfo().IsDir() || path.Base(entry.Name) != want {
			continue
		}
		rc, err := entry.Open()
		if err != nil {
			return fmt.Errorf("engine: read archive: %w", err)
		}
		defer rc.Close()
		return writeExecutable(dst, rc)
	}
	return ErrNotInArchive
}

// writeExecutable copies r to dst and makes it runnable.
func writeExecutable(dst string, r io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("engine: create directory: %w", err)
	}
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return fmt.Errorf("engine: write engine: %w", err)
	}

	written, err := io.Copy(f, io.LimitReader(r, maxEngineSize+1))
	if err != nil {
		f.Close()
		os.Remove(dst)
		return fmt.Errorf("engine: write engine: %w", err)
	}
	if written > maxEngineSize {
		f.Close()
		os.Remove(dst)
		return fmt.Errorf("engine: the archive's engine is larger than %d bytes", int64(maxEngineSize))
	}
	if err := f.Close(); err != nil {
		os.Remove(dst)
		return fmt.Errorf("engine: write engine: %w", err)
	}

	// The mode passed to OpenFile is masked by the process umask, so an 0o077
	// umask would have produced a file this process cannot run. Set it outright.
	if err := os.Chmod(dst, 0o755); err != nil {
		os.Remove(dst)
		return fmt.Errorf("engine: make engine executable: %w", err)
	}
	return nil
}
