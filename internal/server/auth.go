package server

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/gorilla/mux"
)

// WebSocket-Routen authentifizieren über die erste Nachricht statt per Header.
// Normale HTTP-Requests auf denselben Pfad brauchen den Bearer-Key.
var websocketRoutes = map[string]bool{
	"/api/terminals/{session_id}":               true,
	"/p/{policy_id}/api/terminals/{session_id}": true,
}

// authMiddleware setzt die Security-Schemes aus openapi.json um:
//   - TerminalsAdminKey für /api/v1/*: Requests mit X-User-Id sind User-Requests und
//     dürfen nicht an die Admin-API (403).
//   - TerminalsBearer für alles andere. Umgesetzt ist nur der statische Key, nicht der
//     JWT-Modus von terminals (TERMINALS_OPEN_WEBUI_URL).
//
// Ohne konfigurierten Key wird anders als in terminals alles abgelehnt.
func authMiddleware(apiKey string) mux.MiddlewareFunc {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tpl := routeTemplate(r)
			if tpl == "/health" || websocketRoutes[tpl] && isWebSocket(r) {
				next.ServeHTTP(w, r)
				return
			}

			admin := strings.HasPrefix(tpl, "/api/v1/")
			if _, userScoped := r.Header["X-User-Id"]; admin && userScoped {
				writeError(w, http.StatusForbidden, "User-scoped requests cannot access admin APIs")
				return
			}

			header := r.Header.Get("Authorization")
			if !validBearer(header, apiKey) {
				detail := "Invalid API key"
				switch {
				case header == "":
					detail = "Missing Authorization header"
				case admin:
					detail = "Invalid admin API key"
				}
				w.Header().Set("WWW-Authenticate", `Bearer realm="chatbox"`)
				writeError(w, http.StatusUnauthorized, detail)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func routeTemplate(r *http.Request) string {
	if route := mux.CurrentRoute(r); route != nil {
		if tpl, err := route.GetPathTemplate(); err == nil {
			return tpl
		}
	}
	return ""
}

func isWebSocket(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

func validBearer(header, apiKey string) bool {
	// Ohne konfigurierten Key würde "Bearer " sonst als gültig durchgehen.
	if apiKey == "" {
		return false
	}
	scheme, token, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(token)), []byte(apiKey)) == 1
}
