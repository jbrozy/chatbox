package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/jbrozy/chatbox/internal/api"
)

const (
	defaultPolicy  = "default"
	defaultContext = "default"
	maxBodyBytes   = 1 << 20
)

// Handler implementiert api.ServerInterface.
type Handler struct {
	policies *policyStore
}

var _ api.ServerInterface = (*Handler)(nil)

func NewHandler() *Handler {
	return &Handler{policies: newPolicyStore()}
}

// --- System ---

// GetHealth antwortet wie Open Terminal, damit Probes beide Dienste gleich prüfen können.
func (h *Handler) GetHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, api.HealthOpenTerminal{Status: api.HealthOpenTerminalStatusOk})
}

// --- Discovery ---

func (h *Handler) GetOpenapiJson(w http.ResponseWriter, r *http.Request) {
	// TODO: Spec vom Pod des Pseudo-Users "system" holen, security entfernen, 5 min pro Policy cachen
	notImplemented(w)
}

func (h *Handler) GetPPolicyIdOpenapiJson(w http.ResponseWriter, r *http.Request, policyId string) {
	if !h.policies.exists(policyId) {
		writePolicyNotFound(w, policyId)
		return
	}
	notImplemented(w)
}

// /api/config, /system und /info sind Open-Terminal-Endpunkte und gehen an den Pod.

func (h *Handler) GetApiConfig(w http.ResponseWriter, r *http.Request, params api.GetApiConfigParams) {
	h.proxy(w, r, r.URL.Path)
}

func (h *Handler) GetSystem(w http.ResponseWriter, r *http.Request, params api.GetSystemParams) {
	h.proxy(w, r, r.URL.Path)
}

func (h *Handler) GetInfo(w http.ResponseWriter, r *http.Request) {
	h.proxy(w, r, r.URL.Path)
}

// --- Policies ---

func (h *Handler) GetApiV1Policies(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.policies.list())
}

