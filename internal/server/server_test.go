package server

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jbrozy/chatbox/internal/terminal"
)

const testKey = "test-key"

var authHeader = http.Header{"Authorization": {"Bearer " + testKey}}

func do(t *testing.T, method, path string, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	return serve(t, New(":0", testKey, unavailable()).Handler, method, path, "", header)
}

// unavailable ist ein Backend ohne Kubernetes: Jeder Terminal-Zugriff schlägt fehl.
func unavailable() *fakeBackend {
	return &fakeBackend{err: terminal.ErrUnavailable}
}

func serve(t *testing.T, h http.Handler, method, path, body string, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range header {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHealth(t *testing.T) {
	rec := do(t, http.MethodGet, "/health", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	// Format laut openapi.json (wie Open Terminal): genau {"status": "ok"}, keine weiteren Felder.
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON %q: %v", rec.Body.String(), err)
	}
	if len(body) != 1 || body["status"] != "ok" {
		t.Errorf("body = %v, want {status: ok}", body)
	}
}

func TestHealthAcceptsHead(t *testing.T) {
	if rec := do(t, http.MethodHead, "/health", nil); rec.Code != http.StatusOK {
		t.Errorf("HEAD /health: status = %d, want 200", rec.Code)
	}
}

func TestLoggingMiddleware(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	do(t, http.MethodGet, "/execute?stream=true", http.Header{
		"Authorization":         {"Bearer " + testKey},
		"X-Terminal-Context-Id": {"chat:42"},
	})

	out := buf.String()
	for _, want := range []string{
		"level=WARN", "method=GET", "path=/execute", "status=422",
		`query="stream=true"`, "auth=bearer", "context_id=chat:42", "headers.Authorization=[redacted]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, testKey) {
		t.Errorf("log leaks token:\n%s", out)
	}
}
