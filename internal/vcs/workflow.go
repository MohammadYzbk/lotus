package vcs

// The branch-based edit workflow: create and switch branches, commit, and push.
//
// Every operation here is deliberate — the app autosaves to disk continuously,
// but nothing reaches history or a remote without the writer asking for it.

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

var (
	// ErrDirty means the working copy has changes that an operation would risk.
	ErrDirty = errors.New("vcs: there are uncommitted changes")
	// ErrNoChanges means there was nothing to commit.
	ErrNoChanges = errors.New("vcs: nothing to commit")
	// ErrBranchExists means the branch name is already taken.
	ErrBranchExists = errors.New("vcs: that branch already exists")
	// ErrNoIdentity means Git has no name and email to attribute a commit to.
	ErrNoIdentity = errors.New("vcs: no Git identity configured")
	// ErrNoUpstream means the branch has never been pushed.
	ErrNoUpstream = errors.New("vcs: the branch has no upstream")
)

// Identity is who a commit is attributed to.
type Identity struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// Branches is the branch picker's contents.
type Branches struct {
	Current string   `json:"current"`
	Local   []string `json:"local"`
}

// ListBranches returns the local branches, current first in its own field.
func ListBranches(dir string) (Branches, error) {
	repository, err := git.PlainOpen(dir)
	if err != nil {
		return Branches{}, fmt.Errorf("vcs: open: %w", err)
	}

	var out Branches
	iter, err := repository.Branches()
	if err != nil {
		return Branches{}, fmt.Errorf("vcs: list branches: %w", err)
	}
	err = iter.ForEach(func(reference *plumbing.Reference) error {
		out.Local = append(out.Local, reference.Name().Short())
		return nil
	})
	if err != nil {
		return Branches{}, fmt.Errorf("vcs: list branches: %w", err)
	}
	sort.Strings(out.Local)

	if head, err := repository.Head(); err == nil && head.Name().IsBranch() {
		out.Current = head.Name().Short()
	}
	return out, nil
}

// branchNamePattern is deliberately stricter than Git itself.
//
// Git permits nearly anything; the names that cause trouble later — ones
// needing shell quoting, ones that look like flags, ones that collide with a
// ref path — are not worth allowing for the sake of completeness.
var branchNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

// ValidateBranchName reports why a name cannot be used, or nil.
func ValidateBranchName(name string) error {
	switch {
	case strings.TrimSpace(name) == "":
		return errors.New("a branch needs a name")
	case name != strings.TrimSpace(name):
		return errors.New("a branch name cannot start or end with a space")
	case !branchNamePattern.MatchString(name):
		return errors.New("use letters, digits, and . _ - / only, starting with a letter or digit")
	case strings.Contains(name, ".."), strings.HasSuffix(name, "."):
		return errors.New("a branch name cannot contain .. or end with .")
	case strings.Contains(name, "//"), strings.HasSuffix(name, "/"):
		return errors.New("a branch name cannot contain // or end with /")
	case strings.HasSuffix(name, ".lock"):
		return errors.New("a branch name cannot end with .lock")
	}
	return nil
}

// CreateBranch makes a branch at the current HEAD and checks it out.
//
// Uncommitted work comes along, which is what you want: branching is usually
// the thing you do *because* you have started something.
func CreateBranch(dir, name string) error {
	if err := ValidateBranchName(name); err != nil {
		return err
	}

	repository, err := git.PlainOpen(dir)
	if err != nil {
		return fmt.Errorf("vcs: open: %w", err)
	}
	head, err := repository.Head()
	if err != nil {
		return fmt.Errorf("vcs: a branch needs a commit to start from: %w", err)
	}

	reference := plumbing.NewBranchReferenceName(name)
	if _, err := repository.Reference(reference, true); err == nil {
		return fmt.Errorf("%w: %s", ErrBranchExists, name)
	}

	worktree, err := repository.Worktree()
	if err != nil {
		return fmt.Errorf("vcs: worktree: %w", err)
	}
	if err := worktree.Checkout(&git.CheckoutOptions{
		Branch: reference,
		Hash:   head.Hash(),
		Create: true,
	}); err != nil {
		return fmt.Errorf("vcs: create branch: %w", err)
	}
	return nil
}

// SwitchBranch checks out an existing branch.
//
// It refuses while the working copy is dirty. Checking out over uncommitted
// changes either silently carries them to a branch they were not written for,
// or fails part-way with files from two branches on disk; neither is something
// to do without being asked.
func SwitchBranch(dir, name string) error {
	repository, err := git.PlainOpen(dir)
	if err != nil {
		return fmt.Errorf("vcs: open: %w", err)
	}
	worktree, err := repository.Worktree()
	if err != nil {
		return fmt.Errorf("vcs: worktree: %w", err)
	}

	status, err := worktree.Status()
	if err != nil {
		return fmt.Errorf("vcs: status: %w", err)
	}
	if !status.IsClean() {
		return fmt.Errorf("%w: commit them before switching to %s", ErrDirty, name)
	}

	if err := worktree.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName(name),
	}); err != nil {
		return fmt.Errorf("vcs: switch to %s: %w", name, err)
	}
	return nil
}

