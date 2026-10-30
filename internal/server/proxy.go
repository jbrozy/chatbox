package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"sync"
	"time"

	"github.com/jbrozy/chatbox/internal/api"
	"github.com/jbrozy/chatbox/internal/terminal"
)

// Backend stellt Open-Terminal-Instanzen bereit, implementiert von terminal.Kubernetes.
type Backend interface {
	Ensure(ctx context.Context, key terminal.Key, spec terminal.Spec) (terminal.Instance, error)
	Invalidate(key terminal.Key)
	Touch(key terminal.Key)
	Connected(key terminal.Key) (release func())
	ActiveConnections() int
	List(ctx context.Context) ([]terminal.Info, error)
	Refresh(ctx context.Context, f terminal.Filter, onlyIdle, reset bool) (terminal.RefreshResult, error)
	Defaults() terminal.Config
}

const (
	// Pseudo-User, dessen Instanz die Tool-Spec liefert (wie in terminals).
	systemUser = "system"

	specCacheTTL    = 5 * time.Minute
	specFetchTries  = 10
	specFetchDelay  = time.Second
	upstreamTimeout = 360 * time.Second
)

func newTransport() *http.Transport {
	return &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       30 * time.Second,
		ResponseHeaderTimeout: upstreamTimeout,
	}
}

// forward ist der gemeinsame Einstieg für alle Requests, die an den Pod des Users gehen.
// Ablauf wie in terminals: Context prüfen, Instanz finden oder anlegen, Auth gegen den
// Key der Instanz tauschen, Routing-Header entfernen und die Antwort ungepuffert streamen.
func (h *Handler) forward(w http.ResponseWriter, r *http.Request, policyID, path string) {
	ctxID, err := contextID(r.Header.Get("X-Terminal-Context-Id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	key := terminal.Key{UserID: r.Header.Get("X-User-Id"), PolicyID: policyID, ContextID: ctxID}
	// Open WebUI fragt /api/config ohne Context ab. Die Features hängen nur an der Policy,
	// sonst entstünde pro User eine leere Default-Instanz nur für diese Abfrage.
	if path == "/api/config" && r.Header.Get("X-Terminal-Context-Id") == "" {
		key.UserID = systemUser
	}
	spec, _ := h.policies.spec(policyID)

	inst, err := h.backend.Ensure(r.Context(), key, spec)
	if err != nil {
		slog.Error("provisioning failed", "user_id", key.UserID, "policy_id", policyID, "context_id", ctxID, "err", err)
		writeProxyError(w, http.StatusBadGateway, "Terminal instance not reachable")
		return
	}

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = inst.Addr()
			pr.Out.URL.Path = path
			pr.Out.URL.RawPath = ""
			pr.Out.Host = ""
			pr.Out.Header.Set("Authorization", "Bearer "+inst.APIKey)
			// Nur zur Auswahl der Instanz; X-Session-Id bleibt für Open Terminal erhalten.
			pr.Out.Header.Del("X-User-Id")
			pr.Out.Header.Del("X-Terminal-Context-Id")
		},
		Transport:     h.transport,
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(r.Context().Err(), context.Canceled) {
				return // Client weg, niemand liest die Antwort
			}
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				slog.Warn("upstream timeout", "pod", inst.Name, "err", err)
				writeProxyError(w, http.StatusGatewayTimeout, "Upstream response timed out")
				return
			}
			slog.Warn("upstream unreachable", "pod", inst.Name, "err", err)
			h.backend.Invalidate(key)
			writeProxyError(w, http.StatusBadGateway, "Terminal instance not reachable")
		},
	}
	proxy.ServeHTTP(w, r)
}

func writeProxyError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, api.ProxyError{Error: msg})
}

// --- Tool-Spec (/openapi.json) ---

type specCache struct {
	mu      sync.Mutex
	entries map[string]cachedSpec
}

type cachedSpec struct {
	body    []byte
	fetched time.Time
}

func (c *specCache) get(policyID string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[policyID]
	if !ok || time.Since(e.fetched) >= specCacheTTL {
		return nil, false
	}
	return e.body, true
}

func (c *specCache) put(policyID string, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[policyID] = cachedSpec{body: body, fetched: time.Now()}
}

func (c *specCache) invalidate(policyID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, policyID)
}

// toolSpec liefert die OpenAPI-Spec von Open Terminal, wie terminals: von der Instanz des
// Pseudo-Users "system" geholt, ohne Security-Angaben (Auth übernimmt der Proxy) und
// 5 Minuten pro Policy gecacht.
func (h *Handler) toolSpec(w http.ResponseWriter, r *http.Request, policyID, errMsg string) {
	if body, ok := h.specs.get(policyID); ok {
		writeRawJSON(w, body)
		return
	}
	spec, _ := h.policies.spec(policyID)
	key := terminal.Key{UserID: systemUser, PolicyID: policyID, ContextID: defaultContext}

	body, err := h.fetchSpec(r.Context(), key, spec)
	if err != nil {
		slog.Error("fetching tool spec failed", "policy_id", policyID, "err", err)
		h.backend.Invalidate(key)
		writeProxyError(w, http.StatusBadGateway, errMsg)
		return
	}
	h.specs.put(policyID, body)
	writeRawJSON(w, body)
}

func (h *Handler) fetchSpec(ctx context.Context, key terminal.Key, spec terminal.Spec) ([]byte, error) {
	inst, err := h.backend.Ensure(ctx, key, spec)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Transport: h.transport, Timeout: 30 * time.Second}

	var lastErr error
	for range specFetchTries {
		raw, err := getSpec(ctx, client, inst)
		if err == nil {
			return stripSecurity(raw)
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(h.specFetchDelay):
		}
	}
	return nil, lastErr
}

func getSpec(ctx context.Context, client *http.Client, inst terminal.Instance) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, inst.URL("http")+"/openapi.json", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+inst.APIKey)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 16<<20))
}

// stripSecurity entfernt security und securitySchemes, wie _strip_auth_from_spec in terminals.
func stripSecurity(raw []byte) ([]byte, error) {
	var spec map[string]any
	if err := json.Unmarshal(raw, &spec); err != nil {
		return nil, fmt.Errorf("ungültige Spec: %w", err)
	}
	delete(spec, "security")
	if components, ok := spec["components"].(map[string]any); ok {
		delete(components, "securitySchemes")
	}
	if paths, ok := spec["paths"].(map[string]any); ok {
		for _, item := range paths {
			ops, _ := item.(map[string]any)
			for _, op := range ops {
				if op, ok := op.(map[string]any); ok {
					delete(op, "security")
				}
			}
		}
	}
	return json.Marshal(spec)
}

func writeRawJSON(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
