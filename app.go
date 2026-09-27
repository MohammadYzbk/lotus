package main

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/MohammadYzbk/lotus/internal/project"
	"github.com/MohammadYzbk/lotus/internal/synctex"
	"github.com/MohammadYzbk/lotus/internal/tex"
	"github.com/MohammadYzbk/lotus/internal/texlog"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// starterDoc is what a brand-new scratch project contains.
const starterDoc = `\documentclass{article}

\title{A new document}
\author{}

\begin{document}
\maketitle

Type here. It recompiles when you pause; Cmd-S compiles immediately.

\end{document}
`

const (
	maximumDiagnosticFileEntries     = 128
	maximumDiagnosticManifestEntries = 20 * 1024
	maximumDiagnosticAdmissionBytes  = 32 * 1024 * 1024
	// ReadText may consume its 8 MiB limit plus the sentinel byte before it
	// rejects a file that grew after metadata admission.
	maximumDiagnosticValidationReadBytes = 8*1024*1024 + 1
	maximumDiagnosticManifestScanEntries = 64 * 1024
	maximumDiagnosticFallbackScanEntries = 4 * maximumDiagnosticFileEntries
)

type boundedDiagnosticNameSet struct {
	values map[string]struct{}
	order  []string
	next   int
	limit  int
}

type diagnosticFileManifest struct {
	root                     string
	exact                    map[string]struct{}
	truncated                bool
	remainingFallbackProbes  int
	remainingFallbackEntries int
	canonicalCandidate       func(string, string, *int) (string, bool)
}

func newDiagnosticFileManifest(root string) *diagnosticFileManifest {
	return &diagnosticFileManifest{
		root:                     root,
		exact:                    make(map[string]struct{}),
		remainingFallbackProbes:  maximumDiagnosticFileEntries,
		remainingFallbackEntries: maximumDiagnosticFallbackScanEntries,
		canonicalCandidate:       canonicalDiagnosticManifestCandidate,
	}
}

func (manifest *diagnosticFileManifest) add(relative string) {
	manifest.exact[relative] = struct{}{}
}

func (manifest *diagnosticFileManifest) resolve(candidate string) (string, bool) {
	if _, exact := manifest.exact[candidate]; exact {
		return candidate, true
	}
	return manifest.admitFallback(candidate)
}

func (manifest *diagnosticFileManifest) admitFallback(candidate string) (string, bool) {
	if manifest.remainingFallbackProbes <= 0 {
		return "", false
	}
	manifest.remainingFallbackProbes--
	canonical, admitted := manifest.canonicalCandidate(
		manifest.root,
		candidate,
		&manifest.remainingFallbackEntries,
	)
	if !admitted {
		return "", false
	}
	manifest.add(canonical)
	return canonical, true
}

func diagnosticManifestAliasKey(relative string) string {
	normalized := norm.NFC.String(relative)
	return norm.NFC.String(cases.Fold().String(normalized))
}

func (manifest *diagnosticFileManifest) beginUse() {
	manifest.remainingFallbackProbes = maximumDiagnosticFileEntries
	manifest.remainingFallbackEntries = maximumDiagnosticFallbackScanEntries
}

func canonicalDiagnosticManifestCandidate(rootPath, candidate string, remainingEntries *int) (string, bool) {
	if candidate == "" || filepath.IsAbs(candidate) || strings.Contains(candidate, "\\") || strings.ContainsRune(candidate, 0) {
		return "", false
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(candidate)))
	if clean != candidate || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", false
	}
	// Project.Open permits the selected root itself to be a directory symlink.
	// Stat authenticates its current target; os.Root constrains followed child
	// symlinks to that descriptor-rooted project tree.
	rootInfo, err := os.Stat(rootPath)
	if err != nil || !rootInfo.IsDir() {
		return "", false
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return "", false
	}
	defer root.Close()
	openedRootInfo, err := root.Stat(".")
	if err != nil || !os.SameFile(rootInfo, openedRootInfo) {
		return "", false
	}
	components := strings.Split(candidate, "/")
	parent := "."
	parentInfo := rootInfo
	canonical := make([]string, 0, len(components))
	for componentIndex, component := range components {
		reportedChild := filepath.Join(parent, filepath.FromSlash(component))
		targetInfo, err := root.Stat(reportedChild)
		if err != nil {
			return "", false
		}
		directory, err := openDiagnosticManifestDirectory(root, parent)
		if err != nil {
			return "", false
		}
		openedParentInfo, err := directory.Stat()
		if err != nil || !openedParentInfo.IsDir() || !os.SameFile(parentInfo, openedParentInfo) {
			_ = directory.Close()
			return "", false
		}
		exact := ""
		foldedMatch := ""
		ambiguousFolded := false
		identityMatch := ""
		ambiguousIdentity := false
		for {
			entries, readErr := directory.ReadDir(128)
			for _, entry := range entries {
				if *remainingEntries <= 0 {
					_ = directory.Close()
					return "", false
				}
				*remainingEntries--
				entryPath := filepath.Join(parent, entry.Name())
				entryInfo, err := root.Stat(entryPath)
				if err != nil || !os.SameFile(targetInfo, entryInfo) {
					continue
				}
				if entry.Name() == component {
					exact = entry.Name()
					break
				}
				if diagnosticManifestAliasKey(entry.Name()) == diagnosticManifestAliasKey(component) {
					if foldedMatch != "" && foldedMatch != entry.Name() {
						ambiguousFolded = true
					}
					foldedMatch = entry.Name()
				}
				if identityMatch != "" && identityMatch != entry.Name() {
					ambiguousIdentity = true
				}
				identityMatch = entry.Name()
			}
			if exact != "" || readErr == io.EOF {
				break
			}
			if readErr != nil {
				_ = directory.Close()
				return "", false
			}
		}
		if err := directory.Close(); err != nil {
			return "", false
		}
		actual := exact
		if actual == "" && !ambiguousFolded {
			actual = foldedMatch
		}
		if actual == "" && !ambiguousIdentity {
			actual = identityMatch
		}
		if actual == "" {
			return "", false
		}
		canonical = append(canonical, actual)
		if componentIndex == len(components)-1 {
			canonicalRelative := filepath.ToSlash(filepath.Join(canonical...))
			return canonicalRelative, targetInfo.Mode().IsRegular() && isPotentialDiagnosticSource(canonicalRelative)
		}
		if !targetInfo.IsDir() {
			return "", false
		}
		parent = filepath.Join(append([]string{"."}, canonical...)...)
		parentInfo = targetInfo
	}
	return "", false
}

