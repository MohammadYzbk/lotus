package vcs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
)

// twoMachines is the situation this whole file exists for: one remote and two
// working copies of it, standing in for a laptop and a desktop.
func twoMachines(t *testing.T) (remote, laptop, desktop string) {
	t.Helper()
	remote = bareRemote(t)

	laptop = filepath.Join(t.TempDir(), "laptop")
	seed, _ := seeded(t)
	// Point the seeded copy at our bare remote and publish it, so both clones
	// share a history.
	if err := os.RemoveAll(laptop); err != nil {
		t.Fatal(err)
	}
	if err := Clone(context.Background(), seed, laptop, ""); err != nil {
		t.Fatal(err)
	}
	repointOrigin(t, laptop, remote)
	branch := currentBranch(t, laptop)
	if err := Push(context.Background(), laptop, branch, ""); err != nil {
		t.Fatal(err)
	}

	desktop = filepath.Join(t.TempDir(), "desktop")
	if err := Clone(context.Background(), remote, desktop, ""); err != nil {
		t.Fatal(err)
	}
	return remote, laptop, desktop
}

func repointOrigin(t *testing.T, dir, remote string) {
	t.Helper()
	repository, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.DeleteRemote("origin"); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{remote}}); err != nil {
		t.Fatal(err)
	}
}

func currentBranch(t *testing.T, dir string) string {
	t.Helper()
	branches, err := ListBranches(dir)
	if err != nil {
		t.Fatal(err)
	}
	return branches.Current
}

// commitFile writes and commits, standing in for a session of editing.
func commitFile(t *testing.T, dir, name, content, message string) {
	t.Helper()
	write(t, dir, name, content)
	if _, err := Commit(dir, message, testWho); err != nil {
		t.Fatal(err)
	}
}

// --- where a branch stands --------------------------------------------------

func TestSyncStateOnAnUnpushedBranch(t *testing.T) {
	dir := workingCopy(t)
	if err := CreateBranch(dir, "local-only"); err != nil {
		t.Fatal(err)
	}

	state := SyncState(dir, "local-only")
	if state.Tracking {
		t.Error("a branch never pushed claimed to be tracking")
	}
	if state.Error != "" {
		t.Errorf("unexpected error: %s", state.Error)
	}
}

func TestSyncStateCountsBehind(t *testing.T) {
	_, laptop, desktop := twoMachines(t)
	branch := currentBranch(t, laptop)

	commitFile(t, laptop, "a.tex", "from the laptop\n", "laptop work")
	if err := Push(context.Background(), laptop, branch, ""); err != nil {
		t.Fatal(err)
	}

	if err := Fetch(context.Background(), desktop, ""); err != nil {
		t.Fatal(err)
	}
	state := SyncState(desktop, branch)
	if state.Behind != 1 || state.Ahead != 0 {
		t.Errorf("got ahead=%d behind=%d, want 0/1", state.Ahead, state.Behind)
	}
	if state.Diverged {
		t.Error("being merely behind was reported as diverged")
	}
}

func TestSyncStateCountsAhead(t *testing.T) {
	_, laptop, _ := twoMachines(t)
	branch := currentBranch(t, laptop)
	commitFile(t, laptop, "a.tex", "unpublished\n", "local work")

	state := SyncState(laptop, branch)
	if state.Ahead != 1 || state.Behind != 0 {
		t.Errorf("got ahead=%d behind=%d, want 1/0", state.Ahead, state.Behind)
	}
}

