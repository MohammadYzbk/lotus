package vcs

// Keeping a working copy in step with its remote.
//
// Solo does not mean single-machine: a writer moving between a laptop and a
// desktop will diverge, and the whole point of this file is that divergence is
// reported plainly and resolved without losing work.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

// Sync is where the branch stands against its remote.
type Sync struct {
	// Tracking is false when the branch has no counterpart on origin, which is
	// the normal state for a branch that has never been pushed.
	Tracking bool `json:"tracking"`
	// Ahead and Behind count commits either side has that the other lacks.
	Ahead  int `json:"ahead"`
	Behind int `json:"behind"`
	// Diverged means both moved: a fast-forward is impossible either way.
	Diverged bool `json:"diverged"`
	// Rebasing means a rebase stopped part-way and is waiting to be finished
	// or abandoned. Nothing else should be attempted until it is resolved.
	Rebasing bool `json:"rebasing"`
	// Conflicted lists the files Git could not merge, which carry the usual
	// <<<<<<< markers and are edited in place.
	Conflicted []string `json:"conflicted"`
	// CanRebase is false when the git binary is absent, so the UI can explain
	// rather than offer an action that cannot run.
	CanRebase bool   `json:"canRebase"`
	Error     string `json:"error"`
}

// PullOutcome says what a pull actually did.
type PullOutcome struct {
	// Updated is the number of commits brought in.
	Updated int `json:"updated"`
	// AlreadyCurrent means the remote had nothing new.
	AlreadyCurrent bool `json:"alreadyCurrent"`
	// Diverged means a fast-forward was impossible; a rebase is the way on.
	Diverged bool `json:"diverged"`
}

// ErrDiverged means local and remote both moved.
var ErrDiverged = errors.New("vcs: local and remote have both moved")

// ErrRebaseConflict means a rebase stopped on conflicting files.
var ErrRebaseConflict = errors.New("vcs: the rebase stopped on conflicts")

// Fetch updates the remote-tracking refs without touching the working copy.
//
// Always safe: it downloads, and changes nothing the writer can see. That is
// what makes it the right thing to do before reporting where a branch stands.
func Fetch(ctx context.Context, dir, token string) error {
	repository, err := git.PlainOpen(dir)
	if err != nil {
		return fmt.Errorf("vcs: open: %w", err)
	}

	options := &git.FetchOptions{RemoteName: "origin", Tags: git.NoTags}
	if token != "" {
		options.Auth = &githttp.BasicAuth{Username: "x-access-token", Password: token}
	}

	err = repository.FetchContext(ctx, options)
	if err == nil || errors.Is(err, git.NoErrAlreadyUpToDate) {
		return nil
	}
	return fmt.Errorf("vcs: fetch: %w", redact(err, token))
}

// SyncState compares the branch with its remote counterpart.
//
// It reads only what is already downloaded, so it is cheap and offline;
// call Fetch first for an answer about the remote as it is now.
func SyncState(dir, branch string) Sync {
	state := Sync{CanRebase: GitAvailable()}
	if branch == "" {
		return state
	}

	repository, err := git.PlainOpen(dir)
	if err != nil {
		state.Error = err.Error()
		return state
	}

	state.Rebasing = rebaseInProgress(dir)
	state.Conflicted = conflictedFiles(dir)

	local, err := repository.Reference(plumbing.NewBranchReferenceName(branch), true)
	if err != nil {
		state.Error = fmt.Sprintf("could not read %s: %v", branch, err)
		return state
	}
	remote, err := repository.Reference(plumbing.NewRemoteReferenceName("origin", branch), true)
	if err != nil {
		// No counterpart on origin yet. Not an error: an unpushed branch is a
		// perfectly ordinary place to be.
		return state
	}
	state.Tracking = true

	ahead, behind, err := countDivergence(repository, local.Hash(), remote.Hash())
	if err != nil {
		state.Error = err.Error()
		return state
	}
	state.Ahead, state.Behind = ahead, behind
	state.Diverged = ahead > 0 && behind > 0
	return state
}