func newBoundedDiagnosticNameSet(limit int) *boundedDiagnosticNameSet {
	return &boundedDiagnosticNameSet{
		values: make(map[string]struct{}, limit),
		order:  make([]string, 0, limit),
		limit:  limit,
	}
}

func (set *boundedDiagnosticNameSet) contains(value string) bool {
	_, ok := set.values[value]
	return ok
}

func (set *boundedDiagnosticNameSet) add(value string) {
	if set.limit <= 0 {
		return
	}
	if _, exists := set.values[value]; exists {
		return
	}
	if len(set.order) < set.limit {
		set.order = append(set.order, value)
		set.values[value] = struct{}{}
		return
	}
	delete(set.values, set.order[set.next])
	set.order[set.next] = value
	set.next = (set.next + 1) % set.limit
	set.values[value] = struct{}{}
}

// App is the Wails-bound application object.
type App struct {
	ctx      context.Context
	compiler *tex.Compiler

	// settingsFile is where preferences are persisted. Injectable because
	// several bound methods persist as a side effect, and a test that quietly
	// rewrote the real settings.json would reset the writer's open project —
	// which is exactly what happened before this was a field.
	settingsFile string

	// mu guards everything below. Saves can overlap (the idle compile fires
	// while a previous one runs), tree operations mutate the project, and the
	// asset middleware reads paths from the webview's own goroutine.
	mu       sync.Mutex
	proj     *project.Project
	outDir   string
	pdfPath  string
	revision int
	openFile string

	// syncPath is the .synctex.gz from the last compile; sync is its parsed form,
	// built on first use. Parsing is deferred because a long document's SyncTeX
	// data runs to megabytes and most compiles are never followed by a search.
	syncPath string
	sync     *synctex.File

	// opened records which file was handed to the editor and what it looked like
	// on disk at the time. Saves are checked against it, so a desynchronised
	// frontend cannot write one file's contents into another, and an edit made
	// outside the app cannot be silently overwritten by a stale buffer.
	opened openState

	// github is the GitHub half of the app. It carries its own lock: a slow
	// network call must not hold up a compile.
	github *githubState

	// diagnosticReadText is a test seam for verifying that source-backed
	// diagnostic resolution reuses already admitted editor text.
	diagnosticReadText      func(relative string) (string, error)
	diagnosticTextSize      func(relative string) (int64, error)
	diagnosticManifest      *diagnosticFileManifest
	diagnosticManifestEntry func()
}

// openState is the identity of the file currently in the editor.
type openState struct {
	path    string
	modTime time.Time
	size    int64
}

func NewApp() *App {
	return &App{compiler: tex.NewCompiler(), github: newGitHubState()}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx

	a.mu.Lock()
	defer a.mu.Unlock()

	if a.settingsFile == "" {
		if dir, err := appDir(); err == nil {
			a.settingsFile = filepath.Join(dir, settingsFileName)
		}
	}

	s := loadSettings(a.settingsFile)
	// Reopen where we left off. If that directory is gone — moved, unmounted,
	// on a disconnected drive — fall back to the scratch project rather than
	// starting with no project at all.
	if s.LastProject != "" {
		if err := a.openLocked(s.LastProject, s.RootFile, s.OpenFile); err == nil {
			return
		}
	}
	if dir, err := scratchProjectDir(); err == nil {
		_ = a.openLocked(dir, "", "")
	}
}

// context falls back to a background context so the app is usable in tests,
// which never run startup.
func (a *App) context() context.Context {
	if a.ctx == nil {
		return context.Background()
	}
	return a.ctx
}

// scratchProjectDir is the default project for a first run, created on demand.
//
// It lives in the config directory rather than the cache: macOS may purge caches
// at will, and silently losing someone's document would be inexcusable. Phase 9
// replaces this with a proper first-run experience.
func scratchProjectDir() (string, error) {
	base, err := appDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "scratch")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create scratch project: %w", err)
	}
	main := filepath.Join(dir, "main.tex")
	if _, err := os.Stat(main); errors.Is(err, os.ErrNotExist) {
		// Phases 1 and 2 kept the scratch document in the cache directory. Move
		// it rather than silently presenting an empty document to someone who
		// has been writing in it.
		if migrated := migrateLegacyScratch(main); !migrated {
			if err := os.WriteFile(main, []byte(starterDoc), 0o644); err != nil {
				return "", fmt.Errorf("create scratch document: %w", err)
			}
		}
	}
	return dir, nil
}