// The case the whole phase is about: both machines moved.
func TestSyncStateDetectsDivergence(t *testing.T) {
	_, laptop, desktop := twoMachines(t)
	branch := currentBranch(t, laptop)

	commitFile(t, laptop, "a.tex", "from the laptop\n", "laptop work")
	if err := Push(context.Background(), laptop, branch, ""); err != nil {
		t.Fatal(err)
	}
	commitFile(t, desktop, "b.tex", "from the desktop\n", "desktop work")

	if err := Fetch(context.Background(), desktop, ""); err != nil {
		t.Fatal(err)
	}
	state := SyncState(desktop, branch)
	if !state.Diverged {
		t.Fatalf("divergence went unnoticed: ahead=%d behind=%d", state.Ahead, state.Behind)
	}
	if state.Ahead != 1 || state.Behind != 1 {
		t.Errorf("got ahead=%d behind=%d, want 1/1", state.Ahead, state.Behind)
	}
}

// Fetch must never touch the working copy — that is what makes it safe to run
// in order to answer "where am I".
func TestFetchLeavesTheWorkingCopyAlone(t *testing.T) {
	_, laptop, desktop := twoMachines(t)
	branch := currentBranch(t, laptop)
	commitFile(t, laptop, "a.tex", "from the laptop\n", "laptop work")
	if err := Push(context.Background(), laptop, branch, ""); err != nil {
		t.Fatal(err)
	}

	before := headMessage(t, desktop)
	if err := Fetch(context.Background(), desktop, ""); err != nil {
		t.Fatal(err)
	}
	if after := headMessage(t, desktop); after != before {
		t.Errorf("fetch moved HEAD: %q -> %q", before, after)
	}
	if _, err := os.Stat(filepath.Join(desktop, "a.tex")); err == nil {
		t.Error("fetch wrote the remote's file into the working copy")
	}
}

// --- pulling -----------------------------------------------------------------

func TestPullFastForwards(t *testing.T) {
	_, laptop, desktop := twoMachines(t)
	branch := currentBranch(t, laptop)
	commitFile(t, laptop, "a.tex", "from the laptop\n", "laptop work")
	if err := Push(context.Background(), laptop, branch, ""); err != nil {
		t.Fatal(err)
	}

	outcome, err := Pull(context.Background(), desktop, branch, "")
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Updated != 1 {
		t.Errorf("updated: got %d, want 1", outcome.Updated)
	}
	if _, err := os.Stat(filepath.Join(desktop, "a.tex")); err != nil {
		t.Errorf("the pulled file is missing: %v", err)
	}
}

func TestPullWithNothingNew(t *testing.T) {
	_, laptop, desktop := twoMachines(t)
	outcome, err := Pull(context.Background(), desktop, currentBranch(t, laptop), "")
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.AlreadyCurrent {
		t.Errorf("got %+v, want AlreadyCurrent", outcome)
	}
}

// Pulling over divergence must refuse rather than merge or rewrite. Reporting
// it is the whole point; guessing is how work gets lost.
func TestPullRefusesWhenDiverged(t *testing.T) {
	_, laptop, desktop := twoMachines(t)
	branch := currentBranch(t, laptop)
	commitFile(t, laptop, "a.tex", "from the laptop\n", "laptop work")
	if err := Push(context.Background(), laptop, branch, ""); err != nil {
		t.Fatal(err)
	}
	commitFile(t, desktop, "b.tex", "from the desktop\n", "desktop work")
	before := headMessage(t, desktop)

	outcome, err := Pull(context.Background(), desktop, branch, "")
	if !errors.Is(err, ErrDiverged) {
		t.Fatalf("got %v, want ErrDiverged", err)
	}
	if !outcome.Diverged {
		t.Error("the outcome did not report divergence")
	}
	if after := headMessage(t, desktop); after != before {
		t.Errorf("a refused pull moved HEAD: %q -> %q", before, after)
	}
}

// --- rebasing -----------------------------------------------------------------

func requireGit(t *testing.T) {
	t.Helper()
	if !GitAvailable() {
		t.Fatal("git is not installed; the rebase path cannot be exercised")
	}
}

