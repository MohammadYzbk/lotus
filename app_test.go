package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MohammadYzbk/lotus/internal/project"
	"github.com/MohammadYzbk/lotus/internal/tex"
	"github.com/MohammadYzbk/lotus/internal/texlog"
)

const rootDoc = "\\documentclass{article}\n\\begin{document}\nhello\n\\end{document}\n"

// newTestApp opens a project in a temp directory, so tests never touch the real
// scratch project or the user's settings.
func newTestApp(t *testing.T, files map[string]string) *App {
	t.Helper()

	root := t.TempDir()
	for rel, content := range files {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	p, err := project.Open(root)
	if err != nil {
		t.Fatalf("open project: %v", err)
	}
	return &App{
		compiler: tex.NewCompiler(),
		proj:     p,
		outDir:   t.TempDir(),
		openFile: p.RootFile(),
		// Never the real settings file: several bound methods persist as a side
		// effect, and writing temp-dir paths into it reset the developer's own
		// open project on the next launch.
		settingsFile: filepath.Join(t.TempDir(), settingsFileName),
	}
}

func requireEngine(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath(tex.DefaultBinary); err != nil {
		t.Skipf("%s not on PATH", tex.DefaultBinary)
	}
}

func errorDiagnostics(diags []texlog.Diagnostic) []texlog.Diagnostic {
	var out []texlog.Diagnostic
	for _, d := range diags {
		if d.Severity == texlog.SeverityError {
			out = append(out, d)
		}
	}
	return out
}

// --- project and tree --------------------------------------------------------

func TestCurrentProjectDescribesTree(t *testing.T) {
	a := newTestApp(t, map[string]string{
		"main.tex":           rootDoc,
		"chapters/intro.tex": "An introduction.\n",
		"refs.bib":           "@book{x, title={y}}\n",
	})

	info := a.CurrentProject()
	if info.Error != "" {
		t.Fatalf("CurrentProject: %s", info.Error)
	}
	if info.RootFile != "main.tex" {
		t.Errorf("RootFile = %q, want main.tex", info.RootFile)
	}
	if len(info.Tree.Children) != 3 {
		t.Errorf("tree has %d children, want 3: %+v", len(info.Tree.Children), info.Tree.Children)
	}
}

func TestOpenProjectWithoutRootFileReportsIt(t *testing.T) {
	a := newTestApp(t, map[string]string{"notes.txt": "nothing to compile"})

	if got := a.CurrentProject().RootFile; got != "" {
		t.Errorf("RootFile = %q, want empty", got)
	}
	// Compiling must explain itself rather than failing obscurely.
	res := a.Compile()
	if res.Error == "" {
		t.Fatal("expected an error explaining there is nothing to compile")
	}
	if !strings.Contains(res.Error, "documentclass") {
		t.Errorf("error should say what is missing, got %q", res.Error)
	}
}

// --- the Phase 3 headline: compile the root, not the open file ---------------

// Editing a chapter must still compile the root document. Compiling the open
// file would typeset a fragment with no preamble.
func TestSaveAndCompileCompilesRootNotTheEditedFile(t *testing.T) {
	requireEngine(t)
	a := newTestApp(t, map[string]string{
		"main.tex": "\\documentclass{article}\n\\begin{document}\n" +
			"\\input{chapters/intro}\n\\end{document}\n",
		"chapters/intro.tex": "Original text.\n",
	})

	res := a.SaveAndCompile("chapters/intro.tex", "Edited chapter text.\n")
	if res.Error != "" {
		t.Fatalf("SaveAndCompile: %s\n%s", res.Error, res.Log)
	}
	if !res.Compiled {
		t.Fatalf("did not compile:\n%s", res.Log)
	}
	if res.RootFile != "main.tex" {
		t.Errorf("RootFile = %q, want main.tex", res.RootFile)
	}

	// The edit must be on disk...
	onDisk, err := os.ReadFile(filepath.Join(a.proj.Root(), "chapters", "intro.tex"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(onDisk), "Edited chapter text.") {
		t.Error("the edit was not written")
	}
	// ...and must have reached the PDF, which only happens if the root compiled.
	if !strings.Contains(res.Log, "main.tex") && res.PDFURL == "" {
		t.Error("no evidence the root document was the one compiled")
	}
}

// An error inside an \input file must be attributed to that file, not the root,
// or the marker lands in the wrong buffer.
func TestDiagnosticsAreAttributedToTheIncludedFile(t *testing.T) {
	requireEngine(t)
	a := newTestApp(t, map[string]string{
		"main.tex": "\\documentclass{article}\n\\begin{document}\n" +
			"\\input{chapters/intro}\n\\end{document}\n",
		"chapters/intro.tex": "Fine line.\n\\thisIsNotACommand\n",
	})

	res := a.Compile()
	if res.Error != "" {
		t.Fatalf("Compile: %s", res.Error)
	}
	errs := errorDiagnostics(res.Diagnostics)
	if len(errs) == 0 {
		t.Fatalf("no errors parsed from:\n%s", res.Log)
	}

	d := errs[0]
	if d.File != "chapters/intro.tex" {
		t.Errorf("File = %q, want chapters/intro.tex", d.File)
	}
	if d.Line != 2 {
		t.Errorf("Line = %d, want 2", d.Line)
	}
}

func TestDiagnosticsResolveSpacedExtensionlessFilenameAgainstProject(t *testing.T) {
	a := newTestApp(t, map[string]string{
		"main.tex":       rootDoc,
		"my chapter.tex": "\\missing\n",
	})
	rawLog := "note: Running TeX ...\n" +
		"error: my chapter:1: undefined control sequence\n" +
		"l.1 \\missing\n"

	diagnostics := a.diagnosticsLocked(rawLog)

	if len(diagnostics) != 1 {
		t.Fatalf("diagnostics = %+v, want one", diagnostics)
	}
	if diagnostics[0].File != "my chapter.tex" || diagnostics[0].Line != 1 {
		t.Fatalf("location = %q:%d, want my chapter.tex:1", diagnostics[0].File, diagnostics[0].Line)
	}
}

func TestDiagnosticsReuseProjectManifestAcrossUnchangedResults(t *testing.T) {
	a := newTestApp(t, map[string]string{
		"main.tex":           rootDoc,
		"chapters/intro.tex": "\\missing\n",
	})
	visitedEntries := 0
	a.diagnosticManifestEntry = func() { visitedEntries++ }
	rawLog := "error: chapters/intro.tex:1: Undefined control sequence\nl.1 \\missing\n"

	first := a.diagnosticsLocked(rawLog)
	firstWalkEntries := visitedEntries
	second := a.diagnosticsLocked(rawLog)

	if len(first) != 1 || len(second) != 1 || firstWalkEntries == 0 {
		t.Fatalf("diagnostics/manifest instrumentation = (%v, %v, %d)", first, second, firstWalkEntries)
	}
	if visitedEntries != firstWalkEntries {
		t.Fatalf("unchanged diagnostics rescanned project manifest: first %d total %d", firstWalkEntries, visitedEntries)
	}
}

func TestDiagnosticsAdmitExternalSourceWithoutRescanningCachedManifest(t *testing.T) {
	a := newTestApp(t, map[string]string{"main.tex": rootDoc})
	visitedEntries := 0
	a.diagnosticManifestEntry = func() { visitedEntries++ }
	_ = a.diagnosticsLocked("error: main.tex:1: initial failure\n")
	initialWalkEntries := visitedEntries
	if initialWalkEntries == 0 {
		t.Fatal("initial diagnostic manifest was not built")
	}
	latePath := filepath.Join(a.proj.Root(), "chapters", "late.tex")
	if err := os.MkdirAll(filepath.Dir(latePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(latePath, []byte("\\missing\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	diagnostics := a.diagnosticsLocked("error: chapters/late.tex:1: Undefined control sequence\nl.1 \\missing\n")

	if visitedEntries != initialWalkEntries {
		t.Fatalf("external-source admission rescanned manifest: initial %d total %d", initialWalkEntries, visitedEntries)
	}
	if len(diagnostics) != 1 || diagnostics[0].File != "chapters/late.tex" || diagnostics[0].Line != 1 {
		t.Fatalf("external source diagnostics = %+v, want chapters/late.tex:1", diagnostics)
	}
}

func TestDiagnosticManifestReprobesAStaleFoldedAlias(t *testing.T) {
	manifest := newDiagnosticFileManifest(t.TempDir())
	stale := "Foo.tex"
	candidate := "foo.tex"
	manifest.add(stale)
	probes := 0
	manifest.canonicalCandidate = func(root, reported string, remainingEntries *int) (string, bool) {
		probes++
		if root != manifest.root || reported != candidate || *remainingEntries != maximumDiagnosticFallbackScanEntries {
			t.Fatalf("fallback probe = (%q, %q, %d)", root, reported, *remainingEntries)
		}
		return candidate, true
	}
	canonical, known := manifest.resolve(candidate)
	if !known || canonical != candidate {
		t.Fatalf("stale alias resolution = %q, %t, want %q, true", canonical, known, candidate)
	}
	if probes != 1 {
		t.Fatalf("fallback probes = %d, want 1", probes)
	}
	if _, exact := manifest.exact[candidate]; !exact {
		t.Fatal("reprobed source was not admitted as an exact manifest entry")
	}
}

func TestDiagnosticsFollowCaseOnlyExternalRenameFromCachedManifest(t *testing.T) {
	requireEngine(t)
	a := newTestApp(t, map[string]string{
		"main.tex": "\\documentclass{article}\n\\begin{document}\n\\input{Foo}\n\\end{document}\n",
		"Foo.tex":  "\\thisIsNotACommand\n",
	})
	_ = a.diagnosticsLocked("error: Foo.tex:1: initial failure\nl.1 \\missing\n")
	oldPath := filepath.Join(a.proj.Root(), "Foo.tex")
	newPath := filepath.Join(a.proj.Root(), "foo.tex")
	if err := os.Rename(oldPath, newPath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldPath); os.IsNotExist(err) {
		t.Skip("fixture filesystem is case-sensitive")
	} else if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(a.proj.Root())
	if err != nil {
		t.Fatal(err)
	}
	foundCurrentSpelling := false
	for _, entry := range entries {
		if entry.Name() == "foo.tex" {
			foundCurrentSpelling = true
		}
		if entry.Name() == "Foo.tex" {
			t.Fatal("case-only rename did not update the directory entry spelling")
		}
	}
	if !foundCurrentSpelling {
		t.Fatal("case-only rename did not produce foo.tex")
	}

	result := a.Compile()
	if result.Error != "" {
		t.Fatalf("Compile: %s\n%s", result.Error, result.Log)
	}
	for _, diagnostic := range errorDiagnostics(result.Diagnostics) {
		if diagnostic.File == "foo.tex" && diagnostic.Line == 1 {
			return
		}
	}
	t.Fatalf("renamed source diagnostics = %+v, want foo.tex:1\n%s", result.Diagnostics, result.Log)
}

func TestDiagnosticsSpacedExtensionlessDirectoryFallsThroughToTeXFile(t *testing.T) {
	a := newTestApp(t, map[string]string{
		"main.tex":       rootDoc,
		"my chapter.tex": "\\missing\n",
	})
	if err := os.Mkdir(filepath.Join(a.proj.Root(), "my chapter"), 0o755); err != nil {
		t.Fatal(err)
	}
	rawLog := "error: my chapter:1: undefined control sequence\nl.1 \\missing\n"

	diagnostics := a.diagnosticsLocked(rawLog)

	if len(diagnostics) != 1 || diagnostics[0].File != "my chapter.tex" || diagnostics[0].Line != 1 {
		t.Fatalf("diagnostics = %+v, want my chapter.tex:1", diagnostics)
	}
}

func TestDiagnosticsResolveLeadingAndTrailingWhitespaceFilenames(t *testing.T) {
	files := map[string]string{"main.tex": rootDoc}
	for _, relative := range []string{" leading.tex", "trailing.tex ", "\tleading.tex", "trailing.tex\t"} {
		files[relative] = "\\missing\n"
	}
	a := newTestApp(t, files)
	for _, relative := range []string{" leading.tex", "trailing.tex ", "\tleading.tex", "trailing.tex\t"} {
		t.Run(fmt.Sprintf("%q", relative), func(t *testing.T) {
			rawLog := "error: " + relative + ":1: undefined control sequence\nl.1 \\missing\n"
			diagnostics := a.diagnosticsLocked(rawLog)
			if len(diagnostics) != 1 || diagnostics[0].File != relative || diagnostics[0].Line != 1 {
				t.Fatalf("diagnostics = %+v, want %q:1", diagnostics, relative)
			}
		})
	}
}

func TestDiagnosticsNegativeLookupCapDoesNotHideWhitespaceFilename(t *testing.T) {
	a := newTestApp(t, map[string]string{
		"main.tex":      rootDoc,
		"trailing.tex ": "\\missing\n",
	})
	var rawLog strings.Builder
	for index := 0; index < 4*maximumDiagnosticFileEntries; index++ {
		fmt.Fprintf(&rawLog, "error: fake-%03d.tex:1: nonexistent source\n", index)
	}
	rawLog.WriteString("error: trailing.tex :1: genuine whitespace filename error\nl.1 \\missing\n")

	diagnostics := a.diagnosticsLocked(rawLog.String())

	for _, diagnostic := range diagnostics {
		if strings.Contains(diagnostic.Message, "genuine whitespace filename error") {
			if diagnostic.File != "trailing.tex " || diagnostic.Line != 1 {
				t.Fatalf("genuine diagnostic = %+v, want trailing whitespace filename at line 1", diagnostic)
			}
			return
		}
	}
	t.Fatalf("genuine whitespace filename diagnostic missing: %+v", diagnostics)
}

func TestDiagnosticsPositiveAdmissionCapDoesNotHideLateWhitespaceFilename(t *testing.T) {
	files := map[string]string{
		"main.tex":      rootDoc,
		"trailing.tex ": "\\missing\n",
	}
	for index := 0; index < maximumDiagnosticFileEntries+8; index++ {
		files[fmt.Sprintf("decoy-%03d.tex", index)] = "ordinary\n"
	}
	a := newTestApp(t, files)
	var rawLog strings.Builder
	for index := 0; index < maximumDiagnosticFileEntries+8; index++ {
		fmt.Fprintf(&rawLog, "error: decoy-%03d.tex:999: real-file decoy\n", index)
	}
	rawLog.WriteString("error: trailing.tex :1: genuine whitespace filename error\nl.1 \\missing\n")

	diagnostics := a.diagnosticsLocked(rawLog.String())

	for _, diagnostic := range diagnostics {
		if strings.Contains(diagnostic.Message, "genuine whitespace filename error") {
			if diagnostic.File != "trailing.tex " || diagnostic.Line != 1 {
				t.Fatalf("genuine diagnostic = %+v, want trailing whitespace filename at line 1", diagnostic)
			}
			return
		}
	}
	t.Fatalf("genuine whitespace filename diagnostic missing: %+v", diagnostics)
}

func TestDiagnosticManifestFindsLateSourceAfterManyNonSources(t *testing.T) {
	a := newTestApp(t, map[string]string{"main.tex": rootDoc})
	assets := filepath.Join(a.proj.Root(), "assets")
	if err := os.Mkdir(assets, 0o700); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < maximumDiagnosticManifestEntries+32; index++ {
		if err := os.WriteFile(filepath.Join(assets, fmt.Sprintf("asset-%05d.png", index)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(a.proj.Root(), "late.tex"), []byte("\\missing\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	manifest := diagnosticProjectFileManifest(a.proj.Root())

	if canonical, known := manifest.resolve("late.tex"); !known || canonical != "late.tex" {
		t.Fatalf("late source = %q, %t, want late.tex, true", canonical, known)
	}
}

func TestDiagnosticsBoundFilesystemProbesAcrossEvictedNegativeCandidates(t *testing.T) {
	a := newTestApp(t, map[string]string{"main.tex": rootDoc})
	metadataCalls := 0
	a.diagnosticTextSize = func(relative string) (int64, error) {
		metadataCalls++
		return a.proj.EditableTextSize(relative)
	}
	var rawLog strings.Builder
	for attempt := 0; attempt < 4*(maximumDiagnosticFileEntries+1); attempt++ {
		fmt.Fprintf(&rawLog, "error: missing-%03d.tex:1: nonexistent source\n", attempt%(maximumDiagnosticFileEntries+1))
	}

	_ = a.diagnosticsLocked(rawLog.String())

	if metadataCalls > 2*maximumDiagnosticFileEntries {
		t.Fatalf("filesystem metadata probes = %d, want at most %d", metadataCalls, 2*maximumDiagnosticFileEntries)
	}
}

func TestDiagnosticsBoundPositiveAdmissionProbesAcrossKnownSourceCycling(t *testing.T) {
	files := map[string]string{"main.tex": rootDoc}
	const sources = 2*maximumDiagnosticFileEntries + 1
	for index := 0; index < sources; index++ {
		files[fmt.Sprintf("known-%03d.tex", index)] = ""
	}
	a := newTestApp(t, files)
	metadataCalls := 0
	a.diagnosticTextSize = func(relative string) (int64, error) {
		metadataCalls++
		return a.proj.EditableTextSize(relative)
	}
	var rawLog strings.Builder
	for cycle := 0; cycle < 4; cycle++ {
		for index := 0; index < sources; index++ {
			fmt.Fprintf(&rawLog, "error: known-%03d.tex:999: real-file decoy\n", index)
		}
	}

	_ = a.diagnosticsLocked(rawLog.String())

	if metadataCalls > 2*maximumDiagnosticFileEntries {
		t.Fatalf("positive admission metadata probes = %d, want at most %d", metadataCalls, 2*maximumDiagnosticFileEntries)
	}
}

func TestDiagnosticsReuseResolvedSourceContent(t *testing.T) {
	a := newTestApp(t, map[string]string{
		"main.tex": rootDoc + "\\foo\n\\bar\n",
	})
	reads := 0
	a.diagnosticReadText = func(relative string) (string, error) {
		reads++
		return a.proj.ReadText(relative)
	}
	rawLog := "error: File ended while scanning use of \\foo\n" +
		"error: File ended while scanning use of \\bar\n"

	diagnostics := a.diagnosticsLocked(rawLog)

	if len(diagnostics) != 2 || diagnostics[0].Line == 0 || diagnostics[1].Line == 0 {
		t.Fatalf("diagnostics = %+v, want both root-token locations", diagnostics)
	}
	if reads != 1 {
		t.Fatalf("ReadText calls = %d, want one admitted source read reused for location mapping", reads)
	}
}

func TestDiagnosticsNormalizeAliasesBeforeReadingEditableFile(t *testing.T) {
	a := newTestApp(t, map[string]string{
		"main.tex": rootDoc,
	})
	reads := make(map[string]int)
	a.diagnosticReadText = func(relative string) (string, error) {
		reads[relative]++
		return a.proj.ReadText(relative)
	}
	rawLog := strings.Join([]string{
		"error: main.tex:1: first alias",
		"error: ./main.tex:2: second alias",
		"error: chapters/../main.tex:3: third alias",
		"error: main:1: first extensionless alias",
		"error: ./main:2: second extensionless alias",
		"error: chapters/../main:3: third extensionless alias",
	}, "\n")

	diagnostics := a.diagnosticsLocked(rawLog)

	if len(diagnostics) != 6 {
		t.Fatalf("diagnostics = %+v, want all six aliases", diagnostics)
	}
	for _, diagnostic := range diagnostics {
		if diagnostic.File != "main.tex" {
			t.Errorf("diagnostic file = %q, want main.tex", diagnostic.File)
		}
	}
	if reads["main.tex"] != 1 {
		t.Errorf("main.tex ReadText calls = %d, want one normalized validation read", reads["main.tex"])
	}
	if reads["main"] != 0 {
		t.Errorf("main ReadText calls = %d, want metadata rejection before content read", reads["main"])
	}
}

func TestDiagnosticsBoundLargeUniqueFileAdmissionAndDoNotCacheExactSourceContent(t *testing.T) {
	const reportedFiles = 12
	files := map[string]string{"main.tex": rootDoc}
	for index := 0; index < reportedFiles; index++ {
		files[fmt.Sprintf("large-%03d.tex", index)] = ""
	}
	a := newTestApp(t, files)
	large := strings.Repeat("x", 8*1024*1024)
	validatedReads := 0
	var validatedBytes int64
	a.diagnosticTextSize = func(relative string) (int64, error) {
		if relative == "main.tex" {
			return int64(len(rootDoc)), nil
		}
		return 8 * 1024 * 1024, nil
	}
	a.diagnosticReadText = func(relative string) (string, error) {
		validatedReads++
		if relative == "main.tex" {
			validatedBytes += int64(len(rootDoc))
			return rootDoc, nil
		}
		validatedBytes += int64(len(large))
		return large, nil
	}
	var rawLog strings.Builder
	for index := 0; index < reportedFiles; index++ {
		fmt.Fprintf(&rawLog, "error: large-%03d.tex:1: exact diagnostic %d\n", index, index)
	}

	diagnostics := a.diagnosticsLocked(rawLog.String())

	if len(diagnostics) != reportedFiles {
		t.Fatalf("diagnostics = %d, want %d", len(diagnostics), reportedFiles)
	}
	if validatedBytes > maximumDiagnosticAdmissionBytes || validatedReads > 4 {
		t.Fatalf("large unique admission read %d bytes across %d files", validatedBytes, validatedReads)
	}
	for _, diagnostic := range diagnostics {
		if diagnostic.Line != 1 {
			t.Fatalf("exact diagnostic lost reported line: %+v", diagnostic)
		}
	}
}

func TestDiagnosticsChargeActualValidationBytesAfterStaleMetadata(t *testing.T) {
	files := map[string]string{"main.tex": rootDoc}
	for index := 0; index < 12; index++ {
		files[fmt.Sprintf("stale-%03d.tex", index)] = ""
	}
	a := newTestApp(t, files)
	large := strings.Repeat("x", 8*1024*1024)
	reads := make(map[string]int)
	var validationBytes int64
	a.diagnosticTextSize = func(string) (int64, error) { return 1, nil }
	a.diagnosticReadText = func(relative string) (string, error) {
		reads[relative]++
		if relative == "main.tex" {
			if reads[relative] == 1 {
				validationBytes += int64(len(rootDoc))
			}
			return rootDoc, nil
		}
		if reads[relative] == 1 {
			validationBytes += int64(len(large))
		}
		return large, nil
	}
	var rawLog strings.Builder
	for index := 0; index < 12; index++ {
		fmt.Fprintf(&rawLog, "error: stale-%03d.tex:0: File ended while scanning use of \\target\n", index)
	}

	_ = a.diagnosticsLocked(rawLog.String())

	if validationBytes > maximumDiagnosticAdmissionBytes+int64(len(large)) {
		t.Fatalf("stale metadata caused %d bytes of validation I/O", validationBytes)
	}
}

func TestDiagnosticsChargeRejectedBinaryValidationBytes(t *testing.T) {
	a := newTestApp(t, map[string]string{"main.tex": rootDoc})
	const binaryBytes = 8 * 1024 * 1024
	binary := strings.Repeat("\x00", binaryBytes)
	for index := 0; index < 8; index++ {
		relative := fmt.Sprintf("binary-%03d.tex", index)
		if err := os.WriteFile(filepath.Join(a.proj.Root(), relative), []byte(binary), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	reads := make(map[string]int)
	a.diagnosticReadText = func(relative string) (string, error) {
		reads[relative]++
		return a.proj.ReadText(relative)
	}
	var rawLog strings.Builder
	for index := 0; index < 8; index++ {
		fmt.Fprintf(&rawLog, "error: binary-%03d.tex:0: File ended while scanning use of \\target\n", index)
	}

	_ = a.diagnosticsLocked(rawLog.String())

	binaryReads := 0
	for relative, count := range reads {
		if strings.HasPrefix(relative, "binary-") {
			binaryReads += count
		}
	}
	if binaryReads > maximumDiagnosticAdmissionBytes/binaryBytes {
		t.Fatalf("binary validation reads = %d, want at most %d", binaryReads, maximumDiagnosticAdmissionBytes/binaryBytes)
	}
}

func TestDiagnosticsDoNotAttributeExtensionlessReportToBinaryFile(t *testing.T) {
	a := newTestApp(t, map[string]string{
		"main.tex":       rootDoc,
		"my chapter.tex": "\x00binary",
	})

	diagnostics := a.diagnosticsLocked("error: my chapter:1: invalid source\n")

	if len(diagnostics) != 1 {
		t.Fatalf("diagnostics = %+v, want one", diagnostics)
	}
	if diagnostics[0].File == "my chapter.tex" {
		t.Fatalf("binary source was published as editable: %+v", diagnostics[0])
	}
}

func TestDiagnosticsConservativelyChargeFailedPostStatGrowthReads(t *testing.T) {
	files := map[string]string{"main.tex": rootDoc}
	for index := 0; index < 8; index++ {
		files[fmt.Sprintf("grown-binary-%03d.tex", index)] = ""
	}
	a := newTestApp(t, files)
	a.diagnosticTextSize = func(string) (int64, error) { return 0, nil }
	reads := 0
	a.diagnosticReadText = func(relative string) (string, error) {
		reads++
		return "", project.ErrNotText
	}
	var rawLog strings.Builder
	for index := 0; index < 8; index++ {
		fmt.Fprintf(&rawLog, "error: grown-binary-%03d.tex:0: File ended while scanning use of \\target\n", index)
	}

	_ = a.diagnosticsLocked(rawLog.String())

	if reads > maximumDiagnosticAdmissionBytes/maximumDiagnosticValidationReadBytes+1 {
		t.Fatalf("failed post-stat validation reads = %d, budget did not stop worst-case reads", reads)
	}
}

func TestDiagnosticsChargeActualCachedBytesAfterSourceGrowth(t *testing.T) {
	files := map[string]string{"main.tex": rootDoc}
	for index := 0; index < 12; index++ {
		files[fmt.Sprintf("grown-%03d.tex", index)] = ""
	}
	a := newTestApp(t, files)
	const largeBytes = 7 * 1024 * 1024
	large := strings.Repeat("x", largeBytes-len("\\target\n")) + "\\target\n"
	reads := make(map[string]int)
	a.diagnosticTextSize = func(string) (int64, error) { return 1, nil }
	a.diagnosticReadText = func(relative string) (string, error) {
		reads[relative]++
		if relative == "main.tex" {
			return rootDoc, nil
		}
		if reads[relative] == 1 {
			return "x", nil
		}
		return large, nil
	}
	var rawLog strings.Builder
	for index := 0; index < 12; index++ {
		fmt.Fprintf(&rawLog, "error: grown-%03d.tex:0: File ended while scanning use of \\target\n", index)
	}

	diagnostics := a.diagnosticsLocked(rawLog.String())

	locationReads := 0
	for relative, count := range reads {
		if relative != "main.tex" && count > 1 {
			locationReads += count - 1
		}
	}
	if locationReads > 5 {
		t.Fatalf("stale source sizes caused %d large location reads", locationReads)
	}
	resolved := 0
	for _, diagnostic := range diagnostics {
		if diagnostic.Approximate {
			resolved++
		}
	}
	if resolved > maximumDiagnosticAdmissionBytes/largeBytes {
		t.Fatalf("cached %d grown sources beyond the actual-byte budget", resolved)
	}
}

// A warning with no filename belongs to the document being typeset; leaving the
// file empty would make it unplaceable in the editor.
func TestUnattributedDiagnosticsFallBackToRootFile(t *testing.T) {
	requireEngine(t)
	a := newTestApp(t, map[string]string{
		"paper.tex": "\\documentclass{article}\n\\begin{document}\n" +
			"See \\ref{nolabel}.\n\\end{document}\n",
	})

	res := a.Compile()
	if res.Error != "" {
		t.Fatalf("Compile: %s", res.Error)
	}
	found := false
	for _, d := range res.Diagnostics {
		if strings.Contains(d.Message, "nolabel") {
			found = true
			if d.File != "paper.tex" {
				t.Errorf("File = %q, want paper.tex", d.File)
			}
		}
	}
	if !found {
		t.Errorf("undefined reference not reported:\n%s", res.Log)
	}
}

func TestSetRootFile(t *testing.T) {
	a := newTestApp(t, map[string]string{
		"main.tex":  rootDoc,
		"other.tex": rootDoc,
	})

	info := a.SetRootFile("other.tex")
	if info.Error != "" {
		t.Fatalf("SetRootFile: %s", info.Error)
	}
	if info.RootFile != "other.tex" {
		t.Errorf("RootFile = %q, want other.tex", info.RootFile)
	}
	if bad := a.SetRootFile("../escape.tex"); bad.Error == "" {
		t.Error("SetRootFile accepted a path outside the project")
	}
}

// Build artifacts must not land in the project: Phase 6 opens real Git working
// copies, and a repo full of untracked PDFs is noise the writer has to ignore.
func TestArtifactsStayOutOfTheProject(t *testing.T) {
	requireEngine(t)
	a := newTestApp(t, map[string]string{"main.tex": rootDoc})

	if res := a.Compile(); !res.Compiled {
		t.Fatalf("setup compile failed: %s\n%s", res.Error, res.Log)
	}

	entries, err := os.ReadDir(a.proj.Root())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		switch strings.ToLower(filepath.Ext(e.Name())) {
		case ".pdf", ".log", ".aux", ".gz", ".xdv":
			t.Errorf("build artifact %q was written into the project", e.Name())
		}
	}
}

// --- opening files ----------------------------------------------------------

func TestOpenFileText(t *testing.T) {
	a := newTestApp(t, map[string]string{
		"main.tex": rootDoc,
		"refs.bib": "@book{key, title={A Title}}\n",
	})

	got := a.OpenFile("refs.bib")
	if got.Error != "" {
		t.Fatalf("OpenFile: %s", got.Error)
	}
	if got.Kind != project.KindBib {
		t.Errorf("Kind = %q, want bib", got.Kind)
	}
	if !strings.Contains(got.Content, "A Title") {
		t.Errorf("Content = %q", got.Content)
	}
}

// An image must come back as a URL, never as bytes in a text buffer — saving
// that back would corrupt the file.
func TestOpenFileImageReturnsURLNotContent(t *testing.T) {
	a := newTestApp(t, map[string]string{
		"main.tex":         rootDoc,
		"figures/plot.png": "\x89PNG\r\n\x1a\n\x00\x00binary",
	})

	got := a.OpenFile("figures/plot.png")
	if got.Error != "" {
		t.Fatalf("OpenFile: %s", got.Error)
	}
	if got.Kind != project.KindImage {
		t.Errorf("Kind = %q, want image", got.Kind)
	}
	if got.Content != "" {
		t.Error("image content was loaded into the text buffer")
	}
	if !strings.HasPrefix(got.URL, projectURLPrefix) {
		t.Errorf("URL = %q, want a %s… URL", got.URL, projectURLPrefix)
	}
}

func TestOpenFileRefusesEscapingPath(t *testing.T) {
	a := newTestApp(t, map[string]string{"main.tex": rootDoc})
	if got := a.OpenFile("../../etc/passwd"); got.Error == "" {
		t.Error("OpenFile read outside the project")
	}
}

// --- tree editing -----------------------------------------------------------

func TestCreateRenameDelete(t *testing.T) {
	a := newTestApp(t, map[string]string{"main.tex": rootDoc})

	if info := a.CreateEntry("", "chapters", true); info.Error != "" {
		t.Fatalf("CreateEntry dir: %s", info.Error)
	}
	if info := a.CreateEntry("chapters", "intro.tex", false); info.Error != "" {
		t.Fatalf("CreateEntry file: %s", info.Error)
	}
	if !a.proj.Exists("chapters/intro.tex") {
		t.Fatal("file was not created")
	}

	if info := a.RenameEntry("chapters/intro.tex", "beginning.tex"); info.Error != "" {
		t.Fatalf("RenameEntry: %s", info.Error)
	}
	// A rename keeps the file in its directory rather than moving it to the root.
	if !a.proj.Exists("chapters/beginning.tex") {
		t.Error("rename moved the file out of its directory")
	}

	if info := a.DeleteEntry("chapters"); info.Error != "" {
		t.Fatalf("DeleteEntry: %s", info.Error)
	}
	if a.proj.Exists("chapters") {
		t.Error("directory was not deleted")
	}
}

// Deleting the root document must leave the app in an explainable state, not
// compiling a file that no longer exists.
func TestDeletingRootFileIsReported(t *testing.T) {
	a := newTestApp(t, map[string]string{"main.tex": rootDoc, "notes.txt": "hi"})

	info := a.DeleteEntry("main.tex")
	if info.Error != "" {
		t.Fatalf("DeleteEntry: %s", info.Error)
	}
	if info.RootFile != "" {
		t.Errorf("RootFile = %q, want empty", info.RootFile)
	}
	if res := a.Compile(); res.Error == "" {
		t.Error("compiling with no root file should explain itself")
	}
}

// --- SyncTeX ----------------------------------------------------------------

// The interesting case is cross-file: a line in an \input'ed chapter must map
// into the PDF, and a click there must come back to that chapter — not the root.
func TestSyncTeXRoundTripAcrossFiles(t *testing.T) {
	requireEngine(t)
	a := newTestApp(t, map[string]string{
		"main.tex": "\\documentclass{article}\n\\begin{document}\n" +
			"\\input{chapters/intro}\n\\input{chapters/method}\n\\end{document}\n",
		"chapters/intro.tex":  "\\section{Introduction}\nThe first chapter says this.\n",
		"chapters/method.tex": "\\section{Method}\nThe second chapter says something else.\n",
	})

	if res := a.Compile(); !res.Compiled {
		t.Fatalf("setup compile failed: %s\n%s", res.Error, res.Log)
	} else if !res.HasSyncTeX {
		t.Fatal("no SyncTeX data was produced")
	}

	// Forward: the sentence in the second chapter.
	rects := a.ForwardSearch("chapters/method.tex", 2)
	if len(rects) == 0 {
		t.Fatal("forward search from chapters/method.tex found nothing")
	}
	r := rects[0]
	if r.Page != 1 {
		t.Errorf("page = %d, want 1", r.Page)
	}
	if r.SourceFilePath != "chapters/method.tex" {
		t.Errorf("SourceFilePath = %q", r.SourceFilePath)
	}
	if r.Width <= 0 || r.Height <= 0 {
		t.Errorf("degenerate rect: %+v", r)
	}

	// Inverse: clicking there comes back to the same chapter.
	loc := a.InverseSearch(r.Page, r.X+r.Width/2, r.Y+r.Height/2)
	if loc.Error != "" {
		t.Fatalf("InverseSearch: %s", loc.Error)
	}
	if loc.File != "chapters/method.tex" {
		t.Errorf("File = %q, want chapters/method.tex — a click in the second chapter "+
			"must not report the root document", loc.File)
	}
	if loc.Line < 1 || loc.Line > 3 {
		t.Errorf("Line = %d, want the sentence's line (2) or its neighbour", loc.Line)
	}
}

// The two chapters must map to different places, or "sync" is doing nothing.
func TestSyncTeXDistinguishesTheTwoChapters(t *testing.T) {
	requireEngine(t)
	a := newTestApp(t, map[string]string{
		"main.tex": "\\documentclass{article}\n\\begin{document}\n" +
			"\\input{chapters/intro}\n\\input{chapters/method}\n\\end{document}\n",
		"chapters/intro.tex":  "\\section{Introduction}\nThe first chapter says this.\n",
		"chapters/method.tex": "\\section{Method}\nThe second chapter says something else.\n",
	})
	if res := a.Compile(); !res.Compiled {
		t.Fatalf("setup compile failed: %s", res.Error)
	}

	intro := a.ForwardSearch("chapters/intro.tex", 2)
	method := a.ForwardSearch("chapters/method.tex", 2)
	if len(intro) == 0 || len(method) == 0 {
		t.Fatalf("missing rects: intro=%v method=%v", intro, method)
	}
	if !(intro[0].Y < method[0].Y) {
		t.Errorf("intro at y=%.1f is not above method at y=%.1f", intro[0].Y, method[0].Y)
	}
}

// A file with no output must fail quietly rather than inventing a position.
func TestForwardSearchOnALineWithNoOutput(t *testing.T) {
	requireEngine(t)
	a := newTestApp(t, map[string]string{"main.tex": rootDoc})
	if res := a.Compile(); !res.Compiled {
		t.Fatalf("setup compile failed: %s", res.Error)
	}
	// Line 1 is \documentclass, which produces nothing at all.
	if rects := a.ForwardSearch("main.tex", 1); len(rects) > 0 {
		// Falling forward to the first line with output is fine; inventing a
		// position on a page that does not exist is not.
		for _, r := range rects {
			if r.Page < 1 {
				t.Errorf("invented a position: %+v", r)
			}
		}
	}
}

// Searching before anything has been compiled must explain itself.
func TestSyncTeXSearchBeforeCompiling(t *testing.T) {
	a := newTestApp(t, map[string]string{"main.tex": rootDoc})

	if rects := a.ForwardSearch("main.tex", 3); len(rects) != 0 {
		t.Errorf("forward search returned %v with nothing compiled", rects)
	}
	loc := a.InverseSearch(1, 100, 100)
	if loc.Error == "" {
		t.Error("inverse search should say there is no SyncTeX data yet")
	}
}

// Output from a class or package is real, but there is no project file to open.
func TestInverseSearchOutsideTheProject(t *testing.T) {
	requireEngine(t)
	a := newTestApp(t, map[string]string{"main.tex": rootDoc})
	if res := a.Compile(); !res.Compiled {
		t.Fatalf("setup compile failed: %s", res.Error)
	}
	// A page that does not exist has nothing to resolve.
	if loc := a.InverseSearch(99, 100, 100); loc.Error == "" {
		t.Errorf("expected an error for a nonexistent page, got %+v", loc)
	}
}

// --- refusing to destroy work ------------------------------------------------

// Writing the editor's buffer to a path it did not come from would destroy an
// unrelated file. If the two sides have lost track of each other, refusing is
// the only safe answer.
func TestSaveRefusesAPathTheEditorDoesNotHold(t *testing.T) {
	a := newTestApp(t, map[string]string{
		"main.tex":           rootDoc,
		"chapters/intro.tex": "the introduction\n",
	})

	if got := a.OpenFile("chapters/intro.tex"); got.Error != "" {
		t.Fatalf("OpenFile: %s", got.Error)
	}

	// The editor holds intro.tex; a save aimed at main.tex must not land.
	res := a.SaveAndCompile("main.tex", "chapter text that does not belong here\n")
	if res.Error == "" {
		t.Fatal("a cross-file save was allowed")
	}
	if !strings.Contains(res.Error, "chapters/intro.tex") {
		t.Errorf("error should name the file actually open, got %q", res.Error)
	}

	onDisk, err := os.ReadFile(filepath.Join(a.proj.Root(), "main.tex"))
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != rootDoc {
		t.Errorf("main.tex was modified:\n%s", onDisk)
	}
}

// An edit made outside the app — another editor, or a branch switch once Phase 7
// lands — must not be silently discarded by a stale buffer.
func TestSaveRefusesWhenTheFileChangedOnDisk(t *testing.T) {
	a := newTestApp(t, map[string]string{"main.tex": rootDoc})

	if got := a.OpenFile("main.tex"); got.Error != "" {
		t.Fatalf("OpenFile: %s", got.Error)
	}

	// Something else rewrites the file after we handed it to the editor.
	external := "\\documentclass{book}\n\\begin{document}\nedited elsewhere\n\\end{document}\n"
	abs := filepath.Join(a.proj.Root(), "main.tex")
	if err := os.WriteFile(abs, []byte(external), 0o644); err != nil {
		t.Fatal(err)
	}
	// Make the change unambiguous even on a coarse filesystem clock.
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(abs, future, future); err != nil {
		t.Fatal(err)
	}

	res := a.SaveAndCompile("main.tex", rootDoc)
	if res.Error == "" {
		t.Fatal("a stale buffer overwrote an external edit")
	}
	if !strings.Contains(res.Error, "changed on disk") {
		t.Errorf("error should explain what happened, got %q", res.Error)
	}

	onDisk, err := os.ReadFile(abs)
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != external {
		t.Error("the external edit was lost")
	}

	// Reopening adopts the new content and saving works again.
	reopened := a.OpenFile("main.tex")
	if reopened.Content != external {
		t.Errorf("reopen did not pick up the new content: %q", reopened.Content)
	}
	requireEngine(t)
	if res := a.SaveAndCompile("main.tex", external+"% appended\n"); res.Error != "" {
		t.Errorf("save after reopening failed: %s", res.Error)
	}
}

// Ordinary repeated saves must not trip the guard — the app's own writes are
// what moves the file's timestamp most of the time.
func TestRepeatedSavesSucceed(t *testing.T) {
	requireEngine(t)
	a := newTestApp(t, map[string]string{"main.tex": rootDoc})
	if got := a.OpenFile("main.tex"); got.Error != "" {
		t.Fatalf("OpenFile: %s", got.Error)
	}
	for i := range 3 {
		body := fmt.Sprintf("\\documentclass{article}\n\\begin{document}\nrevision %d\n\\end{document}\n", i)
		if res := a.SaveAndCompile("main.tex", body); res.Error != "" {
			t.Fatalf("save %d failed: %s", i, res.Error)
		}
	}
}

// A freshly created file is immediately editable, so its baseline has to exist.
func TestNewlyCreatedFileCanBeSaved(t *testing.T) {
	requireEngine(t)
	a := newTestApp(t, map[string]string{"main.tex": rootDoc})

	if info := a.CreateEntry("", "notes.tex", false); info.Error != "" {
		t.Fatalf("CreateEntry: %s", info.Error)
	}
	if res := a.SaveAndCompile("notes.tex", "Some notes.\n"); res.Error != "" {
		t.Fatalf("saving a new file failed: %s", res.Error)
	}
	got, err := os.ReadFile(filepath.Join(a.proj.Root(), "notes.tex"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "Some notes.\n" {
		t.Errorf("content = %q", got)
	}
}

// --- settings ----------------------------------------------------------------

// Reopening where you left off is the point of persisting anything, and the file
// that gets written must be the one the app was told to use — not a global path.
func TestPersistsProjectAndOpenFile(t *testing.T) {
	a := newTestApp(t, map[string]string{
		"main.tex":           rootDoc,
		"chapters/intro.tex": "intro\n",
	})

	if got := a.OpenFile("chapters/intro.tex"); got.Error != "" {
		t.Fatalf("OpenFile: %s", got.Error)
	}

	s := loadSettings(a.settingsFile)
	if s.LastProject != a.proj.Root() {
		t.Errorf("LastProject = %q, want %q", s.LastProject, a.proj.Root())
	}
	if s.RootFile != "main.tex" {
		t.Errorf("RootFile = %q, want main.tex", s.RootFile)
	}
	if s.OpenFile != "chapters/intro.tex" {
		t.Errorf("OpenFile = %q, want chapters/intro.tex", s.OpenFile)
	}
}

// Restoring must bring back the file that was being edited, not jump back to the
// root document every launch.
func TestOpenLockedRestoresOpenFile(t *testing.T) {
	a := newTestApp(t, map[string]string{
		"main.tex":           rootDoc,
		"chapters/intro.tex": "intro\n",
	})
	root := a.proj.Root()

	fresh := &App{compiler: tex.NewCompiler(), settingsFile: a.settingsFile}
	if err := fresh.openLocked(root, "main.tex", "chapters/intro.tex"); err != nil {
		t.Fatalf("openLocked: %v", err)
	}
	if fresh.openFile != "chapters/intro.tex" {
		t.Errorf("openFile = %q, want chapters/intro.tex", fresh.openFile)
	}

	// A file that has since been deleted must not leave the editor pointing at
	// something that is not there.
	stale := &App{compiler: tex.NewCompiler(), settingsFile: a.settingsFile}
	if err := stale.openLocked(root, "main.tex", "chapters/gone.tex"); err != nil {
		t.Fatalf("openLocked: %v", err)
	}
	if stale.openFile != "main.tex" {
		t.Errorf("openFile = %q, want the root document as a fallback", stale.openFile)
	}
}

// --- serving ----------------------------------------------------------------

// The handler being correct is not enough: it also has to be *reached*. Wiring
// it as the asset server's Handler (a fallback) meant that under `wails dev`,
// where the chain proxies to Vite and Vite answers unknown paths with
// index.html and a 200, the preview received HTML and PDF.js reported "Invalid
// PDF structure".
func TestAssetMiddlewareClaimsOurPathsBeforeAssets(t *testing.T) {
	requireEngine(t)
	a := newTestApp(t, map[string]string{
		"main.tex":         rootDoc,
		"figures/plot.png": "\x89PNG\r\n\x1a\nfake",
	})

	res := a.Compile()
	if !res.Compiled {
		t.Fatalf("setup compile failed: %s\n%s", res.Error, res.Log)
	}

	// Stands in for the asset chain, which in dev serves index.html for anything
	// it does not recognise — including, previously, our PDF.
	assets := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<!DOCTYPE html><html><body>SPA fallback</body></html>"))
	})
	server := a.assetMiddleware()(assets)

	get := func(url string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
		return rec
	}

	pdf := get(res.PDFURL)
	if ct := pdf.Header().Get("Content-Type"); ct != "application/pdf" {
		t.Errorf("PDF Content-Type = %q (asset fallback won the route)", ct)
	}
	if !strings.HasPrefix(pdf.Body.String(), "%PDF-") {
		t.Errorf("served HTML instead of the PDF: %.60q", pdf.Body.String())
	}

	img := get(projectFileURL("figures/plot.png", 1))
	if ct := img.Header().Get("Content-Type"); !strings.HasPrefix(ct, "image/png") {
		t.Errorf("image Content-Type = %q, want image/png", ct)
	}
	if !strings.HasPrefix(img.Body.String(), "\x89PNG") {
		t.Errorf("image body = %.20q", img.Body.String())
	}

	// Everything else must still reach the frontend.
	if body := get("/index.html").Body.String(); !strings.Contains(body, "SPA fallback") {
		t.Error("middleware swallowed a request meant for the frontend")
	}
}

// A crafted URL must not be able to read outside the project.
func TestProjectFileHandlerRefusesEscapingPaths(t *testing.T) {
	a := newTestApp(t, map[string]string{"main.tex": rootDoc})
	handler := a.projectFileHandler()

	for _, url := range []string{
		projectURLPrefix + "../../etc/passwd",
		projectURLPrefix + "..%2f..%2fetc%2fpasswd",
		projectURLPrefix,
	} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: got %d, want 404", url, rec.Code)
		}
	}
}

func TestPDFHandlerBeforeAnyCompile(t *testing.T) {
	a := newTestApp(t, map[string]string{"main.tex": rootDoc})
	rec := httptest.NewRecorder()
	a.pdfHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, pdfURLPath, nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("got %d, want 404 before anything is compiled", rec.Code)
	}
}

// Saving again must advertise a different URL, or the webview serves the previous
// render from cache and the preview looks frozen.
func TestCompileBumpsRevision(t *testing.T) {
	requireEngine(t)
	a := newTestApp(t, map[string]string{"main.tex": rootDoc})

	first := a.SaveAndCompile("main.tex", rootDoc)
	second := a.SaveAndCompile("main.tex", strings.Replace(rootDoc, "hello", "goodbye", 1))
	if first.PDFURL == "" || second.PDFURL == "" {
		t.Fatalf("missing PDF URLs: %q, %q", first.PDFURL, second.PDFURL)
	}
	if first.PDFURL == second.PDFURL {
		t.Errorf("URL did not change between compiles: %s", first.PDFURL)
	}
}

// A document that does not compile is a normal state, not an app failure.
func TestBrokenDocumentReportsErrorsNotFailure(t *testing.T) {
	requireEngine(t)
	a := newTestApp(t, map[string]string{
		"main.tex": "\\documentclass{article}\n\\begin{document}\n\\nopeNotACommand\n\\end{document}\n",
	})

	res := a.Compile()
	if res.Error != "" {
		t.Fatalf("expected a verdict, got a hard error: %s", res.Error)
	}
	if res.Compiled {
		t.Fatal("expected the document to fail")
	}
	errs := errorDiagnostics(res.Diagnostics)
	if len(errs) == 0 {
		t.Fatalf("no diagnostics parsed:\n%s", res.Log)
	}
	if errs[0].Hint == "" {
		t.Error("no plain-language hint")
	}
	if res.Log == "" {
		t.Error("raw log was dropped")
	}
}

// A failed compile leaves the previous PDF on disk. Serving it beats a blank
// pane, but it must be flagged or the UI implies the preview matches the buffer.
func TestFailedCompileFlagsStalePDF(t *testing.T) {
	requireEngine(t)
	a := newTestApp(t, map[string]string{"main.tex": rootDoc})

	if good := a.Compile(); !good.Compiled || good.PDFStale {
		t.Fatalf("setup: compiled=%v stale=%v", good.Compiled, good.PDFStale)
	}
	bad := a.SaveAndCompile("main.tex",
		"\\documentclass{article}\n\\begin{document}\n\\nopeNotACommand\n\\end{document}\n")
	if bad.Compiled {
		t.Fatal("expected failure")
	}
	if bad.PDFURL == "" {
		t.Fatal("expected the previous PDF to still be offered")
	}
	if !bad.PDFStale {
		t.Error("stale PDF was not flagged")
	}
}

func TestEngineVersion(t *testing.T) {
	requireEngine(t)
	a := newTestApp(t, map[string]string{"main.tex": rootDoc})
	if v := a.EngineVersion(); !strings.Contains(strings.ToLower(v), "tectonic") {
		t.Errorf("EngineVersion = %q, want the engine name", v)
	}
}
