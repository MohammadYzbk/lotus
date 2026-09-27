package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// syncApp prepares the two-machine situation and opens the "desktop" copy as
// the app's project: one bare remote, and two working copies of it.
func syncApp(t *testing.T) (app *App, laptop, desktop string) {
	t.Helper()
	app, _ = githubApp(t, okUser)

	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	if _, err := git.PlainInit(remote, true); err != nil {
		t.Fatal(err)
	}

	laptop = filepath.Join(root, "laptop")
	repository, err := git.PlainInit(laptop, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{remote}}); err != nil {
		t.Fatal(err)
	}
	setIdentity(t, laptop)
	machineCommit(t, laptop, "main.tex", rootDoc, "initial")
	pushFrom(t, laptop)

	desktop = filepath.Join(root, "desktop")
	if _, err := git.PlainClone(desktop, false, &git.CloneOptions{URL: remote}); err != nil {
		t.Fatal(err)
	}
	setIdentity(t, desktop)

	if info := app.OpenProject(desktop); info.Error != "" {
		t.Fatalf("open project: %s", info.Error)
	}
	return app, laptop, desktop
}

func setIdentity(t *testing.T, dir string) {
	t.Helper()
	repository, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := repository.Config()
	if err != nil {
		t.Fatal(err)
	}
	cfg.User.Name, cfg.User.Email = "Test", "test@example.com"
	if err := repository.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
}

func machineCommit(t *testing.T, dir, name, content, message string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	repository, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatal(err)
	}
	worktree, err := repository.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := worktree.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := worktree.Commit(message, &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()},
	}); err != nil {
		t.Fatal(err)
	}
}

func pushFrom(t *testing.T, dir string) {
	t.Helper()
	repository, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Push(&git.PushOptions{RemoteName: "origin"}); err != nil {
		t.Fatal(err)
	}
}

func TestSyncStatusOnAPlainFolder(t *testing.T) {
	app, _ := githubApp(t, okUser)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.tex"), []byte(rootDoc), 0o644); err != nil {
		t.Fatal(err)
	}
	if info := app.OpenProject(dir); info.Error != "" {
		t.Fatal(info.Error)
	}

	result := app.SyncStatus()
	if result.State.Repository {
		t.Error("a plain folder was reported as a repository")
	}
	if result.Error != "" {
		t.Errorf("unexpected error: %s", result.Error)
	}
}

func TestFetchReportsWhatIsWaiting(t *testing.T) {
	app, laptop, _ := syncApp(t)
	machineCommit(t, laptop, "a.tex", "from the laptop\n", "laptop work")
	pushFrom(t, laptop)

	result := app.FetchRemote()
	if result.Error != "" {
		t.Fatalf("fetch: %s", result.Error)
	}
	if result.Sync.Behind != 1 {
		t.Errorf("behind: got %d, want 1", result.Sync.Behind)
	}
	if !strings.Contains(result.Detail, "to pull") {
		t.Errorf("detail: got %q", result.Detail)
	}
}

func TestPullFastForwardsThroughTheApp(t *testing.T) {
	app, laptop, desktop := syncApp(t)
	machineCommit(t, laptop, "a.tex", "from the laptop\n", "laptop work")
	pushFrom(t, laptop)

	result := app.PullChanges()
	if result.Error != "" {
		t.Fatalf("pull: %s", result.Error)
	}
	if !strings.Contains(result.Detail, "Pulled 1 commit") {
		t.Errorf("detail: got %q", result.Detail)
	}
	if _, err := os.Stat(filepath.Join(desktop, "a.tex")); err != nil {
		t.Errorf("the pulled file is missing: %v", err)
	}
}

