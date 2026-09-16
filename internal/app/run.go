package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"dbxdl/internal/archive"
	"dbxdl/internal/config"
	"dbxdl/internal/fetch"
	"dbxdl/internal/naming"
	"dbxdl/internal/ui"
)

// Summary reports what a run produced.
type Summary struct {
	Plan *Plan
	// Downloaded, Skipped and Failed count files.
	Downloaded int
	Skipped    int
	Failed     int
	// Bytes is the number of bytes transferred from the network.
	Bytes int64
	// CachedBytes is the size of files that were already present and valid.
	CachedBytes int64
	Elapsed     time.Duration
	Archive     *archive.Result
	Checksum    string
}

// Run plans and (unless DryRun) executes a download plus packaging run.
func Run(ctx context.Context, cfg *config.Config, opts Options, log *ui.Logger) (*Summary, error) {
	planner := NewPlanner(cfg, log)

	log.Stepf("resolving DBX version %s from %s/%s", versionRequestLabel(opts.Version), cfg.GitHub.Owner, cfg.GitHub.Repo)
	plan, err := planner.Build(ctx, opts)
	if err != nil {
		return nil, err
	}

	printPlan(cfg, plan, opts, log)

	if plan.Degraded {
		// Be explicit about what is weaker in this mode: the asset URLs are
		// derived from the configured names, so nothing was verified against
		// the release metadata.
		log.Warnf("the GitHub API refused the request (quota exhausted), so release details were derived from the Atom feed and the configured asset names")
		if plan.DegradedReason != nil {
			log.Debugf("%v", plan.DegradedReason)
		}
		log.Warnf("file sizes cannot be verified in this mode; sha256 is still checked where the release publishes a .sha256 sidecar")
	}

	for _, w := range plan.Warnings {
		log.Warnf("%s", w)
	}
	for _, w := range plan.RevokedWarnings {
		log.Warnf("%s", w)
	}

	if opts.DryRun {
		log.Okf("dry run: nothing was downloaded")
		return &Summary{Plan: plan}, nil
	}

	if err := os.MkdirAll(plan.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("create bundle directory %s: %w", plan.Dir, err)
	}

	reqs := plan.Requests()
	total := plan.TotalSize()
	tty := ui.IsTerminal(log.Writer())
	progress := ui.NewProgress(log.Writer(), log.Color(), tty, total, len(reqs))
	defer progress.Close()

	concurrency := opts.Concurrency
	if concurrency <= 0 {
		concurrency = cfg.Output.Concurrency
	}
	retries := opts.Retries
	if retries < 0 {
		retries = cfg.Output.Retries
	}

	downloader := &fetch.Downloader{
		Concurrency: concurrency,
		Retries:     retries,
		Verify:      cfg.Output.VerifySHA,
		Force:       opts.Force || cfg.Output.Force,
		Log:         log,
		Progress:    progress,
	}
	log.Stepf("downloading %d files (%s) with %d parallel connections",
		len(reqs), ui.HumanBytes(total), concurrency)

	runErr := downloader.Run(ctx, reqs)
	progress.Close()

	stats := progress.Stats()
	summary := &Summary{
		Plan:        plan,
		Downloaded:  stats.FilesDone - stats.FilesSkip - stats.FilesFail,
		Skipped:     stats.FilesSkip,
		Failed:      stats.FilesFail,
		Bytes:       stats.Transferred,
		CachedBytes: stats.Skipped,
		Elapsed:     progress.Elapsed(),
	}

	if runErr != nil {
		// Keep whatever succeeded on disk: the .part files make the next run
		// resumable. Report the failures and stop before packaging a partial
		// bundle.
		return summary, fmt.Errorf("download failed:\n%w", runErr)
	}

	if summary.Skipped > 0 {
		log.Okf("%d downloaded (%s), %d already present (%s) in %s",
			summary.Downloaded, ui.HumanBytes(summary.Bytes),
			summary.Skipped, ui.HumanBytes(summary.CachedBytes),
			summary.Elapsed.Round(time.Millisecond))
	} else {
		log.Okf("%d downloaded (%s) in %s",
			summary.Downloaded, ui.HumanBytes(summary.Bytes),
			summary.Elapsed.Round(time.Millisecond))
	}

	// Remove checksum sidecars that were never requested (defensive: nothing
	// in the default configuration produces them).
	if err := checkBundleComplete(plan, log); err != nil {
		return summary, err
	}

	if !opts.NoArchive && cfg.Archive.Enabled {
		res, checksum, err := buildArchive(cfg, plan, opts, log)
		if err != nil {
			return summary, err
		}
		summary.Archive = &res
		summary.Checksum = checksum
	}

	// The staging directory holds the unpacked bundle; once the archive is on
	// disk it is no longer needed and is removed by default.
	if !cfg.Output.KeepDir {
		if err := removeStagingDir(plan, summary.Archive, log); err != nil {
			return summary, err
		}
	}
	return summary, nil
}

