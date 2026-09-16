package naming

import "testing"

func TestParseVersion(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantTag string
		wantErr bool
	}{
		{in: "0.6.14", want: "0.6.14", wantTag: "v0.6.14"},
		{in: "v0.6.14", want: "0.6.14", wantTag: "v0.6.14"},
		{in: "V1.2.3", want: "1.2.3", wantTag: "v1.2.3"},
		{in: " 0.6.9 ", want: "0.6.9", wantTag: "v0.6.9"},
		{in: "0.7.0-rc.1", want: "0.7.0-rc.1", wantTag: "v0.7.0-rc.1"},
		{in: "1.0", wantErr: true},
		{in: "latest", wantErr: true},
		{in: "", wantErr: true},
	}
	for _, tc := range cases {
		got, err := ParseVersion(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseVersion(%q): expected error, got %v", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseVersion(%q): unexpected error: %v", tc.in, err)
			continue
		}
		if got.String() != tc.want {
			t.Errorf("ParseVersion(%q).String() = %q, want %q", tc.in, got.String(), tc.want)
		}
		if got.Tag() != tc.wantTag {
			t.Errorf("ParseVersion(%q).Tag() = %q, want %q", tc.in, got.Tag(), tc.wantTag)
		}
	}
}

func TestCompare(t *testing.T) {
	mustParse := func(s string) Version {
		v, err := ParseVersion(s)
		if err != nil {
			t.Fatalf("ParseVersion(%q): %v", s, err)
		}
		return v
	}
	cases := []struct {
		a, b string
		want int
	}{
		{"0.6.14", "0.6.13", 1},
		{"0.6.13", "0.6.14", -1},
		{"0.6.14", "0.6.14", 0},
		{"0.7.0", "0.6.99", 1},
		{"1.0.0", "0.99.99", 1},
		{"0.7.0-rc.1", "0.7.0", -1},
		{"0.7.0", "0.7.0-rc.1", 1},
	}
	for _, tc := range cases {
		if got := Compare(mustParse(tc.a), mustParse(tc.b)); got != tc.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestExtractVersionFiltersForeignTagFamilies(t *testing.T) {
	// The dbx repository publishes three unrelated tag families in the same
	// release feed; only the application tags must be accepted.
	cases := []struct {
		tag     string
		want    string
		pattern string
		ok      bool
	}{
		{tag: "v0.6.14", want: "0.6.14", ok: true},
		{tag: "v0.6.9", want: "0.6.9", ok: true},
		// Pre-releases parse successfully; include_prerelease decides whether
		// they are eligible for "latest".
		{tag: "v0.7.0-rc.1", want: "0.7.0-rc.1", ok: true},
		{tag: "packages-v0.4.88", ok: false},
		{tag: "agents-v0.2.111", ok: false},
		{tag: "agents-latest", ok: false},
		// Build metadata is not part of the default pattern, so it is rejected.
		{tag: "v0.6.14+build", ok: false},
		{tag: "0.6.14", ok: false},
		// A pattern that captures the full version tolerates the suffix.
		{tag: "v0.6.14+build", want: "0.6.14-build", pattern: `^v(.+)$`, ok: true},
		// A custom pattern without a capture group falls back to cleaning the
		// tag: it only works when the tag is (essentially) the version.
		{tag: "v0.9.1", want: "0.9.1", pattern: `^v.*$`, ok: true},
		{tag: "release-0.9.1", ok: false, pattern: `^release-.*$`},
		// Capturing the version is the way to accept a prefixed tag.
		{tag: "release-0.9.1", want: "0.9.1", pattern: `^release-(\d+\.\d+\.\d+)$`, ok: true},
		// A custom pattern can select a different tag family instead.
		{tag: "packages-v0.4.88", want: "0.4.88", pattern: `^packages-v(\d+\.\d+\.\d+)$`, ok: true},
	}
	for _, tc := range cases {
		got, ok := ExtractVersion(tc.tag, tc.pattern)
		if ok != tc.ok {
			t.Errorf("ExtractVersion(%q, %q) ok = %v, want %v", tc.tag, tc.pattern, ok, tc.ok)
			continue
		}
		if ok && got.String() != tc.want {
			t.Errorf("ExtractVersion(%q, %q) = %q, want %q", tc.tag, tc.pattern, got.String(), tc.want)
		}
	}
}

func TestSanitizeName(t *testing.T) {
	cases := map[string]string{
		"io.github.t8y2.s3": "io.github.t8y2.s3",
		"io.dbx.ssh":        "io.dbx.ssh",
		"s3":                "s3",
		"io.dbx.bad name":   "io.dbx.bad-name",
		"io.dbx.ssh/../x":   "io.dbx.ssh-..-x",
		"../escape":         "escape",
	}
	for in, want := range cases {
		if got := SanitizeName(in); got != want {
			t.Errorf("SanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBaseName(t *testing.T) {
	cases := map[string]string{
		"https://dl.dbxio.com/plugins/io.dbx.ssh/0.4.76/io.dbx.ssh-0.4.76-linux-x64.dbxp": "io.dbx.ssh-0.4.76-linux-x64.dbxp",
		"https://example.com/a/b/file.dbxp?token=1":                                       "file.dbxp",
		"https://example.com/a/b/file.dbxp#frag":                                          "file.dbxp",
		"https://example.com/":                                                            "example.com",
	}
	for in, want := range cases {
		if got := BaseName(in); got != want {
			t.Errorf("BaseName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSafeRelPath(t *testing.T) {
	if _, err := SafeRelPath("plugins/s3/a.dbxp"); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	for _, bad := range []string{"", "/etc/passwd", "../escape", "a/../../b"} {
		if _, err := SafeRelPath(bad); err == nil {
			t.Errorf("SafeRelPath(%q): expected error", bad)
		}
	}
}

func TestExpandAndDirName(t *testing.T) {
	v, err := ParseVersion("v0.6.14")
	if err != nil {
		t.Fatal(err)
	}
	if got := DirName("dbx{version}", v); got != "dbx0.6.14" {
		t.Errorf("DirName = %q, want dbx0.6.14", got)
	}
	if got := Expand("DBX_{version}_arm64.dmg", AssetVars(v)); got != "DBX_0.6.14_arm64.dmg" {
		t.Errorf("Expand = %q", got)
	}
}
