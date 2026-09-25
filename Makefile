# Bunkarr developer tasks. `make help` lists them.
SHELL := /bin/sh

VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.1.0-dev)
COMMIT     ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
PKG        := github.com/sl0wz3r/bunkarr/internal/version
LDFLAGS    := -s -w -X $(PKG).Version=$(VERSION) -X $(PKG).Commit=$(COMMIT) -X $(PKG).BuildDate=$(BUILD_DATE)
IMAGE      ?= bunkarr:dev

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z_-]+:.*## / {printf "  %-14s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: web
web: ## Build the web UI into web/dist
	cd web && npm ci --no-audit --no-fund && npm run build
	touch web/dist/.gitkeep

.PHONY: build
build: ## Build bin/bunkarr (embeds whatever is in web/dist)
	CGO_ENABLED=0 go build -trimpath -tags timetzdata -ldflags "$(LDFLAGS)" -o bin/bunkarr ./cmd/bunkarr

.PHONY: all
all: web build ## Web UI + binary

.PHONY: run
run: build ## Run locally with ./config as the config directory
	./bin/bunkarr --config ./config

.PHONY: test
test: ## Go tests (race) and web tests
	CGO_ENABLED=1 go test -race -count=1 ./...
	cd web && npm test

.PHONY: lint
lint: ## gofmt, go vet, TypeScript typecheck, shellcheck
	@out="$$(gofmt -l $$(git ls-files '*.go'))"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	go vet ./...
	cd web && npm run typecheck
	@if command -v shellcheck >/dev/null; then shellcheck docker/*.sh; else echo "shellcheck not installed, skipped"; fi

.PHONY: vuln
vuln: ## govulncheck and npm audit (runtime dependencies)
	go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...
	cd web && npm audit --omit=dev --audit-level=moderate

.PHONY: docker
docker: ## Build the container image for this machine ($(IMAGE))
	docker build -t $(IMAGE) --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg BUILD_DATE=$(BUILD_DATE) .

.PHONY: docker-test
docker-test: docker ## Build the image and run docker/test-image.sh against it
	sh docker/test-image.sh $(IMAGE)

# Maintainer only: mirror the private repository to the public one through the sanitizing
# export (scripts/public/ is not part of the public tree).
.PHONY: publish publish-dry mirror-hook
publish: ## Publish HEAD to the public GitHub repository (sanitized)
	scripts/public/sync.sh
publish-dry: ## Like publish, but push nothing
	scripts/public/sync.sh --dry-run
mirror-hook: ## Install the post-commit hook that publishes every commit on main
	install -m 0755 scripts/public/hooks/post-commit "$$(git rev-parse --git-path hooks)/post-commit"

.PHONY: clean
clean: ## Remove build output
	rm -rf bin dist
