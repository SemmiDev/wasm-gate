// WasmGate server entrypoint.
//
// Startup sequence:
//  1. Parse config from environment variables
//  2. Create registry (re-indexes existing plugins from disk)
//  3. Create executor (initializes wazero runtime)
//  4. Wire up HTTP handlers with middleware stack
//  5. Listen for OS signals and shut down gracefully
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	chiMiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/semmidev/wasmgate/internal/api"
	"github.com/semmidev/wasmgate/internal/executor"
	mw "github.com/semmidev/wasmgate/internal/middleware"
	"github.com/semmidev/wasmgate/internal/registry"
)

func main() {
	// ── Config from environment ──────────────────────────────────────────────
	addr := envOr("WASMGATE_ADDR", ":8080")
	storageDir := envOr("WASMGATE_STORAGE_DIR", "./data/plugins")
	logLevel := envOr("WASMGATE_LOG_LEVEL", "info")

	// ── Logger ───────────────────────────────────────────────────────────────
	// slog is Go 1.21's built-in structured logger.
	// JSON handler in production lets tools like Loki/Datadog parse fields.
	// Text handler locally is human-readable.
	var level slog.Level
	level.UnmarshalText([]byte(logLevel))

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	logger.Info("starting WasmGate",
		"addr", addr,
		"storage_dir", storageDir,
	)

	// ── Registry ─────────────────────────────────────────────────────────────
	reg, err := registry.New(storageDir)
	if err != nil {
		logger.Error("initializing registry", "err", err)
		os.Exit(1)
	}
	logger.Info("registry ready", "plugins_on_disk", len(reg.List()))

	// ── Executor ─────────────────────────────────────────────────────────────
	exec := executor.New()
	defer exec.Close(context.Background())

	// Precompile all plugins that were recovered from disk.
	// This avoids a latency spike on the first invocation after restart.
	go func() {
		for _, p := range reg.List() {
			wasmBytes, err := reg.ReadWasm(p.Name)
			if err != nil {
				logger.Warn("reading plugin for precompile", "plugin", p.Name, "err", err)
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			if err := exec.Precompile(ctx, p.Name, wasmBytes); err != nil {
				logger.Warn("precompile failed for recovered plugin", "plugin", p.Name, "err", err)
			} else {
				logger.Info("precompiled recovered plugin", "plugin", p.Name)
			}
			cancel()
		}
	}()

	// ── HTTP server ──────────────────────────────────────────────────────────
	// Middleware stack (applied outermost-first):
	//   1. Recovery   — catch panics, prevent server crash
	//   2. Logger     — structured request logs
	//   3. CORS       — allow cross-origin requests (dev-friendly)
	//   4. MaxBody    — reject huge request bodies early
	//   5. chi.RequestID, chi.RealIP — standard chi middleware
	apiHandler := api.New(reg, exec, logger)

	router := chi.NewRouter()
	router.Use(
		mw.Recovery(logger),
		mw.Logger(logger),
		mw.CORS,
		mw.MaxBodySize(10<<20), // 10 MB
		chiMiddleware.RequestID,
		chiMiddleware.RealIP,
	)
	router.Mount("/", apiHandler.Routes())

	srv := &http.Server{
		Addr:         addr,
		Handler:      router,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 60 * time.Second, // generous: long-running plugins need time
		IdleTimeout:  120 * time.Second,
	}

	// ── Graceful shutdown ─────────────────────────────────────────────────────
	// Listen for SIGINT/SIGTERM (sent by Docker, k8s, and Ctrl+C).
	// When received, stop accepting new connections and wait up to 30s
	// for in-flight requests to complete before forcefully closing.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	serverErr := make(chan error, 1)
	go func() {
		logger.Info("server listening", "addr", addr)
		serverErr <- srv.ListenAndServe()
	}()

	select {
	case err := <-serverErr:
		if err != nil && err != http.ErrServerClosed {
			logger.Error("server error", "err", err)
			os.Exit(1)
		}
	case sig := <-quit:
		logger.Info("received shutdown signal", "signal", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			logger.Error("graceful shutdown failed", "err", err)
		}
		logger.Info("server stopped")
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
