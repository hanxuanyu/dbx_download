package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"

	"dbxdl/internal/config"
	"dbxdl/internal/github"
	"dbxdl/internal/naming"
	"dbxdl/internal/ui"
)

func listUsage(w io.Writer) {
	fmt.Fprint(w, `dbxdl list - list recent DBX releases

USAGE
  dbxdl list [flags]

FLAGS
  -n, --limit <n>   how many releases to show (default 10)
  --all             include unrelated tag families (packages-*, agents-*)
  --prerelease      include pre-releases such as v0.7.0-rc.1
  --json            emit JSON instead of a table
  --check           verify that the configured asset selectors exist

GLOBAL FLAGS
  --config, -c <path>   configuration file (default ./config.yaml)
  --verbose             debug output
  --quiet               errors only
  --no-color            disable ANSI colors

The dbx repository publishes several tag families in one release feed
(v0.6.14, packages-v0.4.88, agents-v0.2.111). By default only tags matching
github.tag_pattern are listed, which is what "download" resolves against.

EXAMPLES
  dbxdl list
  dbxdl list -n 20 --check
  dbxdl list --json | jq '.[].tag_name'
`)
}

func runList(ctx context.Context, env Env, args []string, log *ui.Logger) int {
	fs := newFlagSet("list", env, listUsage)
	var g globalFlags
	g.register(fs)

	var (
		limit      int
		all        bool
		prerelease bool
		asJSON     bool
		check      bool
	)
	fs.IntVar(&limit, "limit", 10, "number of releases to show")
	fs.IntVar(&limit, "n", 10, "shorthand for --limit")
	fs.BoolVar(&all, "all", false, "include all tag families")
	fs.BoolVar(&prerelease, "prerelease", false, "include pre-releases")
	fs.BoolVar(&asJSON, "json", false, "emit JSON")
	fs.BoolVar(&check, "check", false, "verify configured asset selectors")

	if _, stop, code := parseFlags(fs, env, args, listUsage); stop {
		return code
	}
	setupLogger(log, &g, env)

	cfg, err := loadConfig(&g, log)
	if err != nil {
		return fail(log, env, err)
	}
	if limit < 1 {
		return fail(log, env, fmt.Errorf("--limit must be at least 1"))
	}

	client := github.New(github.Options{
		BaseURL: cfg.GitHub.APIBase,
		WebBase: cfg.GitHub.WebBase,
		Token:   cfg.GitHub.Token,
		Timeout: cfg.GitHub.Timeout.Duration(),
		Log:     log,
	})

	releases, err := fetchReleases(ctx, client, cfg, all, prerelease, limit)
	if err != nil {
		if github.IsRateLimited(err) && !all {
			return listFromAtom(ctx, client, cfg, env, log, limit, prerelease, asJSON)
		}
		return fail(log, env, err)
	}
	if len(releases) == 0 {
		log.Warnf("no releases matched tag_pattern %q", cfg.GitHub.TagPattern)
		return 0
	}

	if asJSON {
		return printReleasesJSON(env, releases)
	}
	printReleasesTable(env.Stdout, cfg, releases, check)
	if !client.HasToken() {
		fmt.Fprintf(env.Stderr, "\nnote: unauthenticated GitHub API requests are limited to 60/hour; set GITHUB_TOKEN to raise it\n")
	}
	return 0
}

func fetchReleases(ctx context.Context, client *github.Client, cfg *config.Config, all, prerelease bool, limit int) ([]github.Release, error) {
	if all {
		var out []github.Release
		for page := 1; page <= 5 && len(out) < limit; page++ {
			pageReleases, err := client.ListReleases(ctx, cfg.GitHub.Owner, cfg.GitHub.Repo, page, 100)
			if err != nil {
				return nil, err
			}
			for _, rel := range pageReleases {
				if rel.Prerelease && !prerelease {
					continue
				}
				out = append(out, rel)
				if len(out) >= limit {
					break
				}
			}
			if len(pageReleases) < 100 {
				break
			}
		}
		sortReleasesNewestFirst(out)
		return out, nil
	}
	return client.AppReleases(ctx, cfg.GitHub.Owner, cfg.GitHub.Repo, cfg.GitHub.TagPattern,
		prerelease || cfg.GitHub.IncludePrerelease, limit)
}

