package main

// The GitHub half of the app: connecting an account, listing repositories,
// cloning one, and reporting where the working copy stands.
//
// The token never leaves this file except to go into the keychain or onto an
// outgoing request. It is not in ProjectInfo, not in settings.json, and not in
// anything returned to the frontend.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/MohammadYzbk/lotus/internal/forge"
	"github.com/MohammadYzbk/lotus/internal/secrets"
	"github.com/MohammadYzbk/lotus/internal/vcs"
)

// githubClientID identifies the registered OAuth app. It is set at build time:
//
//	wails build -ldflags "-X main.githubClientID=Iv1.0123456789abcdef"
//
// and falls back to the environment so a developer can try the device flow
// without rebuilding. Empty is a supported state: the device flow is then
// unavailable and the app offers to take a pasted token instead. A client ID
// is public — the device flow exists precisely because a desktop app has
// nowhere safe to keep a secret.
var githubClientID string

const githubClientIDEnv = "LOTUS_GITHUB_CLIENT_ID"

// deviceFlowTimeout bounds a sign-in attempt. GitHub expires its own codes at
// around fifteen minutes; this is the app's backstop if it never says so.
const deviceFlowTimeout = 20 * time.Minute

// githubState is the app's GitHub half, guarded by its own mutex so a slow
// network call never blocks the compile loop.
type githubState struct {
	mu     sync.Mutex
	client *forge.Client
	store  secrets.Store

	// cancelLogin stops an in-flight device flow, so a second attempt or a
	// cancelled dialog does not leave a goroutine polling GitHub for a quarter
	// of an hour.
	cancelLogin context.CancelFunc
	pending     pendingLogin

	// cloneRoot is where clones land. Injectable for the same reason
	// settingsFile is: a test that wrote into the real support directory would
	// leave repositories behind on the developer's machine.
	cloneRoot string
}

// pendingLogin is the device flow currently awaiting the user's browser. The
// context carries the attempt's own deadline, separate from the app's, so a
// sign-in outlives the call that started it.
type pendingLogin struct {
	deviceCode string
	interval   time.Duration
	ctx        context.Context
}

// GitHubAccount is what the UI shows about the connection.
type GitHubAccount struct {
	Connected bool   `json:"connected"`
	Login     string `json:"login"`
	Name      string `json:"name"`
	// DeviceFlow reports whether this build can run the browser sign-in. When
	// false, only a pasted token will work, and the UI should say so rather
	// than offering a button that cannot succeed.
	DeviceFlow bool   `json:"deviceFlow"`
	Error      string `json:"error"`
}

// DeviceLogin is the code to show the user while they authorize in a browser.
type DeviceLogin struct {
	UserCode        string `json:"userCode"`
	VerificationURI string `json:"verificationUri"`
	Error           string `json:"error"`
}

// RepositoryList is the repo picker's contents.
type RepositoryList struct {
	Repositories []forge.Repository `json:"repositories"`
	Error        string             `json:"error"`
}

func newGitHubState() *githubState {
	clientID := githubClientID
	if clientID == "" {
		clientID = os.Getenv(githubClientIDEnv)
	}
	return &githubState{client: forge.New(clientID), store: secrets.NewKeychain()}
}

// token returns the stored token, or "" when no account is connected.
func (g *githubState) token() string {
	secret, err := g.store.Get()
	if err != nil {
		return ""
	}
	return secret
}

// GitHubAccount reports whether an account is connected, and who.
//
// A stored token is verified against GitHub rather than trusted: one revoked
// from the website is the common case, and reporting "connected" until the
// first clone fails would be a lie the user cannot act on.
func (a *App) GitHubAccount() GitHubAccount {
	account := GitHubAccount{DeviceFlow: a.github.client.ClientID != ""}

	token := a.github.token()
	if token == "" {
		return account
	}

	user, err := a.github.client.User(a.context(), token)
	if err != nil {
		account.Error = err.Error()
		return account
	}
	account.Connected = true
	account.Login = user.Login
	account.Name = user.Name
	return account
}

// GitHubStartLogin begins the device flow and returns the code to type.
//
// Polling runs in the background from here, so the frontend shows the code
// immediately and then awaits GitHubAwaitLogin.
func (a *App) GitHubStartLogin() DeviceLogin {
	a.github.mu.Lock()
	defer a.github.mu.Unlock()

	// A second attempt replaces the first rather than racing it.
	a.cancelLoginLocked()

	code, err := a.github.client.StartDeviceFlow(a.context())
	if err != nil {
		if errors.Is(err, forge.ErrNoClientID) {
			return DeviceLogin{Error: "This build has no GitHub client ID, so browser sign-in is unavailable. Paste a personal access token instead."}
		}
		return DeviceLogin{Error: err.Error()}
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(a.context()), deviceFlowTimeout)
	a.github.cancelLogin = cancel
	a.github.pending = pendingLogin{
		deviceCode: code.DeviceCode,
		interval:   time.Duration(code.Interval) * time.Second,
		ctx:        ctx,
	}

	return DeviceLogin{UserCode: code.UserCode, VerificationURI: code.VerificationURI}
}

// GitHubAwaitLogin blocks until the user finishes authorizing, or the attempt
// fails or expires. The frontend awaits it after showing the code.
func (a *App) GitHubAwaitLogin() GitHubAccount {
	a.github.mu.Lock()
	pending := a.github.pending
	a.github.mu.Unlock()

	if pending.deviceCode == "" {
		return GitHubAccount{DeviceFlow: a.github.client.ClientID != "", Error: "no sign-in is in progress"}
	}

	token, err := a.pollUntilAuthorized(pending)
	if err != nil {
		return GitHubAccount{DeviceFlow: a.github.client.ClientID != "", Error: err.Error()}
	}
	return a.adoptToken(token)
}

