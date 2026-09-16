package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"dbxdl/internal/config"
	"dbxdl/internal/ui"
)

func configUsage(w io.Writer) {
	fmt.Fprint(w, `dbxdl config - manage config.yaml

USAGE
  dbxdl config <init|show|path> [flags]

SUBCOMMANDS
  init            write the commented default configuration (--user for a global one)
  show            print the configuration after defaults are applied
  path            print the configuration file path that would be used

FLAGS
  --out <path>    (init) destination, default ./config.yaml
  --user          (init) write to the per-user path instead
  --force         (init) overwrite an existing file
  --json          (show) emit JSON

WHERE THE CONFIGURATION COMES FROM
  The first existing file wins:

    1. --config <path>                     (must exist)
    2. $DBXDL_CONFIG                       (must exist)
    3. ./config.yaml
    4. $XDG_CONFIG_HOME/dbxdl/config.yaml  (default ~/.config/dbxdl/config.yaml)
    5. built-in defaults

  Setting $DBXDL_CONFIG (or creating the per-user file) is what lets a
  globally installed dbxdl use the same settings from any directory.

GLOBAL FLAGS
  --config, -c <path>   configuration file
  --verbose             debug output
  --quiet               errors only
  --no-color            disable ANSI colors

EXAMPLES
  dbxdl config init
  dbxdl config init --user
  dbxdl config init --out build/dbxdl.yaml
  dbxdl config show
`)
}

func runConfig(env Env, args []string, log *ui.Logger) int {
	if len(args) == 0 {
		configUsage(env.Stdout)
		return 2
	}
	switch args[0] {
	case "init":
		return runConfigInit(env, args[1:], log)
	case "show":
		return runConfigShow(env, args[1:], log)
	case "path":
		return runConfigPath(env, args[1:], log)
	case "help", "-h", "--help":
		configUsage(env.Stdout)
		return 0
	default:
		fmt.Fprintf(env.Stderr, "dbxdl config: unknown subcommand %q\n\n", args[0])
		configUsage(env.Stderr)
		return 2
	}
}

func runConfigInit(env Env, args []string, log *ui.Logger) int {
	fs := newFlagSet("config init", env, configUsage)
	var g globalFlags
	g.register(fs)
	var (
		out   string
		force bool
		user  bool
	)
	fs.StringVar(&out, "out", "", "destination path (default ./config.yaml)")
	fs.BoolVar(&force, "force", false, "overwrite an existing file")
	fs.BoolVar(&user, "user", false, "write to the per-user config path instead of ./config.yaml")
	if _, stop, code := parseFlags(fs, env, args, configUsage); stop {
		return code
	}
	setupLogger(log, &g, env)

	path := out
	if path == "" {
		switch {
		case g.configPath != "":
			path = g.configPath
		case user:
			if path = config.UserConfigPath(); path == "" {
				return fail(log, env, fmt.Errorf("cannot determine a per-user config path; pass --out"))
			}
		default:
			path = config.DefaultFileName
		}
	}
	if _, err := os.Stat(path); err == nil && !force {
		return fail(log, env, fmt.Errorf("%s already exists (use --force to overwrite)", path))
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fail(log, env, err)
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fail(log, env, fmt.Errorf("create %s: %w", dir, err))
		}
	}
	if err := os.WriteFile(path, []byte(config.DefaultYAML()), 0o644); err != nil {
		return fail(log, env, fmt.Errorf("write %s: %w", path, err))
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	log.Okf("wrote %s", abs)
	return 0
}

