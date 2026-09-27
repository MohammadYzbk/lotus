package main

// The branch-based edit workflow, bound for the frontend: branch, commit,
// push, and open a pull request.
//
// Every one of these is something the writer asked for explicitly. The app
// autosaves to disk continuously, but nothing reaches history or a remote
// without a deliberate action — so none of this runs on a timer or as a side
// effect of compiling.

import (
	"errors"
	"fmt"
	"strings"

	"github.com/MohammadYzbk/lotus/internal/forge"
	"github.com/MohammadYzbk/lotus/internal/vcs"
)

// GitResult carries the working copy's state after an operation, so the UI
// refreshes from the same call that changed something.
type GitResult struct {
	State vcs.State `json:"state"`
	// Detail is a short note worth showing on success, such as the new commit.
	Detail string `json:"detail"`
	Error  string `json:"error"`
}

// BranchList is the branch picker's contents.
type BranchList struct {
	vcs.Branches
	Error string `json:"error"`
}

// PullRequestResult is the outcome of asking for a pull request.
type PullRequestResult struct {
	PullRequest forge.PullRequest `json:"pullRequest"`
	Error       string            `json:"error"`
}

// projectRoot returns the open project's directory, or an error message.
func (a *App) projectRoot() (string, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.proj == nil {
		return "", "no project is open"
	}
	return a.proj.Root(), ""
}

// gitResult builds a result carrying the current state, plus err if non-nil.
func (a *App) gitResult(root string, detail string, err error) GitResult {
	result := GitResult{State: vcs.Status(root), Detail: detail}
	if err != nil {
		result.Error = err.Error()
	}
	return result
}

// RepositoryBranches lists the local branches.
func (a *App) RepositoryBranches() BranchList {
	root, problem := a.projectRoot()
	if problem != "" {
		return BranchList{Error: problem}
	}
	branches, err := vcs.ListBranches(root)
	if err != nil {
		return BranchList{Error: err.Error()}
	}
	return BranchList{Branches: branches}
}

// CreateBranch starts a branch at the current commit and checks it out.
func (a *App) CreateBranch(name string) GitResult {
	root, problem := a.projectRoot()
	if problem != "" {
		return GitResult{Error: problem}
	}

	name = strings.TrimSpace(name)
	if err := vcs.CreateBranch(root, name); err != nil {
		return a.gitResult(root, "", err)
	}
	return a.gitResult(root, fmt.Sprintf("Now on %s", name), nil)
}

// SwitchBranch checks out an existing branch.
//
// It refuses while there are uncommitted changes, and names them: "there are
// uncommitted changes" leaves the writer hunting for which.
func (a *App) SwitchBranch(name string) GitResult {
	root, problem := a.projectRoot()
	if problem != "" {
		return GitResult{Error: problem}
	}

	err := vcs.SwitchBranch(root, strings.TrimSpace(name))
	if errors.Is(err, vcs.ErrDirty) {
		files := vcs.DirtyFiles(root)
		return a.gitResult(root, "", fmt.Errorf(
			"commit your changes before switching branches — %s", describeFiles(files)))
	}
	if err != nil {
		return a.gitResult(root, "", err)
	}

	// The open file may not exist on the new branch, and the tree certainly
	// changed underneath the editor.
	a.mu.Lock()
	a.invalidateDiagnosticManifestLocked()
	a.mu.Unlock()

	return a.gitResult(root, fmt.Sprintf("Now on %s", name), nil)
}

// CommitChanges records everything currently uncommitted.
func (a *App) CommitChanges(message string) GitResult {
	root, problem := a.projectRoot()
	if problem != "" {
		return GitResult{Error: problem}
	}

	who, err := vcs.ConfiguredIdentity(root)
	if errors.Is(err, vcs.ErrNoIdentity) {
		return a.gitResult(root, "", errors.New(
			"Git has no name and email configured, so a commit cannot be attributed. "+
				"Set them with: git config --global user.name \"…\" and user.email \"…\""))
	}
	if err != nil {
		return a.gitResult(root, "", err)
	}

	hash, err := vcs.Commit(root, message, who)
	if errors.Is(err, vcs.ErrNoChanges) {
		return a.gitResult(root, "", errors.New("there is nothing to commit"))
	}
	if err != nil {
		return a.gitResult(root, "", err)
	}

	short := hash
	if len(short) > 7 {
		short = short[:7]
	}
	return a.gitResult(root, fmt.Sprintf("Committed %s", short), nil)
}

