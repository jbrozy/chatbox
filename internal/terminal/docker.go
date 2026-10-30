package terminal

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Docker-Labels, getrennt von den Kubernetes-Labels und -Annotationen.
const (
	dockerManagedLabel = "chatbox.managed"
	dockerUserLabel    = "chatbox.user-id"
	dockerPolicyLabel  = "chatbox.policy-id"
	dockerContextLabel = "chatbox.context-id"
	dockerIdleLabel    = "chatbox.idle-timeout-minutes"
)

// NewDocker startet pro Key einen Container über die Docker-Engine-API. Die Container
// hängen im Netz cfg.DockerNetwork und werden über ihre IP darin angesprochen, chatbox
// muss also im selben Netz laufen. host ist DOCKER_HOST (unix:// oder tcp://).
func NewDocker(ctx context.Context, host string, cfg Config) (*Manager, error) {
	if cfg.DockerNetwork == "" {
		return nil, errors.New("kein Docker-Netz angegeben (TERMINAL_DOCKER_NETWORK)")
	}
	api, err := newDockerAPI(host)
	if err != nil {
		return nil, err
	}
	if err := api.do(ctx, http.MethodGet, "/_ping", nil, nil, nil); err != nil {
		return nil, fmt.Errorf("docker nicht erreichbar: %w", err)
	}
	cfg.Backend = "docker"
	return newManager(&dockerDriver{
		api:          api,
		cfg:          cfg,
		health:       &http.Client{Timeout: 2 * time.Second},
		port:         Port,
		pollInterval: 500 * time.Millisecond,
	}, cfg), nil
}

type dockerDriver struct {
	api          *dockerAPI
	cfg          Config
	health       *http.Client
	port         int
	pollInterval time.Duration
}

func (d *dockerDriver) provision(ctx context.Context, key Key, name string, spec Spec) (Instance, error) {
	c, err := d.inspect(ctx, name)
	switch {
	case isDockerNotFound(err):
		c = nil
	case err != nil:
		return Instance{}, fmt.Errorf("container %s lesen: %w", name, err)
	case c.State.Status == "exited" || c.State.Status == "dead":
		if err := d.remove(ctx, name, false); err != nil {
			return Instance{}, err
		}
		c = nil
	}

	if c == nil {
		slog.Info("provisioning container", "container", name, "user_id", key.UserID, "policy_id", key.PolicyID, "context_id", key.ContextID)
		if err := d.create(ctx, key, name, spec); err != nil {
			return Instance{}, err
		}
	}
	// Idempotent: Für laufende Container antwortet Docker mit 304.
	if err := d.api.do(ctx, http.MethodPost, "/containers/"+name+"/start", nil, nil, nil); err != nil {
		return Instance{}, fmt.Errorf("container %s starten: %w", name, err)
	}

	if c, err = d.inspect(ctx, name); err != nil {
		return Instance{}, fmt.Errorf("container %s lesen: %w", name, err)
	}
	ip := c.NetworkSettings.Networks[d.cfg.DockerNetwork].IPAddress
	if ip == "" {
		return Instance{}, fmt.Errorf("container %s hat keine IP im Netz %q", name, d.cfg.DockerNetwork)
	}
	inst := Instance{Name: name, Host: ip, Port: d.port, APIKey: envValue(c.Config.Env, "OPEN_TERMINAL_API_KEY")}
	if inst.APIKey == "" {
		return Instance{}, fmt.Errorf("container %s hat keinen API-Key", name)
	}
	if err := d.waitHealthy(ctx, inst); err != nil {
		return Instance{}, err
	}
	return inst, nil
}

