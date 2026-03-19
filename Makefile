.PHONY: build plugins test run clean docker

BINARY     := wasmgate
BUILD_DIR  := ./bin
PLUGIN_DIR := ./plugins/src
WASM_DIR   := ./testdata
GO         := go

# Build the server binary
build:
	@mkdir -p $(BUILD_DIR)
	$(GO) build -o $(BUILD_DIR)/$(BINARY) ./cmd/server/
	@echo "Built: $(BUILD_DIR)/$(BINARY)"

# Compile all WASM plugins
plugins:
	@mkdir -p $(WASM_DIR)
	@for plugin in $(PLUGIN_DIR)/*/; do \
		name=$$(basename $$plugin); \
		echo "Building plugin: $$name"; \
		(cd $$plugin && GOOS=wasip1 GOARCH=wasm $(GO) build -o $(CURDIR)/$(WASM_DIR)/$$name.wasm .); \
	done
	@echo "Plugins built in $(WASM_DIR)/"

# Run all tests
test:
	$(GO) test ./... -timeout 120s -v

# Run the server locally
run: build
	WASMGATE_STORAGE_DIR=./data/plugins $(BUILD_DIR)/$(BINARY)

# Remove build artifacts
clean:
	rm -rf $(BUILD_DIR) $(WASM_DIR)/*.wasm data/

# Build Docker image
docker:
	docker build -t wasmgate:latest .

# Register a plugin (usage: make register PLUGIN=testdata/json-transformer.wasm NAME=json-transformer)
register:
	@curl -s -X POST http://localhost:8080/plugins/$(NAME) \
	  -H "Content-Type: application/wasm" \
	  -H "X-Plugin-Version: 1.0.0" \
	  --data-binary @$(PLUGIN) | jq .

# Invoke a plugin (usage: make invoke NAME=json-transformer PAYLOAD='{"name":"alice"}')
invoke:
	@curl -s -X POST http://localhost:8080/invoke/$(NAME) \
	  -H "Content-Type: application/json" \
	  -d '$(PAYLOAD)' | jq .
