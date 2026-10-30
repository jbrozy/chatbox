package terminal

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// Manager verwaltet Instanzen unabhängig von der Plattform: Cache, parallele
// Provisionierung, Aktivität, Idle-Cleanup und Refresh. Anlegen, Auflisten und Löschen
// übernimmt der driver (Kubernetes oder Docker).
// Ausgelegt für eine chatbox-Replica: Der Aktivitätsstand liegt nur im Speicher.
type Manager struct {
	drv   driver
	cfg   Config
	group singleflight.Group

	mu        sync.Mutex
	instances map[Key]*tracked
}

// driver ist die Plattform unter dem Manager.
type driver interface {
	// provision liefert die bereite Instanz namens name und legt sie bei Bedarf an.
	provision(ctx context.Context, key Key, name string, spec Spec) (Instance, error)
	// list liefert alle von chatbox angelegten Instanzen.
	list(ctx context.Context) ([]state, error)
	// remove löscht die Instanz, mit reset auch ihre Daten.
	remove(ctx context.Context, name string, reset bool) error
}

// state ist eine Instanz, wie die Plattform sie sieht.
type state struct {
	key                Key
	uid, name, host    string
	running            bool
	idleTimeoutMinutes int
}

type tracked struct {
	inst        *Instance // nil, bis die Instanz aufgelöst ist
	lastActive  time.Time
	connections int
}

func newManager(drv driver, cfg Config) *Manager {
	return &Manager{drv: drv, cfg: cfg, instances: map[Key]*tracked{}}
}

func (m *Manager) Defaults() Config {
	return m.cfg
}

// Ensure liefert die bereite Instanz zum Key und legt sie bei Bedarf an. Parallele Aufrufe
// für denselben Key warten auf dieselbe Provisionierung.
func (m *Manager) Ensure(ctx context.Context, key Key, spec Spec) (Instance, error) {
	m.mu.Lock()
	if t := m.instances[key]; t != nil && t.inst != nil {
		t.lastActive = time.Now()
		inst := *t.inst
		m.mu.Unlock()
		return inst, nil
	}
	m.mu.Unlock()

	name := instanceName(key)
	ch := m.group.DoChan(name, func() (any, error) {
		// Unabhängig vom ersten Aufrufer, sonst bricht ein abgebrochener Request die
		// Provisionierung für alle Wartenden ab.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), m.cfg.ProvisionTimeout)
		defer cancel()
		inst, err := m.drv.provision(ctx, key, name, spec)
		if err != nil {
			return nil, err
		}
		m.mu.Lock()
		t := m.trackLocked(key)
		t.inst = &inst
		t.lastActive = time.Now()
		m.mu.Unlock()
		return inst, nil
	})
	select {
	case res := <-ch:
		if res.Err != nil {
			return Instance{}, res.Err
		}
		return res.Val.(Instance), nil
	case <-ctx.Done():
		return Instance{}, ctx.Err()
	}
}

// Invalidate vergisst die aufgelöste Adresse, z. B. nach einem Verbindungsfehler.
// Der nächste Ensure fragt die Plattform erneut.
func (m *Manager) Invalidate(key Key) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t := m.instances[key]; t != nil {
		t.inst = nil
	}
}

// Touch vermerkt Aktivität, damit das Idle-Cleanup die Instanz nicht löscht.
func (m *Manager) Touch(key Key) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.trackLocked(key).lastActive = time.Now()
}

// Connected zählt eine offene WebSocket-Verbindung. Solange eine besteht, gilt die
// Instanz nie als idle. release muss genau einmal aufgerufen werden.
func (m *Manager) Connected(key Key) (release func()) {
	m.mu.Lock()
	m.trackLocked(key).connections++
	m.mu.Unlock()
	return sync.OnceFunc(func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		t := m.trackLocked(key)
		t.connections--
		t.lastActive = time.Now()
	})
}

func (m *Manager) ActiveConnections() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, t := range m.instances {
		n += t.connections
	}
	return n
}

func (m *Manager) trackLocked(key Key) *tracked {
	t := m.instances[key]
	if t == nil {
		t = &tracked{lastActive: time.Now()}
		m.instances[key] = t
	}
	return t
}

// List liefert alle von chatbox verwalteten Instanzen.
func (m *Manager) List(ctx context.Context) ([]Info, error) {
	states, err := m.drv.list(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]Info, 0, len(states))
	for _, s := range states {
		info := Info{
			Key:                s.key,
			UID:                s.uid,
			Name:               s.name,
			Host:               s.host,
			Status:             "stopped",
			IdleTimeoutMinutes: s.idleTimeoutMinutes,
		}
		if s.running {
			info.Status = "running"
		}
		if t := m.instances[s.key]; t != nil {
			info.LastActive = t.lastActive
			idle := int(now.Sub(t.lastActive).Seconds())
			info.IdleSeconds = &idle
		}
		out = append(out, info)
	}
	return out, nil
}

// Refresh baut passende Instanzen ab, damit der nächste Zugriff sie mit der aktuellen
// Policy neu anlegt. Mit onlyIdle bleiben aktive Instanzen stehen, mit reset werden auch
// die Daten gelöscht.
func (m *Manager) Refresh(ctx context.Context, f Filter, onlyIdle, reset bool) (RefreshResult, error) {
	states, err := m.drv.list(ctx)
	if err != nil {
		return RefreshResult{}, err
	}
	var res RefreshResult
	now := time.Now()
	for _, s := range states {
		if !f.matches(s.key) {
			continue
		}
		res.Matched++
		if onlyIdle && !m.idle(s, now) {
			res.SkippedActive++
			continue
		}
		if err := m.drv.remove(ctx, s.name, reset); err != nil {
			return res, err
		}
		m.forget(s.key)
		res.Refreshed++
		if reset {
			res.Reset++
		}
	}
	return res, nil
}

// Run löscht minütlich Instanzen, die länger als ihr Idle-Timeout ungenutzt sind, bis ctx endet.
func (m *Manager) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.reap(ctx)
		}
	}
}

func (m *Manager) reap(ctx context.Context) {
	states, err := m.drv.list(ctx)
	if err != nil {
		slog.Warn("idle cleanup: instanzen auflisten fehlgeschlagen", "err", err)
		return
	}
	now := time.Now()
	for _, s := range states {
		// Nach einem Neustart unbekannte Instanzen ab jetzt beobachten statt sofort zu löschen.
		m.mu.Lock()
		m.trackLocked(s.key)
		m.mu.Unlock()

		if !m.idle(s, now) {
			continue
		}
		if err := m.drv.remove(ctx, s.name, false); err != nil {
			slog.Warn("idle cleanup fehlgeschlagen", "instance", s.name, "err", err)
			continue
		}
		m.forget(s.key)
		slog.Info("idle instance removed", "instance", s.name, "user_id", s.key.UserID, "policy_id", s.key.PolicyID, "context_id", s.key.ContextID)
	}
}

// idle wie in terminals: Beendete Instanzen sind immer idle, laufende nur mit Timeout > 0,
// ohne offene WebSockets und ohne Aktivität seit dem Timeout.
func (m *Manager) idle(s state, now time.Time) bool {
	if !s.running {
		return true
	}
	if s.idleTimeoutMinutes <= 0 {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.instances[s.key]
	if t == nil || t.connections > 0 {
		return false
	}
	return now.Sub(t.lastActive) >= time.Duration(s.idleTimeoutMinutes)*time.Minute
}

func (m *Manager) forget(key Key) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.instances, key)
}
