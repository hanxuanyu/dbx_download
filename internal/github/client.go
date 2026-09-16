// Package github is a minimal read-only client for the GitHub REST API
// endpoints dbxdl needs: listing releases and fetching a release by tag.
//
// It intentionally avoids third-party SDKs so the downloader stays easy to
// audit, and it treats the API as a best-effort source: an unauthenticated
// caller only gets 60 requests per hour, so the caller is expected to cache
// results and fall back to the public Atom feed (see ReleasesAtom) when the
// API refuses to serve a request.
package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"dbxdl/internal/ui"
)

// Options configures a Client.
type Options struct {
	// BaseURL is the API root, normally https://api.github.com.
	BaseURL string
	// WebBase is the website root, normally https://github.com. It is used for
	// the Atom fallback feed.
	WebBase string
	// Token is optional; when empty the GITHUB_TOKEN / GH_TOKEN environment
	// variables are consulted.
	Token     string
	Timeout   time.Duration
	UserAgent string
	Log       *ui.Logger
}

// Client talks to the GitHub REST API.
type Client struct {
	baseURL   string
	webBase   string
	token     string
	http      *http.Client
	userAgent string
	log       *ui.Logger
}

// Release is the subset of the GitHub release payload dbxdl uses.
type Release struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	HTMLURL     string    `json:"html_url"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	CreatedAt   time.Time `json:"created_at"`
	PublishedAt time.Time `json:"published_at"`
	Assets      []Asset   `json:"assets"`
}

// Asset is one downloadable file attached to a release.
type Asset struct {
	ID                 int64  `json:"id"`
	Name               string `json:"name"`
	Size               int64  `json:"size"`
	ContentType        string `json:"content_type"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// DisplayName returns a human-friendly label for the release.
func (r Release) DisplayName() string {
	if r.Name != "" {
		return r.Name
	}
	return r.TagName
}

// Published returns the publication timestamp, falling back to created_at.
func (r Release) Published() time.Time {
	if !r.PublishedAt.IsZero() {
		return r.PublishedAt
	}
	return r.CreatedAt
}

// New builds a Client, resolving the token from the environment when the
// caller did not supply one.
func New(opts Options) *Client {
	base := strings.TrimRight(opts.BaseURL, "/")
	if base == "" {
		base = "https://api.github.com"
	}
	web := strings.TrimRight(opts.WebBase, "/")
	if web == "" {
		web = "https://github.com"
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	ua := opts.UserAgent
	if ua == "" {
		ua = DefaultUserAgent
	}
	token := opts.Token
	if token == "" {
		token = TokenFromEnv()
	}
	return &Client{
		baseURL:   base,
		webBase:   web,
		token:     token,
		userAgent: ua,
		log:       opts.Log,
		http:      &http.Client{Timeout: timeout},
	}
}

// DefaultUserAgent identifies dbxdl to GitHub.
const DefaultUserAgent = "dbxdl (+https://github.com/t8y2/dbx)"

// TokenFromEnv returns the first non-empty token from GITHUB_TOKEN or
// GH_TOKEN.
func TokenFromEnv() string {
	for _, key := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	return ""
}

// HasToken reports whether requests will be authenticated.
func (c *Client) HasToken() bool { return c.token != "" }

// HTTPError describes a non-2xx API response.
type HTTPError struct {
	Status    int
	Method    string
	URL       string
	Message   string
	ResetAt   time.Time
	Remaining int
}

func (e *HTTPError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "github api %s %s: HTTP %d", e.Method, e.URL, e.Status)
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	if e.Status == http.StatusForbidden || e.Status == http.StatusTooManyRequests {
		if !e.ResetAt.IsZero() {
			fmt.Fprintf(&b, " (rate limit resets at %s, %s from now)",
				e.ResetAt.UTC().Format(time.RFC3339), time.Until(e.ResetAt).Round(time.Second))
		}
		b.WriteString("; set GITHUB_TOKEN to raise the limit from 60 to 5000 requests/hour")
	}
	return b.String()
}

