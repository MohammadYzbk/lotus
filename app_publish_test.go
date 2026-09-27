package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/MohammadYzbk/lotus/internal/vcs"
	"github.com/go-git/go-git/v5"
)

// publishApp opens a plain project folder — the state this feature exists for
// — and counts how often the create-repository endpoint is reached.
func publishApp(t *testing.T, remote string) (*App, string, *atomic.Int32) {
	t.Helper()
	var creates atomic.Int32

	app, store := githubApp(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/user/repos") && r.Method == http.MethodPost:
			creates.Add(1)
			fmt.Fprintf(w, `{"full_name":"octocat/paper","clone_url":%q,"default_branch":"main","private":true}`, remote)
		case strings.HasSuffix(r.URL.Path, "/user"):
			okUser(w, r)
		default:
			fmt.Fprint(w, `{}`)
		}
	})
	if err := store.Set("gho_secret"); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.tex"), []byte(rootDoc), 0o644); err != nil {
		t.Fatal(err)
	}
	if info := app.OpenProject(dir); info.Error != "" {
		t.Fatal(info.Error)
	}
	return app, dir, &creates
}

// withGitIdentity gives the test a global Git identity of its own.
//
// Publishing commits, and a commit needs an author. Relying on the developer's
// real ~/.gitconfig would make this pass or fail depending on the machine, so
// HOME is pointed at a directory this test wrote itself.
func withGitIdentity(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)

	config := "[user]\n\tname = Test\n\temail = test@example.com\n"
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
}

// bareTarget is a real repository to publish into, so the push is exercised.
func bareTarget(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "remote.git")
	if _, err := git.PlainInit(dir, true); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestPublishStatusOnAPlainFolder(t *testing.T) {
	app, dir, _ := publishApp(t, bareTarget(t))

	status := app.PublishStatus()
	if !status.Publishable {
		t.Errorf("a plain folder was not publishable: %s", status.Reason)
	}
	if status.Connected {
		t.Error("a plain folder was reported as connected")
	}
	if status.SuggestedName != SuggestRepositoryName(filepath.Base(dir)) {
		t.Errorf("suggested name: got %q", status.SuggestedName)
	}
}

// The whole point: a folder you have been writing in becomes a repository with
// a remote, and the work is on it.
func TestPublishInitialisesCommitsAndPushes(t *testing.T) {
	remote := bareTarget(t)
	withGitIdentity(t)
	app, _, _ := publishApp(t, remote)

	result := app.PublishToGitHub(PublishDraft{Name: "paper", Private: true})
	if result.Error != "" {
		t.Fatalf("publish: %s", result.Error)
	}
	if result.Repository.FullName != "octocat/paper" {
		t.Errorf("repository: got %+v", result.Repository)
	}
	if !result.State.Repository {
		t.Error("the folder is still not a repository")
	}
	if result.State.Remote != remote {
		t.Errorf("origin: got %q, want %q", result.State.Remote, remote)
	}
	if result.State.Dirty {
		t.Error("the working copy is dirty after publishing")
	}

	// The commit really is on the remote, on main.
	bare, err := git.PlainOpen(remote)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bare.Reference("refs/heads/"+vcs.DefaultBranch, true); err != nil {
		t.Errorf("nothing was pushed to %s: %v", vcs.DefaultBranch, err)
	}
	if !strings.Contains(result.Detail, "octocat/paper") {
		t.Errorf("detail: got %q", result.Detail)
	}
}

// Repointing an existing origin would send work somewhere nobody expected.
func TestPublishRefusesAnAlreadyConnectedProject(t *testing.T) {
	app, _, creates := publishApp(t, bareTarget(t))
	source := gitSourceRepo(t)
	if info := app.CloneRepository(source); info.Error != "" {
		t.Fatal(info.Error)
	}

	result := app.PublishToGitHub(PublishDraft{Name: "paper"})
	if result.Error == "" {
		t.Fatal("an already-connected project was published")
	}
	if !strings.Contains(result.Error, "already pushes to") {
		t.Errorf("unhelpful error: %q", result.Error)
	}
	if creates.Load() != 0 {
		t.Error("a repository was created on GitHub despite the refusal")
	}
}

