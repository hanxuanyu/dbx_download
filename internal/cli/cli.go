// Package cli implements the dbxdl command-line interface: argument parsing,
// help text and the individual subcommands.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"dbxdl/internal/config"
	"dbxdl/internal/ui"
)

// Version is the dbxdl release version. It is overridable at build time:
//
//	go build -ldflags "-X dbxdl/internal/cli.Version=1.2.3"
var Version = "dev"

// Env carries the process environment into Run so the CLI is testable.
type Env struct {
	Stdout io.Writer
	Stderr io.Writer
	Stdin  io.Reader
	Args   []string
	// WorkDir is the directory relative paths resolve against.
	WorkDir string
}

// globalFlags are accepted by every subcommand.
type globalFlags struct {
	configPath string
	verbose    bool
	quiet      bool
	noColor    bool
	color      bool
}

func (g *globalFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&g.configPath, "config", "", "path to config.yaml (default ./config.yaml)")
	fs.StringVar(&g.configPath, "c", "", "shorthand for --config")
	fs.BoolVar(&g.verbose, "verbose", false, "print debug output")
	fs.BoolVar(&g.quiet, "quiet", false, "only print errors")
	fs.BoolVar(&g.noColor, "no-color", false, "disable ANSI colors")
	fs.BoolVar(&g.color, "color", false, "force ANSI colors even when not a TTY")
}

// Run executes dbxdl and returns the process exit code.
func Run(ctx context.Context, env Env) int {
	if len(env.Args) == 0 {
		printUsage(env.Stdout)
		return 2
	}

	command := env.Args[0]
	rest := env.Args[1:]

	switch command {
	case "help", "-h", "--help":
		printUsage(env.Stdout)
		return 0
	case "version", "--version":
		fmt.Fprintf(env.Stdout, "dbxdl %s\n", Version)
		return 0
	case "-v":
		fmt.Fprintf(env.Stdout, "dbxdl %s\n", Version)
		return 0
	}

	log := ui.NewLogger(env.Stderr, ui.LevelInfo, ui.ShouldColor(env.Stderr, false))

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch command {
	case "download", "dl", "get":
		return runDownload(ctx, env, rest, log)
	case "latest":
		return runDownload(ctx, env, append([]string{"--version", "latest"}, rest...), log)
	case "list", "ls", "releases":
		return runList(ctx, env, rest, log)
	case "plugins", "plugin":
		return runPlugins(ctx, env, rest, log)
	case "config":
		return runConfig(env, rest, log)
	default:
		fmt.Fprintf(env.Stderr, "dbxdl: unknown command %q\n\n", command)
		printUsage(env.Stderr)
		return 2
	}
}

// newFlagSet builds a FlagSet that reports errors without terminating the
// process, so Run keeps control over the exit code. usage is printed for
// -h/--help and for parse errors.
func newFlagSet(name string, env Env, usage func(io.Writer)) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	fs.Usage = func() { usage(env.Stdout) }
	return fs
}

// parseFlags parses args and returns the positional arguments.
//
// The standard flag package stops at the first non-flag argument, which would
// silently ignore "dbxdl download 0.6.13 --out-dir ./dist" or
// "dbxdl plugins show io.dbx.ssh --targets". To keep flags usable on either
// side of a positional argument, parsing resumes right after each one.
//
// It returns (positional, stop, exitCode); when stop is true the caller must
// return exitCode immediately.
func parseFlags(fs *flag.FlagSet, env Env, args []string, usage func(io.Writer)) (positional []string, stop bool, exitCode int) {
	remaining := args
	for {
		if err := fs.Parse(remaining); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				// fs.Usage already printed the help text.
				return positional, true, 0
			}
			fmt.Fprintf(env.Stderr, "dbxdl %s: %v\n\n", fs.Name(), err)
			usage(env.Stderr)
			return positional, true, 2
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positional, false, 0
		}
		positional = append(positional, rest[0])
		remaining = rest[1:]
	}
}

// setupLogger applies the global verbosity flags.
func setupLogger(log *ui.Logger, g *globalFlags, env Env) {
	switch {
	case g.quiet:
		log.SetLevel(ui.LevelError)
	case g.verbose:
		log.SetLevel(ui.LevelDebug)
	default:
		log.SetLevel(ui.LevelInfo)
	}
	log.SetColor(ui.ShouldColor(env.Stderr, g.color) && !g.noColor)
}

// loadConfig locates the configuration and reports which one was used.
//
// --config and $DBXDL_CONFIG must point at an existing file; otherwise the
// search falls back to ./config.yaml, then the per-user file, then the built-in
// defaults, so a globally installed dbxdl behaves the same everywhere.
func loadConfig(g *globalFlags, log *ui.Logger) (*config.Config, error) {
	cfg, err := config.Resolve(g.configPath)
	if err != nil {
		return nil, err
	}
	if cfg.Source == config.SourceDefault {
		log.Infof("no configuration file found (looked at %s); using built-in defaults",
			strings.Join(config.SearchPath(), ", "))
		log.Infof("run \"dbxdl config init\" here, or set $%s, to customize", config.EnvConfig)
	} else {
		log.Debugf("configuration: %s (from %s)", cfg.Path, cfg.Source)
	}
	return cfg, nil
}

// fail reports an error and returns the conventional exit code.
func fail(log *ui.Logger, env Env, err error) int {
	if errors.Is(err, context.Canceled) {
		log.Warnf("cancelled")
		return 130
	}
	log.Errorf("%v", err)
	fmt.Fprintf(env.Stderr, "\nrun \"dbxdl help\" for usage\n")
	return 1
}

// splitIDVersion parses an "id=version" override.
func splitIDVersion(s string) (string, string, error) {
	idx := strings.Index(s, "=")
	if idx <= 0 || idx == len(s)-1 {
		return "", "", fmt.Errorf("expected id=version, got %q", s)
	}
	return strings.TrimSpace(s[:idx]), strings.TrimSpace(s[idx+1:]), nil
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `dbxdl - download every DBX architecture package plus plugins, then tar them up

USAGE
  dbxdl <command> [flags]

COMMANDS
  download     Download a DBX release (and its plugins) and build the tar bundle
  latest       Shorthand for "download --version latest"
  list         List recent DBX releases published on GitHub
  plugins      Inspect the DBX plugin store (list, show)
  config       Manage config.yaml (init, show, path)
  version      Print the dbxdl version
  help         Show this message

EXAMPLES
  # newest release, using ./config.yaml (or the built-in defaults)
  dbxdl download

  # a specific release into ./dist
  dbxdl download --version 0.6.13 --out-dir ./dist

  # preview the plan without downloading anything
  dbxdl download --dry-run

  # ten most recent releases
  dbxdl list -n 10

  # plugin versions available in the store
  dbxdl plugins show io.dbx.ssh

  # write the commented default configuration
  dbxdl config init

LAYOUT
  The bundle reproduces the reference layout exactly:

    dbx<version>/
      DBX_<version>_arm64.dmg
      DBX_<version>_x64-setup.exe
      DBX_<version>_arm64-browser-static.tar.gz
      DBX_<version>_x64-portable.zip
      DBX_<version>_x64-browser-static.tar.gz
      plugins/
        s3/io.github.t8y2.s3-<pluginver>-<target>.dbxp
        ssh/io.dbx.ssh-<pluginver>-<target>.dbxp
    dbx<version>.tar.gz

Run "dbxdl <command> --help" for command-specific flags.
`)
}