func (h *Handler) PostApiV1Policies(w http.ResponseWriter, r *http.Request) {
	var body api.PolicyCreate
	if !decodeBody(w, r, &body) {
		return
	}
	if body.Id == "" {
		writeValidationError(w, "Field required", "missing", "body", "id")
		return
	}
	var data api.PolicyData
	if body.Data != nil {
		data = *body.Data
	}
	m, err := policyDataMap(data)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	p, ok := h.policies.create(body.Id, m)
	if !ok {
		writeError(w, http.StatusConflict, fmt.Sprintf("Policy '%s' already exists", body.Id))
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (h *Handler) GetApiV1PoliciesPolicyId(w http.ResponseWriter, r *http.Request, policyId string) {
	p, ok := h.policies.get(policyId)
	if !ok {
		writePolicyNotFound(w, policyId)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (h *Handler) PutApiV1PoliciesPolicyId(w http.ResponseWriter, r *http.Request, policyId string) {
	var data api.PolicyData
	if !decodeBody(w, r, &data) {
		return
	}
	m, err := policyDataMap(data)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// TODO: Policy- und Spec-Cache invalidieren, sobald es sie gibt
	writeJSON(w, http.StatusOK, h.policies.upsert(policyId, m))
}

func (h *Handler) DeleteApiV1PoliciesPolicyId(w http.ResponseWriter, r *http.Request, policyId string) {
	if !h.policies.delete(policyId) {
		writePolicyNotFound(w, policyId)
		return
	}
	writeJSON(w, http.StatusOK, api.OkResponse{Ok: api.OkResponseOkTrue})
}

func (h *Handler) GetApiV1PoliciesPolicyIdLifecycle(w http.ResponseWriter, r *http.Request, policyId string) {
	writeJSON(w, http.StatusOK, api.PolicyLifecycleResponse{PolicyId: policyId, Data: h.policies.lifecycle(policyId)})
}

func (h *Handler) PutApiV1PoliciesPolicyIdLifecycle(w http.ResponseWriter, r *http.Request, policyId string) {
	var body api.PolicyLifecycleData
	if !decodeBody(w, r, &body) {
		return
	}
	l, ok := lifecycleData(body)
	if !ok {
		writeError(w, http.StatusBadRequest, "Invalid lifecycle configuration")
		return
	}
	writeJSON(w, http.StatusOK, api.PolicyLifecycleResponse{PolicyId: policyId, Data: h.policies.setLifecycle(policyId, l)})
}

// --- Instanzen ---

func (h *Handler) GetApiV1Status(w http.ResponseWriter, r *http.Request) {
	notImplemented(w)
}

func (h *Handler) GetApiV1Terminals(w http.ResponseWriter, r *http.Request) {
	notImplemented(w)
}

func (h *Handler) PostApiV1TerminalsStop(w http.ResponseWriter, r *http.Request) {
	notImplemented(w)
}

func (h *Handler) PostApiV1TerminalsRefresh(w http.ResponseWriter, r *http.Request) {
	notImplemented(w)
}

// --- Interaktive Terminals ---

func (h *Handler) PostApiTerminals(w http.ResponseWriter, r *http.Request, params api.PostApiTerminalsParams) {
	h.proxy(w, r, r.URL.Path)
}

func (h *Handler) GetApiTerminals(w http.ResponseWriter, r *http.Request, params api.GetApiTerminalsParams) {
	h.proxy(w, r, r.URL.Path)
}

func (h *Handler) DeleteApiTerminalsSessionId(w http.ResponseWriter, r *http.Request, sessionId string, params api.DeleteApiTerminalsSessionIdParams) {
	h.proxy(w, r, r.URL.Path)
}

// GetApiTerminalsSessionId ist je nach Upgrade-Header WebSocket oder normaler Proxy-Request.
func (h *Handler) GetApiTerminalsSessionId(w http.ResponseWriter, r *http.Request, sessionId string, params api.GetApiTerminalsSessionIdParams) {
	if !isWebSocket(r) {
		h.proxy(w, r, r.URL.Path)
		return
	}
	h.terminalWebSocket(w, r, defaultPolicy, sessionId)
}

func (h *Handler) GetPPolicyIdApiTerminalsSessionId(w http.ResponseWriter, r *http.Request, policyId string, sessionId string, params api.GetPPolicyIdApiTerminalsSessionIdParams) {
	if !isWebSocket(r) {
		h.policyProxy(w, r, policyId, "/api/terminals/"+sessionId)
		return
	}
	h.terminalWebSocket(w, r, policyId, sessionId)
}

func (h *Handler) terminalWebSocket(w http.ResponseWriter, r *http.Request, policyID, sessionID string) {
	// TODO: WebSocket terminieren, Auth-Nachricht prüfen/austauschen, Frames zum Pod kopieren
	notImplemented(w)
}

// --- Proxy ---

func (h *Handler) GetPPolicyIdPath(w http.ResponseWriter, r *http.Request, policyId string, path string, params api.GetPPolicyIdPathParams) {
	h.policyProxy(w, r, policyId, "/"+path)
}

func (h *Handler) PostPPolicyIdPath(w http.ResponseWriter, r *http.Request, policyId string, path string, params api.PostPPolicyIdPathParams) {
	h.policyProxy(w, r, policyId, "/"+path)
}

func (h *Handler) PutPPolicyIdPath(w http.ResponseWriter, r *http.Request, policyId string, path string, params api.PutPPolicyIdPathParams) {
	h.policyProxy(w, r, policyId, "/"+path)
}

func (h *Handler) PatchPPolicyIdPath(w http.ResponseWriter, r *http.Request, policyId string, path string, params api.PatchPPolicyIdPathParams) {
	h.policyProxy(w, r, policyId, "/"+path)
}

func (h *Handler) DeletePPolicyIdPath(w http.ResponseWriter, r *http.Request, policyId string, path string, params api.DeletePPolicyIdPathParams) {
	h.policyProxy(w, r, policyId, "/"+path)
}

func (h *Handler) HeadPPolicyIdPath(w http.ResponseWriter, r *http.Request, policyId string, path string, params api.HeadPPolicyIdPathParams) {
	h.policyProxy(w, r, policyId, "/"+path)
}

func (h *Handler) OptionsPPolicyIdPath(w http.ResponseWriter, r *http.Request, policyId string, path string, params api.OptionsPPolicyIdPathParams) {
	h.policyProxy(w, r, policyId, "/"+path)
}

func (h *Handler) GetPath(w http.ResponseWriter, r *http.Request, path string, params api.GetPathParams) {
	h.proxy(w, r, "/"+path)
}

func (h *Handler) PostPath(w http.ResponseWriter, r *http.Request, path string, params api.PostPathParams) {
	h.proxy(w, r, "/"+path)
}

func (h *Handler) PutPath(w http.ResponseWriter, r *http.Request, path string, params api.PutPathParams) {
	h.proxy(w, r, "/"+path)
}

func (h *Handler) PatchPath(w http.ResponseWriter, r *http.Request, path string, params api.PatchPathParams) {
	h.proxy(w, r, "/"+path)
}

func (h *Handler) DeletePath(w http.ResponseWriter, r *http.Request, path string, params api.DeletePathParams) {
	h.proxy(w, r, "/"+path)
}

func (h *Handler) HeadPath(w http.ResponseWriter, r *http.Request, path string, params api.HeadPathParams) {
	h.proxy(w, r, "/"+path)
}

func (h *Handler) OptionsPath(w http.ResponseWriter, r *http.Request, path string, params api.OptionsPathParams) {
	h.proxy(w, r, "/"+path)
}

// proxy reicht ohne /p/-Prefix an den Pod der Default-Policy weiter.
func (h *Handler) proxy(w http.ResponseWriter, r *http.Request, path string) {
	if !requireUserID(w, r) {
		return
	}
	h.forward(w, r, defaultPolicy, path)
}

// policyProxy reicht an den Pod einer gespeicherten Policy weiter (/p/{policy_id}/...).
func (h *Handler) policyProxy(w http.ResponseWriter, r *http.Request, policyID, path string) {
	if !requireUserID(w, r) {
		return
	}
	if !h.policies.exists(policyID) {
		writePolicyNotFound(w, policyID)
		return
	}
	h.forward(w, r, policyID, path)
}

// forward ist der gemeinsame Einstieg für alle Requests, die an den Pod des Users gehen.
func (h *Handler) forward(w http.ResponseWriter, r *http.Request, policyID, path string) {
	if _, err := contextID(r.Header.Get("X-Terminal-Context-Id")); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// TODO: Workspace finden/anlegen, Request umschreiben und streamen
	notImplemented(w)
}

// requireUserID prüft X-User-Id für Requests, die nicht über die generierten Parameter laufen.
func requireUserID(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("X-User-Id") == "" {
		writeValidationError(w, "Field required", "missing", "header", "X-User-Id")
		return false
	}
	return true
}

// contextID normalisiert X-Terminal-Context-Id wie normalize_context_id in terminals.
func contextID(v string) (string, error) {
	id := strings.TrimSpace(v)
	switch {
	case id == "":
		return defaultContext, nil
	case utf8.RuneCountInString(id) > 256:
		return "", errors.New("context_id is too long")
	case strings.ContainsFunc(id, func(c rune) bool { return c < 32 }):
		return "", errors.New("context_id contains control characters")
	case id != defaultContext && !strings.HasPrefix(id, "chat:") && !strings.HasPrefix(id, "automation:"):
		return "", errors.New("only chat and automation contexts are supported")
	}
	return id, nil
}

// --- Helpers ---

// decodeBody liest einen JSON-Body. Unbekannte Felder sind erlaubt; Fehler beantwortet
// es wie FastAPI mit 422.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(v); err != nil {
		writeValidationError(w, err.Error(), "json_invalid", "body")
		return false
	}
	return true
}

// writeParamError beantwortet Fehler der generierten Parameterprüfung wie FastAPI mit 422.
func writeParamError(w http.ResponseWriter, r *http.Request, err error) {
	slog.Warn("invalid request", "method", r.Method, "path", r.URL.Path, "err", err)

	var header *api.RequiredHeaderError
	var param *api.RequiredParamError
	switch {
	case errors.As(err, &header):
		writeValidationError(w, "Field required", "missing", "header", header.ParamName)
	case errors.As(err, &param):
		writeValidationError(w, "Field required", "missing", "query", param.ParamName)
	default:
		writeValidationError(w, err.Error(), "value_error")
	}
}

func writeValidationError(w http.ResponseWriter, msg, typ string, loc ...string) {
	items := make([]api.ValidationError_Loc_Item, len(loc))
	for i, l := range loc {
		_ = items[i].FromValidationErrorLoc0(l)
	}
	detail := []api.ValidationError{{Loc: items, Msg: msg, Type: typ}}
	writeJSON(w, http.StatusUnprocessableEntity, api.HTTPValidationError{Detail: &detail})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, detail string) {
	writeJSON(w, status, api.HTTPError{Detail: detail})
}

func writePolicyNotFound(w http.ResponseWriter, policyID string) {
	writeError(w, http.StatusNotFound, fmt.Sprintf("Policy '%s' not found", policyID))
}

func notImplemented(w http.ResponseWriter) {
	writeError(w, http.StatusNotImplemented, "Not Implemented")
}
