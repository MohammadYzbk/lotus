package main

// Publishing a project that is not yet under version control.
//
// This is the missing direction. Everything else in the app assumes you began
// from a repository and cloned it; this takes a folder you have already been
// writing in, makes it a repository, creates its counterpart on GitHub, and
// pushes. From then on the ordinary workflow — branch, commit, push, pull
// request, fetch, pull — applies unchanged.

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/MohammadYzbk/lotus/internal/forge"
	"github.com/MohammadYzbk/lotus/internal/vcs"
)

// PublishDraft is what the writer filled in.
type PublishDraft struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Private is chosen by the writer, and the UI defaults it to true: a draft
	// paper is not something to put in front of the world by accident.
	Private bool `json:"private"`
}

// PublishResult reports the outcome, and enough state for the UI to catch up.
type PublishResult struct {
	Repository forge.Repository `json:"repository"`
	State      vcs.State        `json:"state"`
	Detail     string           `json:"detail"`
	Error      string           `json:"error"`
	// Created is true when the repository exists on GitHub even though the
	// overall attempt failed — a push that did not land leaves real state
	// behind, and telling the writer to retry the push beats telling them
	// nothing happened.
	Created bool `json:"created"`
}

// PublishState tells the UI whether publishing is even on the table.
type PublishState struct {
	// Publishable is true for an open project that is not yet connected.
	Publishable bool `json:"publishable"`
	// Connected is true when the project already has an origin.
	Connected bool `json:"connected"`
	// SuggestedName is the folder's own name, which is almost always right.
	SuggestedName string `json:"suggestedName"`
	Reason        string `json:"reason"`
}

// repositoryNamePattern matches what GitHub accepts. It normalises rather than
// merely rejecting, because a project folder is often "My Paper" and refusing
// that would send the writer away to rename a directory.
var repositoryNamePattern = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// SuggestRepositoryName turns a folder name into a usable repository name.
func SuggestRepositoryName(folder string) string {
	name := repositoryNamePattern.ReplaceAllString(strings.TrimSpace(folder), "-")
	name = strings.Trim(name, "-._")
	if name == "" {
		return "lotus-project"
	}
	if len(name) > 100 {
		name = strings.Trim(name[:100], "-._")
	}
	return name
}

// ValidateRepositoryName reports why a name cannot be used, or nil.
func ValidateRepositoryName(name string) error {
	switch {
	case strings.TrimSpace(name) == "":
		return errors.New("a repository needs a name")
	case name != strings.TrimSpace(name):
		return errors.New("a repository name cannot start or end with a space")
	case repositoryNamePattern.MatchString(name):
		return errors.New("use letters, digits, and . _ - only")
	case name == "." || name == "..":
		return errors.New("that name is reserved")
	case len(name) > 100:
		return errors.New("that name is too long")
	}
	return nil
}

// PublishStatus reports whether the open project can be published.
func (a *App) PublishStatus() PublishState {
	root, problem := a.projectRoot()
	if problem != "" {
		return PublishState{Reason: problem}
	}

	state := PublishState{SuggestedName: SuggestRepositoryName(filepath.Base(root))}
	current := vcs.Status(root)
	switch {
	case current.Remote != "":
		state.Connected = true
		state.Reason = fmt.Sprintf("Already connected to %s", current.Remote)
	default:
		state.Publishable = true
	}
	return state
}

// PublishToGitHub creates a repository for the open project and pushes to it.
//
// The order is chosen so that the irreversible step happens as late as
// possible: everything local is prepared first, and only then is anything
// created on GitHub. A failure before that point leaves no orphan repository
// behind.
func (a *App) PublishToGitHub(draft PublishDraft) PublishResult {
	root, problem := a.projectRoot()
	if problem != "" {
		return PublishResult{Error: problem}
	}

	name := strings.TrimSpace(draft.Name)
	if err := ValidateRepositoryName(name); err != nil {
		return PublishResult{Error: err.Error()}
	}

	// Refuse before touching anything if the project is already connected;
	// repointing an origin silently is how work ends up somewhere nobody meant.
	if existing := vcs.Status(root); existing.Remote != "" {
		return PublishResult{Error: fmt.Sprintf(
			"this project already pushes to %s — publishing would point it somewhere else", existing.Remote)}
	}

	token := a.github.token()
	if token == "" {
		return PublishResult{Error: "connect a GitHub account first"}
	}

	// --- local preparation, all reversible -----------------------------------

	if !vcs.IsRepository(root) {
		if err := vcs.Init(root); err != nil {
			return PublishResult{Error: err.Error()}
		}
	}

	if !vcs.HasCommits(root) {
		who, err := vcs.ConfiguredIdentity(root)
		if errors.Is(err, vcs.ErrNoIdentity) {
			return PublishResult{Error: "Git has no name and email configured, so the first commit " +
				"cannot be attributed. Set them with: git config --global user.name \"…\" and user.email \"…\""}
		}
		if err != nil {
			return PublishResult{Error: err.Error()}
		}
		if _, err := vcs.Commit(root, "Initial commit", who); err != nil {
			if errors.Is(err, vcs.ErrNoChanges) {
				return PublishResult{Error: "there is nothing in this project to publish yet"}
			}
			return PublishResult{Error: err.Error()}
		}
	} else if dirty := vcs.DirtyFiles(root); len(dirty) > 0 {
		// An existing history is the writer's, and quietly folding their
		// uncommitted work into a publish would put changes in a commit they
		// did not write a message for.
		return PublishResult{Error: fmt.Sprintf(
			"commit your changes before publishing — %s", describeFiles(dirty))}
	}

	branch := vcs.Status(root).Branch
	if branch == "" {
		return PublishResult{Error: "the project has no branch to publish"}
	}

	// --- the irreversible step ------------------------------------------------

	repository, err := a.github.client.CreateRepository(a.context(), token, forge.NewRepository{
		Name:        name,
		Description: strings.TrimSpace(draft.Description),
		Private:     draft.Private,
	})
	if err != nil {
		if errors.Is(err, forge.ErrNameTaken) {
			return PublishResult{Error: fmt.Sprintf(
				"you already have a repository called %s — pick another name", name)}
		}
		return PublishResult{Error: err.Error()}
	}

	// From here the repository exists, so every failure reports Created so the
	// writer knows what is already out there.
	if err := vcs.SetRemote(root, "origin", repository.CloneURL); err != nil {
		return PublishResult{
			Repository: repository, Created: true, State: vcs.Status(root),
			Error: fmt.Sprintf("%s was created, but origin could not be set: %v", repository.FullName, err),
		}
	}

	if err := vcs.Push(a.context(), root, branch, token); err != nil {
		return PublishResult{
			Repository: repository, Created: true, State: vcs.Status(root),
			Error: fmt.Sprintf("%s was created and connected, but the push failed: %v. Press Push to retry",
				repository.FullName, err),
		}
	}

	return PublishResult{
		Repository: repository,
		State:      vcs.Status(root),
		Detail:     fmt.Sprintf("Published %s to %s", branch, repository.FullName),
	}
}