// migrateLegacyScratch moves the pre-Phase-3 cache-resident scratch document to
// its new home. Reports whether anything was moved.
func migrateLegacyScratch(dest string) bool {
	cache, err := os.UserCacheDir()
	if err != nil {
		return false
	}
	old := filepath.Join(cache, "lotus", "scratch", "main.tex")
	data, err := os.ReadFile(old)
	if err != nil {
		return false
	}
	if err := os.WriteFile(dest, data, 0o644); err != nil {
		return false
	}
	// Leave the original in place: a failed migration that also deleted the
	// only copy would be unforgivable, and a stale file costs nothing.
	return true
}

// buildDir is where compile artifacts for a project go.
//
// Deliberately outside the project: a repo full of untracked .pdf and .synctex.gz
// is noise the writer has to keep ignoring, and Phase 6 opens real Git working
// copies. Keyed by project path so switching projects does not mix outputs.
func buildDir(projectRoot string) (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("locate cache dir: %w", err)
	}
	sum := sha256.Sum256([]byte(projectRoot))
	dir := filepath.Join(base, "lotus", "build", hex.EncodeToString(sum[:8]))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create build dir: %w", err)
	}
	return dir, nil
}

// openLocked opens a project and remembers it. openFile may be empty, in which
// case the root document is shown. Callers must hold mu.
func (a *App) openLocked(dir, rootFile, openFile string) error {
	p, err := project.Open(dir)
	if err != nil {
		return err
	}
	if rootFile != "" && p.Exists(rootFile) {
		// A remembered choice beats re-detecting, which might pick differently.
		if err := p.SetRootFile(rootFile); err != nil {
			return err
		}
	}
	out, err := buildDir(p.Root())
	if err != nil {
		return err
	}

	a.proj = p
	a.invalidateDiagnosticManifestLocked()
	a.outDir = out
	// Artifacts from the previous project must not be shown for this one.
	a.pdfPath = ""
	a.openFile = p.RootFile()
	if openFile != "" && p.Exists(openFile) {
		a.openFile = openFile
	}
	a.recordOpenedLocked(a.openFile)
	a.persistLocked()
	return nil
}

// persistLocked records the open project. Callers must hold mu.
func (a *App) persistLocked() {
	if a.proj == nil || a.settingsFile == "" {
		return
	}
	_ = saveSettings(a.settingsFile, settings{
		LastProject: a.proj.Root(),
		RootFile:    a.proj.RootFile(),
		OpenFile:    a.openFile,
	})
}

// --- project ----------------------------------------------------------------

// ProjectInfo is the whole state the file tree needs to draw itself.
type ProjectInfo struct {
	Root string       `json:"root"`
	Name string       `json:"name"`
	Tree project.Node `json:"tree"`

	// RootFile is the document that gets compiled, empty if none was found.
	// It is not necessarily the file being edited — that is the point.
	RootFile string `json:"rootFile"`
	// OpenFile is the file the editor should show.
	OpenFile string `json:"openFile"`

	Error string `json:"error"`
}

// CurrentProject describes the open project, refreshing the tree from disk.
func (a *App) CurrentProject() ProjectInfo {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.invalidateDiagnosticManifestLocked()
	return a.infoLocked("")
}

// infoLocked builds a ProjectInfo, attaching errMsg if non-empty. Callers must
// hold mu.
func (a *App) infoLocked(errMsg string) ProjectInfo {
	if a.proj == nil {
		return ProjectInfo{Error: firstNonEmpty(errMsg, "no project is open")}
	}
	info := ProjectInfo{
		Root:     a.proj.Root(),
		Name:     filepath.Base(a.proj.Root()),
		RootFile: a.proj.RootFile(),
		OpenFile: a.openFile,
		Error:    errMsg,
	}
	tree, err := a.proj.Tree()
	if err != nil {
		info.Error = firstNonEmpty(errMsg, err.Error())
		return info
	}
	info.Tree = tree
	return info
}

// ProjectSymbols lists the cross-reference targets the project defines, so the
// editor can complete \ref and \cite against the whole project rather than the
// buffer on screen.
//
// It rescans on demand rather than caching. A scan of a normal project costs
// milliseconds, and a stale list that omits the label you wrote a moment ago is
// worse than the scan: completion you cannot trust is completion you stop using.
func (a *App) ProjectSymbols() project.Symbols {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.proj == nil {
		return project.Symbols{Error: "no project is open"}
	}
	symbols, err := a.proj.Symbols()
	if err != nil {
		symbols.Error = err.Error()
	}
	return symbols
}

// OpenProjectDialog asks for a directory and opens it as a project.
func (a *App) OpenProjectDialog() ProjectInfo {
	dir, err := runtime.OpenDirectoryDialog(a.context(), runtime.OpenDialogOptions{
		Title: "Open project folder",
	})
	if err != nil {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.infoLocked(fmt.Sprintf("could not open the folder picker: %v", err))
	}
	if dir == "" {
		// Cancelled; nothing changed.
		return a.CurrentProject()
	}
	return a.OpenProject(dir)
}

