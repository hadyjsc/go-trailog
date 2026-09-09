# =============================================================================
# Trailog — cross-platform Makefile  (Windows PowerShell + Linux/macOS sh)
#
# Prerequisites
#   go        https://go.dev/dl/
#   goose     make setup
#   docker    https://docs.docker.com/get-docker/  (Compose v2)
#
# Quick-start (both platforms)
#   make setup          install goose + golangci-lint
#   make env-init       copy .env.example → .env
#   make docker-db-up   start local databases
#   make migrate-up     apply all pending migrations
#   make run            go run ./cmd/trailog
# =============================================================================

# ─── OS detection ─────────────────────────────────────────────────────────────
# Make sets OS=Windows_NT on all Windows versions.
ifeq ($(OS),Windows_NT)
  PLATFORM := windows
else
  PLATFORM := unix
endif

# ─── Shell configuration ──────────────────────────────────────────────────────
# Windows: PowerShell Core (pwsh.exe).
# Linux/macOS: the system sh (bash-compatible, always present).
ifeq ($(PLATFORM),windows)
  SHELL       := pwsh.exe
  .SHELLFLAGS := -NoProfile -NonInteractive -Command
else
  SHELL       := /bin/sh
  .SHELLFLAGS := -ec
endif

# ─── Portable command aliases ─────────────────────────────────────────────────
# Every recipe that needs OS-sensitive commands uses these macros instead of
# raw rm / mkdir / echo / sleep so the same recipe text works on both platforms.
ifeq ($(PLATFORM),windows)
  # PowerShell equivalents
  ECHO      = pwsh.exe -NoProfile -NonInteractive -Command Write-Host
  ECHO_C    = pwsh.exe -NoProfile -NonInteractive -Command "Write-Host $(1) -ForegroundColor $(2)"
  MKDIR     = pwsh.exe -NoProfile -NonInteractive -Command "New-Item -ItemType Directory -Force -Path $(1) | Out-Null"
  RM        = pwsh.exe -NoProfile -NonInteractive -Command "Remove-Item -Recurse -Force -ErrorAction SilentlyContinue $(1)"
  SLEEP     = pwsh.exe -NoProfile -NonInteractive -Command "Start-Sleep -Seconds $(1)"
  TEST_FILE = pwsh.exe -NoProfile -NonInteractive -Command "if (-not (Test-Path $(1))) { exit 1 }"
  COPY_FILE = pwsh.exe -NoProfile -NonInteractive -Command "Copy-Item $(1) $(2)"
  SEP       := \\
  EXE_EXT   := .exe
  RUN_BIN    = .\$(1)
else
  # POSIX sh equivalents
  ECHO      = echo
  ECHO_C    = echo "$(1)"
  MKDIR     = mkdir -p $(1)
  RM        = rm -rf $(1)
  SLEEP     = sleep $(1)
  TEST_FILE = test -f $(1)
  COPY_FILE = cp $(1) $(2)
  SEP       := /
  EXE_EXT   :=
  RUN_BIN    = ./$(1)
endif

# ─── Load .env (existing shell/process env always wins) ───────────────────────
-include .env
export

# ─── Project variables ────────────────────────────────────────────────────────
BINARY        ?= trailog
BUILD_DIR     ?= bin
CMD_DIR       ?= ./cmd/trailog

DB_DRIVER     ?= $(TRAILOG_DB_DRIVER)
DB_DSN        ?= $(TRAILOG_DB_DSN)

GOOSE_BIN     ?= goose
MIGRATIONS_DIR = db/migrations

# Map DB_DRIVER → goose driver name (postgres | mysql | mssql)
ifeq ($(DB_DRIVER),mysql)
  GOOSE_DRIVER = mysql
else ifeq ($(DB_DRIVER),sqlserver)
  GOOSE_DRIVER = mssql
else
  GOOSE_DRIVER = postgres
endif

# Map DB_DRIVER → migrations sub-folder
ifeq ($(DB_DRIVER),mysql)
  MIGRATE_DIR = $(MIGRATIONS_DIR)/mysql
else ifeq ($(DB_DRIVER),sqlserver)
  MIGRATE_DIR = $(MIGRATIONS_DIR)/sqlserver
else
  MIGRATE_DIR = $(MIGRATIONS_DIR)/postgres
endif