// listFromAtom degrades gracefully when the REST API quota is exhausted: the
// public Atom feed still yields tag names, just without asset details.
func listFromAtom(ctx context.Context, client *github.Client, cfg *config.Config, env Env, log *ui.Logger, limit int, prerelease, asJSON bool) int {
	tags, err := client.ReleasesAtom(ctx, cfg.GitHub.Owner, cfg.GitHub.Repo)
	if err != nil {
		return fail(log, env, fmt.Errorf("github API is rate limited and the Atom feed failed too: %w", err))
	}
	type row struct {
		Tag     string `json:"tag_name"`
		Version string `json:"version"`
		Date    string `json:"published"`
	}
	var rows []row
	for _, tag := range tags {
		version, ok := naming.ExtractVersion(tag, cfg.GitHub.TagPattern)
		if !ok {
			continue
		}
		if version.Pre != "" && !prerelease {
			continue
		}
		rows = append(rows, row{Tag: tag, Version: version.String()})
		if len(rows) >= limit {
			break
		}
	}
	if asJSON {
		enc := json.NewEncoder(env.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rows); err != nil {
			return fail(log, env, err)
		}
		return 0
	}
	log.Warnf("GitHub API quota exhausted; showing tag names from the Atom feed (no asset details). Set GITHUB_TOKEN for the full list.")
	fmt.Fprintf(env.Stdout, "\n%-10s %-12s\n", "VERSION", "TAG")
	for _, r := range rows {
		fmt.Fprintf(env.Stdout, "%-10s %-12s\n", r.Version, r.Tag)
	}
	return 0
}

func printReleasesJSON(env Env, releases []github.Release) int {
	type assetJSON struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
		URL  string `json:"browser_download_url"`
	}
	type releaseJSON struct {
		Tag         string      `json:"tag_name"`
		Name        string      `json:"name"`
		Prerelease  bool        `json:"prerelease"`
		PublishedAt string      `json:"published_at"`
		HTMLURL     string      `json:"html_url"`
		Assets      []assetJSON `json:"assets"`
	}
	out := make([]releaseJSON, 0, len(releases))
	for _, rel := range releases {
		item := releaseJSON{
			Tag:         rel.TagName,
			Name:        rel.Name,
			Prerelease:  rel.Prerelease,
			PublishedAt: rel.Published().UTC().Format(time.RFC3339),
			HTMLURL:     rel.HTMLURL,
		}
		for _, a := range rel.Assets {
			item.Assets = append(item.Assets, assetJSON{Name: a.Name, Size: a.Size, URL: a.BrowserDownloadURL})
		}
		out = append(out, item)
	}
	enc := json.NewEncoder(env.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		return 1
	}
	return 0
}

func printReleasesTable(w io.Writer, cfg *config.Config, releases []github.Release, check bool) {
	header := fmt.Sprintf("%-10s %-12s %-12s %8s", "VERSION", "TAG", "PUBLISHED", "ASSETS")
	if check {
		header += "  " + fmt.Sprintf("%-7s", "MATCH")
	}
	fmt.Fprintf(w, "\n%s\n", header)
	fmt.Fprintf(w, "%s\n", strings.Repeat("-", len(header)))

	for _, rel := range releases {
		version, ok := naming.ExtractVersion(rel.TagName, cfg.GitHub.TagPattern)
		versionLabel := version.String()
		if !ok {
			versionLabel = "-"
		}
		line := fmt.Sprintf("%-10s %-12s %-12s %8d",
			versionLabel,
			rel.TagName,
			rel.Published().UTC().Format("2006-01-02"),
			len(rel.Assets))
		if check {
			matched, total := matchSelectors(cfg, rel, version, ok)
			mark := fmt.Sprintf("%d/%d", matched, total)
			if matched == total {
				mark = "ok " + mark
			} else {
				mark = "!! " + mark
			}
			line += "  " + fmt.Sprintf("%-7s", mark)
		}
		fmt.Fprintln(w, line)
	}
	fmt.Fprintln(w)
}

func matchSelectors(cfg *config.Config, rel github.Release, version naming.Version, haveVersion bool) (matched, total int) {
	total = len(cfg.Assets)
	if !haveVersion {
		return 0, total
	}
	vars := naming.AssetVars(version)
	for _, configured := range cfg.Assets {
		selector := naming.Expand(configured.Selector, vars)
		for _, asset := range rel.Assets {
			if ok, err := path.Match(selector, asset.Name); err == nil && ok {
				matched++
				break
			}
		}
	}
	return matched, total
}

// sortReleasesNewestFirst is used by tests.
func sortReleasesNewestFirst(releases []github.Release) {
	sort.SliceStable(releases, func(i, j int) bool {
		return releases[i].Published().After(releases[j].Published())
	})
}