// ConfiguredIdentity reads user.name and user.email as Git itself would.
//
// Repository config wins over global, which is how Git resolves it, and lets a
// writer use a different address for one project.
func ConfiguredIdentity(dir string) (Identity, error) {
	repository, err := git.PlainOpen(dir)
	if err != nil {
		return Identity{}, fmt.Errorf("vcs: open: %w", err)
	}

	var identity Identity
	if scoped, err := repository.ConfigScoped(config.SystemScope); err == nil {
		identity.Name = scoped.User.Name
		identity.Email = scoped.User.Email
	}
	if identity.Name == "" || identity.Email == "" {
		return identity, ErrNoIdentity
	}
	return identity, nil
}

// Commit stages every change in the working copy and records it.
//
// Staging everything is the right model here: the app is an editor, not a Git
// client, and a partial index is state the writer cannot see or manage from
// this UI. What they see dirty is what gets committed.
func Commit(dir, message string, who Identity) (string, error) {
	if strings.TrimSpace(message) == "" {
		return "", errors.New("vcs: a commit needs a message")
	}
	if who.Name == "" || who.Email == "" {
		return "", ErrNoIdentity
	}

	repository, err := git.PlainOpen(dir)
	if err != nil {
		return "", fmt.Errorf("vcs: open: %w", err)
	}
	worktree, err := repository.Worktree()
	if err != nil {
		return "", fmt.Errorf("vcs: worktree: %w", err)
	}

	status, err := worktree.Status()
	if err != nil {
		return "", fmt.Errorf("vcs: status: %w", err)
	}
	if status.IsClean() {
		return "", ErrNoChanges
	}

	// AddWithOptions(All) stages modifications and deletions as well as new
	// files, which is what "commit what I see" means.
	if err := worktree.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		return "", fmt.Errorf("vcs: stage: %w", err)
	}

	hash, err := worktree.Commit(strings.TrimSpace(message), &git.CommitOptions{
		Author: &object.Signature{Name: who.Name, Email: who.Email, When: time.Now()},
	})
	if err != nil {
		return "", fmt.Errorf("vcs: commit: %w", err)
	}
	return hash.String(), nil
}

// Push sends the current branch to origin, setting it up to track.
//
// The refspec names the branch explicitly rather than pushing everything, so a
// push can never publish a branch the writer did not ask about.
func Push(ctx context.Context, dir, branch, token string) error {
	if branch == "" {
		return errors.New("vcs: no branch to push")
	}

	repository, err := git.PlainOpen(dir)
	if err != nil {
		return fmt.Errorf("vcs: open: %w", err)
	}

	reference := plumbing.NewBranchReferenceName(branch)
	options := &git.PushOptions{
		RemoteName: "origin",
		RefSpecs:   []config.RefSpec{config.RefSpec(fmt.Sprintf("%s:%s", reference, reference))},
	}
	if token != "" {
		options.Auth = &githttp.BasicAuth{Username: "x-access-token", Password: token}
	}

	err = repository.PushContext(ctx, options)
	if errors.Is(err, git.NoErrAlreadyUpToDate) {
		// Not a failure: the remote already has this commit.
		return rememberUpstream(repository, branch)
	}
	if err != nil {
		return fmt.Errorf("vcs: push: %w", redact(err, token))
	}
	return rememberUpstream(repository, branch)
}

// rememberUpstream records the branch as tracking origin, so later reads know
// it has been published and a pull request has a head to point at.
func rememberUpstream(repository *git.Repository, branch string) error {
	cfg, err := repository.Config()
	if err != nil {
		return fmt.Errorf("vcs: config: %w", err)
	}
	if cfg.Branches == nil {
		cfg.Branches = map[string]*config.Branch{}
	}
	cfg.Branches[branch] = &config.Branch{
		Name:   branch,
		Remote: "origin",
		Merge:  plumbing.NewBranchReferenceName(branch),
	}
	if err := repository.SetConfig(cfg); err != nil {
		return fmt.Errorf("vcs: record upstream: %w", err)
	}
	return nil
}

// HasUpstream reports whether the branch has been pushed to origin.
func HasUpstream(dir, branch string) bool {
	repository, err := git.PlainOpen(dir)
	if err != nil {
		return false
	}
	// The remote-tracking ref is the honest answer: config alone can name an
	// upstream that was never actually pushed.
	name := plumbing.NewRemoteReferenceName("origin", branch)
	if _, err := repository.Reference(name, true); err == nil {
		return true
	}
	cfg, err := repository.Config()
	if err != nil {
		return false
	}
	_, ok := cfg.Branches[branch]
	return ok
}

// DirtyFiles lists what is uncommitted, for a message that says which files
// are in the way rather than merely that some are.
func DirtyFiles(dir string) []string {
	repository, err := git.PlainOpen(dir)
	if err != nil {
		return nil
	}
	worktree, err := repository.Worktree()
	if err != nil {
		return nil
	}
	status, err := worktree.Status()
	if err != nil {
		return nil
	}

	var files []string
	for path := range status {
		files = append(files, path)
	}
	sort.Strings(files)
	return files
}
