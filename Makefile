# LyricsPlus backend build and dev tooling.

BINARY     := bin/server
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT     ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
BUILD_DATE ?= $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")

LDFLAGS    := -s -w \
	-X lyricsplus/backend/internal/version.Version=$(VERSION) \
	-X lyricsplus/backend/internal/version.Commit=$(COMMIT) \
	-X lyricsplus/backend/internal/version.BuildDate=$(BUILD_DATE)

GOPKG      := lyricsplus/backend

GO         := go

.PHONY: build run test vet fmt fmt-check lint lint-check clean \
	docker-build docker-up docker-down hooks-install

build:
	@mkdir -p bin
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/server
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/gdrive_sync ./cmd/gdrive_sync
	@echo "built $(BINARY) and bin/gdrive_sync (version $(VERSION), commit $(COMMIT), built $(BUILD_DATE))"

run: build
	./$(BINARY)

test:
	$(GO) test ./...

test-race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -w .

fmt-check:
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then \
		echo "These files are not gofmt-formatted:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi

lint: lint-check

lint-check:
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run ./...; \
	else \
		echo "golangci-lint not installed; falling back to 'go vet'."; \
		$(GO) vet ./...; \
	fi

clean:
	rm -rf bin

hooks-install:
	git config core.hooksPath .githooks
	@echo "git hooks installed from .githooks (runs fmt/vet/test on commit)."

docker-build:
	docker build -t lyricsplus-backend \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg BUILD_DATE=$(BUILD_DATE) .

docker-up:
	docker compose up -d --build

docker-down:
	docker compose down