# syntax=docker/dockerfile:1.7

# ---------- build: binario estático, sin CGO ----------
FROM golang:1.25-alpine AS build
WORKDIR /src
# go.mod primero para aprovechar la caché de capas (no hay go.sum: solo stdlib).
COPY go.mod ./
RUN --mount=type=cache,target=/root/go/pkg/mod go mod download
COPY *.go ./
RUN --mount=type=cache,target=/root/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/normativa-rag .

# ---------- ingest: necesita pdftotext (poppler-utils) ----------
FROM alpine:3.22 AS ingest
RUN apk add --no-cache poppler-utils ca-certificates \
 && adduser -D -H -u 65532 app \
 && mkdir -p /app/out && chown app /app/out
WORKDIR /app
COPY --from=build /out/normativa-rag /app/normativa-rag
COPY registry.json evals.json ./
COPY corpus/ ./corpus/
USER 65532
# chunks.jsonl se escribe en el directorio de trabajo.
WORKDIR /app/out
ENV REGISTRY_PATH=/app/registry.json
# `docker compose run --rm ingest -dry` → normativa-rag ingest -dry
ENTRYPOINT ["/app/normativa-rag", "ingest"]

# ---------- runtime (default): API sobre distroless, sin shell, nonroot ----------
FROM gcr.io/distroless/static-debian12:nonroot AS runtime
WORKDIR /app
COPY --from=build /out/normativa-rag /app/normativa-rag
COPY registry.json evals.json ./
ENV ADDR=:8080 REGISTRY_PATH=/app/registry.json
EXPOSE 8080
USER nonroot:nonroot
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD ["/app/normativa-rag", "healthcheck"]
ENTRYPOINT ["/app/normativa-rag"]
CMD ["serve"]
