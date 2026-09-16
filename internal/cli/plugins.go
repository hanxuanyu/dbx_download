package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"dbxdl/internal/config"
	"dbxdl/internal/store"
	"dbxdl/internal/ui"
)

func pluginsUsage(w io.Writer) {
	fmt.Fprint(w, `dbxdl plugins - inspect the DBX plugin store

USAGE
  dbxdl plugins <list|show> [flags]

SUBCOMMANDS
  list              one row per plugin in the store
  show <plugin-id>  every published version of one plugin

FLAGS
  --json            emit JSON instead of a table
  --targets         (show) also print each version's target platforms

GLOBAL FLAGS
  --config, -c <path>   configuration file (default ./config.yaml)
  --verbose             debug output
  --quiet               errors only
  --no-color            disable ANSI colors

Metadata is read from github.com/<store.owner>/<store.repo>/plugins/<id>.json
via raw.githubusercontent.com, which does not consume GitHub API quota.

EXAMPLES
  dbxdl plugins list
  dbxdl plugins show io.dbx.ssh --targets
  dbxdl plugins show io.github.t8y2.s3 --json
`)
}

func runPlugins(ctx context.Context, env Env, args []string, log *ui.Logger) int {
	if len(args) == 0 {
		pluginsUsage(env.Stdout)
		return 2
	}
	switch args[0] {
	case "list", "ls":
		return runPluginsList(ctx, env, args[1:], log)
	case "show", "info":
		return runPluginsShow(ctx, env, args[1:], log)
	case "help", "-h", "--help":
		pluginsUsage(env.Stdout)
		return 0
	default:
		fmt.Fprintf(env.Stderr, "dbxdl plugins: unknown subcommand %q\n\n", args[0])
		pluginsUsage(env.Stderr)
		return 2
	}
}

func newStoreClient(cfg *config.Config, log *ui.Logger) *store.Client {
	return store.New(store.Options{
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
}

func runPluginsList(ctx context.Context, env Env, args []string, log *ui.Logger) int {
	fs := newFlagSet("plugins list", env, pluginsUsage)
	var g globalFlags
	g.register(fs)
	var asJSON bool
	fs.BoolVar(&asJSON, "json", false, "emit JSON")
	if _, stop, code := parseFlags(fs, env, args, pluginsUsage); stop {
		return code
	}
	setupLogger(log, &g, env)

	cfg, err := loadConfig(&g, log)
	if err != nil {
		return fail(log, env, err)
	}
	client := newStoreClient(cfg, log)

	catalog, err := client.Catalog(ctx)
	if err != nil {
		return fail(log, env, fmt.Errorf("read store catalog: %w", err))
	}
	if len(catalog.Plugins) == 0 {
		log.Warnf("store %s/%s lists no plugins", cfg.Store.Owner, cfg.Store.Repo)
		return 0
	}

	// The catalog trims version history, so fetch each plugin's own document
	// for accurate version counts. Raw content requests are not rate limited.
	type row struct {
		Plugin   *store.Plugin
		Err      error
		Metadata *store.Plugin
	}
	rows := make([]row, len(catalog.Plugins))
	var wg sync.WaitGroup
	for i, p := range catalog.Plugins {
		rows[i].Plugin = &catalog.Plugins[i]
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			meta, err := client.Plugin(ctx, id)
			if err != nil {
				rows[i].Err = err
				return
			}
			rows[i].Metadata = meta
		}(i, p.ID)
	}
	wg.Wait()

	if asJSON {
		out := make([]*store.Plugin, 0, len(rows))
		for i := range rows {
			if rows[i].Metadata != nil {
				out = append(out, rows[i].Metadata)
			} else if rows[i].Plugin != nil {
				out = append(out, rows[i].Plugin)
			}
		}
		enc := json.NewEncoder(env.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			return fail(log, env, err)
		}
		return 0
	}

	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Plugin.ID < rows[j].Plugin.ID })
	fmt.Fprintf(env.Stdout, "\n%-22s %-24s %-10s %8s  %s\n", "ID", "NAME", "LATEST", "VERSIONS", "TARGETS")
	fmt.Fprintf(env.Stdout, "%s\n", strings.Repeat("-", 88))
	for i := range rows {
		p := rows[i].Plugin
		name := p.Name
		latest := p.LatestVersion
		versions := "-"
		targets := "-"
		if meta := rows[i].Metadata; meta != nil {
			if meta.Name != "" {
				name = meta.Name
			}
			latest = meta.LatestVersion
			versions = fmt.Sprintf("%d", len(meta.Versions))
			targets = strings.Join(publishedTargets(meta), ",")
		} else if rows[i].Err != nil {
			versions = "unavailable"
		}
		fmt.Fprintf(env.Stdout, "%-22s %-24s %-10s %8s  %s\n",
			p.ID, truncate(name, 24), latest, versions, targets)
	}
	fmt.Fprintf(env.Stdout, "\n%d plugins in %s/%s@%s\n\n",
		len(rows), cfg.Store.Owner, cfg.Store.Repo, cfg.Store.Ref)
	return 0
}

