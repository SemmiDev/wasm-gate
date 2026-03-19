// Integration test untuk WasmGate.
//
// Test ini berbeda dari unit test biasa — ia menyalakan server HTTP asli,
// mengupload WASM binary sungguhan, dan memanggil plugin via HTTP.
// Ini membuktikan bahwa seluruh stack (registry → executor → HTTP) bekerja
// sebagaimana mestinya dalam kondisi production-like.
//
// Jalankan: go test ./... -v -tags integration
package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/semmidev/wasmgate/internal/api"
	"github.com/semmidev/wasmgate/internal/executor"
	"github.com/semmidev/wasmgate/internal/registry"

	"log/slog"
)

// setupTestServer creates a real server with a temp dir for storage.
// httptest.NewServer gives us a real TCP socket — this is a true integration test.
func setupTestServer(t *testing.T) (*httptest.Server, func()) {
	t.Helper()
	storageDir := t.TempDir()

	reg, err := registry.New(storageDir)
	if err != nil {
		t.Fatalf("creating registry: %v", err)
	}

	exec := executor.New()
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))

	handler := api.New(reg, exec, logger)
	srv := httptest.NewServer(handler.Routes())

	cleanup := func() {
		srv.Close()
		exec.Close(context.Background())
	}
	return srv, cleanup
}

// wasmPath returns the absolute path to a compiled test plugin.
// Tests assume plugins are pre-built in ../../testdata/.
func wasmPath(t *testing.T, name string) string {
	t.Helper()
	// Walk up to find the project root
	path := filepath.Join("..", "..", "testdata", name+".wasm")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("test plugin %q not found at %s — run 'make plugins' first", name, path)
	}
	return path
}

// ─── Test: Health check ───────────────────────────────────────────────────────

func TestHealthCheck(t *testing.T) {
	srv, cleanup := setupTestServer(t)
	defer cleanup()

	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}

	var body map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&body)
	if body["status"] != "ok" {
		t.Errorf("expected status=ok, got %v", body["status"])
	}
}

// ─── Test: Plugin registration ────────────────────────────────────────────────

func TestRegisterPlugin(t *testing.T) {
	srv, cleanup := setupTestServer(t)
	defer cleanup()

	wasmFile := wasmPath(t, "json-transformer")
	wasmBytes, err := os.ReadFile(wasmFile)
	if err != nil {
		t.Fatalf("reading wasm: %v", err)
	}

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/plugins/json-transformer", bytes.NewReader(wasmBytes))
	req.Header.Set("Content-Type", "application/wasm")
	req.Header.Set("X-Plugin-Description", "Transforms JSON fields")
	req.Header.Set("X-Plugin-Version", "1.0.0")
	req.Header.Set("X-Plugin-Author", "testauthor")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /plugins: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Errorf("expected 201, got %d", resp.StatusCode)
	}

	var body map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&body)
	plugin := body["plugin"].(map[string]interface{})
	if plugin["name"] != "json-transformer" {
		t.Errorf("unexpected plugin name: %v", plugin["name"])
	}
}

// ─── Test: Full invoke cycle ──────────────────────────────────────────────────

func TestInvokeJsonTransformer(t *testing.T) {
	srv, cleanup := setupTestServer(t)
	defer cleanup()

	// Step 1: register the plugin
	wasmFile := wasmPath(t, "json-transformer")
	wasmBytes, _ := os.ReadFile(wasmFile)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/plugins/json-transformer", bytes.NewReader(wasmBytes))
	req.Header.Set("Content-Type", "application/wasm")
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("registration failed: %d", resp.StatusCode)
	}

	// Step 2: invoke with a JSON payload
	payload := `{"name":"alice","score":42,"active":true}`
	invokeReq, _ := http.NewRequest(
		http.MethodPost,
		srv.URL+"/invoke/json-transformer",
		bytes.NewBufferString(payload),
	)
	invokeReq.Header.Set("Content-Type", "application/json")

	invokeResp, err := http.DefaultClient.Do(invokeReq)
	if err != nil {
		t.Fatalf("POST /invoke: %v", err)
	}
	defer invokeResp.Body.Close()

	if invokeResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", invokeResp.StatusCode)
	}

	var result map[string]interface{}
	json.NewDecoder(invokeResp.Body).Decode(&result)

	// Validate the output from the plugin
	output := result["output"].(map[string]interface{})

	// The plugin should uppercase strings
	if output["name"] != "ALICE" {
		t.Errorf("expected name=ALICE, got %v", output["name"])
	}
	// Numbers should be doubled
	if output["score"] != float64(84) {
		t.Errorf("expected score=84, got %v", output["score"])
	}
	// Booleans should be flipped
	if output["active"] != false {
		t.Errorf("expected active=false, got %v", output["active"])
	}

	t.Logf("plugin output: %v", output)
	t.Logf("elapsed: %v ms", result["elapsed_ms"])
}

// ─── Test: Plugin not found ───────────────────────────────────────────────────

func TestInvokeNonexistentPlugin(t *testing.T) {
	srv, cleanup := setupTestServer(t)
	defer cleanup()

	resp, err := http.Post(
		srv.URL+"/invoke/does-not-exist",
		"application/json",
		bytes.NewBufferString(`{"key":"value"}`),
	)
	if err != nil {
		t.Fatalf("POST /invoke: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

// ─── Test: Invalid plugin name ────────────────────────────────────────────────

func TestRegisterInvalidName(t *testing.T) {
	srv, cleanup := setupTestServer(t)
	defer cleanup()

	req, _ := http.NewRequest(
		http.MethodPost,
		srv.URL+"/plugins/my plugin!", // spaces and ! are invalid
		bytes.NewBufferString("fakewasm"),
	)
	req.Header.Set("Content-Type", "application/wasm")

	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()

	// chi router will 404 on the path before our handler even runs — that's fine
	// The important thing is we don't accept it
	if resp.StatusCode == http.StatusCreated {
		t.Error("should not accept plugin with invalid name")
	}
}

// ─── Test: Delete plugin ──────────────────────────────────────────────────────

func TestDeletePlugin(t *testing.T) {
	srv, cleanup := setupTestServer(t)
	defer cleanup()

	// Register first
	wasmFile := wasmPath(t, "json-transformer")
	wasmBytes, _ := os.ReadFile(wasmFile)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/plugins/to-delete", bytes.NewReader(wasmBytes))
	req.Header.Set("Content-Type", "application/wasm")
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()

	// Delete
	delReq, _ := http.NewRequest(http.MethodDelete, srv.URL+"/plugins/to-delete", nil)
	delResp, err := http.DefaultClient.Do(delReq)
	if err != nil {
		t.Fatalf("DELETE /plugins: %v", err)
	}
	delResp.Body.Close()

	if delResp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 on delete, got %d", delResp.StatusCode)
	}

	// Verify it's gone
	getResp, _ := http.Get(srv.URL + "/plugins/to-delete")
	getResp.Body.Close()
	if getResp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 after delete, got %d", getResp.StatusCode)
	}
}
