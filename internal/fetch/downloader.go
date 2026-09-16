package fetch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"dbxdl/internal/ui"
)

// Permanent failure sentinels, used to decide whether a retry can help.
var (
	// errChecksumMismatch means the payload does not match the digest
	// published by the metadata source. Retrying cannot fix that, so it is
	// reported immediately instead of burning bandwidth.
	errChecksumMismatch = errors.New("checksum mismatch")
	// errIncomplete means the transfer was truncated; a retry resumes it.
	errIncomplete = errors.New("incomplete download")
)

// Request is one file to fetch.
type Request struct {
	// Label is a short human-readable name used in progress output. It
	// defaults to the destination base name.
	Label string
	URL   string
	// Dest is the destination path. Parent directories are created.
	Dest string
	// SHA256 is the expected lowercase hex digest. Empty disables verification.
	SHA256 string
	// Size is the expected size in bytes, or -1 when unknown. It is used to
	// skip already-complete files and to validate the result.
	Size int64
}

// Downloader fetches many files concurrently.
type Downloader struct {
	Concurrency int
	Retries     int
	Verify      bool
	Force       bool
	// Timeout bounds a single request. Zero means 30 minutes.
	Timeout   time.Duration
	UserAgent string
	Log       *ui.Logger
	Progress  *ui.Progress
	// HTTPClient overrides the default client; used by tests.
	HTTPClient *http.Client

	client *http.Client
}

// Result summarises one request's outcome.
type Result struct {
	Request  Request
	Size     int64
	Skipped  bool
	Attempts int
	Err      error
}

func (r Result) label() string {
	if r.Request.Label != "" {
		return r.Request.Label
	}
	return filepath.Base(r.Request.Dest)
}

type job struct {
	index   int
	request Request
}

// Run downloads every request, returning an error joined from all failures.
// Outcomes are reported through Progress, one line (or counter update) per
// file, so the logger stays readable while several downloads run in parallel.
func (d *Downloader) Run(ctx context.Context, reqs []Request) error {
	if len(reqs) == 0 {
		return nil
	}
	concurrency := d.Concurrency
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > len(reqs) {
		concurrency = len(reqs)
	}

	d.client = d.HTTPClient
	if d.client == nil {
		d.client = newHTTPClient(d.Timeout)
	}

	jobs := make(chan job)
	results := make([]Result, len(reqs))
	var wg sync.WaitGroup

	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				res := d.fetchOne(ctx, j.request)
				// Each worker writes a distinct slice element; no lock needed.
				results[j.index] = res
				d.report(res)
			}
		}()
	}

	for i, req := range reqs {
		if ctx.Err() != nil {
			break
		}
		jobs <- job{index: i, request: req}
	}
	close(jobs)
	wg.Wait()

	var errs []error
	for _, res := range results {
		if res.Err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", res.label(), res.Err))
		}
	}
	if err := ctx.Err(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (d *Downloader) report(res Result) {
	if d.Progress != nil {
		label := res.label()
		switch {
		case res.Err != nil:
			d.Progress.Fail(label)
		case res.Skipped:
			d.Progress.Skip(label, res.Size)
		default:
			d.Progress.Finish(label, res.Size)
		}
	}
	if res.Err != nil && d.Log != nil {
		d.Log.Errorf("%s: %v", res.label(), res.Err)
	}
}

// fetchOne performs the download with retries.
func (d *Downloader) fetchOne(ctx context.Context, req Request) Result {
	res := Result{Request: req}

	if !d.Force {
		ok, size, err := d.verifyExisting(req)
		if err != nil {
			if d.Log != nil {
				d.Log.Warnf("%s: re-downloading: %v", res.label(), err)
			}
		}
		if ok {
			res.Skipped = true
			res.Size = size
			return res
		}
	}

	if err := os.MkdirAll(filepath.Dir(req.Dest), 0o755); err != nil {
		res.Err = fmt.Errorf("create destination directory: %w", err)
		return res
	}

	attempts := d.Retries + 1
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		res.Attempts = attempt
		if err := ctx.Err(); err != nil {
			res.Err = err
			return res
		}
		if attempt > 1 {
			delay := backoff(attempt - 1)
			if d.Log != nil {
				d.Log.Warnf("%s: retry %d/%d in %s (%v)", res.label(), attempt-1, d.Retries, delay.Round(time.Millisecond), lastErr)
			}
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				res.Err = ctx.Err()
				return res
			}
		}

		size, err := d.downloadOnce(ctx, req)
		if err == nil {
			res.Size = size
			return res
		}
		lastErr = err
		if !retryableError(err) {
			res.Err = err
			return res
		}
	}
	res.Err = fmt.Errorf("gave up after %d attempts: %w", attempts, lastErr)
	return res
}

