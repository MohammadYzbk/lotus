// Package forge talks to GitHub: signing in, and listing what you can clone.
//
// It is hand-rolled over net/http rather than built on a GitHub SDK. The whole
// surface is four requests, and an SDK would pull in a large dependency to
// save a hundred lines of JSON decoding. If Phase 7 grows this much further,
// that trade flips.
//
// Nothing here writes the token anywhere. It is returned to the caller, who
// decides where it lives; see internal/secrets.
package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Public GitHub. Both are fields rather than constants so tests can point the
// client at an httptest server, and so GitHub Enterprise stays possible.
const (
	DefaultOAuthBase = "https://github.com"
	DefaultAPIBase   = "https://api.github.com"
)

// Scope is the least privilege that still allows cloning and pushing to a
// private repository. Anything narrower cannot write; anything wider asks for
// access the app never uses.
const Scope = "repo"

// Errors the device flow reports as part of normal operation rather than as
// failures: the user simply has not finished authorizing yet.
var (
	// ErrAuthorizationPending means the user has not entered the code yet.
	ErrAuthorizationPending = errors.New("forge: authorization pending")
	// ErrSlowDown means we polled too fast and must back off.
	ErrSlowDown = errors.New("forge: slow down")
	// ErrExpired means the user took too long and must start again.
	ErrExpired = errors.New("forge: the code expired")
	// ErrAccessDenied means the user refused in the browser.
	ErrAccessDenied = errors.New("forge: access was denied")
)

// ErrNoClientID means the app was built without an OAuth client ID, so the
// device flow cannot run. Pasting a token still can.
var ErrNoClientID = errors.New("forge: no OAuth client ID is configured")

// Client talks to one GitHub instance.
type Client struct {
	HTTP      *http.Client
	OAuthBase string
	APIBase   string
	// ClientID identifies the registered OAuth app. Device flow needs no
	// secret, which is exactly why it suits a desktop app: there is nowhere
	// safe to keep one.
	ClientID string
}

// New returns a client for public GitHub.
func New(clientID string) *Client {
	return &Client{
		HTTP:      &http.Client{Timeout: 30 * time.Second},
		OAuthBase: DefaultOAuthBase,
		APIBase:   DefaultAPIBase,
		ClientID:  clientID,
	}
}

// DeviceCode is what the user needs in order to authorize the app.
type DeviceCode struct {
	// DeviceCode identifies this attempt when polling. Not shown to the user.
	DeviceCode string `json:"-"`
	// UserCode is the short code typed into the browser.
	UserCode string `json:"userCode"`
	// VerificationURI is where to type it.
	VerificationURI string `json:"verificationUri"`
	// Interval is the minimum seconds between polls, set by the server.
	Interval int `json:"interval"`
	// ExpiresIn is how long the code remains valid, in seconds.
	ExpiresIn int `json:"expiresIn"`
}

// User is the signed-in account.
type User struct {
	Login string `json:"login"`
	Name  string `json:"name"`
}

// Repository is one entry in the repo picker.
//
// Its tags name the shape the frontend sees. GitHub's own field names are
// snake_case, so decoding goes through repositoryWire rather than this struct —
// one type cannot answer to both spellings, and a mismatch decodes silently to
// an empty struct rather than failing.
type Repository struct {
	FullName      string `json:"fullName"`
	Description   string `json:"description"`
	CloneURL      string `json:"cloneUrl"`
	DefaultBranch string `json:"defaultBranch"`
	Private       bool   `json:"private"`
	// UpdatedAt drives the default ordering: the repo you touched last is
	// almost always the one you want.
	UpdatedAt time.Time `json:"updatedAt"`
}

// StartDeviceFlow asks GitHub for a code to show the user.
func (c *Client) StartDeviceFlow(ctx context.Context) (DeviceCode, error) {
	if c.ClientID == "" {
		return DeviceCode{}, ErrNoClientID
	}

	form := url.Values{"client_id": {c.ClientID}, "scope": {Scope}}
	var body struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		ExpiresIn       int    `json:"expires_in"`
		Interval        int    `json:"interval"`
		Error           string `json:"error"`
		Description     string `json:"error_description"`
	}
	if err := c.postForm(ctx, c.oauthBase()+"/login/device/code", form, &body); err != nil {
		return DeviceCode{}, err
	}
	if body.Error != "" {
		return DeviceCode{}, fmt.Errorf("forge: %s", describe(body.Error, body.Description))
	}
	if body.DeviceCode == "" || body.UserCode == "" {
		return DeviceCode{}, errors.New("forge: GitHub returned no device code")
	}

	// GitHub documents 5 seconds, but the response is authoritative and a
	// missing value must not turn into a busy loop.
	interval := body.Interval
	if interval < 1 {
		interval = 5
	}
	return DeviceCode{
		DeviceCode:      body.DeviceCode,
		UserCode:        body.UserCode,
		VerificationURI: body.VerificationURI,
		Interval:        interval,
		ExpiresIn:       body.ExpiresIn,
	}, nil
}

