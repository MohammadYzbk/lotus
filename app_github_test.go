package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MohammadYzbk/lotus/internal/forge"
	"github.com/MohammadYzbk/lotus/internal/secrets"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// githubApp returns an App whose GitHub half talks to a stub server and keeps
// its token in memory, so no test touches the real keychain or the network.
func githubApp(t *testing.T, handler http.HandlerFunc) (*App, *secrets.Memory) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	client := forge.New("test-client-id")
	client.OAuthBase = server.URL
	client.APIBase = server.URL
	client.HTTP = server.Client()

	store := &secrets.Memory{}
	app := NewApp()
	app.github = &githubState{client: client, store: store, cloneRoot: t.TempDir()}
	return app, store
}

func okUser(w http.ResponseWriter, _ *http.Request) {
	fmt.Fprint(w, `{"login":"octocat","name":"The Octocat"}`)
}

// gitSourceRepo builds a real repository to clone from, laid out as
// <owner>/<name> so Slug reads a plausible pair out of its path.
func gitSourceRepo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "octocat", "paper")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	repository, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.tex"), []byte(rootDoc), 0o644); err != nil {
		t.Fatal(err)
	}
	worktree, err := repository.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worktree.Add("main.tex"); err != nil {
		t.Fatal(err)
	}
	if _, err := worktree.Commit("initial", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()},
	}); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestGitHubAccountWithoutToken(t *testing.T) {
	app, _ := githubApp(t, okUser)

	account := app.GitHubAccount()
	if account.Connected {
		t.Error("reported connected with nothing stored")
	}
	if !account.DeviceFlow {
		t.Error("device flow should be available when a client ID is set")
	}
}

// A build with no client ID must say so, rather than offering a sign-in button
// that cannot work.
func TestGitHubAccountReportsMissingClientID(t *testing.T) {
	app, _ := githubApp(t, okUser)
	app.github.client.ClientID = ""

	if app.GitHubAccount().DeviceFlow {
		t.Error("device flow was advertised without a client ID")
	}
	if login := app.GitHubStartLogin(); !strings.Contains(login.Error, "personal access token") {
		t.Errorf("unhelpful error: %q", login.Error)
	}
}

func TestGitHubUseTokenStoresAfterVerifying(t *testing.T) {
	app, store := githubApp(t, okUser)

	account := app.GitHubUseToken("  gho_secret  ")
	if account.Error != "" {
		t.Fatalf("error: %s", account.Error)
	}
	if !account.Connected || account.Login != "octocat" {
		t.Errorf("got %+v", account)
	}
	// Surrounding whitespace comes free with any paste.
	if got, _ := store.Get(); got != "gho_secret" {
		t.Errorf("stored %q, want the trimmed token", got)
	}
}

