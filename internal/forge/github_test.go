package forge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// testClient points a Client at a stub server standing in for GitHub.
func testClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	client := New("test-client-id")
	client.OAuthBase = server.URL
	client.APIBase = server.URL
	client.HTTP = server.Client()
	return client
}

func TestStartDeviceFlow(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/login/device/code" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		// Without an Accept of JSON, GitHub answers in form encoding and the
		// decode silently produces an empty struct.
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("Accept: got %q, want application/json", got)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if got := r.Form.Get("client_id"); got != "test-client-id" {
			t.Errorf("client_id: got %q", got)
		}
		if got := r.Form.Get("scope"); got != "repo" {
			t.Errorf("scope: got %q, want repo", got)
		}
		fmt.Fprint(w, `{"device_code":"DEV","user_code":"ABCD-1234",
			"verification_uri":"https://github.com/login/device","expires_in":900,"interval":5}`)
	})

	code, err := client.StartDeviceFlow(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if code.UserCode != "ABCD-1234" {
		t.Errorf("user code: got %q", code.UserCode)
	}
	if code.DeviceCode != "DEV" {
		t.Errorf("device code: got %q", code.DeviceCode)
	}
	if code.Interval != 5 {
		t.Errorf("interval: got %d", code.Interval)
	}
}

// A server that omits the interval must not turn the poll loop into a spin.
func TestStartDeviceFlowDefaultsTheInterval(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"device_code":"DEV","user_code":"AB","verification_uri":"https://x"}`)
	})

	code, err := client.StartDeviceFlow(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if code.Interval < 1 {
		t.Errorf("interval: got %d, want a positive default", code.Interval)
	}
}

func TestStartDeviceFlowWithoutClientID(t *testing.T) {
	client := New("")
	if _, err := client.StartDeviceFlow(context.Background()); !errors.Is(err, ErrNoClientID) {
		t.Errorf("got %v, want ErrNoClientID", err)
	}
}

// The pending and slow-down replies are how the flow says "not yet". Reporting
// them as failures would abort a sign-in that is going fine.
func TestPollDeviceFlowOutcomes(t *testing.T) {
	for _, testCase := range []struct {
		body string
		want error
	}{
		{`{"error":"authorization_pending"}`, ErrAuthorizationPending},
		{`{"error":"slow_down"}`, ErrSlowDown},
		{`{"error":"expired_token"}`, ErrExpired},
		{`{"error":"access_denied"}`, ErrAccessDenied},
	} {
		client := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, testCase.body)
		})
		_, err := client.PollDeviceFlow(context.Background(), "DEV")
		if !errors.Is(err, testCase.want) {
			t.Errorf("%s: got %v, want %v", testCase.body, err, testCase.want)
		}
	}
}

func TestPollDeviceFlowSucceeds(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if got := r.Form.Get("grant_type"); got != "urn:ietf:params:oauth:grant-type:device_code" {
			t.Errorf("grant_type: got %q", got)
		}
		if got := r.Form.Get("device_code"); got != "DEV" {
			t.Errorf("device_code: got %q", got)
		}
		fmt.Fprint(w, `{"access_token":"gho_secret","token_type":"bearer"}`)
	})

	token, err := client.PollDeviceFlow(context.Background(), "DEV")
	if err != nil {
		t.Fatal(err)
	}
	if token != "gho_secret" {
		t.Errorf("token: got %q", token)
	}
}

// A 200 with neither token nor error is a protocol violation; treating it as
// success would store an empty token and "connect" the app to nothing.
func TestPollDeviceFlowRejectsEmptyToken(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{}`)
	})
	if _, err := client.PollDeviceFlow(context.Background(), "DEV"); err == nil {
		t.Error("an empty token was accepted")
	}
}

func TestUserSendsBearerToken(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization: got %q", got)
		}
		fmt.Fprint(w, `{"login":"octocat","name":"The Octocat"}`)
	})

	user, err := client.User(context.Background(), "tok")
	if err != nil {
		t.Fatal(err)
	}
	if user.Login != "octocat" {
		t.Errorf("login: got %q", user.Login)
	}
}

