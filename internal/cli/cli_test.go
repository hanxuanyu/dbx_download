package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dbxdl/internal/config"
)

// run invokes the CLI in-process and captures stdout/stderr.
func run(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errBuf bytes.Buffer
	code = Run(context.Background(), Env{
		Stdout: &out,
		Stderr: &errBuf,
		Stdin:  strings.NewReader(""),
		Args:   args,
	})
	return code, out.String(), errBuf.String()
}

func TestVersionCommand(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"--version"}} {
		code, stdout, _ := run(t, args...)
		if code != 0 {
			t.Errorf("%v: exit code %d, want 0", args, code)
		}
		if !strings.Contains(stdout, "dbxdl") {
			t.Errorf("%v: stdout = %q", args, stdout)
		}
	}
}

func TestHelpCommand(t *testing.T) {
	for _, args := range [][]string{{"help"}, {"--help"}, {"-h"}, {}} {
		code, stdout, _ := run(t, args...)
		if len(args) == 0 {
			if code != 2 {
				t.Errorf("no arguments: exit code %d, want 2", code)
			}
		} else if code != 0 {
			t.Errorf("%v: exit code %d, want 0", args, code)
		}
		if !strings.Contains(stdout, "USAGE") {
			t.Errorf("%v: usage text missing from stdout: %q", args, stdout)
		}
	}
}

func TestUnknownCommand(t *testing.T) {
	code, _, stderr := run(t, "frobnicate")
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "unknown command") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestSubcommandHelpExitsZero(t *testing.T) {
	for _, cmd := range []string{"download", "list", "plugins", "config"} {
		code, stdout, _ := run(t, cmd, "--help")
		if code != 0 {
			t.Errorf("%s --help: exit code %d, want 0", cmd, code)
		}
		if !strings.Contains(stdout, "USAGE") {
			t.Errorf("%s --help: no usage text", cmd)
		}
	}
}

func TestUnknownFlagIsRejected(t *testing.T) {
	code, _, stderr := run(t, "download", "--nope")
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "flag provided but not defined") {
		t.Errorf("stderr = %q", stderr)
	}
}

// TestDownloadInvalidVersionFailsBeforeNetwork checks that bad input is
// rejected without contacting GitHub.
func TestDownloadInvalidVersionFailsBeforeNetwork(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(config.DefaultYAML()), 0o644); err != nil {
		t.Fatal(err)
	}

	code, _, stderr := run(t, "download", "--config", cfgPath, "--out-dir", dir, "--version", "not-a-version")
	if code != 1 {
		t.Errorf("exit code = %d, want 1 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stderr, "invalid version") {
		t.Errorf("stderr should explain the bad version: %q", stderr)
	}
}

func TestDownloadRejectsConflictingSelectors(t *testing.T) {
	code, _, stderr := run(t, "download", "--skip-dbx", "--skip-plugins")
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "cannot be combined") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestDownloadRejectsConflictingVersions(t *testing.T) {
	code, _, stderr := run(t, "download", "--version", "0.6.14", "0.6.13")
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "conflicting versions") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestDownloadRejectsBadPluginOverride(t *testing.T) {
	code, _, stderr := run(t, "download", "--plugin", "io.dbx.ssh")
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "id=version") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestDownloadMissingExplicitConfigIsFatal(t *testing.T) {
	code, _, stderr := run(t, "download", "--config", filepath.Join(t.TempDir(), "absent.yaml"))
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "does not exist") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestConfigInitAndShow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "config.yaml")

	code, _, stderr := run(t, "config", "init", "--out", path)
	if code != 0 {
		t.Fatalf("config init: exit %d (stderr: %s)", code, stderr)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config file not written: %v", err)
	}

	// Refuse to clobber without --force.
	code, _, stderr = run(t, "config", "init", "--out", path)
	if code != 1 || !strings.Contains(stderr, "--force") {
		t.Errorf("expected a refusal to overwrite: exit %d, stderr %q", code, stderr)
	}
	if code, _, stderr = run(t, "config", "init", "--out", path, "--force"); code != 0 {
		t.Errorf("config init --force: exit %d (stderr: %s)", code, stderr)
	}

	// config path resolves to the requested location.
	code, stdout, _ := run(t, "config", "path", "--config", path)
	if code != 0 {
		t.Fatalf("config path: exit %d", code)
	}
	if strings.TrimSpace(stdout) != path {
		t.Errorf("config path = %q, want %q", strings.TrimSpace(stdout), path)
	}

	// config show --json must reflect the file contents.
	code, stdout, stderr = run(t, "config", "show", "--config", path, "--json")
	if code != 0 {
		t.Fatalf("config show: exit %d (stderr: %s)", code, stderr)
	}
	var got config.Config
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("config show --json is not valid JSON: %v\n%s", err, stdout)
	}
	if len(got.Assets) != 5 || len(got.Plugins.Items) != 2 {
		t.Errorf("unexpected config: %d assets, %d plugins", len(got.Assets), len(got.Plugins.Items))
	}

	code, stdout, _ = run(t, "config", "show", "--config", path)
	if code != 0 || !strings.Contains(stdout, "io.github.t8y2.s3") {
		t.Errorf("config show output missing plugin list: %q", stdout)
	}
}

