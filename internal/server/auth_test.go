package server

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestAuth(t *testing.T) {
	withUser := http.Header{"Authorization": {"Bearer " + testKey}, "X-User-Id": {"u1"}}
	tests := []struct {
		name   string
		method string
		path   string
		header http.Header
		want   int
	}{
		{"policies ohne Key", http.MethodGet, "/api/v1/policies", nil, http.StatusUnauthorized},
		{"policies falscher Key", http.MethodGet, "/api/v1/policies", http.Header{"Authorization": {"Bearer nope"}}, http.StatusUnauthorized},
		{"policies falsches Schema", http.MethodGet, "/api/v1/policies", http.Header{"Authorization": {"Basic " + testKey}}, http.StatusUnauthorized},
		{"policies richtiger Key", http.MethodGet, "/api/v1/policies", authHeader, http.StatusOK},
		// Mit X-User-Id ist es ein User-Request, der nicht an die Admin-API darf.
		{"policies mit X-User-Id", http.MethodGet, "/api/v1/policies", withUser, http.StatusForbidden},
		{"proxy ohne Key", http.MethodGet, "/p/dev/files/list", http.Header{"X-User-Id": {"u1"}}, http.StatusUnauthorized},
		{"default-proxy ohne Key", http.MethodPost, "/execute", http.Header{"X-User-Id": {"u1"}}, http.StatusUnauthorized},
		{"default-proxy ohne X-User-Id", http.MethodPost, "/execute", authHeader, http.StatusUnprocessableEntity},
		{"mehrsegmentig ohne X-User-Id", http.MethodGet, "/files/list", authHeader, http.StatusUnprocessableEntity},
		{"health ohne Key", http.MethodGet, "/health", nil, http.StatusOK},
		{"health HEAD ohne Key", http.MethodHead, "/health", nil, http.StatusOK},
		// WebSocket authentifiziert per erster Nachricht, nicht per Header.
		{"websocket ohne Key", http.MethodGet, "/api/terminals/abc?user_id=u1", http.Header{"X-User-Id": {"u1"}, "Upgrade": {"websocket"}}, http.StatusNotImplemented},
		{"terminal-GET ohne Key", http.MethodGet, "/api/terminals/abc", http.Header{"X-User-Id": {"u1"}}, http.StatusUnauthorized},
		{"terminal-GET mit Key", http.MethodGet, "/api/terminals/abc", withUser, http.StatusNotImplemented},
		{"ungültiger Context", http.MethodGet, "/files/list", http.Header{"Authorization": {"Bearer " + testKey}, "X-User-Id": {"u1"}, "X-Terminal-Context-Id": {"team:1"}}, http.StatusBadRequest},
		{"unbekannte Policy", http.MethodGet, "/p/dev/files/list", withUser, http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := do(t, tt.method, tt.path, tt.header)
			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d (body %s)", rec.Code, tt.want, rec.Body)
			}
			if rec.Code == http.StatusUnauthorized && rec.Header().Get("WWW-Authenticate") == "" {
				t.Error("WWW-Authenticate fehlt")
			}
		})
	}
}

