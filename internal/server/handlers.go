package server

import (
	"encoding/json"
	"net/http"

	"github.com/jbrozy/chatbox/internal/api"
)

const defaultPolicy = "default"

// Handler implementiert api.ServerInterface.
type Handler struct{}

var _ api.ServerInterface = (*Handler)(nil)

func NewHandler() *Handler {
	return &Handler{}
}

// --- System ---

func (h *Handler) GetHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, api.Health{Status: "ok"})
}

func (h *Handler) GetReady(w http.ResponseWriter, r *http.Request) {
	// TODO: Kubernetes-API und Policy-Cache prüfen
	writeJSON(w, http.StatusOK, api.Readiness{Status: api.Ok})
}

func (h *Handler) GetMetrics(w http.ResponseWriter, r *http.Request) {
	notImplemented(w)
}

// --- Discovery ---

func (h *Handler) GetToolSpecDefault(w http.ResponseWriter, r *http.Request) {
	h.GetToolSpecForPolicy(w, r, defaultPolicy)
}

func (h *Handler) GetToolSpecForPolicy(w http.ResponseWriter, r *http.Request, policyId api.PolicyId) {
	notImplemented(w)
}

func (h *Handler) GetServerConfigDefault(w http.ResponseWriter, r *http.Request, params api.GetServerConfigDefaultParams) {
	h.GetServerConfigForPolicy(w, r, defaultPolicy, api.GetServerConfigForPolicyParams(params))
}

func (h *Handler) GetServerConfigForPolicy(w http.ResponseWriter, r *http.Request, policyId api.PolicyId, params api.GetServerConfigForPolicyParams) {
	notImplemented(w)
}

func (h *Handler) GetSystemPromptForPolicy(w http.ResponseWriter, r *http.Request, policyId api.PolicyId, params api.GetSystemPromptForPolicyParams) {
	notImplemented(w)
}

// --- Policies ---

func (h *Handler) ListPolicies(w http.ResponseWriter, r *http.Request) {
	notImplemented(w)
}

func (h *Handler) GetPolicy(w http.ResponseWriter, r *http.Request, policyId api.PolicyId) {
	notImplemented(w)
}

func (h *Handler) UpsertPolicy(w http.ResponseWriter, r *http.Request, policyId api.PolicyId) {
	notImplemented(w)
}

func (h *Handler) DeletePolicy(w http.ResponseWriter, r *http.Request, policyId api.PolicyId) {
	notImplemented(w)
}

func (h *Handler) GetLifecycle(w http.ResponseWriter, r *http.Request, policyId api.PolicyId) {
	notImplemented(w)
}

func (h *Handler) PutLifecycle(w http.ResponseWriter, r *http.Request, policyId api.PolicyId) {
	notImplemented(w)
}

// --- Terminals ---

func (h *Handler) ListWorkspaces(w http.ResponseWriter, r *http.Request, params api.ListWorkspacesParams) {
	notImplemented(w)
}

func (h *Handler) RefreshWorkspaces(w http.ResponseWriter, r *http.Request) {
	notImplemented(w)
}

// --- Proxy ---

func (h *Handler) TerminalWebSocketDefault(w http.ResponseWriter, r *http.Request, sessionId api.SessionId, params api.TerminalWebSocketDefaultParams) {
	h.TerminalWebSocketForPolicy(w, r, defaultPolicy, sessionId, api.TerminalWebSocketForPolicyParams(params))
}

func (h *Handler) TerminalWebSocketForPolicy(w http.ResponseWriter, r *http.Request, policyId api.PolicyId, sessionId api.SessionId, params api.TerminalWebSocketForPolicyParams) {
	// TODO: WebSocket terminieren, Auth-Nachricht prüfen/austauschen, Frames zum Pod kopieren
	notImplemented(w)
}

func (h *Handler) GetPPolicyIdProxyPath(w http.ResponseWriter, r *http.Request, policyId api.PolicyId, proxyPath string, params api.GetPPolicyIdProxyPathParams) {
	h.proxy(w, r, policyId, "/"+proxyPath)
}

func (h *Handler) PostPPolicyIdProxyPath(w http.ResponseWriter, r *http.Request, policyId api.PolicyId, proxyPath string, params api.PostPPolicyIdProxyPathParams) {
	h.proxy(w, r, policyId, "/"+proxyPath)
}

func (h *Handler) PutPPolicyIdProxyPath(w http.ResponseWriter, r *http.Request, policyId api.PolicyId, proxyPath string, params api.PutPPolicyIdProxyPathParams) {
	h.proxy(w, r, policyId, "/"+proxyPath)
}

func (h *Handler) PatchPPolicyIdProxyPath(w http.ResponseWriter, r *http.Request, policyId api.PolicyId, proxyPath string, params api.PatchPPolicyIdProxyPathParams) {
	h.proxy(w, r, policyId, "/"+proxyPath)
}

func (h *Handler) DeletePPolicyIdProxyPath(w http.ResponseWriter, r *http.Request, policyId api.PolicyId, proxyPath string, params api.DeletePPolicyIdProxyPathParams) {
	h.proxy(w, r, policyId, "/"+proxyPath)
}

func (h *Handler) HeadPPolicyIdProxyPath(w http.ResponseWriter, r *http.Request, policyId api.PolicyId, proxyPath string, params api.HeadPPolicyIdProxyPathParams) {
	h.proxy(w, r, policyId, "/"+proxyPath)
}

func (h *Handler) OptionsPPolicyIdProxyPath(w http.ResponseWriter, r *http.Request, policyId api.PolicyId, proxyPath string, params api.OptionsPPolicyIdProxyPathParams) {
	h.proxy(w, r, policyId, "/"+proxyPath)
}

// proxy ist der gemeinsame Einstieg für alle Requests, die an den Pod des Users gehen.
func (h *Handler) proxy(w http.ResponseWriter, r *http.Request, policyID, path string) {
	if r.Header.Get("X-User-Id") == "" {
		writeError(w, http.StatusBadRequest, "X-User-Id fehlt", api.ErrorCodeMissingUserId)
		return
	}
	// TODO: Auth prüfen, Workspace finden/anlegen, Request umschreiben und streamen
	notImplemented(w)
}

// --- Helpers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string, code api.ErrorCode) {
	e := api.Error{Error: msg}
	if code != "" {
		e.Code = &code
	}
	writeJSON(w, status, e)
}

func notImplemented(w http.ResponseWriter) {
	writeError(w, http.StatusNotImplemented, "not implemented", "")
}
