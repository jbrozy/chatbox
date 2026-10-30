package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/jbrozy/chatbox/internal/api"
	"github.com/jbrozy/chatbox/internal/server"
	"github.com/jbrozy/chatbox/internal/terminal"
)

func main() {
	port := envOr("PORT", "3000")
	apiKey := envOr("API_KEY", "dummy")

	// `chatbox healthcheck` für Docker-HEALTHCHECK: Im distroless-Image gibt es kein curl.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		if err := healthcheck("http://127.0.0.1:" + port + "/health"); err != nil {
			fmt.Fprintln(os.Stderr, "unhealthy:", err)
			os.Exit(1)
		}
		return
	}

	setupLogger()
	if apiKey == "dummy" {
		slog.Warn("API_KEY ist der unsichere Default \"dummy\"")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	backend := newBackend(ctx)
	if m, ok := backend.(*terminal.Manager); ok {
		go m.Run(ctx)
	}

	addr := ":" + port
	srv := server.New(addr, apiKey, backend)

	go func() {
		slog.Info("listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server error", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()

	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown error", "err", err)
		os.Exit(1)
	}
}

// newBackend wählt per TERMINAL_BACKEND, wo die Terminals laufen. Ist die Plattform nicht
// erreichbar, läuft chatbox weiter, aber ohne Terminals.
func newBackend(ctx context.Context) server.Backend {
	cfg := terminal.Config{
		Image:              envOr("TERMINAL_IMAGE", "ghcr.io/open-webui/open-terminal:latest"),
		IdleTimeoutMinutes: envInt("TERMINAL_IDLE_TIMEOUT_MINUTES", 30),
		// Muss unter dem 300-s-Timeout liegen, mit dem Open WebUI auf Antworten wartet.
		ProvisionTimeout: time.Duration(envInt("TERMINAL_PROVISION_TIMEOUT_SECONDS", 120)) * time.Second,
		Namespace:        envOr("TERMINAL_NAMESPACE", "chatbox-terminals"),
		StorageClass:     os.Getenv("TERMINAL_STORAGE_CLASS"),
		DockerNetwork:    os.Getenv("TERMINAL_DOCKER_NETWORK"),
	}

	switch b := envOr("TERMINAL_BACKEND", "kubernetes"); b {
	case "kubernetes":
		return newKubernetesBackend(cfg)
	case "docker":
		m, err := terminal.NewDocker(ctx, os.Getenv("DOCKER_HOST"), cfg)
		if err != nil {
			return unavailable(cfg, "docker", err)
		}
		slog.Info("docker backend", "network", cfg.DockerNetwork, "image", cfg.Image)
		return m
	default:
		return unavailable(cfg, b, fmt.Errorf("unbekanntes TERMINAL_BACKEND %q, erlaubt: kubernetes, docker", b))
	}
}

func unavailable(cfg terminal.Config, backend string, err error) server.Backend {
	slog.Warn("Terminal-Backend nicht verfügbar, Terminals sind deaktiviert", "backend", backend, "err", err)
	cfg.Backend = backend
	return terminal.Unavailable{Err: fmt.Errorf("%w: %v", terminal.ErrUnavailable, err), Cfg: cfg}
}

// newKubernetesBackend verbindet sich im Cluster über den Service-Account, sonst über
// KUBECONFIG bzw. ~/.kube/config.
func newKubernetesBackend(cfg terminal.Config) server.Backend {
	restCfg, err := rest.InClusterConfig()
	if err != nil {
		rules := clientcmd.NewDefaultClientConfigLoadingRules()
		restCfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
	}
	var client kubernetes.Interface
	if err == nil {
		client, err = kubernetes.NewForConfig(restCfg)
	}
	if err != nil {
		return unavailable(cfg, "kubernetes", err)
	}
	slog.Info("kubernetes backend", "api", restCfg.Host, "namespace", cfg.Namespace, "image", cfg.Image)
	return terminal.NewKubernetes(client, cfg)
}

// setupLogger konfiguriert slog über LOG_LEVEL (debug|info|warn|error) und LOG_FORMAT (text|json).
func setupLogger() {
	var level slog.Level
	if err := level.UnmarshalText([]byte(envOr("LOG_LEVEL", "info"))); err != nil {
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}

	var h slog.Handler
	if envOr("LOG_FORMAT", "text") == "json" {
		h = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		h = slog.NewTextHandler(os.Stdout, opts)
	}
	slog.SetDefault(slog.New(h))
}

func healthcheck(url string) error {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	var health api.HealthOpenTerminal
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		return fmt.Errorf("invalid body: %w", err)
	}
	if health.Status != api.HealthOpenTerminalStatusOk {
		return fmt.Errorf("status %q", health.Status)
	}
	return nil
}

func envInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		slog.Warn("ungültige Zahl, nutze Default", "var", key, "value", v, "default", fallback)
		return fallback
	}
	return n
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
