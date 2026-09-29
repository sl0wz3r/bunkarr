# Bunkarr developer tasks. `make help` lists them.
SHELL := /bin/sh

# Without the tag's leading v, like the release images (0.1.0-beta.1, not v0.1.0-beta.1).
VERSION    ?= $(shell v=$$(git describe --tags --always --dirty 2>/dev/null); echo $${v:-0.1.0-dev} | sed 's/^v//')
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
	CGO_ENABLED=1 go test -race -count=1 -timeout 45m ./...
	cd web && npm test

.PHONY: lint
lint: ## gofmt, go vet, TypeScript typecheck, shellcheck
	@out="$$(gofmt -l $$(git ls-files '*.go'))"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	go vet ./...
	go vet -tags e2e ./internal/e2e/...
	cd web && npm run typecheck
	@if command -v shellcheck >/dev/null; then shellcheck docker/*.sh unraid/ca/*.sh scripts/*.sh; else echo "shellcheck not installed, skipped"; fi

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

# Acceptance suite (docs/design/phase1.md §9). test-e2e builds the real binary itself and needs
# no Docker; the Docker targets drive $(IMAGE) with Go tests from internal/e2e (tag e2e).
.PHONY: test-e2e
test-e2e: ## End-to-end tests of the real binary (syncs, hardlinks, kill -9 + resume, guards)
	go test -tags e2e -count=1 -timeout 20m ./internal/e2e/...

.PHONY: test-docker
test-docker: docker ## Docker suite: image smoke test, compose file, container kill test, Plex restore test, SMB/NFS shares
	sh docker/test-image.sh $(IMAGE)
	sh docker/test-compose.sh
	sh docker/test-kill.sh $(IMAGE)
	sh docker/test-plex-restore.sh $(IMAGE)
	sh docker/test-shares.sh $(IMAGE)

.PHONY: test-plex
test-plex: docker ## Plex DB backup + restore test only (slow; pulls plexinc/pms-docker once)
	sh docker/test-plex-restore.sh $(IMAGE)

.PHONY: test-arr
test-arr: docker ## *arr suite: real Radarr/Sonarr/Lidarr imports, upgrades, backups and manifests (needs internet)
	sh docker/test-arr.sh $(IMAGE)

.PHONY: test-shares
test-shares: docker ## Sync and kill tests on SMB and NFS shares only (privileged containers)
	sh docker/test-shares.sh $(IMAGE)

# Real restic and rclone (docs/design/phase4.md D30, §14.4): the enginebin tests run in a
# golang:1.27-alpine container with alpine's restic and rclone (the versions of
# docker/engines/versions.env, as in the image) against MinIO and an SFTP server.
# Needs no Bunkarr image; the binaries are not needed on the host.
.PHONY: test-engines
test-engines: ## Real restic/rclone tests in containers (MinIO + SFTP; go test -tags enginebin)
	sh docker/test-engines.sh

# Off-site acceptance (docs/design/phase4.md §14.6, acceptance 1-9): restic and rclone destinations
# of $(IMAGE) on MinIO and an SFTP server, driven over HTTP by the Go tests TestDockerOffsite*
# (internal/e2e, tag e2e). They build a derived test image with an e2e build of this checkout, the
# argv shims of docker/offsite and curl. Needs Docker, Go and network access (apk, image pulls).
.PHONY: test-offsite
test-offsite: docker ## Off-site acceptance: restic/rclone destinations on MinIO + SFTP (Docker and Go)
	sh docker/test-offsite.sh $(IMAGE)

# Unraid Community Applications (CA): the public template unraid/bunkarr.xml and the repository-root
# ca_profile.xml are rendered from unraid/ca/*.tmpl with the values of ONE file, CA_ENV
# (unraid/ca/publish.env); see unraid/ca/README.md. Rendered files are validated in CA_OUT before
# they replace the committed ones. ca-validate checks that the committed files are current and
# valid (CI, and the public export). CA_OUT is scratch space (git-ignored, and left out of the
# repository scan in a tree that is not a git work tree); a command-line CA_OUT=... overrides it.
CA_DIR      := unraid/ca
CA_OUT      := $(CA_DIR)/out
CA_ENV      ?= $(CA_DIR)/publish.env
CA_TEMPLATE := unraid/bunkarr.xml
CA_ICON     := unraid/icon.png
CA_ICON_SRC := web/public/favicon.svg
CA_PROFILE  ?= ca_profile.xml

.PHONY: ca-template ca-profile ca-validate ca-preflight ca-vars ca-icon

ca-template: ## Render + validate unraid/bunkarr.xml (public CA template) from unraid/ca/ and CA_ENV
	@mkdir -p '$(CA_OUT)'
	sh $(CA_DIR)/render.sh -e '$(CA_ENV)' -o '$(CA_OUT)/bunkarr.xml' $(CA_DIR)/bunkarr.xml.tmpl
	sh $(CA_DIR)/validate-template.sh --icon $(CA_ICON) --repo . --scratch '$(CA_OUT)' --as $(CA_TEMPLATE) '$(CA_OUT)/bunkarr.xml'
	cp '$(CA_OUT)/bunkarr.xml' $(CA_TEMPLATE)
	@echo "Updated $(CA_TEMPLATE). Commit it together with the .tmpl/env change."

ca-profile: ## Render + validate the repository-root ca_profile.xml (CA_PROFILE) from unraid/ca/ and CA_ENV
	@mkdir -p '$(CA_OUT)'
	sh $(CA_DIR)/render.sh -e '$(CA_ENV)' -o '$(CA_OUT)/ca_profile.xml' $(CA_DIR)/ca_profile.xml.tmpl
	sh $(CA_DIR)/validate-template.sh --profile --as ca_profile.xml '$(CA_OUT)/ca_profile.xml'
	cp '$(CA_OUT)/ca_profile.xml' $(CA_PROFILE)
	@echo "Updated $(CA_PROFILE)."

ca-validate: ## Offline CA checks: committed template/profile match their sources and pass every rule
	@mkdir -p '$(CA_OUT)'
	@sh $(CA_DIR)/render.sh -e '$(CA_ENV)' -o '$(CA_OUT)/bunkarr.check.xml' $(CA_DIR)/bunkarr.xml.tmpl >/dev/null
	@cmp -s '$(CA_OUT)/bunkarr.check.xml' $(CA_TEMPLATE) || { echo "$(CA_TEMPLATE) is out of date with $(CA_DIR)/bunkarr.xml.tmpl + $(CA_ENV): run make ca-template" >&2; exit 1; }
	sh $(CA_DIR)/validate-template.sh --icon $(CA_ICON) --repo . --scratch '$(CA_OUT)' $(CA_TEMPLATE)
	@test -f $(CA_PROFILE) || { echo "$(CA_PROFILE) is missing: run make ca-profile" >&2; exit 1; }
	@sh $(CA_DIR)/render.sh -e '$(CA_ENV)' -o '$(CA_OUT)/ca_profile.check.xml' $(CA_DIR)/ca_profile.xml.tmpl >/dev/null
	@cmp -s '$(CA_OUT)/ca_profile.check.xml' $(CA_PROFILE) || { echo "$(CA_PROFILE) is out of date with $(CA_DIR)/ca_profile.xml.tmpl + $(CA_ENV): run make ca-profile" >&2; exit 1; }
	sh $(CA_DIR)/validate-template.sh --profile --as ca_profile.xml $(CA_PROFILE)

ca-preflight: ## Online, read-only, anonymous checks of the live public repo, raw URLs, icon and image (amd64/arm64)
	sh $(CA_DIR)/preflight.sh -e '$(CA_ENV)'

ca-vars: ## Print every public CA value derived from CA_ENV (URLs to paste into the submission form)
	@sh $(CA_DIR)/render.sh -e '$(CA_ENV)' --print

ca-icon: ## Render unraid/icon.png (512x512 RGBA) from web/public/favicon.svg; needs rsvg-convert
	@command -v rsvg-convert >/dev/null || { echo "rsvg-convert is required (macOS: brew install librsvg; Debian/Ubuntu: apt install librsvg2-bin)" >&2; exit 1; }
	@mkdir -p '$(CA_OUT)'
	rsvg-convert -w 512 -h 512 -o '$(CA_OUT)/icon.png' $(CA_ICON_SRC)
	@sh $(CA_DIR)/render.sh -e '$(CA_ENV)' -o '$(CA_OUT)/bunkarr.xml' $(CA_DIR)/bunkarr.xml.tmpl >/dev/null
	sh $(CA_DIR)/validate-template.sh --icon '$(CA_OUT)/icon.png' --as $(CA_TEMPLATE) '$(CA_OUT)/bunkarr.xml'
	cp '$(CA_OUT)/icon.png' $(CA_ICON)
	@echo "Updated $(CA_ICON). Once the app is listed, also append ?v=N to ICON_URL in $(CA_ENV) (CA caches icons by URL)."

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
clean: ## Remove build output (and the CA scratch files in unraid/ca/out)
	rm -rf bin dist unraid/ca/out