// The happy path of the two-machine round trip: both moved, different files,
// and the local work ends up on top of the remote's.
func TestRebaseReplaysLocalWorkOnTop(t *testing.T) {
	requireGit(t)
	_, laptop, desktop := twoMachines(t)
	branch := currentBranch(t, laptop)

	commitFile(t, laptop, "a.tex", "from the laptop\n", "laptop work")
	if err := Push(context.Background(), laptop, branch, ""); err != nil {
		t.Fatal(err)
	}
	commitFile(t, desktop, "b.tex", "from the desktop\n", "desktop work")

	if err := Fetch(context.Background(), desktop, ""); err != nil {
		t.Fatal(err)
	}
	if err := RebaseOntoRemote(context.Background(), desktop, branch); err != nil {
		t.Fatal(err)
	}

	// Both machines' files are present, and the local commit is on top.
	for _, name := range []string{"a.tex", "b.tex"} {
		if _, err := os.Stat(filepath.Join(desktop, name)); err != nil {
			t.Errorf("%s is missing after the rebase: %v", name, err)
		}
	}
	if got := headMessage(t, desktop); !strings.Contains(got, "desktop work") {
		t.Errorf("HEAD is %q, want the local commit on top", got)
	}

	state := SyncState(desktop, branch)
	if state.Diverged || state.Behind != 0 {
		t.Errorf("still diverged after rebasing: %+v", state)
	}
	if state.Ahead != 1 {
		t.Errorf("ahead: got %d, want 1", state.Ahead)
	}
}