// countDivergence counts the commits unique to each side.
//
// Done by walking each history into a set and subtracting, rather than asking
// for a merge base: go-git's merge-base support is limited, and for the depths
// a writer accumulates between two machines this is both simple and fast.
func countDivergence(repository *git.Repository, local, remote plumbing.Hash) (ahead, behind int, err error) {
	// A bound, so a pathological history cannot hang the UI. Past this the
	// exact number stops being interesting anyway.
	const limit = 2000

	localSet, err := ancestry(repository, local, limit)
	if err != nil {
		return 0, 0, err
	}
	remoteSet, err := ancestry(repository, remote, limit)
	if err != nil {
		return 0, 0, err
	}

	for hash := range localSet {
		if _, shared := remoteSet[hash]; !shared {
			ahead++
		}
	}
	for hash := range remoteSet {
		if _, shared := localSet[hash]; !shared {
			behind++
		}
	}
	return ahead, behind, nil
}

func ancestry(repository *git.Repository, from plumbing.Hash, limit int) (map[plumbing.Hash]struct{}, error) {
	seen := map[plumbing.Hash]struct{}{}
	commit, err := repository.CommitObject(from)
	if err != nil {
		return nil, fmt.Errorf("vcs: read commit: %w", err)
	}

	queue := []*object.Commit{commit}
	for len(queue) > 0 && len(seen) < limit {
		current := queue[0]
		queue = queue[1:]
		if _, already := seen[current.Hash]; already {
			continue
		}
		seen[current.Hash] = struct{}{}

		for _, parentHash := range current.ParentHashes {
			parent, err := repository.CommitObject(parentHash)
			if err != nil {
				// A shallow clone runs out of history; what is here is enough.
				continue
			}
			queue = append(queue, parent)
		}
	}
	return seen, nil
}

// Pull brings the remote's commits in by fast-forward.
//
// It never merges or rewrites: if the histories diverged, it says so and
// leaves everything alone. Rebase is a separate, explicit action.
func Pull(ctx context.Context, dir, branch, token string) (PullOutcome, error) {
	if branch == "" {
		return PullOutcome{}, errors.New("vcs: no branch to pull into")
	}
	if err := Fetch(ctx, dir, token); err != nil {
		return PullOutcome{}, err
	}

	state := SyncState(dir, branch)
	if state.Error != "" {
		return PullOutcome{}, errors.New(state.Error)
	}
	switch {
	case !state.Tracking:
		return PullOutcome{}, errors.New("vcs: this branch is not on origin yet")
	case state.Diverged:
		return PullOutcome{Diverged: true}, ErrDiverged
	case state.Behind == 0:
		return PullOutcome{AlreadyCurrent: true}, nil
	}

	repository, err := git.PlainOpen(dir)
	if err != nil {
		return PullOutcome{}, fmt.Errorf("vcs: open: %w", err)
	}
	worktree, err := repository.Worktree()
	if err != nil {
		return PullOutcome{}, fmt.Errorf("vcs: worktree: %w", err)
	}

	// Behind with nothing of our own: a fast-forward, which go-git does.
	err = worktree.PullContext(ctx, &git.PullOptions{
		RemoteName:    "origin",
		ReferenceName: plumbing.NewBranchReferenceName(branch),
		Auth:          basicAuth(token),
	})
	switch {
	case err == nil, errors.Is(err, git.NoErrAlreadyUpToDate):
		return PullOutcome{Updated: state.Behind}, nil
	case errors.Is(err, git.ErrNonFastForwardUpdate):
		// The counts said otherwise; trust go-git and report honestly.
		return PullOutcome{Diverged: true}, ErrDiverged
	default:
		return PullOutcome{}, fmt.Errorf("vcs: pull: %w", redact(err, token))
	}
}

func basicAuth(token string) *githttp.BasicAuth {
	if token == "" {
		return nil
	}
	return &githttp.BasicAuth{Username: "x-access-token", Password: token}
}