// removeStagingDir deletes the bundle directory after archiving, refusing to do
// so unless the archive is demonstrably complete. Deleting is the default
// behaviour (output.keep_dir=false), so the checks here are the only thing
// standing between a failed archive step and a lost download.
func removeStagingDir(plan *Plan, res *archive.Result, log *ui.Logger) error {
	if res == nil {
		// No archive was requested (--no-archive or archive.enabled=false);
		// deleting the directory would discard the whole download.
		log.Warnf("output.keep_dir is false but no archive was created; keeping %s", plan.DirName)
		return nil
	}
	info, err := os.Stat(res.Path)
	if err != nil {
		return fmt.Errorf("archive %s is missing; keeping %s: %w", res.Path, plan.DirName, err)
	}
	if info.Size() == 0 {
		return fmt.Errorf("archive %s is empty; keeping %s", res.Path, plan.DirName)
	}
	if res.Files < len(plan.Items) {
		return fmt.Errorf("archive %s contains %d files but %d were downloaded; keeping %s",
			filepath.Base(res.Path), res.Files, len(plan.Items), plan.DirName)
	}
	// RemoveAll is indiscriminate, so refuse to run it over anything this run
	// did not put there: a stray note, extra artifact or hand-placed file is a
	// clear signal that the directory is not just our scratch space.
	extras, err := foreignFiles(plan)
	if err != nil {
		return err
	}
	if len(extras) > 0 {
		log.Warnf("%s also contains %d file(s) this run did not download; keeping the directory",
			plan.DirName, len(extras))
		for _, name := range extras {
			log.Warnf("  unexpected: %s", name)
		}
		return nil
	}

	log.Stepf("removing staging directory %s (keep it with --keep-dir)", plan.Dir)
	if err := os.RemoveAll(plan.Dir); err != nil {
		return fmt.Errorf("remove staging directory %s: %w", plan.Dir, err)
	}
	log.Okf("removed %s; only %s remains", plan.DirName, filepath.Base(res.Path))
	return nil
}

func versionRequestLabel(requested string) string {
	if requested == "" || strings.EqualFold(requested, "latest") {
		return "latest"
	}
	return requested
}

// printPlan renders the resolved plan so the user can see exactly what will be
// downloaded before the transfer starts.
func printPlan(cfg *config.Config, plan *Plan, opts Options, log *ui.Logger) {
	if log.Level() > ui.LevelInfo {
		return
	}
	out := log.Writer()

	release := plan.Release
	published := ""
	if release != nil && !release.Published().IsZero() {
		published = release.Published().UTC().Format("2006-01-02")
	}
	log.Heading("DBX %s", plan.Version.String())
	fmt.Fprintf(out, "  release      %s", plan.Version.Tag())
	if published != "" {
		fmt.Fprintf(out, "  (%s)", published)
	}
	if plan.Degraded {
		fmt.Fprintf(out, "  [degraded: GitHub API quota exhausted]")
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "  bundle dir   %s\n", plan.Dir)
	if !opts.NoArchive && cfg.Archive.Enabled {
		fmt.Fprintf(out, "  archive      %s\n", plan.ArchivePath)
	}
	fmt.Fprintf(out, "  files        %d (%s)\n", len(plan.Items), ui.HumanBytes(plan.TotalSize()))

	var releaseItems, pluginItems []Item
	for _, item := range plan.Items {
		if item.Kind == KindRelease {
			releaseItems = append(releaseItems, item)
		} else {
			pluginItems = append(pluginItems, item)
		}
	}

	if len(releaseItems) > 0 {
		fmt.Fprintf(out, "\n  DBX packages (%d)\n", len(releaseItems))
		writeTable(out, releaseItems)
	}
	if len(pluginItems) > 0 {
		fmt.Fprintf(out, "\n  Plugins (%d)\n", len(pluginItems))
		for _, p := range plan.Plugins {
			fmt.Fprintf(out, "    %s %s -> %s/  [%s]  %s\n",
				p.ID, p.Version, p.SubDir, strings.Join(p.Targets, ","), p.Name)
		}
		writeTable(out, pluginItems)
	}
	fmt.Fprintln(out)
}

func writeTable(out io.Writer, items []Item) {
	width := 0
	for _, item := range items {
		if len(item.RelPath) > width {
			width = len(item.RelPath)
		}
	}
	if width > 64 {
		width = 64
	}
	for _, item := range items {
		size := "-"
		if item.Size > 0 {
			size = ui.HumanBytes(item.Size)
		}
		fmt.Fprintf(out, "    %s  %10s  %s\n", pad(item.RelPath, width), size, item.Note)
	}
}

func pad(s string, width int) string {
	if len(s) >= width {
		return s
	}
	return s + strings.Repeat(" ", width-len(s))
}

