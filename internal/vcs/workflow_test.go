package vcs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

var testWho = Identity{Name: "Test", Email: "test@example.com"}

// workingCopy returns a clone with one commit, which is the state every one of
// these operations starts from in the app.
func workingCopy(t *testing.T) string {
	t.Helper()
	source := sourceRepo(t)
	destination := filepath.Join(t.TempDir(), "clone")
	if err := Clone(context.Background(), source, destination, ""); err != nil {
		t.Fatal(err)
	}
	return destination
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func headMessage(t *testing.T, dir string) string {
	t.Helper()
	repository, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatal(err)
	}
	head, err := repository.Head()
	if err != nil {
		t.Fatal(err)
	}
	commit, err := repository.CommitObject(head.Hash())
	if err != nil {
		t.Fatal(err)
	}
	return commit.Message
}

// --- branch names -------------------------------------------------------------

func TestValidateBranchName(t *testing.T) {
	for _, good := range []string{"main", "feature/zoom", "fix-123", "v2.1", "a"} {
		if err := ValidateBranchName(good); err != nil {
			t.Errorf("%q was rejected: %v", good, err)
		}
	}
	for _, bad := range []string{
		"", "  ", " leading", "trailing ", "-starts-with-dash", "has space",
		"has..dots", "ends.", "double//slash", "trailing/", "wip.lock",
		"quote'name", "semi;colon", "tilde~name",
	} {
		if err := ValidateBranchName(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

// --- creating and switching -----------------------------------------------------

func TestCreateBranchChecksItOut(t *testing.T) {
	dir := workingCopy(t)

	if err := CreateBranch(dir, "feature/zoom"); err != nil {
		t.Fatal(err)
	}
	branches, err := ListBranches(dir)
	if err != nil {
		t.Fatal(err)
	}
	if branches.Current != "feature/zoom" {
		t.Errorf("current branch: got %q", branches.Current)
	}
	if !slices.Contains(branches.Local, "feature/zoom") {
		t.Errorf("new branch missing from %v", branches.Local)
	}
}

// Branching is usually what you do *because* you have started something, so
// work in progress has to come along rather than block the action.
func TestCreateBranchCarriesUncommittedWork(t *testing.T) {
	dir := workingCopy(t)
	write(t, dir, "draft.tex", "in progress\n")

	if err := CreateBranch(dir, "wip"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "draft.tex")); err != nil {
		t.Errorf("the in-progress file did not come along: %v", err)
	}
}

func TestCreateBranchRefusesADuplicate(t *testing.T) {
	dir := workingCopy(t)
	if err := CreateBranch(dir, "dup"); err != nil {
		t.Fatal(err)
	}
	if err := CreateBranch(dir, "dup"); !errors.Is(err, ErrBranchExists) {
		t.Errorf("got %v, want ErrBranchExists", err)
	}
}

func TestCreateBranchRejectsABadName(t *testing.T) {
	dir := workingCopy(t)
	if err := CreateBranch(dir, "no spaces allowed"); err == nil {
		t.Error("a name with spaces was accepted")
	}
}

// The plan's safety rule: never switch over uncommitted changes without an
// explicit choice. Blocking is that choice being withheld.
func TestSwitchBranchRefusesWhileDirty(t *testing.T) {
	dir := workingCopy(t)
	if err := CreateBranch(dir, "other"); err != nil {
		t.Fatal(err)
	}
	start, err := ListBranches(dir)
	if err != nil {
		t.Fatal(err)
	}
	write(t, dir, "main.tex", "edited but not committed\n")

	err = SwitchBranch(dir, start.Local[0])
	if !errors.Is(err, ErrDirty) {
		t.Fatalf("got %v, want ErrDirty", err)
	}
	// And it must not have half-switched.
	after, err := ListBranches(dir)
	if err != nil {
		t.Fatal(err)
	}
	if after.Current != start.Current {
		t.Errorf("the branch changed anyway: %q -> %q", start.Current, after.Current)
	}
}

func TestSwitchBranchWorksWhenClean(t *testing.T) {
	dir := workingCopy(t)
	original, err := ListBranches(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := CreateBranch(dir, "other"); err != nil {
		t.Fatal(err)
	}

	if err := SwitchBranch(dir, original.Current); err != nil {
		t.Fatal(err)
	}
	now, err := ListBranches(dir)
	if err != nil {
		t.Fatal(err)
	}
	if now.Current != original.Current {
		t.Errorf("current: got %q, want %q", now.Current, original.Current)
	}
}

// DirtyFiles is what turns "there are uncommitted changes" into a message that
// says which ones.
func TestDirtyFilesNamesTheFilesInTheWay(t *testing.T) {
	dir := workingCopy(t)
	if files := DirtyFiles(dir); len(files) != 0 {
		t.Fatalf("a fresh clone reported %v", files)
	}
	write(t, dir, "main.tex", "edited\n")
	write(t, dir, "new.tex", "added\n")

	files := DirtyFiles(dir)
	if !slices.Contains(files, "main.tex") || !slices.Contains(files, "new.tex") {
		t.Errorf("got %v, want both changed files", files)
	}
}

// --- committing ------------------------------------------------------------------

func TestCommitRecordsEverythingDirty(t *testing.T) {
	dir := workingCopy(t)
	write(t, dir, "main.tex", "edited\n")
	write(t, dir, "extra.tex", "new file\n")

	hash, err := Commit(dir, "  describe the change  ", testWho)
	if err != nil {
		t.Fatal(err)
	}
	if hash == "" {
		t.Error("no commit hash returned")
	}
	// Surrounding whitespace comes free with a textarea.
	if got := headMessage(t, dir); got != "describe the change" {
		t.Errorf("message: got %q", got)
	}
	// A new file has to be staged too, or "commit what I see" is a lie.
	if Status(dir).Dirty {
		t.Error("the working copy is still dirty after committing")
	}
}

func TestCommitRefusesWithNothingToDo(t *testing.T) {
	dir := workingCopy(t)
	if _, err := Commit(dir, "empty", testWho); !errors.Is(err, ErrNoChanges) {
		t.Errorf("got %v, want ErrNoChanges", err)
	}
}

func TestCommitRefusesWithoutAMessage(t *testing.T) {
	dir := workingCopy(t)
	write(t, dir, "main.tex", "edited\n")
	if _, err := Commit(dir, "   ", testWho); err == nil {
		t.Error("an empty message was accepted")
	}
}

// An unattributable commit is worse than a refused one: it lands in history
// with a blank author and cannot be fixed without a rewrite.
func TestCommitRefusesWithoutAnIdentity(t *testing.T) {
	dir := workingCopy(t)
	write(t, dir, "main.tex", "edited\n")
	if _, err := Commit(dir, "message", Identity{}); !errors.Is(err, ErrNoIdentity) {
		t.Errorf("got %v, want ErrNoIdentity", err)
	}
}

func TestCommitRemovesDeletedFiles(t *testing.T) {
	dir := workingCopy(t)
	if err := os.Remove(filepath.Join(dir, "main.tex")); err != nil {
		t.Fatal(err)
	}

	if _, err := Commit(dir, "remove main", testWho); err != nil {
		t.Fatal(err)
	}
	if Status(dir).Dirty {
		t.Error("a deletion was not staged")
	}
}

// --- pushing ----------------------------------------------------------------------

// bareRemote is a real remote to push into, so the push path is exercised end
// to end without a network.
func bareRemote(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "remote.git")
	if _, err := git.PlainInit(dir, true); err != nil {
		t.Fatal(err)
	}
	return dir
}

// seeded returns a working copy whose origin is a real bare repository.
func seeded(t *testing.T) (dir, remote string) {
	t.Helper()
	remote = bareRemote(t)
	dir = t.TempDir()

	repository, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{remote}}); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "main.tex", "hello\n")
	worktree, err := repository.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worktree.Add("main.tex"); err != nil {
		t.Fatal(err)
	}
	if _, err := worktree.Commit("initial", &git.CommitOptions{
		Author: &object.Signature{Name: testWho.Name, Email: testWho.Email, When: time.Now()},
	}); err != nil {
		t.Fatal(err)
	}
	return dir, remote
}