// publishedTargets collects every target mentioned by any version.
func publishedTargets(p *store.Plugin) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range p.Versions {
		for _, a := range v.Artifacts {
			if !seen[a.Target] {
				seen[a.Target] = true
				out = append(out, a.Target)
			}
		}
	}
	sort.Strings(out)
	return out
}

func runPluginsShow(ctx context.Context, env Env, args []string, log *ui.Logger) int {
	fs := newFlagSet("plugins show", env, pluginsUsage)
	var g globalFlags
	g.register(fs)
	var (
		asJSON  bool
		targets bool
	)
	fs.BoolVar(&asJSON, "json", false, "emit JSON")
	fs.BoolVar(&targets, "targets", false, "print target platforms per version")
	positional, stop, code := parseFlags(fs, env, args, pluginsUsage)
	if stop {
		return code
	}
	setupLogger(log, &g, env)

	if len(positional) != 1 {
		return fail(log, env, fmt.Errorf("usage: dbxdl plugins show <plugin-id>"))
	}
	pluginID := positional[0]

	cfg, err := loadConfig(&g, log)
	if err != nil {
		return fail(log, env, err)
	}
	client := newStoreClient(cfg, log)
	plugin, err := client.Plugin(ctx, pluginID)
	if err != nil {
		return fail(log, env, err)
	}

	if asJSON {
		enc := json.NewEncoder(env.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(plugin); err != nil {
			return fail(log, env, err)
		}
		return 0
	}

	fmt.Fprintf(env.Stdout, "\n%s (%s)\n", plugin.Name, plugin.ID)
	if plugin.Description != "" {
		fmt.Fprintf(env.Stdout, "  %s\n", plugin.Description)
	}
	fmt.Fprintf(env.Stdout, "  publisher    %s%s\n", plugin.Publisher, verifiedMark(plugin.Verified))
	if plugin.License != "" {
		fmt.Fprintf(env.Stdout, "  license      %s\n", plugin.License)
	}
	fmt.Fprintf(env.Stdout, "  latest       %s\n", plugin.LatestVersion)
	if plugin.Homepage != "" {
		fmt.Fprintf(env.Stdout, "  homepage     %s\n", plugin.Homepage)
	}
	fmt.Fprintf(env.Stdout, "  metadata     %s\n", client.PluginURL(plugin.ID))

	fmt.Fprintf(env.Stdout, "\n  %-10s %-12s %-9s %s\n", "VERSION", "RELEASED", "SIZE", "TARGETS")
	fmt.Fprintf(env.Stdout, "  %s\n", strings.Repeat("-", 72))
	for _, v := range plugin.Versions {
		var total int64
		for _, a := range v.Artifacts {
			total += a.Size
		}
		targetList := ""
		if targets {
			targetList = strings.Join(v.Targets(), ",")
		} else {
			targetList = fmt.Sprintf("%d targets", len(v.Artifacts))
		}
		marker := " "
		if v.Version == plugin.LatestVersion {
			marker = "*"
		}
		fmt.Fprintf(env.Stdout, "  %-10s %-12s %-9s %s %s\n",
			v.Version, v.ReleasedAtString(), ui.HumanBytes(total), marker, targetList)
	}
	if targets {
		fmt.Fprintf(env.Stdout, "\n  %-10s %-14s %-10s %s\n", "VERSION", "TARGET", "SIZE", "SHA256")
		fmt.Fprintf(env.Stdout, "  %s\n", strings.Repeat("-", 78))
		for _, v := range plugin.Versions {
			for _, a := range v.Artifacts {
				fmt.Fprintf(env.Stdout, "  %-10s %-14s %-10s %s\n",
					v.Version, a.Target, ui.HumanBytes(a.Size), shortHash(a.SHA256))
			}
		}
	}
	fmt.Fprintln(env.Stdout)
	return 0
}

func verifiedMark(verified bool) string {
	if verified {
		return " (verified)"
	}
	return ""
}

func shortHash(hash string) string {
	if len(hash) <= 16 {
		return hash
	}
	return hash[:16] + "..."
}

func truncate(s string, width int) string {
	if len(s) <= width {
		return s
	}
	if width <= 3 {
		return s[:width]
	}
	return s[:width-3] + "..."
}
