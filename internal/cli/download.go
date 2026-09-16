package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"dbxdl/internal/app"
	"dbxdl/internal/ui"
)

// stringList collects a repeatable string flag.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// downloadOptions mirrors app.Options plus the raw plugin overrides.
type downloadOptions struct {
	version        string
	outDir         string
	assets         stringList
	pluginOverride stringList
	pluginLatest   bool
	skipPlugins    bool
	skipDBX        bool
	noArchive      bool
	force          bool
	dryRun         bool
	allowMissing   bool
	concurrency    int
	retries        int
	format         string
	keepDir        *bool
}

func downloadUsage(w io.Writer) {
	fmt.Fprint(w, `dbxdl download - fetch a DBX release plus plugins and build a tar bundle

USAGE
  dbxdl download [version] [flags]

  version may be positional or passed with --version. "latest" (the default)
  asks GitHub for the newest matching release.

FLAGS
  --version, -V <v>     release version, e.g. 0.6.14 or v0.6.14 ("latest")
  --out-dir, -o <dir>   output directory (default: output.dir from config)
  --asset <selector>    extra release asset selector; repeatable.
                        Supports * and ? wildcards and {version}
  --plugin <id=ver>     pin a plugin version; repeatable
  --plugin-latest       ignore pinned plugin versions, use the newest
  --skip-plugins        download only the DBX packages
  --skip-dbx            download only the plugins
  --no-archive          download into the bundle directory but do not tar it
  --format <fmt>        tar.gz (default) or tar
  --keep-dir            keep the staging directory after archiving. By default
                        it is deleted once the tar file is written, so only the
                        tar remains
  --allow-missing       treat a missing release asset as a warning
  --force               re-download files even when already present
  --concurrency <n>     parallel downloads (default: output.concurrency)
  --retries <n>         retries per file (default: output.retries)
  --dry-run             print the plan, download nothing

GLOBAL FLAGS
  --config, -c <path>   configuration file (default ./config.yaml)
  --verbose             debug output
  --quiet               errors only
  --no-color            disable ANSI colors

EXAMPLES
  dbxdl download
  dbxdl download 0.6.13 --out-dir ./dist
  dbxdl download --asset 'dbx-jdbc-plugin-{version}.zip'
  dbxdl download --plugin io.dbx.ssh=0.4.75
  dbxdl download --dry-run
`)
}

func runDownload(ctx context.Context, env Env, args []string, log *ui.Logger) int {
	fs := newFlagSet("download", env, downloadUsage)
	var g globalFlags
	g.register(fs)

	var opts downloadOptions
	fs.StringVar(&opts.version, "version", "", "release version or \"latest\"")
	fs.StringVar(&opts.version, "V", "", "shorthand for --version")
	fs.StringVar(&opts.outDir, "out-dir", "", "output directory")
	fs.StringVar(&opts.outDir, "o", "", "shorthand for --out-dir")
	fs.Var(&opts.assets, "asset", "extra release asset selector (repeatable)")
	fs.Var(&opts.pluginOverride, "plugin", "plugin version pin, id=version (repeatable)")
	fs.BoolVar(&opts.pluginLatest, "plugin-latest", false, "use the newest plugin versions")
	fs.BoolVar(&opts.skipPlugins, "skip-plugins", false, "skip plugins")
	fs.BoolVar(&opts.skipDBX, "skip-dbx", false, "skip DBX packages")
	fs.BoolVar(&opts.noArchive, "no-archive", false, "do not create the tar file")
	fs.BoolVar(&opts.force, "force", false, "re-download existing files")
	fs.BoolVar(&opts.dryRun, "dry-run", false, "plan only")
	fs.BoolVar(&opts.allowMissing, "allow-missing", false, "warn instead of failing on a missing asset")
	fs.IntVar(&opts.concurrency, "concurrency", 0, "parallel downloads")
	fs.IntVar(&opts.retries, "retries", -1, "retries per file")
	fs.StringVar(&opts.format, "format", "", "archive format: tar.gz or tar")
	keepDir := fs.Bool("keep-dir", false, "keep the bundle directory after archiving (default: delete it)")

	positional, stop, code := parseFlags(fs, env, args, downloadUsage)
	if stop {
		return code
	}
	setupLogger(log, &g, env)

	cfg, err := loadConfig(&g, log)
	if err != nil {
		return fail(log, env, err)
	}

	// A positional argument is the version unless --version was also given.
	if len(positional) > 1 {
		return fail(log, env, fmt.Errorf("at most one version argument is accepted, got %d", len(positional)))
	}
	if len(positional) == 1 {
		if opts.version != "" && opts.version != positional[0] {
			return fail(log, env, fmt.Errorf("conflicting versions: --version %s and positional %s", opts.version, positional[0]))
		}
		opts.version = positional[0]
	}
	if opts.skipDBX && opts.skipPlugins {
		return fail(log, env, fmt.Errorf("--skip-dbx and --skip-plugins cannot be combined"))
	}

	overrides := map[string]string{}
	for _, raw := range opts.pluginOverride {
		id, version, err := splitIDVersion(raw)
		if err != nil {
			return fail(log, env, err)
		}
		overrides[id] = version
	}

	if fs.Lookup("keep-dir") != nil {
		// Only override the configuration when the flag was actually passed.
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "keep-dir" {
				cfg.Output.KeepDir = *keepDir
			}
		})
	}
	if opts.outDir != "" {
		cfg.Output.Dir = opts.outDir
	}
	if opts.format != "" {
		cfg.Archive.Format = opts.format
	}
	if err := cfg.Validate(); err != nil {
		return fail(log, env, err)
	}

	appOpts := app.Options{
		Version:        opts.version,
		OutDir:         cfg.Output.Dir,
		Assets:         opts.assets,
		IncludeRelease: !opts.skipDBX,
		IncludePlugins: !opts.skipPlugins,
		AllowMissing:   opts.allowMissing,
		PluginVersions: overrides,
		PluginLatest:   opts.pluginLatest,
		Force:          opts.force,
		Concurrency:    opts.concurrency,
		Retries:        opts.retries,
		ArchiveFormat:  opts.format,
		NoArchive:      opts.noArchive,
		DryRun:         opts.dryRun,
	}

	summary, err := app.Run(ctx, cfg, appOpts, log)
	if err != nil {
		return fail(log, env, err)
	}

	if !opts.dryRun && !opts.noArchive && cfg.Archive.Enabled && summary.Archive != nil {
		// Also print the archive with an absolute path so the result can be
		// copied straight out of the terminal.
		abs, absErr := filepath.Abs(summary.Archive.Path)
		if absErr == nil {
			fmt.Fprintf(env.Stdout, "\n%s\n", abs)
		}
	}
	return 0
}