// A mistyped token must fail where it was typed, not at the first clone.
func TestGitHubUseTokenRejectsBadToken(t *testing.T) {
	app, store := githubApp(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"message":"Bad credentials"}`)
	})

	account := app.GitHubUseToken("nope")
	if account.Connected {
		t.Error("a rejected token was reported as connected")
	}
	if account.Error == "" {
		t.Error("no explanation given")
	}
	if _, err := store.Get(); err == nil {
		t.Error("a rejected token was stored anyway")
	}
}

func TestGitHubUseTokenRejectsEmpty(t *testing.T) {
	app, _ := githubApp(t, okUser)
	if account := app.GitHubUseToken("   "); account.Connected {
		t.Error("an empty token connected")
	}
}

func TestGitHubDisconnectClearsTheToken(t *testing.T) {
	app, store := githubApp(t, okUser)
	if err := store.Set("gho_secret"); err != nil {
		t.Fatal(err)
	}

	if account := app.GitHubDisconnect(); account.Connected {
		t.Error("still connected after disconnecting")
	}
	if _, err := store.Get(); err == nil {
		t.Error("the token survived disconnect")
	}
}

func TestGitHubRepositoriesNeedsAnAccount(t *testing.T) {
	app, _ := githubApp(t, okUser)
	if list := app.GitHubRepositories(); list.Error == "" {
		t.Error("listed repositories without a connected account")
	}
}

func TestGitHubRepositoriesSortsNewestFirst(t *testing.T) {
	app, store := githubApp(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/user") {
			okUser(w, r)
			return
		}
		fmt.Fprint(w, `[
			{"full_name":"me/old","clone_url":"https://x/old.git","updated_at":"2020-01-01T00:00:00Z"},
			{"full_name":"me/new","clone_url":"https://x/new.git","updated_at":"2026-01-01T00:00:00Z"}
		]`)
	})
	if err := store.Set("gho_secret"); err != nil {
		t.Fatal(err)
	}

	list := app.GitHubRepositories()
	if list.Error != "" {
		t.Fatalf("error: %s", list.Error)
	}
	if len(list.Repositories) != 2 {
		t.Fatalf("got %d repositories", len(list.Repositories))
	}
	if list.Repositories[0].FullName != "me/new" {
		t.Errorf("order: got %q first, want the most recently updated", list.Repositories[0].FullName)
	}
}

// The clone lands under the managed directory, keyed by owner and name, so two
// repositories with the same name never collide.
func TestCloneRepositoryLaysOutByOwnerAndName(t *testing.T) {
	app, _ := githubApp(t, okUser)
	source := gitSourceRepo(t)

	info := app.CloneRepository(source)
	if info.Error != "" {
		t.Fatalf("clone failed: %s", info.Error)
	}

	owner, name := filepath.Base(filepath.Dir(source)), filepath.Base(source)
	want := filepath.Join(app.github.cloneRoot, owner, name)
	if info.Root != want {
		t.Errorf("cloned to %q, want %q", info.Root, want)
	}
	if _, err := os.Stat(filepath.Join(want, "main.tex")); err != nil {
		t.Errorf("the working copy has no content: %v", err)
	}
}

// Picking the same repository twice should return you to your work, not fail
// because the directory is in the way.
func TestCloneRepositoryReopensAnExistingClone(t *testing.T) {
	app, _ := githubApp(t, okUser)
	source := gitSourceRepo(t)

	first := app.CloneRepository(source)
	if first.Error != "" {
		t.Fatalf("first clone: %s", first.Error)
	}
	// A local edit proves the second call reused the clone instead of
	// replacing it.
	scratch := filepath.Join(first.Root, "notes.tex")
	if err := os.WriteFile(scratch, []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	second := app.CloneRepository(source)
	if second.Error != "" {
		t.Fatalf("second clone: %s", second.Error)
	}
	if second.Root != first.Root {
		t.Errorf("reopened %q, want %q", second.Root, first.Root)
	}
	if _, err := os.Stat(scratch); err != nil {
		t.Errorf("the existing clone was replaced, losing local work: %v", err)
	}
}

func TestCloneRepositoryRejectsNonsenseURL(t *testing.T) {
	app, _ := githubApp(t, okUser)
	if info := app.CloneRepository("not-a-url"); info.Error == "" {
		t.Error("a nonsense URL was accepted")
	}
}

// A plain folder is a perfectly normal project; the Git panel simply has
// nothing to say about it.
func TestRepositoryStateOnPlainProject(t *testing.T) {
	app, _ := githubApp(t, okUser)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.tex"), []byte(rootDoc), 0o644); err != nil {
		t.Fatal(err)
	}
	if info := app.OpenProject(dir); info.Error != "" {
		t.Fatalf("open: %s", info.Error)
	}

	if state := app.RepositoryState(); state.Repository {
		t.Error("a plain folder was reported as a repository")
	}
}

func TestRepositoryStateAfterClone(t *testing.T) {
	app, _ := githubApp(t, okUser)
	source := gitSourceRepo(t)
	if info := app.CloneRepository(source); info.Error != "" {
		t.Fatalf("clone: %s", info.Error)
	}

	state := app.RepositoryState()
	if !state.Repository {
		t.Fatal("a clone was not recognised as a repository")
	}
	if state.Branch == "" {
		t.Error("no branch reported")
	}
	if state.Dirty {
		t.Error("a fresh clone was reported dirty")
	}
}