// OpenProject opens a directory as the current project.
func (a *App) OpenProject(dir string) ProjectInfo {
	a.mu.Lock()
	defer a.mu.Unlock()

	if err := a.openLocked(dir, "", ""); err != nil {
		return a.infoLocked(err.Error())
	}
	return a.infoLocked("")
}

// SetRootFile chooses which document to compile.
func (a *App) SetRootFile(rel string) ProjectInfo {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.proj == nil {
		return a.infoLocked("")
	}
	if err := a.proj.SetRootFile(rel); err != nil {
		return a.infoLocked(err.Error())
	}
	a.persistLocked()
	return a.infoLocked("")
}

// --- files ------------------------------------------------------------------

// FileContent is one opened file.
type FileContent struct {
	Path string       `json:"path"`
	Kind project.Kind `json:"kind"`

	// Content is the text, for editable files.
	Content string `json:"content"`
	// URL is set for images, which the preview pane displays rather than
	// loading into a text buffer.
	URL string `json:"url"`

	Error string `json:"error"`
}

// OpenFile reads a file for display, deciding by content whether it is text.
func (a *App) OpenFile(rel string) FileContent {
	a.mu.Lock()
	defer a.mu.Unlock()

	out := FileContent{Path: rel}
	if a.proj == nil {
		out.Error = "no project is open"
		return out
	}

	kind, err := a.proj.StatKind(rel)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	out.Kind = kind

	if kind == project.KindImage || kind == project.KindBinary {
		// Not text: hand back a URL so the pane can show the figure, or say
		// plainly that there is nothing to edit.
		if kind == project.KindImage {
			out.URL = projectFileURL(rel, a.revision)
		}
		a.openFile = rel
		// Not a text buffer, so there is nothing the editor may save here.
		a.opened = openState{}
		a.persistLocked()
		return out
	}

	content, err := a.proj.ReadText(rel)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	out.Content = content
	a.openFile = rel
	a.recordOpenedLocked(rel)
	a.persistLocked()
	return out
}

// CreateEntry adds a file or folder under parent ("" for the project root).
func (a *App) CreateEntry(parent, name string, dir bool) ProjectInfo {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.proj == nil {
		return a.infoLocked("")
	}
	rel := name
	if parent != "" {
		rel = parent + "/" + name
	}
	if err := a.proj.Create(rel, dir); err != nil {
		return a.infoLocked(err.Error())
	}
	a.invalidateDiagnosticManifestLocked()
	if !dir {
		a.openFile = rel
		a.recordOpenedLocked(rel)
	}
	a.persistLocked()
	return a.infoLocked("")
}

// RenameEntry renames a file or folder in place, keeping it in its directory.
func (a *App) RenameEntry(rel, newName string) ProjectInfo {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.proj == nil {
		return a.infoLocked("")
	}
	dir := filepath.ToSlash(filepath.Dir(filepath.FromSlash(rel)))
	target := newName
	if dir != "." && dir != "" {
		target = dir + "/" + newName
	}
	if err := a.proj.Rename(rel, target); err != nil {
		return a.infoLocked(err.Error())
	}
	a.invalidateDiagnosticManifestLocked()
	if a.openFile == rel {
		a.openFile = target
		// The buffer now belongs to a different path on disk.
		a.recordOpenedLocked(target)
	}
	a.persistLocked()
	return a.infoLocked("")
}

// ConfirmDelete asks the writer to confirm a deletion, using the platform's own
// dialog. Deleting is the one irreversible thing the file tree can do, and the
// webview's window.confirm is not dependable here.
func (a *App) ConfirmDelete(rel string, isDir bool) bool {
	what := "file"
	if isDir {
		what = "folder and everything in it"
	}
	answer, err := runtime.MessageDialog(a.context(), runtime.MessageDialogOptions{
		Type:          runtime.QuestionDialog,
		Title:         "Delete " + filepath.Base(rel) + "?",
		Message:       fmt.Sprintf("This deletes the %s from disk and cannot be undone.", what),
		Buttons:       []string{"Delete", "Cancel"},
		DefaultButton: "Cancel",
		CancelButton:  "Cancel",
	})
	if err != nil {
		// Without a usable dialog, refuse rather than delete unconfirmed.
		return false
	}
	return answer == "Delete"
}

// DeleteEntry removes a file, or a folder and its contents.
func (a *App) DeleteEntry(rel string) ProjectInfo {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.proj == nil {
		return a.infoLocked("")
	}
	if err := a.proj.Delete(rel); err != nil {
		return a.infoLocked(err.Error())
	}
	a.invalidateDiagnosticManifestLocked()
	if a.openFile == rel {
		a.openFile = a.proj.RootFile()
		// The editor still shows the deleted file's text; refuse to save it
		// anywhere until the frontend opens something.
		a.opened = openState{}
	}
	a.persistLocked()
	return a.infoLocked("")
}

// --- compiling ---------------------------------------------------------------