// retryableError decides whether another attempt could succeed.
func retryableError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, errChecksumMismatch) {
		return false
	}
	if errors.Is(err, errIncomplete) {
		return true
	}
	var se *statusError
	if errors.As(err, &se) {
		return retryable(nil, se.Status)
	}
	// A filesystem problem (permission denied, disk full) will not fix itself.
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return false
	}
	// Anything else is a transport-level failure.
	return true
}

// verifyExisting checks a pre-existing destination file.
func (d *Downloader) verifyExisting(req Request) (ok bool, size int64, err error) {
	st, err := os.Stat(req.Dest)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, 0, nil
		}
		return false, 0, err
	}
	if st.IsDir() {
		return false, 0, errors.New("destination is a directory")
	}
	if req.Size > 0 && st.Size() != req.Size {
		return false, st.Size(), fmt.Errorf("size mismatch: have %d bytes, want %d", st.Size(), req.Size)
	}
	if d.Verify && req.SHA256 != "" {
		sum, hashErr := SHA256File(req.Dest)
		if hashErr != nil {
			return false, st.Size(), hashErr
		}
		if !strings.EqualFold(sum, req.SHA256) {
			return false, st.Size(), fmt.Errorf("%w: have %s, want %s", errChecksumMismatch, sum, req.SHA256)
		}
	}
	return true, st.Size(), nil
}

// downloadOnce streams the file to Dest+".part" and renames it into place.
// A leftover part file is resumed with a Range request, so an interrupted run
// does not restart a large download from zero.
func (d *Downloader) downloadOnce(ctx context.Context, req Request) (int64, error) {
	part := req.Dest + ".part"
	if req.URL == "" {
		return 0, &statusError{URL: req.URL, Status: 0, Body: "no download URL available"}
	}

	var offset int64
	st, err := os.Stat(part)
	switch {
	case err == nil && st.Size() > 0:
		if req.Size > 0 && st.Size() == req.Size {
			// The transfer finished but the rename did not happen.
			return d.finalize(req, part)
		}
		if req.Size > 0 && st.Size() > req.Size {
			_ = os.Remove(part) // stale or corrupt
		} else {
			offset = st.Size()
		}
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return 0, fmt.Errorf("inspect partial file: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, req.URL, nil)
	if err != nil {
		return 0, err
	}
	httpReq.Header.Set("User-Agent", d.userAgent())
	// Range offsets must refer to raw bytes, so opt out of transparent gzip.
	httpReq.Header.Set("Accept-Encoding", "identity")
	if offset > 0 {
		httpReq.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}

	resp, err := d.client.Do(httpReq)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		offset = 0 // full body, either no Range was sent or it was ignored
	case http.StatusPartialContent:
		if start, ok := parseContentRangeStart(resp.Header.Get("Content-Range")); ok && start != offset {
			offset = 0
		} else if offset > 0 && d.Progress != nil {
			// Resumed bytes came from disk, not the network; they fill the
			// progress bar without inflating the transferred total.
			d.Progress.Resume(offset)
		}
	case http.StatusRequestedRangeNotSatisfiable:
		if rmErr := os.Remove(part); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			return 0, rmErr
		}
		return 0, fmt.Errorf("%w: server rejected resume at offset %d", errIncomplete, offset)
	default:
		return 0, &statusError{URL: req.URL, Status: resp.StatusCode, Body: snippet(resp.Body)}
	}

	flags := os.O_CREATE | os.O_WRONLY
	if offset == 0 {
		flags |= os.O_TRUNC
	}
	file, err := os.OpenFile(part, flags, 0o644)
	if err != nil {
		return 0, fmt.Errorf("open partial file: %w", err)
	}
	if offset > 0 {
		if _, err := file.Seek(offset, io.SeekStart); err != nil {
			_ = file.Close()
			return 0, fmt.Errorf("seek partial file: %w", err)
		}
	}

	buf := make([]byte, 256<<10)
	var written int64
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := file.Write(buf[:n]); writeErr != nil {
				_ = file.Close()
				return 0, fmt.Errorf("write partial file: %w", writeErr)
			}
			written += int64(n)
			if d.Progress != nil {
				d.Progress.Add(int64(n))
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			_ = file.Close()
			return 0, fmt.Errorf("read response body: %w", readErr)
		}
	}
	if err := file.Close(); err != nil {
		return 0, fmt.Errorf("close partial file: %w", err)
	}

	if req.Size > 0 && offset+written != req.Size {
		return 0, fmt.Errorf("%w: have %d bytes, want %d", errIncomplete, offset+written, req.Size)
	}
	return d.finalize(req, part)
}

