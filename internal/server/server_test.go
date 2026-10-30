package server

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jbrozy/chatbox/internal/api"
)

func do(t *testing.T, method, path string, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	for k, v := range header {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	New(":0").Handler.ServeHTTP(rec, req)
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

	// Format laut openapi.yaml: genau {"status": "ok"}, keine weiteren Felder.
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON %q: %v", rec.Body.String(), err)
	}
	if len(body) != 1 || body["status"] != "ok" {
		t.Errorf("body = %v, want {status: ok}", body)
	}
}

func TestReady(t *testing.T) {
	rec := do(t, http.MethodGet, "/ready", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body api.Readiness
	dec := json.NewDecoder(rec.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		t.Fatalf("invalid body: %v", err)
	}
	if !body.Status.Valid() {
		t.Errorf("status = %q, want one of ok|degraded", body.Status)
	}
}

func TestProbesAcceptHead(t *testing.T) {
	for _, path := range []string{"/health", "/ready"} {
		if rec := do(t, http.MethodHead, path, nil); rec.Code != http.StatusOK {
			t.Errorf("HEAD %s: status = %d, want 200", path, rec.Code)
		}
	}
}

func TestLoggingMiddleware(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	do(t, http.MethodGet, "/execute?stream=true", http.Header{
		"Authorization":         {"Bearer secret-token"},
		"X-Terminal-Context-Id": {"chat:42"},
	})

	out := buf.String()
	for _, want := range []string{
		"level=WARN", "method=GET", "path=/execute", "status=400",
		`query="stream=true"`,"auth=bearer", "context_id=chat:42", "headers.Authorization=[redacted]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "secret-token") {
		t.Errorf("log leaks token:\n%s", out)
	}
}