func (d *dockerDriver) create(ctx context.Context, key Key, name string, spec Spec) error {
	extraEnv, lim, err := containerEnv(spec)
	if err != nil {
		return err
	}
	env := []string{"OPEN_TERMINAL_API_KEY=" + rand.Text()}
	for _, e := range extraEnv {
		env = append(env, e.name+"="+e.value)
	}

	labels := map[string]string{
		dockerManagedLabel: "true",
		dockerUserLabel:    key.UserID,
		dockerPolicyLabel:  key.PolicyID,
		dockerContextLabel: key.ContextID,
		dockerIdleLabel:    strconv.Itoa(idleTimeout(d.cfg, spec)),
	}
	hostConfig := dockerHostConfig{
		NetworkMode:   d.cfg.DockerNetwork,
		RestartPolicy: dockerRestartPolicy{Name: "unless-stopped"},
	}
	if lim.cpu != nil {
		hostConfig.NanoCPUs = lim.cpu.MilliValue() * 1_000_000
	}
	if lim.memory != nil {
		hostConfig.Memory = lim.memory.Value()
	}
	if egressFiltered(spec) {
		hostConfig.CapAdd = []string{"NET_ADMIN"}
	}
	// Docker setzt keine Größe durch; storage entscheidet nur, ob /home/user erhalten bleibt.
	if str(spec, "storage") != "" {
		vol := map[string]any{"Name": name, "Labels": labels}
		if err := d.api.do(ctx, http.MethodPost, "/volumes/create", nil, vol, nil); err != nil {
			return fmt.Errorf("volume %s anlegen: %w", name, err)
		}
		hostConfig.Mounts = []dockerMount{{Type: "volume", Source: name, Target: homeDir}}
	}

	image := imageRef(cmp.Or(str(spec, "image"), d.cfg.Image))
	body := map[string]any{"Image": image, "Env": env, "Labels": labels, "HostConfig": hostConfig}
	query := url.Values{"name": {name}}
	err = d.api.do(ctx, http.MethodPost, "/containers/create", query, body, nil)
	if isDockerNotFound(err) {
		slog.Info("pulling image", "image", image)
		if err := d.api.pull(ctx, image); err != nil {
			return err
		}
		err = d.api.do(ctx, http.MethodPost, "/containers/create", query, body, nil)
	}
	if err != nil {
		return fmt.Errorf("container %s anlegen: %w", name, err)
	}
	return nil
}

func (d *dockerDriver) waitHealthy(ctx context.Context, inst Instance) error {
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, inst.URL("http")+"/health", nil)
		if resp, err := d.health.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		if c, err := d.inspect(ctx, inst.Name); err == nil && (c.State.Status == "exited" || c.State.Status == "dead") {
			return fmt.Errorf("container %s ist beendet (Exit-Code %d), siehe docker logs %s", inst.Name, c.State.ExitCode, inst.Name)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("container %s nicht rechtzeitig bereit: %w", inst.Name, ctx.Err())
		case <-time.After(d.pollInterval):
		}
	}
}

func (d *dockerDriver) list(ctx context.Context) ([]state, error) {
	filters, _ := json.Marshal(map[string][]string{"label": {dockerManagedLabel + "=true"}})
	var containers []dockerSummary
	query := url.Values{"all": {"1"}, "filters": {string(filters)}}
	if err := d.api.do(ctx, http.MethodGet, "/containers/json", query, nil, &containers); err != nil {
		return nil, fmt.Errorf("container auflisten: %w", err)
	}
	states := make([]state, 0, len(containers))
	for _, c := range containers {
		idle, _ := strconv.Atoi(c.Labels[dockerIdleLabel])
		var name string
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		states = append(states, state{
			key: Key{
				UserID:    c.Labels[dockerUserLabel],
				PolicyID:  c.Labels[dockerPolicyLabel],
				ContextID: c.Labels[dockerContextLabel],
			},
			uid:                c.ID,
			name:               name,
			host:               c.NetworkSettings.Networks[d.cfg.DockerNetwork].IPAddress,
			running:            c.State == "running" || c.State == "created" || c.State == "restarting",
			idleTimeoutMinutes: idle,
		})
	}
	return states, nil
}

// remove löscht den Container, mit reset auch das Volume (Daten des Users).
func (d *dockerDriver) remove(ctx context.Context, name string, reset bool) error {
	err := d.api.do(ctx, http.MethodDelete, "/containers/"+name, url.Values{"force": {"1"}}, nil, nil)
	if err != nil && !isDockerNotFound(err) {
		return fmt.Errorf("container %s löschen: %w", name, err)
	}
	if reset {
		err := d.api.do(ctx, http.MethodDelete, "/volumes/"+name, nil, nil, nil)
		if err != nil && !isDockerNotFound(err) {
			return fmt.Errorf("volume %s löschen: %w", name, err)
		}
	}
	return nil
}

