package server

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/mux"

	"github.com/jbrozy/chatbox/internal/api"
)

// New baut den HTTP-Server mit allen Routen aus openapi.json.
// apiKey ist der Bearer-Key, den Open WebUI bei `auth_type: bearer` mitschickt.
// backend stellt die Open-Terminal-Instanzen bereit.
func New(addr, apiKey string, backend Backend) *http.Server {
	h := NewHandler(apiKey, backend)

	r := mux.NewRouter()
	r.Use(loggingMiddleware, authMiddleware(apiKey))

	// Docker, Load-Balancer und `curl -I` prüfen teils per HEAD. Die Route muss vor den
	// generierten stehen, sonst greift der Catch-all HEAD /{path} und verlangt X-User-Id.
	r.Path("/health").Methods(http.MethodHead).HandlerFunc(h.GetHealth)

	api.HandlerWithOptions(h, api.GorillaServerOptions{
		BaseRouter:       r,
		ErrorHandlerFunc: writeParamError,
	})

	// Die generierten Catch-alls /{path} und /p/{policy_id}/{path} matchen in gorilla/mux nur
	// ein Pfadsegment. Mehrsegmentige Pfade wie /files/list laufen hierüber.
	r.PathPrefix("/p/{policy_id}/").HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		policyID := mux.Vars(r)["policy_id"]
		h.policyProxy(w, r, policyID, strings.TrimPrefix(r.URL.Path, "/p/"+policyID))
	})
	r.PathPrefix("/").HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.proxy(w, r, r.URL.Path)
	})

	return &http.Server{
		Addr:        addr,
		Handler:     r,
		ReadTimeout: 10 * time.Second,
		// Kein WriteTimeout: Proxy-Antworten werden gestreamt und Open WebUI wartet bis zu 300 s.
		IdleTimeout: 60 * time.Second,
		ErrorLog:    slog.NewLogLogger(slog.Default().Handler(), slog.LevelWarn),
	}
}
