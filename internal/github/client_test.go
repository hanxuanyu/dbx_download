package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dbxdl/internal/naming"
)

func newTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return New(Options{BaseURL: srv.URL, Timeout: 5e9}), srv
}

// releaseFeed mirrors the shape of t8y2/dbx: one release feed carrying the
// application tags plus unrelated packages-* and agents-* families.
const releaseFeed = `[
  {"tag_name":"packages-v0.4.88","name":"packages v0.4.88","draft":false,"prerelease":false,
   "created_at":"2026-09-15T18:00:00Z","published_at":"2026-09-15T18:02:06Z","assets":[]},
  {"tag_name":"v0.6.14","name":"DBX v0.6.14","draft":false,"prerelease":false,
   "created_at":"2026-09-15T17:40:35Z","published_at":"2026-09-15T19:10:49Z",
   "assets":[{"name":"DBX_0.6.14_arm64.dmg","size":33774766,"browser_download_url":"https://example.com/dmg"},
             {"name":"DBX_0.6.14_x64-setup.exe","size":24383320,"browser_download_url":"https://example.com/exe"}]},
  {"tag_name":"agents-v0.2.111","name":"agents v0.2.111","draft":false,"prerelease":false,
   "created_at":"2026-09-15T16:47:22Z","published_at":"2026-09-15T17:16:45Z","assets":[]},
  {"tag_name":"v0.6.13","name":"DBX v0.6.13","draft":false,"prerelease":false,
   "created_at":"2026-09-14T17:41:51Z","published_at":"2026-09-14T20:21:05Z","assets":[]},
  {"tag_name":"v0.7.0-rc.1","name":"DBX v0.7.0-rc.1","draft":false,"prerelease":true,
   "created_at":"2026-09-13T00:00:00Z","published_at":"2026-09-13T00:00:00Z","assets":[]},
  {"tag_name":"v0.5.0","name":"draft release","draft":true,"prerelease":false,
   "created_at":"2026-09-12T00:00:00Z","published_at":"2026-09-12T00:00:00Z","assets":[]}
]`

func serveReleaseFeed(t *testing.T) *Client {
	t.Helper()
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/t8y2/dbx/releases":
			// Only one page: fewer than 100 entries.
			_, _ = w.Write([]byte(releaseFeed))
		case "/repos/t8y2/dbx/releases/tags/v0.6.14":
			var releases []map[string]any
			_ = json.Unmarshal([]byte(releaseFeed), &releases)
			_ = json.NewEncoder(w).Encode(releases[1])
		case "/repos/t8y2/dbx/releases/tags/0.6.14":
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	})
	return client
}

func TestLatestAppReleaseIgnoresOtherTagFamilies(t *testing.T) {
	client := serveReleaseFeed(t)
	rel, err := client.LatestAppRelease(context.Background(), "t8y2", "dbx", naming.DefaultTagPattern, false)
	if err != nil {
		t.Fatalf("LatestAppRelease: %v", err)
	}
	if rel.TagName != "v0.6.14" {
		t.Errorf("tag = %q, want v0.6.14", rel.TagName)
	}
	if len(rel.Assets) != 2 {
		t.Errorf("assets = %d, want 2", len(rel.Assets))
	}
	if rel.Published().UTC().Format("2006-01-02") != "2026-09-15" {
		t.Errorf("published = %v", rel.Published())
	}
}

func TestLatestAppReleaseIncludesPrereleaseWhenAsked(t *testing.T) {
	client := serveReleaseFeed(t)
	rel, err := client.LatestAppRelease(context.Background(), "t8y2", "dbx", naming.DefaultTagPattern, true)
	if err != nil {
		t.Fatalf("LatestAppRelease: %v", err)
	}
	if rel.TagName != "v0.7.0-rc.1" {
		t.Errorf("tag = %q, want v0.7.0-rc.1", rel.TagName)
	}
}

func TestAppReleasesRespectsLimit(t *testing.T) {
	client := serveReleaseFeed(t)
	releases, err := client.AppReleases(context.Background(), "t8y2", "dbx", naming.DefaultTagPattern, false, 2)
	if err != nil {
		t.Fatalf("AppReleases: %v", err)
	}
	if len(releases) != 2 {
		t.Fatalf("got %d releases, want 2", len(releases))
	}
	if releases[0].TagName != "v0.6.14" || releases[1].TagName != "v0.6.13" {
		t.Errorf("releases = %s, %s; want newest first", releases[0].TagName, releases[1].TagName)
	}
}

func TestAppReleasesNoMatch(t *testing.T) {
	client := serveReleaseFeed(t)
	_, err := client.LatestAppRelease(context.Background(), "t8y2", "dbx", `^nope-v(\d+\.\d+\.\d+)$`, false)
	if err == nil {
		t.Fatal("expected an error when nothing matches the tag pattern")
	}
}

func TestReleaseByTagNotFound(t *testing.T) {
	client := serveReleaseFeed(t)
	_, err := client.ReleaseByTag(context.Background(), "t8y2", "dbx", "v9.9.9")
	if err == nil {
		t.Fatal("expected an error for a missing tag")
	}
	if !IsNotFound(err) {
		t.Errorf("IsNotFound(%v) = false, want true", err)
	}
}

