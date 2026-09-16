// Package archive packages a downloaded bundle directory into a single tar
// (optionally gzip-compressed) file whose entries are prefixed with the bundle
// directory name, so extracting it reproduces the original layout:
//
//	dbx0.6.14/
//	  DBX_0.6.14_arm64.dmg
//	  ...
//	  plugins/s3/io.github.t8y2.s3-0.1.9-linux-x64.dbxp
package archive

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"dbxdl/internal/config"
	"dbxdl/internal/ui"
)

// Options configures a single archive run.
type Options struct {
	// SourceDir is the bundle directory to package.
	SourceDir string
	// BaseName is the prefix used inside the archive, normally the bundle
	// directory's base name ("dbx0.6.14").
	BaseName string
	// DestPath is the archive to create. A temporary ".part" file is used and
	// renamed on success.
	DestPath string
	Format   string
	// GzipLevel is 1..9 and only applies to "tar.gz".
	GzipLevel int
	// ModTime is stamped on every entry so repeated builds of identical input
	// produce identical archives. Defaults to time.Now().
	ModTime time.Time
	Log     *ui.Logger
}

// Result describes the created archive.
type Result struct {
	Path   string
	Size   int64
	SHA256 string
	// Files is the number of regular files written (directory entries are not
	// counted).
	Files int
	// Bytes is the uncompressed payload size of those files.
	Bytes int64
}

// junkNames are filesystem metadata that must never end up in a distributable
// bundle. The user's reference layout contains none of them.
var junkNames = map[string]bool{
	".DS_Store":        true,
	".AppleDouble":     true,
	".LSOverride":      true,
	"Thumbs.db":        true,
	"ehthumbs.db":      true,
	"desktop.ini":      true,
	".Spotlight-V100":  true,
	".Trashes":         true,
	".fseventsd":       true,
	".VolumeIcon.icns": true,
	"Icon\r":           true,
}

func isJunk(name string) bool { return IsMetadataFile(name) }

// IsMetadataFile reports whether a file name is operating-system metadata that
// must never end up in a distributable bundle ("​.DS_Store", "Thumbs.db", "._*").
func IsMetadataFile(name string) bool {
	if junkNames[name] {
		return true
	}
	// macOS AppleDouble sidecar files ("." + "_" + name).
	return strings.HasPrefix(name, "._")
}

