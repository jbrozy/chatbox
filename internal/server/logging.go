package server

import (
	"bufio"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

// Header, die Open WebUI mitschickt und die für die Workspace-Auswahl relevant sind.
var loggedHeaders = []struct{ header, key string }{
	{"X-User-Id", "user_id"},
	{"X-Session-Id", "session_id"},
	{"X-Terminal-Context-Id", "context_id"},
}

// Werden im Debug-Log nie im Klartext ausgegeben.
var redactedHeaders = map[string]bool{
	"Authorization": true,
	"Cookie":        true,
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}

		attrs := []slog.Attr{
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", rec.status),
			slog.Int64("bytes", rec.bytes),
			slog.Duration("duration", time.Since(start)),
			slog.String("remote", r.RemoteAddr),
			slog.String("auth", authScheme(r)),
		}
		if r.URL.RawQuery != "" {
			attrs = append(attrs, slog.String("query", r.URL.RawQuery))
		}
		for _, h := range loggedHeaders {
			if v := r.Header.Get(h.header); v != "" {
				attrs = append(attrs, slog.String(h.key, v))
			}
		}
		if slog.Default().Enabled(r.Context(), slog.LevelDebug) {
			attrs = append(attrs, headerAttrs(r.Header))
		}

		slog.LogAttrs(r.Context(), levelForStatus(rec.status), "request", attrs...)
	})
}

func levelForStatus(status int) slog.Level {
	switch {
	case status >= 500 && status != http.StatusNotImplemented:
		return slog.LevelError
	case status >= 400:
		return slog.LevelWarn
	default:
		return slog.LevelInfo
	}
}

// authScheme verrät nur, ob und wie authentifiziert wurde, nie den Token selbst.
func authScheme(r *http.Request) string {
	v := r.Header.Get("Authorization")
	if v == "" {
		return "none"
	}
	if scheme, _, ok := strings.Cut(v, " "); ok {
		return strings.ToLower(scheme)
	}
	return "unknown"
}

func headerAttrs(h http.Header) slog.Attr {
	attrs := make([]any, 0, len(h))
	for k, v := range h {
		if redactedHeaders[k] {
			attrs = append(attrs, slog.String(k, "[redacted]"))
			continue
		}
		if len(v) == 1 {
			attrs = append(attrs, slog.String(k, v[0]))
		} else {
			attrs = append(attrs, slog.Any(k, v))
		}
	}
	return slog.Group("headers", attrs...)
}

// statusRecorder merkt sich Status und Größe der Antwort für das Request-Log.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += int64(n)
	return n, err
}

// Unwrap lässt http.ResponseController zum eigentlichen Writer durch.
func (s *statusRecorder) Unwrap() http.ResponseWriter {
	return s.ResponseWriter
}

// Flush und Hijack direkt implementieren, weil Streaming (/execute) und WebSocket-Libraries
// teils per Type-Assertion statt über http.ResponseController prüfen.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		if s.status == 0 {
			s.status = http.StatusOK
		}
		f.Flush()
	}
}

func (s *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := s.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("hijack not supported")
	}
	if s.status == 0 {
		s.status = http.StatusSwitchingProtocols
	}
	return h.Hijack()
}
