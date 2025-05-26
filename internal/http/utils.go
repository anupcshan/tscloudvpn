package http

import (
	"io"
	"log"
	"net/http"

	"github.com/felixge/httpsnoop"
)

// flushWriter wraps an io.Writer and flushes after each write if possible
type flushWriter struct {
	w io.Writer
}

// Write implements io.Writer interface with automatic flushing
func (f flushWriter) Write(b []byte) (int, error) {
	n, err := f.w.Write(b)
	if flusher, ok := f.w.(http.Flusher); ok {
		flusher.Flush()
	}

	return n, err
}

// NewFlushWriter creates a new flushWriter wrapping the given writer
func NewFlushWriter(w io.Writer) io.Writer {
	return flushWriter{w: w}
}

// LogRequest returns an HTTP middleware that logs requests
func LogRequest(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m := httpsnoop.CaptureMetrics(handler, w, r)
		log.Printf(
			"%s %d %s %s %s",
			r.RemoteAddr,
			m.Code,
			r.Method,
			r.URL,
			m.Duration,
		)
	})
}
