package server

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jbrozy/chatbox/internal/api"
)

// policyStore hält Policies und Lifecycle-Einstellungen im Speicher.
// TODO: persistieren (ConfigMap/CRD), aktuell gehen sie bei einem Neustart verloren.
type policyStore struct {
	mu         sync.RWMutex
	policies   map[string]storedPolicy
	lifecycles map[string]api.PolicyLifecycleData
}

type storedPolicy struct {
	id                   string
	data                 map[string]any
	createdAt, updatedAt time.Time
}

func (p storedPolicy) response() api.PolicyResponse {
	created, updated := pyTime(p.createdAt), pyTime(p.updatedAt)
	return api.PolicyResponse{Id: p.id, Data: p.data, CreatedAt: &created, UpdatedAt: &updated}
}

func newPolicyStore() *policyStore {
	return &policyStore{
		policies:   map[string]storedPolicy{},
		lifecycles: map[string]api.PolicyLifecycleData{},
	}
}

func (s *policyStore) list() []api.PolicyResponse {
	s.mu.RLock()
	defer s.mu.RUnlock()

	ps := slices.SortedFunc(maps.Values(s.policies), func(a, b storedPolicy) int {
		return cmp.Or(a.createdAt.Compare(b.createdAt), strings.Compare(a.id, b.id))
	})
	// Leere Liste muss als [] und nicht als null serialisiert werden.
	out := make([]api.PolicyResponse, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.response())
	}
	return out
}

func (s *policyStore) get(id string) (api.PolicyResponse, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.policies[id]
	return p.response(), ok
}

func (s *policyStore) exists(id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.policies[id]
	return ok
}

// create legt eine Policy an und liefert false, wenn die ID schon vergeben ist.
func (s *policyStore) create(id string, data map[string]any) (api.PolicyResponse, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.policies[id]; ok {
		return api.PolicyResponse{}, false
	}
	now := time.Now().UTC()
	p := storedPolicy{id: id, data: data, createdAt: now, updatedAt: now}
	s.policies[id] = p
	return p.response(), true
}

// upsert ersetzt die Daten vollständig, wie terminals. Nur created_at bleibt erhalten.
func (s *policyStore) upsert(id string, data map[string]any) api.PolicyResponse {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	p := storedPolicy{id: id, data: data, createdAt: now, updatedAt: now}
	if prev, ok := s.policies[id]; ok {
		p.createdAt = prev.createdAt
	}
	s.policies[id] = p
	return p.response()
}

func (s *policyStore) delete(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.policies[id]; !ok {
		return false
	}
	delete(s.policies, id)
	delete(s.lifecycles, id)
	return true
}

// lifecycle liefert für unbekannte Policies ein leeres Objekt, siehe GET .../lifecycle.
func (s *policyStore) lifecycle(id string) api.PolicyLifecycleData {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lifecycles[id]
}

func (s *policyStore) setLifecycle(id string, l api.PolicyLifecycleData) api.PolicyLifecycleData {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lifecycles[id] = l
	return l
}

// pyTime formatiert wie Pythons str(datetime) für naive UTC-Zeiten. In dieser Form
// liefert terminals created_at und updated_at.
func pyTime(t time.Time) string {
	t = t.UTC()
	if t.Nanosecond()/1000 == 0 {
		return t.Format(time.DateTime)
	}
	return t.Format("2006-01-02 15:04:05.000000")
}

// --- Validierung wie in terminals (routers/policy.py) ---

// Lifecycle-Einstellungen gehören nach /lifecycle, nicht in die Policy.
var lifecycleKeys = []string{"lifecycle", "reset", "reset_schedule", "reset_timezone"}

// Env-Variablen, die im Restricted-Modus (OpenShift) nicht funktionieren.
var restrictedEnvKeys = []string{
	"OPEN_TERMINAL_ALLOWED_DOMAINS",
	"OPEN_TERMINAL_NPM_PACKAGES",
	"OPEN_TERMINAL_PACKAGES",
	"OPEN_TERMINAL_PIP_PACKAGES",
}