func TestAuthEmptyKeyRejectsAll(t *testing.T) {
	rec := serve(t, New(":0", "").Handler, http.MethodGet, "/api/v1/policies", "", http.Header{"Authorization": {"Bearer "}})
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestPolicyLifecycle(t *testing.T) {
	h := New(":0", testKey).Handler
	call := func(method, path, body string) (int, string) {
		rec := serve(t, h, method, path, body, authHeader)
		return rec.Code, rec.Body.String()
	}

	// Verify Connection: leere Liste muss [] sein, nicht null.
	if code, body := call(http.MethodGet, "/api/v1/policies", ""); code != 200 || body != "[]\n" {
		t.Fatalf("list = %d %q, want 200 []", code, body)
	}
	if code, body := call(http.MethodGet, "/api/v1/policies/dev", ""); code != 404 || body != `{"detail":"Policy 'dev' not found"}`+"\n" {
		t.Fatalf("get unknown = %d %s, want 404", code, body)
	}
	if code, body := call(http.MethodGet, "/api/v1/policies/dev/lifecycle", ""); code != 200 || body != `{"data":{},"policy_id":"dev"}`+"\n" {
		t.Fatalf("lifecycle unknown = %d %s", code, body)
	}

	// PUT ersetzt die Daten vollständig; unbekannte Felder werden gespeichert, null fällt weg.
	if code, body := call(http.MethodPut, "/api/v1/policies/dev", `{"image":"a","pod_template":{"x":1}}`); code != 200 {
		t.Fatalf("put = %d %s", code, body)
	}
	if code, body := call(http.MethodPut, "/api/v1/policies/dev", `{"image":"b","storage":null,"custom":"c"}`); code != 200 {
		t.Fatalf("put = %d %s", code, body)
	}
	_, body := call(http.MethodGet, "/api/v1/policies/dev", "")
	var p struct {
		ID        string         `json:"id"`
		Data      map[string]any `json:"data"`
		CreatedAt string         `json:"created_at"`
	}
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatal(err)
	}
	if want := map[string]any{"image": "b", "custom": "c"}; len(p.Data) != len(want) || p.Data["image"] != "b" || p.Data["custom"] != "c" {
		t.Errorf("data = %v, want %v", p.Data, want)
	}
	if p.ID != "dev" || p.CreatedAt == "" {
		t.Errorf("policy = %s", body)
	}

	if code, _ := call(http.MethodPost, "/api/v1/policies", `{"id":"dev"}`); code != 409 {
		t.Errorf("create existing = %d, want 409", code)
	}
	if code, body := call(http.MethodPost, "/api/v1/policies", `{"id":"prod","data":{"cpu_limit":"2"}}`); code != 201 {
		t.Errorf("create = %d %s, want 201", code, body)
	}

	for _, tt := range []struct {
		path, body string
		want       int
	}{
		{"/api/v1/policies/dev", `{"reset":{}}`, 400},
		{"/api/v1/policies/dev", `{"restricted":true,"env":{"OPEN_TERMINAL_MULTI_USER":"true"}}`, 400},
		{"/api/v1/policies/dev", `{"image":1}`, 422},
		{"/api/v1/policies/dev", `not json`, 422},
		{"/api/v1/policies/dev/lifecycle", `{"reset":{"schedule":"jeden Montag"}}`, 400},
		{"/api/v1/policies/dev/lifecycle", `{"reset":{"schedule":"2026-12-24 18:00"}}`, 400},
		{"/api/v1/policies/dev/lifecycle", `{"reset":{"schedule":"0 3 * * 1","timezone":"Europe/Berlin"}}`, 200},
		{"/api/v1/policies/dev/lifecycle", `{"reset":{"schedule":"2026-12-24T18:00:00Z"}}`, 200},
		{"/api/v1/policies/dev/lifecycle", `{"reset":{"schedule":"@weekly"}}`, 200},
	} {
		if code, body := call(http.MethodPut, tt.path, tt.body); code != tt.want {
			t.Errorf("PUT %s %s = %d %s, want %d", tt.path, tt.body, code, body, tt.want)
		}
	}

	// Mit gespeicherter Policy geht der Request bis zum (noch fehlenden) Proxy durch.
	rec := serve(t, h, http.MethodGet, "/p/dev/files/list", "", http.Header{"Authorization": {"Bearer " + testKey}, "X-User-Id": {"u1"}})
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("proxy known policy = %d, want 501", rec.Code)
	}

	if code, body := call(http.MethodDelete, "/api/v1/policies/dev", ""); code != 200 || body != `{"ok":true}`+"\n" {
		t.Errorf("delete = %d %s, want 200 {ok:true}", code, body)
	}
	if code, _ := call(http.MethodDelete, "/api/v1/policies/dev", ""); code != 404 {
		t.Errorf("delete again = %d, want 404", code)
	}
}

func TestPyTime(t *testing.T) {
	// Wie Pythons str(datetime): Mikrosekunden nur, wenn sie nicht 0 sind.
	for in, want := range map[time.Time]string{
		time.Date(2026, 10, 30, 16, 15, 1, 123456789, time.UTC): "2026-10-30 16:15:01.123456",
		time.Date(2026, 10, 30, 16, 15, 1, 0, time.UTC):         "2026-10-30 16:15:01",
	} {
		if got := pyTime(in); got != want {
			t.Errorf("pyTime(%v) = %q, want %q", in, got, want)
		}
	}
}
