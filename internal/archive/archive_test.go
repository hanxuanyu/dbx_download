package archive

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"dbxdl/internal/config"
)

// referenceLayout mirrors the directory structure the user prepared by hand,
// which the archive must reproduce exactly.
var referenceLayout = map[string]string{
	"DBX_0.6.14_arm64.dmg":                                "dmg",
	"DBX_0.6.14_x64-setup.exe":                            "exe",
	"DBX_0.6.14_arm64-browser-static.tar.gz":              "arm64-browser",
	"DBX_0.6.14_x64-portable.zip":                         "portable",
	"DBX_0.6.14_x64-browser-static.tar.gz":                "x64-browser",
	"plugins/s3/io.github.t8y2.s3-0.1.9-linux-x64.dbxp":   "s3-linux-x64",
	"plugins/s3/io.github.t8y2.s3-0.1.9-linux-arm64.dbxp": "s3-linux-arm64",
	"plugins/s3/io.github.t8y2.s3-0.1.9-windows-x64.dbxp": "s3-win",
	"plugins/ssh/io.dbx.ssh-0.4.76-linux-x64.dbxp":        "ssh-linux-x64",
	"plugins/ssh/io.dbx.ssh-0.4.76-linux-arm64.dbxp":      "ssh-linux-arm64",
	"plugins/ssh/io.dbx.ssh-0.4.76-windows-x64.dbxp":      "ssh-win",
}

func buildReferenceDir(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "dbx0.6.14")
	for rel, body := range referenceLayout {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

type tarEntry struct {
	name  string
	isDir bool
	body  string
}

func readArchive(t *testing.T, path string) []tarEntry {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	var reader io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = gz.Close() }()
		reader = gz
	}

	tr := tar.NewReader(reader)
	var entries []tarEntry
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read tar: %v", err)
		}
		entry := tarEntry{name: hdr.Name, isDir: hdr.Typeflag == tar.TypeDir}
		if hdr.Typeflag == tar.TypeReg {
			buf := make([]byte, hdr.Size)
			if _, err := io.ReadFull(tr, buf); err != nil {
				t.Fatalf("read %s: %v", hdr.Name, err)
			}
			entry.body = string(buf)
		}
		entries = append(entries, entry)
	}
	return entries
}

