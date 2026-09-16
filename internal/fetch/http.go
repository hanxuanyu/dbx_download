// Package fetch implements the concurrent, resumable, checksum-verified file
// downloader used for both DBX release assets and plugin artifacts.
package fetch

import (
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"time"
)

// DefaultUserAgent identifies dbxdl to the CDNs it downloads from.
const DefaultUserAgent = "dbxdl (+https://github.com/t8y2/dbx)"

// newHTTPClient builds the shared HTTP client. Redirects are followed
// automatically (GitHub release assets redirect to objects.githubusercontent.com
// and dl.dbxio.com may redirect to a CDN), and idle connections are pooled so
// concurrent downloads reuse TCP connections.
func newHTTPClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 5 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	return &http.Client{Transport: transport, Timeout: timeout}
}

// retryable reports whether a failed request is worth retrying.
func retryable(err error, status int) bool {
	if status == http.StatusRequestTimeout ||
		status == http.StatusTooManyRequests ||
		status >= 500 {
		return true
	}
	if status != 0 {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	// Unexpected EOF and similar transport truncations are common on long
	// downloads and are safe to retry thanks to resumption.
	return errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF)
}

// backoff returns the delay before attempt n (1-based), with jitter so that
// parallel downloads do not stampede the same host after a shared failure.
// The math/rand top-level functions are safe for concurrent use.
func backoff(attempt int) time.Duration {
	base := time.Duration(1) << uint(attempt-1)
	if base > 20*time.Second {
		base = 20 * time.Second
	}
	return base*time.Second + time.Duration(rand.Intn(750))*time.Millisecond
}

// statusError is a non-2xx response turned into an error.
type statusError struct {
	URL    string
	Status int
	Body   string
}

func (e *statusError) Error() string {
	msg := fmt.Sprintf("GET %s: HTTP %d", e.URL, e.Status)
	if e.Body != "" {
		msg += ": " + e.Body
	}
	return msg
}
