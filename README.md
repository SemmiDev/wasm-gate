# WasmGate

A production-grade WASM plugin execution engine. Upload any WASM binary as a plugin, then invoke it via REST API with JSON payloads. Every plugin runs in an isolated sandbox with memory limits and timeouts — no plugin can crash or block the server.

```
POST /plugins/{name}   — upload a WASM plugin
GET  /plugins          — list all plugins
POST /invoke/{name}    — run a plugin with JSON input
DELETE /plugins/{name} — remove a plugin
GET  /health           — health check
```

## Quick start

```bash
# 1. Build server + plugins
make build plugins

# 2. Run server
make run

# 3. Register a plugin
curl -X POST http://localhost:8080/plugins/json-transformer \
  -H "Content-Type: application/wasm" \
  -H "X-Plugin-Description: Transforms JSON fields" \
  -H "X-Plugin-Version: 1.0.0" \
  --data-binary @testdata/json-transformer.wasm

# 4. Invoke it
curl -X POST http://localhost:8080/invoke/json-transformer \
  -H "Content-Type: application/json" \
  -d '{"name":"alice","score":42,"active":true}'
```

Response:
```json
{
  "plugin": "json-transformer",
  "output": {
    "name": "ALICE",
    "score": 84,
    "active": false,
    "_plugin": "json-transformer@1.0.0",
    "_fields_transformed": 3
  },
  "elapsed_ms": 270,
  "memory_bytes": 33554432
}
```

## Writing a plugin

A WasmGate plugin is any WASM binary that reads JSON from stdin and writes JSON to stdout. Target: `GOOS=wasip1 GOARCH=wasm`.

```go
package main

import (
    "encoding/json"
    "os"
)

func main() {
    var input map[string]interface{}
    json.NewDecoder(os.Stdin).Decode(&input)

    result := map[string]interface{}{
        "processed": true,
        "input_keys": len(input),
    }
    json.NewEncoder(os.Stdout).Encode(result)
}
```

Build:
```bash
GOOS=wasip1 GOARCH=wasm go build -o myplugin.wasm .
```

Any WASI-targeting language works: Rust, C, AssemblyScript, Python-wasi.

## Architecture

```
HTTP Client
    │
    ▼
Go HTTP Server (chi router)
    ├── /plugins/*  → Registry (disk-backed .wasm store)
    └── /invoke/*   → Executor (wazero WASM runtime)
                            │
                     Fresh module instance
                     per invocation (share-nothing)
                            │
                     stdin ──→ plugin ──→ stdout
```

**Key properties:**
- Isolation: each invocation gets a brand-new WASM instance — no shared state
- Safety: 5-second default timeout, 32MB memory cap per invocation
- Performance: compiled modules are cached; only first invocation pays compile cost
- Portability: wazero is pure-Go, zero CGO — single static binary
- Persistence: plugins survive server restarts (stored on disk, re-indexed on startup)

## Configuration

| Env var | Default | Description |
|---|---|---|
| `WASMGATE_ADDR` | `:8080` | Listen address |
| `WASMGATE_STORAGE_DIR` | `./data/plugins` | Plugin storage directory |
| `WASMGATE_LOG_LEVEL` | `info` | Log level (debug/info/warn/error) |

## API reference

### POST /plugins/{name}
Upload a WASM plugin. Headers:
- `Content-Type: application/wasm`
- `X-Plugin-Description` (optional)
- `X-Plugin-Version` (optional, default: 1.0.0)
- `X-Plugin-Author` (optional)
- `X-Timeout-Ms` (optional, per-invocation timeout override)

### POST /invoke/{name}
Invoke a plugin. Body: any JSON. Response:
```json
{ "plugin": "name", "output": { ... }, "elapsed_ms": 270 }
```

### GET /plugins
List all registered plugins with metadata.

### DELETE /plugins/{name}
Remove a plugin and its binary.

## Running tests

```bash
make plugins   # must build plugins first
make test
```

## Docker

```bash
make docker
docker run -p 8080:8080 wasmgate:latest
```