// CompileResult is one save-and-compile round trip, shaped for the status bar.
type CompileResult struct {
	// Error is set when we could not get a verdict at all — no project, no root
	// file, the engine is missing, the run timed out. Distinct from a document
	// that simply does not compile.
	Error string `json:"error"`

	// Compiled reports the engine's verdict on the document.
	Compiled   bool  `json:"compiled"`
	DurationMS int64 `json:"durationMs"`

	// RootFile is the document that was compiled, which may not be the file
	// being edited.
	RootFile string `json:"rootFile"`

	// PDFURL is served by the asset middleware, with a revision query so the
	// webview cannot hand back a stale render. Empty when no PDF exists yet.
	PDFURL string `json:"pdfUrl"`
	// PDFStale means the PDF at PDFURL predates this compile.
	PDFStale bool `json:"pdfStale"`

	// Diagnostics are the parsed problems, each carrying the project-relative
	// file it belongs to so the editor can show only the ones for the open file.
	Diagnostics []texlog.Diagnostic `json:"diagnostics"`

	// Log is the raw engine output, always kept available: the parser covers the
	// common cases, and anything it does not recognise must still be readable.
	Log string `json:"log"`

	SyncTeXBytes int64 `json:"synctexBytes"`
	// HasSyncTeX tells the frontend whether source/PDF navigation is available
	// at all, so it can say so rather than silently doing nothing.
	HasSyncTeX bool `json:"hasSynctex"`
}

// SaveAndCompile writes a file and compiles the project's root document.
//
// The file being edited is usually not the entry point: editing chapter3.tex has
// to compile the root, or the preview would show a fragment with no preamble.
func (a *App) SaveAndCompile(rel, content string) CompileResult {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.proj == nil {
		return CompileResult{Error: "no project is open"}
	}
	if rel != "" {
		if err := a.writeOpenFileLocked(rel, content); err != nil {
			return CompileResult{Error: err.Error()}
		}
	}
	return a.compileLocked()
}

// writeOpenFileLocked saves the editor's buffer, but only over the file the
// editor was actually given, and only if nothing else has changed it since.
// Callers must hold mu.
//
// Both checks guard against silent, unrecoverable loss of someone's writing:
//
//   - Writing a buffer to a path other than the one it came from would destroy
//     an unrelated file. The editor is handed a file by OpenFile and may save
//     that file; anything else means the two sides have lost track of each other,
//     and the right response is to refuse rather than to guess.
//   - If the file changed on disk after we handed it over — edited in another
//     program, or replaced by a branch switch, which Phase 7 makes routine — the
//     buffer is stale and writing it back would discard that change unseen.
func (a *App) writeOpenFileLocked(rel, content string) error {
	if a.opened.path != "" && rel != a.opened.path {
		return fmt.Errorf(
			"refusing to save: the editor holds %s but asked to write %s. Reopen the file",
			a.opened.path, rel)
	}

	abs, err := a.proj.Abs(rel)
	if err != nil {
		return err
	}
	if info, statErr := os.Stat(abs); statErr == nil && a.opened.path == rel {
		changed := !info.ModTime().Equal(a.opened.modTime) || info.Size() != a.opened.size
		if changed {
			return fmt.Errorf(
				"%s changed on disk since it was opened; reopen it to see the new version "+
					"(nothing was overwritten)", rel)
		}
	}

	if err := a.proj.WriteText(rel, content); err != nil {
		return err
	}
	a.recordOpenedLocked(rel)
	return nil
}

// recordOpenedLocked notes a file's on-disk state as the baseline for future
// saves. Callers must hold mu.
func (a *App) recordOpenedLocked(rel string) {
	a.opened = openState{path: rel}
	abs, err := a.proj.Abs(rel)
	if err != nil {
		return
	}
	if info, err := os.Stat(abs); err == nil {
		a.opened.modTime = info.ModTime()
		a.opened.size = info.Size()
	}
}

// Compile builds the root document without writing anything first.
func (a *App) Compile() CompileResult {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.proj == nil {
		return CompileResult{Error: "no project is open"}
	}
	return a.compileLocked()
}

// compileLocked runs the engine on the root document. Callers must hold mu.
func (a *App) compileLocked() CompileResult {
	// The project may have changed outside Lotus since the last result. Rebuild
	// source membership only after this compile so cached exact spellings cannot
	// survive a case-only rename on a case-insensitive filesystem.
	a.invalidateDiagnosticManifestLocked()
	rootFile := a.proj.RootFile()
	out := CompileResult{RootFile: rootFile, PDFURL: a.currentPDFURL()}

	if rootFile == "" {
		out.Error = "no document to compile: no file in this project contains \\documentclass"
		return out
	}
	rootDocument, err := project.ParseRelativePath(rootFile)
	if err != nil {
		out.Error = "the root document path is not project-relative"
		return out
	}
	mainAbs, err := a.proj.Abs(rootDocument.String())
	if err != nil {
		out.Error = err.Error()
		return out
	}

	res, err := a.compiler.Compile(a.context(), mainAbs, a.outDir)
	if res != nil {
		out.Log = res.RawLog
		out.DurationMS = res.Duration.Milliseconds()
		out.Diagnostics = a.diagnosticsLocked(res.RawLog)
	}
	if err != nil {
		out.Error = err.Error()
		return out
	}

	out.Compiled = res.Success
	if res.SyncTeXPath != "" {
		if fi, statErr := os.Stat(res.SyncTeXPath); statErr == nil {
			out.SyncTeXBytes = fi.Size()
		}
		// Positions from the previous run describe a document that no longer
		// exists, so drop them; the next search reparses.
		if res.SyncTeXPath != a.syncPath || !res.PDFStale {
			a.syncPath = res.SyncTeXPath
			a.sync = nil
		}
	}
	out.HasSyncTeX = a.syncPath != ""
	if res.PDFPath != "" {
		a.pdfPath = res.PDFPath
		a.revision++
		out.PDFURL = a.currentPDFURL()
		out.PDFStale = res.PDFStale
	}
	return out
}

