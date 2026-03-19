package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

const (
	defaultTimeout = 5 * time.Second
	maxMemoryPages = 512
)

type InvokeRequest struct {
	Payload []byte
	Timeout time.Duration
}

type InvokeResult struct {
	Output      []byte `json:"output"`
	ElapsedMs   int64  `json:"elapsed_ms"`
	MemoryBytes uint32 `json:"memory_bytes"`
}

// Executor manages the wazero runtime and compiled module cache.
//
// THREADING MODEL:
//   compileMu  — serializes CompileModule (wazero: not goroutine-safe)
//   cacheMu    — guards the compiled-module map for concurrent reads
//   InstantiateModule IS concurrent-safe; multiple invocations run in parallel.
//
// COMMUNICATION MODEL (WASI-native stdin/stdout):
//   Host pipes JSON payload to plugin's stdin.
//   Plugin reads from stdin, writes result JSON to stdout.
//   Host captures stdout as the invocation result.
//   This matches how every WASI runtime works (wasmtime, WasmEdge, etc.)
type Executor struct {
	runtime       wazero.Runtime
	compileMu     sync.Mutex
	cacheMu       sync.RWMutex
	compiledCache map[string]wazero.CompiledModule
}

func New() *Executor {
	ctx := context.Background()
	cfg := wazero.NewRuntimeConfig().WithMemoryLimitPages(maxMemoryPages)
	rt := wazero.NewRuntimeWithConfig(ctx, cfg)
	wasi_snapshot_preview1.MustInstantiate(ctx, rt)
	return &Executor{
		runtime:       rt,
		compiledCache: make(map[string]wazero.CompiledModule),
	}
}

func (e *Executor) Precompile(ctx context.Context, name string, wasmBytes []byte) error {
	_, err := e.compileOrCached(ctx, name, wasmBytes)
	return err
}

func (e *Executor) Evict(ctx context.Context, name string) {
	e.cacheMu.Lock()
	if m, ok := e.compiledCache[name]; ok {
		m.Close(ctx)
		delete(e.compiledCache, name)
	}
	e.cacheMu.Unlock()
}

func (e *Executor) compileOrCached(ctx context.Context, name string, wasmBytes []byte) (wazero.CompiledModule, error) {
	// Fast path
	e.cacheMu.RLock()
	cached, ok := e.compiledCache[name]
	e.cacheMu.RUnlock()
	if ok {
		return cached, nil
	}

	// Slow path: serialize compilation
	e.compileMu.Lock()
	defer e.compileMu.Unlock()

	// Double-check after acquiring lock
	e.cacheMu.RLock()
	cached, ok = e.compiledCache[name]
	e.cacheMu.RUnlock()
	if ok {
		return cached, nil
	}

	compiled, err := e.runtime.CompileModule(ctx, wasmBytes)
	if err != nil {
		return nil, fmt.Errorf("compiling wasm module %q: %w", name, err)
	}

	e.cacheMu.Lock()
	if old, exists := e.compiledCache[name]; exists {
		old.Close(ctx)
	}
	e.compiledCache[name] = compiled
	e.cacheMu.Unlock()

	return compiled, nil
}

// Invoke runs a WASI plugin using stdin→stdout communication.
//
// Protocol:
//   1. Host pipes req.Payload bytes to the plugin's stdin.
//   2. Plugin reads from stdin (os.Stdin in Go), processes, writes to stdout.
//   3. Host captures all stdout bytes as the result.
//   4. Stderr is captured separately for error diagnostics.
//
// This is 100% standard WASI I/O — any WASI-targeting language works without
// any special host-side ABI: Go, Rust, C, Python-wasi, etc.
func (e *Executor) Invoke(ctx context.Context, name string, wasmBytes []byte, req InvokeRequest) (*InvokeResult, error) {
	timeout := req.Timeout
	if timeout == 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()

	compiled, err := e.compileOrCached(ctx, name, wasmBytes)
	if err != nil {
		return nil, err
	}

	var stdout, stderr bytes.Buffer

	modCfg := wazero.NewModuleConfig().
		WithName("").
		WithStdin(bytes.NewReader(req.Payload)). // plugin reads payload from here
		WithStdout(&stdout).                      // plugin writes result here
		WithStderr(&stderr)                       // capture stderr for diagnostics

	mod, err := e.runtime.InstantiateModule(ctx, compiled, modCfg)
	if err != nil {
		// For WASI modules, _start runs to completion during InstantiateModule.
		// A non-zero exit code surfaces as an error here.
		if ctx.Err() != nil {
			stderrMsg := ""
			if stderr.Len() > 0 {
				stderrMsg = " stderr: " + stderr.String()
			}
			return nil, fmt.Errorf("plugin %q timed out after %s%s", name, timeout, stderrMsg)
		}
		// Check if it's a clean exit (exit code 0) — wazero wraps that as an error too
		// We check stdout: if there's output, the plugin completed successfully
		if stdout.Len() > 0 {
			// Plugin ran and produced output — the "error" is just wazero signaling _start returned
			output, _ := io.ReadAll(&stdout)
			return &InvokeResult{
				Output:    output,
				ElapsedMs: time.Since(start).Milliseconds(),
			}, nil
		}
		stderrMsg := stderr.String()
		if stderrMsg != "" {
			return nil, fmt.Errorf("plugin %q failed: %w (stderr: %s)", name, err, stderrMsg)
		}
		return nil, fmt.Errorf("plugin %q failed: %w", name, err)
	}
	if mod != nil {
		mod.Close(ctx)
	}

	output := stdout.Bytes()
	if len(output) == 0 {
		stderrMsg := stderr.String()
		if stderrMsg != "" {
			return nil, fmt.Errorf("plugin %q produced no output (stderr: %s)", name, stderrMsg)
		}
		return nil, fmt.Errorf("plugin %q produced no output — did it write to stdout?", name)
	}

	return &InvokeResult{
		Output:    output,
		ElapsedMs: time.Since(start).Milliseconds(),
	}, nil
}

func (e *Executor) Close(ctx context.Context) error {
	return e.runtime.Close(ctx)
}
