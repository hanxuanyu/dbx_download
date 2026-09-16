package store

import (
	"testing"
)

func fixturePlugin() *Plugin {
	return &Plugin{
		ID:            "io.dbx.ssh",
		Name:          "SSH Terminal",
		LatestVersion: "0.4.76",
		Homepage:      "https://github.com/jinpy666/dbx-plugin-ssh",
		Source:        "https://github.com/jinpy666/dbx-plugin-ssh/tree/ssh-v0.4.76",
		Versions: []Version{
			{Version: "0.4.72", Artifacts: []Artifact{{Target: "linux-x64", URL: "u72"}}},
			{Version: "0.4.73", Artifacts: []Artifact{{Target: "linux-x64", URL: "u73"}}},
			{Version: "0.4.75", Artifacts: []Artifact{{Target: "linux-x64", URL: "u75"}}},
			{Version: "0.4.76", Artifacts: []Artifact{
				{Target: "linux-x64", URL: "https://dl.dbxio.com/plugins/io.dbx.ssh/0.4.76/io.dbx.ssh-0.4.76-linux-x64.dbxp", SHA256: "b2a6", Size: 6252221},
				{Target: "linux-arm64", URL: "arm", SHA256: "51fe", Size: 6118066},
				{Target: "windows-x64", URL: "win", SHA256: "87b3", Size: 6493626},
				{Target: "darwin-x64", URL: "mac", SHA256: "mac", Size: 6366235},
			}},
		},
	}
}

func TestResolveVersionLatest(t *testing.T) {
	p := fixturePlugin()
	for _, requested := range []string{"", "latest", "LATEST"} {
		got, err := ResolveVersion(p, requested, "pinned")
		if err != nil {
			t.Fatalf("ResolveVersion(%q): %v", requested, err)
		}
		if got.Version != "0.4.76" {
			t.Errorf("ResolveVersion(%q) = %s, want 0.4.76", requested, got.Version)
		}
	}
}

func TestResolveVersionExact(t *testing.T) {
	p := fixturePlugin()
	got, err := ResolveVersion(p, "0.4.75", "pinned")
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != "0.4.75" {
		t.Errorf("got %s, want 0.4.75", got.Version)
	}
}

func TestResolveVersionPrefix(t *testing.T) {
	p := fixturePlugin()
	for _, requested := range []string{"0.4.x", "0.4.*"} {
		got, err := ResolveVersion(p, requested, "pinned")
		if err != nil {
			t.Fatalf("ResolveVersion(%q): %v", requested, err)
		}
		if got.Version != "0.4.76" {
			t.Errorf("ResolveVersion(%q) = %s, want 0.4.76", requested, got.Version)
		}
	}
}

func TestResolveVersionMissing(t *testing.T) {
	p := fixturePlugin()
	if _, err := ResolveVersion(p, "9.9.9", "pinned"); err == nil {
		t.Fatal("expected an error for an unknown version")
	}
}

func TestResolveVersionPolicyLatestOverridesPin(t *testing.T) {
	p := fixturePlugin()
	got, err := ResolveVersion(p, "0.4.72", "latest")
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != "0.4.76" {
		t.Errorf("policy latest should ignore the pin, got %s", got.Version)
	}
}

// TestResolveVersionFallsBackToHighest covers a store document whose
// latestVersion lags behind the published version list.
func TestResolveVersionFallsBackToHighest(t *testing.T) {
	p := fixturePlugin()
	p.LatestVersion = "0.4.72"
	got, err := ResolveVersion(p, "", "pinned")
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != "0.4.72" {
		t.Errorf("explicit latestVersion wins, got %s", got.Version)
	}

	p.LatestVersion = "does-not-exist"
	got, err = ResolveVersion(p, "", "pinned")
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != "0.4.76" {
		t.Errorf("expected the highest published version, got %s", got.Version)
	}
}

func TestVersionHelpers(t *testing.T) {
	p := fixturePlugin()
	versions := p.VersionStrings()
	if len(versions) != 4 || versions[0] != "0.4.76" || versions[3] != "0.4.72" {
		t.Errorf("VersionStrings() = %v, want newest first", versions)
	}

	v := p.Versions[3]
	if _, ok := v.ArtifactFor("linux-x64"); !ok {
		t.Error("expected a linux-x64 artifact")
	}
	if _, ok := v.ArtifactFor("windows-arm64"); ok {
		t.Error("did not expect a windows-arm64 artifact")
	}
	if got := len(v.Targets()); got != 4 {
		t.Errorf("Targets() = %d, want 4", got)
	}
}