func (d *dockerDriver) inspect(ctx context.Context, name string) (*dockerContainer, error) {
	var c dockerContainer
	if err := d.api.do(ctx, http.MethodGet, "/containers/"+name+"/json", nil, nil, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// --- Docker-Engine-API ---

type dockerHostConfig struct {
	NetworkMode   string
	RestartPolicy dockerRestartPolicy
	NanoCPUs      int64         `json:"NanoCpus,omitempty"`
	Memory        int64         `json:",omitempty"`
	CapAdd        []string      `json:",omitempty"`
	Mounts        []dockerMount `json:",omitempty"`
}

type dockerRestartPolicy struct {
	Name string
}

type dockerMount struct {
	Type, Source, Target string
}

type dockerNetworks struct {
	Networks map[string]struct{ IPAddress string }
}

type dockerContainer struct {
	ID    string `json:"Id"`
	State struct {
		Status   string
		ExitCode int
	}
	Config struct {
		Env []string
	}
	NetworkSettings dockerNetworks
}

type dockerSummary struct {
	ID              string `json:"Id"`
	Names           []string
	State           string
	Labels          map[string]string
	NetworkSettings dockerNetworks
}

type dockerAPI struct {
	client *http.Client
	base   string
}

func newDockerAPI(host string) (*dockerAPI, error) {
	host = cmp.Or(host, "unix:///var/run/docker.sock")
	switch {
	case strings.HasPrefix(host, "unix://"):
		path := strings.TrimPrefix(host, "unix://")
		transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		}}
		return &dockerAPI{client: &http.Client{Transport: transport}, base: "http://docker"}, nil
	case strings.HasPrefix(host, "tcp://"):
		return &dockerAPI{client: &http.Client{}, base: "http://" + strings.TrimPrefix(host, "tcp://")}, nil
	}
	return nil, fmt.Errorf("DOCKER_HOST %q: nur unix:// und tcp:// (ohne TLS) werden unterstützt", host)
}

type dockerError struct {
	status  int
	message string
}

func (e *dockerError) Error() string {
	return fmt.Sprintf("docker: %d %s", e.status, e.message)
}

func isDockerNotFound(err error) bool {
	var de *dockerError
	return errors.As(err, &de) && de.status == http.StatusNotFound
}

// do ruft die Engine-API ohne Versionspräfix auf; Docker nimmt dann seine aktuelle Version.
// 304 (z. B. Start eines laufenden Containers) gilt als Erfolg.
func (d *dockerAPI) do(ctx context.Context, method, path string, query url.Values, in, out any) error {
	resp, err := d.request(ctx, method, path, query, in)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func (d *dockerAPI) request(ctx context.Context, method, path string, query url.Values, in any) (*http.Response, error) {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(raw)
	}
	target := d.base + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotModified {
		defer resp.Body.Close()
		var e struct {
			Message string `json:"message"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return nil, &dockerError{status: resp.StatusCode, message: e.Message}
	}
	return resp, nil
}

// pull lädt ein Image. Fehler meldet Docker im Stream, nicht über den Statuscode.
func (d *dockerAPI) pull(ctx context.Context, image string) error {
	resp, err := d.request(ctx, http.MethodPost, "/images/create", url.Values{"fromImage": {image}}, nil)
	if err != nil {
		return fmt.Errorf("image %s ziehen: %w", image, err)
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(resp.Body)
	for {
		var msg struct {
			Error string `json:"error"`
		}
		if err := dec.Decode(&msg); err == io.EOF {
			return nil
		} else if err != nil {
			return fmt.Errorf("image %s ziehen: %w", image, err)
		}
		if msg.Error != "" {
			return fmt.Errorf("image %s ziehen: %s", image, msg.Error)
		}
	}
}

// imageRef ergänzt ":latest", sonst zieht Docker alle Tags eines Images.
func imageRef(image string) string {
	last := image[strings.LastIndex(image, "/")+1:]
	if strings.Contains(last, ":") || strings.Contains(image, "@") {
		return image
	}
	return image + ":latest"
}

func envValue(env []string, name string) string {
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, name+"="); ok {
			return v
		}
	}
	return ""
}
