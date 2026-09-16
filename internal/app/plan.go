// Package app turns configuration plus command-line options into a concrete
// download plan and executes it.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"dbxdl/internal/config"
	"dbxdl/internal/fetch"
	"dbxdl/internal/github"
	"dbxdl/internal/naming"
	"dbxdl/internal/store"
	"dbxdl/internal/ui"
)

// Kind distinguishes the two halves of a bundle.
type Kind string

const (
	// KindRelease is a DBX application package from GitHub Releases.
	KindRelease Kind = "dbx"
	// KindPlugin is a DBX plugin artifact.
	KindPlugin Kind = "plugin"
)

// Item is one file in the plan.
type Item struct {
	Kind Kind
	// Label is shown in progress output.
	Label string
	// RelPath is the path inside the bundle directory, always slash-separated.
	RelPath string
	URL     string
	SHA256  string
	// Size is the expected size in bytes, or -1 when unknown.
	Size int64
	// Note describes provenance for the summary table.
	Note string
}

// Plan is a fully resolved, ready-to-execute download plan.
type Plan struct {
	Version naming.Version
	// DirName is the bundle directory name, e.g. "dbx0.6.14".
	DirName string
	// Dir is the bundle directory path on disk.
	Dir string
	// ArchivePath is where the tar file will be written.
	ArchivePath string
	Items       []Item
	// Release is the resolved GitHub release.
	Release *github.Release
	// Plugins records the resolved plugin versions for the summary.
	Plugins []ResolvedPlugin
	// Warnings are non-fatal problems (missing optional artifacts).
	Warnings []string
	// RevokedWarnings lists plugin versions withdrawn by the store.
	RevokedWarnings []string
	// Degraded reports that the GitHub API was unavailable (quota exhausted)
	// and release details were derived from the Atom feed and configured asset
	// names instead. File sizes cannot be verified in that mode.
	Degraded bool
	// DegradedReason is the underlying API error, kept for diagnostics.
	DegradedReason error
}

// ResolvedPlugin is one plugin version selected for the bundle.
type ResolvedPlugin struct {
	ID      string
	Name    string
	Version string
	SubDir  string
	Targets []string
	Source  string
}

// Options are the command-line overrides applied on top of the configuration.
type Options struct {
	// Version is "latest", empty (=latest) or an explicit version.
	Version string
	// OutDir overrides output.dir.
	OutDir string
	// Assets adds extra asset selectors to the configured ones.
	Assets []string
	// IncludePlugins and IncludeRelease select which halves to build.
	IncludeRelease bool
	IncludePlugins bool
	// AllowMissing downgrades a missing release asset from error to warning.
	AllowMissing bool
	// PluginVersions overrides pinned plugin versions, keyed by plugin id.
	PluginVersions map[string]string
	// PluginLatest forces plugins.version_policy=latest.
	PluginLatest bool
	// Force, Concurrency, Retries override the matching output settings when
	// non-zero.
	Force       bool
	Concurrency int
	Retries     int
	// ArchiveFormat overrides archive.format.
	ArchiveFormat string
	// NoArchive skips writing the tar file.
	NoArchive bool
	// DryRun plans without downloading.
	DryRun bool
}

// Planner resolves remote metadata into a Plan.
type Planner struct {
	cfg    *config.Config
	gh     *github.Client
	store  *store.Client
	log    *ui.Logger
	client *http.Client

	// githubAssets caches, per "plugin@version", the release assets published
	// by the plugin's own repository. Planning is single-goroutine.
	githubAssets map[string]map[string]github.Asset
	githubErrs   map[string]error

	// degraded records that release information had to be derived without the
	// GitHub API because the anonymous quota was exhausted.
	degraded       bool
	degradedReason error
}