GO           ?= go
COMPOSE      ?= docker compose
COMPOSE_FILE ?= docker-compose.yml
GOLANGCI     ?= golangci-lint

TEST_FLAGS    ?= -v -race -count=1
COVERAGE_OUT  ?= coverage.out
COVERAGE_HTML ?= coverage.html

# Determine binary output name (adds .exe on Windows)
BINARY_OUT = $(BUILD_DIR)$(SEP)$(BINARY)$(EXE_EXT)

.DEFAULT_GOAL := help

# =============================================================================
# HELP
# =============================================================================
.PHONY: help
help: ## Show this help message
ifeq ($(PLATFORM),windows)
	@pwsh.exe -NoProfile -NonInteractive -Command "Write-Host ''"
	@pwsh.exe -NoProfile -NonInteractive -Command "Write-Host '  Trailog - available targets' -ForegroundColor Cyan"
	@pwsh.exe -NoProfile -NonInteractive -Command "Write-Host ''"
	@pwsh.exe -NoProfile -NonInteractive -Command "Select-String -Path '$(MAKEFILE_LIST)' -Pattern '^[a-zA-Z_-]+:.*## ' | ForEach-Object { if ($$_.Line -match '^([a-zA-Z_-]+):.*## (.+)') { Write-Host ('  {0,-26} {1}' -f $$Matches[1], $$Matches[2]) } }"
	@pwsh.exe -NoProfile -NonInteractive -Command "Write-Host ''"
	@pwsh.exe -NoProfile -NonInteractive -Command "Write-Host '  Platform    : windows (pwsh)'"
	@pwsh.exe -NoProfile -NonInteractive -Command "Write-Host '  DB_DRIVER   : $(DB_DRIVER)'"
	@pwsh.exe -NoProfile -NonInteractive -Command "Write-Host '  MIGRATE_DIR : $(MIGRATE_DIR)'"
ifeq ($(DB_DSN),)
	@pwsh.exe -NoProfile -NonInteractive -Command "Write-Host '  DB_DSN      : NOT SET  -- add TRAILOG_DB_DSN to .env' -ForegroundColor Yellow"
else
	@pwsh.exe -NoProfile -NonInteractive -Command "Write-Host '  DB_DSN      : configured'"
endif
	@pwsh.exe -NoProfile -NonInteractive -Command "Write-Host ''"