// PollDeviceFlow asks once whether the user has finished authorizing.
//
// The pending and slow-down cases come back as errors the caller is expected
// to handle rather than report: they are how the flow says "not yet".
func (c *Client) PollDeviceFlow(ctx context.Context, deviceCode string) (string, error) {
	if c.ClientID == "" {
		return "", ErrNoClientID
	}

	form := url.Values{
		"client_id":   {c.ClientID},
		"device_code": {deviceCode},
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
	}
	var body struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if err := c.postForm(ctx, c.oauthBase()+"/login/oauth/access_token", form, &body); err != nil {
		return "", err
	}

	switch body.Error {
	case "":
	case "authorization_pending":
		return "", ErrAuthorizationPending
	case "slow_down":
		return "", ErrSlowDown
	case "expired_token":
		return "", ErrExpired
	case "access_denied":
		return "", ErrAccessDenied
	default:
		return "", fmt.Errorf("forge: %s", describe(body.Error, body.Description))
	}

	if body.AccessToken == "" {
		return "", errors.New("forge: GitHub returned no access token")
	}
	return body.AccessToken, nil
}

// User identifies the account a token belongs to. It doubles as the check that
// a pasted token is valid at all.
func (c *Client) User(ctx context.Context, token string) (User, error) {
	var user User
	err := c.get(ctx, c.apiBase()+"/user", token, &user)
	return user, err
}

// Repositories lists what the account can clone, most recently updated first.
//
// Pagination is followed to a bounded number of pages: someone with thousands
// of repositories does not want to wait for all of them before the picker
// opens, and the picker searches what it has.
func (c *Client) Repositories(ctx context.Context, token string) ([]Repository, error) {
	const perPage, maxPages = 100, 10

	var all []Repository
	for page := 1; page <= maxPages; page++ {
		query := url.Values{
			"per_page":    {strconv.Itoa(perPage)},
			"page":        {strconv.Itoa(page)},
			"sort":        {"updated"},
			"affiliation": {"owner,collaborator,organization_member"},
		}
		var batch []repositoryWire
		if err := c.get(ctx, c.apiBase()+"/user/repos?"+query.Encode(), token, &batch); err != nil {
			return nil, err
		}
		for _, wire := range batch {
			all = append(all, wire.repository())
		}
		if len(batch) < perPage {
			break
		}
	}
	return all, nil
}