// NewPlanner wires up the metadata clients.
func NewPlanner(cfg *config.Config, log *ui.Logger) *Planner {
	gh := github.New(github.Options{
		BaseURL: cfg.GitHub.APIBase,
		WebBase: cfg.GitHub.WebBase,
		Token:   cfg.GitHub.Token,
		Timeout: cfg.GitHub.Timeout.Duration(),
		Log:     log,
	})
	st := store.New(store.Options{
		Owner:       cfg.Store.Owner,
		Repo:        cfg.Store.Repo,
		Ref:         cfg.Store.Ref,
		PluginsPath: cfg.Store.PluginsPath,
		RawBase:     cfg.Store.RawBase,
		APIBase:     cfg.GitHub.APIBase,
		Token:       cfg.GitHub.Token,
		Timeout:     cfg.Store.Timeout.Duration(),
		Log:         log,
	})
	return &Planner{
		cfg:          cfg,
		gh:           gh,
		store:        st,
		log:          log,
		client:       &http.Client{Timeout: 60 * time.Second},
		githubAssets: map[string]map[string]github.Asset{},
		githubErrs:   map[string]error{},
	}
}

// ResolveVersion returns the version to bundle, querying GitHub when the
// request is "latest".
//
// When the GitHub API is rate limited it degrades instead of failing: the
// version comes from the public Atom feed (or from the explicit request) and
// the release is synthesized from the configured asset names, whose URLs are
// fully determined by the version. Sizes cannot be checked in that mode, so
// the plan is flagged as degraded.
func (p *Planner) ResolveVersion(ctx context.Context, requested string) (naming.Version, *github.Release, error) {
	version, release, err := p.resolveVersionAPI(ctx, requested)
	if err == nil {
		return version, release, nil
	}
	if !github.IsRateLimited(err) {
		return naming.Version{}, nil, err
	}

	p.degraded = true
	p.degradedReason = err

	if v, ok := explicitVersion(requested); ok {
		release, buildErr := p.syntheticRelease(v)
		if buildErr != nil {
			return naming.Version{}, nil, fmt.Errorf("%w; %v", err, buildErr)
		}
		return v, release, nil
	}

	// "latest": the Atom feed carries the same tags and costs no quota.
	versions, atomErr := p.gh.AtomVersions(ctx, p.cfg.GitHub.Owner, p.cfg.GitHub.Repo,
		p.cfg.GitHub.TagPattern, p.cfg.GitHub.IncludePrerelease, 1)
	if atomErr != nil {
		return naming.Version{}, nil, fmt.Errorf("%w; the Atom fallback also failed: %v", err, atomErr)
	}
	release, buildErr := p.syntheticRelease(versions[0])
	if buildErr != nil {
		return naming.Version{}, nil, fmt.Errorf("%w; %v", err, buildErr)
	}
	return versions[0], release, nil
}

// explicitVersion parses a pinned version request, reporting false for
// "latest" or empty input.
func explicitVersion(requested string) (naming.Version, bool) {
	requested = strings.TrimSpace(requested)
	if requested == "" || strings.EqualFold(requested, "latest") {
		return naming.Version{}, false
	}
	v, err := naming.ParseVersion(requested)
	if err != nil {
		return naming.Version{}, false
	}
	return v, true
}

// syntheticRelease builds a release from the configured asset selectors when
// the API is unavailable. Release asset URLs are fully determined by the tag
// and file name, so no API call is needed; only the size is unknown.
//
// A wildcard selector cannot be expanded without the API. Rather than silently
// ship an incomplete bundle, that is a hard error: the caller must supply a
// token or use literal selectors.
func (p *Planner) syntheticRelease(v naming.Version) (*github.Release, error) {
	base := fmt.Sprintf("https://github.com/%s/%s/releases/download/%s",
		url.PathEscape(p.cfg.GitHub.Owner), url.PathEscape(p.cfg.GitHub.Repo), url.PathEscape(v.Tag()))

	release := &github.Release{
		TagName: v.Tag(),
		Name:    "DBX " + v.Tag(),
		HTMLURL: fmt.Sprintf("https://github.com/%s/%s/releases/tag/%s",
			p.cfg.GitHub.Owner, p.cfg.GitHub.Repo, v.Tag()),
	}
	vars := naming.AssetVars(v)
	var skipped []string
	for _, configured := range p.cfg.Assets {
		name := naming.Expand(configured.Selector, vars)
		if name == "" {
			continue
		}
		if strings.ContainsAny(name, "*?[") {
			skipped = append(skipped, configured.Selector)
			continue
		}
		escaped := url.PathEscape(name)
		release.Assets = append(release.Assets,
			github.Asset{Name: name, Size: -1, BrowserDownloadURL: base + "/" + escaped},
			// Probe for a checksum sidecar; a miss is reported as "no checksum".
			github.Asset{Name: name + ".sha256", Size: -1, BrowserDownloadURL: base + "/" + url.PathEscape(name+".sha256")},
		)
	}
	if len(release.Assets) == 0 {
		return nil, fmt.Errorf("cannot derive any download URL without the GitHub API: every asset selector uses a wildcard")
	}
	if len(skipped) > 0 {
		return nil, fmt.Errorf("cannot expand wildcard asset selectors without the GitHub API: %s", strings.Join(skipped, ", "))
	}
	return release, nil
}

