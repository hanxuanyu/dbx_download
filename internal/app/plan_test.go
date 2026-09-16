package app

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"dbxdl/internal/archive"
	"dbxdl/internal/config"
	"dbxdl/internal/naming"
	"dbxdl/internal/ui"
)

// fixture is a self-contained fake of GitHub Releases and the dbx-store raw
// content, so the whole plan-and-download pipeline can be exercised offline.
type fixture struct {
	server *httptest.Server
	bodies map[string][]byte
}

const (
	s3Latest  = "0.1.9"
	sshLatest = "0.4.76"
)

// pluginTargets are the targets a plugin's own GitHub release publishes.
var pluginTargets = []string{"linux-x64", "linux-arm64", "windows-x64", "darwin-arm64", "darwin-x64"}

// storeTargets are the targets the store metadata advertises. darwin-x64 is
// deliberately missing so plugins.source=auto has something to fall back for.
var storeTargets = []string{"linux-x64", "linux-arm64", "windows-x64", "darwin-arm64"}

func (f *fixture) bodyFor(name string, body string) {
	f.bodies[name] = []byte(body)
}

func sha(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{bodies: map[string][]byte{}}

	// Release asset payloads. The .sha256 sidecar mirrors what DBX publishes
	// for the browser-static bundles only.
	releaseAssets := []struct {
		name string
		body string
	}{
		{"DBX_0.6.14_arm64.dmg", "dmg-payload"},
		{"DBX_0.6.14_x64-setup.exe", "exe-payload"},
		{"DBX_0.6.14_arm64-browser-static.tar.gz", "arm64-browser-payload"},
		{"DBX_0.6.14_x64-portable.zip", "portable-payload"},
		{"DBX_0.6.14_x64-browser-static.tar.gz", "x64-browser-payload"},
		// Assets that must NOT be selected by the default configuration.
		{"DBX_0.6.14_amd64.deb", "deb-payload"},
		{"DBX-0.6.14-1.x86_64.rpm", "rpm-payload"},
		{"DBX_0.6.14_x64-offline-setup.exe", "offline-payload"},
		{"latest.json", "{}"},
	}
	for _, a := range releaseAssets {
		f.bodyFor(a.name, a.body)
	}
	f.bodyFor("DBX_0.6.14_x64-browser-static.tar.gz.sha256",
		sha([]byte("x64-browser-payload"))+"  DBX_0.6.14_x64-browser-static.tar.gz\n")
	f.bodyFor("DBX_0.6.14_arm64-browser-static.tar.gz.sha256",
		sha([]byte("arm64-browser-payload"))+"  DBX_0.6.14_arm64-browser-static.tar.gz\n")

	// Plugin artifact payloads, one per id/target.
	for _, id := range []string{"io.github.t8y2.s3", "io.dbx.ssh"} {
		for _, target := range []string{"linux-x64", "linux-arm64", "windows-x64", "darwin-arm64", "darwin-x64"} {
			f.bodyFor(fmt.Sprintf("%s|%s", id, target), id+"-"+target+"-payload")
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", f.handle)
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fixture) handle(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	switch {
	case path == "/repos/t8y2/dbx/releases":
		f.writeReleaseList(w)
		return
	case path == "/repos/t8y2/dbx/releases/tags/v0.6.14":
		f.writeJSON(w, f.release())
		return
	case strings.HasSuffix(path, "/revoked.json"):
		f.writeJSON(w, map[string]any{"version": 1, "pluginVersions": []any{}, "signingKeys": []any{}})
		return
	case strings.HasSuffix(path, "/plugins/io.github.t8y2.s3.json"):
		f.writeJSON(w, s3Metadata(f))
		return
	case strings.HasSuffix(path, "/plugins/io.dbx.ssh.json"):
		f.writeJSON(w, sshMetadata(f))
		return
	case path == "/repos/t8y2/dbx-plugin-s3/releases":
		// The plugin's own releases, used when the store tag hint is stale.
		f.writeJSON(w, []any{
			pluginRelease(f, "t8y2/dbx-plugin-s3", "v0.1.9", "io.github.t8y2.s3", "0.1.9"),
			pluginRelease(f, "t8y2/dbx-plugin-s3", "v0.1.8", "io.github.t8y2.s3", "0.1.8"),
		})
		return
	case path == "/repos/jinpy666/dbx-plugin-ssh/releases/tags/ssh-v0.4.76":
		f.writeJSON(w, pluginRelease(f, "jinpy666/dbx-plugin-ssh", "ssh-v0.4.76", "io.dbx.ssh", "0.4.76"))
		return
	case path == "/repos/t8y2/dbx-plugin-s3/releases/tags/v0.1.9":
		f.writeJSON(w, pluginRelease(f, "t8y2/dbx-plugin-s3", "v0.1.9", "io.github.t8y2.s3", "0.1.9"))
		return
	}

	// Artifact payloads.
	if strings.HasPrefix(path, "/dl/plugin/") {
		// /dl/plugin/<id>/<version>/<id>-<version>-<target>.dbxp
		parts := strings.Split(strings.TrimPrefix(path, "/dl/plugin/"), "/")
		if len(parts) == 3 {
			file := strings.TrimSuffix(parts[2], ".dbxp")
			for _, target := range pluginTargets {
				if strings.HasSuffix(file, "-"+target) {
					key := fmt.Sprintf("%s|%s", parts[0], target)
					if body, ok := f.bodies[key]; ok {
						_, _ = w.Write(body)
						return
					}
				}
			}
		}
		http.NotFound(w, r)
		return
	}
	name := strings.TrimPrefix(path, "/dl/")
	if body, ok := f.bodies[name]; ok {
		_, _ = w.Write(body)
		return
	}
	http.NotFound(w, r)
}

// pluginRelease fakes one release of a plugin's own repository, with the
// per-target assets plugins publish.
func pluginRelease(f *fixture, repo, tag, id, version string) map[string]any {
	var assets []map[string]any
	for _, target := range pluginTargets {
		body := f.bodies[fmt.Sprintf("%s|%s", id, target)]
		assets = append(assets, map[string]any{
			"name":                 fmt.Sprintf("%s-%s-%s.dbxp", id, version, target),
			"size":                 len(body),
			"browser_download_url": fmt.Sprintf("%s/dl/plugin/%s/%s/%s-%s-%s.dbxp", f.server.URL, id, version, id, version, target),
		})
	}
	return map[string]any{
		"tag_name":     tag,
		"name":         id + " " + version,
		"html_url":     "https://github.com/" + repo + "/releases/tag/" + tag,
		"draft":        false,
		"prerelease":   false,
		"created_at":   "2026-09-15T13:21:49Z",
		"published_at": "2026-09-15T13:21:49Z",
		"assets":       assets,
	}
}

func (f *fixture) writeReleaseList(w http.ResponseWriter) {
	// The real repository interleaves unrelated tag families; include one so a
	// regression in tag filtering is caught.
	other := map[string]any{
		"tag_name": "packages-v0.4.88", "name": "packages v0.4.88",
		"draft": false, "prerelease": false,
		"created_at": "2026-09-15T18:00:00Z", "published_at": "2026-09-15T18:02:06Z",
		"assets": []any{},
	}
	f.writeJSON(w, []any{other, f.release()})
}

func (f *fixture) release() map[string]any {
	names := []string{
		"DBX_0.6.14_arm64.dmg",
		"DBX_0.6.14_x64-setup.exe",
		"DBX_0.6.14_arm64-browser-static.tar.gz",
		"DBX_0.6.14_arm64-browser-static.tar.gz.sha256",
		"DBX_0.6.14_x64-portable.zip",
		"DBX_0.6.14_x64-browser-static.tar.gz",
		"DBX_0.6.14_x64-browser-static.tar.gz.sha256",
		"DBX_0.6.14_amd64.deb",
		"DBX-0.6.14-1.x86_64.rpm",
		"DBX_0.6.14_x64-offline-setup.exe",
		"latest.json",
	}
	assets := make([]map[string]any, 0, len(names))
	for _, name := range names {
		body := f.bodies[name]
		assets = append(assets, map[string]any{
			"name":                 name,
			"size":                 len(body),
			"browser_download_url": f.server.URL + "/dl/" + name,
		})
	}
	return map[string]any{
		"tag_name":     "v0.6.14",
		"name":         "DBX v0.6.14",
		"html_url":     "https://github.com/t8y2/dbx/releases/tag/v0.6.14",
		"draft":        false,
		"prerelease":   false,
		"created_at":   "2026-09-15T17:40:35Z",
		"published_at": "2026-09-15T19:10:49Z",
		"assets":       assets,
	}
}

// pluginArtifact describes one store artifact.
func pluginArtifact(f *fixture, id, version, target string) map[string]any {
	body := f.bodies[fmt.Sprintf("%s|%s", id, target)]
	url := fmt.Sprintf("%s/dl/plugin/%s/%s/%s-%s-%s.dbxp", f.server.URL, id, version, id, version, target)
	return map[string]any{
		"target":       target,
		"url":          url,
		"sha256":       sha(body),
		"signingKeyId": "dbx-store-release-2026",
		"size":         len(body),
	}
}

func s3Metadata(f *fixture) map[string]any {
	targets := storeTargets
	var versions []map[string]any
	for _, version := range []string{"0.1.8", s3Latest} {
		artifacts := make([]map[string]any, 0, len(targets))
		for _, target := range targets {
			artifacts = append(artifacts, pluginArtifact(f, "io.github.t8y2.s3", version, target))
		}
		versions = append(versions, map[string]any{
			"version": version, "releasedAt": "2026-09-15T13:21:49Z", "artifacts": artifacts,
		})
	}
	return map[string]any{
		"id":        "io.github.t8y2.s3",
		"name":      "S3 Browser",
		"publisher": "t8y2",
		"homepage":  "https://github.com/t8y2/dbx-plugin-s3",
		// Deliberately stale, exactly like the real store document was: the
		// source tag lags behind latestVersion, so the planner must not trust
		// it and has to query the plugin's release list instead.
		"source":        "https://github.com/t8y2/dbx-plugin-s3/tree/v0.1.8",
		"latestVersion": s3Latest,
		"versions":      versions,
	}
}

func sshMetadata(f *fixture) map[string]any {
	targets := storeTargets
	var versions []map[string]any
	for _, version := range []string{"0.4.75", sshLatest} {
		artifacts := make([]map[string]any, 0, len(targets))
		for _, target := range targets {
			artifacts = append(artifacts, pluginArtifact(f, "io.dbx.ssh", version, target))
		}
		versions = append(versions, map[string]any{
			"version": version, "releasedAt": "2026-09-15T09:51:35Z", "artifacts": artifacts,
		})
	}
	return map[string]any{
		"id":            "io.dbx.ssh",
		"name":          "SSH Terminal",
		"publisher":     "jinpy",
		"homepage":      "https://github.com/jinpy666/dbx-plugin-ssh",
		"source":        "https://github.com/jinpy666/dbx-plugin-ssh/tree/ssh-v" + sshLatest,
		"latestVersion": sshLatest,
		"versions":      versions,
	}
}

func (f *fixture) writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// testConfig points every data source at the fixture server, so the tests are
// fully offline: no request may escape to api.github.com or
// raw.githubusercontent.com.
func testConfig(f *fixture) *config.Config {
	cfg := config.Default()
	cfg.GitHub.APIBase = f.server.URL
	cfg.GitHub.WebBase = f.server.URL
	cfg.Store.RawBase = f.server.URL
	cfg.Output.Concurrency = 2
	cfg.Output.Retries = 1
	return cfg
}

func newTestPlanner(t *testing.T, f *fixture, cfg *config.Config) *Planner {
	t.Helper()
	return NewPlanner(cfg, ui.NewLogger(io.Discard, ui.LevelError, false))
}

// referencePaths is the exact layout of the user's hand-made bundle.
func referencePaths() []string {
	paths := []string{
		"DBX_0.6.14_arm64-browser-static.tar.gz",
		"DBX_0.6.14_arm64.dmg",
		"DBX_0.6.14_x64-browser-static.tar.gz",
		"DBX_0.6.14_x64-portable.zip",
		"DBX_0.6.14_x64-setup.exe",
	}
	for _, target := range []string{"linux-arm64", "linux-x64", "windows-x64"} {
		paths = append(paths, fmt.Sprintf("plugins/s3/io.github.t8y2.s3-%s-%s.dbxp", s3Latest, target))
	}
	for _, target := range []string{"linux-arm64", "linux-x64", "windows-x64"} {
		paths = append(paths, fmt.Sprintf("plugins/ssh/io.dbx.ssh-%s-%s.dbxp", sshLatest, target))
	}
	sort.Strings(paths)
	return paths
}

func TestBuildLatestReproducesReferenceLayout(t *testing.T) {
	f := newFixture(t)
	cfg := testConfig(f)
	planner := newTestPlanner(t, f, cfg)

	plan, err := planner.Build(context.Background(), Options{
		Version:        "latest",
		OutDir:         t.TempDir(),
		IncludeRelease: true,
		IncludePlugins: true,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if plan.Version.String() != "0.6.14" {
		t.Errorf("version = %s, want 0.6.14 (the unrelated packages-* tag must be ignored)", plan.Version)
	}
	if plan.DirName != "dbx0.6.14" {
		t.Errorf("DirName = %q, want dbx0.6.14", plan.DirName)
	}
	if got := filepath.Base(plan.ArchivePath); got != "dbx0.6.14.tar.gz" {
		t.Errorf("ArchivePath base = %q, want dbx0.6.14.tar.gz", got)
	}

	var got []string
	for _, item := range plan.Items {
		got = append(got, item.RelPath)
	}
	sort.Strings(got)
	if want := referencePaths(); !reflect.DeepEqual(got, want) {
		t.Errorf("plan paths mismatch\n got: %v\nwant: %v", got, want)
	}

	// The two sha256 sidecars published alongside the browser-static bundles
	// must be picked up as expected digests, and every plugin must carry the
	// digest from the store metadata.
	var releaseHashed, pluginHashed, pluginTotal int
	for _, item := range plan.Items {
		if item.Kind == KindPlugin {
			pluginTotal++
		}
		if item.SHA256 == "" {
			continue
		}
		if len(item.SHA256) != 64 {
			t.Errorf("%s: SHA256 = %q", item.RelPath, item.SHA256)
		}
		if item.Kind == KindRelease {
			releaseHashed++
		} else {
			pluginHashed++
		}
	}
	if releaseHashed != 2 {
		t.Errorf("%d release items carry a checksum, want 2", releaseHashed)
	}
	if pluginTotal != 6 || pluginHashed != 6 {
		t.Errorf("plugin checksums = %d/%d, want 6/6", pluginHashed, pluginTotal)
	}

	// Plugin versions must honour the pinned config values.
	resolved := map[string]string{}
	for _, p := range plan.Plugins {
		resolved[p.ID] = p.Version
	}
	if resolved["io.github.t8y2.s3"] != s3Latest {
		t.Errorf("s3 version = %q, want %q", resolved["io.github.t8y2.s3"], s3Latest)
	}
	if resolved["io.dbx.ssh"] != sshLatest {
		t.Errorf("ssh version = %q, want %q", resolved["io.dbx.ssh"], sshLatest)
	}
	if len(plan.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", plan.Warnings)
	}
}

func TestBuildExplicitVersionUsesTagEndpoint(t *testing.T) {
	f := newFixture(t)
	cfg := testConfig(f)
	planner := newTestPlanner(t, f, cfg)

	plan, err := planner.Build(context.Background(), Options{
		Version: "v0.6.14", OutDir: t.TempDir(), IncludeRelease: true,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if plan.Release.TagName != "v0.6.14" {
		t.Errorf("tag = %q, want v0.6.14", plan.Release.TagName)
	}
	if len(plan.Items) != 5 {
		t.Errorf("%d release items, want 5", len(plan.Items))
	}
}

func TestBuildMissingAssetFailsWithAssetList(t *testing.T) {
	f := newFixture(t)
	cfg := testConfig(f)
	cfg.Assets = append(cfg.Assets, config.Asset{Selector: "DBX_{version}_riscv64.deb"})
	planner := newTestPlanner(t, f, cfg)

	_, err := planner.Build(context.Background(), Options{
		Version: "latest", OutDir: t.TempDir(), IncludeRelease: true,
	})
	if err == nil {
		t.Fatal("expected an error for an unmatched selector")
	}
	if !strings.Contains(err.Error(), "riscv64") {
		t.Errorf("error should name the selector: %v", err)
	}
	if !strings.Contains(err.Error(), "DBX_0.6.14_amd64.deb") {
		t.Errorf("error should list the available assets: %v", err)
	}
}

func TestBuildAllowMissingDowngradesToWarning(t *testing.T) {
	f := newFixture(t)
	cfg := testConfig(f)
	cfg.Assets = append(cfg.Assets, config.Asset{Selector: "DBX_{version}_riscv64.deb"})
	planner := newTestPlanner(t, f, cfg)

	plan, err := planner.Build(context.Background(), Options{
		Version: "latest", OutDir: t.TempDir(), IncludeRelease: true, AllowMissing: true,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(plan.Warnings) != 1 {
		t.Errorf("warnings = %v, want 1", plan.Warnings)
	}
	if len(plan.Items) != 5 {
		t.Errorf("%d items, want the 5 available ones", len(plan.Items))
	}
}

// TestBuildPluginSourceGitHub covers the GitHub fallback, including the stale
// source-tag bug: s3 metadata advertises tree/v0.1.8 while 0.1.9 is requested,
// so the artifact must come from the v0.1.9 release found by listing releases.
func TestBuildPluginSourceGitHub(t *testing.T) {
	f := newFixture(t)
	cfg := testConfig(f)
	cfg.Plugins.Source = config.SourceGitHub
	cfg.Plugins.Targets = []string{"linux-x64"}
	planner := newTestPlanner(t, f, cfg)

	plan, err := planner.Build(context.Background(), Options{
		Version: "latest", OutDir: t.TempDir(), IncludePlugins: true,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(plan.Items) != 2 {
		t.Fatalf("%d items, want 2 (one target for each plugin)", len(plan.Items))
	}
	urls := map[string]string{}
	for _, item := range plan.Items {
		urls[item.RelPath] = item.URL
		if item.SHA256 != "" {
			t.Errorf("%s: GitHub release assets have no published digest here", item.RelPath)
		}
		if item.Size <= 0 {
			t.Errorf("%s: the GitHub API reports an asset size, so it must be set", item.RelPath)
		}
		if !strings.Contains(item.URL, f.server.URL) {
			t.Errorf("%s: URL %q is not the plugin release asset", item.RelPath, item.URL)
		}
	}

	// The path encodes the version, so this proves the right release was used.
	wantS3 := fmt.Sprintf("%s/dl/plugin/io.github.t8y2.s3/%s/io.github.t8y2.s3-%s-linux-x64.dbxp",
		f.server.URL, s3Latest, s3Latest)
	if got := urls["plugins/s3/io.github.t8y2.s3-"+s3Latest+"-linux-x64.dbxp"]; got != wantS3 {
		t.Errorf("s3 URL = %q, want %q (v0.1.8 must not be used)", got, wantS3)
	}
	wantSSH := fmt.Sprintf("%s/dl/plugin/io.dbx.ssh/%s/io.dbx.ssh-%s-linux-x64.dbxp",
		f.server.URL, sshLatest, sshLatest)
	if got := urls["plugins/ssh/io.dbx.ssh-"+sshLatest+"-linux-x64.dbxp"]; got != wantSSH {
		t.Errorf("ssh URL = %q, want %q", got, wantSSH)
	}
}

// TestBuildPluginSourceStoreWinsInAuto checks that auto prefers the store when
// the store has the artifact.
func TestBuildPluginSourceStoreWinsInAuto(t *testing.T) {
	f := newFixture(t)
	cfg := testConfig(f)
	cfg.Plugins.Source = config.SourceAuto
	cfg.Plugins.Targets = []string{"windows-x64"}
	planner := newTestPlanner(t, f, cfg)

	plan, err := planner.Build(context.Background(), Options{
		Version: "latest", OutDir: t.TempDir(), IncludePlugins: true,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(plan.Items) == 0 {
		t.Fatal("no items")
	}
	for _, item := range plan.Items {
		if !strings.Contains(item.Note, "dbx-store") {
			t.Errorf("%s: note = %q, want the store to win when it has the artifact", item.RelPath, item.Note)
		}
		if item.SHA256 == "" {
			t.Errorf("%s: store artifacts always carry a digest", item.RelPath)
		}
	}
}

// TestBuildPluginSourceAutoFallsBackToGitHub requests a target the store does
// not advertise and checks that auto resolves it from the plugin's own
// release instead of failing.
func TestBuildPluginSourceAutoFallsBackToGitHub(t *testing.T) {
	f := newFixture(t)
	cfg := testConfig(f)
	cfg.Plugins.Source = config.SourceAuto
	cfg.Plugins.Targets = []string{"darwin-x64"}
	planner := newTestPlanner(t, f, cfg)

	plan, err := planner.Build(context.Background(), Options{
		Version: "latest", OutDir: t.TempDir(), IncludePlugins: true,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(plan.Items) != 2 {
		t.Fatalf("%d items, want 2", len(plan.Items))
	}
	for _, item := range plan.Items {
		if !strings.Contains(item.Note, "github release") {
			t.Errorf("%s: note = %q, want the GitHub fallback", item.RelPath, item.Note)
		}
		if !strings.HasSuffix(item.RelPath, "-darwin-x64.dbxp") {
			t.Errorf("%s: unexpected path", item.RelPath)
		}
		if item.Size <= 0 {
			t.Errorf("%s: expected an asset size from the GitHub API", item.RelPath)
		}
	}
}

func TestBuildPluginOverridesAndLatestPolicy(t *testing.T) {
	f := newFixture(t)
	cfg := testConfig(f)
	planner := newTestPlanner(t, f, cfg)

	plan, err := planner.Build(context.Background(), Options{
		Version:        "latest",
		OutDir:         t.TempDir(),
		IncludePlugins: true,
		PluginVersions: map[string]string{"io.github.t8y2.s3": "0.1.8"},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, p := range plan.Plugins {
		if p.ID == "io.github.t8y2.s3" && p.Version != "0.1.8" {
			t.Errorf("override ignored: s3 version = %s, want 0.1.8", p.Version)
		}
	}

	plan, err = planner.Build(context.Background(), Options{
		Version: "latest", OutDir: t.TempDir(), IncludePlugins: true, PluginLatest: true,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, p := range plan.Plugins {
		if p.ID == "io.github.t8y2.s3" && p.Version != s3Latest {
			t.Errorf("policy latest: s3 version = %s, want %s", p.Version, s3Latest)
		}
	}
}

func TestBuildRestrictedTargetsWarns(t *testing.T) {
	f := newFixture(t)
	cfg := testConfig(f)
	cfg.Plugins.Targets = []string{"windows-arm64"}
	planner := newTestPlanner(t, f, cfg)

	_, err := planner.Build(context.Background(), Options{
		Version: "latest", OutDir: t.TempDir(), IncludePlugins: true,
	})
	if err == nil {
		t.Fatal("expected an error when no configured target exists")
	}
	if !strings.Contains(err.Error(), "available") {
		t.Errorf("error should list published targets: %v", err)
	}
}

// TestRunDownloadsAndArchives exercises the full pipeline offline. It keeps the
// staging directory so the layout and the warm re-run can be inspected; the
// default (delete it) is covered by TestRunRemovesStagingDirByDefault.
func TestRunDownloadsAndArchives(t *testing.T) {
	f := newFixture(t)
	cfg := testConfig(f)
	cfg.Output.KeepDir = true
	outDir := t.TempDir()
	outDir = filepath.Join(outDir, "out")
	log := ui.NewLogger(io.Discard, ui.LevelError, false)

	summary, err := Run(context.Background(), cfg, Options{
		Version: "latest", OutDir: outDir, IncludeRelease: true, IncludePlugins: true,
	}, log)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if summary.Downloaded != len(referencePaths()) {
		t.Errorf("Downloaded = %d, want %d", summary.Downloaded, len(referencePaths()))
	}
	if summary.Archive == nil {
		t.Fatal("expected an archive to be created")
	}
	// Every artifact must have come from the fixture: a passing test must
	// never depend on the real network.
	for _, item := range summary.Plan.Items {
		if !strings.HasPrefix(item.URL, f.server.URL) {
			t.Errorf("%s: URL %q escaped the fixture server", item.RelPath, item.URL)
		}
	}

	// The bundle directory must match the reference layout exactly.
	bundleDir := filepath.Join(outDir, "dbx0.6.14")
	var got []string
	err = filepath.WalkDir(bundleDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, relErr := filepath.Rel(bundleDir, path)
		if relErr != nil {
			return relErr
		}
		got = append(got, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	if want := referencePaths(); !reflect.DeepEqual(got, want) {
		t.Errorf("bundle layout mismatch\n got: %v\nwant: %v", got, want)
	}

	// No .part files may survive a successful run.
	for _, rel := range got {
		if strings.HasSuffix(rel, ".part") {
			t.Errorf("leftover partial file: %s", rel)
		}
	}

	// The archive must contain the bundle directory and its checksum sidecar
	// must be written next to it.
	entries := readArchiveEntries(t, summary.Archive.Path)
	wantPrefix := "dbx0.6.14/"
	for _, e := range entries {
		if !strings.HasPrefix(e, wantPrefix) {
			t.Errorf("archive entry %q does not live under %s", e, wantPrefix)
		}
	}
	if summary.Checksum == "" {
		t.Error("expected a non-empty archive checksum")
	}
	if _, err := os.Stat(summary.Archive.Path + ".sha256"); err != nil {
		t.Errorf("checksum sidecar missing: %v", err)
	}

	// A second run must reuse everything, download nothing, and still produce
	// a byte-identical archive (entries are stamped with the release date).
	summary2, err := Run(context.Background(), cfg, Options{
		Version: "latest", OutDir: outDir, IncludeRelease: true, IncludePlugins: true,
	}, log)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if summary2.Downloaded != 0 {
		t.Errorf("second run downloaded %d files, want 0", summary2.Downloaded)
	}
	if summary2.Skipped != len(referencePaths()) {
		t.Errorf("second run skipped %d files, want %d", summary2.Skipped, len(referencePaths()))
	}
	if summary2.Checksum != summary.Checksum {
		t.Errorf("archive is not reproducible: %s != %s", summary2.Checksum, summary.Checksum)
	}
}

// TestRunRebuildsArchiveDeterministically pins the reproducibility guarantee:
// the same inputs must yield the same bytes on every run.
func TestRunRebuildsArchiveDeterministically(t *testing.T) {
	f := newFixture(t)
	cfg := testConfig(f)
	log := ui.NewLogger(io.Discard, ui.LevelError, false)

	var checksums []string
	for i := 0; i < 3; i++ {
		outDir := t.TempDir()
		summary, err := Run(context.Background(), cfg, Options{
			Version: "latest", OutDir: outDir, IncludeRelease: true, IncludePlugins: true,
		}, log)
		if err != nil {
			t.Fatalf("Run %d: %v", i, err)
		}
		checksums = append(checksums, summary.Checksum)
	}
	for i := 1; i < len(checksums); i++ {
		if checksums[i] != checksums[0] {
			t.Errorf("run %d produced %s, run 0 produced %s", i, checksums[i], checksums[0])
		}
	}
}

func TestRunWithoutArchive(t *testing.T) {
	f := newFixture(t)
	cfg := testConfig(f)
	outDir := t.TempDir()
	log := ui.NewLogger(io.Discard, ui.LevelError, false)

	summary, err := Run(context.Background(), cfg, Options{
		Version: "latest", OutDir: outDir, IncludeRelease: true, NoArchive: true,
	}, log)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if summary.Archive != nil {
		t.Error("no archive was requested")
	}
	if _, err := os.Stat(filepath.Join(outDir, "dbx0.6.14.tar.gz")); !os.IsNotExist(err) {
		t.Error("an archive was created despite NoArchive")
	}
	// keep_dir defaults to false, but deleting the only copy of the download
	// would be destructive, so the guard must keep the directory.
	if _, err := os.Stat(filepath.Join(outDir, "dbx0.6.14")); err != nil {
		t.Errorf("the staging directory must be kept when no archive was written: %v", err)
	}
}

func TestRunDryRunTouchesNothing(t *testing.T) {
	f := newFixture(t)
	cfg := testConfig(f)
	outDir := t.TempDir()
	log := ui.NewLogger(io.Discard, ui.LevelError, false)

	if _, err := Run(context.Background(), cfg, Options{
		Version: "latest", OutDir: outDir, IncludeRelease: true, IncludePlugins: true, DryRun: true,
	}, log); err != nil {
		t.Fatalf("Run: %v", err)
	}
	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("dry run created %d entries: %v", len(entries), entries)
	}
}

// TestRunRemovesStagingDirByDefault pins the default behaviour: after a
// successful archive only the tar file (and its checksum) remain.
func TestRunRemovesStagingDirByDefault(t *testing.T) {
	f := newFixture(t)
	cfg := testConfig(f)
	if cfg.Output.KeepDir {
		t.Fatal("the built-in default must be keep_dir=false")
	}
	outDir := t.TempDir()
	log := ui.NewLogger(io.Discard, ui.LevelError, false)

	summary, err := Run(context.Background(), cfg, Options{
		Version: "latest", OutDir: outDir, IncludeRelease: true, IncludePlugins: true,
	}, log)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "dbx0.6.14")); !os.IsNotExist(err) {
		t.Error("the staging directory should have been removed")
	}

	// Only the archive and its checksum may be left behind.
	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	want := []string{"dbx0.6.14.tar.gz", "dbx0.6.14.tar.gz.sha256"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("output directory contains %v, want %v", names, want)
	}

	// The archive must still be complete and reusable.
	if summary.Archive == nil || summary.Archive.Files != len(referencePaths()) {
		t.Errorf("archive = %+v, want %d files", summary.Archive, len(referencePaths()))
	}
	if _, err := os.Stat(summary.Archive.Path); err != nil {
		t.Errorf("archive missing: %v", err)
	}
}

// TestRunKeepsStagingDirWhenConfigured covers output.keep_dir=true.
func TestRunKeepsStagingDirWhenConfigured(t *testing.T) {
	f := newFixture(t)
	cfg := testConfig(f)
	cfg.Output.KeepDir = true
	outDir := t.TempDir()
	log := ui.NewLogger(io.Discard, ui.LevelError, false)

	if _, err := Run(context.Background(), cfg, Options{
		Version: "latest", OutDir: outDir, IncludeRelease: true,
	}, log); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "dbx0.6.14")); err != nil {
		t.Errorf("staging directory should have been kept: %v", err)
	}
}

// TestRemoveStagingDirGuards checks that a missing or incomplete archive never
// costs the user their download.
func TestRemoveStagingDirGuards(t *testing.T) {
	log := ui.NewLogger(io.Discard, ui.LevelError, false)
	dir := t.TempDir()
	plan := &Plan{DirName: "dbx0.6.14", Dir: dir, Items: []Item{{RelPath: "a"}, {RelPath: "b"}}}
	if err := os.WriteFile(filepath.Join(dir, "a"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// No archive at all: keep the directory rather than deleting the download.
	if err := removeStagingDir(plan, nil, log); err != nil {
		t.Fatalf("nil archive should not be an error: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("the staging directory must survive a nil archive: %v", err)
	}

	archivePath := filepath.Join(t.TempDir(), "out.tar.gz")

	// Archive path that does not exist.
	if err := removeStagingDir(plan, &archive.Result{Path: archivePath, Files: 2}, log); err == nil {
		t.Error("expected an error when the archive is missing")
	} else if _, statErr := os.Stat(dir); statErr != nil {
		t.Errorf("the staging directory must survive a missing archive: %v", statErr)
	}

	// Zero-byte archive.
	if err := os.WriteFile(archivePath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := removeStagingDir(plan, &archive.Result{Path: archivePath, Files: 2}, log); err == nil {
		t.Error("expected an error for an empty archive")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("the staging directory must survive an empty archive: %v", err)
	}

	// Archive holding fewer files than were downloaded.
	if err := os.WriteFile(archivePath, []byte("tar"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := removeStagingDir(plan, &archive.Result{Path: archivePath, Files: 1}, log); err == nil {
		t.Error("expected an error for an incomplete archive")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("the staging directory must survive an incomplete archive: %v", err)
	}

	// A complete archive, but the directory holds a file this run did not
	// download: keep it rather than destroy someone else's data.
	foreign := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(foreign, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := removeStagingDir(plan, &archive.Result{Path: archivePath, Files: 2}, log); err != nil {
		t.Fatalf("removeStagingDir: %v", err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Errorf("a foreign file must block deletion: %v", err)
	}

	// Remove the foreign file again: operating-system metadata alone must not
	// block deletion.
	if err := os.Remove(foreign); err != nil {
		t.Fatal(err)
	}
	for _, junk := range []string{".DS_Store", filepath.Join("plugins", "Thumbs.db")} {
		full := filepath.Join(dir, junk)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("junk"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// A complete archive and no foreign files: now the directory goes away.
	if err := removeStagingDir(plan, &archive.Result{Path: archivePath, Files: 2}, log); err != nil {
		t.Fatalf("removeStagingDir: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("the staging directory should have been removed")
	}
}

// TestRunKeepsStagingDirWithExtraFile checks the guard through the public entry
// point: an unexpected file in the bundle directory must survive a run.
func TestRunKeepsStagingDirWithExtraFile(t *testing.T) {
	f := newFixture(t)
	cfg := testConfig(f)
	outDir := t.TempDir()
	bundleDir := filepath.Join(outDir, "dbx0.6.14")
	if err := os.MkdirAll(filepath.Join(bundleDir, "plugins"), 0o755); err != nil {
		t.Fatal(err)
	}
	note := filepath.Join(bundleDir, "notes.txt")
	if err := os.WriteFile(note, []byte("hand written"), 0o644); err != nil {
		t.Fatal(err)
	}
	log := ui.NewLogger(io.Discard, ui.LevelError, false)

	if _, err := Run(context.Background(), cfg, Options{
		Version: "latest", OutDir: outDir, IncludeRelease: true, IncludePlugins: true,
	}, log); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(note); err != nil {
		t.Errorf("the unexpected file must survive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "dbx0.6.14.tar.gz")); err != nil {
		t.Errorf("the archive should still be produced: %v", err)
	}
}

// TestRunRemovesStagingDirIgnoringMetadata checks that a stray .DS_Store left by
// Finder does not prevent the cleanup.
func TestRunRemovesStagingDirIgnoringMetadata(t *testing.T) {
	f := newFixture(t)
	cfg := testConfig(f)
	outDir := t.TempDir()
	bundleDir := filepath.Join(outDir, "dbx0.6.14")
	if err := os.MkdirAll(filepath.Join(bundleDir, "plugins"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, ".DS_Store"), []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	log := ui.NewLogger(io.Discard, ui.LevelError, false)

	if _, err := Run(context.Background(), cfg, Options{
		Version: "latest", OutDir: outDir, IncludeRelease: true, IncludePlugins: true,
	}, log); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(bundleDir); !os.IsNotExist(err) {
		t.Error("metadata files must not block the cleanup")
	}
}

func TestRunPropagatesDownloadFailure(t *testing.T) {
	f := newFixture(t)
	// Withdraw one asset from the fake CDN; its release entry still exists, so
	// planning succeeds and only the transfer fails.
	delete(f.bodies, "DBX_0.6.14_arm64.dmg")
	cfg := testConfig(f)
	cfg.Assets = []config.Asset{{Selector: "DBX_{version}_arm64.dmg"}}
	log := ui.NewLogger(io.Discard, ui.LevelError, false)

	outDir := t.TempDir()
	_, err := Run(context.Background(), cfg, Options{
		Version: "latest", OutDir: outDir, IncludeRelease: true,
	}, log)
	if err == nil {
		t.Fatal("expected Run to fail when an asset cannot be downloaded")
	}
	if !strings.Contains(err.Error(), "HTTP 404") {
		t.Errorf("unexpected error: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(outDir, "dbx0.6.14.tar.gz")); !os.IsNotExist(statErr) {
		t.Error("a partial bundle must not be archived")
	}
}

func TestEnsureDirNameIsSafe(t *testing.T) {
	for _, ok := range []string{"dbx0.6.14", "dbx-0.6.14_bundle"} {
		if err := EnsureDirNameIsSafe(ok); err != nil {
			t.Errorf("EnsureDirNameIsSafe(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", ".", "..", "a/b", `a\b`, "/abs"} {
		if err := EnsureDirNameIsSafe(bad); err == nil {
			t.Errorf("EnsureDirNameIsSafe(%q) = nil, want an error", bad)
		}
	}
}

func TestTotalSizeAndRequests(t *testing.T) {
	plan := &Plan{
		Dir: "/tmp/dbx0.6.14",
		Items: []Item{
			{RelPath: "a.bin", Size: 10},
			{RelPath: "plugins/s3/b.dbxp", Size: -1},
			{RelPath: "plugins/s3/c.dbxp", Size: 5},
		},
	}
	if got := plan.TotalSize(); got != 15 {
		t.Errorf("TotalSize = %d, want 15", got)
	}
	reqs := plan.Requests()
	if len(reqs) != 3 {
		t.Fatalf("Requests = %d, want 3", len(reqs))
	}
	want := filepath.Join("/tmp/dbx0.6.14", "plugins", "s3", "b.dbxp")
	if reqs[1].Dest != want {
		t.Errorf("Dest = %q, want %q", reqs[1].Dest, want)
	}
}

func TestArchiveNameHonoursFormat(t *testing.T) {
	cfg := config.Default()
	v, err := naming.ParseVersion("1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	if got := archiveName(cfg, v, config.FormatTar); got != "dbx1.2.3.tar" {
		t.Errorf("archiveName = %q, want dbx1.2.3.tar", got)
	}
	if got := archiveName(cfg, v, config.FormatTarGz); got != "dbx1.2.3.tar.gz" {
		t.Errorf("archiveName = %q, want dbx1.2.3.tar.gz", got)
	}
}

// readArchiveEntries lists the names inside a .tar.gz.
func readArchiveEntries(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	var names []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, hdr.Name)
	}
	return names
}

// rateLimitedHandler answers every GitHub API request with a quota refusal.
func rateLimitedHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-RateLimit-Remaining", "0")
	w.Header().Set("X-RateLimit-Reset", "1800000000")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(`{"message":"API rate limit exceeded for 1.2.3.4."}`))
}

// TestResolveVersionDegradesOnRateLimit covers the quota-exhausted path: the
// version comes from the Atom feed and the asset URLs are derived from the
// configured selectors, so an unauthenticated CI run still works.
func TestResolveVersionDegradesOnRateLimit(t *testing.T) {
	f := newFixture(t)
	mux := http.NewServeMux()
	// Atom feed plus artifact payloads stay available; only the API is limited.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/repos/") {
			rateLimitedHandler(w, r)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/releases.atom") {
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <entry><link rel="alternate" href="https://github.com/t8y2/dbx/releases/tag/packages-v0.4.88"/><title>packages v0.4.88</title></entry>
  <entry><link rel="alternate" href="https://github.com/t8y2/dbx/releases/tag/v0.6.14"/><title>DBX v0.6.14</title></entry>
</feed>`))
			return
		}
		f.handle(w, r)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := config.Default()
	cfg.GitHub.APIBase = srv.URL
	cfg.GitHub.WebBase = srv.URL
	cfg.Store.RawBase = srv.URL
	cfg.Plugins.Enabled = false
	cfg.Output.Concurrency = 2
	planner := NewPlanner(cfg, ui.NewLogger(io.Discard, ui.LevelError, false))

	plan, err := planner.Build(context.Background(), Options{
		Version: "latest", OutDir: t.TempDir(), IncludeRelease: true,
	})
	if err != nil {
		t.Fatalf("Build should degrade instead of failing: %v", err)
	}
	if !plan.Degraded {
		t.Error("plan should be flagged as degraded")
	}
	if plan.DegradedReason == nil {
		t.Error("the underlying API error should be retained")
	}
	if plan.Version.String() != "0.6.14" {
		t.Errorf("version = %s, want 0.6.14 from the Atom feed", plan.Version)
	}
	if len(plan.Items) != 5 {
		t.Fatalf("%d items, want the 5 configured packages", len(plan.Items))
	}
	for _, item := range plan.Items {
		want := "https://github.com/t8y2/dbx/releases/download/v0.6.14/" + item.RelPath
		if item.URL != want {
			t.Errorf("%s: URL = %q, want %q", item.RelPath, item.URL, want)
		}
		if item.Size != -1 {
			t.Errorf("%s: size = %d, want -1 (unknown without the API)", item.RelPath, item.Size)
		}
	}
}

// TestResolveVersionDegradationRejectsWildcards makes sure the degraded mode
// refuses to guess when a selector cannot be expanded.
func TestResolveVersionDegradationRejectsWildcards(t *testing.T) {
	f := newFixture(t)
	_ = f
	srv := httptest.NewServer(http.HandlerFunc(rateLimitedHandler))
	defer srv.Close()

	cfg := config.Default()
	cfg.GitHub.APIBase = srv.URL
	cfg.GitHub.WebBase = srv.URL
	cfg.Plugins.Enabled = false
	cfg.Assets = []config.Asset{{Selector: "DBX_{version}_*.dmg"}}
	planner := NewPlanner(cfg, ui.NewLogger(io.Discard, ui.LevelError, false))

	_, err := planner.Build(context.Background(), Options{
		Version: "0.6.14", OutDir: t.TempDir(), IncludeRelease: true,
	})
	if err == nil {
		t.Fatal("expected an error for an unexpandable wildcard selector")
	}
	if !strings.Contains(err.Error(), "wildcard") {
		t.Errorf("error = %v, want it to mention the wildcard", err)
	}
}

// TestPluginItemsUseConfiguredDir verifies the bundle layout still comes from
// plugins.items[].dir, and that clearing it switches to the plugin id.
func TestPluginItemsUseConfiguredDir(t *testing.T) {
	f := newFixture(t)
	cfg := testConfig(f)

	planner := newTestPlanner(t, f, cfg)
	plan, err := planner.Build(context.Background(), Options{
		Version: "latest", OutDir: t.TempDir(), IncludePlugins: true,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, item := range plan.Items {
		if !strings.HasPrefix(item.RelPath, "plugins/s3/") && !strings.HasPrefix(item.RelPath, "plugins/ssh/") {
			t.Errorf("%s: unexpected plugin directory", item.RelPath)
		}
	}

	// With dir unset the plugin id becomes the directory name.
	cfg.Plugins.Items[0].Dir = ""
	planner = newTestPlanner(t, f, cfg)
	plan, err = planner.Build(context.Background(), Options{
		Version: "latest", OutDir: t.TempDir(), IncludePlugins: true,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	found := false
	for _, item := range plan.Items {
		if strings.HasPrefix(item.RelPath, "plugins/io.github.t8y2.s3/") {
			found = true
		}
	}
	if !found {
		t.Error("an unset dir must fall back to the plugin id as the directory name")
	}
}

// TestSummarySeparatesTransferredFromCachedBytes checks that a fully cached
// re-run reports no transferred bytes, which is what users read to decide
// whether the tool actually hit the network.
func TestSummarySeparatesTransferredFromCachedBytes(t *testing.T) {
	f := newFixture(t)
	cfg := testConfig(f)
	cfg.Output.KeepDir = true
	outDir := t.TempDir()
	log := ui.NewLogger(io.Discard, ui.LevelError, false)

	first, err := Run(context.Background(), cfg, Options{
		Version: "latest", OutDir: outDir, IncludeRelease: true, IncludePlugins: true,
	}, log)
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if first.Bytes <= 0 {
		t.Error("the first run transferred bytes")
	}
	if first.CachedBytes != 0 {
		t.Errorf("CachedBytes = %d on a cold run, want 0", first.CachedBytes)
	}

	second, err := Run(context.Background(), cfg, Options{
		Version: "latest", OutDir: outDir, IncludeRelease: true, IncludePlugins: true,
	}, log)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if second.Bytes != 0 {
		t.Errorf("Bytes = %d on a warm run, want 0", second.Bytes)
	}
	if second.CachedBytes != first.Bytes {
		t.Errorf("CachedBytes = %d, want the %d bytes written by the first run",
			second.CachedBytes, first.Bytes)
	}
}
