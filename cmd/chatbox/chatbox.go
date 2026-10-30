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
	"syscall"
	"time"

	"github.com/jbrozy/chatbox/internal/api"
	"github.com/jbrozy/chatbox/internal/server"
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
		slog.Warn("API_KEY nicht gesetzt, nutze unsicheren Default \"dummy\"")
	}

	addr := ":" + port
	srv := server.New(addr, apiKey)

	go func() {
		slog.Info("listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server error", "err", err)
			os.Exit(1)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()

	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown error", "err", err)
		os.Exit(1)
	}
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

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