// resolveVersionAPI is the normal, API-backed resolution path.
func (p *Planner) resolveVersionAPI(ctx context.Context, requested string) (naming.Version, *github.Release, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" || strings.EqualFold(requested, "latest") {
		rel, err := p.gh.LatestAppRelease(ctx, p.cfg.GitHub.Owner, p.cfg.GitHub.Repo,
			p.cfg.GitHub.TagPattern, p.cfg.GitHub.IncludePrerelease)
		if err != nil {
			return naming.Version{}, nil, err
		}
		v, ok := naming.ExtractVersion(rel.TagName, p.cfg.GitHub.TagPattern)
		if !ok {
			// Fall back to the published version embedded in the release name.
			var parseErr error
			v, parseErr = naming.ParseVersion(rel.TagName)
			if parseErr != nil {
				return naming.Version{}, nil, fmt.Errorf("release %s: cannot derive a version from tag %q",
					rel.DisplayName(), rel.TagName)
			}
		}
		return v, rel, nil
	}

	v, err := naming.ParseVersion(requested)
	if err != nil {
		return naming.Version{}, nil, err
	}
	rel, err := p.gh.ResolveRelease(ctx, p.cfg.GitHub.Owner, p.cfg.GitHub.Repo, p.cfg.GitHub.TagPattern, v)
	if err != nil {
		return naming.Version{}, nil, err
	}
	return v, rel, nil
}

// Build resolves every configured artifact into a Plan.
func (p *Planner) Build(ctx context.Context, opts Options) (*Plan, error) {
	version, release, err := p.ResolveVersion(ctx, opts.Version)
	if err != nil {
		return nil, err
	}

	outDir := opts.OutDir
	if outDir == "" {
		outDir = p.cfg.Output.Dir
	}
	absOut, err := filepath.Abs(outDir)
	if err != nil {
		return nil, fmt.Errorf("resolve output directory: %w", err)
	}
	dirName := naming.DirName(p.cfg.Output.DirNameTemplate, version)
	if err := EnsureDirNameIsSafe(dirName); err != nil {
		return nil, fmt.Errorf("output.dir_name_template: %w", err)
	}

	archivePath := filepath.Join(absOut, p.cfg.ArchiveFileName(version))
	if opts.ArchiveFormat != "" {
		archivePath = filepath.Join(absOut, archiveName(p.cfg, version, opts.ArchiveFormat))
	}

	plan := &Plan{
		Version:     version,
		DirName:     dirName,
		Dir:         filepath.Join(absOut, dirName),
		ArchivePath: archivePath,
		Release:     release,
		Degraded:    p.degraded,
	}
	if p.degraded {
		plan.DegradedReason = p.degradedReason
	}

	if opts.IncludeRelease {
		items, warnings, err := p.releaseItems(ctx, version, release, opts)
		if err != nil {
			return nil, err
		}
		plan.Items = append(plan.Items, items...)
		plan.Warnings = append(plan.Warnings, warnings...)
	}

	if opts.IncludePlugins && p.cfg.Plugins.Enabled {
		items, resolved, revoked, warnings, err := p.pluginItems(ctx, opts)
		if err != nil {
			return nil, err
		}
		plan.Items = append(plan.Items, items...)
		plan.Plugins = resolved
		plan.RevokedWarnings = revoked
		plan.Warnings = append(plan.Warnings, warnings...)
	}

	if len(plan.Items) == 0 {
		return nil, errors.New("nothing to download: check the assets and plugins configuration")
	}
	return plan, nil
}

func archiveName(cfg *config.Config, v naming.Version, format string) string {
	tmp := *cfg
	tmp.Archive.Format = format
	return tmp.ArchiveFileName(v)
}

