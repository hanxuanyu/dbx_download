// Package store reads DBX plugin metadata from the t8y2/dbx-store repository.
//
// The store keeps one JSON document per plugin under plugins/<id>.json holding
// every published version and, per target platform, the download URL, size and
// sha256 digest. A consolidated catalog lives at catalog/index.json, which is
// convenient for listing but only retains a trimmed version history; the
// per-plugin documents are authoritative.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"dbxdl/internal/naming"
	"dbxdl/internal/ui"
)

// Options configures a Client.
type Options struct {
	Owner       string
	Repo        string
	Ref         string
	PluginsPath string
	// APIBase is the GitHub API root used as a fallback when raw
	// content is unavailable (for example a private fork).
	APIBase string
	// RawBase is the raw-content host, normally
	// https://raw.githubusercontent.com. It is configurable so tests (and
	// GitHub Enterprise mirrors) can point it elsewhere.
	RawBase   string
	Token     string
	Timeout   time.Duration
	UserAgent string
	Log       *ui.Logger
}

// Client reads plugin metadata.
type Client struct {
	owner       string
	repo        string
	ref         string
	pluginsPath string
	apiBase     string
	rawBase     string
	token       string
	userAgent   string
	http        *http.Client
	log         *ui.Logger
}

// Artifact is one downloadable plugin binary for a target platform.
type Artifact struct {
	Target       string `json:"target"`
	URL          string `json:"url"`
	SHA256       string `json:"sha256"`
	SigningKeyID string `json:"signingKeyId"`
	Size         int64  `json:"size"`
}

// Version is one published plugin version.
type Version struct {
	Version      string     `json:"version"`
	ReleasedAt   time.Time  `json:"releasedAt"`
	ReleaseNotes string     `json:"releaseNotes"`
	Artifacts    []Artifact `json:"artifacts"`
}

// ReleasedAtString renders the release date, or "-" when unknown.
func (v Version) ReleasedAtString() string {
	if v.ReleasedAt.IsZero() {
		return "-"
	}
	return v.ReleasedAt.UTC().Format("2006-01-02")
}

// Targets lists the artifact targets available for this version.
func (v Version) Targets() []string {
	out := make([]string, 0, len(v.Artifacts))
	for _, a := range v.Artifacts {
		out = append(out, a.Target)
	}
	sort.Strings(out)
	return out
}

// ArtifactFor returns the artifact for a target.
func (v Version) ArtifactFor(target string) (Artifact, bool) {
	for _, a := range v.Artifacts {
		if a.Target == target {
			return a, true
		}
	}
	return Artifact{}, false
}

// Plugin is a plugin metadata document.
type Plugin struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Description   string    `json:"description"`
	Publisher     string    `json:"publisher"`
	Verified      bool      `json:"verified"`
	Icon          string    `json:"icon"`
	Tags          []string  `json:"tags"`
	Source        string    `json:"source"`
	Homepage      string    `json:"homepage"`
	License       string    `json:"license"`
	LatestVersion string    `json:"latestVersion"`
	Versions      []Version `json:"versions"`
}

// VersionStrings lists the published versions, newest first.
func (p *Plugin) VersionStrings() []string {
	versions := make([]naming.Version, 0, len(p.Versions))
	byString := map[string]string{}
	for _, v := range p.Versions {
		parsed, err := naming.ParseVersion(v.Version)
		if err != nil {
			continue
		}
		versions = append(versions, parsed)
		byString[parsed.String()] = v.Version
	}
	sort.Slice(versions, func(i, j int) bool { return naming.Compare(versions[i], versions[j]) > 0 })
	out := make([]string, 0, len(versions))
	for _, v := range versions {
		out = append(out, byString[v.String()])
	}
	return out
}

// New builds a Client.
func New(opts Options) *Client {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	ua := opts.UserAgent
	if ua == "" {
		ua = "dbxdl (+https://github.com/t8y2/dbx)"
	}
	owner := opts.Owner
	if owner == "" {
		owner = "t8y2"
	}
	repo := opts.Repo
	if repo == "" {
		repo = "dbx-store"
	}
	ref := opts.Ref
	if ref == "" {
		ref = "main"
	}
	path := strings.Trim(opts.PluginsPath, "/")
	if path == "" {
		path = "plugins"
	}
	apiBase := strings.TrimRight(opts.APIBase, "/")
	if apiBase == "" {
		apiBase = "https://api.github.com"
	}
	rawBase := strings.TrimRight(opts.RawBase, "/")
	if rawBase == "" {
		rawBase = "https://raw.githubusercontent.com"
	}
	return &Client{
		owner:       owner,
		repo:        repo,
		ref:         ref,
		pluginsPath: path,
		apiBase:     apiBase,
		rawBase:     rawBase,
		token:       opts.Token,
		userAgent:   ua,
		log:         opts.Log,
		http:        &http.Client{Timeout: timeout},
	}
}