// finalize verifies the part file and moves it into place.
func (d *Downloader) finalize(req Request, part string) (int64, error) {
	info, err := os.Stat(part)
	if err != nil {
		return 0, fmt.Errorf("stat partial file: %w", err)
	}
	if req.Size > 0 && info.Size() != req.Size {
		_ = os.Remove(part)
		return 0, fmt.Errorf("%w: have %d bytes, want %d", errIncomplete, info.Size(), req.Size)
	}
	if d.Verify && req.SHA256 != "" {
		sum, hashErr := SHA256File(part)
		if hashErr != nil {
			return 0, hashErr
		}
		if !strings.EqualFold(sum, req.SHA256) {
			_ = os.Remove(part)
			return 0, fmt.Errorf("%w: have %s, want %s", errChecksumMismatch, sum, req.SHA256)
		}
		if d.Log != nil {
			d.Log.Debugf("%s: sha256 verified (%s)", req.Label, sum)
		}
	}
	if err := os.Rename(part, req.Dest); err != nil {
		return 0, fmt.Errorf("move into place: %w", err)
	}
	return info.Size(), nil
}

func (d *Downloader) userAgent() string {
	if d.UserAgent != "" {
		return d.UserAgent
	}
	return DefaultUserAgent
}

// SHA256File returns the lowercase hex sha256 digest of a file.
func SHA256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// parseContentRangeStart parses "bytes 100-999/1000" and returns 100.
func parseContentRangeStart(header string) (int64, bool) {
	header = strings.TrimSpace(header)
	if !strings.HasPrefix(strings.ToLower(header), "bytes ") {
		return 0, false
	}
	rest := strings.TrimSpace(header[len("bytes "):])
	dash := strings.IndexByte(rest, '-')
	if dash <= 0 {
		return 0, false
	}
	var start int64
	if _, err := fmt.Sscanf(rest[:dash], "%d", &start); err != nil {
		return 0, false
	}
	return start, true
}

// snippet reads a short prefix of a response body for error messages.
func snippet(r io.Reader) string {
	buf := make([]byte, 256)
	n, _ := io.ReadFull(r, buf)
	if n <= 0 {
		return ""
	}
	text := strings.ReplaceAll(strings.TrimSpace(string(buf[:n])), "\n", " ")
	if len(text) > 160 {
		text = text[:160] + "..."
	}
	return text
}