// releaseItems matches the configured selectors against the release assets.
func (p *Planner) releaseItems(ctx context.Context, version naming.Version, release *github.Release, opts Options) ([]Item, []string, error) {
	selectors := make([]string, 0, len(p.cfg.Assets)+len(opts.Assets))
	selectors = append(selectors, opts.Assets...)
	for _, a := range p.cfg.Assets {
		selectors = append(selectors, a.Selector)
	}

	vars := naming.AssetVars(version)
	var items []Item
	var warnings []string
	used := map[string]bool{}

	for _, raw := range selectors {
		selector := naming.Expand(strings.TrimSpace(raw), vars)
		if selector == "" {
			continue
		}
		if _, err := path.Match(selector, "probe"); err != nil {
			return nil, nil, fmt.Errorf("invalid asset selector %q: %w", raw, err)
		}

		var matched []github.Asset
		for _, asset := range release.Assets {
			if ok, _ := path.Match(selector, asset.Name); ok {
				matched = append(matched, asset)
			}
		}
		if len(matched) == 0 {
			msg := fmt.Sprintf("release %s has no asset matching %q", release.TagName, selector)
			if opts.AllowMissing {
				warnings = append(warnings, msg)
				continue
			}
			return nil, nil, fmt.Errorf("%s\navailable assets:\n%s", msg, assetList(release))
		}

		for _, asset := range matched {
			if used[asset.Name] {
				continue
			}
			used[asset.Name] = true
			rel, err := naming.SafeRelPath(asset.Name)
			if err != nil {
				return nil, nil, err
			}
			item := Item{
				Kind:    KindRelease,
				Label:   asset.Name,
				RelPath: rel,
				URL:     asset.BrowserDownloadURL,
				Size:    asset.Size,
				Note:    fmt.Sprintf("release %s", release.TagName),
			}
			if digest, err := p.releaseDigest(ctx, release, asset); err != nil {
				if p.log != nil {
					p.log.Debugf("%s: no usable checksum sidecar (%v)", asset.Name, err)
				}
			} else {
				item.SHA256 = digest
			}
			items = append(items, item)
		}
	}

	sort.SliceStable(items, func(i, j int) bool { return items[i].RelPath < items[j].RelPath })
	return items, warnings, nil
}

// releaseDigest looks for a "<asset>.sha256" sidecar in the same release and
// reads the digest from it. DBX publishes sidecars for only some artifacts,
// so a miss is expected and not an error.
func (p *Planner) releaseDigest(ctx context.Context, release *github.Release, asset github.Asset) (string, error) {
	var sidecar *github.Asset
	for i := range release.Assets {
		candidate := release.Assets[i]
		if candidate.Name == asset.Name+".sha256" {
			sidecar = &candidate
			break
		}
	}
	if sidecar == nil {
		return "", errors.New("no .sha256 sidecar published for this asset")
	}
	body, err := p.getText(ctx, sidecar.BrowserDownloadURL)
	if err != nil {
		return "", err
	}
	// Format: "<hex>  <filename>".
	fields := strings.Fields(body)
	if len(fields) == 0 {
		return "", fmt.Errorf("checksum sidecar %s is empty", sidecar.Name)
	}
	digest := strings.ToLower(fields[0])
	if !hexDigestRe.MatchString(digest) {
		return "", fmt.Errorf("checksum sidecar %s does not start with a sha256 digest", sidecar.Name)
	}
	return digest, nil
}

var hexDigestRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// pluginItems resolves plugin metadata into download items.
func (p *Planner) pluginItems(ctx context.Context, opts Options) ([]Item, []ResolvedPlugin, []string, []string, error) {
	policy := p.cfg.Plugins.VersionPolicy
	if opts.PluginLatest {
		policy = config.PolicyLatest
	}

	var revoked *store.Revoked
	if r, err := p.store.Revoked(ctx); err != nil {
		if p.log != nil {
			p.log.Debugf("revocation list unavailable: %v", err)
		}
	} else {
		revoked = r
	}

	var items []Item
	var resolved []ResolvedPlugin
	var revokedWarnings []string
	var warnings []string

	for _, configured := range p.cfg.Plugins.Items {
		meta, err := p.store.Plugin(ctx, configured.ID)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		requested := configured.Version
		if override, ok := opts.PluginVersions[configured.ID]; ok {
			requested = override
		}
		selected, err := store.ResolveVersion(meta, requested, policy)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		if reason, bad := revoked.IsRevoked(meta.ID, selected.Version); bad {
			revokedWarnings = append(revokedWarnings,
				fmt.Sprintf("plugin %s %s is revoked by the store: %s", meta.ID, selected.Version, reason))
		}

		subDir := configured.SubDir()
		if subDir == "" {
			return nil, nil, nil, nil, fmt.Errorf("plugin %s: cannot derive a directory name", meta.ID)
		}

		entry := ResolvedPlugin{
			ID:      meta.ID,
			Name:    meta.Name,
			Version: selected.Version,
			SubDir:  subDir,
		}

		for _, target := range p.cfg.Plugins.Targets {
			item, source, err := p.pluginItem(ctx, meta, selected, target, subDir)
			if err != nil {
				warnings = append(warnings, err.Error())
				continue
			}
			item.Note = fmt.Sprintf("%s %s", source, selected.Version)
			items = append(items, item)
			entry.Targets = append(entry.Targets, target)
		}
		if len(entry.Targets) == 0 {
			return nil, nil, nil, nil, fmt.Errorf("plugin %s %s: none of the targets %v are available (published: %v)",
				meta.ID, selected.Version, p.cfg.Plugins.Targets, selected.Targets())
		}
		resolved = append(resolved, entry)
	}

	sort.SliceStable(items, func(i, j int) bool { return items[i].RelPath < items[j].RelPath })
	return items, resolved, revokedWarnings, warnings, nil
}

// pluginItem builds one download item according to plugins.source.
func (p *Planner) pluginItem(ctx context.Context, meta *store.Plugin, version *store.Version, target, subDir string) (Item, string, error) {
	base := Item{Kind: KindPlugin, Label: fmt.Sprintf("%s (%s)", meta.ID, target)}

	storeArtifact, hasStore := version.ArtifactFor(target)

	switch p.cfg.Plugins.Source {
	case config.SourceGitHub:
		return p.githubPluginItem(ctx, base, meta, version, target, subDir)
	case config.SourceStore:
		if !hasStore {
			return base, "", fmt.Errorf("plugin %s %s: store metadata has no artifact for target %s",
				meta.ID, version.Version, target)
		}
		return p.finishPluginItem(base, meta, version, target, subDir,
			storeArtifact.URL, storeArtifact.SHA256, storeArtifact.Size, "dbx-store")
	default: // auto: prefer the store, fall back to the plugin's own release
		if hasStore {
			return p.finishPluginItem(base, meta, version, target, subDir,
				storeArtifact.URL, storeArtifact.SHA256, storeArtifact.Size, "dbx-store")
		}
		return p.githubPluginItem(ctx, base, meta, version, target, subDir)
	}
}

// githubPluginItem resolves one artifact from the plugin's own GitHub release.
func (p *Planner) githubPluginItem(ctx context.Context, base Item, meta *store.Plugin, version *store.Version, target, subDir string) (Item, string, error) {
	assets, err := p.pluginGitHubAssets(ctx, meta, version)
	if err != nil {
		return base, "", err
	}
	name := store.GitHubAssetName(meta, version.Version, target)
	asset, ok := assets[name]
	if !ok {
		available := make([]string, 0, len(assets))
		for candidate := range assets {
			available = append(available, candidate)
		}
		sort.Strings(available)
		return base, "", fmt.Errorf("plugin %s %s: GitHub release has no asset %q (found: %s)",
			meta.ID, version.Version, name, strings.Join(available, ", "))
	}
	return p.finishPluginItem(base, meta, version, target, subDir,
		asset.BrowserDownloadURL, "", asset.Size, "github release")
}

// pluginGitHubAssets returns the release assets published by the plugin's own
// repository for one version, keyed by file name.
//
// The store metadata cannot be trusted for the tag: its "source" field is not
// always refreshed (io.github.t8y2.s3 still pointed at v0.1.3 while 0.1.9 was
// current), so a stale hint is detected and the release list is queried
// instead.
func (p *Planner) pluginGitHubAssets(ctx context.Context, meta *store.Plugin, version *store.Version) (map[string]github.Asset, error) {
	key := meta.ID + "@" + version.Version
	if cached, ok := p.githubAssets[key]; ok {
		return cached, p.githubErrs[key]
	}
	assets, err := p.fetchPluginGitHubAssets(ctx, meta, version)
	p.githubAssets[key] = assets
	p.githubErrs[key] = err
	return assets, err
}