// foreignFiles lists regular files inside the staging directory that this run
// did not download. Operating-system metadata is ignored.
func foreignFiles(plan *Plan) ([]string, error) {
	expected := make(map[string]bool, len(plan.Items))
	for _, item := range plan.Items {
		expected[filepath.FromSlash(item.RelPath)] = true
	}

	var extras []string
	err := filepath.WalkDir(plan.Dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if archive.IsMetadataFile(entry.Name()) || strings.HasSuffix(entry.Name(), ".part") {
			return nil
		}
		rel, relErr := filepath.Rel(plan.Dir, path)
		if relErr != nil {
			return relErr
		}
		if !expected[rel] {
			extras = append(extras, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan staging directory %s: %w", plan.Dir, err)
	}
	sort.Strings(extras)
	return extras, nil
}

// checkBundleComplete confirms every planned file landed on disk. The
// downloader already verifies each transfer (and each reused file) against the
// published digest, so this only guards against a file disappearing between
// the transfer and the packaging step.
func checkBundleComplete(plan *Plan, log *ui.Logger) error {
	for _, item := range plan.Items {
		path := filepath.Join(plan.Dir, filepath.FromSlash(item.RelPath))
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("missing file after download: %s", item.RelPath)
		}
		if item.Size > 0 && info.Size() != item.Size {
			return fmt.Errorf("size changed after download: %s (have %d, want %d)",
				item.RelPath, info.Size(), item.Size)
		}
	}
	log.Okf("verified %d files on disk", len(plan.Items))
	return nil
}

// buildArchive writes the tar file and its optional checksum sidecar.
func buildArchive(cfg *config.Config, plan *Plan, opts Options, log *ui.Logger) (archive.Result, string, error) {
	format := cfg.Archive.Format
	gzipLevel := cfg.Archive.GzipLevel
	if opts.ArchiveFormat != "" {
		format = opts.ArchiveFormat
		if format == config.FormatTar {
			gzipLevel = 0
		}
	}

	log.Stepf("packaging %s (%s, gzip level %d)", filepath.Base(plan.ArchivePath), format, gzipLevel)
	started := time.Now()

	res, err := archive.Write(archive.Options{
		SourceDir: plan.Dir,
		BaseName:  plan.DirName,
		DestPath:  plan.ArchivePath,
		Format:    format,
		GzipLevel: gzipLevel,
		// Stamp every entry with the release date instead of "now", so that
		// repeated builds of the same version are byte-identical.
		ModTime: archiveStamp(plan),
		Log:     log,
	})
	if err != nil {
		return archive.Result{}, "", err
	}

	checksum := res.SHA256
	if cfg.Archive.SHA256Sidecar {
		path, err := archive.WriteSHA256Sidecar(res.Path, res.SHA256)
		if err != nil {
			return res, "", err
		}
		log.Okf("wrote %s", filepath.Base(path))
	}

	ratio := ""
	if res.Bytes > 0 {
		ratio = fmt.Sprintf("  (from %s, %.0f%%)", ui.HumanBytes(res.Bytes), float64(res.Size)/float64(res.Bytes)*100)
	}
	log.Okf("%s  %s  %d files  in %s%s",
		filepath.Base(res.Path), ui.HumanBytes(res.Size), res.Files, time.Since(started).Round(time.Millisecond), ratio)
	log.Infof("sha256 %s", checksum)
	return res, checksum, nil
}

// archiveStamp returns the timestamp recorded on every tar entry. Using the
// release publication date keeps the archive reproducible for a given version;
// an unknown date falls back to the Unix epoch rather than to the wall clock.
func archiveStamp(plan *Plan) time.Time {
	if plan.Release != nil {
		if published := plan.Release.Published(); !published.IsZero() {
			return published.UTC()
		}
	}
	return time.Unix(0, 0).UTC()
}

// Message renders the final human-readable summary.
func (s *Summary) Message() string {
	if s == nil || s.Plan == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "DBX %s", s.Plan.Version.String())
	if s.Archive != nil {
		fmt.Fprintf(&b, " -> %s", s.Archive.Path)
	} else {
		fmt.Fprintf(&b, " -> %s", s.Plan.Dir)
	}
	var parts []string
	parts = append(parts, fmt.Sprintf("%d downloaded", s.Downloaded))
	if s.Skipped > 0 {
		parts = append(parts, fmt.Sprintf("%d cached", s.Skipped))
	}
	if s.Failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", s.Failed))
	}
	fmt.Fprintf(&b, " (%s)", strings.Join(parts, ", "))
	return b.String()
}

// EnsureDirNameIsSafe rejects bundle directory names that would escape the
// output directory.
func EnsureDirNameIsSafe(name string) error {
	if name == "" {
		return errors.New("empty bundle directory name")
	}
	if filepath.IsAbs(name) || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("bundle directory name %q must be a single path element", name)
	}
	if name == "." || name == ".." {
		return fmt.Errorf("invalid bundle directory name %q", name)
	}
	if _, err := naming.SafeRelPath(name); err != nil {
		return err
	}
	return nil
}