// RebaseOntoRemote replays the local commits on top of origin's.
//
// This is the one operation that shells out: go-git's Merge supports only
// fast-forward and it has no rebase at all. Everything needed is already
// downloaded by the preceding fetch, so the subprocess gets no credentials.
func RebaseOntoRemote(ctx context.Context, dir, branch string) error {
	if !GitAvailable() {
		return ErrGitMissing
	}
	if branch == "" {
		return errors.New("vcs: no branch to rebase")
	}
	if rebaseInProgress(dir) {
		return errors.New("vcs: a rebase is already in progress — finish or abort it first")
	}

	if dirty := DirtyFiles(dir); len(dirty) > 0 {
		return fmt.Errorf("%w: commit them before rebasing", ErrDirty)
	}

	_, err := runGit(ctx, dir, "rebase", "origin/"+branch)
	if err == nil {
		return nil
	}
	if rebaseInProgress(dir) {
		return ErrRebaseConflict
	}
	return err
}

// ContinueRebase resumes after the conflicts have been edited away.
func ContinueRebase(ctx context.Context, dir string) error {
	if !rebaseInProgress(dir) {
		return errors.New("vcs: no rebase is in progress")
	}
	// Whether the markers are gone, not whether the index is staged. The app's
	// model is that conflicts are fixed by editing the file, and `--diff-filter=U`
	// keeps reporting a path as unmerged until it is added — so asking the index
	// would refuse a resolution the writer has genuinely finished.
	if remaining := unresolved(dir, conflictedFiles(dir)); len(remaining) > 0 {
		return fmt.Errorf("%w: %s still %s conflict markers",
			ErrRebaseConflict, strings.Join(remaining, ", "), plural(len(remaining), "carries", "carry"))
	}

	// Stage the resolutions, which is what `git rebase --continue` expects.
	if _, err := runGit(ctx, dir, "add", "-A"); err != nil {
		return err
	}
	if _, err := runGit(ctx, dir, "rebase", "--continue"); err != nil {
		if rebaseInProgress(dir) {
			return ErrRebaseConflict
		}
		return err
	}
	return nil
}

// AbortRebase puts the branch back exactly as it was.
//
// The escape hatch that makes trying a rebase safe: whatever state the writer
// has got themselves into, this returns them to the commit they started from.
func AbortRebase(ctx context.Context, dir string) error {
	if !rebaseInProgress(dir) {
		return errors.New("vcs: no rebase is in progress")
	}
	_, err := runGit(ctx, dir, "rebase", "--abort")
	return err
}

// rebaseInProgress reports whether Git left a rebase half-finished.
//
// Detected from the directories Git itself uses, so it is true regardless of
// whether this app or a terminal started it.
func rebaseInProgress(dir string) bool {
	for _, name := range []string{"rebase-merge", "rebase-apply"} {
		if _, err := os.Stat(filepath.Join(dir, ".git", name)); err == nil {
			return true
		}
	}
	return false
}

// unresolved returns the files that still carry conflict markers.
//
// A file the writer deleted as their resolution is not unresolved, so a read
// failure is treated as done rather than as an error.
func unresolved(dir string, files []string) []string {
	var remaining []string
	for _, name := range files {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		if bytes.Contains(data, []byte("<<<<<<< ")) || bytes.Contains(data, []byte("\n>>>>>>> ")) {
			remaining = append(remaining, name)
		}
	}
	return remaining
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// conflictedFiles lists the paths Git could not merge.
//
// Read through the git binary rather than go-git. go-git's status does not
// model unmerged index stages: during a rebase conflict it reports the file as
// plainly modified ('M'), indistinguishable from an ordinary edit, so there is
// nothing in it to key on. `--diff-filter=U` names exactly the unmerged paths.
//
// Conflicts only arise from the rebase path, which already requires the binary,
// so nothing is lost when it is absent.
func conflictedFiles(dir string) []string {
	if !GitAvailable() {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	out, err := runGit(ctx, dir, "diff", "--name-only", "--diff-filter=U")
	if err != nil || strings.TrimSpace(out) == "" {
		return nil
	}
	files := strings.Split(strings.TrimSpace(out), "\n")
	sort.Strings(files)
	return files
}