// TestResolveReleaseFallsBackToPlainTag covers repositories that tag a release
// without the "v" prefix.
func TestResolveReleaseFallsBackToPlainTag(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/releases/tags/v1.2.3":
			http.NotFound(w, r)
		case "/repos/o/r/releases/tags/1.2.3":
			_, _ = w.Write([]byte(`{"tag_name":"1.2.3","name":"1.2.3","assets":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	client := New(Options{BaseURL: srv.URL})

	version, err := naming.ParseVersion("1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	rel, err := client.ResolveRelease(context.Background(), "o", "r", naming.DefaultTagPattern, version)
	if err != nil {
		t.Fatalf("ResolveRelease: %v", err)
	}
	if rel.TagName != "1.2.3" {
		t.Errorf("tag = %q, want 1.2.3", rel.TagName)
	}
}

func TestRateLimitDetection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", "1800000000")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"API rate limit exceeded for 1.2.3.4."}`))
	}))
	defer srv.Close()
	client := New(Options{BaseURL: srv.URL})

	_, err := client.ListReleases(context.Background(), "o", "r", 1, 10)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !IsRateLimited(err) {
		t.Errorf("IsRateLimited(%v) = false, want true", err)
	}
	var he *HTTPError
	if !errors.As(err, &he) {
		t.Fatalf("expected an *HTTPError, got %T", err)
	}
	if he.Status != http.StatusForbidden || he.Remaining != 0 {
		t.Errorf("HTTPError = %+v", he)
	}
	if he.ResetAt.IsZero() {
		t.Error("expected a parsed reset time")
	}
	if !strings.Contains(he.Error(), "GITHUB_TOKEN") {
		t.Errorf("the error should suggest setting GITHUB_TOKEN: %s", he.Error())
	}
}

// TestForbiddenWithoutRateLimitIsNotRateLimited guards against treating every
// 403 (for example a permissions problem) as quota exhaustion.
func TestForbiddenWithoutRateLimitIsNotRateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "42")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Resource not accessible"}`))
	}))
	defer srv.Close()
	client := New(Options{BaseURL: srv.URL})

	_, err := client.ListReleases(context.Background(), "o", "r", 1, 10)
	if err == nil {
		t.Fatal("expected an error")
	}
	if IsRateLimited(err) {
		t.Errorf("IsRateLimited(%v) = true, want false", err)
	}
}

func TestReleasesAtom(t *testing.T) {
	const feed = `<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <entry>
    <link rel="alternate" type="text/html" href="https://github.com/t8y2/dbx/releases/tag/v0.6.14"/>
    <title>DBX v0.6.14</title>
  </entry>
  <entry>
    <link rel="alternate" type="text/html" href="https://github.com/t8y2/dbx/releases/tag/packages-v0.4.88"/>
    <title>packages v0.4.88</title>
  </entry>
</feed>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/t8y2/dbx/releases.atom" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(feed))
	}))
	defer srv.Close()
	client := New(Options{BaseURL: srv.URL, WebBase: srv.URL})

	tags, err := client.ReleasesAtom(context.Background(), "t8y2", "dbx")
	if err != nil {
		t.Fatalf("ReleasesAtom: %v", err)
	}
	if len(tags) != 2 || tags[0] != "v0.6.14" || tags[1] != "packages-v0.4.88" {
		t.Errorf("tags = %v", tags)
	}
}

func TestTokenFromEnv(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "from-gh")
	if got := TokenFromEnv(); got != "from-gh" {
		t.Errorf("TokenFromEnv = %q, want from-gh", got)
	}
	t.Setenv("GITHUB_TOKEN", "primary")
	if got := TokenFromEnv(); got != "primary" {
		t.Errorf("TokenFromEnv = %q, want primary", got)
	}
}

func TestHasToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	if New(Options{}).HasToken() {
		t.Error("no token is configured")
	}
	if !New(Options{Token: "abc"}).HasToken() {
		t.Error("explicit token should be reported")
	}
}

// TestResponseTooLargeIsReportedClearly guards the read cap: the real dbx
// release feed is ~10 MB for one page, so an undersized cap must produce an
// explicit error rather than a JSON syntax error.
func TestResponseTooLargeIsReportedClearly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("x"), 4096))
	}))
	defer srv.Close()
	client := New(Options{BaseURL: srv.URL})

	original := maxResponseBytes
	maxResponseBytes = 1024
	t.Cleanup(func() { maxResponseBytes = original })

	_, err := client.ListReleases(context.Background(), "o", "r", 1, 10)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Errorf("error = %v, want ErrResponseTooLarge", err)
	}
}

// TestLargeReleaseFeedIsParsed covers the size the real feed reaches.
func TestLargeReleaseFeedIsParsed(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString(`[{"tag_name":"v0.6.14","name":"DBX v0.6.14","assets":[],"notes":"`)
	buf.Write(bytes.Repeat([]byte("n"), 4<<20))
	buf.WriteString(`"}]`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(buf.Bytes())
	}))
	defer srv.Close()
	client := New(Options{BaseURL: srv.URL})

	releases, err := client.ListReleases(context.Background(), "o", "r", 1, 100)
	if err != nil {
		t.Fatalf("ListReleases: %v", err)
	}
	if len(releases) != 1 || releases[0].TagName != "v0.6.14" {
		t.Errorf("releases = %+v", releases)
	}
}