// PushBranch publishes the current branch to origin.
func (a *App) PushBranch() GitResult {
	root, problem := a.projectRoot()
	if problem != "" {
		return GitResult{Error: problem}
	}

	state := vcs.Status(root)
	if !state.Repository {
		return a.gitResult(root, "", errors.New("this project is not a Git repository"))
	}
	if state.Branch == "" {
		return a.gitResult(root, "", errors.New("there is no branch to push"))
	}
	if state.Remote == "" {
		return a.gitResult(root, "", errors.New("this repository has no origin to push to"))
	}

	if err := vcs.Push(a.context(), root, state.Branch, a.github.token()); err != nil {
		return a.gitResult(root, "", err)
	}
	return a.gitResult(root, fmt.Sprintf("Pushed %s", state.Branch), nil)
}

// OpenPullRequest opens a pull request for the current branch, or returns the
// one already open for it.
//
// It does not push on the writer's behalf. Publishing a branch is its own
// deliberate action, and doing it silently inside "open a pull request" is how
// work reaches a remote before someone meant it to.
func (a *App) OpenPullRequest(title, body string) PullRequestResult {
	root, problem := a.projectRoot()
	if problem != "" {
		return PullRequestResult{Error: problem}
	}

	state := vcs.Status(root)
	switch {
	case !state.Repository:
		return PullRequestResult{Error: "this project is not a Git repository"}
	case state.Branch == "":
		return PullRequestResult{Error: "there is no branch to open a pull request from"}
	case state.Remote == "":
		return PullRequestResult{Error: "this repository has no origin"}
	case state.Dirty:
		return PullRequestResult{Error: "commit your changes first — a pull request only shows what has been committed"}
	case !vcs.HasUpstream(root, state.Branch):
		return PullRequestResult{Error: fmt.Sprintf("push %s first, so GitHub has the branch to compare", state.Branch)}
	}

	token := a.github.token()
	if token == "" {
		return PullRequestResult{Error: "connect a GitHub account first"}
	}

	owner, name, err := vcs.Slug(state.Remote)
	if err != nil {
		return PullRequestResult{Error: err.Error()}
	}

	// An already-open pull request is the answer to "open a pull request" just
	// as much as a new one is.
	if existing, found, err := a.github.client.OpenPullRequestFor(a.context(), token, owner, name, state.Branch); err != nil {
		return PullRequestResult{Error: err.Error()}
	} else if found {
		return PullRequestResult{PullRequest: existing}
	}

	info, err := a.github.client.Repository(a.context(), token, owner, name)
	if err != nil {
		return PullRequestResult{Error: err.Error()}
	}
	base := info.DefaultBranch
	if base == "" {
		base = "main"
	}
	if base == state.Branch {
		return PullRequestResult{Error: fmt.Sprintf(
			"%s is the default branch, so there is nothing to merge it into — create a branch first", base)}
	}

	title = strings.TrimSpace(title)
	if title == "" {
		title = state.Branch
	}

	pr, err := a.github.client.CreatePullRequest(a.context(), token, owner, name, forge.PullRequestDraft{
		Title: title,
		Body:  strings.TrimSpace(body),
		Head:  state.Branch,
		Base:  base,
	})
	if err != nil {
		return PullRequestResult{Error: err.Error()}
	}
	return PullRequestResult{PullRequest: pr}
}

// describeFiles names what is in the way, without listing forty of them.
func describeFiles(files []string) string {
	switch {
	case len(files) == 0:
		return "no files reported"
	case len(files) == 1:
		return files[0] + " is uncommitted"
	case len(files) <= 3:
		return strings.Join(files, ", ") + " are uncommitted"
	default:
		return fmt.Sprintf("%s and %d more are uncommitted", strings.Join(files[:3], ", "), len(files)-3)
	}
}
