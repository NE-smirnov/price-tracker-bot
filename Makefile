SHELL := /bin/bash
.DEFAULT_GOAL := help

MODULE       := github.com/NE-smirnov/price-tracker-bot
GO           ?= go
TOOLS_DIR    := $(CURDIR)/.tools
BIN_DIR      := $(CURDIR)/bin
GOFLAGS_TEST ?= -race -count=1

GOLANGCI_VERSION := v2.13.2
GOIMPORTS_VERSION := latest
BUF_VERSION := v1.72.0
MIGRATE_VERSION := v4.19.1

export PATH := $(TOOLS_DIR):$(PATH)

# Services with a buildable cmd/ entrypoint. Add scraper and notifier here as
# they land, so `make build` never tries to build an empty package.
SERVICES := bot core

# DSN used by the integration tests. They are skipped when TEST_DATABASE_URL is
# empty, so `make test` stays runnable without any infrastructure.
TEST_DATABASE_URL ?= postgres://price:price@localhost:5432/price_tracker_test?sslmode=disable

## ---------------------------------------------------------------- help

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z0-9_.-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}'

## ---------------------------------------------------------------- tooling

.PHONY: tools
tools: $(TOOLS_DIR)/golangci-lint $(TOOLS_DIR)/goimports $(TOOLS_DIR)/buf $(TOOLS_DIR)/protoc-gen-go $(TOOLS_DIR)/protoc-gen-go-grpc $(TOOLS_DIR)/migrate ## Install all pinned dev tools into .tools/

$(TOOLS_DIR):
	@mkdir -p $(TOOLS_DIR)

$(TOOLS_DIR)/golangci-lint: | $(TOOLS_DIR)
	GOBIN=$(TOOLS_DIR) $(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION)

$(TOOLS_DIR)/goimports: | $(TOOLS_DIR)
	GOBIN=$(TOOLS_DIR) $(GO) install golang.org/x/tools/cmd/goimports@$(GOIMPORTS_VERSION)

$(TOOLS_DIR)/buf: | $(TOOLS_DIR)
	GOBIN=$(TOOLS_DIR) $(GO) install github.com/bufbuild/buf/cmd/buf@$(BUF_VERSION)

$(TOOLS_DIR)/protoc-gen-go: | $(TOOLS_DIR)
	GOBIN=$(TOOLS_DIR) $(GO) install google.golang.org/protobuf/cmd/protoc-gen-go@latest

$(TOOLS_DIR)/protoc-gen-go-grpc: | $(TOOLS_DIR)
	GOBIN=$(TOOLS_DIR) $(GO) install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest

$(TOOLS_DIR)/migrate: | $(TOOLS_DIR)
	GOBIN=$(TOOLS_DIR) $(GO) install -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate@$(MIGRATE_VERSION)