// A fast-forward over uncommitted work can fail part-way, leaving files from
// two commits on disk.
func TestPullRefusesWhileDirty(t *testing.T) {
	app, laptop, desktop := syncApp(t)
	machineCommit(t, laptop, "a.tex", "from the laptop\n", "laptop work")
	pushFrom(t, laptop)
	if err := os.WriteFile(filepath.Join(desktop, "main.tex"), []byte("edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	result := app.PullChanges()
	if result.Error == "" {
		t.Fatal("the pull was allowed over uncommitted changes")
	}
	if !strings.Contains(result.Error, "main.tex") {
		t.Errorf("the error does not name the file: %q", result.Error)
	}
}

// The message has to say what happened and what to do, not just fail.
func TestPullOnDivergenceExplainsAndOffersRebase(t *testing.T) {
	app, laptop, desktop := syncApp(t)
	machineCommit(t, laptop, "a.tex", "from the laptop\n", "laptop work")
	pushFrom(t, laptop)
	machineCommit(t, desktop, "b.tex", "from the desktop\n", "desktop work")

	result := app.PullChanges()
	if result.Error == "" {
		t.Fatal("a diverged pull reported success")
	}
	for _, want := range []string{"both moved", "Rebase"} {
		if !strings.Contains(result.Error, want) {
			t.Errorf("error %q does not mention %q", result.Error, want)
		}
	}
	if !result.Sync.Diverged {
		t.Error("the result did not report divergence")
	}
}

func TestRebaseResolvesDivergenceThroughTheApp(t *testing.T) {
	app, laptop, desktop := syncApp(t)
	machineCommit(t, laptop, "a.tex", "from the laptop\n", "laptop work")
	pushFrom(t, laptop)
	machineCommit(t, desktop, "b.tex", "from the desktop\n", "desktop work")

	result := app.RebaseOnRemote()
	if result.Error != "" {
		t.Fatalf("rebase: %s", result.Error)
	}
	if result.Sync.Diverged || result.Sync.Behind != 0 {
		t.Errorf("still diverged: %+v", result.Sync)
	}
	for _, name := range []string{"a.tex", "b.tex"} {
		if _, err := os.Stat(filepath.Join(desktop, name)); err != nil {
			t.Errorf("%s missing after the rebase: %v", name, err)
		}
	}
}

// When it stops, the writer needs to know which file to open and what to do.
func TestRebaseConflictTellsTheWriterWhatToDo(t *testing.T) {
	app, laptop, desktop := syncApp(t)
	machineCommit(t, laptop, "main.tex", "the laptop's version\n", "laptop edit")
	pushFrom(t, laptop)
	machineCommit(t, desktop, "main.tex", "the desktop's version\n", "desktop edit")

	result := app.RebaseOnRemote()
	if result.Error == "" {
		t.Fatal("a conflicting rebase reported success")
	}
	for _, want := range []string{"main.tex", "<<<<<<<", "continue"} {
		if !strings.Contains(result.Error, want) {
			t.Errorf("error %q does not mention %q", result.Error, want)
		}
	}
	if !result.Sync.Rebasing {
		t.Error("the result did not report a rebase in progress")
	}

	// And the escape hatch works.
	aborted := app.AbortRebase()
	if aborted.Error != "" {
		t.Fatalf("abort: %s", aborted.Error)
	}
	if aborted.Sync.Rebasing {
		t.Error("still rebasing after aborting")
	}
	content, err := os.ReadFile(filepath.Join(desktop, "main.tex"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(content), "<<<<<<<") {
		t.Error("conflict markers survived the abort")
	}
}

func TestContinueRebaseThroughTheApp(t *testing.T) {
	app, laptop, desktop := syncApp(t)
	machineCommit(t, laptop, "main.tex", "the laptop's version\n", "laptop edit")
	pushFrom(t, laptop)
	machineCommit(t, desktop, "main.tex", "the desktop's version\n", "desktop edit")

	if result := app.RebaseOnRemote(); result.Error == "" {
		t.Fatal("expected a conflict")
	}
	// What the writer does in the editor.
	if err := os.WriteFile(filepath.Join(desktop, "main.tex"),
		[]byte("the laptop's version\nthe desktop's version\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	result := app.ContinueRebase()
	if result.Error != "" {
		t.Fatalf("continue: %s", result.Error)
	}
	if result.Sync.Rebasing || result.Sync.Behind != 0 {
		t.Errorf("not finished: %+v", result.Sync)
	}
}
