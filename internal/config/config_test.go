package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"

	"dbxdl/internal/naming"
)

// TestDefaultYAMLMatchesDefaults guards against drift between the commented
// template embedded in the binary and the programmatic defaults. If someone
// edits default.yaml without updating Default() (or vice versa), this fails.
func TestDefaultYAMLMatchesDefaults(t *testing.T) {
	var fromYAML Config
	if err := yaml.Unmarshal([]byte(DefaultYAML()), &fromYAML); err != nil {
		t.Fatalf("embedded default.yaml does not parse: %v", err)
	}
	want := Default()
	if !reflect.DeepEqual(fromYAML, *want) {
		t.Errorf("embedded default.yaml and config.Default() differ\nfrom yaml: %+v\nfrom code: %+v", fromYAML, *want)
	}
}

func TestDefaultConfigValidates(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatalf("built-in defaults must be valid: %v", err)
	}
}

func TestLoadMissingFileFallsBackToDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	cfg, err := Load(path)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	got := *cfg
	got.Path = "" // Load records where it looked, which Default() does not.
	if !reflect.DeepEqual(got, *Default()) {
		t.Error("missing file should yield the built-in defaults")
	}
	if cfg.Path != path {
		t.Errorf("Path = %q, want %q", cfg.Path, path)
	}
}

func TestLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(DefaultYAML()), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.GitHub.Owner != "t8y2" || cfg.GitHub.Repo != "dbx" {
		t.Errorf("unexpected github section: %+v", cfg.GitHub)
	}
	if got := len(cfg.Assets); got != 5 {
		t.Errorf("assets = %d, want 5", got)
	}
	if got := len(cfg.Plugins.Items); got != 2 {
		t.Errorf("plugin items = %d, want 2", got)
	}
	if cfg.Plugins.Items[0].SubDir() != "s3" {
		t.Errorf("plugin dir = %q, want s3", cfg.Plugins.Items[0].SubDir())
	}
	if cfg.Plugins.Items[1].SubDir() != "ssh" {
		t.Errorf("plugin dir = %q, want ssh", cfg.Plugins.Items[1].SubDir())
	}
	if cfg.GitHub.Timeout.Duration().Seconds() != 60 {
		t.Errorf("timeout = %v, want 60s", cfg.GitHub.Timeout.Duration())
	}
}

// TestLoadRejectsUnknownKeys makes sure a typo such as "plugnis:" is reported
// instead of silently disabling a whole section.
func TestLoadRejectsUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := "github:\n  owner: t8y2\n  repo: dbx\nplugnis:\n  enabled: true\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected an error for the unknown key")
	}
}

func TestValidateCatchesBadValues(t *testing.T) {
	cases := map[string]func(*Config){
		"empty owner":       func(c *Config) { c.GitHub.Owner = "" },
		"bad regex":         func(c *Config) { c.GitHub.TagPattern = "(" },
		"bad concurrency":   func(c *Config) { c.Output.Concurrency = 0 },
		"bad format":        func(c *Config) { c.Archive.Format = "rar" },
		"bad gzip":          func(c *Config) { c.Archive.GzipLevel = 42 },
		"no assets":         func(c *Config) { c.Assets = nil },
		"no targets":        func(c *Config) { c.Plugins.Targets = nil },
		"bad plugin source": func(c *Config) { c.Plugins.Source = "ftp" },
		"bad policy":        func(c *Config) { c.Plugins.VersionPolicy = "newest" },
		"bad plugin ver":    func(c *Config) { c.Plugins.Items[0].Version = "one" },
		"duplicate plugin":  func(c *Config) { c.Plugins.Items[1].ID = c.Plugins.Items[0].ID },
	}
	for name, mutate := range cases {
		cfg := Default()
		mutate(cfg)
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}
}

// TestPluginSubDirFallsBackToID pins the documented behaviour: with no "dir"
// configured the plugin id itself is used, with no short-name derivation.
func TestPluginSubDirFallsBackToID(t *testing.T) {
	cases := []struct {
		item PluginItem
		want string
	}{
		{item: PluginItem{ID: "io.github.t8y2.s3"}, want: "io.github.t8y2.s3"},
		{item: PluginItem{ID: "io.dbx.ssh"}, want: "io.dbx.ssh"},
		{item: PluginItem{ID: "io.github.t8y2.s3", Dir: "s3"}, want: "s3"},
		{item: PluginItem{ID: "io.dbx.ssh", Dir: "  ssh  "}, want: "ssh"},
	}
	for _, tc := range cases {
		if got := tc.item.SubDir(); got != tc.want {
			t.Errorf("PluginItem{ID: %q, Dir: %q}.SubDir() = %q, want %q",
				tc.item.ID, tc.item.Dir, got, tc.want)
		}
	}
}

func TestDefaultYAMLConfiguresPluginDirs(t *testing.T) {
	cfg := Default()
	for _, item := range cfg.Plugins.Items {
		if item.Dir == "" {
			t.Errorf("default.yaml must spell out plugins.items[%s].dir", item.ID)
		}
	}
}

func TestArchiveFileName(t *testing.T) {
	cfg := Default()
	v, err := naming.ParseVersion("0.6.14")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.ArchiveFileName(v); got != "dbx0.6.14.tar.gz" {
		t.Errorf("ArchiveFileName = %q, want dbx0.6.14.tar.gz", got)
	}
	cfg.Archive.Format = FormatTar
	cfg.Archive.NameTemplate = "DBX-{version}-bundle"
	if got := cfg.ArchiveFileName(v); got != "DBX-0.6.14-bundle.tar" {
		t.Errorf("ArchiveFileName = %q, want DBX-0.6.14-bundle.tar", got)
	}
}