func TestConfigInitUsesDefaultFileName(t *testing.T) {
	// Change into an empty directory so the relative default lands there.
	dir := t.TempDir()
	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(original) })

	code, _, stderr := run(t, "config", "init")
	if code != 0 {
		t.Fatalf("exit %d (stderr: %s)", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, config.DefaultFileName)); err != nil {
		t.Errorf("expected %s in the working directory: %v", config.DefaultFileName, err)
	}
}

func TestPluginSubcommandUsage(t *testing.T) {
	code, stdout, _ := run(t, "plugins")
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stdout, "USAGE") {
		t.Errorf("stdout = %q", stdout)
	}

	code, _, stderr := run(t, "plugins", "frobnicate")
	if code != 2 || !strings.Contains(stderr, "unknown subcommand") {
		t.Errorf("exit %d, stderr %q", code, stderr)
	}

	// show requires exactly one plugin id, which is checked before any network
	// access.
	code, _, stderr = run(t, "plugins", "show")
	if code != 1 || !strings.Contains(stderr, "plugin-id") {
		t.Errorf("exit %d, stderr %q", code, stderr)
	}
}

func TestSplitIDVersion(t *testing.T) {
	id, version, err := splitIDVersion("io.dbx.ssh=0.4.76")
	if err != nil || id != "io.dbx.ssh" || version != "0.4.76" {
		t.Errorf("splitIDVersion = (%q, %q, %v)", id, version, err)
	}
	for _, bad := range []string{"", "io.dbx.ssh", "=1.0.0", "io.dbx.ssh="} {
		if _, _, err := splitIDVersion(bad); err == nil {
			t.Errorf("splitIDVersion(%q): expected an error", bad)
		}
	}
}

func TestListRejectsBadLimit(t *testing.T) {
	code, _, stderr := run(t, "list", "--limit", "0")
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "at least 1") {
		t.Errorf("stderr = %q", stderr)
	}
}

// TestUsageMentionsReferenceLayout keeps the documented output contract
// visible in the help text.
func TestUsageMentionsReferenceLayout(t *testing.T) {
	_, stdout, _ := run(t, "help")
	for _, want := range []string{
		"DBX_<version>_arm64.dmg",
		"DBX_<version>_x64-portable.zip",
		"plugins/",
		"dbx<version>.tar.gz",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("help text should mention %q", want)
		}
	}
}

// TestFlagsAfterPositionalArgument covers the standard flag package's habit of
// stopping at the first non-flag argument, which used to break the documented
// "dbxdl download 0.6.13 --out-dir ./dist" and
// "dbxdl plugins show <id> --targets" forms.
func TestFlagsAfterPositionalArgument(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(config.DefaultYAML()), 0o644); err != nil {
		t.Fatal(err)
	}

	// --out-dir precedes a trailing positional version.
	code, _, stderr := run(t, "download", "--config", cfgPath, "--out-dir", dir, "--asset", "DBX_{version}_riscv64.deb", "not-a-version")
	if code != 1 || !strings.Contains(stderr, "invalid version") {
		t.Errorf("the trailing positional version must reach the version parser: exit %d, stderr %q", code, stderr)
	}

	// The same, with the flag on the other side of the positional argument:
	// previously the flag was silently swallowed and the version was rejected
	// as "too many arguments".
	code, _, stderr = run(t, "download", "--config", cfgPath, "not-a-version", "--out-dir", dir)
	if code != 1 || !strings.Contains(stderr, "invalid version") {
		t.Errorf("flags after the positional version must still be parsed: exit %d, stderr %q", code, stderr)
	}

	// Two positionals are still rejected.
	code, _, stderr = run(t, "download", "0.6.14", "0.6.13")
	if code != 1 || !strings.Contains(stderr, "at most one version") {
		t.Errorf("exit %d, stderr %q", code, stderr)
	}
}

// TestPluginsShowAcceptsTrailingFlags checks the subcommand argument order used
// throughout the documentation.
func TestPluginsShowAcceptsTrailingFlags(t *testing.T) {
	// --help after the id must be honoured before any network access.
	code, stdout, stderr := run(t, "plugins", "show", "io.dbx.ssh", "--help")
	if code != 0 {
		t.Fatalf("exit %d (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "USAGE") {
		t.Errorf("stdout = %q", stdout)
	}

	// The id itself must be accepted with flags on either side.
	code, _, stderr = run(t, "plugins", "show", "--json", "io.dbx.ssh", "--targets")
	if code == 1 && strings.Contains(stderr, "usage:") {
		t.Errorf("the plugin id was not recognised as the positional argument: %q", stderr)
	}
}
