package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MohammadYzbk/lotus/internal/vcs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
)

// gitApp returns an App whose project is a real repository and whose GitHub
// half talks to a stub, so the whole workflow runs without a network.
func gitApp(t *testing.T, handler http.HandlerFunc) *App {
	t.Helper()
	app, _ := githubApp(t, handler)
	source := gitSourceRepo(t)
	if info := app.CloneRepository(source); info.Error != "" {
		t.Fatalf("clone: %s", info.Error)
	}
	return app
}

// identify writes an author into the repository's own config, so a commit does
// not depend on whatever global Git identity the machine happens to have.
func identify(t *testing.T, root string) {
	t.Helper()
	repository, err := git.PlainOpen(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := repository.Config()
	if err != nil {
		t.Fatal(err)
	}
	cfg.User.Name = "Test"
	cfg.User.Email = "test@example.com"
	if err := repository.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
}

func projectDir(t *testing.T, app *App) string {
	t.Helper()
	root, problem := app.projectRoot()
	if problem != "" {
		t.Fatal(problem)
	}
	return root
}

func TestCreateAndSwitchBranchThroughTheApp(t *testing.T) {
	app := gitApp(t, okUser)
	before := app.RepositoryBranches()
	if before.Error != "" {
		t.Fatal(before.Error)
	}

	created := app.CreateBranch("feature/zoom")
	if created.Error != "" {
		t.Fatalf("create: %s", created.Error)
	}
	if created.State.Branch != "feature/zoom" {
		t.Errorf("state after create: %+v", created.State)
	}

	back := app.SwitchBranch(before.Current)
	if back.Error != "" {
		t.Fatalf("switch: %s", back.Error)
	}
	if back.State.Branch != before.Current {
		t.Errorf("state after switch: %+v", back.State)
	}
}

// "There are uncommitted changes" leaves the writer hunting; the message has to
// say which files are in the way.
func TestSwitchBranchNamesTheFilesInTheWay(t *testing.T) {
	app := gitApp(t, okUser)
	root := projectDir(t, app)
	start := app.RepositoryBranches().Current

	if result := app.CreateBranch("other"); result.Error != "" {
		t.Fatal(result.Error)
	}
	if err := os.WriteFile(filepath.Join(root, "main.tex"), []byte("edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	result := app.SwitchBranch(start)
	if result.Error == "" {
		t.Fatal("the switch was allowed over uncommitted changes")
	}
	if !strings.Contains(result.Error, "main.tex") {
		t.Errorf("error does not name the file: %s", result.Error)
	}
	if result.State.Branch != "other" {
		t.Errorf("the branch changed anyway: %+v", result.State)
	}
}

func TestCommitThroughTheApp(t *testing.T) {
	app := gitApp(t, okUser)
	root := projectDir(t, app)
	identify(t, root)

	if err := os.WriteFile(filepath.Join(root, "main.tex"), []byte("edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	result := app.CommitChanges("describe it")
	if result.Error != "" {
		t.Fatalf("commit: %s", result.Error)
	}
	if result.State.Dirty {
		t.Error("still dirty after committing")
	}
	if !strings.HasPrefix(result.Detail, "Committed ") {
		t.Errorf("detail: got %q", result.Detail)
	}
}

func TestCommitWithNothingToDoSaysSo(t *testing.T) {
	app := gitApp(t, okUser)
	identify(t, projectDir(t, app))

	if result := app.CommitChanges("nothing"); !strings.Contains(result.Error, "nothing to commit") {
		t.Errorf("got %q", result.Error)
	}
}

// A commit with no author lands in history unattributable and cannot be fixed
// without a rewrite, so the app explains how to set one instead.
func TestCommitWithoutAnIdentityExplainsHowToSetOne(t *testing.T) {
	// Git resolves an identity from global config as well as the repository's
	// own, so the developer's real ~/.gitconfig would otherwise satisfy this and
	// the test would quietly skip itself. Pointing HOME at an empty directory is
	// what makes "no identity anywhere" reproducible.
	empty := t.TempDir()
	t.Setenv("HOME", empty)
	t.Setenv("XDG_CONFIG_HOME", empty)

	app := gitApp(t, okUser)
	root := projectDir(t, app)

	if who, err := vcs.ConfiguredIdentity(root); err == nil {
		t.Fatalf("an identity leaked in despite an empty HOME: %+v", who)
	}
	if err := os.WriteFile(filepath.Join(root, "main.tex"), []byte("edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	result := app.CommitChanges("message")
	if result.Error == "" {
		t.Fatal("an unattributable commit was allowed")
	}
	if !strings.Contains(result.Error, "git config") {
		t.Errorf("the error does not say how to fix it: %q", result.Error)
	}
}

func TestPushWithoutARemoteSaysSo(t *testing.T) {
	app := gitApp(t, okUser)
	root := projectDir(t, app)

	repository, err := git.PlainOpen(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.DeleteRemote("origin"); err != nil {
		t.Fatal(err)
	}

	if result := app.PushBranch(); !strings.Contains(result.Error, "no origin") {
		t.Errorf("got %q", result.Error)
	}
}

// --- pull requests ---------------------------------------------------------------

// githubRemote repoints origin at a GitHub URL and marks the branch as pushed,
// so the pull-request path can run without a network.
func githubRemote(t *testing.T, root, branch string) {
	t.Helper()
	repository, err := git.PlainOpen(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.DeleteRemote("origin"); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.CreateRemote(&config.RemoteConfig{
		Name: "origin",
		URLs: []string{"https://github.com/octocat/paper.git"},
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err := repository.Config()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Branches = map[string]*config.Branch{
		branch: {Name: branch, Remote: "origin", Merge: plumbing.NewBranchReferenceName(branch)},
	}
	if err := repository.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
}

func TestOpenPullRequestCreatesOne(t *testing.T) {
	app := gitApp(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/user"):
			okUser(w, r)
		case strings.HasSuffix(r.URL.Path, "/pulls") && r.Method == http.MethodGet:
			fmt.Fprint(w, `[]`)
		case strings.HasSuffix(r.URL.Path, "/pulls") && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"number":3,"html_url":"https://github.com/octocat/paper/pull/3","title":"t"}`)
		default:
			fmt.Fprint(w, `{"full_name":"octocat/paper","default_branch":"main"}`)
		}
	})
	root := projectDir(t, app)
	identify(t, root)
	if result := app.CreateBranch("feature/zoom"); result.Error != "" {
		t.Fatal(result.Error)
	}
	githubRemote(t, root, "feature/zoom")
	app.github.store.Set("gho_secret")

	result := app.OpenPullRequest("Zoom the preview", "why")
	if result.Error != "" {
		t.Fatalf("open: %s", result.Error)
	}
	if result.PullRequest.URL != "https://github.com/octocat/paper/pull/3" {
		t.Errorf("got %+v", result.PullRequest)
	}
}

// Pressing the button twice must hand back the open one, not an error.
func TestOpenPullRequestReturnsAnExistingOne(t *testing.T) {
	app := gitApp(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/user") {
			okUser(w, r)
			return
		}
		if r.Method == http.MethodPost {
			t.Error("a second pull request was created instead of reusing the open one")
		}
		fmt.Fprint(w, `[{"number":3,"html_url":"https://github.com/octocat/paper/pull/3","title":"Already"}]`)
	})
	root := projectDir(t, app)
	identify(t, root)
	if result := app.CreateBranch("feature/zoom"); result.Error != "" {
		t.Fatal(result.Error)
	}
	githubRemote(t, root, "feature/zoom")
	app.github.store.Set("gho_secret")

	result := app.OpenPullRequest("", "")
	if result.Error != "" {
		t.Fatalf("open: %s", result.Error)
	}
	if !result.PullRequest.Existing {
		t.Error("the existing pull request was not reported as existing")
	}
}

// The guards, in the order a writer would trip over them.
func TestOpenPullRequestGuards(t *testing.T) {
	t.Run("uncommitted changes", func(t *testing.T) {
		app := gitApp(t, okUser)
		root := projectDir(t, app)
		if err := os.WriteFile(filepath.Join(root, "main.tex"), []byte("edited\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if result := app.OpenPullRequest("t", ""); !strings.Contains(result.Error, "commit your changes") {
			t.Errorf("got %q", result.Error)
		}
	})

	t.Run("branch never pushed", func(t *testing.T) {
		app := gitApp(t, okUser)
		root := projectDir(t, app)
		identify(t, root)
		if result := app.CreateBranch("feature/zoom"); result.Error != "" {
			t.Fatal(result.Error)
		}
		if result := app.OpenPullRequest("t", ""); !strings.Contains(result.Error, "push") {
			t.Errorf("got %q", result.Error)
		}
	})

	t.Run("no account connected", func(t *testing.T) {
		app := gitApp(t, okUser)
		root := projectDir(t, app)
		identify(t, root)
		if result := app.CreateBranch("feature/zoom"); result.Error != "" {
			t.Fatal(result.Error)
		}
		githubRemote(t, root, "feature/zoom")
		if result := app.OpenPullRequest("t", ""); !strings.Contains(result.Error, "connect a GitHub account") {
			t.Errorf("got %q", result.Error)
		}
	})

	t.Run("already on the default branch", func(t *testing.T) {
		app := gitApp(t, func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/user") {
				okUser(w, r)
				return
			}
			if strings.HasSuffix(r.URL.Path, "/pulls") {
				fmt.Fprint(w, `[]`)
				return
			}
			fmt.Fprint(w, `{"full_name":"octocat/paper","default_branch":"master"}`)
		})
		root := projectDir(t, app)
		identify(t, root)
		current := app.RepositoryBranches().Current
		githubRemote(t, root, current)
		app.github.store.Set("gho_secret")

		result := app.OpenPullRequest("t", "")
		if !strings.Contains(result.Error, "nothing to merge it into") {
			t.Errorf("got %q", result.Error)
		}
	})
}