// policyDataMap wandelt den Body in die gespeicherte Form: null-Felder fallen weg,
// unbekannte Felder bleiben erhalten.
// TODO: Hard-Caps (max CPU/Memory/Storage) und Image-Allowlist, sobald es dafür Konfiguration gibt.
func policyDataMap(d api.PolicyData) (map[string]any, error) {
	raw, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	maps.DeleteFunc(m, func(_ string, v any) bool { return v == nil })

	var forbidden []string
	for _, k := range lifecycleKeys {
		if _, ok := m[k]; ok {
			forbidden = append(forbidden, k)
		}
	}
	if len(forbidden) > 0 {
		return nil, fmt.Errorf("Lifecycle keys are not valid policy fields: %s", strings.Join(forbidden, ", "))
	}

	if truthy(m["restricted"]) {
		env, _ := m["env"].(map[string]any)
		var errs []string
		for _, k := range restrictedEnvKeys {
			if _, ok := env[k]; ok {
				errs = append(errs, k+" is not supported in restricted Kubernetes/OpenShift mode")
			}
		}
		if truthy(env["OPEN_TERMINAL_MULTI_USER"]) {
			errs = append(errs, "OPEN_TERMINAL_MULTI_USER=true is not supported in restricted Kubernetes/OpenShift mode")
		}
		if len(errs) > 0 {
			return nil, errors.New(strings.Join(errs, "; "))
		}
	}
	return m, nil
}

// truthy entspricht truthy() in terminals: bool direkt, sonst "1", "true", "yes" oder "on".
func truthy(v any) bool {
	switch v := v.(type) {
	case nil:
		return false
	case bool:
		return v
	default:
		s := strings.ToLower(strings.TrimSpace(fmt.Sprint(v)))
		return slices.Contains([]string{"1", "true", "yes", "on"}, s)
	}
}

// lifecycleData entfernt null-Felder (wie exclude_none in terminals) und prüft den Reset-Zeitplan.
func lifecycleData(l api.PolicyLifecycleData) (api.PolicyLifecycleData, bool) {
	maps.DeleteFunc(l.AdditionalProperties, func(_ string, v any) bool { return v == nil })
	if l.Reset == nil || l.Reset.Schedule == nil || *l.Reset.Schedule == "" {
		return l, true
	}
	return l, validSchedule(*l.Reset.Schedule)
}

var (
	cronAliases = []string{"@yearly", "@annually", "@monthly", "@weekly", "@daily", "@midnight", "@hourly"}
	cronField   = regexp.MustCompile(`^[0-9A-Za-z*?/,#-]+$`)
	// Formate, die Pythons datetime.fromisoformat für einmalige Resets akzeptiert.
	isoLayouts = []string{
		time.RFC3339Nano,
		"2006-01-02T15:04:05.999999999",
		"2006-01-02T15:04Z07:00",
		"2006-01-02T15:04",
		time.DateOnly,
	}
)

// validSchedule akzeptiert Cron-Ausdrücke mit 5 bis 7 Feldern, Cron-Aliase und einmalige
// ISO-8601-Zeitpunkte. Cron-Felder werden nur syntaktisch geprüft, nicht auf Wertebereiche.
func validSchedule(s string) bool {
	s = strings.TrimSpace(s)
	if slices.Contains(cronAliases, s) {
		return true
	}
	// terminals behandelt alles mit Leerzeichen als Cron, also auch "2026-12-24 18:00".
	if fields := strings.Fields(s); len(fields) > 1 {
		if len(fields) < 5 || len(fields) > 7 {
			return false
		}
		for _, f := range fields {
			if !cronField.MatchString(f) {
				return false
			}
		}
		return true
	}
	for _, layout := range isoLayouts {
		if _, err := time.Parse(layout, s); err == nil {
			return true
		}
	}
	return false
}