func TestGitHubRepo(t *testing.T) {
	p := fixturePlugin()
	if repo, ok := GitHubRepo(p); !ok || repo != "jinpy666/dbx-plugin-ssh" {
		t.Errorf("GitHubRepo = (%q, %v)", repo, ok)
	}

	// homepage alone is enough.
	p.Source = ""
	if repo, ok := GitHubRepo(p); !ok || repo != "jinpy666/dbx-plugin-ssh" {
		t.Errorf("GitHubRepo from homepage = (%q, %v)", repo, ok)
	}

	// a trailing .git suffix (or "tree/..." path) must not leak into the repo.
	p.Homepage = ""
	p.Source = "https://github.com/t8y2/dbx-plugin-s3.git/tree/v0.1.9"
	if repo, ok := GitHubRepo(p); !ok || repo != "t8y2/dbx-plugin-s3" {
		t.Errorf("GitHubRepo with .git = (%q, %v)", repo, ok)
	}

	p.Homepage = "https://example.com/plugin"
	p.Source = ""
	if _, ok := GitHubRepo(p); ok {
		t.Error("expected no GitHub repository for a non-GitHub homepage")
	}
}

// TestGitHubTagHintRejectsStaleTags is the regression test for the store's
// stale source field: io.github.t8y2.s3 advertised tree/v0.1.3 while its
// latestVersion was already 0.1.9.
func TestGitHubTagHintRejectsStaleTags(t *testing.T) {
	p := fixturePlugin()
	if tag, ok := GitHubTagHint(p, "0.4.76"); !ok || tag != "ssh-v0.4.76" {
		t.Errorf("GitHubTagHint = (%q, %v), want the current tag", tag, ok)
	}
	if tag, ok := GitHubTagHint(p, "0.4.72"); ok {
		t.Errorf("a hint for a different version must be rejected, got %q", tag)
	}

	p.Source = "https://github.com/t8y2/dbx-plugin-s3/tree/v0.1.3"
	if tag, ok := GitHubTagHint(p, "0.1.9"); ok {
		t.Errorf("stale hint v0.1.3 must not be used for 0.1.9, got %q", tag)
	}

	p.Source = "https://github.com/t8y2/dbx-plugin-s3"
	if _, ok := GitHubTagHint(p, "0.1.9"); ok {
		t.Error("expected no tag hint without a /tree/ segment")
	}
}

func TestGitHubAssetName(t *testing.T) {
	p := fixturePlugin()
	if got, want := GitHubAssetName(p, "0.4.76", "linux-x64"), "io.dbx.ssh-0.4.76-linux-x64.dbxp"; got != want {
		t.Errorf("GitHubAssetName = %q, want %q", got, want)
	}
}

func TestRevoked(t *testing.T) {
	r := &Revoked{
		PluginVersions: []RevokedPluginVersion{
			{ID: "io.dbx.bad", Version: "1.0.0", Reason: "malware"},
		},
	}

	reason, bad := r.IsRevoked("io.dbx.bad", "1.0.0")
	if !bad {
		t.Error("expected the version to be reported as revoked")
	}
	if reason != "malware" {
		t.Errorf("reason = %q, want malware", reason)
	}
	if _, bad := r.IsRevoked("io.dbx.bad", "1.0.1"); bad {
		t.Error("did not expect an unrelated version to be revoked")
	}
	if _, bad := r.IsRevoked("io.dbx.other", "1.0.0"); bad {
		t.Error("did not expect an unrelated plugin to be revoked")
	}
	var nilRevoked *Revoked
	if _, bad := nilRevoked.IsRevoked("io.dbx.bad", "1.0.0"); bad {
		t.Error("a nil revocation list must be safe to query")
	}
}

func TestURLs(t *testing.T) {
	c := New(Options{Owner: "t8y2", Repo: "dbx-store", Ref: "main"})
	if got, want := c.PluginURL("io.dbx.ssh"), "https://raw.githubusercontent.com/t8y2/dbx-store/main/plugins/io.dbx.ssh.json"; got != want {
		t.Errorf("PluginURL = %q, want %q", got, want)
	}
	if got, want := c.CatalogURL(), "https://raw.githubusercontent.com/t8y2/dbx-store/main/catalog/index.json"; got != want {
		t.Errorf("CatalogURL = %q, want %q", got, want)
	}
}
