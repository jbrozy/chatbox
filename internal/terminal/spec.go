package terminal

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
)

const homeDir = "/home/user"

// instanceName ist deterministisch, damit chatbox seine Instanzen nach einem Neustart
// wiederfindet. Gültig als Pod-, Container-, Secret- und Volume-Name.
func instanceName(key Key) string {
	sum := sha256.Sum256([]byte(key.UserID + "\x00" + key.PolicyID + "\x00" + key.ContextID))
	return "chatbox-" + hex.EncodeToString(sum[:])[:20]
}

type envVar struct {
	name, value string
}

// limits sind die Ressourcen-Grenzen aus der Policy, nil wenn nicht gesetzt.
type limits struct {
	cpu, memory *resource.Quantity
}

// containerEnv baut die Umgebung wie build_terminal_env in terminals: die Env der Policy
// (ohne OPEN_TERMINAL_API_KEY), dazu die Limits, damit Open Terminal sie kennt.
func containerEnv(spec Spec) ([]envVar, limits, error) {
	var env []envVar
	policyEnv, _ := spec["env"].(map[string]any)
	for _, k := range slices.Sorted(maps.Keys(policyEnv)) {
		if v := policyEnv[k]; k != "OPEN_TERMINAL_API_KEY" && v != nil {
			env = append(env, envVar{k, fmt.Sprint(v)})
		}
	}

	var l limits
	if v := str(spec, "cpu_limit"); v != "" {
		q, err := resource.ParseQuantity(v)
		if err != nil {
			return nil, l, fmt.Errorf("cpu_limit %q: %w", v, err)
		}
		l.cpu = &q
		cores := strconv.FormatFloat(float64(q.MilliValue())/1000, 'f', -1, 64)
		env = append(env, envVar{"OPEN_TERMINAL_CPU_LIMIT", v}, envVar{"OPEN_TERMINAL_CPU_COUNT", cores})
	}
	if v := str(spec, "memory_limit"); v != "" {
		q, err := resource.ParseQuantity(v)
		if err != nil {
			return nil, l, fmt.Errorf("memory_limit %q: %w", v, err)
		}
		l.memory = &q
		env = append(env, envVar{"OPEN_TERMINAL_MEMORY_LIMIT", v}, envVar{"OPEN_TERMINAL_MEMORY_BYTES", strconv.FormatInt(q.Value(), 10)})
	}
	return env, l, nil
}

// egressFiltered: Open Terminal filtert Egress selbst per iptables und braucht dafür NET_ADMIN.
func egressFiltered(spec Spec) bool {
	policyEnv, _ := spec["env"].(map[string]any)
	_, ok := policyEnv["OPEN_TERMINAL_ALLOWED_DOMAINS"]
	return ok
}

func idleTimeout(cfg Config, spec Spec) int {
	if v, ok := spec["idle_timeout_minutes"].(float64); ok {
		return int(v)
	}
	return cfg.IdleTimeoutMinutes
}

// str liest einen Policy-Wert als String; Zahlen (z. B. cpu_limit: 1) werden formatiert.
func str(spec Spec, key string) string {
	switch v := spec[key].(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(v)
	default:
		return fmt.Sprint(v)
	}
}
