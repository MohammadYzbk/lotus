package main

// Keeping the working copy in step with its remote, bound for the frontend.
//
// The ordering here is the safety story: fetching is always allowed because it
// changes nothing visible; pulling only fast-forwards; and rewriting history
// with a rebase is a separate action the writer asks for after being told the
// branches diverged.

import (
	"errors"
	"fmt"

	"github.com/MohammadYzbk/lotus/internal/vcs"
)

// SyncResult is the state of the working copy and its relation to origin,
// returned by everything in this file so one call updates the whole panel.
type SyncResult struct {
	State vcs.State `json:"state"`
	Sync  vcs.Sync  `json:"sync"`
	// Detail is a short note worth showing on success.
	Detail string `json:"detail"`
	Error  string `json:"error"`
}

// syncResult reads both states, attaching detail or err.
func (a *App) syncResult(root, branch, detail string, err error) SyncResult {
	state := vcs.Status(root)
	if branch == "" {
		branch = state.Branch
	}
	result := SyncResult{State: state, Sync: vcs.SyncState(root, branch), Detail: detail}
	if err != nil {
		result.Error = err.Error()
	}
	return result
}

// SyncStatus reports where the branch stands without touching the network.
//
// Cheap and offline, so the UI can call it after anything that might have
// moved the working copy.
func (a *App) SyncStatus() SyncResult {
	root, problem := a.projectRoot()
	if problem != "" {
		return SyncResult{Error: problem}
	}
	return a.syncResult(root, "", "", nil)
}

// FetchRemote updates the remote-tracking refs.
//
// Always safe: it downloads and changes nothing the writer can see, which is
// what makes it the right thing to do before reporting where a branch stands.
func (a *App) FetchRemote() SyncResult {
	root, problem := a.projectRoot()
	if problem != "" {
		return SyncResult{Error: problem}
	}

	state := vcs.Status(root)
	if !state.Repository {
		return a.syncResult(root, "", "", errors.New("this project is not a Git repository"))
	}
	if state.Remote == "" {
		return a.syncResult(root, "", "", errors.New("this repository has no origin"))
	}

	if err := vcs.Fetch(a.context(), root, a.github.token()); err != nil {
		return a.syncResult(root, "", "", err)
	}

	sync := vcs.SyncState(root, state.Branch)
	return a.syncResult(root, state.Branch, describeSync(sync), nil)
}

// PullChanges brings the remote's commits in by fast-forward.
func (a *App) PullChanges() SyncResult {
	root, problem := a.projectRoot()
	if problem != "" {
		return SyncResult{Error: problem}
	}

	state := vcs.Status(root)
	switch {
	case !state.Repository:
		return a.syncResult(root, "", "", errors.New("this project is not a Git repository"))
	case state.Branch == "":
		return a.syncResult(root, "", "", errors.New("there is no branch to pull into"))
	case state.Dirty:
		// A fast-forward over uncommitted work can fail part-way, leaving files
		// from two commits on disk.
		return a.syncResult(root, "", "", fmt.Errorf(
			"commit your changes before pulling — %s", describeFiles(vcs.DirtyFiles(root))))
	}

	outcome, err := vcs.Pull(a.context(), root, state.Branch, a.github.token())
	if errors.Is(err, vcs.ErrDiverged) {
		sync := vcs.SyncState(root, state.Branch)
		return a.syncResult(root, state.Branch, "", fmt.Errorf(
			"%s and origin have both moved — %d local, %d remote. Rebase to replay your work on top",
			state.Branch, sync.Ahead, sync.Behind))
	}
	if err != nil {
		return a.syncResult(root, state.Branch, "", err)
	}

	switch {
	case outcome.AlreadyCurrent:
		return a.syncResult(root, state.Branch, "Already up to date", nil)
	default:
		return a.syncResult(root, state.Branch, fmt.Sprintf(
			"Pulled %d %s", outcome.Updated, plural(outcome.Updated, "commit", "commits")), nil)
	}
}

// RebaseOnRemote replays local commits on top of origin's.
//
// The one thing here that rewrites history, so it is never automatic: the
// writer reaches it only after being told the branches diverged.
func (a *App) RebaseOnRemote() SyncResult {
	root, problem := a.projectRoot()
	if problem != "" {
		return SyncResult{Error: problem}
	}

	state := vcs.Status(root)
	if state.Branch == "" {
		return a.syncResult(root, "", "", errors.New("there is no branch to rebase"))
	}
	if !vcs.GitAvailable() {
		return a.syncResult(root, state.Branch, "", errors.New(
			"rebasing needs the git command line, which is not installed. "+
				"Everything else works without it; install Git to resolve diverged branches here"))
	}

	// Rebase onto what origin has now, not what was downloaded an hour ago.
	if err := vcs.Fetch(a.context(), root, a.github.token()); err != nil {
		return a.syncResult(root, state.Branch, "", err)
	}

	err := vcs.RebaseOntoRemote(a.context(), root, state.Branch)
	switch {
	case errors.Is(err, vcs.ErrDirty):
		return a.syncResult(root, state.Branch, "", fmt.Errorf(
			"commit your changes before rebasing — %s", describeFiles(vcs.DirtyFiles(root))))
	case errors.Is(err, vcs.ErrRebaseConflict):
		sync := vcs.SyncState(root, state.Branch)
		return a.syncResult(root, state.Branch, "", fmt.Errorf(
			"the rebase stopped on %s. Open %s, remove the <<<<<<< markers, then continue",
			describeFiles(sync.Conflicted), plural(len(sync.Conflicted), "it", "them")))
	case err != nil:
		return a.syncResult(root, state.Branch, "", err)
	}
	return a.syncResult(root, state.Branch, "Replayed your work on top of origin", nil)
}

// ContinueRebase resumes once the conflicts have been edited away.
func (a *App) ContinueRebase() SyncResult {
	root, problem := a.projectRoot()
	if problem != "" {
		return SyncResult{Error: problem}
	}

	err := vcs.ContinueRebase(a.context(), root)
	if errors.Is(err, vcs.ErrRebaseConflict) {
		return a.syncResult(root, "", "", err)
	}
	if err != nil {
		return a.syncResult(root, "", "", err)
	}

	// The files on disk changed underneath the editor.
	a.mu.Lock()
	a.invalidateDiagnosticManifestLocked()
	a.mu.Unlock()

	return a.syncResult(root, "", "Rebase finished", nil)
}

// AbortRebase puts the branch back exactly as it was.
//
// The escape hatch that makes trying a rebase safe.
func (a *App) AbortRebase() SyncResult {
	root, problem := a.projectRoot()
	if problem != "" {
		return SyncResult{Error: problem}
	}

	if err := vcs.AbortRebase(a.context(), root); err != nil {
		return a.syncResult(root, "", "", err)
	}

	a.mu.Lock()
	a.invalidateDiagnosticManifestLocked()
	a.mu.Unlock()

	return a.syncResult(root, "", "Rebase abandoned; the branch is back where it was", nil)
}

// describeSync turns the counts into the sentence the status line shows.
func describeSync(sync vcs.Sync) string {
	switch {
	case !sync.Tracking:
		return "This branch is not on origin yet"
	case sync.Diverged:
		return fmt.Sprintf("Diverged: %d local, %d remote", sync.Ahead, sync.Behind)
	case sync.Behind > 0:
		return fmt.Sprintf("%d %s to pull", sync.Behind, plural(sync.Behind, "commit", "commits"))
	case sync.Ahead > 0:
		return fmt.Sprintf("%d %s to push", sync.Ahead, plural(sync.Ahead, "commit", "commits"))
	default:
		return "Up to date with origin"
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
