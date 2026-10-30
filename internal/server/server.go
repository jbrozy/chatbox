package server

import (
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/mux"

	"github.com/jbrozy/chatbox/internal/api"
)

// New baut den HTTP-Server mit allen Routen aus openapi.yaml.
func New(addr string) *http.Server {
	h := NewHandler()

	r := mux.NewRouter()
	r.Use(loggingMiddleware)

	api.HandlerWithOptions(h, api.GorillaServerOptions{
		BaseRouter: r,
		ErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			writeError(w, http.StatusBadRequest, err.Error(), "")
		},
	})

	// Der generierte Catch-all /p/{policy_id}/{proxy_path} matcht in gorilla/mux nur ein
	// Pfadsegment. Mehrsegmentige Pfade und die Default-Policy ohne Prefix laufen hierüber.
	r.PathPrefix("/p/{policy_id}/").HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		policyID := mux.Vars(r)["policy_id"]
		h.proxy(w, r, policyID, strings.TrimPrefix(r.URL.Path, "/p/"+policyID))
	})
	r.PathPrefix("/").HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.proxy(w, r, defaultPolicy, r.URL.Path)
	})

	return &http.Server{
		Addr:        addr,
		Handler:     r,
		ReadTimeout: 10 * time.Second,
		// Kein WriteTimeout: Proxy-Antworten werden gestreamt und Open WebUI wartet bis zu 300 s.
		IdleTimeout: 60 * time.Second,
	}
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start))
	})
}