// IsNotFound reports whether err is a 404 from the API.
func IsNotFound(err error) bool {
	var he *HTTPError
	return errors.As(err, &he) && he.Status == http.StatusNotFound
}

// IsRateLimited reports whether err is a 403/429 rate-limit refusal.
func IsRateLimited(err error) bool {
	var he *HTTPError
	if !errors.As(err, &he) {
		return false
	}
	if he.Status == http.StatusTooManyRequests {
		return true
	}
	return he.Status == http.StatusForbidden &&
		(he.Remaining == 0 || strings.Contains(strings.ToLower(he.Message), "rate limit"))
}

// maxResponseBytes bounds how much of an API response is read into memory.
// The t8y2/dbx release feed is unusually large (release notes are long): one
// page of 100 releases is already ~10 MB, so a small cap silently truncates
// the JSON and produces a confusing parse error.
var maxResponseBytes int64 = 32 << 20

// ErrResponseTooLarge reports a response that exceeded maxResponseBytes.
var ErrResponseTooLarge = errors.New("github api response too large")

func (c *Client) get(ctx context.Context, rawURL string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", c.userAgent)
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("github api GET %s: %w", rawURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("github api GET %s: read body: %w", rawURL, err)
	}
	if int64(len(body)) > maxResponseBytes {
		return fmt.Errorf("%w: %s returned more than %d bytes", ErrResponseTooLarge, redact(rawURL), maxResponseBytes)
	}

	if c.log != nil {
		if remaining := resp.Header.Get("X-RateLimit-Remaining"); remaining != "" {
			c.log.Debugf("github api %s -> HTTP %d (rate limit remaining: %s)", redact(rawURL), resp.StatusCode, remaining)
		}
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		he := &HTTPError{
			Status:  resp.StatusCode,
			Method:  http.MethodGet,
			URL:     redact(rawURL),
			Message: apiMessage(body),
		}
		if v := resp.Header.Get("X-RateLimit-Remaining"); v != "" {
			he.Remaining, _ = strconv.Atoi(v)
		}
		if v := resp.Header.Get("X-RateLimit-Reset"); v != "" {
			if secs, convErr := strconv.ParseInt(v, 10, 64); convErr == nil {
				he.ResetAt = time.Unix(secs, 0)
			}
		}
		return he
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("github api GET %s: decode response: %w", rawURL, err)
	}
	return nil
}

// apiMessage extracts the "message" field from a GitHub error payload.
func apiMessage(body []byte) string {
	var payload struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &payload); err == nil && payload.Message != "" {
		return payload.Message
	}
	trimmed := strings.TrimSpace(string(body))
	if len(trimmed) > 200 {
		trimmed = trimmed[:200] + "..."
	}
	return trimmed
}

// redact hides tokens that may appear in query strings.
func redact(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	if u.RawQuery != "" {
		u.RawQuery = "redacted"
	}
	return u.String()
}

// ListReleases returns a single page of releases, newest first.
func (c *Client) ListReleases(ctx context.Context, owner, repo string, page, perPage int) ([]Release, error) {
	if perPage <= 0 || perPage > 100 {
		perPage = 100
	}
	if page <= 0 {
		page = 1
	}
	rawURL := fmt.Sprintf("%s/repos/%s/%s/releases?per_page=%d&page=%d",
		c.baseURL, url.PathEscape(owner), url.PathEscape(repo), perPage, page)
	var releases []Release
	if err := c.get(ctx, rawURL, &releases); err != nil {
		return nil, err
	}
	return releases, nil
}

// ReleaseByTag fetches one release. The tag is used verbatim.
func (c *Client) ReleaseByTag(ctx context.Context, owner, repo, tag string) (*Release, error) {
	rawURL := fmt.Sprintf("%s/repos/%s/%s/releases/tags/%s",
		c.baseURL, url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(tag))
	var rel Release
	if err := c.get(ctx, rawURL, &rel); err != nil {
		return nil, err
	}
	return &rel, nil
}