else
	@echo ""
	@echo "  Trailog — available targets"
	@echo ""
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
	  | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-26s\033[0m %s\n",$$1,$$2}'
	@echo ""
	@echo "  Platform    : unix (sh)"
	@echo "  DB_DRIVER   : $(DB_DRIVER)"
	@echo "  MIGRATE_DIR : $(MIGRATE_DIR)"
ifeq ($(DB_DSN),)
	@echo "  DB_DSN      : NOT SET  -- add TRAILOG_DB_DSN to .env"
else
	@echo "  DB_DSN      : configured"
endif
	@echo ""
endif

# =============================================================================
# SETUP
# =============================================================================
.PHONY: setup
setup: ## Install goose CLI and golangci-lint into GOPATH/bin
	$(GO) install github.com/pressly/goose/v3/cmd/goose@latest
	$(GO) install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
	@$(ECHO) "Done. Ensure $$(go env GOPATH)/bin is in your PATH."

.PHONY: deps
deps: ## Download and tidy Go module dependencies
	$(GO) mod download
	$(GO) mod tidy

# =============================================================================
# ENV
# =============================================================================
.PHONY: env-init
env-init: ## Copy .env.example to .env (no-op if .env already exists)
ifeq ($(PLATFORM),windows)
	@pwsh.exe -NoProfile -NonInteractive -Command " \
	  if (Test-Path '.env') { \
	    Write-Host '.env already exists - skipping. Edit it manually.' -ForegroundColor Yellow \
	  } else { \
	    Copy-Item '.env.example' '.env'; \
	    Write-Host '.env created from .env.example. Fill in your values.' -ForegroundColor Green \
	  }"
else
	@if [ -f .env ]; then \
	  echo ".env already exists — skipping. Edit it manually."; \
	else \
	  cp .env.example .env; \
	  echo ".env created from .env.example. Fill in your values."; \
	fi
endif

.PHONY: env-print
env-print: ## Print resolved TRAILOG_* values (DSN existence only, not value)
	@$(ECHO) "TRAILOG_DB_DRIVER      = $(DB_DRIVER)"
	@$(ECHO) "TRAILOG_RUN_MIGRATIONS = $(TRAILOG_RUN_MIGRATIONS)"
	@$(ECHO) "TRAILOG_HTTP_ADDR      = $(TRAILOG_HTTP_ADDR)"
	@$(ECHO) "TRAILOG_ASYNC          = $(TRAILOG_ASYNC)"
	@$(ECHO) "TRAILOG_LOG_LEVEL      = $(TRAILOG_LOG_LEVEL)"
	@$(ECHO) "PLATFORM               = $(PLATFORM)"
ifeq ($(DB_DSN),)
	@$(ECHO) "TRAILOG_DB_DSN         = NOT SET -- add TRAILOG_DB_DSN to .env"
else
	@$(ECHO) "TRAILOG_DB_DSN         = configured"
endif

# =============================================================================
# BUILD
# =============================================================================
.PHONY: build
build: ## Build the binary to bin/trailog(.exe)
	$(call MKDIR,$(BUILD_DIR))
	$(GO) build -trimpath -ldflags="-s -w" -o $(BINARY_OUT) $(CMD_DIR)
	@$(ECHO) "Built: $(BINARY_OUT)"

.PHONY: build-all
build-all: ## Cross-compile for linux/darwin/windows amd64 + linux arm64
	$(call MKDIR,$(BUILD_DIR))
ifeq ($(PLATFORM),windows)
	@pwsh.exe -NoProfile -NonInteractive -Command "$$env:GOOS='linux';   $$env:GOARCH='amd64'; go build -trimpath -ldflags='-s -w' -o $(BUILD_DIR)/$(BINARY)-linux-amd64    $(CMD_DIR)"
	@pwsh.exe -NoProfile -NonInteractive -Command "$$env:GOOS='linux';   $$env:GOARCH='arm64'; go build -trimpath -ldflags='-s -w' -o $(BUILD_DIR)/$(BINARY)-linux-arm64    $(CMD_DIR)"
	@pwsh.exe -NoProfile -NonInteractive -Command "$$env:GOOS='darwin';  $$env:GOARCH='amd64'; go build -trimpath -ldflags='-s -w' -o $(BUILD_DIR)/$(BINARY)-darwin-amd64   $(CMD_DIR)"
	@pwsh.exe -NoProfile -NonInteractive -Command "$$env:GOOS='windows'; $$env:GOARCH='amd64'; go build -trimpath -ldflags='-s -w' -o $(BUILD_DIR)/$(BINARY)-windows-amd64.exe $(CMD_DIR)"
else
	GOOS=linux   GOARCH=amd64 $(GO) build -trimpath -ldflags="-s -w" -o $(BUILD_DIR)/$(BINARY)-linux-amd64    $(CMD_DIR)
	GOOS=linux   GOARCH=arm64 $(GO) build -trimpath -ldflags="-s -w" -o $(BUILD_DIR)/$(BINARY)-linux-arm64    $(CMD_DIR)
	GOOS=darwin  GOARCH=amd64 $(GO) build -trimpath -ldflags="-s -w" -o $(BUILD_DIR)/$(BINARY)-darwin-amd64   $(CMD_DIR)
	GOOS=windows GOARCH=amd64 $(GO) build -trimpath -ldflags="-s -w" -o $(BUILD_DIR)/$(BINARY)-windows-amd64.exe $(CMD_DIR)
endif
	@$(ECHO) "Cross-compiled binaries in $(BUILD_DIR)/"

.PHONY: clean
clean: ## Remove build artefacts (bin/, coverage files)
	$(call RM,$(BUILD_DIR) $(COVERAGE_OUT) $(COVERAGE_HTML))
	@$(ECHO) "Cleaned."

# =============================================================================
# RUN
# =============================================================================
.PHONY: run
run: ## Run the service from source (go run) — reads .env automatically
	$(GO) run $(CMD_DIR)

.PHONY: run-binary
run-binary: build ## Build then run the compiled binary
ifeq ($(PLATFORM),windows)
	.\$(BUILD_DIR)\$(BINARY).exe
else
	./$(BUILD_DIR)/$(BINARY)
endif

# =============================================================================
# MIGRATIONS — goose
#
# DB_DRIVER and DB_DSN come from .env / environment automatically.
# The correct sub-folder is chosen from DB_DRIVER.
# Goose receives its config via environment variables to avoid shell quoting
# issues with DSN strings containing ://  (works on both Windows and Linux).
#
# Per-invocation override:
#   make migrate-up DB_DRIVER=mysql DB_DSN=user:pass@tcp(localhost:3306)/db?parseTime=true
# =============================================================================

# Internal guard — aborts with a clear error when DB_DSN is not set.
.PHONY: _require-dsn
_require-dsn:
ifeq ($(DB_DSN),)
	$(error TRAILOG_DB_DSN is required. Set it in .env or run: make migrate-up DB_DSN=<dsn>)
endif

# Inline env-var prefix for goose invocations.
# On Linux/macOS: prefix the command with VAR=value pairs (POSIX sh).
# On Windows/pwsh: set $env: variables then call goose in the same recipe line.
ifeq ($(PLATFORM),windows)
  # PowerShell: set env vars then call goose using the call operator (&)
  # The & operator bypasses PowerShell's URL parser for the goose binary path.
  GOOSE_ENV = $$env:GOOSE_DRIVER='$(GOOSE_DRIVER)'; $$env:GOOSE_DBSTRING='$(DB_DSN)'; $$env:GOOSE_MIGRATION_DIR='$(MIGRATE_DIR)';
  GOOSE_RUN = $(GOOSE_ENV) & $(GOOSE_BIN)
else
  # POSIX sh: inline env-var assignment
  GOOSE_RUN = GOOSE_DRIVER=$(GOOSE_DRIVER) GOOSE_DBSTRING=$(DB_DSN) GOOSE_MIGRATION_DIR=$(MIGRATE_DIR) $(GOOSE_BIN)
endif

.PHONY: migrate-up
migrate-up: _require-dsn ## Apply all pending migrations  (goose up)
	@$(ECHO) "goose up  driver=$(GOOSE_DRIVER)  dir=$(MIGRATE_DIR)"
	$(GOOSE_RUN) up

.PHONY: migrate-down
migrate-down: _require-dsn ## Roll back the last applied migration  (goose down)
	@$(ECHO) "goose down  driver=$(GOOSE_DRIVER)  dir=$(MIGRATE_DIR)"
	$(GOOSE_RUN) down

.PHONY: migrate-down-to
migrate-down-to: _require-dsn ## Roll back to VERSION  (make migrate-down-to VERSION=0)
ifeq ($(VERSION),)
	$(error Usage: make migrate-down-to VERSION=<number>)
endif
	$(GOOSE_RUN) down-to $(VERSION)

.PHONY: migrate-status
migrate-status: _require-dsn ## Show applied / pending migrations  (goose status)
	$(GOOSE_RUN) status

.PHONY: migrate-reset
migrate-reset: _require-dsn ## Roll back ALL migrations — DESTRUCTIVE
	@$(ECHO) "WARNING: rolling back ALL migrations. Ctrl-C to abort."
	$(call SLEEP,3)
	$(GOOSE_RUN) reset

.PHONY: migrate-create
migrate-create: ## Create a new empty migration  (make migrate-create NAME=add_index)
ifeq ($(NAME),)
	$(error Usage: make migrate-create NAME=<description>)
endif
	$(GOOSE_BIN) -dir $(MIGRATE_DIR) create $(NAME) sql
	@$(ECHO) "Created in $(MIGRATE_DIR)/"

.PHONY: migrate-validate
migrate-validate: ## Validate migration file syntax — no DB connection needed
	$(GOOSE_BIN) -dir $(MIGRATE_DIR) validate

# Apply migrations for all three dialects at once (CI).
# Requires POSTGRES_DSN, MYSQL_DSN, MSSQL_DSN to be set.
.PHONY: migrate-up-all
migrate-up-all: ## Apply migrations for all three dialects (CI)
	@$(ECHO) "--- Postgres ---"
	$(MAKE) migrate-up DB_DRIVER=postgres   DB_DSN=$(POSTGRES_DSN)
	@$(ECHO) "--- MySQL ---"
	$(MAKE) migrate-up DB_DRIVER=mysql      DB_DSN=$(MYSQL_DSN)
	@$(ECHO) "--- SQL Server ---"
	$(MAKE) migrate-up DB_DRIVER=sqlserver  DB_DSN=$(MSSQL_DSN)

# =============================================================================
# DOCKER
# =============================================================================
.PHONY: docker-db-up
docker-db-up: ## Start only the database containers (postgres, mysql, sqlserver)
	$(COMPOSE) -f $(COMPOSE_FILE) up -d postgres mysql sqlserver
	@$(ECHO) "Databases starting — waiting for healthchecks."
	@$(ECHO) "  Postgres   -> localhost:5432"
	@$(ECHO) "  MySQL      -> localhost:3306"
	@$(ECHO) "  SQL Server -> localhost:1433"

.PHONY: docker-up
docker-up: ## Start databases + trailog API service (builds image if needed)
	$(COMPOSE) -f $(COMPOSE_FILE) --profile full up -d --build
	@$(ECHO) "Stack up. API -> http://localhost:8080/health"

.PHONY: docker-down
docker-down: ## Stop and remove containers (keeps volumes)
	$(COMPOSE) -f $(COMPOSE_FILE) --profile full down

.PHONY: docker-down-volumes
docker-down-volumes: ## Stop containers AND delete all volumes — DESTRUCTIVE
	@$(ECHO) "WARNING: all database volumes will be deleted. Ctrl-C to abort."
	$(call SLEEP,3)
	$(COMPOSE) -f $(COMPOSE_FILE) --profile full down -v

.PHONY: docker-build
docker-build: ## Rebuild the trailog Docker image
	$(COMPOSE) -f $(COMPOSE_FILE) build trailog

.PHONY: docker-logs
docker-logs: ## Tail logs from all containers
	$(COMPOSE) -f $(COMPOSE_FILE) --profile full logs -f

.PHONY: docker-logs-api
docker-logs-api: ## Tail logs from the trailog service only
	$(COMPOSE) -f $(COMPOSE_FILE) logs -f trailog

.PHONY: docker-ps
docker-ps: ## Show container status
	$(COMPOSE) -f $(COMPOSE_FILE) --profile full ps

.PHONY: docker-exec-pg
docker-exec-pg: ## Open psql shell in the Postgres container
	$(COMPOSE) -f $(COMPOSE_FILE) exec postgres psql -U trailog -d auditdb

.PHONY: docker-exec-mysql
docker-exec-mysql: ## Open mysql shell in the MySQL container
	$(COMPOSE) -f $(COMPOSE_FILE) exec mysql mysql -utrailog -ptrailog auditdb

# =============================================================================
# TESTING
# =============================================================================
.PHONY: test
test: ## Run all tests
	$(GO) test $(TEST_FLAGS) ./...

.PHONY: test-short
test-short: ## Run tests, skipping slow/integration tests
	$(GO) test -short ./...

.PHONY: cover
cover: ## Run tests with coverage and open HTML report
	$(GO) test $(TEST_FLAGS) -coverprofile=$(COVERAGE_OUT) -covermode=atomic ./...
	$(GO) tool cover -html=$(COVERAGE_OUT) -o $(COVERAGE_HTML)
	@$(ECHO) "Coverage report: $(COVERAGE_HTML)"

.PHONY: cover-pct
cover-pct: ## Print total coverage percentage
	$(GO) test -coverprofile=$(COVERAGE_OUT) -covermode=atomic ./...
	$(GO) tool cover -func=$(COVERAGE_OUT)

# =============================================================================
# CODE QUALITY
# =============================================================================
.PHONY: lint
lint: ## Run golangci-lint
	$(GOLANGCI) run ./...

.PHONY: lint-fix
lint-fix: ## Run golangci-lint with auto-fix
	$(GOLANGCI) run --fix ./...

.PHONY: vet
vet: ## Run go vet
	$(GO) vet ./...

.PHONY: fmt
fmt: ## Format all Go source files
	$(GO) fmt ./...

.PHONY: check
check: fmt vet lint test ## Run all quality checks (fmt + vet + lint + test)

# =============================================================================
# COMPOSITE WORKFLOWS
# =============================================================================
.PHONY: dev
dev: docker-db-up ## Start databases, wait, apply migrations, then run the service
	@$(ECHO) "Waiting 8 s for databases to be healthy..."
	$(call SLEEP,8)
	$(MAKE) migrate-up
	$(MAKE) run

.PHONY: ci
ci: deps vet lint test ## Full CI pipeline (deps + vet + lint + test — no docker)