func runConfigShow(env Env, args []string, log *ui.Logger) int {
	fs := newFlagSet("config show", env, configUsage)
	var g globalFlags
	g.register(fs)
	var asJSON bool
	fs.BoolVar(&asJSON, "json", false, "emit JSON")
	if _, stop, code := parseFlags(fs, env, args, configUsage); stop {
		return code
	}
	setupLogger(log, &g, env)

	cfg, err := config.Resolve(g.configPath)
	if err != nil {
		return fail(log, env, err)
	}
	if err := cfg.Validate(); err != nil {
		return fail(log, env, err)
	}

	if asJSON {
		enc := json.NewEncoder(env.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(cfg); err != nil {
			return fail(log, env, err)
		}
		return 0
	}

	out := env.Stdout
	source := cfg.Path
	if source == "" {
		source = "(built-in defaults)"
	}
	fmt.Fprintf(out, "\nconfig file: %s  [%s]\n\n", source, cfg.Source)
	fmt.Fprintf(out, "search order: --config, $%s, %s\n\n",
		config.EnvConfig, strings.Join(config.SearchPath(), ", "))
	fmt.Fprintf(out, "github        %s/%s  api=%s  auth=%s\n",
		cfg.GitHub.Owner, cfg.GitHub.Repo, cfg.GitHub.APIBase, authLabel(cfg.GitHub.Token))
	fmt.Fprintf(out, "              tag_pattern=%s  prerelease=%v  timeout=%s\n",
		cfg.GitHub.TagPattern, cfg.GitHub.IncludePrerelease, cfg.GitHub.Timeout.Duration())
	fmt.Fprintf(out, "store         %s/%s@%s  path=%s\n",
		cfg.Store.Owner, cfg.Store.Repo, cfg.Store.Ref, cfg.Store.PluginsPath)
	fmt.Fprintf(out, "output        dir=%s  dir_name=%s  keep_dir=%v  force=%v\n",
		cfg.Output.Dir, cfg.Output.DirNameTemplate, cfg.Output.KeepDir, cfg.Output.Force)
	fmt.Fprintf(out, "              concurrency=%d  retries=%d  verify_sha256=%v\n",
		cfg.Output.Concurrency, cfg.Output.Retries, cfg.Output.VerifySHA)
	fmt.Fprintf(out, "archive       enabled=%v  format=%s  name=%s  gzip=%d  sidecar=%v\n",
		cfg.Archive.Enabled, cfg.Archive.Format, cfg.Archive.NameTemplate, cfg.Archive.GzipLevel, cfg.Archive.SHA256Sidecar)

	fmt.Fprintf(out, "\ndbx packages (%d)\n", len(cfg.Assets))
	for _, a := range cfg.Assets {
		fmt.Fprintf(out, "  %-46s %s\n", a.Selector, a.Description)
	}

	fmt.Fprintf(out, "\nplugins (%d)  source=%s  policy=%s  root=%s\n",
		len(cfg.Plugins.Items), cfg.Plugins.Source, cfg.Plugins.VersionPolicy, cfg.Plugins.Root)
	fmt.Fprintf(out, "  targets: %s\n", strings.Join(cfg.Plugins.Targets, ", "))
	for _, item := range cfg.Plugins.Items {
		version := item.Version
		if version == "" {
			version = "latest"
		}
		fmt.Fprintf(out, "  %-22s version=%-10s dir=%s\n", item.ID, version, item.SubDir())
	}
	fmt.Fprintln(out)
	return 0
}

func authLabel(token string) string {
	if token == "" {
		return "anonymous (GITHUB_TOKEN/GH_TOKEN not set)"
	}
	return "token"
}

func runConfigPath(env Env, args []string, log *ui.Logger) int {
	fs := newFlagSet("config path", env, configUsage)
	var g globalFlags
	g.register(fs)
	if _, stop, code := parseFlags(fs, env, args, configUsage); stop {
		return code
	}
	setupLogger(log, &g, env)

	cfg, err := config.Resolve(g.configPath)
	if err != nil {
		return fail(log, env, err)
	}

	path := cfg.Path
	if path == "" {
		// Nothing was found: point at the location a per-user file would take,
		// which is what a user asking for the path usually wants to create.
		path = config.UserConfigPath()
		fmt.Fprintf(env.Stderr, "note: no configuration file found; using built-in defaults\n")
	}
	if path == "" {
		return fail(log, env, fmt.Errorf("no configuration file and no home directory to place one in"))
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	fmt.Fprintln(env.Stdout, abs)
	return 0
}
