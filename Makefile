.DEFAULT_GOAL := help
.PHONY: help generate api-gen api-lint build go-build web-install web-build web-embed test go-test web-test lint go-lint web-lint run web-dev docker clean

MODULE    := github.com/joinsider/uptimecluster
GO_PKGS   := ./cmd/... ./internal/...
BIN       := bin/uptimecluster
WEB_DIST  := web/dist/uptimecluster-web/browser
EMBED_DIR := internal/webui/dist

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w \
	-X $(MODULE)/internal/version.Version=$(VERSION) \
	-X $(MODULE)/internal/version.Commit=$(COMMIT) \
	-X $(MODULE)/internal/version.Date=$(DATE)

help: ## Show available targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-12s %s\n", $$1, $$2}'

generate: api-gen ## Regenerate code from api/openapi.yaml (and later proto/, sqlc)

api-gen: web/node_modules ## Generate Go server and Angular client from the OpenAPI contract
	go tool -modfile=tools/go.mod oapi-codegen -config api/oapi-codegen.yaml api/openapi.yaml
	go run ./internal/api/internal/stubgen internal/api/gen/api.gen.go internal/api/unimplemented.go
	cd web && npx orval --config orval.config.ts

api-lint: web/node_modules ## Lint the OpenAPI contract
	cd web && npx redocly lint ../api/openapi.yaml --config ../api/redocly.yaml

build: web-embed go-build ## Build the frontend and the binary with the embedded SPA

go-build: ## Build only the Go binary (embeds whatever is in internal/webui/dist)
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/uptimecluster

web/node_modules: web/package.json web/package-lock.json
	cd web && npm ci
	@touch web/node_modules

web-install: web/node_modules ## Install frontend dependencies

web-build: web/node_modules ## Build the Angular SPA
	cd web && npx ng build

web-embed: web-build ## Copy the SPA build into the Go embed directory
	rsync -a --delete --exclude .gitkeep $(WEB_DIST)/ $(EMBED_DIR)/

test: go-test web-test ## Run all tests

go-test: ## Run Go tests with race detector
	go test -race -cover $(GO_PKGS)

web-test: web/node_modules ## Run frontend unit tests
	cd web && npx ng test --watch=false

lint: go-lint web-lint api-lint ## Run all linters

go-lint: ## go vet and gofmt check
	go vet $(GO_PKGS)
	@out="$$(gofmt -l cmd internal)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

web-lint: web/node_modules ## Prettier check
	cd web && npx prettier --check src

run: build ## Run a local single voter on :8080
	mkdir -p .data
	DATA_DIR=.data NODE_NAME=dev LOG_FORMAT=text $(BIN)

web-dev: web/node_modules ## Angular dev server on :4200 proxying the API to :8080
	cd web && npx ng serve

docker: ## Build the container image
	docker build --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg DATE=$(DATE) -t uptimecluster:dev .

clean: ## Remove build output
	rm -rf bin web/dist
	find $(EMBED_DIR) -mindepth 1 ! -name .gitkeep -delete
