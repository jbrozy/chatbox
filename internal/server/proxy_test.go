package server

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/jbrozy/chatbox/internal/terminal"
)

const podKey = "pod-key"

// podInstance zeigt auf einen Test-Server, der einen Open-Terminal-Pod spielt.
func podInstance(t *testing.T, srv *httptest.Server) terminal.Instance {
	t.Helper()
	u, _ := url.Parse(srv.URL)
	host, port, _ := net.SplitHostPort(u.Host)
	p, _ := strconv.Atoi(port)
	return terminal.Instance{Name: "pod", Host: host, Port: p, APIKey: podKey}
}

func newTestServer(t *testing.T, backend Backend) *httptest.Server {
	t.Helper()
	srv := New(":0", testKey, backend)
	h := srv.Handler
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts
}

func userHeader(extra ...string) http.Header {
	h := http.Header{"Authorization": {"Bearer " + testKey}, "X-User-Id": {"u1"}}
	for i := 0; i+1 < len(extra); i += 2 {
		h.Set(extra[i], extra[i+1])
	}
	return h
}

func TestProxyForwardsToInstance(t *testing.T) {
	var got *http.Request
	var gotBody string
	pod := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(r.Context())
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"proc-1"}`))
	}))
	defer pod.Close()
	backend := &fakeBackend{inst: podInstance(t, pod)}
	h := New(":0", testKey, backend).Handler

	// Policy anlegen, damit /p/dev/... sie findet und ihre Daten ans Backend gehen.
	if rec := serve(t, h, http.MethodPut, "/api/v1/policies/dev", `{"cpu_limit":"2"}`, authHeader); rec.Code != 200 {
		t.Fatalf("put policy = %d", rec.Code)
	}

	rec := serve(t, h, http.MethodPost, "/p/dev/execute?wait=5", `{"command":"ls"}`,
		userHeader("X-Session-Id", "chat-1", "X-Terminal-Context-Id", "chat:chat-1", "Content-Type", "application/json"))

	if rec.Code != http.StatusCreated || rec.Body.String() != `{"id":"proc-1"}` {
		t.Fatalf("response = %d %s", rec.Code, rec.Body)
	}
	if got.URL.Path != "/execute" || got.URL.RawQuery != "wait=5" || gotBody != `{"command":"ls"}` {
		t.Errorf("upstream request = %s %s?%s %q", got.Method, got.URL.Path, got.URL.RawQuery, gotBody)
	}
	if a := got.Header.Get("Authorization"); a != "Bearer "+podKey {
		t.Errorf("Authorization = %q, want Pod-Key", a)
	}
	if got.Header.Get("X-User-Id") != "" || got.Header.Get("X-Terminal-Context-Id") != "" {
		t.Error("Routing-Header wurden weitergereicht")
	}
	if got.Header.Get("X-Session-Id") != "chat-1" {
		t.Error("X-Session-Id fehlt beim Pod")
	}
	if k := backend.lastKey(); k != (terminal.Key{UserID: "u1", PolicyID: "dev", ContextID: "chat:chat-1"}) {
		t.Errorf("key = %+v", k)
	}
	if spec := backend.specs[len(backend.specs)-1]; spec["cpu_limit"] != "2" {
		t.Errorf("spec = %v, want Policy-Daten", spec)
	}

	// Ohne Prefix: Default-Policy, Default-Context, mehrsegmentiger Pfad.
	rec = serve(t, h, http.MethodGet, "/files/list?directory=%2Ftmp", "", userHeader())
	if rec.Code != http.StatusCreated || got.URL.Path != "/files/list" || got.URL.Query().Get("directory") != "/tmp" {
		t.Errorf("default proxy = %d %s?%s", rec.Code, got.URL.Path, got.URL.RawQuery)
	}
	if k := backend.lastKey(); k != (terminal.Key{UserID: "u1", PolicyID: "default", ContextID: "default"}) {
		t.Errorf("key = %+v", k)
	}

	// /api/config ohne Context geht an die Instanz von "system", nicht an eine neue des Users.
	for _, p := range []string{"/api/config", "/p/dev/api/config"} {
		serve(t, h, http.MethodGet, p, "", userHeader())
		if k := backend.lastKey(); k.UserID != systemUser || k.ContextID != "default" {
			t.Errorf("%s: key = %+v", p, k)
		}
	}
	serve(t, h, http.MethodGet, "/api/config", "", userHeader("X-Terminal-Context-Id", "chat:chat-1"))
	if k := backend.lastKey(); k != (terminal.Key{UserID: "u1", PolicyID: "default", ContextID: "chat:chat-1"}) {
		t.Errorf("api/config mit Context: key = %+v", k)
	}
}

func TestProxyStreams(t *testing.T) {
	release := make(chan struct{})
	pod := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("first\n"))
		w.(http.Flusher).Flush()
		<-release
		_, _ = w.Write([]byte("second\n"))
	}))
	defer pod.Close()
	defer close(release)
	ts := newTestServer(t, &fakeBackend{inst: podInstance(t, pod)})

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/execute/x/status", nil)
	req.Header = userHeader()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Die erste Zeile muss ankommen, bevor der Pod die Antwort beendet.
	buf := make([]byte, 6)
	done := make(chan error, 1)
	go func() { _, err := io.ReadFull(resp.Body, buf); done <- err }()
	select {
	case err := <-done:
		if err != nil || string(buf) != "first\n" {
			t.Fatalf("read = %q, %v", buf, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Antwort wird gepuffert statt gestreamt")
	}
}

func TestProxyErrors(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	down := podInstance(t, closed)
	closed.Close()

	for _, tt := range []struct {
		name    string
		backend *fakeBackend
		want    string
	}{
		{"provisionierung scheitert", &fakeBackend{err: terminal.ErrUnavailable}, "Terminal instance not reachable"},
		{"pod nicht erreichbar", &fakeBackend{inst: down}, "Terminal instance not reachable"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := serve(t, New(":0", testKey, tt.backend).Handler, http.MethodGet, "/files/list", "", userHeader())
			if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), tt.want) {
				t.Errorf("response = %d %s", rec.Code, rec.Body)
			}
		})
	}
}

func TestProxyUnreachableInvalidates(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	backend := &fakeBackend{inst: podInstance(t, closed)}
	closed.Close()
	serve(t, New(":0", testKey, backend).Handler, http.MethodGet, "/files/list", "", userHeader())
	if backend.invalidated != 1 {
		t.Errorf("invalidated = %d, want 1", backend.invalidated)
	}
}

func TestToolSpec(t *testing.T) {
	var fetches atomic.Int32
	pod := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/openapi.json" || r.Header.Get("Authorization") != "Bearer "+podKey {
			http.Error(w, "unexpected", http.StatusTeapot)
			return
		}
		fetches.Add(1)
		_, _ = w.Write([]byte(`{
			"openapi": "3.1.0",
			"security": [{"HTTPBearer": []}],
			"components": {"securitySchemes": {"HTTPBearer": {"type": "http"}}, "schemas": {}},
			"paths": {"/files/list": {"get": {"operationId": "list_files", "security": [{"HTTPBearer": []}]}}}
		}`))
	}))
	defer pod.Close()
	backend := &fakeBackend{inst: podInstance(t, pod)}
	h := New(":0", testKey, backend).Handler

	rec := serve(t, h, http.MethodGet, "/openapi.json", "", authHeader)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	var spec map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &spec); err != nil {
		t.Fatal(err)
	}
	op := spec["paths"].(map[string]any)["/files/list"].(map[string]any)["get"].(map[string]any)
	if _, ok := spec["security"]; ok {
		t.Error("globale security nicht entfernt")
	}
	if _, ok := spec["components"].(map[string]any)["securitySchemes"]; ok {
		t.Error("securitySchemes nicht entfernt")
	}
	if _, ok := op["security"]; ok || op["operationId"] != "list_files" {
		t.Errorf("operation = %v", op)
	}
	if k := backend.lastKey(); k != (terminal.Key{UserID: "system", PolicyID: "default", ContextID: "default"}) {
		t.Errorf("key = %+v, want Pseudo-User system", k)
	}

	serve(t, h, http.MethodGet, "/openapi.json", "", authHeader)
	if fetches.Load() != 1 {
		t.Errorf("Spec %d-mal geholt, want 1 (Cache)", fetches.Load())
	}

	// Policy-Spec: unbekannt → 404, nach PUT vom Pod der Policy.
	if rec := serve(t, h, http.MethodGet, "/p/dev/openapi.json", "", authHeader); rec.Code != http.StatusNotFound {
		t.Errorf("unknown policy = %d", rec.Code)
	}
	serve(t, h, http.MethodPut, "/api/v1/policies/dev", `{}`, authHeader)
	if rec := serve(t, h, http.MethodGet, "/p/dev/openapi.json", "", authHeader); rec.Code != http.StatusOK {
		t.Errorf("policy spec = %d", rec.Code)
	}
	if k := backend.lastKey(); k.PolicyID != "dev" {
		t.Errorf("key = %+v", k)
	}
}

func TestToolSpecUnavailable(t *testing.T) {
	rec := serve(t, New(":0", testKey, unavailable()).Handler, http.MethodGet, "/openapi.json", "", authHeader)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "Failed to fetch OpenAPI spec") {
		t.Errorf("response = %d %s", rec.Code, rec.Body)
	}
}

// echoPod spielt das WebSocket-Terminal von Open Terminal: erst Auth, dann Echo.
func echoPod(t *testing.T, gotAuth chan<- wsAuth) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/terminals/sess1" {
			http.NotFound(w, r)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var auth wsAuth
		if err := conn.ReadJSON(&auth); err != nil {
			return
		}
		gotAuth <- auth
		for {
			typ, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if string(data) == "exit" {
				_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(4004, "Session has ended"))
				return
			}
			_ = conn.WriteMessage(typ, data)
		}
	}))
}

func dialChatbox(t *testing.T, ts *httptest.Server, path string, header http.Header) *websocket.Conn {
	t.Helper()
	conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+path, header)
	if err != nil {
		t.Fatalf("dial: %v (resp %v)", err, resp)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func TestTerminalWebSocket(t *testing.T) {
	gotAuth := make(chan wsAuth, 1)
	pod := echoPod(t, gotAuth)
	defer pod.Close()
	backend := &fakeBackend{inst: podInstance(t, pod)}
	ts := newTestServer(t, backend)

	// Kein Authorization-Header: Der WebSocket authentifiziert per erster Nachricht.
	conn := dialChatbox(t, ts, "/api/terminals/sess1?user_id=u1", http.Header{
		"X-Session-Id":          {"chat-1"},
		"X-Terminal-Context-Id": {"chat:chat-1"},
	})
	if err := conn.WriteJSON(wsAuth{Type: "auth", Token: testKey}); err != nil {
		t.Fatal(err)
	}

	select {
	case auth := <-gotAuth:
		if auth != (wsAuth{Type: "auth", Token: podKey, ChatID: "chat-1"}) {
			t.Errorf("auth beim Pod = %+v", auth)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Pod hat keine Auth-Nachricht bekommen")
	}
	if k := backend.lastKey(); k != (terminal.Key{UserID: "u1", PolicyID: "default", ContextID: "chat:chat-1"}) {
		t.Errorf("key = %+v", k)
	}

	// Frame-Typ bleibt erhalten.
	for _, msg := range []struct {
		typ  int
		data string
	}{{websocket.BinaryMessage, "ls\n"}, {websocket.TextMessage, `{"type":"resize","cols":120,"rows":40}`}} {
		if err := conn.WriteMessage(msg.typ, []byte(msg.data)); err != nil {
			t.Fatal(err)
		}
		typ, data, err := conn.ReadMessage()
		if err != nil || typ != msg.typ || string(data) != msg.data {
			t.Errorf("echo = %d %q %v, want %d %q", typ, data, err, msg.typ, msg.data)
		}
	}
	if backend.ActiveConnections() != 1 {
		t.Errorf("connections = %d, want 1", backend.ActiveConnections())
	}

	// Close-Code des Pods kommt beim Client an.
	_ = conn.WriteMessage(websocket.TextMessage, []byte("exit"))
	_, _, err := conn.ReadMessage()
	if ce, ok := err.(*websocket.CloseError); !ok || ce.Code != 4004 {
		t.Errorf("close = %v, want 4004", err)
	}
}

func TestTerminalWebSocketRejects(t *testing.T) {
	pod := echoPod(t, make(chan wsAuth, 1))
	defer pod.Close()

	for _, tt := range []struct {
		name string
		path string
		auth any
		want int
	}{
		{"falscher Key", "/api/terminals/sess1?user_id=u1", wsAuth{Type: "auth", Token: "nope"}, closeAuthFailed},
		{"keine Auth-Nachricht", "/api/terminals/sess1?user_id=u1", map[string]string{"type": "input"}, closeAuthFailed},
		{"ohne user_id", "/api/terminals/sess1", wsAuth{Type: "auth", Token: testKey}, closeMissingUser},
		{"unbekannte Policy", "/p/nope/api/terminals/sess1?user_id=u1", wsAuth{Type: "auth", Token: testKey}, closeInvalidTarget},
		{"ungültiger Context", "/api/terminals/sess1?user_id=u1&context_id=team:1", wsAuth{Type: "auth", Token: testKey}, closeInvalidTarget},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ts := newTestServer(t, &fakeBackend{inst: podInstance(t, pod)})
			conn := dialChatbox(t, ts, tt.path, nil)
			_ = conn.WriteJSON(tt.auth)
			_, _, err := conn.ReadMessage()
			if ce, ok := err.(*websocket.CloseError); !ok || ce.Code != tt.want {
				t.Errorf("close = %v, want %d", err, tt.want)
			}
		})
	}

	t.Run("pod nicht erreichbar", func(t *testing.T) {
		ts := newTestServer(t, &fakeBackend{err: terminal.ErrUnavailable})
		conn := dialChatbox(t, ts, "/api/terminals/sess1?user_id=u1", nil)
		_ = conn.WriteJSON(wsAuth{Type: "auth", Token: testKey})
		_, _, err := conn.ReadMessage()
		if ce, ok := err.(*websocket.CloseError); !ok || ce.Code != closeUnreachable {
			t.Errorf("close = %v, want %d", err, closeUnreachable)
		}
	})
}

func TestAdminTerminals(t *testing.T) {
	backend := &fakeBackend{
		infos: []terminal.Info{{
			Key: terminal.Key{UserID: "u1", PolicyID: "default", ContextID: "default"},
			UID: "uid-1", Name: "chatbox-abc", Host: "10.0.0.1", Status: "running",
			LastActive: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC), IdleSeconds: new(5), IdleTimeoutMinutes: 30,
		}},
		result: terminal.RefreshResult{Matched: 2, Refreshed: 1, SkippedActive: 1},
	}
	h := New(":0", testKey, backend).Handler

	rec := serve(t, h, http.MethodGet, "/api/v1/terminals", "", authHeader)
	want := `[{"context_id":"default","host":"10.0.0.1","idle_seconds":5,"idle_timeout_minutes":30,"instance_id":"uid-1","instance_name":"chatbox-abc","last_active_at":"2026-10-30T12:00:00Z","policy_id":"default","port":8000,"status":"running","user_id":"u1"}]` + "\n"
	if rec.Code != 200 || rec.Body.String() != want {
		t.Errorf("terminals = %d %s", rec.Code, rec.Body)
	}

	rec = serve(t, h, http.MethodGet, "/api/v1/status", "", authHeader)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"active_terminals":1`) || !strings.Contains(rec.Body.String(), `"backend":"kubernetes"`) {
		t.Errorf("status = %d %s", rec.Code, rec.Body)
	}

	// Open WebUI schickt beim Refresh-Button nur only_idle und reset.
	rec = serve(t, h, http.MethodPost, "/api/v1/terminals/refresh", `{"only_idle":true,"reset":false}`, authHeader)
	if rec.Code != 200 || rec.Body.String() != `{"matched":2,"refreshed":1,"reset":0,"skipped_active":1}`+"\n" {
		t.Errorf("refresh = %d %s", rec.Code, rec.Body)
	}

	rec = serve(t, h, http.MethodPost, "/api/v1/terminals/stop", `{"user_id":"u1"}`, authHeader)
	if rec.Code != 200 || rec.Body.String() != `{"matched":2,"skipped_active":1,"stopped":1}`+"\n" {
		t.Errorf("stop = %d %s", rec.Code, rec.Body)
	}
	if f := backend.refreshed[len(backend.refreshed)-1]; *f.UserID != "u1" || *f.PolicyID != "default" || *f.ContextID != "default" {
		t.Errorf("stop filter = %+v", f)
	}

	backend.result = terminal.RefreshResult{}
	if rec := serve(t, h, http.MethodPost, "/api/v1/terminals/stop", `{"user_id":"nobody"}`, authHeader); rec.Code != http.StatusNotFound {
		t.Errorf("stop unknown = %d", rec.Code)
	}
}