// --- SyncTeX ----------------------------------------------------------------

// SourceLocation is a place in the project's source, as resolved from a point on
// a page.
type SourceLocation struct {
	File string `json:"file"`
	Line int    `json:"line"`
	// Rect is what the click resolved to, so the UI can show the writer which
	// piece of the page it took them from.
	Rect  synctex.Rect `json:"rect"`
	Error string       `json:"error"`
}

// ForwardSearch answers "where did this line end up in the PDF?".
//
// Returns nothing, without an error, when the line produced no output — comments,
// blank preamble lines and macro definitions never appear on a page, and that is
// not a failure worth reporting.
func (a *App) ForwardSearch(file string, line int) []synctex.Rect {
	a.mu.Lock()
	defer a.mu.Unlock()

	sync, err := a.syncLocked()
	if err != nil || a.proj == nil {
		return nil
	}
	source, err := project.ParseRelativePath(file)
	if err != nil {
		return nil
	}
	tag, ok := a.syncTagLocked(sync, source.String())
	if !ok {
		return nil
	}
	rects := sync.Forward(tag, line)
	for i := range rects {
		rects[i].SourceFilePath = source.String()
	}
	return rects
}

// InverseSearch answers "which line produced this spot?", for a point in PDF
// points from the page's top-left corner.
func (a *App) InverseSearch(page int, x, y float64) SourceLocation {
	a.mu.Lock()
	defer a.mu.Unlock()

	sync, err := a.syncLocked()
	if err != nil {
		return SourceLocation{Error: err.Error()}
	}
	hit, ok := sync.Inverse(page, x, y)
	if !ok {
		return SourceLocation{Error: fmt.Sprintf("nothing on page %d to jump to", page)}
	}
	file, ok := a.syncFileForTagLocked(sync, hit.Tag)
	if !ok {
		// Bundle files, a class or a package: real output, but not something in
		// this project that we could open.
		return SourceLocation{Error: "that came from a file outside this project"}
	}
	return SourceLocation{File: file, Line: hit.Line, Rect: hit.Rect}
}

// syncLocked returns the parsed SyncTeX data for the last compile, parsing on
// first use. Callers must hold mu.
func (a *App) syncLocked() (*synctex.File, error) {
	if a.sync != nil {
		return a.sync, nil
	}
	if a.syncPath == "" {
		return nil, errors.New("no SyncTeX data yet; compile first")
	}
	f, err := synctex.ParseFile(a.syncPath)
	if err != nil {
		return nil, err
	}
	a.sync = f
	return f, nil
}

// enginePath turns a path as the engine recorded it into an absolute one.
// Tectonic writes absolute paths; other engines write them relative to the
// directory the compile ran in, which is the root document's directory.
func (a *App) enginePath(recorded string) string {
	if filepath.IsAbs(recorded) {
		return filepath.Clean(recorded)
	}
	return filepath.Clean(filepath.Join(a.proj.Root(), recorded))
}

// syncTagLocked finds the input tag for a project-relative file. Callers must
// hold mu.
func (a *App) syncTagLocked(sync *synctex.File, rel string) (int, bool) {
	abs, err := a.proj.Abs(rel)
	if err != nil {
		return 0, false
	}
	abs = filepath.Clean(abs)

	if tag, ok := sync.TagFor(func(recorded string) bool {
		return a.enginePath(recorded) == abs
	}); ok {
		return tag, true
	}
	// Last resort: match on the file name. An engine that records paths through a
	// symlink or a temporary directory still names the file correctly, and a
	// wrong-but-same-named file is a better answer than refusing to sync at all.
	base := filepath.Base(rel)
	return sync.TagFor(func(recorded string) bool {
		return filepath.Base(recorded) == base
	})
}

// syncFileForTagLocked maps an input tag back to a file in the project. Callers
// must hold mu.
func (a *App) syncFileForTagLocked(sync *synctex.File, tag int) (string, bool) {
	recorded, ok := sync.Inputs[tag]
	if !ok {
		return "", false
	}
	rel, err := a.proj.Rel(a.enginePath(recorded))
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", false
	}
	if !a.proj.Exists(rel) {
		return "", false
	}
	return rel, true
}