func TestPublishNeedsAnAccount(t *testing.T) {
	withGitIdentity(t)
	app, _, creates := publishApp(t, bareTarget(t))
	if err := app.github.store.Clear(); err != nil {
		t.Fatal(err)
	}

	result := app.PublishToGitHub(PublishDraft{Name: "paper"})
	if !strings.Contains(result.Error, "connect a GitHub account") {
		t.Errorf("got %q", result.Error)
	}
	if creates.Load() != 0 {
		t.Error("a repository was created without an account")
	}
}

// Nothing may be created on GitHub until every local step has succeeded — an
// orphan repository is real state the writer then has to clean up by hand.
func TestPublishCreatesNothingWhenLocalPreparationFails(t *testing.T) {
	app, _, creates := publishApp(t, bareTarget(t))

	for _, name := range []string{"", "   ", "has spaces", "bad/slash"} {
		result := app.PublishToGitHub(PublishDraft{Name: name})
		if result.Error == "" {
			t.Errorf("%q was accepted as a repository name", name)
		}
		if result.Created {
			t.Errorf("%q reported a created repository", name)
		}
	}
	if creates.Load() != 0 {
		t.Errorf("GitHub was called %d times for names that never validated", creates.Load())
	}
}

// An existing history belongs to the writer; folding their uncommitted work
// into a publish would put changes in a commit they never wrote a message for.
func TestPublishRefusesDirtyWorkOnAnExistingHistory(t *testing.T) {
	withGitIdentity(t)
	app, dir, creates := publishApp(t, bareTarget(t))
	if err := vcs.Init(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := vcs.Commit(dir, "existing history", vcs.Identity{Name: "Test", Email: "t@e.com"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "draft.tex"), []byte("in progress\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	result := app.PublishToGitHub(PublishDraft{Name: "paper"})
	if !strings.Contains(result.Error, "commit your changes") {
		t.Errorf("got %q", result.Error)
	}
	if !strings.Contains(result.Error, "draft.tex") {
		t.Errorf("the error does not name the file: %q", result.Error)
	}
	if creates.Load() != 0 {
		t.Error("a repository was created despite uncommitted work")
	}
}

// GitHub answers a duplicate with a 422 whose useful part is buried.
func TestPublishReportsATakenName(t *testing.T) {
	withGitIdentity(t)
	app, _, _ := githubAppForTakenName(t)

	result := app.PublishToGitHub(PublishDraft{Name: "paper"})
	if !strings.Contains(result.Error, "already have a repository") {
		t.Errorf("got %q", result.Error)
	}
	if result.Created {
		t.Error("a refused creation was reported as created")
	}
}

func githubAppForTakenName(t *testing.T) (*App, string, *atomic.Int32) {
	t.Helper()
	var creates atomic.Int32
	app, store := githubApp(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/user/repos") && r.Method == http.MethodPost {
			creates.Add(1)
			w.WriteHeader(http.StatusUnprocessableEntity)
			fmt.Fprint(w, `{"message":"Repository creation failed.","errors":[{"message":"name already exists on this account"}]}`)
			return
		}
		okUser(w, r)
	})
	if err := store.Set("gho_secret"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.tex"), []byte(rootDoc), 0o644); err != nil {
		t.Fatal(err)
	}
	if info := app.OpenProject(dir); info.Error != "" {
		t.Fatal(info.Error)
	}
	return app, dir, &creates
}

func TestSuggestRepositoryName(t *testing.T) {
	for input, want := range map[string]string{
		"my-paper":       "my-paper",
		"My Paper":       "My-Paper",
		"thesis (final)": "thesis-final",
		"  spaced  ":     "spaced",
		"...":            "lotus-project",
		"":               "lotus-project",
	} {
		if got := SuggestRepositoryName(input); got != want {
			t.Errorf("%q: got %q, want %q", input, got, want)
		}
	}
}
