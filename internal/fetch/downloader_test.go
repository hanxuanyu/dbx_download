package fetch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"dbxdl/internal/ui"
)

// payload builds deterministic test content of the requested size.
func payload(size int) []byte {
	buf := make([]byte, size)
	for i := range buf {
		buf[i] = byte('a' + i%26)
	}
	return buf
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func newTestDownloader(t *testing.T) *Downloader {
	t.Helper()
	return &Downloader{
		Concurrency: 2,
		Retries:     1,
		Verify:      true,
		Log:         ui.NewLogger(os.Stderr, ui.LevelError, false),
	}
}

func TestDownloadWritesFileAndVerifiesDigest(t *testing.T) {
	body := payload(64 * 1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "nested", "file.bin")
	d := newTestDownloader(t)
	err := d.Run(context.Background(), []Request{{
		Label: "file.bin", URL: srv.URL, Dest: dest, SHA256: digest(body), Size: int64(len(body)),
	}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(body) {
		t.Errorf("wrote %d bytes, want %d", len(got), len(body))
	}
	if _, err := os.Stat(dest + ".part"); !os.IsNotExist(err) {
		t.Error("the .part file should have been renamed away")
	}
}

func TestDownloadDetectsChecksumMismatch(t *testing.T) {
	body := payload(1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "file.bin")
	d := newTestDownloader(t)
	err := d.Run(context.Background(), []Request{{
		Label: "file.bin", URL: srv.URL, Dest: dest, SHA256: strings.Repeat("0", 64),
	}})
	if err == nil {
		t.Fatal("expected a checksum failure")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("unexpected error: %v", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Error("a corrupt download must not be left in place")
	}
}

func TestDownloadSkipsValidExistingFile(t *testing.T) {
	body := payload(2048)
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "file.bin")
	if err := os.WriteFile(dest, body, 0o644); err != nil {
		t.Fatal(err)
	}

	d := newTestDownloader(t)
	if err := d.Run(context.Background(), []Request{{
		Label: "file.bin", URL: srv.URL, Dest: dest, SHA256: digest(body), Size: int64(len(body)),
	}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := atomic.LoadInt64(&hits); got != 0 {
		t.Errorf("server was contacted %d times; a valid cached file must not be re-downloaded", got)
	}
}

func TestDownloadReplacesFileWithWrongSize(t *testing.T) {
	body := payload(4096)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "file.bin")
	if err := os.WriteFile(dest, []byte("short"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := newTestDownloader(t)
	if err := d.Run(context.Background(), []Request{{
		Label: "file.bin", URL: srv.URL, Dest: dest, SHA256: digest(body), Size: int64(len(body)),
	}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(body) {
		t.Errorf("replaced file has %d bytes, want %d", len(got), len(body))
	}
}

func TestDownloadForceRefetches(t *testing.T) {
	body := payload(512)
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "file.bin")
	if err := os.WriteFile(dest, body, 0o644); err != nil {
		t.Fatal(err)
	}
	d := newTestDownloader(t)
	d.Force = true
	if err := d.Run(context.Background(), []Request{{
		Label: "file.bin", URL: srv.URL, Dest: dest, SHA256: digest(body), Size: int64(len(body)),
	}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := atomic.LoadInt64(&hits); got != 1 {
		t.Errorf("server hits = %d, want 1 with Force", got)
	}
}

// TestDownloadResumesFromPartialFile leaves a truncated .part file behind and
// checks that only the missing tail is requested.
func TestDownloadResumesFromPartialFile(t *testing.T) {
	body := payload(200 * 1024)
	const already = 50 * 1024

	var mu sync.Mutex
	var rangeHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		rangeHeader = r.Header.Get("Range")
		mu.Unlock()
		http.ServeContent(w, r, "file.bin", time.Now(), strings.NewReader(string(body)))
	}))
	defer srv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "file.bin")
	if err := os.WriteFile(dest+".part", body[:already], 0o644); err != nil {
		t.Fatal(err)
	}

	d := newTestDownloader(t)
	if err := d.Run(context.Background(), []Request{{
		Label: "file.bin", URL: srv.URL, Dest: dest, SHA256: digest(body), Size: int64(len(body)),
	}}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	gotRange := rangeHeader
	mu.Unlock()
	if want := fmt.Sprintf("bytes=%d-", already); gotRange != want {
		t.Errorf("Range header = %q, want %q", gotRange, want)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if digest(got) != digest(body) {
		t.Error("resumed file does not match the original payload")
	}
}

// TestDownloadRestartsWhenServerIgnoresRange covers servers that answer a
// Range request with a full 200 response.
func TestDownloadRestartsWhenServerIgnoresRange(t *testing.T) {
	body := payload(32 * 1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Deliberately ignore Range and always send the whole object.
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "file.bin")
	if err := os.WriteFile(dest+".part", body[:1024], 0o644); err != nil {
		t.Fatal(err)
	}

	d := newTestDownloader(t)
	if err := d.Run(context.Background(), []Request{{
		Label: "file.bin", URL: srv.URL, Dest: dest, SHA256: digest(body), Size: int64(len(body)),
	}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if digest(got) != digest(body) {
		t.Error("file does not match the original payload")
	}
}

func TestDownloadRetriesOnServerError(t *testing.T) {
	body := payload(1024)
	var attempts int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt64(&attempts, 1) == 1 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "file.bin")
	d := newTestDownloader(t)
	d.Retries = 3
	if err := d.Run(context.Background(), []Request{{
		Label: "file.bin", URL: srv.URL, Dest: dest, SHA256: digest(body), Size: int64(len(body)),
	}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := atomic.LoadInt64(&attempts); got != 2 {
		t.Errorf("attempts = %d, want 2", got)
	}
}

func TestDownloadGivesUpOnNotFound(t *testing.T) {
	var attempts int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&attempts, 1)
		http.NotFound(w, r)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "file.bin")
	d := newTestDownloader(t)
	d.Retries = 3
	err := d.Run(context.Background(), []Request{{Label: "file.bin", URL: srv.URL, Dest: dest, Size: 10}})
	if err == nil {
		t.Fatal("expected an error for HTTP 404")
	}
	if got := atomic.LoadInt64(&attempts); got != 1 {
		t.Errorf("a 404 must not be retried; attempts = %d", got)
	}
}

func TestDownloadTruncatedTransferIsRetried(t *testing.T) {
	body := payload(16 * 1024)
	var attempts int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		if atomic.AddInt64(&attempts, 1) == 1 {
			// Declare more bytes than are sent, then close the connection.
			_, _ = w.Write(body[:1024])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			hj, ok := w.(http.Hijacker)
			if ok {
				conn, _, err := hj.Hijack()
				if err == nil {
					_ = conn.Close()
				}
			}
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "file.bin")
	d := newTestDownloader(t)
	d.Retries = 2
	if err := d.Run(context.Background(), []Request{{
		Label: "file.bin", URL: srv.URL, Dest: dest, SHA256: digest(body), Size: int64(len(body)),
	}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got, _ := os.ReadFile(dest); digest(got) != digest(body) {
		t.Error("file does not match the original payload after a retry")
	}
}

func TestRunReportsPerFileErrors(t *testing.T) {
	good := payload(256)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/missing") {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(good)
	}))
	defer srv.Close()

	dir := t.TempDir()
	d := newTestDownloader(t)
	err := d.Run(context.Background(), []Request{
		{Label: "good.bin", URL: srv.URL + "/good", Dest: filepath.Join(dir, "good.bin"), SHA256: digest(good), Size: int64(len(good))},
		{Label: "missing.bin", URL: srv.URL + "/missing", Dest: filepath.Join(dir, "missing.bin"), Size: 10},
	})
	if err == nil {
		t.Fatal("expected the aggregate error to mention the failing file")
	}
	if !strings.Contains(err.Error(), "missing.bin") {
		t.Errorf("error should name the failing file: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "good.bin")); statErr != nil {
		t.Errorf("the successful file must still be present: %v", statErr)
	}
}

func TestParseContentRangeStart(t *testing.T) {
	cases := map[string]struct {
		want int64
		ok   bool
	}{
		"bytes 100-999/1000": {100, true},
		"bytes 0-0/1":        {0, true},
		"":                   {0, false},
		"items 1-2/3":        {0, false},
		"bytes */1000":       {0, false},
	}
	for header, want := range cases {
		got, ok := parseContentRangeStart(header)
		if ok != want.ok || (ok && got != want.want) {
			t.Errorf("parseContentRangeStart(%q) = (%d, %v), want (%d, %v)", header, got, ok, want.want, want.ok)
		}
	}
}

func TestRetryableErrorClassification(t *testing.T) {
	if retryableError(errChecksumMismatch) {
		t.Error("a checksum mismatch must not be retried")
	}
	if !retryableError(errIncomplete) {
		t.Error("a truncated transfer should be retried")
	}
	if retryableError(context.Canceled) {
		t.Error("cancellation must not be retried")
	}
	if !retryableError(&statusError{Status: http.StatusServiceUnavailable}) {
		t.Error("HTTP 503 should be retried")
	}
	if retryableError(&statusError{Status: http.StatusNotFound}) {
		t.Error("HTTP 404 should not be retried")
	}
}
