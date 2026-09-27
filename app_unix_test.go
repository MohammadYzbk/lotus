//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/MohammadYzbk/lotus/internal/project"
)

func TestDiagnosticsResolveIncludedSourceThroughSymlinkedProjectRoot(t *testing.T) {
	realRoot := t.TempDir()
	chapter := filepath.Join(realRoot, "chapters", "my chapter.tex")
	if err := os.MkdirAll(filepath.Dir(chapter), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realRoot, "main.tex"), []byte(rootDoc), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(chapter, []byte("\\missing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	linkedRoot := filepath.Join(t.TempDir(), "linked-project")
	if err := os.Symlink(realRoot, linkedRoot); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	p, err := project.Open(linkedRoot)
	if err != nil {
		t.Fatal(err)
	}
	a := &App{proj: p}

	diagnostics := a.diagnosticsLocked("error: chapters/my chapter:1: Undefined control sequence\nl.1 \\missing\n")

	if len(diagnostics) != 1 || diagnostics[0].File != "chapters/my chapter.tex" || diagnostics[0].Line != 1 {
		t.Fatalf("symlink-root diagnostics = %+v, want chapters/my chapter.tex:1", diagnostics)
	}
}

func TestDiagnosticsResolveContainedSymlinkSources(t *testing.T) {
	a := newTestApp(t, map[string]string{
		"main.tex":                   rootDoc,
		"chapters/actual.tex":        "\\missing\n",
		"chapters/nested actual.tex": "\\nested\n",
	})
	if err := os.Symlink("chapters/actual.tex", filepath.Join(a.proj.Root(), "linked.tex")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := os.Symlink("chapters", filepath.Join(a.proj.Root(), "linked chapters")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	// A contained cycle must remain visible as a link without making manifest
	// traversal recurse forever.
	if err := os.Symlink("..", filepath.Join(a.proj.Root(), "chapters", "cycle")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	exact := a.diagnosticsLocked("error: linked.tex:1: Undefined control sequence\nl.1 \\missing\n")
	a.invalidateDiagnosticManifestLocked()
	extensionFallback := a.diagnosticsLocked("error: linked chapters/nested actual:1: Undefined control sequence\nl.1 \\nested\n")

	if len(exact) != 1 || exact[0].File != "linked.tex" || exact[0].Line != 1 {
		t.Fatalf("exact contained-symlink diagnostics = %+v", exact)
	}
	if len(extensionFallback) != 1 || extensionFallback[0].File != "linked chapters/nested actual.tex" || extensionFallback[0].Line != 1 {
		t.Fatalf("extension-fallback contained-symlink diagnostics = %+v", extensionFallback)
	}
}

func TestDiagnosticsDoNotResolveExternalSymlinkSource(t *testing.T) {
	a := newTestApp(t, map[string]string{"main.tex": rootDoc})
	outside := filepath.Join(t.TempDir(), "outside chapter.tex")
	if err := os.WriteFile(outside, []byte("\\missing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(a.proj.Root(), "external chapter.tex")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	diagnostics := a.diagnosticsLocked("error: external chapter:1: Undefined control sequence\nl.1 \\missing\n")

	if len(diagnostics) != 1 {
		t.Fatalf("external-symlink diagnostics = %+v, want one unlocated error", diagnostics)
	}
	if diagnostics[0].File == "external chapter.tex" {
		t.Fatalf("external symlink was admitted as a project source: %+v", diagnostics[0])
	}
}

func TestDiagnosticManifestDirectoryRaceCannotBlockOnFIFO(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("nonblocking directory open is a Darwin/Linux boundary")
	}
	a := newTestApp(t, map[string]string{
		"main.tex":         rootDoc,
		"chapters/one.tex": "text\n",
	})
	directoryPath := filepath.Join(a.proj.Root(), "chapters")
	originalPath := directoryPath + ".original"
	replaced := make(chan struct{})
	result := make(chan *diagnosticFileManifest, 1)
	var hookErr error
	go func() {
		result <- diagnosticProjectFileManifestWithHooks(a.proj.Root(), func(relative string) {
			if relative != "chapters" {
				return
			}
			defer close(replaced)
			if err := os.Rename(directoryPath, originalPath); err != nil {
				hookErr = err
				return
			}
			if err := syscall.Mkfifo(directoryPath, 0o600); err != nil {
				hookErr = err
			}
		}, nil)
	}()
	select {
	case <-replaced:
	case <-time.After(time.Second):
		t.Fatal("manifest did not reach the raced directory")
	}
	select {
	case manifest := <-result:
		if hookErr != nil {
			t.Fatalf("prepare directory race: %v", hookErr)
		}
		if canonical, known := manifest.resolve("chapters/one.tex"); known {
			t.Fatalf("raced directory source resolved as %q", canonical)
		}
	case <-time.After(250 * time.Millisecond):
		writer, _ := os.OpenFile(directoryPath, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if writer != nil {
			_ = writer.Close()
		}
		t.Fatal("diagnostic manifest blocked opening a directory raced to FIFO")
	}
	if err := os.Remove(directoryPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(originalPath, directoryPath); err != nil {
		t.Fatal(err)
	}
}

func TestDiagnosticsRejectFIFOAtMetadataBoundaryBeforeReading(t *testing.T) {
	a := newTestApp(t, map[string]string{"main.tex": rootDoc})
	if err := syscall.Mkfifo(filepath.Join(a.proj.Root(), "blocked.tex"), 0o600); err != nil {
		t.Fatal(err)
	}
	reads := make(map[string]int)
	a.diagnosticReadText = func(relative string) (string, error) {
		reads[relative]++
		return a.proj.ReadText(relative)
	}

	diagnostics := a.diagnosticsLocked("error: blocked.tex:1: must not block\n")

	if len(diagnostics) != 1 {
		t.Fatalf("diagnostics = %+v, want the unmapped engine diagnostic", diagnostics)
	}
	if reads["blocked.tex"] != 0 {
		t.Fatalf("FIFO ReadText calls = %d, want metadata rejection", reads["blocked.tex"])
	}
	if info, err := os.Lstat(filepath.Join(a.proj.Root(), "blocked.tex")); err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("FIFO fixture changed: info=%v error=%v", info, err)
	}
}

func TestDiagnosticsCanonicalizesCaseAliasOnInsensitiveFilesystem(t *testing.T) {
	a := newTestApp(t, map[string]string{
		"main.tex":           rootDoc,
		"chapters/Intro.tex": "\\missing\n",
	})
	actual := filepath.Join(a.proj.Root(), "chapters", "Intro.tex")
	alias := filepath.Join(a.proj.Root(), "chapters", "intro.tex")
	actualInfo, err := os.Stat(actual)
	if err != nil {
		t.Fatal(err)
	}
	aliasInfo, err := os.Stat(alias)
	if os.IsNotExist(err) || err == nil && !os.SameFile(actualInfo, aliasInfo) {
		t.Skip("fixture filesystem is case-sensitive")
	}
	if err != nil {
		t.Fatal(err)
	}

	diagnostics := a.diagnosticsLocked("error: chapters/intro:1: Undefined control sequence\nl.1 \\missing\n")

	if len(diagnostics) != 1 || diagnostics[0].File != "chapters/Intro.tex" || diagnostics[0].Line != 1 {
		t.Fatalf("diagnostics = %+v, want canonical chapters/Intro.tex:1", diagnostics)
	}
}

func TestCachedDiagnosticManifestCanonicalizesExternalCaseAlias(t *testing.T) {
	a := newTestApp(t, map[string]string{"main.tex": rootDoc})
	_ = a.diagnosticsLocked("error: main.tex:1: initial failure\n")
	actual := filepath.Join(a.proj.Root(), "Late.tex")
	if err := os.WriteFile(actual, []byte("\\missing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(a.proj.Root(), "late.tex")
	actualInfo, err := os.Stat(actual)
	if err != nil {
		t.Fatal(err)
	}
	aliasInfo, err := os.Stat(alias)
	if os.IsNotExist(err) || err == nil && !os.SameFile(actualInfo, aliasInfo) {
		t.Skip("fixture filesystem is case-sensitive")
	}
	if err != nil {
		t.Fatal(err)
	}

	diagnostics := a.diagnosticsLocked("error: late.tex:1: Undefined control sequence\nl.1 \\missing\n")

	if len(diagnostics) != 1 || diagnostics[0].File != "Late.tex" || diagnostics[0].Line != 1 {
		t.Fatalf("external case-alias diagnostics = %+v, want canonical Late.tex:1", diagnostics)
	}
}

func TestDiagnosticManifestDoesNotReuseAliasVerificationAcrossSpellings(t *testing.T) {
	a := newTestApp(t, map[string]string{
		"main.tex": rootDoc,
		"I.tex":    "text\n",
	})
	actualInfo, err := os.Stat(filepath.Join(a.proj.Root(), "I.tex"))
	if err != nil {
		t.Fatal(err)
	}
	aliasInfo, err := os.Stat(filepath.Join(a.proj.Root(), "i.tex"))
	if os.IsNotExist(err) || err == nil && !os.SameFile(actualInfo, aliasInfo) {
		t.Skip("fixture filesystem is case-sensitive")
	}
	if err != nil {
		t.Fatal(err)
	}
	manifest := diagnosticProjectFileManifest(a.proj.Root())
	if canonical, ok := manifest.resolve("i.tex"); !ok || canonical != "I.tex" {
		t.Fatalf("ASCII case alias = %q, %v, want I.tex, true", canonical, ok)
	}
	if canonical, ok := manifest.resolve("İ.tex"); ok {
		t.Fatalf("nonexistent dotted-I spelling resolved as %q after cached ASCII alias", canonical)
	}
}

func TestDiagnosticsReuseCanonicalAdmissionAcrossCaseAliases(t *testing.T) {
	a := newTestApp(t, map[string]string{
		"main.tex":           rootDoc,
		"chapters/Intro.tex": "\\missing\n",
		"late.tex ":          "\\late\n",
	})
	actualInfo, err := os.Stat(filepath.Join(a.proj.Root(), "chapters", "Intro.tex"))
	if err != nil {
		t.Fatal(err)
	}
	aliasInfo, err := os.Stat(filepath.Join(a.proj.Root(), "CHAPTERS", "INTRO.TEX"))
	if os.IsNotExist(err) || err == nil && !os.SameFile(actualInfo, aliasInfo) {
		t.Skip("fixture filesystem is case-sensitive")
	}
	if err != nil {
		t.Fatal(err)
	}
	reads := make(map[string]int)
	a.diagnosticReadText = func(relative string) (string, error) {
		reads[relative]++
		return a.proj.ReadText(relative)
	}
	rawLog := "error: chapters/intro.tex:1: first alias\n" +
		"error: CHAPTERS/INTRO.TEX:1: second alias\n" +
		"error: Chapters/intro.tex:1: third alias\n" +
		"error: late.tex :1: genuine later source\nl.1 \\late\n"

	diagnostics := a.diagnosticsLocked(rawLog)

	if reads["chapters/Intro.tex"] != 1 {
		t.Fatalf("canonical source reads = %d, want one canonical validation read", reads["chapters/Intro.tex"])
	}
	last := diagnostics[len(diagnostics)-1]
	if last.File != "late.tex " || last.Line != 1 {
		t.Fatalf("later diagnostic = %+v, want late.tex at line 1", last)
	}
}

func TestDiagnosticsCanonicalizeCaseAliasedRootFile(t *testing.T) {
	a := newTestApp(t, map[string]string{"Main.tex": rootDoc + "\\missing\n"})
	actualInfo, err := os.Stat(filepath.Join(a.proj.Root(), "Main.tex"))
	if err != nil {
		t.Fatal(err)
	}
	aliasInfo, err := os.Stat(filepath.Join(a.proj.Root(), "main.tex"))
	if os.IsNotExist(err) || err == nil && !os.SameFile(actualInfo, aliasInfo) {
		t.Skip("fixture filesystem is case-sensitive")
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := a.proj.SetRootFile("main.tex"); err != nil {
		t.Fatal(err)
	}

	diagnostics := a.diagnosticsLocked("error: main.tex:1: root failure\nl.1 \\missing\n")

	if len(diagnostics) != 1 || diagnostics[0].File != "Main.tex" {
		t.Fatalf("diagnostics = %+v, want canonical Main.tex", diagnostics)
	}
}

func TestDiagnosticManifestBoundsCaseAliasCanonicalization(t *testing.T) {
	a := newTestApp(t, map[string]string{
		"main.tex":     rootDoc,
		"abcdefgh.tex": "text\n",
		"real.tex":     "text\n",
	})
	actualInfo, err := os.Stat(filepath.Join(a.proj.Root(), "abcdefgh.tex"))
	if err != nil {
		t.Fatal(err)
	}
	aliasInfo, err := os.Stat(filepath.Join(a.proj.Root(), "ABCDEFGH.tex"))
	if os.IsNotExist(err) || err == nil && !os.SameFile(actualInfo, aliasInfo) {
		t.Skip("fixture filesystem is case-sensitive")
	}
	if err != nil {
		t.Fatal(err)
	}
	manifest := diagnosticProjectFileManifest(a.proj.Root())
	base := []byte("abcdefgh")
	for mask := 1; mask <= 1<<len(base); mask++ {
		candidate := append([]byte(nil), base...)
		for index := range candidate {
			if mask&(1<<index) != 0 {
				candidate[index] -= 'a' - 'A'
			}
		}
		_, _ = manifest.resolve(string(candidate) + ".tex")
	}
	if manifest.remainingFallbackProbes != 0 || manifest.remainingFallbackEntries < 0 {
		t.Fatalf("alias canonicalization work = probes %d, entries %d", manifest.remainingFallbackProbes, manifest.remainingFallbackEntries)
	}
	if canonical, known := manifest.resolve("real.tex"); !known || canonical != "real.tex" {
		t.Fatalf("late exact source = %q, %t, want real.tex, true", canonical, known)
	}
}

func TestDiagnosticManifestCanonicalizesUnicodeNormalizationAlias(t *testing.T) {
	for _, test := range []struct {
		name     string
		actual   string
		reported string
	}{
		{name: "nfc entry", actual: "résumé.tex", reported: "re\u0301sume\u0301.tex"},
		{name: "nfd entry", actual: "re\u0301sume\u0301.tex", reported: "résumé.tex"},
	} {
		t.Run(test.name, func(t *testing.T) {
			a := newTestApp(t, map[string]string{"main.tex": rootDoc, test.actual: "text\n"})
			actualInfo, err := os.Stat(filepath.Join(a.proj.Root(), test.actual))
			if err != nil {
				t.Fatal(err)
			}
			reportedInfo, err := os.Stat(filepath.Join(a.proj.Root(), test.reported))
			if os.IsNotExist(err) || err == nil && !os.SameFile(actualInfo, reportedInfo) {
				t.Skip("fixture filesystem is normalization-sensitive")
			}
			if err != nil {
				t.Fatal(err)
			}
			manifest := diagnosticProjectFileManifest(a.proj.Root())
			if canonical, known := manifest.resolve(test.reported); !known || canonical != test.actual {
				t.Fatalf("normalization alias = %q, %t, want %q, true", canonical, known, test.actual)
			}
		})
	}
}