// Write creates the archive and returns its digest.
func Write(opts Options) (Result, error) {
	if opts.Format == "" {
		opts.Format = config.FormatTarGz
	}
	if opts.Format != config.FormatTarGz && opts.Format != config.FormatTar {
		return Result{}, fmt.Errorf("unsupported archive format %q", opts.Format)
	}
	if opts.BaseName == "" {
		opts.BaseName = filepath.Base(opts.SourceDir)
	}
	if opts.ModTime.IsZero() {
		opts.ModTime = time.Now()
	}

	files, err := collect(opts.SourceDir)
	if err != nil {
		return Result{}, err
	}
	if len(files) == 0 {
		return Result{}, fmt.Errorf("nothing to archive: %s is empty", opts.SourceDir)
	}

	part := opts.DestPath + ".part"
	out, err := os.Create(part)
	if err != nil {
		return Result{}, fmt.Errorf("create archive: %w", err)
	}

	hasher := sha256.New()
	counting := &countingWriter{w: io.MultiWriter(out, hasher)}

	var tarSink io.Writer = counting
	var gz *gzip.Writer
	if opts.Format == config.FormatTarGz {
		level := opts.GzipLevel
		if level < 1 || level > 9 {
			level = gzip.BestSpeed
		}
		gz, err = gzip.NewWriterLevel(counting, level)
		if err != nil {
			_ = out.Close()
			_ = os.Remove(part)
			return Result{}, fmt.Errorf("create gzip writer: %w", err)
		}
		tarSink = gz
	}

	var payloadBytes int64
	var fileCount int
	tw := tar.NewWriter(tarSink)
	for _, rel := range files {
		full := filepath.Join(opts.SourceDir, rel)
		info, statErr := os.Lstat(full)
		if statErr != nil {
			_ = tw.Close()
			_ = out.Close()
			_ = os.Remove(part)
			return Result{}, fmt.Errorf("stat %s: %w", rel, statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			if opts.Log != nil {
				opts.Log.Warnf("skipping symbolic link %s", rel)
			}
			continue
		}

		header, hdrErr := tar.FileInfoHeader(info, "")
		if hdrErr != nil {
			_ = tw.Close()
			_ = out.Close()
			_ = os.Remove(part)
			return Result{}, fmt.Errorf("build tar header for %s: %w", rel, hdrErr)
		}
		// Slash-separated, prefixed with the bundle directory name so that
		// "tar xf" recreates dbx0.6.14/ exactly as it is on disk. Directories
		// keep their trailing slash, as tar expects.
		header.Name = opts.BaseName + "/" + filepath.ToSlash(rel)
		if info.IsDir() {
			header.Name += "/"
		}
		// Normalize the metadata that would otherwise make archives
		// irreproducible or leak the builder's machine: tar.FileInfoHeader
		// copies ownership, and on Unix it also copies atime/ctime (written as
		// PAX records whose length depends on the values).
		header.ModTime = opts.ModTime
		header.AccessTime = time.Time{}
		header.ChangeTime = time.Time{}
		header.Uid = 0
		header.Gid = 0
		header.Uname = ""
		header.Gname = ""
		header.Format = tar.FormatPAX

		if err := tw.WriteHeader(header); err != nil {
			_ = tw.Close()
			_ = out.Close()
			_ = os.Remove(part)
			return Result{}, fmt.Errorf("write tar header for %s: %w", rel, err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		fileCount++

		src, openErr := os.Open(full)
		if openErr != nil {
			_ = tw.Close()
			_ = out.Close()
			_ = os.Remove(part)
			return Result{}, fmt.Errorf("open %s: %w", rel, openErr)
		}
		written, copyErr := io.Copy(tw, src)
		closeErr := src.Close()
		if copyErr != nil {
			_ = tw.Close()
			_ = out.Close()
			_ = os.Remove(part)
			return Result{}, fmt.Errorf("archive %s: %w", rel, copyErr)
		}
		if closeErr != nil {
			_ = tw.Close()
			_ = out.Close()
			_ = os.Remove(part)
			return Result{}, fmt.Errorf("close %s: %w", rel, closeErr)
		}
		payloadBytes += written
	}

	if err := tw.Close(); err != nil {
		_ = out.Close()
		_ = os.Remove(part)
		return Result{}, fmt.Errorf("finalize tar stream: %w", err)
	}
	if gz != nil {
		if err := gz.Close(); err != nil {
			_ = out.Close()
			_ = os.Remove(part)
			return Result{}, fmt.Errorf("finalize gzip stream: %w", err)
		}
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(part)
		return Result{}, fmt.Errorf("close archive: %w", err)
	}
	if err := os.Rename(part, opts.DestPath); err != nil {
		_ = os.Remove(part)
		return Result{}, fmt.Errorf("move archive into place: %w", err)
	}

	return Result{
		Path:   opts.DestPath,
		Size:   counting.n,
		SHA256: hex.EncodeToString(hasher.Sum(nil)),
		Files:  fileCount,
		Bytes:  payloadBytes,
	}, nil
}

// WriteSHA256Sidecar writes "<archive>.sha256" in the same format as the
// "sha256sum" utility, so it can be checked with "sha256sum -c".
func WriteSHA256Sidecar(archivePath, digest string) (string, error) {
	path := archivePath + ".sha256"
	body := fmt.Sprintf("%s  %s\n", digest, filepath.Base(archivePath))
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return "", fmt.Errorf("write checksum file: %w", err)
	}
	return path, nil
}

// collect walks root and returns the sorted list of directories and regular
// files to archive, relative to root. Sorting keeps the output deterministic
// and guarantees a parent directory entry precedes its children.
func collect(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		name := entry.Name()
		if isJunk(name) {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(name, ".part") {
			// A download was interrupted; never ship a partial artifact.
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		files = append(files, rel)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan %s: %w", root, err)
	}
	sort.Strings(files)
	return files, nil
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}
