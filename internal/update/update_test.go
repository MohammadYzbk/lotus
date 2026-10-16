package update

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"1.0.1", "1.0.0", 1},
		{"1.0.0", "1.0.1", -1},
		{"1.1.0", "1.0.9", 1},
		{"2.0.0", "1.99.99", 1},
		// A "v" prefix is how the tag is written, not a different version.
		{"v1.2.0", "1.2.0", 0},
		// Missing segments are zero, so these are the same release.
		{"1.2", "1.2.0", 0},
		{"1.2.0.1", "1.2.0", 1},
		// Ten sorts above nine, which a string comparison would get wrong.
		{"1.10.0", "1.9.0", 1},
		// A pre-release comes before the release it leads to.
		{"1.2.0-rc1", "1.2.0", -1},
		{"1.2.0", "1.2.0-rc1", 1},
		{"1.2.0-rc1", "1.2.0-rc2", -1},
		// Build metadata does not affect precedence.
		{"1.2.0+build7", "1.2.0", 0},
	}
	for _, c := range cases {
		if got := Compare(c.a, c.b); got != c.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// serveRelease stands in for the GitHub releases endpoint.
func serveRelease(t *testing.T, status int, body string) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/releases/latest") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func TestCheckFindsANewerRelease(t *testing.T) {
	base := serveRelease(t, http.StatusOK,
		`{"tag_name":"v1.3.0","html_url":"https://example.test/releases/v1.3.0"}`)

	got := Checker{Repo: "owner/lotus", BaseURL: base}.Check(t.Context(), "1.2.0")
	if got.Error != "" {
		t.Fatalf("check failed: %s", got.Error)
	}
	if !got.Newer {
		t.Error("1.3.0 was not reported as newer than 1.2.0")
	}
	if got.Latest != "1.3.0" {
		t.Errorf("latest: got %q", got.Latest)
	}
	if got.URL == "" {
		t.Error("no release page to send the writer to")
	}
}

func TestCheckIsQuietWhenUpToDate(t *testing.T) {
	base := serveRelease(t, http.StatusOK, `{"tag_name":"v1.2.0","html_url":"https://example.test/r"}`)

	got := Checker{Repo: "owner/lotus", BaseURL: base}.Check(t.Context(), "1.2.0")
	if got.Newer {
		t.Error("the running version was reported as out of date")
	}
	if got.Error != "" {
		t.Errorf("unexpected error: %s", got.Error)
	}
}

// Running a build newer than the last release — which is every developer,
// every day — must not be reported as an available downgrade.
func TestCheckDoesNotOfferAnOlderRelease(t *testing.T) {
	base := serveRelease(t, http.StatusOK, `{"tag_name":"v1.1.0","html_url":"https://example.test/r"}`)

	got := Checker{Repo: "owner/lotus", BaseURL: base}.Check(t.Context(), "1.2.0")
	if got.Newer {
		t.Error("an older release was offered as an update")
	}
}

func TestCheckSaysNothingToADevelopmentBuild(t *testing.T) {
	base := serveRelease(t, http.StatusOK, `{"tag_name":"v9.9.9","html_url":"https://example.test/r"}`)

	got := Checker{Repo: "owner/lotus", BaseURL: base}.Check(t.Context(), DevVersion)
	if !got.Development {
		t.Error("a dev build was not recognised as one")
	}
	if got.Newer {
		t.Error("a dev build was told it is out of date")
	}
	// It still reports what the latest release is, which is useful to know.
	if got.Latest != "9.9.9" {
		t.Errorf("latest: got %q", got.Latest)
	}
}

// A project with no releases yet is a fact, not a malfunction.
func TestCheckExplainsAnEmptyReleaseList(t *testing.T) {
	base := serveRelease(t, http.StatusNotFound, `{"message":"Not Found"}`)

	got := Checker{Repo: "owner/lotus", BaseURL: base}.Check(t.Context(), "1.0.0")
	if !strings.Contains(got.Error, "no releases") {
		t.Errorf("unhelpful error: %q", got.Error)
	}
	if got.Newer {
		t.Error("an update was offered by a failed check")
	}
}

func TestCheckReportsAnUnexpectedStatus(t *testing.T) {
	base := serveRelease(t, http.StatusForbidden, `{"message":"rate limited"}`)

	got := Checker{Repo: "owner/lotus", BaseURL: base}.Check(t.Context(), "1.0.0")
	if got.Error == "" {
		t.Fatal("a 403 was treated as a successful check")
	}
	if !strings.Contains(got.Error, "403") {
		t.Errorf("the error does not carry the status: %q", got.Error)
	}
}

// Being offline is the ordinary state of an offline-first editor, so a failed
// check reports itself rather than surfacing as a broken feature.
func TestCheckSurvivesAnUnreachableServer(t *testing.T) {
	// A port nothing is listening on, reserved by RFC 6890 for documentation.
	// The client's own timeout is short here so the suite does not wait out
	// the production one.
	checker := Checker{
		Repo:    "owner/lotus",
		BaseURL: "http://192.0.2.1:1",
		Client:  &http.Client{Timeout: 250 * time.Millisecond},
	}
	got := checker.Check(t.Context(), "1.0.0")
	if got.Error == "" {
		t.Error("an unreachable server was treated as a successful check")
	}
	if got.Newer {
		t.Error("an update was offered by a failed check")
	}
}

func TestCheckIgnoresADraftOrPrerelease(t *testing.T) {
	for _, body := range []string{
		`{"tag_name":"v2.0.0","draft":true,"html_url":"https://example.test/r"}`,
		`{"tag_name":"v2.0.0","prerelease":true,"html_url":"https://example.test/r"}`,
	} {
		base := serveRelease(t, http.StatusOK, body)
		got := Checker{Repo: "owner/lotus", BaseURL: base}.Check(t.Context(), "1.0.0")
		if got.Newer {
			t.Errorf("an unpublished release was offered as an update: %s", body)
		}
	}
}

func TestCheckReportsUnreadableJSON(t *testing.T) {
	base := serveRelease(t, http.StatusOK, `not json`)

	got := Checker{Repo: "owner/lotus", BaseURL: base}.Check(t.Context(), "1.0.0")
	if got.Error == "" {
		t.Error("unreadable JSON was treated as a successful check")
	}
}
