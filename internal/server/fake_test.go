package server

import (
	"context"
	"sync"

	"github.com/jbrozy/chatbox/internal/terminal"
)

// fakeBackend liefert für jeden Key dieselbe Instanz, z. B. einen httptest-Server.
type fakeBackend struct {
	mu          sync.Mutex
	inst        terminal.Instance
	err         error
	ensured     []terminal.Key
	specs       []terminal.Spec
	invalidated int
	connections int
	infos       []terminal.Info
	refreshed   []terminal.Filter
	result      terminal.RefreshResult
}

func (f *fakeBackend) Ensure(_ context.Context, key terminal.Key, spec terminal.Spec) (terminal.Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensured = append(f.ensured, key)
	f.specs = append(f.specs, spec)
	return f.inst, f.err
}

func (f *fakeBackend) Invalidate(terminal.Key) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.invalidated++
}

func (f *fakeBackend) Touch(terminal.Key) {}

func (f *fakeBackend) Connected(terminal.Key) func() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connections++
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.connections--
	}
}

func (f *fakeBackend) ActiveConnections() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connections
}

func (f *fakeBackend) List(context.Context) ([]terminal.Info, error) {
	return f.infos, f.err
}

func (f *fakeBackend) Refresh(_ context.Context, filter terminal.Filter, _, _ bool) (terminal.RefreshResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refreshed = append(f.refreshed, filter)
	return f.result, f.err
}

func (f *fakeBackend) Defaults() terminal.Config {
	return terminal.Config{Backend: "kubernetes", Image: "ghcr.io/open-webui/open-terminal:latest", IdleTimeoutMinutes: 30}
}

func (f *fakeBackend) lastKey() terminal.Key {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ensured[len(f.ensured)-1]
}
