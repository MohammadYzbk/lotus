// Package update asks GitHub whether a newer Lotus has been released.
//
// It checks and reports; it does not replace anything. Installing an update in
// place means writing over a running, signed application, which on macOS and
// Windows is the business of a framework with its own update signing — and
// getting that wrong turns a working install into one the OS refuses to
// launch. Until Lotus has signed releases to feed such a framework, telling
// the writer there is a new version and opening the page is the honest and
// safe half of the job.
package update

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DevVersion is the version a build carries when none was stamped in. A
// developer build is never "out of date": it is not on the release line at all.
const DevVersion = "dev"

// Result is the answer to "is there anything newer?".
type Result struct {
	// Current is the running version.
	Current string `json:"current"`
	// Latest is the newest published version, empty if the check failed.
	Latest string `json:"latest"`
	// URL is the release page, for the writer to go and get it.
	URL string `json:"url"`
	// Newer is true when Latest is actually ahead of Current.
	Newer bool `json:"newer"`
	// Development is true for an unstamped build, where the question does not
	// apply and no comparison was made.
	Development bool `json:"development"`
	// Error explains a check that could not be completed.
	Error string `json:"error"`
}

// release is the part of GitHub's release payload we use.
type release struct {
	TagName    string `json:"tag_name"`
	HTMLURL    string `json:"html_url"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
}

// Checker queries a repository's releases.
type Checker struct {
	// Repo is "owner/name".
	Repo string
	// BaseURL is the API root. Empty means GitHub's.
	BaseURL string
	// Client is the HTTP client. Nil means a client with a short timeout.
	Client *http.Client
}

const defaultBaseURL = "https://api.github.com"

// Check reports whether anything newer than current has been released.
//
// A failed check is not an error the caller has to handle: being offline is
// the normal state of an offline-first editor, and it returns a Result whose
// Error explains itself rather than something that interrupts the writer.
func (c Checker) Check(ctx context.Context, current string) Result {
	out := Result{Current: current}
	if current == "" || current == DevVersion {
		out.Development = true
		out.Current = DevVersion
	}

	base := c.BaseURL
	if base == "" {
		base = defaultBaseURL
	}
	client := c.Client
	if client == nil {
		// Short: this runs behind a menu action and on a timer, and a check
		// that hangs is worse than one that says it could not reach GitHub.
		client = &http.Client{Timeout: 15 * time.Second}
	}

	url := fmt.Sprintf("%s/repos/%s/releases/latest", strings.TrimSuffix(base, "/"), c.Repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := client.Do(req)
	if err != nil {
		out.Error = "could not reach GitHub"
		return out
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		// A repository with no published release answers 404 here, which is a
		// fact about the project rather than a failure.
		out.Error = "no releases have been published yet"
		return out
	default:
		out.Error = fmt.Sprintf("GitHub answered %s", resp.Status)
		return out
	}

	var latest release
	if err := json.NewDecoder(resp.Body).Decode(&latest); err != nil {
		out.Error = "GitHub's answer could not be read"
		return out
	}
	if latest.Draft || latest.Prerelease {
		out.Error = "no stable release has been published yet"
		return out
	}

	out.Latest = strings.TrimPrefix(latest.TagName, "v")
	out.URL = latest.HTMLURL
	if !out.Development {
		out.Newer = Compare(out.Latest, out.Current) > 0
	}
	return out
}

// Compare orders two versions: -1 if a is older than b, 0 if they are the
// same, 1 if a is newer.
//
// Dotted numbers, with a "v" prefix and a pre-release suffix both tolerated.
// A pre-release sorts before the release it leads to, so 1.2.0-rc1 is older
// than 1.2.0 — the rule every release convention agrees on, and the one that
// decides whether a release candidate thinks it is out of date.
func Compare(a, b string) int {
	aNums, aPre := split(a)
	bNums, bPre := split(b)

	for i := 0; i < len(aNums) || i < len(bNums); i++ {
		if d := at(aNums, i) - at(bNums, i); d != 0 {
			return sign(d)
		}
	}

	switch {
	case aPre == "" && bPre == "":
		return 0
	case aPre == "":
		return 1 // a release beats the pre-release leading to it
	case bPre == "":
		return -1
	default:
		return sign(strings.Compare(aPre, bPre))
	}
}

// split separates the dotted numbers from any pre-release suffix.
func split(v string) ([]int, string) {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")

	// Build metadata is not part of precedence, so it is dropped outright.
	if plus := strings.IndexByte(v, '+'); plus >= 0 {
		v = v[:plus]
	}
	pre := ""
	if dash := strings.IndexByte(v, '-'); dash >= 0 {
		v, pre = v[:dash], v[dash+1:]
	}

	var nums []int
	for part := range strings.SplitSeq(v, ".") {
		n, err := strconv.Atoi(part)
		if err != nil {
			// An unparseable segment stops the numeric run rather than
			// poisoning the comparison: "1.2.x" compares as 1.2.
			break
		}
		nums = append(nums, n)
	}
	return nums, pre
}

func at(nums []int, i int) int {
	if i < len(nums) {
		return nums[i]
	}
	// A missing segment is zero, so 1.2 and 1.2.0 are the same version.
	return 0
}

func sign(n int) int {
	switch {
	case n > 0:
		return 1
	case n < 0:
		return -1
	default:
		return 0
	}
}