// PluginURL returns the raw metadata URL for a plugin id.
func (c *Client) PluginURL(id string) string {
	return fmt.Sprintf("%s/%s/%s/%s/%s/%s.json",
		c.rawBase, url.PathEscape(c.owner), url.PathEscape(c.repo), url.PathEscape(c.ref),
		c.pluginsPath, url.PathEscape(id))
}

// CatalogURL returns the raw URL of the consolidated catalog.
func (c *Client) CatalogURL() string {
	return fmt.Sprintf("%s/%s/%s/%s/catalog/index.json",
		c.rawBase, url.PathEscape(c.owner), url.PathEscape(c.repo), url.PathEscape(c.ref))
}

// Catalog is the trimmed marketplace index.
type Catalog struct {
	CatalogVersion int      `json:"catalogVersion"`
	Plugins        []Plugin `json:"plugins"`
}

// Plugin fetches one plugin document. Raw content is tried first because it
// does not consume GitHub API quota; the REST contents API is the fallback.
func (c *Client) Plugin(ctx context.Context, id string) (*Plugin, error) {
	if strings.TrimSpace(id) == "" {
		return nil, errors.New("empty plugin id")
	}
	body, err := c.fetch(ctx, c.PluginURL(id), false)
	if err != nil {
		if c.log != nil {
			c.log.Debugf("raw metadata for %s failed (%v); trying the contents API", id, err)
		}
		apiURL := fmt.Sprintf("%s/repos/%s/%s/contents/%s/%s.json?ref=%s",
			c.apiBase, url.PathEscape(c.owner), url.PathEscape(c.repo),
			c.pluginsPath, url.PathEscape(id), url.PathEscape(c.ref))
		body, err = c.fetch(ctx, apiURL, true)
		if err != nil {
			return nil, fmt.Errorf("fetch metadata for plugin %s: %w", id, err)
		}
	}
	var plugin Plugin
	if err := json.Unmarshal(body, &plugin); err != nil {
		return nil, fmt.Errorf("parse metadata for plugin %s: %w", id, err)
	}
	if plugin.ID == "" {
		plugin.ID = id
	}
	if len(plugin.Versions) == 0 {
		return nil, fmt.Errorf("plugin %s: metadata lists no versions", id)
	}
	return &plugin, nil
}

// Catalog fetches the consolidated marketplace index.
func (c *Client) Catalog(ctx context.Context) (*Catalog, error) {
	body, err := c.fetch(ctx, c.CatalogURL(), false)
	if err != nil {
		return nil, err
	}
	var catalog Catalog
	if err := json.Unmarshal(body, &catalog); err != nil {
		return nil, fmt.Errorf("parse catalog: %w", err)
	}
	return &catalog, nil
}

// RevokedPluginVersion is one withdrawn plugin version.
type RevokedPluginVersion struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	Reason  string `json:"reason"`
}

// RevokedSigningKey is one withdrawn signing key.
type RevokedSigningKey struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// Revoked lists plugin versions and signing keys withdrawn by the store.
type Revoked struct {
	Version        int                    `json:"version"`
	PluginVersions []RevokedPluginVersion `json:"pluginVersions"`
	SigningKeys    []RevokedSigningKey    `json:"signingKeys"`
}

// RevokedURL returns the raw URL of revoked.json.
func (c *Client) RevokedURL() string {
	return fmt.Sprintf("%s/%s/%s/%s/revoked.json",
		c.rawBase, url.PathEscape(c.owner), url.PathEscape(c.repo), url.PathEscape(c.ref))
}

// Revoked fetches the revocation list. Callers should treat a failure as
// non-fatal: the list is an advisory integrity signal.
func (c *Client) Revoked(ctx context.Context) (*Revoked, error) {
	body, err := c.fetch(ctx, c.RevokedURL(), false)
	if err != nil {
		return nil, err
	}
	var revoked Revoked
	if err := json.Unmarshal(body, &revoked); err != nil {
		return nil, fmt.Errorf("parse revoked.json: %w", err)
	}
	return &revoked, nil
}

// IsRevoked reports whether the store withdrew a plugin version.
func (r *Revoked) IsRevoked(id, version string) (string, bool) {
	if r == nil {
		return "", false
	}
	for _, pv := range r.PluginVersions {
		if pv.ID == id && pv.Version == version {
			reason := pv.Reason
			if reason == "" {
				reason = "no reason given"
			}
			return reason, true
		}
	}
	return "", false
}