// diagnosticsLocked parses the log and attributes every diagnostic to a file in
// the project. Callers must hold mu.
func (a *App) diagnosticsLocked(rawLog string) []texlog.Diagnostic {
	rootFile := a.proj.RootFile()
	knownFiles := a.diagnosticProjectFileManifestLocked()
	if rootFile != "" {
		rootFile = filepath.ToSlash(filepath.Clean(rootFile))
		if canonical, known := knownFiles.resolve(rootFile); known {
			rootFile = canonical
		} else {
			knownFiles.add(rootFile)
		}
	}
	type fileResolution struct {
		relative string
		size     int64
		found    bool
	}
	type admittedFile struct {
		resolution fileResolution
		content    string
		entry      *list.Element
	}
	resolvedFiles := make(map[string]fileResolution)
	admittedFiles := make(map[string]*admittedFile)
	var admittedFileLRU list.List
	negativeResolvedFiles := newBoundedDiagnosticNameSet(maximumDiagnosticFileEntries)
	rejectedAdmittedFiles := newBoundedDiagnosticNameSet(maximumDiagnosticFileEntries)
	var admittedBytes int64
	remainingAdmissionProbes := 2 * maximumDiagnosticFileEntries
	admit := func(relative string) fileResolution {
		relative = filepath.ToSlash(filepath.Clean(relative))
		if cached, ok := admittedFiles[relative]; ok {
			admittedFileLRU.MoveToFront(cached.entry)
			return cached.resolution
		}
		canonical, known := knownFiles.resolve(relative)
		if !known {
			return fileResolution{relative: relative}
		}
		relative = canonical
		if cached, ok := admittedFiles[relative]; ok {
			admittedFileLRU.MoveToFront(cached.entry)
			return cached.resolution
		}
		if rejectedAdmittedFiles.contains(relative) {
			return fileResolution{relative: relative}
		}
		resolution := fileResolution{relative: relative}
		if remainingAdmissionProbes <= 0 {
			rejectedAdmittedFiles.add(relative)
			return resolution
		}
		remainingAdmissionProbes--
		size, err := a.diagnosticTextSizeLocked(relative)
		remaining := maximumDiagnosticAdmissionBytes - admittedBytes
		if err != nil || size < 0 || remaining <= 0 || size > remaining {
			rejectedAdmittedFiles.add(relative)
			return resolution
		}
		// Reserve the metadata size before reading. ReadText must inspect the
		// entire file before it can reject invalid UTF-8 or embedded NUL bytes;
		// charging only successful reads would let a log force unbounded binary
		// validation I/O while App.mu is held.
		admittedBytes += size
		content, err := a.readDiagnosticTextLocked(relative)
		if err != nil {
			failedCharge := int64(maximumDiagnosticValidationReadBytes)
			if failedCharge > size {
				additional := failedCharge - size
				if additional > maximumDiagnosticAdmissionBytes-admittedBytes {
					admittedBytes = maximumDiagnosticAdmissionBytes
				} else {
					admittedBytes += additional
				}
			}
			rejectedAdmittedFiles.add(relative)
			return resolution
		}
		actualSize := int64(len(content))
		growth := actualSize - size
		if growth > maximumDiagnosticAdmissionBytes-admittedBytes {
			admittedBytes = maximumDiagnosticAdmissionBytes
			rejectedAdmittedFiles.add(relative)
			return resolution
		}
		if growth > 0 {
			admittedBytes += growth
		}
		resolution.size = actualSize
		resolution.found = true
		if len(admittedFiles) >= maximumDiagnosticFileEntries {
			oldest := admittedFileLRU.Back()
			if oldest != nil {
				delete(admittedFiles, oldest.Value.(string))
				admittedFileLRU.Remove(oldest)
			}
		}
		entry := admittedFileLRU.PushFront(relative)
		admittedFiles[relative] = &admittedFile{resolution: resolution, content: content, entry: entry}
		return resolution
	}
	resolveReported := func(reported string) fileResolution {
		if cached, ok := resolvedFiles[reported]; ok {
			return cached
		}
		if reported == "" {
			return fileResolution{}
		}
		candidates := diagnosticReportedFileCandidates(reported)
		resolution := fileResolution{relative: candidates[0]}
		if negativeResolvedFiles.contains(reported) {
			return resolution
		}
		for _, candidate := range candidates {
			if admitted := admit(candidate); admitted.found {
				resolution = admitted
				break
			}
		}
		if len(resolvedFiles) < maximumDiagnosticFileEntries {
			if resolution.found {
				resolvedFiles[reported] = resolution
			} else {
				negativeResolvedFiles.add(reported)
			}
		}
		return resolution
	}
	if rootFile != "" {
		_ = resolveReported(rootFile)
	}
	diags := texlog.ParseWithReportedFileLookup(rawLog, func(reported string) bool {
		return resolveReported(reported).found
	})
	for i := range diags {
		// LaTeX's own warnings carry no filename. They come from the document
		// being typeset, so attribute them to the root rather than dropping
		// them or leaving the editor unable to place them.
		if diags[i].File == "" {
			resolution := resolveReported(rootFile)
			if resolution.found {
				diags[i].File = resolution.relative
			} else {
				diags[i].File = rootFile
			}
			continue
		}
		diags[i].File = resolveReported(diags[i].File).relative
	}

	// Resolution needs whichever file a diagnostic belongs to, not just the one
	// that was saved.
	return texlog.ResolveLocations(diags, func(file string) (string, bool) {
		if file == "" {
			return "", false
		}
		resolution := resolveReported(file)
		if !resolution.found {
			return "", false
		}
		resolution = admit(resolution.relative)
		if !resolution.found {
			return "", false
		}
		cached := admittedFiles[resolution.relative]
		if cached == nil {
			return "", false
		}
		return cached.content, true
	})
}

func (a *App) diagnosticProjectFileManifestLocked() *diagnosticFileManifest {
	root := a.proj.Root()
	if a.diagnosticManifest == nil || a.diagnosticManifest.root != root {
		a.diagnosticManifest = diagnosticProjectFileManifestWithHooks(root, nil, a.diagnosticManifestEntry)
	}
	a.diagnosticManifest.beginUse()
	return a.diagnosticManifest
}

func (a *App) invalidateDiagnosticManifestLocked() {
	a.diagnosticManifest = nil
}

func diagnosticProjectFileManifest(root string) *diagnosticFileManifest {
	return diagnosticProjectFileManifestWithHooks(root, nil, nil)
}