func TestPushPublishesTheBranch(t *testing.T) {
	dir, remote := seeded(t)
	branches, err := ListBranches(dir)
	if err != nil {
		t.Fatal(err)
	}

	if HasUpstream(dir, branches.Current) {
		t.Error("an unpushed branch claimed an upstream")
	}
	if err := Push(context.Background(), dir, branches.Current, ""); err != nil {
		t.Fatal(err)
	}

	// The commit really is in the remote.
	bare, err := git.PlainOpen(remote)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bare.Reference(plumbing.NewBranchReferenceName(branches.Current), true); err != nil {
		t.Errorf("the branch is not in the remote: %v", err)
	}
	if !HasUpstream(dir, branches.Current) {
		t.Error("the branch was not recorded as tracking origin")
	}
}

// Pushing twice with nothing new is a normal thing to do by accident, and must
// not read as a failure.
func TestPushAgainWithNothingNewSucceeds(t *testing.T) {
	dir, _ := seeded(t)
	branches, err := ListBranches(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := Push(context.Background(), dir, branches.Current, ""); err != nil {
		t.Fatal(err)
	}
	if err := Push(context.Background(), dir, branches.Current, ""); err != nil {
		t.Errorf("a second push reported: %v", err)
	}
}

// A push must publish the branch asked for and nothing else.
func TestPushPublishesOnlyTheNamedBranch(t *testing.T) {
	dir, remote := seeded(t)
	if err := CreateBranch(dir, "secret"); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "secret.tex", "not for sharing yet\n")
	if _, err := Commit(dir, "wip", testWho); err != nil {
		t.Fatal(err)
	}
	if err := SwitchBranch(dir, "master"); err != nil {
		// go-git's default initial branch name; skip rather than assert on it.
		t.Skipf("unexpected default branch layout: %v", err)
	}

	branches, err := ListBranches(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := Push(context.Background(), dir, branches.Current, ""); err != nil {
		t.Fatal(err)
	}

	bare, err := git.PlainOpen(remote)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bare.Reference(plumbing.NewBranchReferenceName("secret"), true); err == nil {
		t.Error("pushing one branch published another")
	}
}
