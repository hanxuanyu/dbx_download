// Package config loads and validates dbxdl's YAML configuration.
//
// Every field has a sensible built-in default so the tool works without a
// configuration file at all; "dbxdl config init" materializes the defaults
// (with comments) into ./config.yaml.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"dbxdl/internal/naming"
)

// DefaultFileName is the configuration file dbxdl looks for in the current
// directory when --config is not given.
const DefaultFileName = "config.yaml"

// EnvConfig names the environment variable that points at a configuration
// file. It is what makes a globally installed dbxdl behave identically in
// every directory.
const EnvConfig = "DBXDL_CONFIG"

// EnvConfigHome names the environment variable that relocates the per-user
// configuration directory (the XDG base directory specification).
const EnvConfigHome = "XDG_CONFIG_HOME"

// ErrNotFound is returned by Load when the configuration file does not exist.
// Callers may fall back to the built-in defaults.
var ErrNotFound = errors.New("config file not found")

// SourceKind records where a configuration came from.
type SourceKind string

const (
	// SourceFlag is the path passed with --config.
	SourceFlag SourceKind = "flag"
	// SourceEnv is the path named by $DBXDL_CONFIG.
	SourceEnv SourceKind = "env"
	// SourceCwd is ./config.yaml in the working directory.
	SourceCwd SourceKind = "cwd"
	// SourceUser is the per-user configuration file.
	SourceUser SourceKind = "user"
	// SourceDefault means no file was found and the built-in defaults apply.
	SourceDefault SourceKind = "default"
)

// UserConfigPath returns the per-user configuration path:
// $XDG_CONFIG_HOME/dbxdl/config.yaml, defaulting to
// ~/.config/dbxdl/config.yaml. It returns "" when no home directory can be
// determined.
func UserConfigPath() string {
	if dir := strings.TrimSpace(os.Getenv(EnvConfigHome)); dir != "" {
		return filepath.Join(dir, "dbxdl", DefaultFileName)
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".config", "dbxdl", DefaultFileName)
}

// Duration is a time.Duration that unmarshals from a Go duration string
// ("90s", "2m"), which is friendlier than raw nanoseconds in YAML.
type Duration time.Duration