// A revoked or under-scoped token is the most likely failure in daily use, so
// it gets an explanation rather than a bare status code.
func TestUnauthorizedIsExplained(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"message":"Bad credentials"}`)
	})

	_, err := client.User(context.Background(), "bad")
	if err == nil {
		t.Fatal("a 401 was treated as success")
	}
	if !strings.Contains(err.Error(), "revoked") {
		t.Errorf("unhelpful message: %v", err)
	}
}

func TestRepositoriesFollowsPagination(t *testing.T) {
	var pages int
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		pages++
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if got := r.URL.Query().Get("per_page"); got != "100" {
			t.Errorf("per_page: got %q", got)
		}
		if page == 1 {
			// A full page means there may be more.
			var entries []string
			for i := range 100 {
				entries = append(entries, fmt.Sprintf(`{"full_name":"me/r%d","clone_url":"https://x/%d.git"}`, i, i))
			}
			fmt.Fprint(w, "["+strings.Join(entries, ",")+"]")
			return
		}
		fmt.Fprint(w, `[{"full_name":"me/last","clone_url":"https://x/last.git"}]`)
	})

	repos, err := client.Repositories(context.Background(), "tok")
	if err != nil {
		t.Fatal(err)
	}
	if pages != 2 {
		t.Errorf("requested %d pages, want 2", pages)
	}
	if len(repos) != 101 {
		t.Errorf("got %d repositories, want 101", len(repos))
	}
	if repos[100].FullName != "me/last" {
		t.Errorf("last repository: got %q", repos[100].FullName)
	}
}

// A short page means the end; asking for another wastes a request and, on a
// server that ignores `page`, loops forever.
func TestRepositoriesStopsOnShortPage(t *testing.T) {
	var pages int
	client := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		pages++
		fmt.Fprint(w, `[{"full_name":"me/only","clone_url":"https://x/only.git"}]`)
	})

	if _, err := client.Repositories(context.Background(), "tok"); err != nil {
		t.Fatal(err)
	}
	if pages != 1 {
		t.Errorf("requested %d pages, want 1", pages)
	}
}

// --- pull requests -------------------------------------------------------------

func TestRepositoryReadsTheDefaultBranch(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/octocat/paper" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		fmt.Fprint(w, `{"full_name":"octocat/paper","default_branch":"trunk"}`)
	})

	info, err := client.Repository(context.Background(), "tok", "octocat", "paper")
	if err != nil {
		t.Fatal(err)
	}
	if info.DefaultBranch != "trunk" {
		t.Errorf("default branch: got %q, want trunk", info.DefaultBranch)
	}
}

func TestCreatePullRequestSendsTheDraft(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method: got %s", r.Method)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["head"] != "feature/zoom" || body["base"] != "main" {
			t.Errorf("head/base: got %q -> %q", body["head"], body["base"])
		}
		if body["title"] != "Zoom the preview" {
			t.Errorf("title: got %q", body["title"])
		}
		fmt.Fprint(w, `{"number":7,"html_url":"https://github.com/o/r/pull/7","title":"Zoom the preview"}`)
	})

	pr, err := client.CreatePullRequest(context.Background(), "tok", "o", "r", PullRequestDraft{
		Title: "Zoom the preview", Body: "why", Head: "feature/zoom", Base: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	if pr.Number != 7 || pr.URL != "https://github.com/o/r/pull/7" {
		t.Errorf("got %+v", pr)
	}
	if pr.Existing {
		t.Error("a freshly created pull request was marked as existing")
	}
}

// Pressing the button twice is ordinary. GitHub answers the second attempt with
// a 422 that explains nothing, so the existing one is found instead.
func TestOpenPullRequestForFindsAnExistingOne(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("head"); got != "octocat:feature/zoom" {
			t.Errorf("head filter: got %q, want it qualified by owner", got)
		}
		if got := r.URL.Query().Get("state"); got != "open" {
			t.Errorf("state: got %q", got)
		}
		fmt.Fprint(w, `[{"number":7,"html_url":"https://github.com/o/r/pull/7","title":"Already open"}]`)
	})

	pr, found, err := client.OpenPullRequestFor(context.Background(), "tok", "octocat", "r", "feature/zoom")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("an open pull request was not found")
	}
	if !pr.Existing {
		t.Error("the pull request was not marked as existing")
	}
	if pr.Number != 7 {
		t.Errorf("number: got %d", pr.Number)
	}
}

func TestOpenPullRequestForReportsNone(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[]`)
	})

	_, found, err := client.OpenPullRequestFor(context.Background(), "tok", "o", "r", "feature/zoom")
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Error("an empty list was read as a pull request")
	}
}

// A 200 with no URL would otherwise be reported as success with nothing to open.
func TestCreatePullRequestRejectsAReplyWithNoURL(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"number":7}`)
	})
	if _, err := client.CreatePullRequest(context.Background(), "tok", "o", "r", PullRequestDraft{}); err == nil {
		t.Error("a reply with no URL was accepted")
	}
}
