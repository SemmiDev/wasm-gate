// Package api wires together the registry and executor into HTTP handlers.
// Each handler follows the same pattern: decode request → call domain logic
// → encode response. There is zero business logic here — that lives in
// registry and executor packages.
//
// API surface:
//
//	POST   /plugins/{name}         Register/update a plugin (multipart or raw binary)
//	GET    /plugins                List all registered plugins
//	GET    /plugins/{name}         Get metadata for a specific plugin
//	DELETE /plugins/{name}         Unregister a plugin
//	POST   /invoke/{name}          Invoke a plugin with a JSON payload
//	GET    /health                 Health check (for load balancers / k8s probes)
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/semmidev/wasmgate/internal/executor"
	"github.com/semmidev/wasmgate/internal/registry"
)

// Handler holds the dependencies for all HTTP handlers.
// Passing dependencies via a struct (rather than globals) makes the
// handlers trivially testable — inject a mock registry and you're done.
type Handler struct {
	registry *registry.Registry
	executor *executor.Executor
	logger   *slog.Logger
}

// New creates a Handler and returns a chi.Router with all routes registered.
func New(reg *registry.Registry, exec *executor.Executor, logger *slog.Logger) *Handler {
	return &Handler{
		registry: reg,
		executor: exec,
		logger:   logger,
	}
}

// Routes returns the fully configured router.
func (h *Handler) Routes() http.Handler {
	r := chi.NewRouter()

	r.Get("/health", h.healthCheck)

	r.Route("/plugins", func(r chi.Router) {
		r.Get("/", h.listPlugins)
		r.Post("/{name}", h.registerPlugin)
		r.Get("/{name}", h.getPlugin)
		r.Delete("/{name}", h.deletePlugin)
	})

	r.Post("/invoke/{name}", h.invokePlugin)

	return r
}

// ─── Handler: Health check ────────────────────────────────────────────────────

func (h *Handler) healthCheck(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "ok",
		"timestamp": time.Now().UTC(),
	})
}

// ─── Handler: Register plugin ─────────────────────────────────────────────────

// registerPlugin accepts a WASM binary as the request body (Content-Type: application/wasm)
// plus plugin metadata passed as HTTP headers. Using headers for metadata (rather than
// multipart) keeps the upload simple — a single curl command with no form encoding:
//
//	curl -X POST http://localhost:8080/plugins/my-plugin \
//	  -H "Content-Type: application/wasm" \
//	  -H "X-Plugin-Description: Transforms JSON keys to uppercase" \
//	  -H "X-Plugin-Version: 1.0.0" \
//	  --data-binary @plugin.wasm
func (h *Handler) registerPlugin(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")

	// Read metadata from headers — simple and curl-friendly
	description := r.Header.Get("X-Plugin-Description")
	version := r.Header.Get("X-Plugin-Version")
	if version == "" {
		version = "1.0.0"
	}
	author := r.Header.Get("X-Plugin-Author")

	p, err := h.registry.Register(name, description, version, author, r.Body)
	if err != nil {
		if strings.Contains(err.Error(), "invalid character") {
			writeError(w, http.StatusBadRequest, err.Error())
		} else {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("registering plugin: %s", err))
		}
		return
	}

	// Precompile synchronously — pays compile cost upfront (~100-300ms for Go WASM).
	// Non-fatal: a failure here just means first invoke will recompile lazily.
	if wasmBytes, rerr := h.registry.ReadWasm(p.Name); rerr == nil {
		pCtx, pCancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer pCancel()
		if cerr := h.executor.Precompile(pCtx, p.Name, wasmBytes); cerr != nil {
			h.logger.Warn("precompile failed (will retry on first invoke)", "plugin", p.Name, "err", cerr)
		} else {
			h.logger.Info("plugin precompiled", "plugin", p.Name, "size_bytes", p.SizeBytes)
		}
	}

	h.logger.Info("plugin registered", "name", p.Name, "size_bytes", p.SizeBytes)
	writeJSON(w, http.StatusCreated, map[string]any{
		"plugin":  p,
		"message": fmt.Sprintf("plugin %q registered successfully", name),
	})
}

// ─── Handler: List plugins ────────────────────────────────────────────────────

func (h *Handler) listPlugins(w http.ResponseWriter, r *http.Request) {
	plugins := h.registry.List()
	writeJSON(w, http.StatusOK, map[string]any{
		"plugins": plugins,
		"count":   len(plugins),
	})
}

// ─── Handler: Get plugin metadata ─────────────────────────────────────────────

func (h *Handler) getPlugin(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	p, err := h.registry.Get(name)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// ─── Handler: Delete plugin ───────────────────────────────────────────────────

func (h *Handler) deletePlugin(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if err := h.registry.Delete(name); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	h.executor.Evict(r.Context(), name)
	h.logger.Info("plugin deleted", "name", name)
	writeJSON(w, http.StatusOK, map[string]string{
		"message": fmt.Sprintf("plugin %q deleted", name),
	})
}

// ─── Handler: Invoke plugin ───────────────────────────────────────────────────

// invokePlugin is the most important endpoint. It calls the WASM plugin
// with the JSON body as input and returns the plugin's output.
//
// The client controls the timeout via the X-Timeout-Ms header.
// If not set, the executor's default (5s) applies.
//
// Request:  POST /invoke/{name}  body: any JSON
// Response: { "output": <plugin output>, "elapsed_ms": N, "memory_bytes": N }
func (h *Handler) invokePlugin(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")

	// Read the full request body — this is the input payload for the plugin
	payload, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "reading request body: "+err.Error())
		return
	}
	if len(payload) == 0 {
		writeError(w, http.StatusBadRequest, "request body is empty; plugin requires a JSON payload")
		return
	}

	// Optional per-request timeout override
	var timeout time.Duration
	if ms := r.Header.Get("X-Timeout-Ms"); ms != "" {
		var msVal int64
		if _, err := fmt.Sscan(ms, &msVal); err == nil && msVal > 0 {
			timeout = time.Duration(msVal) * time.Millisecond
		}
	}

	// Fetch the WASM binary — executor needs it for lazy compilation fallback
	wasmBytes, err := h.registry.ReadWasm(name)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	result, err := h.executor.Invoke(r.Context(), name, wasmBytes, executor.InvokeRequest{
		Payload: payload,
		Timeout: timeout,
	})
	if err != nil {
		if strings.Contains(err.Error(), "timed out") {
			writeError(w, http.StatusGatewayTimeout, err.Error())
		} else if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, err.Error())
		} else {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
		}
		return
	}

	h.logger.Info("plugin invoked",
		"name", name,
		"input_bytes", len(payload),
		"output_bytes", len(result.Output),
		"elapsed_ms", result.ElapsedMs,
	)

	// Return the raw output bytes as a JSON string field.
	// The output itself may or may not be valid JSON — that's up to the plugin.
	// We wrap it to keep the response envelope consistent.
	writeJSON(w, http.StatusOK, map[string]any{
		"plugin":       name,
		"output":       json.RawMessage(result.Output),
		"elapsed_ms":   result.ElapsedMs,
		"memory_bytes": result.MemoryBytes,
	})
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// At this point we've already written the header, so we can't
		// change the status code. Just log it.
		slog.Error("encoding JSON response", "err", err)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