func diagnosticProjectFileManifestWithHooks(
	root string,
	beforeDirectoryOpen func(relative string),
	beforeEntry func(),
) *diagnosticFileManifest {
	manifest := newDiagnosticFileManifest(root)
	// The project root may be a caller-selected directory symlink. Its opened
	// target identity is authoritative; walked child symlinks remain excluded.
	rootInfo, err := os.Stat(root)
	if err != nil || !rootInfo.IsDir() {
		return manifest
	}
	openedRoot, err := os.OpenRoot(root)
	if err != nil {
		return manifest
	}
	defer openedRoot.Close()
	sourceEntries := 0
	scannedEntries := 0
	_ = walkDiagnosticProjectManifest(
		openedRoot,
		".",
		rootInfo,
		[]os.FileInfo{rootInfo},
		manifest,
		&sourceEntries,
		&scannedEntries,
		beforeDirectoryOpen,
		beforeEntry,
	)
	return manifest
}

func walkDiagnosticProjectManifest(
	root *os.Root,
	relative string,
	expected os.FileInfo,
	ancestors []os.FileInfo,
	manifest *diagnosticFileManifest,
	sourceEntries *int,
	scannedEntries *int,
	beforeDirectoryOpen func(relative string),
	beforeEntry func(),
) error {
	directory, err := openDiagnosticManifestDirectory(root, relative)
	if err != nil {
		return err
	}
	defer directory.Close()
	openedInfo, err := directory.Stat()
	if err != nil || !openedInfo.IsDir() || !os.SameFile(expected, openedInfo) {
		return fmt.Errorf("diagnostic manifest directory changed while opening")
	}
	for {
		batch, readErr := directory.ReadDir(128)
		for _, entry := range batch {
			if beforeEntry != nil {
				beforeEntry()
			}
			*scannedEntries++
			if *scannedEntries > maximumDiagnosticManifestScanEntries {
				manifest.truncated = true
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				continue
			}
			child := entry.Name()
			if relative != "." {
				child = filepath.Join(relative, child)
			}
			if info.Mode()&os.ModeSymlink != 0 {
				// os.Root follows only links whose resolved target remains beneath
				// the opened project root. Preserve valid in-project link spellings;
				// an external or broken target fails this descriptor-rooted Stat.
				info, err = root.Stat(child)
				if err != nil {
					continue
				}
			}
			if info.IsDir() {
				if entry.Name() == ".git" || entry.Name() == "node_modules" {
					continue
				}
				if diagnosticManifestDirectorySeen(ancestors, info) {
					continue
				}
				if beforeDirectoryOpen != nil {
					beforeDirectoryOpen(filepath.ToSlash(child))
				}
				if err := walkDiagnosticProjectManifest(
					root,
					child,
					info,
					append(ancestors, info),
					manifest,
					sourceEntries,
					scannedEntries,
					beforeDirectoryOpen,
					beforeEntry,
				); err != nil {
					return err
				}
				if manifest.truncated {
					return nil
				}
				continue
			}
			if !info.Mode().IsRegular() || !isPotentialDiagnosticSource(child) {
				continue
			}
			*sourceEntries++
			if *sourceEntries > maximumDiagnosticManifestEntries {
				manifest.truncated = true
				return nil
			}
			manifest.add(filepath.ToSlash(child))
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

func diagnosticManifestDirectorySeen(ancestors []os.FileInfo, candidate os.FileInfo) bool {
	for _, ancestor := range ancestors {
		if os.SameFile(ancestor, candidate) {
			return true
		}
	}
	return false
}

func isPotentialDiagnosticSource(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".aux", ".dvi", ".gif", ".gz", ".jpeg", ".jpg", ".log", ".pdf", ".png", ".synctex", ".tif", ".tiff", ".xdv":
		return false
	default:
		return true
	}
}

func diagnosticReportedFileCandidates(reported string) []string {
	rel := filepath.ToSlash(filepath.Clean(reported))
	candidates := []string{rel}
	if filepath.Ext(rel) == "" {
		candidates = append(candidates, rel+".tex")
	}
	return candidates
}

func (a *App) readDiagnosticTextLocked(relative string) (string, error) {
	if a.diagnosticReadText != nil {
		return a.diagnosticReadText(relative)
	}
	return a.proj.ReadText(relative)
}

func (a *App) diagnosticTextSizeLocked(relative string) (int64, error) {
	if a.diagnosticTextSize != nil {
		return a.diagnosticTextSize(relative)
	}
	return a.proj.EditableTextSize(relative)
}

// currentPDFURL returns the asset URL for the latest PDF, or "" if there is
// none. Callers must hold mu.
func (a *App) currentPDFURL() string {
	if a.pdfPath == "" {
		return ""
	}
	return fmt.Sprintf("%s?rev=%d", pdfURLPath, a.revision)
}

// ToggleFullscreen switches the window in and out of fullscreen.
//
// macOS has its own affordance for this once the window is allowed to go
// fullscreen at all, but Windows offers no standard one, so the app has to
// provide the control itself to behave the same on every platform.
func (a *App) ToggleFullscreen() bool {
	full := !runtime.WindowIsFullscreen(a.context())
	if full {
		runtime.WindowFullscreen(a.context())
	} else {
		runtime.WindowUnfullscreen(a.context())
	}
	return full
}

// EngineVersion reports the engine's self-identification for the status bar, or
// an explanation of why there isn't one.
func (a *App) EngineVersion() string {
	v, err := a.compiler.Version(a.context())
	if err != nil {
		return fmt.Sprintf("no engine: %v", err)
	}
	return v
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