func (p *Planner) fetchPluginGitHubAssets(ctx context.Context, meta *store.Plugin, version *store.Version) (map[string]github.Asset, error) {
	repo, ok := store.GitHubRepo(meta)
	if !ok {
		return nil, fmt.Errorf("plugin %s: metadata has no GitHub repository to fall back to", meta.ID)
	}
	owner, name, _ := strings.Cut(repo, "/")

	index := func(rel github.Release) map[string]github.Asset {
		out := make(map[string]github.Asset, len(rel.Assets))
		for _, asset := range rel.Assets {
			out[asset.Name] = asset
		}
		return out
	}

	if tag, ok := store.GitHubTagHint(meta, version.Version); ok {
		rel, err := p.gh.ReleaseByTag(ctx, owner, name, tag)
		if err == nil {
			if p.log != nil {
				p.log.Debugf("plugin %s %s: using GitHub release tag %s", meta.ID, version.Version, tag)
			}
			return index(*rel), nil
		}
		if !github.IsNotFound(err) {
			return nil, fmt.Errorf("plugin %s: look up release %s of %s: %w", meta.ID, tag, repo, err)
		}
	}

	releases, err := p.gh.ListReleases(ctx, owner, name, 1, 100)
	if err != nil {
		return nil, fmt.Errorf("plugin %s: list releases of %s: %w", meta.ID, repo, err)
	}
	prefix := fmt.Sprintf("%s-%s-", meta.ID, version.Version)
	for i := range releases {
		for _, asset := range releases[i].Assets {
			if strings.HasPrefix(asset.Name, prefix) && strings.HasSuffix(asset.Name, ".dbxp") {
				if p.log != nil {
					p.log.Debugf("plugin %s %s: matched GitHub release %s", meta.ID, version.Version, releases[i].TagName)
				}
				return index(releases[i]), nil
			}
		}
	}
	return nil, fmt.Errorf("plugin %s %s: none of the %d releases of %s publish %s* assets",
		meta.ID, version.Version, len(releases), repo, prefix)
}

func (p *Planner) finishPluginItem(base Item, meta *store.Plugin, version *store.Version, target, subDir, rawURL, sha string, size int64, source string) (Item, string, error) {
	name := naming.BaseName(rawURL)
	if name == "" {
		name = fmt.Sprintf("%s-%s-%s.dbxp", meta.ID, version.Version, target)
	}
	rel, err := naming.SafeRelPath(path.Join(p.cfg.Plugins.Root, subDir, name))
	if err != nil {
		return base, "", fmt.Errorf("plugin %s: %w", meta.ID, err)
	}
	base.Label = name
	base.RelPath = rel
	base.URL = rawURL
	base.SHA256 = strings.ToLower(sha)
	base.Size = size
	return base, source, nil
}

// getText fetches a small text document (checksum sidecars).
func (p *Planner) getText(ctx context.Context, rawURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", fetch.DefaultUserAgent)
	resp, err := p.client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: HTTP %d", rawURL, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func assetList(release *github.Release) string {
	names := make([]string, 0, len(release.Assets))
	for _, a := range release.Assets {
		names = append(names, "  - "+a.Name)
	}
	sort.Strings(names)
	return strings.Join(names, "\n")
}

// Requests converts a plan into downloader requests.
func (p *Plan) Requests() []fetch.Request {
	reqs := make([]fetch.Request, 0, len(p.Items))
	for _, item := range p.Items {
		reqs = append(reqs, fetch.Request{
			Label:  item.Label,
			URL:    item.URL,
			Dest:   filepath.Join(p.Dir, filepath.FromSlash(item.RelPath)),
			SHA256: item.SHA256,
			Size:   item.Size,
		})
	}
	return reqs
}

// TotalSize sums the expected sizes, ignoring unknown ones.
func (p *Plan) TotalSize() int64 {
	var total int64
	for _, item := range p.Items {
		if item.Size > 0 {
			total += item.Size
		}
	}
	return total
}
