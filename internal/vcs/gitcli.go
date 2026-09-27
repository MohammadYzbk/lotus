package vcs

// The one place this package shells out to Git.
//
// go-git covers cloning, status, branching, committing, fetching and
// fast-forward pulls. What it cannot do is rebase: its Merge supports only
// fast-forward, and there is no Rebase or CherryPick at all. Divergence — two
// machines that both moved — is exactly the case that needs one, so that
// single operation goes to the git binary.
//
// The split is deliberate rather than reluctant. Fetching stays in go-git, so
// the token remains in this process and is never passed to a subprocess or
// written into a config a credential helper might cache. Everything reached
// through here works on local objects that are already downloaded, so it needs
// no credentials and no network.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// ErrGitMissing means the git binary is not installed.
//
// Reported rather than hidden: the app works without it right up until
// histories diverge, and a writer who hits that deserves to know what is
// missing instead of watching the button do nothing.
var ErrGitMissing = errors.New("vcs: git is not installed")

// gitBinary finds git, or explains that it is absent.
func gitBinary() (string, error) {
	path, err := exec.LookPath("git")
	if err != nil {
		return "", ErrGitMissing
	}
	return path, nil
}

// GitAvailable reports whether the operations needing the binary can run.
func GitAvailable() bool {
	_, err := gitBinary()
	return err == nil
}

// runGit executes git in dir and returns its combined output.
//
// The environment is pinned so the result does not depend on the writer's Git
// configuration: an editor set to something interactive, or a pager, would
// otherwise hang a rebase forever behind a prompt nobody can see.
func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	binary, err := gitBinary()
	if err != nil {
		return "", err
	}

	command := exec.CommandContext(ctx, binary, args...)
	command.Dir = dir
	command.Env = append(command.Environ(),
		// Nothing here may wait for a human.
		"GIT_TERMINAL_PROMPT=0",
		"GIT_EDITOR=true",
		"GIT_SEQUENCE_EDITOR=true",
		"GIT_PAGER=cat",
		"GIT_OPTIONAL_LOCKS=0",
	)

	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output

	if err := command.Run(); err != nil {
		text := strings.TrimSpace(output.String())
		if text == "" {
			return "", fmt.Errorf("vcs: git %s: %w", strings.Join(args, " "), err)
		}
		return text, fmt.Errorf("vcs: git %s: %s", strings.Join(args, " "), firstLines(text, 4))
	}
	return strings.TrimSpace(output.String()), nil
}

// firstLines keeps an error to something readable. Git is voluble on failure
// and the first few lines carry the reason.
func firstLines(text string, n int) string {
	lines := strings.Split(text, "\n")
	if len(lines) <= n {
		return strings.Join(lines, "; ")
	}
	return strings.Join(lines[:n], "; ") + " …"
}