// pollUntilAuthorized asks GitHub, at the interval it asked for, until the user
// finishes in the browser.
func (a *App) pollUntilAuthorized(pending pendingLogin) (string, error) {
	interval := pending.interval
	if interval < time.Second {
		interval = 5 * time.Second
	}

	for {
		select {
		case <-pending.ctx.Done():
			return "", errors.New("sign-in timed out or was cancelled")
		case <-time.After(interval):
		}

		token, err := a.github.client.PollDeviceFlow(pending.ctx, pending.deviceCode)
		switch {
		case err == nil:
			return token, nil
		case errors.Is(err, forge.ErrAuthorizationPending):
			// Expected until the user finishes in the browser.
		case errors.Is(err, forge.ErrSlowDown):
			// GitHub asks for a longer gap; honouring it avoids being throttled.
			interval += 5 * time.Second
		default:
			return "", err
		}
	}
}

// GitHubUseToken connects using a pasted personal access token.
//
// This is the path for a build with no client ID, and for org repositories
// that block OAuth apps outright.
func (a *App) GitHubUseToken(token string) GitHubAccount {
	token = strings.TrimSpace(token)
	if token == "" {
		return GitHubAccount{DeviceFlow: a.github.client.ClientID != "", Error: "no token was given"}
	}
	return a.adoptToken(token)
}

// adoptToken verifies a token before storing it, so a typo fails at the moment
// it was made rather than at the first clone.
func (a *App) adoptToken(token string) GitHubAccount {
	account := GitHubAccount{DeviceFlow: a.github.client.ClientID != ""}

	user, err := a.github.client.User(a.context(), token)
	if err != nil {
		account.Error = err.Error()
		return account
	}
	if err := a.github.store.Set(token); err != nil {
		account.Error = fmt.Sprintf("signed in, but the token could not be saved: %v", err)
		return account
	}

	account.Connected = true
	account.Login = user.Login
	account.Name = user.Name
	return account
}

// GitHubDisconnect forgets the token.
func (a *App) GitHubDisconnect() GitHubAccount {
	a.github.mu.Lock()
	a.cancelLoginLocked()
	a.github.mu.Unlock()

	account := GitHubAccount{DeviceFlow: a.github.client.ClientID != ""}
	if err := a.github.store.Clear(); err != nil {
		account.Error = err.Error()
	}
	return account
}

// GitHubCancelLogin abandons a sign-in the user backed out of.
func (a *App) GitHubCancelLogin() {
	a.github.mu.Lock()
	defer a.github.mu.Unlock()
	a.cancelLoginLocked()
}

// cancelLoginLocked stops any in-flight device flow. Callers must hold the
// GitHub mutex.
func (a *App) cancelLoginLocked() {
	if a.github.cancelLogin != nil {
		a.github.cancelLogin()
		a.github.cancelLogin = nil
	}
	a.github.pending = pendingLogin{}
}

// GitHubRepositories lists what the connected account can clone.
func (a *App) GitHubRepositories() RepositoryList {
	token := a.github.token()
	if token == "" {
		return RepositoryList{Error: "connect a GitHub account first"}
	}

	repositories, err := a.github.client.Repositories(a.context(), token)
	if err != nil {
		return RepositoryList{Error: err.Error()}
	}
	// GitHub sorts by update time, but a manual page boundary can interleave;
	// sorting here makes the picker's order match what it claims.
	sort.SliceStable(repositories, func(i, j int) bool {
		return repositories[i].UpdatedAt.After(repositories[j].UpdatedAt)
	})
	return RepositoryList{Repositories: repositories}
}

// CloneRepository clones a repository into the managed directory and opens it
// as the current project.
//
// An existing clone is reused rather than refused: picking the same repository
// twice should take you back to your work, not produce an error.
func (a *App) CloneRepository(cloneURL string) ProjectInfo {
	owner, name, err := vcs.Slug(cloneURL)
	if err != nil {
		return ProjectInfo{Error: err.Error()}
	}

	dir, err := a.clonePath(owner, name)
	if err != nil {
		return ProjectInfo{Error: err.Error()}
	}

	if existing := vcs.Status(dir); existing.Repository {
		return a.OpenProject(dir)
	}

	if err := vcs.Clone(a.context(), cloneURL, dir, a.github.token()); err != nil {
		if errors.Is(err, vcs.ErrExists) {
			return ProjectInfo{Error: fmt.Sprintf("%s already exists but is not a repository; move or remove it and try again", dir)}
		}
		return ProjectInfo{Error: err.Error()}
	}
	return a.OpenProject(dir)
}

// RepositoryState describes the open project's working copy.
//
// It is a separate call rather than part of ProjectInfo because reading it
// walks the working tree, and the file tree is re-rendered far more often than
// the Git state changes.
func (a *App) RepositoryState() vcs.State {
	a.mu.Lock()
	proj := a.proj
	a.mu.Unlock()

	if proj == nil {
		return vcs.State{}
	}
	return vcs.Status(proj.Root())
}

// clonePath is where a repository lives once cloned: one directory per owner
// and name, under the app's own support directory, so a clone never lands in a
// folder the user is already using for something else.
func (a *App) clonePath(owner, name string) (string, error) {
	root := a.github.cloneRoot
	if root == "" {
		base, err := appDir()
		if err != nil {
			return "", err
		}
		root = filepath.Join(base, "repos")
	}
	return filepath.Join(root, owner, name), nil
}
