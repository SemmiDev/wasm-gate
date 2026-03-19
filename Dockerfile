# ── Stage 1: Build ──────────────────────────────────────────────────────────
FROM golang:1.22-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build server
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /bin/wasmgate ./cmd/server/

# Build bundled plugins
RUN mkdir -p /plugins && \
    for dir in plugins/src/*/; do \
      name=$(basename $dir); \
      GOOS=wasip1 GOARCH=wasm go build -o /plugins/${name}.wasm ./${dir}; \
    done

# ── Stage 2: Runtime ────────────────────────────────────────────────────────
FROM scratch

COPY --from=builder /bin/wasmgate /wasmgate
COPY --from=builder /plugins /plugins

ENV WASMGATE_ADDR=:8080
ENV WASMGATE_STORAGE_DIR=/data/plugins

EXPOSE 8080

ENTRYPOINT ["/wasmgate"]
