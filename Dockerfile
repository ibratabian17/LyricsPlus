# syntax=docker/dockerfile:1

FROM golang:1.27-alpine AS build
ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown

WORKDIR /src

# Download modules in their own layer, cached by go.mod/go.sum hash.
# This layer is only invalidated when dependencies change, not on every
# source edit, and a BuildKit cache mount persists the module cache
# across builds instead of re-downloading every time.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

# Cache Go's build cache across builds too, so unchanged packages are
# never recompiled from scratch.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    set -e; \
    if [ -z "$BUILD_DATE" ] || [ "$BUILD_DATE" = "unknown" ]; then \
        BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"; \
    fi; \
    CGO_ENABLED=0 go build -trimpath \
        -ldflags "-s -w \
          -X lyricsplus/backend/internal/version.Version=${VERSION} \
          -X lyricsplus/backend/internal/version.Commit=${COMMIT} \
          -X lyricsplus/backend/internal/version.BuildDate=${BUILD_DATE}" \
        -o /out/server ./cmd/server

FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata dumb-init
WORKDIR /app

COPY --from=build /out/server /usr/local/bin/lyricsplus-server

ENV PORT=3000 \
    DISABLE_LOGGING=false \
    LOG_LEVEL=info \
    LOG_FORMAT=text \
    GDRIVE_ENABLED=false \
    DAILY_DUMP_ENABLED=false \
    SQLITE_PATH=/app/database/lyrics_cache.db \
    DAILY_DUMP_DIR=/app/data/dumps

RUN mkdir -p /app/database /app/data/dumps

EXPOSE 3000

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1:3000/health || exit 1

ENTRYPOINT ["/usr/bin/dumb-init", "--", "/usr/local/bin/lyricsplus-server"]