// repositoryWire is GitHub's spelling of a repository.
type repositoryWire struct {
	FullName      string    `json:"full_name"`
	Description   string    `json:"description"`
	CloneURL      string    `json:"clone_url"`
	DefaultBranch string    `json:"default_branch"`
	Private       bool      `json:"private"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func (w repositoryWire) repository() Repository {
	return Repository{
		FullName:      w.FullName,
		Description:   w.Description,
		CloneURL:      w.CloneURL,
		DefaultBranch: w.DefaultBranch,
		Private:       w.Private,
		UpdatedAt:     w.UpdatedAt,
	}
}

// --- transport ---------------------------------------------------------------

func (c *Client) oauthBase() string {
	return strings.TrimSuffix(orDefault(c.OAuthBase, DefaultOAuthBase), "/")
}
func (c *Client) apiBase() string {
	return strings.TrimSuffix(orDefault(c.APIBase, DefaultAPIBase), "/")
}

func (c *Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (c *Client) postForm(ctx context.Context, endpoint string, form url.Values, out any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("forge: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// Without this the OAuth endpoints reply with form-encoded bodies.
	request.Header.Set("Accept", "application/json")
	return c.do(request, out)
}

func (c *Client) get(ctx context.Context, endpoint, token string, out any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("forge: %w", err)
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	return c.do(request, out)
}

func (c *Client) do(request *http.Request, out any) error {
	response, err := c.client().Do(request)
	if err != nil {
		return fmt.Errorf("forge: %w", err)
	}
	defer response.Body.Close()

	// Bounded: a wrong base URL can otherwise stream an entire website into
	// memory before the JSON decode fails.
	data, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("forge: %w", err)
	}

	if response.StatusCode == http.StatusUnauthorized {
		return errors.New("forge: GitHub rejected the token — it may have been revoked or lack the repo scope")
	}
	if response.StatusCode >= 400 {
		return fmt.Errorf("forge: GitHub returned %s: %s", response.Status, firstLine(data))
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("forge: could not read GitHub's reply: %w", err)
	}
	return nil
}

// describe prefers GitHub's human-readable explanation over its error code.
func describe(code, description string) string {
	if description != "" {
		return description
	}
	return code
}

// firstLine keeps an error message to one line; GitHub error bodies are JSON
// and a raw dump helps nobody.
func firstLine(data []byte) string {
	var body struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(data, &body); err == nil && body.Message != "" {
		return body.Message
	}
	text := strings.TrimSpace(string(data))
	if index := strings.IndexByte(text, '\n'); index >= 0 {
		text = text[:index]
	}
	if len(text) > 200 {
		text = text[:200] + "…"
	}
	return text
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// --- pull requests -------------------------------------------------------------

// PullRequest is an opened or existing pull request.
type PullRequest struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
	Title  string `json:"title"`
	// Existing is true when this was already open rather than just created.
	Existing bool `json:"existing"`
}

// RepositoryInfo is what the app needs to know about a repository it did not
// list: chiefly which branch a pull request should target.
type RepositoryInfo struct {
	FullName      string `json:"fullName"`
	DefaultBranch string `json:"defaultBranch"`
}

// Repository fetches one repository, for its default branch.
func (c *Client) Repository(ctx context.Context, token, owner, name string) (RepositoryInfo, error) {
	var wire struct {
		FullName      string `json:"full_name"`
		DefaultBranch string `json:"default_branch"`
	}
	if err := c.get(ctx, fmt.Sprintf("%s/repos/%s/%s", c.apiBase(), owner, name), token, &wire); err != nil {
		return RepositoryInfo{}, err
	}
	return RepositoryInfo{FullName: wire.FullName, DefaultBranch: wire.DefaultBranch}, nil
}

// OpenPullRequestFor returns the open pull request for a branch, if there is
// one.
//
// Checked before creating, because pressing the button twice is an ordinary
// thing to do and GitHub answers the second attempt with a 422 that explains
// nothing useful. Finding the existing one and handing back its URL is what
// the writer wanted either way.
func (c *Client) OpenPullRequestFor(ctx context.Context, token, owner, name, branch string) (PullRequest, bool, error) {
	query := url.Values{
		// The head filter is qualified by owner, which is how GitHub scopes it.
		"head":  {owner + ":" + branch},
		"state": {"open"},
	}
	endpoint := fmt.Sprintf("%s/repos/%s/%s/pulls?%s", c.apiBase(), owner, name, query.Encode())

	var wire []struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
		Title   string `json:"title"`
	}
	if err := c.get(ctx, endpoint, token, &wire); err != nil {
		return PullRequest{}, false, err
	}
	if len(wire) == 0 {
		return PullRequest{}, false, nil
	}
	return PullRequest{
		Number:   wire[0].Number,
		URL:      wire[0].HTMLURL,
		Title:    wire[0].Title,
		Existing: true,
	}, true, nil
}

// CreatePullRequest opens a pull request from branch into base.
func (c *Client) CreatePullRequest(ctx context.Context, token, owner, name string, pr PullRequestDraft) (PullRequest, error) {
	body := map[string]string{
		"title": pr.Title,
		"head":  pr.Head,
		"base":  pr.Base,
		"body":  pr.Body,
	}
	var wire struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
		Title   string `json:"title"`
	}
	endpoint := fmt.Sprintf("%s/repos/%s/%s/pulls", c.apiBase(), owner, name)
	if err := c.postJSON(ctx, endpoint, token, body, &wire); err != nil {
		return PullRequest{}, err
	}
	if wire.HTMLURL == "" {
		return PullRequest{}, errors.New("forge: GitHub opened no pull request")
	}
	return PullRequest{Number: wire.Number, URL: wire.HTMLURL, Title: wire.Title}, nil
}

// PullRequestDraft is what to open.
type PullRequestDraft struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Head  string `json:"head"`
	Base  string `json:"base"`
}

func (c *Client) postJSON(ctx context.Context, endpoint, token string, payload, out any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("forge: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("forge: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	return c.do(request, out)
}