func (c *Client) fetch(ctx context.Context, rawURL string, viaAPI bool) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.userAgent)
	if viaAPI {
		req.Header.Set("Accept", "application/vnd.github.raw")
		token := c.token
		if token == "" {
			token = tokenFromEnv()
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", rawURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("GET %s: read body: %w", rawURL, err)
	}
	if resp.StatusCode != http.StatusOK {
		hint := ""
		if resp.StatusCode == http.StatusNotFound {
			hint = " (check the plugin id and store.ref)"
		}
		return nil, fmt.Errorf("GET %s: HTTP %d%s: %s", rawURL, resp.StatusCode, hint, strings.TrimSpace(firstLine(body)))
	}
	return body, nil
}

func firstLine(b []byte) string {
	s := string(b)
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		s = s[:idx]
	}
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

func tokenFromEnv() string {
	for _, key := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	return ""
}

// ResolveVersion picks the plugin version to download.
//
//   - requested empty or "latest": the newest published version (the store's
//     latestVersion field when it resolves, otherwise the highest semver).
//   - requested exact ("0.4.76"): that version.
//   - requested partial ("0.4", "0.4.x", "0.4.*"): the highest version with
//     that prefix.
//
// policy "latest" ignores requested entirely.
func ResolveVersion(p *Plugin, requested, policy string) (*Version, error) {
	if p == nil {
		return nil, errors.New("nil plugin")
	}
	if policy == "latest" {
		requested = "latest"
	}
	requested = strings.TrimSpace(requested)

	if requested == "" || strings.EqualFold(requested, "latest") {
		if p.LatestVersion != "" {
			if v := p.find(p.LatestVersion); v != nil {
				return v, nil
			}
		}
		highest := p.highest("")
		if highest == nil {
			return nil, fmt.Errorf("plugin %s has no parseable versions", p.ID)
		}
		return highest, nil
	}

	if v := p.find(requested); v != nil {
		return v, nil
	}

	prefix := strings.TrimSuffix(strings.TrimSuffix(requested, "x"), "*")
	prefix = strings.TrimSuffix(prefix, ".")
	if prefix != "" && prefix != requested {
		if v := p.highest(prefix + "."); v != nil {
			return v, nil
		}
	}
	return nil, fmt.Errorf("plugin %s has no version %q; available: %s",
		p.ID, requested, strings.Join(p.VersionStrings(), ", "))
}

func (p *Plugin) find(version string) *Version {
	want, err := naming.ParseVersion(version)
	if err != nil {
		return nil
	}
	for i := range p.Versions {
		got, err := naming.ParseVersion(p.Versions[i].Version)
		if err != nil {
			continue
		}
		if naming.Compare(got, want) == 0 {
			return &p.Versions[i]
		}
	}
	return nil
}

// highest returns the newest version whose string starts with prefix.
func (p *Plugin) highest(prefix string) *Version {
	var best *Version
	var bestVersion naming.Version
	for i := range p.Versions {
		if !strings.HasPrefix(p.Versions[i].Version, prefix) {
			continue
		}
		got, err := naming.ParseVersion(p.Versions[i].Version)
		if err != nil {
			continue
		}
		if best == nil || naming.Compare(got, bestVersion) > 0 {
			best = &p.Versions[i]
			bestVersion = got
		}
	}
	return best
}

// GitHubRepo returns the "owner/repo" of the plugin's own GitHub repository.
//
// The metadata records it in "homepage"; "source" is the fallback. Note that
// "source" also carries a tag which can lag behind latestVersion (the store
// does not always refresh it), so only the repository is taken from here.
func GitHubRepo(p *Plugin) (string, bool) {
	const prefix = "https://github.com/"
	for _, raw := range []string{p.Homepage, p.Source} {
		if !strings.HasPrefix(raw, prefix) {
			continue
		}
		rest := strings.TrimPrefix(raw, prefix)
		parts := strings.SplitN(rest, "/", 3)
		if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
			continue
		}
		return parts[0] + "/" + strings.TrimSuffix(parts[1], ".git"), true
	}
	return "", false
}

// GitHubTagHint returns the tag recorded in the metadata "source" field, but
// only when it actually refers to the requested version. A stale hint (for
// example "tree/v0.1.3" while bundling 0.1.9) is rejected so the caller falls
// back to querying the release list.
func GitHubTagHint(p *Plugin, version string) (string, bool) {
	const marker = "/tree/"
	idx := strings.Index(p.Source, marker)
	if idx < 0 {
		return "", false
	}
	tag := strings.Trim(p.Source[idx+len(marker):], "/")
	if tag == "" || !strings.Contains(tag, version) {
		return "", false
	}
	return tag, true
}

// GitHubAssetName is the file name plugins publish for one target.
func GitHubAssetName(p *Plugin, version, target string) string {
	return fmt.Sprintf("%s-%s-%s.dbxp", p.ID, version, target)
}
