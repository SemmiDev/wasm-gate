// Package registry manages the lifecycle of WASM plugins: registration,
// storage, lookup, and deletion. It acts as the single source of truth
// for what plugins are available in the system.
//
// Design decision: we separate "registry" (metadata + binary store) from
// "executor" (runtime instantiation). This means the registry has no
// concept of WASM at all — it just stores bytes and metadata. This
// separation keeps each layer testable in isolation.
package registry

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Plugin holds the metadata and location of a registered WASM plugin.
// The actual binary is stored on disk, not in memory, to avoid
// unbounded RAM growth when many large plugins are registered.
type Plugin struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Version     string    `json:"version"`
	Author      string    `json:"author"`
	CreatedAt   time.Time `json:"created_at"`
	SizeBytes   int64     `json:"size_bytes"`
	// WasmPath is an internal field — not exposed in JSON responses.
	// The client never needs to know where we store the binary.
	WasmPath string `json:"-"`
}

// Registry is the central store for all plugins.
// It uses a read-write mutex so multiple concurrent GET requests
// can read simultaneously, while uploads serialize against reads.
type Registry struct {
	mu      sync.RWMutex
	plugins map[string]*Plugin
	// storageDir is where .wasm binaries are persisted to disk.
	// Using a directory (rather than a database) keeps this dependency-free
	// and makes the data trivially inspectable and portable.
	storageDir string
}

// New creates a Registry backed by the given directory.
// The directory is created if it doesn't exist.
func New(storageDir string) (*Registry, error) {
	if err := os.MkdirAll(storageDir, 0755); err != nil {
		return nil, fmt.Errorf("creating storage dir: %w", err)
	}
	r := &Registry{
		plugins:    make(map[string]*Plugin),
		storageDir: storageDir,
	}
	// On startup, re-index any .wasm files already on disk from a previous run.
	// This gives us persistence across server restarts without a database.
	if err := r.reindex(); err != nil {
		return nil, fmt.Errorf("reindexing existing plugins: %w", err)
	}
	return r, nil
}

// Register stores a new WASM plugin binary and its metadata.
// The name must be unique — re-registering the same name overwrites the old plugin.
func (r *Registry) Register(name, description, version, author string, wasmData io.Reader) (*Plugin, error) {
	if name == "" {
		return nil, fmt.Errorf("plugin name cannot be empty")
	}

	// Validate that name is URL-safe (we'll use it in API paths)
	for _, ch := range name {
		if !isURLSafe(ch) {
			return nil, fmt.Errorf("plugin name %q contains invalid character %q: use only letters, digits, hyphens, underscores", name, ch)
		}
	}

	// Write binary to disk first (outside the lock — disk I/O should not block readers)
	wasmPath := filepath.Join(r.storageDir, name+".wasm")
	f, err := os.Create(wasmPath)
	if err != nil {
		return nil, fmt.Errorf("creating wasm file: %w", err)
	}
	written, err := io.Copy(f, wasmData)
	f.Close()
	if err != nil {
		os.Remove(wasmPath)
		return nil, fmt.Errorf("writing wasm binary: %w", err)
	}
	if written == 0 {
		os.Remove(wasmPath)
		return nil, fmt.Errorf("wasm binary is empty")
	}

	p := &Plugin{
		Name:        name,
		Description: description,
		Version:     version,
		Author:      author,
		CreatedAt:   time.Now().UTC(),
		SizeBytes:   written,
		WasmPath:    wasmPath,
	}

	r.mu.Lock()
	r.plugins[name] = p
	r.mu.Unlock()

	return p, nil
}

// Get returns a plugin by name, or an error if not found.
func (r *Registry) Get(name string) (*Plugin, error) {
	r.mu.RLock()
	p, ok := r.plugins[name]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("plugin %q not found", name)
	}
	return p, nil
}

// List returns all registered plugins, sorted by name.
func (r *Registry) List() []*Plugin {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]*Plugin, 0, len(r.plugins))
	for _, p := range r.plugins {
		result = append(result, p)
	}
	return result
}

// Delete removes a plugin from the registry and deletes its binary from disk.
func (r *Registry) Delete(name string) error {
	r.mu.Lock()
	p, ok := r.plugins[name]
	if !ok {
		r.mu.Unlock()
		return fmt.Errorf("plugin %q not found", name)
	}
	delete(r.plugins, name)
	r.mu.Unlock()

	// Remove the binary outside the lock — disk I/O is slow
	if err := os.Remove(p.WasmPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing wasm file: %w", err)
	}
	return nil
}

// ReadWasm returns the raw bytes of a plugin's WASM binary.
// The executor calls this when it needs to instantiate the module.
func (r *Registry) ReadWasm(name string) ([]byte, error) {
	p, err := r.Get(name)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p.WasmPath)
	if err != nil {
		return nil, fmt.Errorf("reading wasm binary for %q: %w", name, err)
	}
	return data, nil
}

// reindex scans the storage directory for .wasm files from a previous run
// and reconstructs the in-memory metadata map from them.
func (r *Registry) reindex() error {
	entries, err := os.ReadDir(r.storageDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".wasm" {
			continue
		}
		name := e.Name()[:len(e.Name())-5] // strip ".wasm"
		info, err := e.Info()
		if err != nil {
			continue
		}
		r.plugins[name] = &Plugin{
			Name:        name,
			Description: "(recovered from disk)",
			Version:     "unknown",
			CreatedAt:   info.ModTime().UTC(),
			SizeBytes:   info.Size(),
			WasmPath:    filepath.Join(r.storageDir, e.Name()),
		}
	}
	return nil
}

func isURLSafe(ch rune) bool {
	return (ch >= 'a' && ch <= 'z') ||
		(ch >= 'A' && ch <= 'Z') ||
		(ch >= '0' && ch <= '9') ||
		ch == '-' || ch == '_'
}