func TestWritePreservesReferenceStructure(t *testing.T) {
	root := buildReferenceDir(t)
	dest := filepath.Join(filepath.Dir(root), "dbx0.6.14.tar.gz")

	res, err := Write(Options{
		SourceDir: root,
		BaseName:  "dbx0.6.14",
		DestPath:  dest,
		Format:    config.FormatTarGz,
		GzipLevel: 1,
		ModTime:   time.Unix(1700000000, 0),
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if res.Files != len(referenceLayout) {
		t.Errorf("Files = %d, want %d", res.Files, len(referenceLayout))
	}

	entries := readArchive(t, dest)

	var gotFiles []string
	contents := map[string]string{}
	dirs := map[string]bool{}
	for _, e := range entries {
		if e.isDir {
			dirs[strings.TrimSuffix(e.name, "/")] = true
			continue
		}
		gotFiles = append(gotFiles, e.name)
		contents[e.name] = e.body
	}

	var wantFiles []string
	for rel := range referenceLayout {
		wantFiles = append(wantFiles, "dbx0.6.14/"+rel)
	}
	sort.Strings(wantFiles)
	sort.Strings(gotFiles)
	if !reflect.DeepEqual(gotFiles, wantFiles) {
		t.Errorf("archive entries mismatch\n got: %v\nwant: %v", gotFiles, wantFiles)
	}

	// Parent directory entries must be present so extraction preserves the
	// tree even for empty folders.
	for _, dir := range []string{"dbx0.6.14/plugins", "dbx0.6.14/plugins/s3", "dbx0.6.14/plugins/ssh"} {
		if !dirs[dir] {
			t.Errorf("missing directory entry %q (have %v)", dir, dirs)
		}
	}

	for rel, body := range referenceLayout {
		got, ok := contents["dbx0.6.14/"+rel]
		if !ok {
			t.Errorf("missing content for %s", rel)
			continue
		}
		if got != body {
			t.Errorf("%s: content = %q, want %q", rel, got, body)
		}
	}
}

// TestExtractRoundTrip proves the archive recreates the bundle directory.
func TestExtractRoundTrip(t *testing.T) {
	root := buildReferenceDir(t)
	dest := filepath.Join(t.TempDir(), "bundle.tar.gz")
	if _, err := Write(Options{SourceDir: root, BaseName: "dbx0.6.14", DestPath: dest, Format: config.FormatTarGz, GzipLevel: 1}); err != nil {
		t.Fatal(err)
	}

	target := t.TempDir()
	for _, e := range readArchive(t, dest) {
		full := filepath.Join(target, filepath.FromSlash(e.name))
		if e.isDir {
			if err := os.MkdirAll(full, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(e.body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	for rel, body := range referenceLayout {
		full := filepath.Join(target, "dbx0.6.14", filepath.FromSlash(rel))
		got, err := os.ReadFile(full)
		if err != nil {
			t.Errorf("extracted file missing: %v", err)
			continue
		}
		if string(got) != body {
			t.Errorf("%s: extracted %q, want %q", rel, got, body)
		}
	}
}

func TestWriteSkipsJunkFiles(t *testing.T) {
	root := buildReferenceDir(t)
	junk := []string{".DS_Store", "plugins/.DS_Store", "plugins/s3/._sidecar", "Thumbs.db", "half.dbxp.part"}
	for _, rel := range junk {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("junk"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	dest := filepath.Join(t.TempDir(), "out.tar.gz")
	if _, err := Write(Options{SourceDir: root, DestPath: dest, Format: config.FormatTarGz, GzipLevel: 1}); err != nil {
		t.Fatal(err)
	}
	for _, e := range readArchive(t, dest) {
		switch filepath.Base(e.name) {
		case ".DS_Store", "Thumbs.db", "._sidecar", "half.dbxp.part":
			t.Errorf("junk entry %q must not be archived", e.name)
		}
	}
}

func TestWritePlainTar(t *testing.T) {
	root := buildReferenceDir(t)
	dest := filepath.Join(t.TempDir(), "out.tar")
	res, err := Write(Options{SourceDir: root, DestPath: dest, Format: config.FormatTar})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(readArchive(t, dest)); got == 0 {
		t.Error("plain tar contains no entries")
	}
	if res.Size <= 0 {
		t.Error("expected a positive archive size")
	}
	if res.SHA256 == "" || len(res.SHA256) != 64 {
		t.Errorf("SHA256 = %q, want a 64-character digest", res.SHA256)
	}
}

func TestWriteIsDeterministic(t *testing.T) {
	root := buildReferenceDir(t)
	stamp := time.Unix(1700000000, 0)
	dir := t.TempDir()
	first := filepath.Join(dir, "a.tar.gz")
	second := filepath.Join(dir, "b.tar.gz")
	for _, dest := range []string{first, second} {
		if _, err := Write(Options{
			SourceDir: root, BaseName: "dbx0.6.14", DestPath: dest,
			Format: config.FormatTarGz, GzipLevel: 1, ModTime: stamp,
		}); err != nil {
			t.Fatal(err)
		}
	}
	a, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(second)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) == 0 || !reflect.DeepEqual(a, b) {
		t.Errorf("archives differ: %d vs %d bytes", len(a), len(b))
	}
}

func TestWriteRejectsEmptySource(t *testing.T) {
	empty := t.TempDir()
	if _, err := Write(Options{SourceDir: empty, DestPath: filepath.Join(t.TempDir(), "x.tar.gz")}); err == nil {
		t.Fatal("expected an error for an empty bundle directory")
	}
}

func TestWriteSHA256Sidecar(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "dbx0.6.14.tar.gz")
	if err := os.WriteFile(archivePath, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	path, err := WriteSHA256Sidecar(archivePath, "abc123")
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "abc123  dbx0.6.14.tar.gz\n"
	if string(body) != want {
		t.Errorf("sidecar = %q, want %q", string(body), want)
	}
}
