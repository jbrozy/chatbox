package terminal

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const testNetwork = "chatbox"

// fakeEngine bildet die Teile der Docker-Engine-API nach, die der Treiber nutzt.
type fakeEngine struct {
	mu         sync.Mutex
	images     map[string]bool
	containers map[string]*fakeContainer
	volumes    map[string]bool
	pulls      []string
	creates    int
}

type fakeContainer struct {
	body   map[string]any
	status string
}

func newFakeEngine(t *testing.T) (*fakeEngine, string) {
	t.Helper()
	e := &fakeEngine{images: map[string]bool{}, containers: map[string]*fakeContainer{}, volumes: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_ping", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("POST /images/create", func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		defer e.mu.Unlock()
		image := r.URL.Query().Get("fromImage")
		e.pulls = append(e.pulls, image)
		if strings.Contains(image, "gibt-es-nicht") {
			_, _ = w.Write([]byte(`{"status":"Pulling"}` + "\n" + `{"error":"manifest unknown"}` + "\n"))
			return
		}
		e.images[image] = true
		_, _ = w.Write([]byte(`{"status":"Downloaded"}` + "\n"))
	})
	mux.HandleFunc("POST /volumes/create", func(w http.ResponseWriter, r *http.Request) {
		var v struct{ Name string }
		_ = json.NewDecoder(r.Body).Decode(&v)
		e.mu.Lock()
		e.volumes[v.Name] = true
		e.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("DELETE /volumes/{name}", func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		defer e.mu.Unlock()
		if !e.volumes[r.PathValue("name")] {
			notFound(w)
			return
		}
		delete(e.volumes, r.PathValue("name"))
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /containers/create", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		e.mu.Lock()
		defer e.mu.Unlock()
		if !e.images[body["Image"].(string)] {
			notFound(w)
			return
		}
		e.creates++
		e.containers[r.URL.Query().Get("name")] = &fakeContainer{body: body, status: "created"}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"Id":"id"}`))
	})
	mux.HandleFunc("POST /containers/{name}/start", func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		defer e.mu.Unlock()
		c := e.containers[r.PathValue("name")]
		switch {
		case c == nil:
			notFound(w)
		case c.status == "running":
			w.WriteHeader(http.StatusNotModified)
		default:
			c.status = "running"
			w.WriteHeader(http.StatusNoContent)
		}
	})
	mux.HandleFunc("GET /containers/{name}/json", func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		defer e.mu.Unlock()
		c := e.containers[r.PathValue("name")]
		if c == nil {
			notFound(w)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Id":              "id-" + r.PathValue("name"),
			"State":           map[string]any{"Status": c.status, "ExitCode": 1},
			"Config":          map[string]any{"Env": c.body["Env"]},
			"NetworkSettings": map[string]any{"Networks": map[string]any{testNetwork: map[string]any{"IPAddress": "127.0.0.1"}}},
		})
	})
	mux.HandleFunc("DELETE /containers/{name}", func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.containers[r.PathValue("name")] == nil {
			notFound(w)
			return
		}
		delete(e.containers, r.PathValue("name"))
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /containers/json", func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		defer e.mu.Unlock()
		out := []map[string]any{}
		for name, c := range e.containers {
			out = append(out, map[string]any{
				"Id": "id-" + name, "Names": []string{"/" + name}, "State": c.status, "Labels": c.body["Labels"],
				"NetworkSettings": map[string]any{"Networks": map[string]any{testNetwork: map[string]any{"IPAddress": "127.0.0.1"}}},
			})
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return e, "tcp://" + strings.TrimPrefix(srv.URL, "http://")
}

func notFound(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(`{"message":"No such object"}`))
}

// newDockerTestBackend lässt die Container per Health-Server auf 127.0.0.1 bereit wirken.
func newDockerTestBackend(t *testing.T) (*Manager, *fakeEngine) {
	t.Helper()
	engine, host := newFakeEngine(t)
	health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(health.Close)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(health.URL, "http://"))

	m, err := NewDocker(context.Background(), host, Config{
		Image:              "ghcr.io/open-webui/open-terminal",
		IdleTimeoutMinutes: 30,
		ProvisionTimeout:   5 * time.Second,
		DockerNetwork:      testNetwork,
	})
	if err != nil {
		t.Fatal(err)
	}
	d := m.drv.(*dockerDriver)
	d.port, _ = strconv.Atoi(port)
	d.pollInterval = 5 * time.Millisecond
	return m, engine
}

func TestDockerEnsureCreatesContainer(t *testing.T) {
	m, engine := newDockerTestBackend(t)
	key := Key{UserID: "u1", PolicyID: "dev", ContextID: "chat:42"}
	spec := Spec{"cpu_limit": "500m", "memory_limit": "1Gi", "storage": "5Gi", "env": map[string]any{"FOO": "bar"}}

	inst, err := m.Ensure(context.Background(), key, spec)
	if err != nil {
		t.Fatal(err)
	}
	if inst.Host != "127.0.0.1" || inst.APIKey == "" || inst.Name != instanceName(key) {
		t.Errorf("instance = %+v", inst)
	}
	// Image fehlte lokal: einmal mit Tag gezogen.
	if len(engine.pulls) != 1 || engine.pulls[0] != "ghcr.io/open-webui/open-terminal:latest" {
		t.Errorf("pulls = %v", engine.pulls)
	}

	c := engine.containers[inst.Name]
	if c.status != "running" {
		t.Errorf("status = %s", c.status)
	}
	env := strings.Join(toStrings(c.body["Env"]), "\n")
	for _, want := range []string{"OPEN_TERMINAL_API_KEY=" + inst.APIKey, "FOO=bar", "OPEN_TERMINAL_CPU_COUNT=0.5", "OPEN_TERMINAL_MEMORY_BYTES=1073741824"} {
		if !strings.Contains(env, want) {
			t.Errorf("env fehlt %q:\n%s", want, env)
		}
	}
	host := c.body["HostConfig"].(map[string]any)
	if host["NetworkMode"] != testNetwork || host["NanoCpus"] != float64(500_000_000) || host["Memory"] != float64(1<<30) {
		t.Errorf("host config = %v", host)
	}
	if mounts, _ := host["Mounts"].([]any); len(mounts) != 1 || !engine.volumes[inst.Name] {
		t.Errorf("mounts = %v, volumes = %v", host["Mounts"], engine.volumes)
	}
	labels := c.body["Labels"].(map[string]any)
	if labels[dockerContextLabel] != "chat:42" || labels[dockerManagedLabel] != "true" {
		t.Errorf("labels = %v", labels)
	}

	// Nach einem Neustart von chatbox (neuer Manager, leerer Cache) wird der Container wiederverwendet.
	again, err := newManager(m.drv, m.cfg).Ensure(context.Background(), key, spec)
	if err != nil || again.APIKey != inst.APIKey || engine.creates != 1 {
		t.Errorf("again = %+v, %v, creates = %d", again, err, engine.creates)
	}
}

func TestDockerPullError(t *testing.T) {
	m, _ := newDockerTestBackend(t)
	_, err := m.Ensure(context.Background(), Key{UserID: "u1"}, Spec{"image": "gibt-es-nicht"})
	if err == nil || !strings.Contains(err.Error(), "manifest unknown") {
		t.Fatalf("err = %v", err)
	}
}

func TestDockerReplacesExitedContainer(t *testing.T) {
	m, engine := newDockerTestBackend(t)
	key := Key{UserID: "u1", PolicyID: "default", ContextID: "default"}
	first, err := m.Ensure(context.Background(), key, nil)
	if err != nil {
		t.Fatal(err)
	}
	engine.containers[first.Name].status = "exited"
	m.Invalidate(key)

	second, err := m.Ensure(context.Background(), key, nil)
	if err != nil {
		t.Fatal(err)
	}
	if second.APIKey == first.APIKey || engine.creates != 2 {
		t.Errorf("Container wurde nicht ersetzt: creates = %d", engine.creates)
	}
}

func TestDockerRefresh(t *testing.T) {
	m, engine := newDockerTestBackend(t)
	a := Key{UserID: "a", PolicyID: "default", ContextID: "default"}
	b := Key{UserID: "b", PolicyID: "default", ContextID: "default"}
	instA, _ := m.Ensure(context.Background(), a, Spec{"storage": "1Gi"})
	_, _ = m.Ensure(context.Background(), b, nil)

	user := "a"
	res, err := m.Refresh(context.Background(), Filter{UserID: &user}, false, true)
	if err != nil || res != (RefreshResult{Matched: 1, Refreshed: 1, Reset: 1}) {
		t.Fatalf("refresh = %+v, %v", res, err)
	}
	if engine.containers[instA.Name] != nil || engine.volumes[instA.Name] {
		t.Error("Container oder Volume von a existiert noch")
	}
	infos, _ := m.List(context.Background())
	if len(infos) != 1 || infos[0].UserID != "b" || infos[0].Status != "running" {
		t.Errorf("list = %+v", infos)
	}
}

func TestImageRef(t *testing.T) {
	for in, want := range map[string]string{
		"ghcr.io/open-webui/open-terminal":        "ghcr.io/open-webui/open-terminal:latest",
		"ghcr.io/open-webui/open-terminal:0.14.6": "ghcr.io/open-webui/open-terminal:0.14.6",
		"localhost:5000/open-terminal":            "localhost:5000/open-terminal:latest",
		"img@sha256:abc":                          "img@sha256:abc",
	} {
		if got := imageRef(in); got != want {
			t.Errorf("imageRef(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNewDockerRequiresNetwork(t *testing.T) {
	if _, err := NewDocker(context.Background(), "unix:///nope.sock", Config{}); err == nil || !strings.Contains(err.Error(), "TERMINAL_DOCKER_NETWORK") {
		t.Errorf("err = %v", err)
	}
}

func toStrings(v any) []string {
	var out []string
	for _, s := range v.([]any) {
		out = append(out, s.(string))
	}
	return out
}