// Duration returns the value as a time.Duration.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return fmt.Errorf("duration must be a string such as \"90s\": %w", err)
	}
	if s == "" {
		*d = 0
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

// MarshalYAML implements yaml.Marshaler.
func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

// Config is the root document.
type Config struct {
	GitHub  GitHub  `yaml:"github"`
	Store   Store   `yaml:"store"`
	Output  Output  `yaml:"output"`
	Archive Archive `yaml:"archive"`
	Assets  []Asset `yaml:"assets"`
	Plugins Plugins `yaml:"plugins"`

	// Path records where the configuration was loaded from; it is not part of
	// the YAML document. It is empty when the built-in defaults are in use.
	Path string `yaml:"-" json:"-"`
	// Source records how the configuration was located. It is set by Resolve.
	Source SourceKind `yaml:"-" json:"-"`
}

// GitHub locates the DBX application releases.
type GitHub struct {
	Owner   string `yaml:"owner"`
	Repo    string `yaml:"repo"`
	APIBase string `yaml:"api_base"`
	// WebBase is the website root, used for the Atom release feed fallback.
	// Override it together with api_base when pointing at GitHub Enterprise.
	WebBase string `yaml:"web_base"`
	// Token is optional. Prefer the GITHUB_TOKEN / GH_TOKEN environment
	// variables so the token never lands in a file.
	Token string `yaml:"token"`
	// TagPattern selects application releases and must capture the version.
	TagPattern string `yaml:"tag_pattern"`
	// IncludePrerelease allows tags such as v0.7.0-rc.1 to win "latest".
	IncludePrerelease bool     `yaml:"include_prerelease"`
	Timeout           Duration `yaml:"timeout"`
}

// Store locates the DBX plugin marketplace metadata.
type Store struct {
	Owner string `yaml:"owner"`
	Repo  string `yaml:"repo"`
	Ref   string `yaml:"ref"`
	// RawBase is the raw content host, normally
	// https://raw.githubusercontent.com. Reading metadata from raw content
	// does not consume GitHub API quota. Override it for a mirror or GitHub
	// Enterprise deployment.
	RawBase string `yaml:"raw_base"`
	// PluginsPath is the directory inside the store repository holding one
	// JSON document per plugin.
	PluginsPath string   `yaml:"plugins_path"`
	Timeout     Duration `yaml:"timeout"`
}

// Output controls where and how files are written.
type Output struct {
	Dir string `yaml:"dir"`
	// DirNameTemplate is the bundle directory name; supports {version}.
	DirNameTemplate string `yaml:"dir_name_template"`
	// KeepDir keeps the staging directory after archiving. When false (the
	// default) the directory is deleted once the archive is safely written,
	// so only the tar file remains.
	KeepDir bool `yaml:"keep_dir"`
	// Force re-downloads files that are already present and valid.
	Force       bool `yaml:"force"`
	Concurrency int  `yaml:"concurrency"`
	Retries     int  `yaml:"retries"`
	VerifySHA   bool `yaml:"verify_sha256"`
}

// Archive controls the final tar packaging.
type Archive struct {
	Enabled bool `yaml:"enabled"`
	// Format is "tar.gz" or "tar".
	Format string `yaml:"format"`
	// NameTemplate is the archive base name without extension; supports
	// {version}. The extension follows Format.
	NameTemplate string `yaml:"name_template"`
	// GzipLevel is 1 (fastest) to 9 (best). The payload is already compressed
	// by upstream, so the default favours speed.
	GzipLevel int `yaml:"gzip_level"`
	// SHA256Sidecar writes "<archive>.sha256" next to the archive.
	SHA256Sidecar bool `yaml:"sha256_sidecar"`
}

// Asset is one release asset to include in the bundle. Selector is matched
// against the GitHub release asset name and supports "*", "?" and "{version}".
type Asset struct {
	Selector    string `yaml:"selector"`
	Description string `yaml:"description"`
}

// Plugins configures the plugin portion of the bundle.
type Plugins struct {
	Enabled bool `yaml:"enabled"`
	// Source is "store" (metadata download URL), "github" (the plugin's own
	// release assets) or "auto" (store with a GitHub fallback).
	Source string `yaml:"source"`
	// Root is the directory inside the bundle holding the plugin folders.
	Root string `yaml:"root"`
	// VersionPolicy is "pinned" (use each item's version) or "latest".
	VersionPolicy string `yaml:"version_policy"`
	// Targets are the artifact targets to fetch, matching the store metadata
	// "target" field.
	Targets []string     `yaml:"targets"`
	Items   []PluginItem `yaml:"items"`
}

// PluginItem is a single plugin to bundle.
type PluginItem struct {
	ID string `yaml:"id"`
	// Version pins the plugin version. Empty means "use latestVersion".
	Version string `yaml:"version"`
	// Dir is the directory the plugin's artifacts land in. When it is empty
	// the plugin id itself is used (io.github.t8y2.s3 -> io.github.t8y2.s3/).
	Dir string `yaml:"dir"`
}

// SubDir returns the plugin's directory inside the bundle: the configured Dir,
// or the plugin id when Dir is unset.
func (p PluginItem) SubDir() string {
	if p.Dir != "" {
		return naming.SanitizeName(p.Dir)
	}
	return naming.SanitizeName(p.ID)
}

// Source constants for Plugins.Source.
const (
	SourceStore  = "store"
	SourceGitHub = "github"
	SourceAuto   = "auto"
)

// VersionPolicy constants for Plugins.VersionPolicy.
const (
	PolicyPinned = "pinned"
	PolicyLatest = "latest"
)

// Default returns the built-in configuration, which mirrors the bundled
// config.yaml: the five DBX packages plus the s3 and ssh plugins for the
// linux-x64, linux-arm64 and windows-x64 targets.
func Default() *Config {
	return &Config{
		GitHub: GitHub{
			Owner:             "t8y2",
			Repo:              "dbx",
			APIBase:           "https://api.github.com",
			WebBase:           "https://github.com",
			TagPattern:        naming.DefaultTagPattern,
			IncludePrerelease: false,
			Timeout:           Duration(60 * time.Second),
		},
		Store: Store{
			Owner:       "t8y2",
			Repo:        "dbx-store",
			Ref:         "main",
			RawBase:     "https://raw.githubusercontent.com",
			PluginsPath: "plugins",
			Timeout:     Duration(60 * time.Second),
		},
		Output: Output{
			Dir:             ".",
			DirNameTemplate: "dbx{version}",
			// The bundle directory is a staging area: it is removed once the
			// archive has been written, leaving only the tar file.
			KeepDir:     false,
			Force:       false,
			Concurrency: 4,
			Retries:     3,
			VerifySHA:   true,
		},
		Archive: Archive{
			Enabled:       true,
			Format:        FormatTarGz,
			NameTemplate:  "dbx{version}",
			GzipLevel:     1,
			SHA256Sidecar: true,
		},
		Assets: []Asset{
			{Selector: "DBX_{version}_arm64.dmg", Description: "macOS (Apple Silicon) 安装包"},
			{Selector: "DBX_{version}_x64-setup.exe", Description: "Windows x64 安装程序"},
			{Selector: "DBX_{version}_arm64-browser-static.tar.gz", Description: "Linux arm64（browser static）"},
			{Selector: "DBX_{version}_x64-portable.zip", Description: "Windows x64 免安装便携版"},
			{Selector: "DBX_{version}_x64-browser-static.tar.gz", Description: "Linux x64（browser static）"},
		},
		Plugins: Plugins{
			Enabled:       true,
			Source:        SourceStore,
			Root:          "plugins",
			VersionPolicy: PolicyPinned,
			Targets:       []string{"linux-x64", "linux-arm64", "windows-x64"},
			Items: []PluginItem{
				{ID: "io.github.t8y2.s3", Version: "0.1.9", Dir: "s3"},
				{ID: "io.dbx.ssh", Version: "0.4.76", Dir: "ssh"},
			},
		},
	}
}

// Archive format constants.
const (
	FormatTarGz = "tar.gz"
	FormatTar   = "tar"
)

// Load reads the configuration from path. An empty path means
// ./config.yaml. When the file is missing it returns the built-in defaults
// together with ErrNotFound so the caller can decide whether that is fatal.
func Load(path string) (*Config, error) {
	if path == "" {
		path = DefaultFileName
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			cfg := Default()
			cfg.Path = path
			return cfg, ErrNotFound
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	cfg := Default()
	dec := yaml.NewDecoder(bytes.NewReader(data))
	// Unknown keys are almost always typos, so fail loudly instead of silently
	// ignoring a mistyped option.
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	cfg.Path = path
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// Resolve loads the configuration dbxdl should use, searching in this order:
//
//  1. explicitPath, i.e. --config; a missing file is an error
//  2. $DBXDL_CONFIG; a missing file is an error
//  3. ./config.yaml in the working directory
//  4. <user config dir>/dbxdl/config.yaml
//  5. the built-in defaults
//
// Unlike Load it never returns ErrNotFound: the built-in defaults are a valid
// outcome, reported through Config.Source.
func Resolve(explicitPath string) (*Config, error) {
	if p := strings.TrimSpace(explicitPath); p != "" {
		cfg, err := loadFrom(p, SourceFlag)
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("config file %s does not exist", p)
		}
		if err != nil {
			return nil, err
		}
		return cfg, nil
	}

	if p := strings.TrimSpace(os.Getenv(EnvConfig)); p != "" {
		cfg, err := loadFrom(p, SourceEnv)
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("config file %s does not exist (from $%s)", p, EnvConfig)
		}
		if err != nil {
			return nil, err
		}
		return cfg, nil
	}

	cfg, err := loadFrom(DefaultFileName, SourceCwd)
	if err == nil {
		return cfg, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}

	if p := UserConfigPath(); p != "" {
		cfg, err = loadFrom(p, SourceUser)
		if err == nil {
			return cfg, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return nil, err
		}
	}

	fallback := Default()
	fallback.Source = SourceDefault
	return fallback, nil
}

// loadFrom reads one candidate location, tagging the result with its origin.
func loadFrom(path string, source SourceKind) (*Config, error) {
	cfg, err := Load(path)
	if err != nil {
		return nil, err
	}
	cfg.Source = source
	return cfg, nil
}

// SearchPath lists the locations Resolve consults, for help and error text.
func SearchPath() []string {
	paths := []string{DefaultFileName}
	if p := UserConfigPath(); p != "" {
		paths = append(paths, p)
	}
	return paths
}

// ApplyDefaults fills in fields that were omitted from the YAML document.
func (c *Config) ApplyDefaults() {
	d := Default()
	if c.GitHub.Owner == "" {
		c.GitHub.Owner = d.GitHub.Owner
	}
	if c.GitHub.Repo == "" {
		c.GitHub.Repo = d.GitHub.Repo
	}
	if c.GitHub.APIBase == "" {
		c.GitHub.APIBase = d.GitHub.APIBase
	}
	if c.GitHub.WebBase == "" {
		c.GitHub.WebBase = d.GitHub.WebBase
	}
	if c.GitHub.TagPattern == "" {
		c.GitHub.TagPattern = d.GitHub.TagPattern
	}
	if c.GitHub.Timeout <= 0 {
		c.GitHub.Timeout = d.GitHub.Timeout
	}
	if c.Store.Owner == "" {
		c.Store.Owner = d.Store.Owner
	}
	if c.Store.Repo == "" {
		c.Store.Repo = d.Store.Repo
	}
	if c.Store.Ref == "" {
		c.Store.Ref = d.Store.Ref
	}
	if c.Store.RawBase == "" {
		c.Store.RawBase = d.Store.RawBase
	}
	if c.Store.PluginsPath == "" {
		c.Store.PluginsPath = d.Store.PluginsPath
	}
	if c.Store.Timeout <= 0 {
		c.Store.Timeout = d.Store.Timeout
	}
	if c.Output.Dir == "" {
		c.Output.Dir = d.Output.Dir
	}
	if c.Output.DirNameTemplate == "" {
		c.Output.DirNameTemplate = d.Output.DirNameTemplate
	}
	if c.Output.Concurrency <= 0 {
		c.Output.Concurrency = d.Output.Concurrency
	}
	if c.Output.Retries < 0 {
		c.Output.Retries = d.Output.Retries
	}
	if c.Archive.Format == "" {
		c.Archive.Format = d.Archive.Format
	}
	if c.Archive.NameTemplate == "" {
		c.Archive.NameTemplate = d.Archive.NameTemplate
	}
	if c.Archive.GzipLevel == 0 {
		c.Archive.GzipLevel = d.Archive.GzipLevel
	}
	if c.Plugins.Source == "" {
		c.Plugins.Source = d.Plugins.Source
	}
	if c.Plugins.Root == "" {
		c.Plugins.Root = d.Plugins.Root
	}
	if c.Plugins.VersionPolicy == "" {
		c.Plugins.VersionPolicy = d.Plugins.VersionPolicy
	}
}

// Validate reports configuration problems before any network traffic happens.
func (c *Config) Validate() error {
	if c.GitHub.Owner == "" || c.GitHub.Repo == "" {
		return errors.New("github.owner and github.repo must be set")
	}
	if _, err := regexp.Compile(c.GitHub.TagPattern); err != nil {
		return fmt.Errorf("github.tag_pattern is not a valid regular expression: %w", err)
	}
	if c.Output.Concurrency < 1 || c.Output.Concurrency > 32 {
		return fmt.Errorf("output.concurrency must be between 1 and 32, got %d", c.Output.Concurrency)
	}
	if c.Output.Retries < 0 || c.Output.Retries > 10 {
		return fmt.Errorf("output.retries must be between 0 and 10, got %d", c.Output.Retries)
	}
	switch c.Archive.Format {
	case FormatTar, FormatTarGz:
	default:
		return fmt.Errorf("archive.format must be %q or %q, got %q", FormatTar, FormatTarGz, c.Archive.Format)
	}
	if c.Archive.GzipLevel < 1 || c.Archive.GzipLevel > 9 {
		return fmt.Errorf("archive.gzip_level must be between 1 and 9, got %d", c.Archive.GzipLevel)
	}
	if len(c.Assets) == 0 {
		return errors.New("assets must list at least one release asset selector")
	}
	for i, a := range c.Assets {
		if a.Selector == "" {
			return fmt.Errorf("assets[%d].selector must not be empty", i)
		}
	}
	if !c.Plugins.Enabled {
		return nil
	}
	switch c.Plugins.Source {
	case SourceStore, SourceGitHub, SourceAuto:
	default:
		return fmt.Errorf("plugins.source must be %q, %q or %q, got %q",
			SourceStore, SourceGitHub, SourceAuto, c.Plugins.Source)
	}
	switch c.Plugins.VersionPolicy {
	case PolicyPinned, PolicyLatest:
	default:
		return fmt.Errorf("plugins.version_policy must be %q or %q, got %q",
			PolicyPinned, PolicyLatest, c.Plugins.VersionPolicy)
	}
	if len(c.Plugins.Targets) == 0 {
		return errors.New("plugins.targets must list at least one target")
	}
	seen := map[string]bool{}
	for i, it := range c.Plugins.Items {
		if it.ID == "" {
			return fmt.Errorf("plugins.items[%d].id must not be empty", i)
		}
		if seen[it.ID] {
			return fmt.Errorf("plugins.items contains duplicate id %q", it.ID)
		}
		seen[it.ID] = true
		if it.Version != "" {
			if _, err := naming.ParseVersion(it.Version); err != nil {
				return fmt.Errorf("plugins.items[%d] (%s): %w", i, it.ID, err)
			}
		}
		if sub := it.SubDir(); sub == "" {
			return fmt.Errorf("plugins.items[%d] (%s): could not derive a directory name", i, it.ID)
		}
	}
	return nil
}

// ArchiveFileName renders the final archive file name for a version.
func (c *Config) ArchiveFileName(v naming.Version) string {
	tmpl := c.Archive.NameTemplate
	if tmpl == "" {
		tmpl = "dbx{version}"
	}
	base := naming.Expand(tmpl, naming.AssetVars(v))
	return base + "." + c.Archive.Format
}