// The same file edited on both machines: Git stops, and the app has to report
// which files carry markers rather than failing opaquely.
func TestRebaseSurfacesConflicts(t *testing.T) {
	requireGit(t)
	_, laptop, desktop := twoMachines(t)
	branch := currentBranch(t, laptop)

	commitFile(t, laptop, "main.tex", "the laptop's version\n", "laptop edit")
	if err := Push(context.Background(), laptop, branch, ""); err != nil {
		t.Fatal(err)
	}
	commitFile(t, desktop, "main.tex", "the desktop's version\n", "desktop edit")

	if err := Fetch(context.Background(), desktop, ""); err != nil {
		t.Fatal(err)
	}
	err := RebaseOntoRemote(context.Background(), desktop, branch)
	if !errors.Is(err, ErrRebaseConflict) {
		t.Fatalf("got %v, want ErrRebaseConflict", err)
	}

	state := SyncState(desktop, branch)
	if !state.Rebasing {
		t.Error("a stopped rebase was not reported as in progress")
	}
	if len(state.Conflicted) == 0 {
		t.Fatal("no conflicted files were reported")
	}
	if state.Conflicted[0] != "main.tex" {
		t.Errorf("conflicted: got %v, want main.tex", state.Conflicted)
	}

	// The file on disk carries the standard markers, which is what makes the
	// editor the place this gets fixed.
	content, err := os.ReadFile(filepath.Join(desktop, "main.tex"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "<<<<<<<") || !strings.Contains(string(content), ">>>>>>>") {
		t.Errorf("no conflict markers in the file:\n%s", content)
	}
}

// Abort is what makes trying a rebase safe: whatever happened, this puts the
// branch back where it started.
func TestAbortRebaseRestoresTheBranch(t *testing.T) {
	requireGit(t)
	_, laptop, desktop := twoMachines(t)
	branch := currentBranch(t, laptop)

	commitFile(t, laptop, "main.tex", "the laptop's version\n", "laptop edit")
	if err := Push(context.Background(), laptop, branch, ""); err != nil {
		t.Fatal(err)
	}
	commitFile(t, desktop, "main.tex", "the desktop's version\n", "desktop edit")
	before := headMessage(t, desktop)

	if err := Fetch(context.Background(), desktop, ""); err != nil {
		t.Fatal(err)
	}
	if err := RebaseOntoRemote(context.Background(), desktop, branch); !errors.Is(err, ErrRebaseConflict) {
		t.Fatalf("expected a conflict, got %v", err)
	}
	if err := AbortRebase(context.Background(), desktop); err != nil {
		t.Fatal(err)
	}

	if after := headMessage(t, desktop); after != before {
		t.Errorf("abort left HEAD at %q, want %q", after, before)
	}
	if SyncState(desktop, branch).Rebasing {
		t.Error("still reported as rebasing after aborting")
	}
	content, err := os.ReadFile(filepath.Join(desktop, "main.tex"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(content), "<<<<<<<") {
		t.Error("conflict markers survived the abort")
	}
}

// Resolving the markers and continuing finishes the round trip.
func TestContinueRebaseAfterResolving(t *testing.T) {
	requireGit(t)
	_, laptop, desktop := twoMachines(t)
	branch := currentBranch(t, laptop)

	commitFile(t, laptop, "main.tex", "the laptop's version\n", "laptop edit")
	if err := Push(context.Background(), laptop, branch, ""); err != nil {
		t.Fatal(err)
	}
	commitFile(t, desktop, "main.tex", "the desktop's version\n", "desktop edit")

	if err := Fetch(context.Background(), desktop, ""); err != nil {
		t.Fatal(err)
	}
	if err := RebaseOntoRemote(context.Background(), desktop, branch); !errors.Is(err, ErrRebaseConflict) {
		t.Fatalf("expected a conflict, got %v", err)
	}

	// What the writer does in the editor: remove the markers, keep both.
	write(t, desktop, "main.tex", "the laptop's version\nthe desktop's version\n")

	if err := ContinueRebase(context.Background(), desktop); err != nil {
		t.Fatal(err)
	}
	state := SyncState(desktop, branch)
	if state.Rebasing {
		t.Error("still rebasing after continuing")
	}
	if len(state.Conflicted) != 0 {
		t.Errorf("conflicts remain: %v", state.Conflicted)
	}
	if state.Behind != 0 {
		t.Errorf("behind: got %d, want 0", state.Behind)
	}
}

// Continuing with the markers still in place would commit them into history.
func TestContinueRebaseRefusesWhileStillConflicted(t *testing.T) {
	requireGit(t)
	_, laptop, desktop := twoMachines(t)
	branch := currentBranch(t, laptop)

	commitFile(t, laptop, "main.tex", "the laptop's version\n", "laptop edit")
	if err := Push(context.Background(), laptop, branch, ""); err != nil {
		t.Fatal(err)
	}
	commitFile(t, desktop, "main.tex", "the desktop's version\n", "desktop edit")
	if err := Fetch(context.Background(), desktop, ""); err != nil {
		t.Fatal(err)
	}
	if err := RebaseOntoRemote(context.Background(), desktop, branch); !errors.Is(err, ErrRebaseConflict) {
		t.Fatalf("expected a conflict, got %v", err)
	}

	if err := ContinueRebase(context.Background(), desktop); !errors.Is(err, ErrRebaseConflict) {
		t.Errorf("got %v, want ErrRebaseConflict", err)
	}
}

// Rebasing over uncommitted work would discard or stash it invisibly.
func TestRebaseRefusesWhileDirty(t *testing.T) {
	requireGit(t)
	_, laptop, desktop := twoMachines(t)
	branch := currentBranch(t, laptop)
	commitFile(t, laptop, "a.tex", "from the laptop\n", "laptop work")
	if err := Push(context.Background(), laptop, branch, ""); err != nil {
		t.Fatal(err)
	}

	write(t, desktop, "scratch.tex", "not committed\n")
	if err := Fetch(context.Background(), desktop, ""); err != nil {
		t.Fatal(err)
	}
	if err := RebaseOntoRemote(context.Background(), desktop, branch); !errors.Is(err, ErrDirty) {
		t.Errorf("got %v, want ErrDirty", err)
	}
	if _, err := os.Stat(filepath.Join(desktop, "scratch.tex")); err != nil {
		t.Errorf("the uncommitted file was lost: %v", err)
	}
}