.PHONY: hooks
hooks: ## Enable repo-managed git hooks (.githooks)
	git config core.hooksPath .githooks
	chmod +x .githooks/*
	@echo "git hooks enabled: core.hooksPath=.githooks"

## ---------------------------------------------------------------- format / lint

.PHONY: fmt
fmt: $(TOOLS_DIR)/goimports ## Auto-fix formatting and imports
	$(GO) fmt ./...
	$(TOOLS_DIR)/goimports -local $(MODULE) -w $(shell find . -name '*.go' -not -path './.tools/*' -not -path './internal/gen/*')

.PHONY: tidy
tidy: ## Sync go.mod/go.sum
	$(GO) mod tidy

.PHONY: vet
vet: ## Run go vet
	$(GO) vet ./...

.PHONY: lint
lint: $(TOOLS_DIR)/golangci-lint ## Run golangci-lint (read-only)
	$(TOOLS_DIR)/golangci-lint run ./...

.PHONY: lint-fix
lint-fix: $(TOOLS_DIR)/golangci-lint ## Run golangci-lint with autofix
	$(TOOLS_DIR)/golangci-lint run --fix ./...

.PHONY: fix
fix: fmt tidy lint-fix ## One-shot autofix: format, imports, tidy, lint --fix

.PHONY: check
check: fmt-check vet lint test ## Full read-only gate (what CI runs)

.PHONY: fmt-check
fmt-check: $(TOOLS_DIR)/goimports ## Fail if any file is not formatted
	@out="$$($(TOOLS_DIR)/goimports -local $(MODULE) -l $$(find . -name '*.go' -not -path './.tools/*' -not -path './internal/gen/*'))"; \
	if [[ -n "$$out" ]]; then echo "not formatted:"; echo "$$out"; exit 1; fi

.PHONY: tidy-check
tidy-check: ## Fail if go.mod/go.sum are not tidy
	@cp go.mod go.mod.bak; [[ -f go.sum ]] && cp go.sum go.sum.bak || true; \
	$(GO) mod tidy; \
	status=0; \
	diff -q go.mod go.mod.bak >/dev/null || status=1; \
	if [[ -f go.sum.bak ]]; then diff -q go.sum go.sum.bak >/dev/null || status=1; fi; \
	mv go.mod.bak go.mod; [[ -f go.sum.bak ]] && mv go.sum.bak go.sum || true; \
	if [[ $$status -ne 0 ]]; then echo "go.mod/go.sum are not tidy -> run 'make tidy'"; exit 1; fi

## ---------------------------------------------------------------- test / build

.PHONY: test
test: ## Run unit tests with race detector (database tests are skipped)
	$(GO) test $(GOFLAGS_TEST) ./...

.PHONY: test-integration
test-integration: ## Run all tests including the ones that need PostgreSQL
	@echo "using TEST_DATABASE_URL=$(TEST_DATABASE_URL)"
	TEST_DATABASE_URL="$(TEST_DATABASE_URL)" $(GO) test $(GOFLAGS_TEST) -p 1 ./...

.PHONY: test-db-create
test-db-create: ## Create the scratch database the integration tests use
	docker compose exec -T postgres psql -U price -d postgres \
		-c "CREATE DATABASE price_tracker_test OWNER price" || true

.PHONY: cover
cover: ## Run tests with coverage report
	$(GO) test -covermode=atomic -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -1

.PHONY: build
build: $(addprefix build-,$(SERVICES)) ## Build all service binaries into bin/

.PHONY: $(addprefix build-,$(SERVICES))
$(addprefix build-,$(SERVICES)): build-%:
	CGO_ENABLED=0 $(GO) build -trimpath -o $(BIN_DIR)/$* ./cmd/$*

.PHONY: run-bot
run-bot: ## Run the telegram bot locally
	$(GO) run ./cmd/bot

.PHONY: run-core
run-core: ## Run the core gRPC service locally
	$(GO) run ./cmd/core

## ---------------------------------------------------------------- proto

.PHONY: proto
proto: $(TOOLS_DIR)/buf $(TOOLS_DIR)/protoc-gen-go $(TOOLS_DIR)/protoc-gen-go-grpc ## Generate Go code from .proto
	$(TOOLS_DIR)/buf generate

.PHONY: proto-lint
proto-lint: $(TOOLS_DIR)/buf ## Lint .proto files
	$(TOOLS_DIR)/buf lint

.PHONY: proto-format
proto-format: $(TOOLS_DIR)/buf ## Format .proto files in place
	$(TOOLS_DIR)/buf format -w

## ---------------------------------------------------------------- infra

.PHONY: up
up: ## Start postgres + redis (infra only)
	docker compose up -d postgres redis

.PHONY: up-all
up-all: ## Start the whole stack
	docker compose up -d --build

.PHONY: down
down: ## Stop the stack
	docker compose down

.PHONY: logs
logs: ## Tail stack logs
	docker compose logs -f --tail=100

.PHONY: migrate-up
migrate-up: $(TOOLS_DIR)/migrate ## Apply DB migrations
	$(TOOLS_DIR)/migrate -path migrations -database "$${POSTGRES_DSN:-postgres://price:price@localhost:5432/price_tracker?sslmode=disable}" up

.PHONY: migrate-down
migrate-down: $(TOOLS_DIR)/migrate ## Roll back one migration
	$(TOOLS_DIR)/migrate -path migrations -database "$${POSTGRES_DSN:-postgres://price:price@localhost:5432/price_tracker?sslmode=disable}" down 1

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf $(BIN_DIR) coverage.out
